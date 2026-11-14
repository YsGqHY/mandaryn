// Package recording 是「录制与录音实例」领域包（FR-03 / DOC-02 §4.3 §4.5 §6）。
//
// 分层约束（与 internal/capture 同一套规矩）：
//   - 本包属于 L2 领域层，**禁止 import Wails**，因此可被单测直接驱动；
//   - 与前端共享的 DTO 一律用 internal/contract（RecordingState / RecordingTick /
//     StartRequest / Session），本包不重复定义，避免前后端字段漂移；
//   - 本包只做「采集 → 重采样 → 落盘 → 封存录音实例」，**不做转写**。
//     转写是 internal/asr 与 transcriptsvc 的事（FR-03 铁律：录制与转写解耦）。
//
// 三条不可违反的实现约定：
//  1. **时间基 = 帧计数**（DOC-02 §4.5.1）：任何 startMs/endMs/时长都由
//     `frameCount / 16000 * 1000` 换算，不用 wall clock —— 系统休眠、设备时钟漂移
//     下 wall clock 会走、音频帧不会走，用时钟算时间轴必然漂移且无法与文件对齐。
//     wall clock 只用于 sessions.started_at/ended_at 的「显示时间」。
//  2. **收尾只封存**（ST-05）：Stop 只封 WAV + 写元数据，**绝不同步跑推理**。
//     拖慢收尾会放大「崩溃丢音频」的窗口。
//  3. **音频与正文永不进日志**（NFR-03-5）：日志只写元数据（时长/字节/错误码）。
package recording

// 内部统一音频格式（DOC-02 §4.3）：16kHz / 16bit / 单声道 PCM。
//
// 这是落盘、回放与后续推理共用的唯一格式；源格式一律通过 capture.AudioFormat
// 协商后重采样进来，不做「按源格式原样落盘」——那会让播放器、声纹与 ASR
// 三处各自处理多种格式，且会话时长换算要带采样率上下文。
const (
	// InternalSampleRate 内部采样率（Hz）。帧与时长的换算分母固定为它。
	InternalSampleRate = 16000
	// InternalChannels 内部声道数：单声道（说话人分离靠声纹，不靠声道）。
	InternalChannels = 1
	// InternalBitsPerSample 位深：WAV PCM 16bit。
	InternalBitsPerSample = 16
)

// 管线与落盘参数（DOC-02 §4.3 表）。
const (
	// RingBufferSeconds 环形缓冲容量（秒）。用于吸收一次 GC 抖动 / 慢 fsync；
	// 溢出时丢**最旧**并计数告警，绝不阻塞采集线程。
	RingBufferSeconds = 3
	// RingBufferCapacity 环形缓冲容量（样本数，单声道）。
	RingBufferCapacity = InternalSampleRate * RingBufferSeconds

	// SyncEveryFrames 每写多少个帧 fsync 一次 = 200ms（ARCH §4.3「200ms 刷盘」）。
	// 崩溃最多丢 200ms，配合 Recover 的 WAV 头修补（UC-07）。
	SyncEveryFrames = InternalSampleRate / 5

	// ReportWindowSamples 指标上报窗口 = 250ms → 4Hz（EV-02/EV-03 的服务侧节流）。
	// 用**帧数**而不是时钟来切窗口：节拍只依赖音频进度，测试可确定复现。
	ReportWindowSamples = InternalSampleRate / 4

	// SilentPeakWindowCount 「未检测到音频」的峰值观察窗数与阈值。
	// 峰值判定用绝对值上限：低于 -60dBFS 视为数字静音（与 capture.ProducingPeakThreshold 同口径）。
	SilentPeakThreshold = 0.001

	// SilenceWarnMs 连续静音多久提示「未检测到音频」（FR-03-7 / ROADMAP 4.1）。
	SilenceWarnMs = 10000

	// MinDurationMs 空录音判定门槛：时长 < 1s 视为误触。
	MinDurationMs = 1000

	// EmptyRecordingAction 空录音时 state.lastError 的动作键。
	//
	// 空录音沿用 ERR_STATE_FORBIDDEN（API-2.4 的错误码表里没有专门的码），
	// 但它不是「状态冲突」：前端靠这个动作键区分，提示「没录到声音、请确认源在发声」，
	// 否则会显示成「当前状态下不允许该操作」，而用户刚刚只是正常点了停止。
	EmptyRecordingAction = "check-source"
)

// 文件布局（DATA-MODEL §1：`<dataRoot>/sessions/<id>/audio.wav`）。
const (
	// AudioFileName 会话音频文件名。
	AudioFileName = "audio.wav"
	// AudioFormatWAV 落库的 audio_format 值。
	AudioFormatWAV = "wav"
)

// FramesToMs 帧计数换算毫秒（DOC-02 §4.5.1 的唯一公式）。
//
// 全程整数运算：`frames * 1000 / 16000`。用浮点会在长会话里累积到毫秒级偏差，
// 且回放 seek 与 segments.startMs 的对齐会出现「差一毫秒」的抖动。
func FramesToMs(frames int64) int64 { return frames * 1000 / InternalSampleRate }

// MsToFrames 毫秒换算帧数（向下取整，用于标记位对齐）。
func MsToFrames(ms int64) int64 { return ms * InternalSampleRate / 1000 }

// SamplesToMs 指定采样率下样本数换算毫秒（仅用于源格式侧的诊断指标）。
func SamplesToMs(samples int64, sampleRate int) int64 {
	if sampleRate <= 0 {
		return 0
	}
	return samples * 1000 / int64(sampleRate)
}
