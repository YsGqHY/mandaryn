package contract

// 本文件是对 docs/API.md §2.3 的**向后兼容扩展**，只追加、不修改既有类型
// （internal/contract/session.go 属于已评审契约，字段改动必须走文档 + 重新生成 bindings）。
//
// 为什么需要这三个类型：API-3.2 / API-3.4 的契约形态无法表达 UI 必须知道的两条信息 ——
//   - 详情页要判断「回放按钮是否可用」，即音频文件是否仍在磁盘上；
//   - 批量删除要如实告知「记录已删、但 N 个音频文件没删掉」。
//
// 落文档时同步 DOC-03 §2.3（新增 API-3.6 GetDetail / API-3.7 DeleteDetailed）。

// SessionDetail 是会话详情（API-3.2 的扩展返回）。
//
// 不用结构体嵌入 Session：Wails 的 bindings 生成器对嵌入字段展开不可靠，
// 显式字段能让前端拿到确定的 interface 形状。
type SessionDetail struct {
	Session Session `json:"session"`
	// AudioExists 音频文件当前是否可在 dataRoot 下找到。
	AudioExists bool `json:"audioExists"`
}

// DeleteResult 是批量删除的逐项结果（API-3.4 的扩展返回）。
type DeleteResult struct {
	// Deleted 实际删除的会话数。
	Deleted int `json:"deleted"`
	// AudioDeleted 成功删除音频的会话数。
	AudioDeleted int `json:"audioDeleted"`
	// Missing 请求里不存在的会话 ID（已被并发删除时不当作失败）。
	Missing []string `json:"missing"`
	// AudioFailed 记录已删、音频未能删除的会话 ID（孤儿文件，需清理提示）。
	AudioFailed []string `json:"audioFailed"`
}
