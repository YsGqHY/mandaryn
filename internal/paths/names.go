package paths

// 应用标识常量（ARCH §10.1「应用标识收尾」的**唯一来源**）。
//
// 铁律：storage / cryptox / logx 三处此前各留一份 "Foundation" 常量，
// 不同步就会出现「数据库在新目录、密钥在旧目录」→ 表现为会话丢失 / 密钥失效。
// 现在统一从这里取，改一处即可全链路生效。
const (
	// AppDirName 是数据根目录在用户目录下的子文件夹名（dataRoot 末段）。
	AppDirName = "Mandaryn"
	// ModelDirName 是默认模型根目录名（`~/.mandaryn`，FR-07-1）。
	ModelDirName = "mandaryn"
)

// 文件名。
const (
	// DBFileName SQLite 主文件名（WAL/SHM 由 SQLite 自动管理为 -wal / -shm）。
	DBFileName = "mandaryn.db"
	// ConfigFileName 是 dataRoot 的用户层配置（storage.json）。
	// 它**不能**存进数据库本身（鸡生蛋：要先知道目录才能开库）。
	ConfigFileName = "storage.json"
	// MasterKeyFileName cryptox 主密钥文件名。
	MasterKeyFileName = ".master.key"
	// LogsDirName 日志子目录名（相对 dataRoot）。
	LogsDirName = "logs"
	// PortableDataDirName 便携包的标记目录名：exe 同级存在该目录即视为便携模式，
	// 并把它当作 dataRoot（来源标记 contract.PathSourcePortable）。
	PortableDataDirName = "data"
	// SessionsDirName 会话音频子目录名（相对 dataRoot）。
	SessionsDirName = "sessions"
)

// DefaultDataRoot 返回平台默认数据根（不含 cli/env/user 接管）。
func DefaultDataRoot() (string, error) { return defaultDataRoot() }

// DefaultModelRoot 返回默认模型根 `~/.mandaryn`。
func DefaultModelRoot() (string, error) { return defaultModelRoot() }
