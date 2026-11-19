package contract

// 录制主状态（DOC-02 §6.1）。存 `sessions.state` / `RecordingState.State`。
//
// 注意：`degraded`（ST-07）**不是**独立状态而是正交标记，因此这里不列为常量值，
// 只保留 ST-01~ST-06、ST-08、ST-09 八个真正可存的 state 值。
const (
	StateIdle        = "idle"        // ST-01
	StatePreparing   = "preparing"   // ST-02
	StateRecording   = "recording"   // ST-03
	StatePaused      = "paused"      // ST-04
	StateFinalizing  = "finalizing"  // ST-05
	StateCompleted   = "completed"   // ST-06
	StateInterrupted = "interrupted" // ST-08
	StateFailed      = "failed"      // ST-09
)

// 性能档位（DOC-02 §4.7.3）。
const (
	LevelL0 = "L0" // 正常
	LevelL1 = "L1" // 关二次校正
	LevelL2 = "L2" // 单线程推理
	LevelL3 = "L3" // 隔句跳过
	LevelP  = "P"  // 暂停推理
)

// 模型来源（FR-01-1）。
const (
	ModelSourceLocal  = "local"
	ModelSourceOnline = "online"
)

// 转写任务状态（DOC-02 §6.4）。与录制主状态正交，禁止写入 State。
const (
	TranscriptionNone    = "none"
	TranscriptionQueued  = "queued"
	TranscriptionRunning = "running"
	TranscriptionDone    = "done"
	TranscriptionFailed  = "failed"
)

// RecordingState 录制状态快照（EV-01 / API-2.x）。
type RecordingState struct {
	// State ST-01..ST-09 的枚举值。
	State string `json:"state"`
	// SessionID 当前会话 ID（idle 时为空）。
	SessionID string `json:"sessionId"`
	// AudioFormat is the format fixed at the current session's start.
	AudioFormat    string `json:"audioFormat"`
	NoAudioPersist bool   `json:"noAudioPersist"`
	// Degraded ST-07 正交标记：模型来源是否回退过。
	Degraded bool `json:"degraded"`
	// DegradeNote 降级原因（已脱敏）。
	DegradeNote string `json:"degradeNote"`
	// Throttled NFR-08 正交标记：正在降档。
	Throttled bool `json:"throttled"`
	// ThrottleLevel L0|L1|L2|L3|P。
	ThrottleLevel string `json:"throttleLevel"`
	// Suspended NFR-08 正交标记：预算耗尽已暂停推理。
	Suspended bool `json:"suspended"`
	// Lagging 转写落后于音频（暂停期或降档期）。
	Lagging bool `json:"lagging"`
	// StartedAt RFC3339Nano（wall clock，仅显示用）。
	StartedAt string `json:"startedAt"`
	// ElapsedMs 逻辑时长（帧计数换算，ARCH §4.5.1）。
	ElapsedMs int64 `json:"elapsedMs"`
	// SourceID 当前源。
	SourceID string `json:"sourceId"`
	// ModelSource local | online。
	ModelSource string `json:"modelSource"`
	// EngineID bundleId@version 或 online:<model>。
	EngineID string `json:"engineId"`
	// LastError 最近一次错误（成功时为空）。
	LastError *ErrorDTO `json:"lastError"`
}

// PerfUsage NFR-08 的三个分量读数（EV-15 / API-9.10）。
type PerfUsage struct {
	// CapPct 用户设定的整机预算 N。
	CapPct int `json:"capPct"`
	// TotalCpuPct 整机 CPU 占用（按核数归一化）。
	TotalCpuPct int `json:"totalCpuPct"`
	// SelfCpuPct 本应用（含引擎子进程）。
	SelfCpuPct int `json:"selfCpuPct"`
	// OtherPct 其他应用 = total − self。
	OtherPct int `json:"otherPct"`
	// HeadroomPct 本应用当前可用额度 = N − other。
	HeadroomPct int `json:"headroomPct"`
	// Level L0|L1|L2|L3|P。
	Level string `json:"level"`
	// Suspended 是否因预算耗尽暂停推理。
	Suspended bool `json:"suspended"`
	// WindowMs 判据窗口（固定 5000）。
	WindowMs int64 `json:"windowMs"`
	// SampledAt RFC3339Nano。
	SampledAt string `json:"sampledAt"`
}

// StartRequest 录制启动参数（API-2.1）。
type StartRequest struct {
	// SourceID 目标源。
	SourceID string `json:"sourceId"`
	// ModelSource 空则用 app_settings 当前值。
	ModelSource string `json:"modelSource"`
	// Title 空则自动命名（日期 + 来源应用）。
	Title string `json:"title"`
}

// ErrorDTO 是 apierr.Err 的纯数据投影，用于嵌套在其它 DTO 里（避免 contract 依赖 apierr）。
type ErrorDTO struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Action    string `json:"action,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Retryable bool   `json:"retryable"`
}

// RecordingLevel 采集电平（EV-02，10Hz 节流）。
type RecordingLevel struct {
	SessionID string `json:"sessionId"`
	// RMS 均方根电平 0–1。
	RMS float32 `json:"rms"`
	// Peak 峰值 0–1。
	Peak float32 `json:"peak"`
	// SilentMs 连续静音时长（用于 FR-03-7 无音频提示）。
	SilentMs int64 `json:"silentMs"`
}

// RecordingTick 采集节拍（EV-03，1Hz）。
type RecordingTick struct {
	SessionID string `json:"sessionId"`
	// ElapsedMs 逻辑时长。
	ElapsedMs int64 `json:"elapsedMs"`
	// BytesWritten 已写字节（UI 显示「已存 x MB」）。
	BytesWritten int64 `json:"bytesWritten"`
	// CharCount 仅边录边转时有意义。
	CharCount int `json:"charCount"`
}
