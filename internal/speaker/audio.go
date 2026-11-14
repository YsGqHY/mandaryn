package speaker

import (
	"context"
	"encoding/binary"
	"io"
	"math"

	"vip.oberon.mandaryn/internal/apierr"
)

// 片段级音频读取（声纹提取的输入）。
//
// 为什么自己解析 WAV 而不复用 internal/transcribe 的实现：那边的 `openWAV` /
// `wavSource` 是**包内私有**（transcribe 域不允许被本域修改），且它按「整段续读」
// 设计，本域需要的是「按片段毫秒区间随机取一段」。这里只实现本域用到的最小子集：
// RIFF/WAVE、16bit PCM 与 32bit float、单声道或立体声下混为单声道。
//
// 边界：录制侧固定 16kHz（ARCH §4.3），因此采样率不是 16k 时直接报错，
// 不做重采样 —— 声纹模型按 16k 训练，喂别的采样率得到的是无意义向量。

const (
	wavFormatPCM       = 1
	wavFormatIEEEFloat = 3
	// wavMaxHeaderChunks 限制块扫描次数，防止损坏文件里的零长块导致死循环。
	wavMaxHeaderChunks = 64
)

// AudioSource 是可随机读取的音频句柄（*os.File 天然满足）。
type AudioSource interface {
	Read(p []byte) (int, error)
	Seek(offset int64, whence int) (int64, error)
	Close() error
}

// AudioOpener 由装配层注入：把会话 ID 解析成可读音频文件。
//
// 放在装配层是因为路径必须经 paths.Resolver（ADR-02），L2 不碰路径优先级。
// 音频缺失 → ERR_NOT_FOUND；dataRoot 不可用 → ERR_PATH_UNWRITABLE。
type AudioOpener func(ctx context.Context, sessionID string) (AudioSource, error)

// AudioLoader 是「按毫秒区间取单声道浮点样本」的端口。
//
// 抽成接口是为了让单测用假的加载器（不需要真音频文件也能驱动整条 pipeline）。
type AudioLoader interface {
	// LoadSegment 返回 [startMs, endMs) 的 16kHz 单声道样本（已归一化到 [-1,1]）。
	LoadSegment(ctx context.Context, sessionID string, startMs, endMs int64) ([]float32, error)
}

// WAVLoader 是基于 AudioOpener 的 AudioLoader 实现。
//
// 每个片段打开一次文件：片段数通常几十到几百，打开成本可忽略；
// 反过来「整段读进内存」在长会话上会吃掉几百 MB（1 小时 16k 单声道 float32 ≈ 230MB）。
type WAVLoader struct {
	open AudioOpener
}

// NewWAVLoader 构造 WAV 片段读取器；open 为 nil 时报参数错误。
func NewWAVLoader(open AudioOpener) (*WAVLoader, error) {
	if open == nil {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("AudioOpener 不能为空")
	}
	return &WAVLoader{open: open}, nil
}

// 编译期断言：WAVLoader 满足 AudioLoader。
var _ AudioLoader = (*WAVLoader)(nil)

// LoadSegment 读取指定毫秒区间。
//
// 时间基：`毫秒 → 帧` 用 `ms * rate / 1000`（帧计数换算，ARCH §4.5.1），
// 与片段落库时的换算方向一致，避免「片段边界与音频位置差半句」。
func (l *WAVLoader) LoadSegment(ctx context.Context, sessionID string, startMs, endMs int64) ([]float32, error) {
	if endMs <= startMs {
		return nil, nil
	}
	src, err := l.open(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()

	inf, err := readWAVHeader(src)
	if err != nil {
		return nil, err
	}
	if inf.sampleRate != SampleRate {
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithAction("re-record").
			WithDetail("音频采样率 %dHz 不受支持（声纹提取要求 %dHz）", inf.sampleRate, SampleRate)
	}

	startFrame := startMs * int64(inf.sampleRate) / 1000
	endFrame := endMs * int64(inf.sampleRate) / 1000
	if startFrame >= inf.frames {
		return nil, nil
	}
	if endFrame > inf.frames {
		endFrame = inf.frames
	}
	if endFrame <= startFrame {
		return nil, nil
	}

	return readFrames(src, inf, startFrame, endFrame-startFrame)
}

// wavInfo 是 WAV 头解析结果。
type wavInfo struct {
	sampleRate    int
	channels      int
	bitsPerSample int
	format        int
	dataOffset    int64
	dataBytes     int64
	frames        int64
}

// readWAVHeader 扫描 RIFF 块，定位 `fmt ` 与 `data`。
//
// 按块扫描而不是硬编码 44 字节：真实文件常带 `LIST`/`fact` 等附加块。
func readWAVHeader(r AudioSource) (wavInfo, error) {
	var inf wavInfo
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return inf, errAudio("音频定位失败").WithCause(err)
	}
	head := make([]byte, 12)
	if _, err := io.ReadFull(r, head); err != nil {
		return inf, errAudio("音频头不足 12 字节（不是 WAV）").WithCause(err)
	}
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return inf, errAudio("不是 RIFF/WAVE 音频")
	}

	var pos int64 = 12
	haveFmt := false
	for i := 0; i < wavMaxHeaderChunks; i++ {
		chunkHead := make([]byte, 8)
		if _, err := io.ReadFull(r, chunkHead); err != nil {
			break
		}
		id := string(chunkHead[0:4])
		size := int64(binary.LittleEndian.Uint32(chunkHead[4:8]))
		body := pos + 8

		switch id {
		case "fmt ":
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return inf, errAudio("fmt 块损坏").WithCause(err)
			}
			if len(buf) < 16 {
				return inf, errAudio("fmt 块过短")
			}
			inf.format = int(binary.LittleEndian.Uint16(buf[0:2]))
			inf.channels = int(binary.LittleEndian.Uint16(buf[2:4]))
			inf.sampleRate = int(binary.LittleEndian.Uint32(buf[4:8]))
			inf.bitsPerSample = int(binary.LittleEndian.Uint16(buf[14:16]))
			haveFmt = true
			pos = body + size + size%2
			if _, err := r.Seek(pos, io.SeekStart); err != nil {
				return inf, errAudio("音频定位失败").WithCause(err)
			}
		case "data":
			if !haveFmt {
				return inf, errAudio("data 块出现在 fmt 之前")
			}
			inf.dataOffset = body
			inf.dataBytes = size
			// 长录音 + 文件被截断：size 可能大于实际可用字节，以文件尾为准裁剪。
			if tail, err := r.Seek(0, io.SeekEnd); err == nil && tail < body+size {
				inf.dataBytes = tail - body
				if inf.dataBytes < 0 {
					inf.dataBytes = 0
				}
			}
			if err := validateWAVFormat(inf); err != nil {
				return inf, err
			}
			if bytesPerFrame := int64(inf.channels * inf.bitsPerSample / 8); bytesPerFrame > 0 {
				inf.frames = inf.dataBytes / bytesPerFrame
			}
			return inf, nil
		default:
			pos = body + size + size%2
			if _, err := r.Seek(pos, io.SeekStart); err != nil {
				return inf, errAudio("音频定位失败").WithCause(err)
			}
		}
	}
	return inf, errAudio("WAV 缺少 data 块")
}

// validateWAVFormat 校验支持的采样格式。
func validateWAVFormat(inf wavInfo) error {
	switch inf.format {
	case wavFormatPCM:
		if inf.bitsPerSample != 16 {
			return errAudio("仅支持 16bit PCM，实际 %dbit", inf.bitsPerSample)
		}
	case wavFormatIEEEFloat:
		if inf.bitsPerSample != 32 {
			return errAudio("仅支持 32bit float，实际 %dbit", inf.bitsPerSample)
		}
	default:
		return errAudio("不支持的 WAV 编码格式 %d", inf.format)
	}
	if inf.channels != 1 && inf.channels != 2 {
		return errAudio("仅支持单声道或立体声，实际 %d 声道", inf.channels)
	}
	if inf.sampleRate <= 0 || inf.sampleRate > 192000 {
		return errAudio("采样率异常：%d", inf.sampleRate)
	}
	return nil
}

// readFrames 读取从帧偏移 start 开始的 n 帧，返回单声道 float32。
func readFrames(r AudioSource, inf wavInfo, start, n int64) ([]float32, error) {
	if n <= 0 {
		return nil, nil
	}
	ch := int64(inf.channels)
	bytesPerSample := int64(inf.bitsPerSample / 8)
	want := n * ch * bytesPerSample

	if _, err := r.Seek(inf.dataOffset+start*ch*bytesPerSample, io.SeekStart); err != nil {
		return nil, errAudio("音频定位失败").WithCause(err)
	}
	buf := make([]byte, want)
	got, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, errAudio("音频读取失败").WithCause(err)
	}
	buf = buf[:got]

	bps := inf.bitsPerSample / 8
	if ch <= 0 || bps <= 0 {
		return nil, errAudio("音频头声道/位深非法")
	}
	samples := len(buf) / (int(ch) * bps)
	out := make([]float32, 0, samples)
	for i := 0; i < samples; i++ {
		base := i * int(ch) * bps
		var acc float32
		for c := 0; c < int(ch); c++ {
			off := base + c*bps
			switch inf.format {
			case wavFormatPCM:
				v := int16(binary.LittleEndian.Uint16(buf[off : off+2]))
				acc += float32(v) / 32768.0
			case wavFormatIEEEFloat:
				acc += math.Float32frombits(binary.LittleEndian.Uint32(buf[off : off+4]))
			}
		}
		out = append(out, acc/float32(ch))
	}
	return out, nil
}

// errAudio 构造「音频不可用」错误（复用既有码 ERR_INVALID_ARGUMENT，
// 错误码表里没有专门的音频码，见 docs/DEV-ISSUES-S4.md S4-01）。
func errAudio(format string, args ...any) *apierr.Err {
	e := apierr.New(apierr.CodeInvalidArgument).WithAction("re-record")
	if len(args) > 0 {
		return e.WithDetail(format, args...)
	}
	return e.WithDetail("%s", format)
}
