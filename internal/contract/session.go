package contract

// EngineStatus 引擎状态（EV-08 / API-5.1）。
type EngineStatus struct {
	// Source local | online（用户选择的来源）。
	Source string `json:"source"`
	// State loading|ready|failed|degraded|paused。
	State string `json:"state"`
	// EngineID bundleId@version 或 online:<model>。
	EngineID string `json:"engineId"`
	// LoadMs 预热耗时。
	LoadMs int64 `json:"loadMs"`
	// Degraded 是否处于降级（在线失败已回退本地）。
	Degraded bool `json:"degraded"`
	// DegradeNote 降级原因。
	DegradeNote string `json:"degradeNote"`
	// LastError 最近一次错误。
	LastError *ErrorDTO `json:"lastError"`
}

// 引擎状态取值（EngineStatus.State）。
const (
	EngineStateLoading  = "loading"
	EngineStateReady    = "ready"
	EngineStateFailed   = "failed"
	EngineStateDegraded = "degraded"
	EngineStatePaused   = "paused"
)

// OnlineConfig 在线端点配置（API-5.4 / API-5.6）。
type OnlineConfig struct {
	// Endpoint 必须 https。
	Endpoint string `json:"endpoint"`
	// Model 端点模型名。
	Model string `json:"model"`
	// Language auto | zh | en | ...
	Language string `json:"language"`
	// APIKey 读取时为掩码（sk-***last4），写入时传明文。
	APIKey string `json:"apiKey"`
	// TimeoutMs 首字节超时。
	TimeoutMs int64 `json:"timeoutMs"`
}

// FallbackPolicy 降级策略（API-5.7）。
type FallbackPolicy struct {
	// FallbackToLocal 默认 true。
	FallbackToLocal bool `json:"fallbackToLocal"`
	// AutoSwitchBack 默认 false（不自动切回在线，ARCH §4.2.4）。
	AutoSwitchBack bool `json:"autoSwitchBack"`
	// MaxRetries 默认 2。
	MaxRetries int `json:"maxRetries"`
}

// TestResult 连通性测试结果（API-5.4 / API-9.4）。
type TestResult struct {
	// OK 是否成功。
	OK bool `json:"ok"`
	// LatencyMs 往返延迟。
	LatencyMs int64 `json:"latencyMs"`
	// Category 错误分类（network | timeout | auth | rate-limit | proxy | tls | unknown）。
	Category string `json:"category"`
	// Note 已脱敏的说明。
	Note string `json:"note"`
}

// Segment 定稿片段（EV-05 / Timeline）。
//
// 注意：RoleName 由 roles JOIN 得到，**不是** segments 表里的冗余列
// （FR-06-3 改名全局同步的唯一正确实现，ARCH §4.4）。
type Segment struct {
	ID        int64  `json:"id"`
	SessionID string `json:"sessionId"`
	// Seq 会话内单调递增（从 0）。
	Seq int `json:"seq"`
	// StartMs 时间轴起点（VAD 端点）。
	StartMs int64 `json:"startMs"`
	// EndMs 时间轴终点。
	EndMs int64 `json:"endMs"`
	// Text 定稿文本。
	Text string `json:"text"`
	// SpeakerID 空 = 未识别。
	SpeakerID string `json:"speakerId"`
	// RoleID 角色 ID。
	RoleID string `json:"roleId"`
	// RoleName 由 roles join 得到，非冗余存储。
	RoleName string `json:"roleName"`
	// EngineID 产出该片段的引擎（降级前后可不同）。
	EngineID string `json:"engineId"`
	// Finalized false 表示 partial（仅边录边转）。
	Finalized bool `json:"finalized"`
	// Confidence 0–1。
	Confidence float32 `json:"confidence"`
}

// Marker 时间轴标记（DOC-04 TBL-10）。
type Marker struct {
	AtMs int64  `json:"atMs"`
	Kind string `json:"kind"` // degrade | source-lost | device-switch | paused | resumed | perf-throttle | perf-suspend | perf-resume
	Note string `json:"note"`
}

// 时间轴标记类型。
const (
	MarkerDegrade      = "degrade"
	MarkerSourceLost   = "source-lost"
	MarkerDeviceSwitch = "device-switch"
	MarkerPaused       = "paused"
	MarkerResumed      = "resumed"
	MarkerPerfThrottle = "perf-throttle"
	MarkerPerfSuspend  = "perf-suspend"
	MarkerPerfResume   = "perf-resume"
)

// Session 会话（录音实例）元数据（API-3.x）。
type Session struct {
	ID string `json:"id"`
	// Title 默认「YYYY-MM-DD HH:mm · KOOK」。
	Title      string `json:"title"`
	SourceID   string `json:"sourceId"`
	SourceName string `json:"sourceName"`
	// ModelSource local | online（导出保留，FR-08-3）。
	ModelSource string `json:"modelSource"`
	EngineID    string `json:"engineId"`
	// Status 录制状态：completed | interrupted | failed | degraded。
	Status    string `json:"status"`
	Degraded  bool   `json:"degraded"`
	StartedAt string `json:"startedAt"`
	EndedAt   string `json:"endedAt"`
	// DurationMs 逻辑时长（帧计数）。
	DurationMs   int64 `json:"durationMs"`
	SegmentCount int   `json:"segmentCount"`
	SpeakerCount int   `json:"speakerCount"`
	// AudioPath 相对 dataRoot。
	AudioPath string `json:"audioPath"`
	SizeBytes int64  `json:"sizeBytes"`
	// TranscriptionStatus none|queued|running|done|failed（独立轴）。
	TranscriptionStatus string `json:"transcriptionStatus"`
	// TranscribeProgressMs 已转写位置（断点续跑）。
	TranscribeProgressMs int64 `json:"transcribeProgressMs"`
	// CharCount 转写完成后回填。
	CharCount int64 `json:"charCount"`
}

// Timeline 整段会话时间轴（API-4.1）。
type Timeline struct {
	SessionID  string    `json:"sessionId"`
	DurationMs int64     `json:"durationMs"`
	Segments   []Segment `json:"segments"`
	Markers    []Marker  `json:"markers"`
}

// TimelinePage 超大通话分页（API-4.2）。
//
// DurationMs 与 Markers 随页一起给：超大通话只能分页取片段，但播放器同时需要
// 「总时长」与「降档/暂停标记」。以前这两项只有整段 Timeline 才有，导致前端为了
// 拿标记不得不再发一次整段请求（反而把要大通话省下来的数据全拉回来）。
type TimelinePage struct {
	SessionID string    `json:"sessionId"`
	Total     int       `json:"total"`
	Offset    int       `json:"offset"`
	Limit     int       `json:"limit"`
	Segments  []Segment `json:"segments"`
	// DurationMs 会话总时长（毫秒）。
	DurationMs int64 `json:"durationMs"`
	// Markers 时间轴标记（性能降档/暂停/恢复等）。
	//
	// 仅在 offset==0 时填充：标记数量少但每页都重复下发没有意义，
	// 前端首屏拿到后自行保留（见 docs/API.md API-4.2）。
	Markers []Marker `json:"markers"`
}

// TranscriptionState 转写任务状态（EV-16 / API-4.5）。
type TranscriptionState struct {
	SessionID string `json:"sessionId"`
	// Status none | queued | running | done | failed。
	Status string `json:"status"`
	// ProgressMs 已转写的音频位置。
	ProgressMs int64 `json:"progressMs"`
	// AudioMs 音频总时长。
	AudioMs int64 `json:"audioMs"`
	// Segments 已落库句数。
	Segments int    `json:"segments"`
	EngineID string `json:"engineId"`
	// Trigger manual | idle。
	Trigger    string `json:"trigger"`
	ErrorCode  string `json:"errorCode,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// 转写触发来源。
const (
	TriggerManual = "manual"
	TriggerIdle   = "idle"
)

// ListQuery 会话库查询（API-3.1）。
type ListQuery struct {
	// Keyword 标题 + 片段全文。
	Keyword      string `json:"keyword"`
	SourceIDLike string `json:"sourceIdLike"`
	// Status 空 = 全部。
	Status string `json:"status"`
	// ModelSource local|online|""（FR-08-3 筛选在线会话）。
	ModelSource string `json:"modelSource"`
	// TranscriptionStatus 空 = 全部；用于「待转写」筛选。
	TranscriptionStatus string `json:"transcriptionStatus"`
	// From / To RFC3339，含。
	From   string `json:"from"`
	To     string `json:"to"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// ListResult 分页结果（API-3.1）。
type ListResult struct {
	Total int       `json:"total"`
	Items []Session `json:"items"`
}

// ExportRequest 导出请求（API-3.5）。
type ExportRequest struct {
	SessionIDs []string `json:"sessionIds"`
	// Format txt | srt | csv | xlsx。
	Format            string `json:"format"`
	IncludeTimestamps bool   `json:"includeTimestamps"`
	IncludeSpeakers   bool   `json:"includeSpeakers"`
	// TargetPath 空则弹原生「另存为」。
	TargetPath string `json:"targetPath"`
}

// SegmentQuery 片段级检索（API-4.3）。
type SegmentQuery struct {
	Keyword   string `json:"keyword"`
	SessionID string `json:"sessionId"`
	SpeakerID string `json:"speakerId"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}
