package contract

// 本文件是 S9-a（FR-08 隐私边界）新增的契约类型。
// 只做追加，不改既有 DTO；对应方法签名见 docs/API.md 待补条目（docs/DEV-ISSUES-S9a.md）。

// PrivacyStatus 是离线状态 + 近期出站汇总（设置页与诊断共用）。
type PrivacyStatus struct {
	// Offline 离线是否生效（env 与 user 取或）。
	Offline bool `json:"offline"`
	// OfflineSource "" | "env" | "user" | "env+user"，让用户知道是谁在生效。
	OfflineSource string `json:"offlineSource"`
	// OfflineEnvVar 接管变量名（MANDARYN_OFFLINE），便于 UI 说明「由环境变量接管」。
	OfflineEnvVar string `json:"offlineEnvVar"`
	// EnvOffline / UserOffline 两层各自的原始值。
	EnvOffline  bool `json:"envOffline"`
	UserOffline bool `json:"userOffline"`

	// WindowStartMs / WindowEndMs 统计窗口（默认最近 24h，与保留策略一致）。
	WindowStartMs int64 `json:"windowStartMs"`
	WindowEndMs   int64 `json:"windowEndMs"`
	// RecentCount 窗口内出站记录总条数。
	RecentCount int64 `json:"recentCount"`
	// BlockedCount 被离线闸门拦下的条数（**没有真的出站**）。
	BlockedCount int64 `json:"blockedCount"`
	// RealCount 真实出站条数。
	RealCount int64 `json:"realCount"`
	// PendingCount 结果未知（崩溃遗留）的条数。
	PendingCount int64 `json:"pendingCount"`
	// LastOutboundAt 最近一条记录时刻（ms，0 表示无记录）。
	LastOutboundAt int64 `json:"lastOutboundAt"`
	// RetentionHours 保留时长（小时）。
	RetentionHours int `json:"retentionHours"`
}

// OutboundRecord 是出站记录条目（API-9.8 的加强形态）。
//
// 铁律：Host 只含「域名 + 前两段路径」，**不含** query、userinfo、疑似密钥。
type OutboundRecord struct {
	// At 毫秒时间戳。
	At int64 `json:"at"`
	// Host 目标（域名 + 路径前缀，已脱敏）。
	Host string `json:"host"`
	// Kind 业务类型（model-download / online-asr）。
	Kind string `json:"kind"`
	// Audio 是否包含音频。
	Audio bool `json:"audio"`
	// Bytes 往返字节数合计。
	Bytes int64 `json:"bytes"`
	// Result ok / pending / blocked / ERR_*。
	Result string `json:"result"`
	// Blocked 该条是否只是「被拦下的尝试」（没有真的出站）。
	Blocked bool `json:"blocked"`
	// Pending 该条结果未知（进程崩溃遗留）。
	Pending bool `json:"pending"`
}

// OutboundPage 是分页结果。
type OutboundPage struct {
	Records []OutboundRecord `json:"records"`
	// Total 全库条数（分页器用）。
	Total int64 `json:"total"`
	// Offset / Limit 回显本次分页参数。
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// ClearResult 是清空类操作的统一回执。
type ClearResult struct {
	// Deleted 实际删除条数。
	Deleted int64 `json:"deleted"`
}

// AuditFilter 是零出站自检的窗口与过滤器；零值表示「最近 24h、全部类型」。
type AuditFilter struct {
	// FromMs / ToMs 时间窗（ms）；0 = 用默认值。
	FromMs int64 `json:"fromMs"`
	ToMs   int64 `json:"toMs"`
	// Kinds 只审这些业务类型；空 = 全部。
	Kinds []string `json:"kinds"`
	// Limit 最多扫描多少行；0/负数 = 不限。
	Limit int `json:"limit"`
}

// AuditFinding 是一条可疑 / 违例明细（不含正文与凭据）。
type AuditFinding struct {
	AtMs int64 `json:"atMs"`
	// Kind / Host / Result 来自 outbound_log 的安全列。
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	Result string `json:"result"`
	// Reason 机器可读原因键：real-outbound-while-offline / unfinished-attempt …
	Reason string `json:"reason"`
}

// AuditReport 是自检结论。ZeroOutbound 是可判定的布尔结论，
// 明细见 Allowed / Violations / Notes。
type AuditReport struct {
	// GeneratedAtMs 自检执行时刻。
	GeneratedAtMs int64 `json:"generatedAtMs"`
	// FromMs / ToMs 实际使用的窗口。
	FromMs int64 `json:"fromMs"`
	ToMs   int64 `json:"toMs"`
	// Offline 自检时离线是否生效。
	Offline bool `json:"offline"`
	// OfflineSource 离线生效来源。
	OfflineSource string `json:"offlineSource"`
	// ZeroOutbound 结论：窗口内**没有任何真实出站**。
	ZeroOutbound bool `json:"zeroOutbound"`
	// Scanned 扫描的记录条数。
	Scanned int `json:"scanned"`
	// AllowedCount 被闸门拦下的尝试条数（不算出站）。
	AllowedCount int `json:"allowedCount"`
	// Violations 违例明细：离线生效期间出现真实出站 / 结果未知的尝试。
	Violations []AuditFinding `json:"violations"`
	// Notes 人类可读的补充说明（不含正文与凭据）。
	Notes []string `json:"notes"`
}

// DiagnosticsArtifact 是脱敏诊断包产物。
//
// Bytes 由前端经 NativeDialogs 另存为文件；后端不落音频、不做导出 UI。
type DiagnosticsArtifact struct {
	// Filename 建议文件名（含时间戳）。
	Filename string `json:"filename"`
	// MIME 固定 application/json。
	MIME string `json:"mime"`
	// Bytes 已脱敏的诊断包字节（Wails 侧序列化为 base64 传给前端）。
	Bytes []byte `json:"bytes"`
	// Redactions 脱敏命中次数（UI 可提示「已脱敏 N 处」）。
	Redactions int `json:"redactions"`
	// Dropped 因无法证明安全而被丢弃的条目数（日志行等）。
	Dropped int `json:"dropped"`
}
