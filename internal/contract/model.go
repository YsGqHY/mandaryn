package contract

// BundleInfo 可下载模型包（API-6.1）。
type BundleInfo struct {
	BundleID  string   `json:"bundleId"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	SizeBytes int64    `json:"sizeBytes"`
	RAMHintMB int      `json:"ramHintMb"`
	Languages []string `json:"languages"`
	License   string   `json:"license"`
	// Roles streaming | offline | vad | speaker。
	Roles  []string `json:"roles"`
	SHA256 string   `json:"sha256"`
	// Installed 是否已安装（列表展示用）。
	Installed bool `json:"installed"`
	// UpdateAvailable 已装版本可更新。
	UpdateAvailable bool `json:"updateAvailable"`
	// Downloadable 清单中的每个文件均具有可用的 URL、SHA-256 与安全路径。
	// 只说明清单可用于发起下载，不代表服务器一定可达。
	Downloadable bool `json:"downloadable"`
}

// BundleCatalog 同时返回条目与回退告警。Wails 在调用失败时不会传递其它返回值，
// 因此不能用 error 返回仍可展示的缓存或内置条目。
type BundleCatalog struct {
	Bundles []BundleInfo `json:"bundles"`
	Warning *ErrorDTO    `json:"warning"`
}

// ModelMirror exposes the saved user value and the deployment override, if any.
type ModelMirror struct {
	SavedURL  string `json:"savedUrl"`
	ManagedBy string `json:"managedBy"`
}

// InstalledBundle 已装模型包（API-6.2）。
type InstalledBundle struct {
	BundleInfo
	// Path 相对 modelRoot。
	Path string `json:"path"`
	// State installed | corrupt | missing。
	State string `json:"state"`
	// VerifiedAt 最近一次校验通过时间（ms）。
	VerifiedAt int64 `json:"verifiedAt"`
	// InstalledAt 安装时间（ms）。
	InstalledAt int64 `json:"installedAt"`
}

// 已装包状态。
const (
	BundleInstalled = "installed"
	BundleCorrupt   = "corrupt"
	BundleMissing   = "missing"
)

// DownloadState 下载进度（EV-09 / API-6.3）。
type DownloadState struct {
	BundleID string `json:"bundleId"`
	// State queued|downloading|verifying|paused|done|failed。
	State      string    `json:"state"`
	BytesDone  int64     `json:"bytesDone"`
	BytesTotal int64     `json:"bytesTotal"`
	SpeedBps   int64     `json:"speedBps"`
	EtaMs      int64     `json:"etaMs"`
	Retries    int       `json:"retries"`
	Mirror     string    `json:"mirror"`
	LastError  *ErrorDTO `json:"lastError"`
}

// 下载状态取值。
const (
	DownloadQueued      = "queued"
	DownloadDownloading = "downloading"
	DownloadVerifying   = "verifying"
	DownloadPaused      = "paused"
	DownloadDone        = "done"
	DownloadFailed      = "failed"
)

// MigratePlan 迁移预检结果（API-6.9）。
type MigratePlan struct {
	From        string `json:"from"`
	To          string `json:"to"`
	TotalBytes  int64  `json:"totalBytes"`
	FreeBytes   int64  `json:"freeBytes"`
	BundleCount int    `json:"bundleCount"`
	// Warnings 如 "network-path"（跨设备迁移不保证原子性，ARCH §4.6.3）。
	Warnings []string `json:"warnings"`
	Feasible bool     `json:"feasible"`
}

// MigrateRequest 执行迁移（API-6.10）。
type MigrateRequest struct {
	// Target 目标目录。
	Target string `json:"target"`
	// CopyOnly 目标为网络盘时的「复制模式」：不删除源（双份共存）。
	CopyOnly bool `json:"copyOnly"`
}

// MigrateState 迁移进度（EV-11 / API-6.12）。
type MigrateState struct {
	// Phase idle|preflight|copying|verifying|switching|cleaning|done|rollback|failed。
	Phase      string `json:"phase"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
	// Current 当前文件。
	Current   string    `json:"current"`
	LastError *ErrorDTO `json:"lastError"`
}

// 迁移阶段取值（DOC-02 §4.6.3）。
const (
	MigrateIdle      = "idle"
	MigratePreflight = "preflight"
	MigrateCopying   = "copying"
	MigrateVerifying = "verifying"
	MigrateSwitching = "switching"
	MigrateCleaning  = "cleaning"
	MigrateDone      = "done"
	MigrateRollback  = "rollback"
	MigrateFailed    = "failed"
)

// VerifyResult 重新校验结果（API-6.6）。
type VerifyResult struct {
	BundleID string `json:"bundleId"`
	OK       bool   `json:"ok"`
	// BadFiles 校验失败的文件（相对路径）。
	BadFiles []string `json:"badFiles"`
	// Note 结论说明。
	Note string `json:"note"`
}

// StoreStats 模型目录占用（API-6.8）。
type StoreStats struct {
	Root       string `json:"root"`
	TotalBytes int64  `json:"totalBytes"`
	// DownloadsBytes 未完成分片占用。
	DownloadsBytes int64             `json:"downloadsBytes"`
	Bundles        []InstalledBundle `json:"bundles"`
}

// ModelChanged 是 EV-10 models:changed 的载荷（安装 / 删除 / 校验完成）。
type ModelChanged struct {
	BundleID  string `json:"bundleId"`
	Installed bool   `json:"installed"`
	Verified  bool   `json:"verified"`
}
