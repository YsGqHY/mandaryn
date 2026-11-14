package storage

// 声纹身份与角色映射模型，对应 DOC-04 TBL-05 / TBL-06 / TBL-07。

// Speaker 是**全局**声纹身份（TBL-05）。
//
// 向量必须加密落库（cryptox），因此不能做 SQL 向量检索：
// 启动时解密并注入内存注册表（sherpa-offline SpeakerEmbeddingManager，ARCH §4.4）。
// 规模假设：单用户 ≤ 5000 个说话人，全量加载 ≈ 5MB，可接受。
type Speaker struct {
	ID string `gorm:"primaryKey;size:32"` // sp_<ULID>
	// EmbeddingEnc 是 AES-GCM 密文（base64），禁止明文落库。
	EmbeddingEnc string `gorm:"type:text"`
	Dim          int    `gorm:"not null;default:0"`
	// ModelID 产出该向量的声纹模型；换模型后跨模型不比较相似度（DOC-04 §4.3）。
	ModelID string `gorm:"size:128"`

	FirstSeenAt int64 `gorm:"not null"`
	LastSeenAt  int64 `gorm:"not null"`
	SessionCount int  `gorm:"not null;default:0"`
	TotalSpeechMs int64 `gorm:"not null;default:0"`
}

func (Speaker) TableName() string { return "speakers" }

// SessionSpeaker 把「会话 × 说话人」映射到一个角色（TBL-06）。
//
// role_id 刻意**不建**外键约束：「删除角色」的语义是把 role_id 指回 rl_unknown
// （DOC-04 §4.1），用代码显式 UPDATE 比 SQLite 的 ON DELETE SET DEFAULT
// （要求默认值对应的行预存且受外键检查）更可控。
type SessionSpeaker struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	SessionID string `gorm:"size:32;not null;uniqueIndex:uq_session_speakers,priority:1"`
	SpeakerID string `gorm:"size:32;not null;uniqueIndex:uq_session_speakers,priority:2;index:idx_session_speakers_speaker"`
	RoleID    string `gorm:"size:32;not null;index:idx_session_speakers_role"`

	UtteranceCount int   `gorm:"not null;default:0"`
	SpeechMs       int64 `gorm:"not null;default:0"`

	Session *Session `gorm:"foreignKey:SessionID;constraint:OnDelete:CASCADE"`
	Speaker *Speaker `gorm:"foreignKey:SpeakerID;constraint:OnDelete:CASCADE"`
}

func (SessionSpeaker) TableName() string { return "session_speakers" }

// Role 是用户可见的角色标签（TBL-07）。
//
// 改名 = `UPDATE roles SET name=?` 一行，历史会话立即生效（FR-06-3）。
// ColorToken 只存主题令牌（如 role-3），**不存十六进制**（前端配色铁律）。
type Role struct {
	ID         string `gorm:"primaryKey;size:32"` // rl_<ULID>；内置 rl_unknown
	Name       string `gorm:"size:64;not null"`
	ColorToken string `gorm:"size:32;not null"`
	// IsSystem 1 = 「未识别」内置角色，不可删。
	IsSystem  bool  `gorm:"not null;default:false"`
	CreatedAt int64 `gorm:"autoCreateTime:milli"`
	UpdatedAt int64 `gorm:"autoUpdateTime:milli"`
}

func (Role) TableName() string { return "roles" }
