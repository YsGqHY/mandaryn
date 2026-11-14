package storage

import "gorm.io/gorm"

// ModelDescriptor 是模型在持久化层的"元数据"包装：
// 除了 GORM struct 本身，还附带表名 / i18n 标签 key / 是否允许"手动清空"。
//
// 这样数据存储设置页可以列出每张表的占用情况、并对"非敏感、可重建"的表
// 提供一键清空。新增表时遵循同样规范，i18n 键名由开发者填，UI 自动展示。
type ModelDescriptor struct {
	// Model 是 GORM 模型实例的指针（&Preference{}）。
	Model any
	// TableName SQLite 中的实际表名。
	// 必须显式提供：业务方可能用 GORM TableName() 钩子重命名，避免推断不一致。
	TableName string
	// LabelKey 设置页展示的 i18n 路径，文案在
	// settings.database.tables.<key>.{label,description}。
	LabelKey string
	// Clearable=true 表示允许在设置页手动清空。
	// 涉及"用户输入" / "敏感凭据" / "无法重建的关键状态"的表设为 false。
	// 当前两张表的判断：
	//   - preferences   = true：UI 行为开关，重置即默认
	//   - app_settings  = false：主题选择 / 自定义主题 / 语言，"全清"会重置用户视觉设置，应让用户走具体的"重置"动作
	Clearable bool
}

// AllModels 是 AutoMigrate 与设置页的注册中心。
// 加新表只需要：
//  1. 在 storage 包内新建 *.go 文件定义 GORM 结构体
//  2. 把对应 ModelDescriptor 追加到这里
//  3. 重启应用 → 自动建表
//
// AutoMigrate 会自动做：建表、加列、加索引、加外键、改列类型。
// 不会做：删列、删表、重命名（保留旧 schema 不丢数据，但不会清理）。
//
// 重命名字段需手写一次性数据迁移钩子；这种情况极少见。
var AllModels = []ModelDescriptor{
	{
		Model:     &Preference{},
		TableName: "preferences",
		LabelKey:  "preferences",
		Clearable: true,
	},
	{
		Model:     &AppSettings{},
		TableName: "app_settings",
		LabelKey:  "appSettings",
		Clearable: false,
	},

	// —— 业务表（DOC-04 §2）——
	// Clearable 的判断口径：清空后是否会**不可恢复地丢用户数据**。
	// sessions / segments / speakers 承载不可再生数据（录音、正文、声纹），
	// 一律 false —— 删除必须走业务动作（带二次确认与音频清理），
	// 不允许在「数据存储」页一键清空。
	{
		Model:     &Session{},
		TableName: "sessions",
		LabelKey:  "sessions",
		Clearable: false,
	},
	{
		Model:     &Segment{},
		TableName: "segments",
		LabelKey:  "segments",
		Clearable: false,
	},
	{
		Model:     &SessionEvent{},
		TableName: "session_events",
		LabelKey:  "sessionEvents",
		Clearable: false,
	},
	{
		Model:     &Speaker{},
		TableName: "speakers",
		LabelKey:  "speakers",
		Clearable: false,
	},
	{
		Model:     &SessionSpeaker{},
		TableName: "session_speakers",
		LabelKey:  "sessionSpeakers",
		Clearable: false,
	},
	{
		Model:     &Role{},
		TableName: "roles",
		LabelKey:  "roles",
		Clearable: false,
	},
	{
		Model:     &Bundle{},
		TableName: "bundles",
		LabelKey:  "bundles",
		Clearable: false,
	},
	{
		// 下载状态可清：清掉只是失去断点续传信息，重新下载即可恢复。
		Model:     &Download{},
		TableName: "downloads",
		LabelKey:  "downloads",
		Clearable: true,
	},
	{
		Model:     &DataMigration{},
		TableName: "data_migrations",
		LabelKey:  "dataMigrations",
		Clearable: false,
	},
	{
		// 出站记录可清：纯诊断信息，清空不影响任何功能。
		Model:     &OutboundLog{},
		TableName: "outbound_log",
		LabelKey:  "outboundLog",
		Clearable: true,
	},
}

// Preference 行式偏好存储：每个键一行，便于增量加新键不改 schema。
// Value 用 TEXT 存 JSON，让前端可以直接放任意可序列化值。
type Preference struct {
	Key       string `gorm:"primaryKey;size:128"`
	Value     string `gorm:"type:text;not null"`
	UpdatedAt int64  `gorm:"autoUpdateTime:milli"`
}

// AppSettings 是单行表（CHECK id=1 锁死）：
// 保存"必有且唯一"的应用级配置。和 Preference 的区别：
//   - 这里的字段是已知有限集，schema 强校验
//   - Preference 是开放键值，业务方可以随时塞新键
//
// 列定义严格对齐 DOC-04 TBL-02（含 UX-SPEC §3 的 25 项可自定义项）。
// 默认值必须与 UX-SPEC §3 一致，三处（DOC-04 / DOC-05 / 本结构体）同步改。
type AppSettings struct {
	ID uint `gorm:"primaryKey;check:id = 1"`

	// —— 既有（主题 / 语言），保持不动以免视觉与 i18n 回退 ——
	ThemeChoice  string `gorm:"size:64;not null;default:'system'"`
	CustomTheme  string `gorm:"type:text"`                       // JSON: { mode, palette }
	LocaleChoice string `gorm:"size:32;not null;default:'auto'"` // 'auto' | LocaleCode

	// —— FR-01 模型来源与在线端点 ——
	ModelSource    string `gorm:"size:16;not null;default:'local'"` // local | online
	OnlineEndpoint string `gorm:"size:512"`
	OnlineModel    string `gorm:"size:128"`
	OnlineLanguage string `gorm:"size:16;default:'auto'"`
	// OnlineAPIKeyEnc 是 AES-GCM 密文（cryptox），界面只显掩码。
	OnlineAPIKeyEnc string `gorm:"type:text"`
	// OnlineConsentAt 0 = 未确认；>0 = 已确认时间（ms）（FR-08-2）。
	OnlineConsentAt int64 `gorm:"not null;default:0"`
	// OnlineConsentHost 确认时绑定的域名，变更需重新确认。
	OnlineConsentHost string `gorm:"size:255"`

	// —— 降级策略（ARCH §4.2.4）——
	FallbackToLocal bool `gorm:"not null;default:true"`
	// AutoSwitchBack 默认关：避免网络抖动造成反复切换。
	AutoSwitchBack bool `gorm:"not null;default:false"`
	// MaxRetries 在线失败的重试次数（0–10，默认 2）；存库后重启不再回落到默认值。
	MaxRetries int `gorm:"not null;default:2"`

	// —— 代理（UX-SPEC §4.2 合并为单一入口）——
	ProxyType     string `gorm:"size:16;default:'none'"` // none | http | https | socks5
	ProxyHost     string `gorm:"size:255"`
	ProxyPort     int
	ProxyUsername string `gorm:"size:128"`
	// ProxyPasswordEnc AES-GCM 密文。
	ProxyPasswordEnc string `gorm:"type:text"`
	ProxyForDownload bool   `gorm:"not null;default:true"`
	ProxyForOnline   bool   `gorm:"not null;default:true"`
	ProxyNoProxy     string `gorm:"size:512"`

	// —— 模型根目录（FR-07，ADR-02）——
	// ModelRoot 语义是**用户层的值**：即使被 env/cli 接管也原样保留，
	// 供 UI 展示「你的设置（未生效）」（DOC-02 §4.8）。
	ModelRoot string `gorm:"size:1024;default:''"`
	// ModelRootPending 非空表示受管期间用户改过的目录，解除接管并重启后自动生效。
	ModelRootPending string `gorm:"size:1024;default:''"`

	// —— 音频与录制（FR-03）——
	SaveFormat      string `gorm:"size:8;default:'wav'"`       // wav | flac
	Sensitivity     string `gorm:"size:16;default:'standard'"` // standard | noisy
	MaxSpeakers     int    `gorm:"not null;default:0"`         // 0 = auto，2–8
	DefaultSourceID string `gorm:"size:255"`
	AutoName        bool   `gorm:"not null;default:true"`

	// —— 边录边转（FR-03-10）——
	// LiveTranscribe 默认 **false**：录制与转写解耦是产品默认形态，
	// 实时出字是显式开关（会额外占 CPU，且需要流式模型包）。
	LiveTranscribe bool `gorm:"not null;default:false"`

	// —— 隐私 ——
	NoAudioPersist bool   `gorm:"not null;default:false"`
	LogLevel       string `gorm:"size:8;default:'info'"` // info | debug | off

	// —— NFR-08 整机性能预算 ——
	// ResourceCapPct 是**整机**预算 N（不是本应用上限）。
	// 步进 5 由应用层校验（SQLite 无法表达步进），越界返回 ERR_PERF_CAP_RANGE。
	ResourceCapPct int `gorm:"not null;default:80;check:chk_app_settings_cap,resource_cap_pct >= 10 AND resource_cap_pct <= 100"`

	UpdatedAt int64 `gorm:"autoUpdateTime:milli"`
}

// modelObjs 把 ModelDescriptor 列表展开成 AutoMigrate 需要的 any 切片。
func modelObjs() []any {
	out := make([]any, 0, len(AllModels))
	for _, m := range AllModels {
		out = append(out, m.Model)
	}
	return out
}

// autoMigrate 把所有 model 同步到当前数据库。
func autoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(modelObjs()...)
}

// FindDescriptor 按表名查找 ModelDescriptor；找不到返回 nil。
// storagesvc.ClearTable 用它做白名单校验，避免任意 SQL 注入。
func FindDescriptor(tableName string) *ModelDescriptor {
	for i := range AllModels {
		if AllModels[i].TableName == tableName {
			return &AllModels[i]
		}
	}
	return nil
}

// ensureSingletonSettings 保证 app_settings 至少存在一行，让上层
// 可以直接 First / Save 而不需要 First or Create 两段逻辑。
//
// 注意：这里必须**显式**写出所有非零默认值。GORM 的 Create 会把结构体零值
// 一并写进 INSERT，从而覆盖掉 SQL 列默认值（尤其 resource_cap_pct 会变成 0
// 并触发 CHECK 失败）。默认值口径见 DOC-04 TBL-02。
func ensureSingletonSettings(db *gorm.DB) error {
	var count int64
	if err := db.Model(&AppSettings{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return db.Create(&AppSettings{
		ID:              1,
		ThemeChoice:     "system",
		LocaleChoice:    "auto",
		ModelSource:     "local",
		OnlineLanguage:  "auto",
		FallbackToLocal: true,
		AutoSwitchBack:  false,
		// MaxRetries 必须显式写非零值：GORM 的 Create 会把结构体零值写进列，
		// 覆盖 SQL 的 default:2（与 S0 遇到的 resource_cap_pct 同类问题）。
		MaxRetries:       2,
		ProxyType:        "none",
		ProxyForDownload: true,
		ProxyForOnline:   true,
		SaveFormat:       "wav",
		Sensitivity:      "standard",
		MaxSpeakers:      0,
		AutoName:         true,
		// LiveTranscribe 默认关，但**必须显式写**：GORM 的 Create 会把结构体零值
		// 写进 INSERT，覆盖 SQL 的 default:false —— 值恰好一致，但把「默认关」
		// 表达成「零值」会让以后改默认值时的行为不可预测（同类问题见 resource_cap_pct）。
		LiveTranscribe: false,
		NoAudioPersist: false,
		LogLevel:       "info",
		ResourceCapPct: 80,
	}).Error
}
