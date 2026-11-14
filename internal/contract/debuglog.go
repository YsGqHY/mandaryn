package contract

// 调试模式与调试日志（设置 → 隐私边界 → 调试模式；API-9.16、API-10.8 ~ 10.11）。

// 调试模式开关的来源：环境变量接管 / 设置页（用户层）。
const (
	DebugModeSourceEnv  = "env"
	DebugModeSourceUser = "user"
)

// DebugModeState 是调试模式的生效状态。
type DebugModeState struct {
	// Enabled 调试日志当前是否在写（生效值）。
	Enabled bool `json:"enabled"`
	// UserEnabled 设置页保存的值（app_settings.log_level == "debug"）；被环境变量接管时可能与 Enabled 不同。
	UserEnabled bool `json:"userEnabled"`
	// Source env | user。
	Source string `json:"source"`
	// EnvVar 接管时为环境变量名（MANDARYN_LOG_LEVEL），否则为空。
	EnvVar string `json:"envVar"`
}

// DebugLogFile 是一个调试日志文件的元数据。
type DebugLogFile struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"sizeBytes"`
	ModifiedAt int64  `json:"modifiedAt"` // 毫秒时间戳
}

// DebugLogStatus 是设置页「调试模式」卡片要展示的全部信息。
type DebugLogStatus struct {
	Mode DebugModeState `json:"mode"`
	// Dir 调试日志所在目录（<dataRoot>/logs）。
	Dir string `json:"dir"`
	// Format 文件格式，固定 "jsonl"（JSON Lines）。
	Format string `json:"format"`
	// Files 现存的调试日志文件（当前文件在前）。
	Files      []DebugLogFile `json:"files"`
	TotalBytes int64          `json:"totalBytes"`
	// 保留策略：单文件上限、最多文件数、按修改时间的保留天数。
	MaxFileBytes  int64 `json:"maxFileBytes"`
	MaxFiles      int   `json:"maxFiles"`
	RetentionDays int   `json:"retentionDays"`
}

// DebugLogQuery 是查看器的查询条件。空字段表示不过滤。
type DebugLogQuery struct {
	// Limit 最多返回条数；0 用默认 200，上限 500。
	Limit int `json:"limit"`
	// MinLevel DEBUG | INFO | WARN | ERROR。
	MinLevel string `json:"minLevel"`
	// Origin backend | frontend。
	Origin string `json:"origin"`
	// Component 来源模块（精确匹配）。
	Component string `json:"component"`
}

// DebugLogEntry 是一条调试日志。
type DebugLogEntry struct {
	Time      string `json:"time"`
	Level     string `json:"level"`
	Origin    string `json:"origin"`
	Component string `json:"component"`
	Source    string `json:"source"`
	Msg       string `json:"msg"`
	// Fields 其余上下文字段的 JSON 文本（"{}" 表示没有）。
	Fields string `json:"fields"`
}

// DebugLogPage 是一次查询的结果（新的在前）。
type DebugLogPage struct {
	Entries []DebugLogEntry `json:"entries"`
	// Components 读到的记录里出现过的模块（筛选下拉框用）。
	Components []string `json:"components"`
	// Scanned 本次解析的行数；Malformed 无法解析而跳过的行数。
	Scanned   int `json:"scanned"`
	Malformed int `json:"malformed"`
	// Truncated 还有更早的记录没读（超出读取预算或条数上限）。
	Truncated bool `json:"truncated"`
}

// FrontendLogEntry 是前端上报的一条错误 / 警告（只在调试模式开启时上报）。
type FrontendLogEntry struct {
	// Level error | warn。
	Level   string `json:"level"`
	Message string `json:"message"`
	Stack   string `json:"stack"`
	// Route 当前页面（record / library / models / settings …）。
	Route string `json:"route"`
	// Window main | child | systray。
	Window string `json:"window"`
	// Kind error（未捕获异常）| rejection（未处理的 Promise 拒绝）| console。
	Kind string `json:"kind"`
}
