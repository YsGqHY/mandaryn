package contract

// 本文件是 `internal/contract/speaker.go` 的**追加**扩展（原文件由契约基线维护，
// S7 不修改它）。只放 S7 新增的 DTO 与判定辅助，避免与既有定义重复。

// DiarizeResult 是一次「会话内说话人分离」的结果摘要（API-7.x 扩展）。
//
// 只含计数与模型标识：**不含**任何声纹向量、音频内容或正文（NFR-03-5）。
type DiarizeResult struct {
	SessionID string `json:"sessionId"`
	// SegmentCount 会话内参与分离的定稿片段总数。
	SegmentCount int `json:"segmentCount"`
	// SpeakerCount 本次分出的说话人数（受 maxSpeakers 上限约束）。
	SpeakerCount int `json:"speakerCount"`
	// AssignedSegments 成功归属到某个说话人的片段数。
	AssignedSegments int `json:"assignedSegments"`
	// UnidentifiedSegments 归入「未识别」的片段数（重叠语音 / 提取失败 / 过短）。
	UnidentifiedSegments int `json:"unidentifiedSegments"`
	// TooShortSegments 因过短被跳过声纹提取的片段数（ARCH §4.4：< 1.2s）。
	TooShortSegments int `json:"tooShortSegments"`
	// FailedSegments 音频不可读或提取失败的片段数。
	FailedSegments int `json:"failedSegments"`
	// ModelID 产出向量的声纹模型标识（`bundleId@version` 或 `speaker:<file>`）。
	ModelID string `json:"modelId"`
}

// RoleIsUnidentified 判断角色是否为内置「未识别」角色。
//
// 前端 / 上层用本函数而不是硬编码 `"rl_unknown"`：内置 ID 未来若调整，
// 只需要改这里一处。
func RoleIsUnidentified(roleID string) bool { return roleID == RoleUnknownID }
