// Package transcribe 是「延后转写任务」的 L2 领域层（FR-03-4 / FR-03-10，ARCH §1.2 / §6.4）。
//
// 职责边界：
//   - 对**已封存**的录音实例读取音频 → VAD 分句 → 逐句调引擎 → 片段落库；
//   - 维护转写任务轴 `transcription_status`（与录制主状态 `sessions.state` **正交**）；
//   - 幂等覆盖（`(session_id, seq)`）、进度与断点续跑、并发上限 1、空闲/抢占调度；
//   - 时间轴查询（片段 + 会话事件标记）。
//
// 铁律：
//   - 本包**不 import Wails**，领域逻辑可被单测直接驱动；状态通知走注入回调
//     `func(contract.TranscriptionState)`，EV-16 `transcription:state` 由装配层转发。
//   - **绝不**写 `sessions.state`，**绝不**新增 ST 编号（ARCH §6.4 约束）。
//   - 转写失败**不得**删除或改写音频与其元数据；片段只按 seq 覆盖，不删旧行。
//   - 音频内容与转写正文**永不进日志**（只记时长 / 句数 / 错误码，NFR-03-5）。
//   - 音频读取自己实现（os + WAV 解析），**不 import internal/recording**（那是 S1 的域）。
package transcribe

import (
	"context"

	"vip.oberon.mandaryn/internal/asr"
)

// SessionRow 是转写任务需要的会话元数据（TBL-03 的投影）。
//
// 只投影转写需要的列：音频位置、时长、以及转写轴的四个字段。
// `State` 是录制主状态，**只读**——本包任何写入路径都不得改它（ARCH §6.4）。
type SessionRow struct {
	ID string
	// Title 仅用于日志（不含正文）。
	Title string
	// AudioPath 相对 dataRoot（TBL-03 约定），由 L3 负责解析成绝对路径。
	AudioPath string
	// DurationMs 逻辑时长（帧计数换算）。音频头缺失时作为兜底时长。
	DurationMs int64

	// State 录制主状态（ST-01..ST-09）。**只读**，禁止在本包内写入。
	State string

	// TranscriptionStatus none|queued|running|done|failed（独立轴，ARCH §6.4）。
	TranscriptionStatus string
	// TranscribeProgressMs 已转写位置（断点续跑锚点）。
	TranscribeProgressMs int64
	// TranscribeEngineID 实际产出片段的引擎。
	TranscribeEngineID string
	// TranscribedAt 转写完成时间（ms）。
	TranscribedAt int64
	// EngineID 会话主引擎（录制侧记录的来源）。
	EngineID string
	// LiveTranscribed 该会话录制时是否开启了「边录边转」（FR-03-10）。
	//
	// 为 true 时**不再**参与「空闲自动转写」排队（片段已在录制中产出），
	// 但用户仍可手动重转（幂等覆盖）。见 ShouldAutoTranscribe。
	LiveTranscribed bool
	// SegmentCount / CharCount 冗余计数，转写完回填。
	SegmentCount int
	CharCount    int64
}

// SegmentRow 是时间轴片段行（TBL-04）。
//
// 不变量（ARCH §4.5.2）：`(SessionID, Seq)` 唯一；`StartMs <= EndMs` 且
// `StartMs >= 上一句.StartMs`。写入方必须自己保证，Store 实现负责落库。
type SegmentRow struct {
	ID         int64
	SessionID  string
	Seq        int
	StartMs    int64
	EndMs      int64
	Text       string
	SpeakerID  string
	Confidence float32
	EngineID   string
	IsFinal    bool
	// Edited 用户人工修正过（API-4.4）；重转覆盖时**不得**覆盖它。
	Edited bool
}

// MarkerRow 是时间轴标记（TBL-10 `session_events`）。
type MarkerRow struct {
	AtMs int64
	Kind string
	Note string
}

// TranscriptionUpdate 是转写轴的增量更新。
//
// 指针字段为 nil 表示「本次不改该列」——这样「只把状态置 queued」不会顺手把
// progressMs / engineId 抹成零值（那会让断点续跑丢失进度）。
//
// 注意：这里**没有**也**不允许有**录制主状态的字段（ARCH §6.4）。
type TranscriptionUpdate struct {
	SessionID string
	// Status 目标转写状态；空串表示不改。
	Status string
	// ProgressMs 已转写位置；nil 不改。
	ProgressMs *int64
	// EngineID / TranscribedAt / SegmentCount / CharCount 同理。
	EngineID      *string
	TranscribedAt *int64
	SegmentCount  *int
	CharCount     *int64
}

// SegmentFilter 是片段查询条件（时间轴 / 分页 / 检索共用）。
//
// 范围语义：`FromMs`/`ToMs` 为 0 表示该端不限；两者都非 0 时是**闭区间**。
// 结果恒按 `seq` 升序（前端二分定位的前提，API §2.4「对齐契约」）。
type SegmentFilter struct {
	SessionID string
	// Keyword 片段全文包含匹配（空 = 不限）。
	Keyword string
	// SpeakerID 空 = 不限。
	SpeakerID string
	FromMs    int64
	ToMs      int64
	Offset    int
	Limit     int
}

// SegmentPage 是片段查询结果。
type SegmentPage struct {
	// Total 满足条件的总条数（不受 Offset/Limit 影响）。
	Total int
	// Rows 本次返回的行，按 seq 升序。
	Rows []SegmentRow
}

// Store 是 transcribe 依赖的持久化端口（由 L3 的 transcriptsvc 用 GORM 实现）。
//
// 用接口而不是直接依赖 storage：L2 保持无 Wails / 无 ORM，单测可用内存假实现；
// 同时 `(session_id, seq)` 的唯一约束与 upsert 语义由真实实现负责（见 transcriptsvc）。
type Store interface {
	// LoadSession 读取会话元数据；不存在 → ERR_NOT_FOUND。
	LoadSession(ctx context.Context, sessionID string) (SessionRow, error)
	// UpdateTranscription 只更新转写轴；**不得**触碰 sessions.state（ARCH §6.4）。
	UpdateTranscription(ctx context.Context, u TranscriptionUpdate) error
	// UpsertSegment 按 `(session_id, seq)` 幂等覆盖，返回落库后的行（带 ID）。
	// 已 deleted 的会话 → ERR_NOT_FOUND。
	UpsertSegment(ctx context.Context, row SegmentRow) (SegmentRow, error)
	// ListSegments 按 seq 升序返回片段；Offset/Limit 对 Rows 生效。
	ListSegments(ctx context.Context, f SegmentFilter) (SegmentPage, error)
	// ListMarkers 返回会话事件标记，按 at_ms 升序。
	ListMarkers(ctx context.Context, sessionID string) ([]MarkerRow, error)
	// UpdateSegmentText 人工修正文本（API-4.4），置 edited=true。
	UpdateSegmentText(ctx context.Context, segmentID int64, text string) error
	// MaxSeq 返回该会话已落库的最大 seq；无片段时返回 -1。
	MaxSeq(ctx context.Context, sessionID string) (int, error)
}

// AudioSource 是可随机读取的音频句柄（*os.File 天然满足）。
//
// 用 io.ReadSeeker 而不是整段读进内存：1 小时 16k 单声道 float32 约 230MB，
// 逐窗读取让内存只与「单句最大长度 + 一窗」相关。
type AudioSource interface {
	Read(p []byte) (int, error)
	Seek(offset int64, whence int) (int64, error)
	Close() error
}

// AudioOpener 由 L3 注入：把会话元数据解析成可读音频（dataRoot + audio_path）。
//
// 音频缺失 → ERR_NOT_FOUND；音频损坏 / 采样格式不支持 → 由 wav 解析层给错误。
// 放在 L3 是因为路径必须经 paths.Resolver（ADR-02），L2 不碰路径优先级。
type AudioOpener func(ctx context.Context, session SessionRow) (AudioSource, error)

// Recognizer 是本包对引擎的最小依赖（asr.Engine / asr.Router 都满足）。
//
// 只要 Finalize 与 ID：延后转写不用 Feed（那是边录边转）。
type Recognizer interface {
	// ID 返回 bundleId@version 或 online:<model>。
	ID() string
	// Finalize 对一整句做离线识别。
	Finalize(seg asr.AudioSegment) (asr.FinalResult, error)
}
