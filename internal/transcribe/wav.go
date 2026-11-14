package transcribe

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"vip.oberon.mandaryn/internal/apierr"
)

// WAV 读取（自己实现，不依赖 internal/recording：录制域在 S1，本包只消费成品文件）。
//
// 支持范围（音频落盘格式由 FR-03 固定为 16kHz 单声道 PCM16，见 DATA-MODEL §1.3）：
//   - RIFF/WAVE 容器，`fmt ` + `data` 块；
//   - 位深 16bit PCM（format 1）与 32bit IEEE float（format 3）；
//   - 声道数 1 或 2（立体声按声道平均下混为单声道）；
//   - 其它格式返回 ERR_INVALID_ARGUMENT（转写任务置 failed，**不动音频**）。
//
// 之所以按块解析而不是硬编码 44 字节：真实文件常带 `LIST`/`fact` 等附加块，
// 硬编码偏移会读进元数据当成采样，表现为「满篇噪声 / 偏移 44 字节的乱码」。
const (
	wavFormatPCM       = 1
	wavFormatIEEEFloat = 3
	// wavMaxHeaderChunks 限制块扫描次数，防止损坏文件里 size=0 的块导致死循环。
	wavMaxHeaderChunks = 64
)

// wavInfo 是 WAV 头解析结果。
type wavInfo struct {
	sampleRate    int
	channels      int
	bitsPerSample int
	format        int
	// dataOffset `data` 块起始字节偏移（相对文件头）。
	dataOffset int64
	// dataBytes 音频数据字节数。
	dataBytes int64
	// frames 采样帧数（每帧含 channels 个采样）。
	frames int64
}

// totalMs 按帧计数换算总时长（ARCH §4.5.1：不用墙钟）。
func (h wavInfo) totalMs() int64 {
	if h.sampleRate <= 0 {
		return 0
	}
	return h.frames * 1000 / int64(h.sampleRate)
}

// wavSource 是 WAV 文件的随机读封装：按帧读取并归一化成 [-1,1] 的 float32 单声道。
type wavSource struct {
	r   AudioSource
	inf wavInfo
}

// openWAV 解析 WAV 头并返回读取器。
//
// 错误码约定：
//   - 文件不是 RIFF/WAVE、位深/编码不支持 → ERR_INVALID_ARGUMENT（不可重试，用户需重录/修复）；
//   - 读失败（IO）→ ERR_DB_WRITE 不合适，用 ERR_INVALID_ARGUMENT 之外的兜底由调用方决定，
//     这里统一返回 ERR_INVALID_ARGUMENT 并带 Detail（不含音频内容）。
func openWAV(r AudioSource) (*wavSource, error) {
	if r == nil {
		return nil, errAudioInvalid("音频句柄为空")
	}
	inf, err := readWAVHeader(r)
	if err != nil {
		return nil, err
	}
	return &wavSource{r: r, inf: inf}, nil
}

// Close 关闭底层音频句柄。
func (s *wavSource) Close() error {
	if s == nil || s.r == nil {
		return nil
	}
	return s.r.Close()
}

// Info 返回解析出的头信息。
func (s *wavSource) Info() wavInfo { return s.inf }

// TotalFrames 返回总采样帧数。
func (s *wavSource) TotalFrames() int64 { return s.inf.frames }

// TotalMs 返回总时长（帧计数换算）。
func (s *wavSource) TotalMs() int64 { return s.inf.totalMs() }

// SampleRate 返回采样率。
func (s *wavSource) SampleRate() int { return s.inf.sampleRate }

// ReadFrames 读取从帧偏移 start 开始的 n 帧，返回单声道 float32（已归一化）。
//
// 返回值长度可能小于 n（读到 EOF 时），调用方按实际长度推进。
func (s *wavSource) ReadFrames(start, n int64) ([]float32, error) {
	if n <= 0 {
		return nil, nil
	}
	if start < 0 {
		return nil, errAudioInvalid("起始帧为负")
	}
	if start >= s.inf.frames {
		return nil, nil
	}
	if start+n > s.inf.frames {
		n = s.inf.frames - start
	}

	ch := int64(s.inf.channels)
	bytesPerSample := int64(s.inf.bitsPerSample / 8)
	readBytes := n * ch * bytesPerSample

	buf := make([]byte, readBytes)
	// 数据段可能被截断（崩溃恢复场景，ARCH §6.3）：按实际读到的字节裁剪，
	// 而不是把整段当成错误——能转多少转多少。
	got, err := s.readAt(start*ch*bytesPerSample, buf)
	if err != nil {
		return nil, err
	}
	if int64(got) < readBytes {
		buf = buf[:got]
	}
	return s.decode(buf)
}

// readAt 从 data 块内偏移 off 处读满 buf（允许短读）。
func (s *wavSource) readAt(off int64, buf []byte) (int, error) {
	if _, err := s.r.Seek(s.inf.dataOffset+off, io.SeekStart); err != nil {
		return 0, errAudioInvalid("音频定位失败")
	}
	total := 0
	for total < len(buf) {
		n, err := s.r.Read(buf[total:])
		total += n
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, errAudioInvalid("音频读取失败").WithCause(err)
		}
		if n == 0 {
			return total, nil
		}
	}
	return total, nil
}

// decode 把 PCM 字节解成单声道 float32（立体声按声道平均下混）。
func (s *wavSource) decode(buf []byte) ([]float32, error) {
	ch := s.inf.channels
	bps := s.inf.bitsPerSample / 8
	if ch <= 0 || bps <= 0 {
		return nil, errAudioInvalid("音频头声道/位深非法")
	}
	samples := len(buf) / (ch * bps)
	out := make([]float32, 0, samples)

	for i := 0; i < samples; i++ {
		base := i * ch * bps
		var acc float32
		for c := 0; c < ch; c++ {
			off := base + c*bps
			switch s.inf.format {
			case wavFormatPCM:
				// 16bit 有符号：除以 32768 归一到 [-1, 1)。
				v := int16(binary.LittleEndian.Uint16(buf[off : off+2]))
				acc += float32(v) / 32768.0
			case wavFormatIEEEFloat:
				bits := binary.LittleEndian.Uint32(buf[off : off+4])
				acc += math.Float32frombits(bits)
			default:
				return nil, errAudioInvalid("不支持的 WAV 编码格式 %d", s.inf.format)
			}
		}
		out = append(out, acc/float32(ch))
	}
	return out, nil
}

// readWAVHeader 扫描 RIFF 块，定位 `fmt ` 与 `data`。
func readWAVHeader(r AudioSource) (wavInfo, error) {
	var inf wavInfo

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return inf, errAudioInvalid("音频定位失败").WithCause(err)
	}
	head := make([]byte, 12)
	if _, err := io.ReadFull(r, head); err != nil {
		return inf, errAudioInvalid("音频头不足 12 字节（不是 WAV）").WithCause(err)
	}
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return inf, errAudioInvalid("不是 RIFF/WAVE 音频")
	}

	var pos int64 = 12
	haveFmt := false
	for i := 0; i < wavMaxHeaderChunks; i++ {
		chunkHead := make([]byte, 8)
		if _, err := io.ReadFull(r, chunkHead); err != nil {
			break // 头读完仍未找到 data：下面按缺失处理
		}
		id := string(chunkHead[0:4])
		size := int64(binary.LittleEndian.Uint32(chunkHead[4:8]))
		body := pos + 8

		switch id {
		case "fmt ":
			fmtBuf := make([]byte, size)
			if _, err := io.ReadFull(r, fmtBuf); err != nil {
				return inf, errAudioInvalid("fmt 块损坏").WithCause(err)
			}
			if len(fmtBuf) < 16 {
				return inf, errAudioInvalid("fmt 块过短")
			}
			inf.format = int(binary.LittleEndian.Uint16(fmtBuf[0:2]))
			inf.channels = int(binary.LittleEndian.Uint16(fmtBuf[2:4]))
			inf.sampleRate = int(binary.LittleEndian.Uint32(fmtBuf[4:8]))
			inf.bitsPerSample = int(binary.LittleEndian.Uint16(fmtBuf[14:16]))
			haveFmt = true
			pos = body + size + size%2
			if _, err := r.Seek(pos, io.SeekStart); err != nil {
				return inf, errAudioInvalid("音频定位失败").WithCause(err)
			}
			continue
		case "data":
			if !haveFmt {
				return inf, errAudioInvalid("data 块出现在 fmt 之前")
			}
			inf.dataOffset = body
			inf.dataBytes = size
			// 长录音 + 文件被截断时 size 会大于实际可用字节：以文件尾为准裁剪。
			if tail, err := r.Seek(0, io.SeekEnd); err == nil && tail < body+size {
				inf.dataBytes = tail - body
				if inf.dataBytes < 0 {
					inf.dataBytes = 0
				}
			}
			if err := validateWAVFormat(inf); err != nil {
				return inf, err
			}
			bytesPerFrame := int64(inf.channels * inf.bitsPerSample / 8)
			if bytesPerFrame > 0 {
				inf.frames = inf.dataBytes / bytesPerFrame
			}
			return inf, nil
		default:
			// LIST / fact / cue 等：跳过（注意 RIFF 块按 2 字节对齐）。
			pos = body + size + size%2
			if _, err := r.Seek(pos, io.SeekStart); err != nil {
				return inf, errAudioInvalid("音频定位失败").WithCause(err)
			}
			continue
		}
	}
	return inf, errAudioInvalid("WAV 缺少 data 块")
}

// validateWAVFormat 校验本包支持的采样格式。
func validateWAVFormat(inf wavInfo) error {
	switch inf.format {
	case wavFormatPCM:
		if inf.bitsPerSample != 16 {
			return errAudioInvalid("仅支持 16bit PCM，实际 %d bit", inf.bitsPerSample)
		}
	case wavFormatIEEEFloat:
		if inf.bitsPerSample != 32 {
			return errAudioInvalid("仅支持 32bit float，实际 %d bit", inf.bitsPerSample)
		}
	default:
		return errAudioInvalid("不支持的 WAV 编码格式 %d", inf.format)
	}
	if inf.channels != 1 && inf.channels != 2 {
		return errAudioInvalid("仅支持单声道或立体声，实际 %d 声道", inf.channels)
	}
	if inf.sampleRate <= 0 || inf.sampleRate > 192000 {
		return errAudioInvalid("采样率异常：%d", inf.sampleRate)
	}
	return nil
}

// errAudioInvalid 构造「音频不可用」错误。
//
// 复用最接近的既有码 ERR_INVALID_ARGUMENT（错误码表里没有专门的音频码，
// 见 docs/DEV-ISSUES-S4.md S4-01）。不可重试：同一文件重试也不会变好，用户需重录或修复。
func errAudioInvalid(format string, args ...any) *apierr.Err {
	e := apierr.New(apierr.CodeInvalidArgument)
	e = e.WithAction("re-record")
	if len(args) > 0 {
		e = e.WithDetail(format, args...)
	} else {
		e = e.WithDetail("%s", format)
	}
	return e
}

// describeWAV 生成日志用的音频摘要（**只含格式元数据，不含音频内容**）。
func describeWAV(inf wavInfo) string {
	return fmt.Sprintf("%dHz/%dch/%dbit/%dms", inf.sampleRate, inf.channels,
		inf.bitsPerSample, inf.totalMs())
}
