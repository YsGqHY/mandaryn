package storage

// 模型包注册表与下载断点模型，对应 DOC-04 TBL-08 / TBL-09。

// Bundle 是已装模型包的注册表行（TBL-08）。
//
// Path 存**相对 modelRoot** 的路径，因此模型根整体迁移后无需改库。
type Bundle struct {
	// ID = bundleId@version，作为主键便于按「包 + 版本」幂等 upsert。
	ID string `gorm:"primaryKey;size:128"`
	BundleID string `gorm:"size:64;not null;uniqueIndex:uq_bundles_bundle_version,priority:1"`
	Version  string `gorm:"size:32;not null;uniqueIndex:uq_bundles_bundle_version,priority:2"`

	Name      string `gorm:"size:128"`
	Path      string `gorm:"size:1024"`
	SizeBytes int64  `gorm:"not null;default:0"`
	SHA256    string `gorm:"size:64"`
	VerifiedAt int64

	// State installed | corrupt | missing。
	State string `gorm:"size:16;index:idx_bundles_state"`
	// Roles / Languages 存 JSON 数组文本（SQLite 无数组类型）。
	Roles     string `gorm:"type:text"`
	Languages string `gorm:"type:text"`

	License  string `gorm:"size:64"`
	RAMHintMB int
	InstalledAt int64 `gorm:"autoCreateTime:milli"`
}

func (Bundle) TableName() string { return "bundles" }

// Download 是断点续传状态（TBL-09）。
//
// 分片文件本身在 `<modelRoot>/.downloads/`，本表只存**状态**；
// 两者不一致时以表为准并清理孤儿分片（DOC-04 §4.4）。
type Download struct {
	// ID = bundleId@version。
	ID string `gorm:"primaryKey;size:128"`
	// URLIdx 当前镜像索引（失败切镜像）。
	URLIdx int `gorm:"not null;default:0"`
	// ETag 续传判据：ETag 变则整包重下（避免拼接脏数据，ARCH §4.6.2）。
	ETag string `gorm:"size:128"`

	BytesDone  int64 `gorm:"not null;default:0"`
	BytesTotal int64 `gorm:"not null;default:0"`
	// PartsDone 已完成分片序号数组的 JSON 文本。
	PartsDone string `gorm:"type:text"`
	Retries   int    `gorm:"not null;default:0"`

	// State queued|downloading|verifying|paused|failed。
	State string `gorm:"size:16"`
	// LastError 错误码 + 摘要（不含凭据）。
	LastError string `gorm:"size:255"`
	UpdatedAt int64  `gorm:"autoUpdateTime:milli"`
}

func (Download) TableName() string { return "downloads" }

// OutboundLog 是出站留痕（TBL-12，FR-08 / NFR-03）。
//
// 只存**域名**而非完整 URL：避免 query 里的凭据 / 参数泄漏。
// 保留策略：仅最近 24h（启动时清理）。
type OutboundLog struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	At int64 `gorm:"not null;index:idx_outbound_at"`
	Host string `gorm:"size:255"`
	// Kind model-download | online-asr。
	Kind string `gorm:"size:16"`
	// Audio 是否包含音频（1 = 是）。
	Audio bool `gorm:"not null;default:false"`
	Bytes int64
	// Result ok 或错误码。
	Result string `gorm:"size:32"`
}

func (OutboundLog) TableName() string { return "outbound_log" }
