package contract

// Role 用户可见角色标签（API-7.1）。
//
// 角色是**全局**标签：改名只 UPDATE 一行，历史会话立即生效（FR-06-3）。
type Role struct {
	ID string `json:"id"`
	// Name 默认「说话人 N」。
	Name string `json:"name"`
	// ColorToken 主题令牌（如 role-3），**不存十六进制**。
	ColorToken string `json:"colorToken"`
	// SpeakerIDs 该角色覆盖的声纹身份。
	SpeakerIDs []string `json:"speakerIds"`
	// SessionCount 命中会话数。
	SessionCount int `json:"sessionCount"`
	// IsSystem 内置「未识别」角色不可删。
	IsSystem bool `json:"isSystem"`
}

// RoleUnknownID 内置「未识别」角色 ID（DOC-04 TBL-07）。
const RoleUnknownID = "rl_unknown"

// SessionSpeaker 会话内说话人 → 角色映射（API-7.3）。
type SessionSpeaker struct {
	SpeakerID string `json:"speakerId"`
	RoleID string `json:"roleId"`
	RoleName string `json:"roleName"`
	ColorToken string `json:"colorToken"`
	// UtteranceCount 该会话内句数。
	UtteranceCount int `json:"utteranceCount"`
	// SpeechMs 该会话内语音总时长。
	SpeechMs int64 `json:"speechMs"`
}

// RoleChanged 角色变更事件载荷（EV-07）。
type RoleChanged struct {
	RoleID string `json:"roleId"`
	Name string `json:"name"`
	ColorToken string `json:"colorToken"`
}

// SpeakerAssigned 声纹归并完成事件载荷（EV-06）。
type SpeakerAssigned struct {
	SessionID string `json:"sessionId"`
	SegmentID int64 `json:"segmentId"`
	SpeakerID string `json:"speakerId"`
	RoleID string `json:"roleId"`
	RoleName string `json:"roleName"`
}
