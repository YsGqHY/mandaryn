package contract

// 路径键（ADR-02 / DOC-03 §2.9.3）。
const (
	PathKeyDataRoot  = "dataRoot"
	PathKeyModelRoot = "modelRoot"
	// PathKeyLogs 只用于 RevealPath：日志目录 <dataRoot>/logs（app.log 与调试日志所在处），
	// 不是独立可配置的根目录，跟随 dataRoot。
	PathKeyLogs = "logs"
)

// 路径来源层。前端**只按 source 判定 UI 状态**，不得自行推断优先级。
const (
	PathSourceDefault = "default"
	PathSourceUser    = "user"
	PathSourceEnv     = "env"
	PathSourceCLI     = "cli"
	// PathSourcePortable 便携模式：exe 同级存在 data\ 目录时自动采用（见 docs/DEPLOYMENT.md §3）。
	// 优先级低于 user、高于 default —— 用户显式设置不该被"把 exe 放进便携包"这一动作推翻。
	PathSourcePortable = "portable"
)

// ResolvedPath 路径解析结果（API-9.11）。
type ResolvedPath struct {
	// Key dataRoot | modelRoot。
	Key string `json:"key"`
	// Effective 当前生效绝对路径。
	Effective string `json:"effective"`
	// Source default | user | env | cli。
	Source string `json:"source"`
	// UserValue 用户设置里保存的值（可能因被接管而未生效，空表示未设）。
	UserValue string `json:"userValue"`
	// PendingValue 受管期间暂存的值，解除接管并重启后自动应用。
	PendingValue string `json:"pendingValue"`
	// EnvName 接管它的变量名（source 为 env/cli 时非空）。
	EnvName string `json:"envName"`
	// OverrideHint 可复制的解除提示，由后端按平台生成。
	OverrideHint string `json:"overrideHint"`
	// Writable 生效路径当前是否可写。
	Writable bool `json:"writable"`
	// FallbackFrom 非空表示该层因不可用而降级，值为被跳过的原路径。
	FallbackFrom string `json:"fallbackFrom"`
}
