package contract

// 边录边转（FR-03-10）的对外契约。
//
// 只有可用性一个类型：partial / 定稿分别复用 EV-04 / EV-05 的 `Segment`
// 载荷（`finalized` 字段区分），**不新增事件、不新增状态值**。

// LiveAvailability 是「边录边转现在能不能用」的答复（API-4.8）。
//
// 三件事要分开表达，前端才能给出可操作的提示：
//   - `Available`：能力在不在（不是开关值，开关读 appsettings 的 liveTranscribe）；
//   - `EngineID`：可用时实际会用哪个流式引擎；
//   - `ErrorCode` + `Reason`：不可用时**为什么**（缺流式模型包 / 本构建不含 / 加载失败）。
type LiveAvailability struct {
	// Available 能力当前可用。
	Available bool `json:"available"`
	// EngineID 流式引擎标识（bundleId@version）；未加载时为空。
	EngineID string `json:"engineId"`
	// ErrorCode 不可用原因的错误码（ERR_LOCAL_UNAVAILABLE / ERR_MODEL_NOT_FOUND / ...）。
	ErrorCode string `json:"errorCode"`
	// Reason 面向用户的中文短说明（已脱敏，不含路径与音频内容）。
	Reason string `json:"reason"`
}
