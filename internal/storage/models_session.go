package storage

// 会话（录音实例）与时间轴片段模型，对应 DOC-04 TBL-03 / TBL-04 / TBL-10。
//
// 设计要点：
//   - `Session.Status` 只表达**录制**状态（与 DOC-02 §6 的 ST 对齐）；
//     转写进度走 `TranscriptionStatus`（另一条轴，禁止混入 Status）；
//   - `Segment` **不存角色名**，只存 `SpeakerID`：角色名靠 roles JOIN，
//     这是 FR-06-3「改名全局同步、禁止新旧名并存」的唯一正确实现；
//   - `AudioPath` 存**相对 dataRoot** 的路径，数据目录迁移不需要改库。

// Session 是录音实例的元数据（TBL-03）。
type Session struct {
	ID         string `gorm:"primaryKey;size:32"` // s_<ULID>
	Title      string `gorm:"size:255;not null"`
	SourceID   string `gorm:"size:255;index:idx_sessions_source"`
	SourceName string `gorm:"size:128"`
	SourceKind string `gorm:"size:16"` // application | device

	ModelSource string `gorm:"size:16;not null;index:idx_sessions_model_source"`
	EngineID    string `gorm:"size:128"`
	Status      string `gorm:"size:16;not null;index:idx_sessions_status"`

	Degraded      bool   `gorm:"not null;default:false"`
	DegradeReason string `gorm:"size:255"`

	StartedAt  int64 `gorm:"not null;index:idx_sessions_started,sort:desc"`
	EndedAt    int64
	DurationMs int64 `gorm:"not null;default:0"` // 逻辑时长（帧计数换算）

	AudioPath   string `gorm:"size:512"`
	AudioFormat string `gorm:"size:8"`
	VadPreset   string `gorm:"column:vad_preset;size:24;not null;default:'standard-v1'"`
	SizeBytes   int64  `gorm:"not null;default:0"`

	SegmentCount int   `gorm:"not null;default:0"`
	SpeakerCount int   `gorm:"not null;default:0"`
	CharCount    int64 `gorm:"not null;default:0"`

	// —— 转写任务轴（与 Status 正交，DOC-02 §6.4）——
	TranscriptionStatus  string `gorm:"size:16;not null;default:'none';index:idx_sessions_transcription,priority:1"`
	TranscribedAt        int64
	TranscribeEngineID   string `gorm:"size:128"`
	LiveTranscribed      bool   `gorm:"not null;default:false"`
	TranscribeProgressMs int64  `gorm:"not null;default:0"`

	CreatedAt int64 `gorm:"autoCreateTime:milli"`
	UpdatedAt int64 `gorm:"autoUpdateTime:milli"`
}

// TableName 显式声明，避免依赖 GORM 复数推断。
func (Session) TableName() string { return "sessions" }

// Segment 是时间轴片段（TBL-04）。
//
// 不变量（ARCH §4.5.2，写入时必须断言）：
//   - `(SessionID, Seq)` 唯一 → 重转幂等覆盖，不产生重复句；
//   - `StartMs <= EndMs` 且 `StartMs >= 上一句.StartMs` → 前端二分定位的前提。
type Segment struct {
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	SessionID string `gorm:"size:32;not null;uniqueIndex:uq_segments_session_seq,priority:1;index:idx_segments_timeline,priority:1"`
	Seq       int    `gorm:"not null;uniqueIndex:uq_segments_session_seq,priority:2"`

	StartMs int64  `gorm:"not null;index:idx_segments_timeline,priority:2"`
	EndMs   int64  `gorm:"not null"`
	Text    string `gorm:"type:text;not null"`

	// SpeakerID 空 = 未识别（重叠语音不强行拆分，ARCH §4.4）。
	SpeakerID  string `gorm:"size:32;index:idx_segments_speaker"`
	Confidence float64
	EngineID   string `gorm:"size:128"`

	IsFinal   bool  `gorm:"not null;default:true"`
	Edited    bool  `gorm:"not null;default:false"`
	CreatedAt int64 `gorm:"autoCreateTime:milli"`

	// Session 关联只为让 AutoMigrate 生成 ON DELETE CASCADE；
	// 业务代码不需要给这个字段赋值（GORM 允许 nil 关联）。
	Session *Session `gorm:"foreignKey:SessionID;constraint:OnDelete:CASCADE"`
}

func (Segment) TableName() string { return "segments" }

// SessionEvent 是时间轴标记（TBL-10）：录制中的异常在回放时可见。
type SessionEvent struct {
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	SessionID string `gorm:"size:32;not null;index:idx_session_events,priority:1"`
	AtMs      int64  `gorm:"not null;index:idx_session_events,priority:2"`
	// Kind degrade|source-lost|device-switch|paused|resumed|perf-throttle|perf-suspend|perf-resume。
	Kind string `gorm:"size:32;not null"`
	Note string `gorm:"size:255"`

	Session *Session `gorm:"foreignKey:SessionID;constraint:OnDelete:CASCADE"`
}

func (SessionEvent) TableName() string { return "session_events" }

// DataMigration 记录一次性数据迁移钩子是否已执行（TBL-11）。
// GORM AutoMigrate 不做删列 / 改名 / 数据搬迁，那些变更走 migrations.go 具名钩子。
type DataMigration struct {
	Name      string `gorm:"primaryKey;size:128"`
	AppliedAt int64  `gorm:"not null"`
	Note      string `gorm:"size:255"`
}

func (DataMigration) TableName() string { return "data_migrations" }
