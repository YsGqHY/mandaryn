package recording

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/utils/filex"
)

// WAV PCM 头长度（RIFF/fmt/data 三个块的最小合法组合）。
const wavHeaderSize = 44

// maxWAVDataBytes WAV 的 data 块长度上限（RIFF 用 uint32 存长度）。
//
// 16kHz/16bit/单声道 = 32KB/s，4GiB 上限对应约 37 小时单会话。
// 超限**不静默截断**：截断会让用户的音频在后面无声，且时长字段与实际不符。
// 见 docs/DEV-ISSUES-S2.md（S2-04）。
const maxWAVDataBytes = math.MaxUint32 - uint64(wavHeaderSize)

// WAVInfo 是读回的 WAV 头信息（测试与恢复自检共用）。
type WAVInfo struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	// DataBytes data 块在头里声明的长度。
	DataBytes uint32
	// FileBytes 文件实际大小。
	FileBytes int64
	// DataFileBytes 文件里实际存在的 data 字节数（FileBytes - 44，可能小于声明值）。
	DataFileBytes int64
}

// Frames 按头里声明的 data 长度换算的帧数。
func (i WAVInfo) Frames() int64 {
	block := int64(i.Channels) * int64(i.BitsPerSample) / 8
	if block <= 0 {
		return 0
	}
	return int64(i.DataBytes) / block
}

// DurationMs 按头声明长度换算的时长（毫秒）。
func (i WAVInfo) DurationMs() int64 {
	return FramesToMs(i.Frames())
}

// Writer 把内部格式（16kHz/16bit/单声道）样本写成 WAV 并维护帧计数。
//
// 两个关键约定：
//   - **帧计数是唯一时间基**：`frames` 只随实际写入的样本增加，
//     与时钟无关；会话时长、segments.startMs、回放 seek 全部由它推导。
//   - **头长度字段只在 Seal 回填**：录制中头里写 0，崩溃后由 RepairWAV
//     按实际文件长度重算 —— 这样「进程被杀」不会留下一个声明长度远大于
//     实际长度的头，导致播放器读到文件尾报错。
//
// 并发安全（内部加锁）：写由落盘 goroutine 驱动，Sync/Metrics 读取
// 可能来自其它 goroutine。
type Writer struct {
	mu   sync.Mutex
	f    *os.File
	path string

	frames       int64
	dataBytes    int64
	syncedFrames int64

	sealed bool
	closed bool
	// err 首个写入错误（磁盘满等），后续写入直接返回它，避免刷屏。
	err error
}

// CreateWriter 创建会话 WAV 文件并写入占位头。
//
// 目录不存在时自动创建（`sessions/<id>/`）。
func CreateWriter(path string) (*Writer, error) {
	if err := filex.EnsureDir(filepath.Dir(path)); err != nil {
		return nil, wrapFSError(err, "创建音频目录失败")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, wrapFSError(err, "创建音频文件失败")
	}
	w := &Writer{f: f, path: path}
	// 先落占位头，保证文件至少是合法 WAV（崩溃后 Repair 只改长度字段）。
	if err := w.writeHeaderLocked(); err != nil {
		_ = f.Close()
		return nil, wrapFSError(err, "写入 WAV 头失败")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, wrapFSError(err, "同步 WAV 头失败")
	}
	return w, nil
}

// Path 音频文件路径。
func (w *Writer) Path() string { return w.path }

// Frames 已写入的帧数（= 单声道样本数）。
func (w *Writer) Frames() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.frames
}

// DurationMs 已录音的逻辑时长（由帧计数换算）。
func (w *Writer) DurationMs() int64 { return FramesToMs(w.Frames()) }

// DataBytes 已写入的 PCM 数据字节数。
func (w *Writer) DataBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dataBytes
}

// SizeBytes 音频文件当前的总字节数（含头）。崩溃恢复与 UI「已存 x MB」用。
func (w *Writer) SizeBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// 关闭后以磁盘实际长度为准（Seal 会改写头，长度不变，但更稳妥）。
		if st, err := os.Stat(w.path); err == nil {
			return st.Size()
		}
	}
	return wavHeaderSize + w.dataBytes
}

// Err 返回首个写入错误（nil 表示正常）。
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Write 写入一批内部格式（[-1,1] float32）样本。
//
// 内部格式固定单声道，因此样本数即帧数。
func (w *Writer) Write(samples []float32) error {
	if len(samples) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return apierr.Newf(apierr.CodeStateForbidden, "音频文件已关闭，无法继续写入").
			WithDetail("path=%s", w.path)
	}
	if uint64(w.dataBytes)+uint64(len(samples))*2 > maxWAVDataBytes {
		w.err = apierr.New(apierr.CodeDiskFull).
			WithDetail("单会话音频超过 WAV 4GiB 上限（约 37 小时），已停止写入：%s", w.path)
		return w.err
	}

	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(floatToPCM16(s)))
	}
	n, err := w.f.Write(buf)
	w.dataBytes += int64(n)
	w.frames += int64(n) / 2
	if err != nil {
		w.err = wrapFSError(err, "写入音频数据失败")
		return w.err
	}

	// 200ms 刷盘（按帧数触发，不依赖时钟）。
	if w.frames-w.syncedFrames >= SyncEveryFrames {
		if serr := w.f.Sync(); serr != nil {
			w.err = wrapFSError(serr, "刷盘失败")
			return w.err
		}
		w.syncedFrames = w.frames
	}
	return nil
}

// Sync 强制刷盘（Stop 前的最后一次落盘）。
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	if err := w.f.Sync(); err != nil {
		return wrapFSError(err, "刷盘失败")
	}
	w.syncedFrames = w.frames
	return nil
}

// Seal 封口：回填 RIFF/data 长度字段并 fsync。
//
// 幂等：重复调用只做一次写入。封口后文件立刻可被标准播放器播放
// （不 Seal 的文件头里 data 长度是 0，播放器会认为没有音频）。
func (w *Writer) Seal() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sealed {
		return nil
	}
	if w.closed {
		return apierr.New(apierr.CodeStateForbidden).
			WithDetail("音频文件已关闭，无法封口：%s", w.path)
	}
	if err := w.writeHeaderLocked(); err != nil {
		return wrapFSError(err, "回填 WAV 头失败")
	}
	if err := w.f.Sync(); err != nil {
		return wrapFSError(err, "封口刷盘失败")
	}
	w.sealed = true
	return nil
}

// Close 关闭文件（未封口则先封口，保证不留下不可播放的文件）。
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	_ = w.Seal()

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.f.Close()
}

// writeHeaderLocked 改写 44 字节头，并把写入位置恢复到文件末尾。
//
// 坑（docs/DEV-ISSUES-S2.md S2-02）：`os.File.WriteAt` **不会移动文件偏移**，
// 而录制期间的数据是顺序 `Write` 上去的。若不显式 seek 到末尾，
// Create 后第一次 Write 会从偏移 0 开始，直接覆盖掉刚写的头 ——
// 表现为「文件大小正好等于 data 长度、差 44 字节」，且文件不是合法 WAV。
func (w *Writer) writeHeaderLocked() error {
	header := buildWAVHeader(InternalSampleRate, InternalChannels, InternalBitsPerSample, uint32(w.dataBytes))
	if _, err := w.f.WriteAt(header, 0); err != nil {
		return err
	}
	_, err := w.f.Seek(0, io.SeekEnd)
	return err
}

// buildWAVHeader 生成 44 字节标准 PCM WAV 头。
func buildWAVHeader(sampleRate, channels, bits int, dataBytes uint32) []byte {
	h := make([]byte, wavHeaderSize)
	blockAlign := uint16(channels * bits / 8)
	byteRate := uint32(sampleRate) * uint32(blockAlign)

	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(wavHeaderSize-8)+dataBytes)
	copy(h[8:12], "WAVE")

	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16) // fmt 块长度
	binary.LittleEndian.PutUint16(h[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:32], byteRate)
	binary.LittleEndian.PutUint16(h[32:34], blockAlign)
	binary.LittleEndian.PutUint16(h[34:36], uint16(bits))

	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], dataBytes)
	return h
}

// floatToPCM16 把 [-1,1] 浮点样本转成 16bit 整型样本。
//
// 满量程映射：+1.0 → 32767、-1.0 → -32768。用 32767 缩放而不是 32768，
// 是为了让 +1.0 不溢出成 -32768（那会是一次可听见的爆音）；
// 代价是 -1.0 精确落在 -32767 而不是 -32768，误差 1 LSB（-90dBFS），可忽略。
func floatToPCM16(v float32) int16 {
	if v >= 1 {
		return 32767
	}
	if v <= -1 {
		return -32768
	}
	return int16(math.Round(float64(v) * 32767))
}

// pcm16ToFloat 读回测试与校验用。
func pcm16ToFloat(v int16) float32 { return float32(v) / 32767 }

// InspectWAV 读取 WAV 头并返回元数据（不解析音频内容）。
func InspectWAV(path string) (WAVInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return WAVInfo{}, wrapFSError(err, "打开音频文件失败")
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return WAVInfo{}, wrapFSError(err, "读取音频文件信息失败")
	}
	info := WAVInfo{FileBytes: st.Size()}
	if st.Size() < wavHeaderSize {
		return info, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("音频文件不完整（%d 字节，缺少 WAV 头）：%s", st.Size(), path)
	}

	head := make([]byte, wavHeaderSize)
	if _, err := f.ReadAt(head, 0); err != nil {
		return info, wrapFSError(err, "读取 WAV 头失败")
	}
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return info, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("不是合法的 WAV 文件（RIFF/WAVE 标记缺失）：%s", path)
	}
	info.Channels = int(binary.LittleEndian.Uint16(head[22:24]))
	info.SampleRate = int(binary.LittleEndian.Uint32(head[24:28]))
	info.BitsPerSample = int(binary.LittleEndian.Uint16(head[34:36]))
	info.DataBytes = binary.LittleEndian.Uint32(head[40:44])
	info.DataFileBytes = st.Size() - wavHeaderSize
	return info, nil
}

// RepairWAV 按**实际文件长度**回填 RIFF/data 长度字段，返回修补后的帧数。
//
// 用于崩溃/断电恢复（UC-07 / Recover）：
//   - 录制中头里 data 长度写 0（只在 Seal 回填），崩溃后播放器会认为没有音频；
//   - 文件可能因为 200ms 刷盘策略被截断在半帧上（奇数字节），需要裁掉半帧 ——
//     WAV 的 blockAlign=2，奇数字节会让所有播放器/解码库报错。
//
// 修补只动头与前 44 字节后的截断，**不重写、不丢弃任何音频数据**。
func RepairWAV(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return 0, wrapFSError(err, "打开音频文件失败")
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return 0, wrapFSError(err, "读取音频文件信息失败")
	}
	if st.Size() < wavHeaderSize {
		return 0, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("音频文件过短（%d 字节），无法修补：%s", st.Size(), path)
	}

	head := make([]byte, wavHeaderSize)
	if _, err := f.ReadAt(head, 0); err != nil {
		return 0, wrapFSError(err, "读取 WAV 头失败")
	}
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return 0, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("不是合法的 WAV 文件，无法修补：%s", path)
	}
	channels := int(binary.LittleEndian.Uint16(head[22:24]))
	bits := int(binary.LittleEndian.Uint16(head[34:36]))
	sampleRate := int(binary.LittleEndian.Uint32(head[24:28]))
	blockAlign := channels * bits / 8
	if blockAlign <= 0 {
		return 0, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("WAV 头里的声道/位深非法（channels=%d bits=%d）：%s", channels, bits, path)
	}

	dataBytes := st.Size() - wavHeaderSize
	// 裁掉不完整的尾帧：奇数字节（或非 blockAlign 整数倍）会让解码器报错。
	if rem := dataBytes % int64(blockAlign); rem != 0 {
		dataBytes -= rem
		if err := f.Truncate(wavHeaderSize + dataBytes); err != nil {
			return 0, wrapFSError(err, "截断不完整尾帧失败")
		}
	}
	if uint64(dataBytes) > maxWAVDataBytes {
		dataBytes = int64(maxWAVDataBytes)
	}

	header := buildWAVHeader(sampleRate, channels, bits, uint32(dataBytes))
	if _, err := f.WriteAt(header, 0); err != nil {
		return 0, wrapFSError(err, "回填 WAV 头失败")
	}
	if err := f.Sync(); err != nil {
		return 0, wrapFSError(err, "修补后刷盘失败")
	}
	return dataBytes / int64(blockAlign), nil
}

// errDiskFull 用于错误分类的哨兵（跨平台由 wrapFSError 判定后产生）。
var errDiskFullSentinel = errors.New("disk full")

// wrapFSError 把文件系统错误归一到 apierr：磁盘满 → ERR_DISK_FULL，其余 → ERR_PATH_UNWRITABLE。
//
// Windows 的 ERROR_DISK_FULL / ERROR_HANDLE_DISK_FULL 由 Go 运行时映射成 ENOSPC，
// 但不同层（os / syscall / 第三方）会包装，因此既判 errors.Is 也兜底看错误文本。
func wrapFSError(err error, what string) error {
	if err == nil {
		return nil
	}
	e := apierr.Wrap(err, apierr.CodePathUnwritable)
	e.Message = fmt.Sprintf("%s：%v", what, err)
	if isDiskFull(err) {
		return apierr.Wrap(err, apierr.CodeDiskFull).
			WithDetail("%s：磁盘空间不足", what)
	}
	return e
}

// isDiskFull 判断是否是「磁盘/配额不足」类错误。
func isDiskFull(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errDiskFullSentinel) {
		return true
	}
	if errors.Is(err, errnoENOSPC()) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space left") ||
		strings.Contains(msg, "disk full") ||
		strings.Contains(msg, "not enough space") ||
		strings.Contains(msg, "空间不足")
}
