package contract

// 检查更新（API-11.x）。设计前提：**不做静默自更新**（见 docs/DEPLOYMENT.md §6.5）——
// 只回答"有没有新版本"，下载与安装一律由用户手动完成。

// UpdateInfo 是一次「检查更新」的结果。
type UpdateInfo struct {
	// Current 当前运行版本（来自 build/config.yml 的 info.version，编译期注入）。
	Current string `json:"current"`
	// Latest 远端最新版本（去掉前缀 v 的语义化版本）。
	Latest string `json:"latest"`
	// HasUpdate Latest > Current 时为 true。
	HasUpdate bool `json:"hasUpdate"`
	// DownloadURL 发布页地址（由前端用系统浏览器打开，**不由后端下载**）。
	DownloadURL string `json:"downloadUrl"`
	// NotesURL 发行说明地址（通常与 DownloadURL 相同，留字段便于将来分离）。
	NotesURL string `json:"notesUrl"`
	// PublishedAt 该版本的发布时间（毫秒时间戳；0 表示未知）。
	PublishedAt int64 `json:"publishedAt"`
	// Source 清单来源描述（host 名，便于用户判断"这一路请求打到了哪里"）。
	Source string `json:"source"`
}
