package contract

// Binding 快捷键绑定（API-8.1）。
type Binding struct {
	// Action 动作键，如 record.toggle / playback.play。
	Action string `json:"action"`
	// Accelerator 规范化组合，如 "Ctrl+Alt+R" / "⌘⌥R"。
	Accelerator string `json:"accelerator"`
	// Scope global | app。
	Scope string `json:"scope"`
	// Default 默认组合。
	Default string `json:"default"`
	// Enabled 是否启用（false 等价存 null）。
	Enabled bool `json:"enabled"`
	// RegisterErr 注册失败原因（被系统 / 他程序占用），空表示正常。
	RegisterErr string `json:"registerErr"`
}

// 快捷键作用域（UX-SPEC §2.1）。
const (
	HotkeyScopeGlobal = "global"
	HotkeyScopeApp    = "app"
)

// HotkeyConflict 单条冲突。
type HotkeyConflict struct {
	Action string `json:"action"`
	Accelerator string `json:"accelerator"`
	// Reason 冲突原因键（system | duplicate）。
	Reason string `json:"reason"`
}

// ConflictReport 冲突检测结果（API-8.2 / API-8.5）。
type ConflictReport struct {
	Conflicts []HotkeyConflict `json:"conflicts"`
	// Preview 规范化后的显示形式。
	Preview string `json:"preview"`
	OK bool `json:"ok"`
}

// HotkeyTrigger 热键触发事件载荷（EV-17 `hotkey:trigger`）。
//
// 有些动作的实现在前端（切页、播放器跳转、聚焦搜索框）：后端起不到作用，
// 若在后端硬造一份「导航实现」就会产生两套真相。这里只把动作 ID 广播出去，
// 由前端的路由/播放器各接自己那部分。
type HotkeyTrigger struct {
	// Action 动作键，如 playback.nextSegment / nav.library。
	Action string `json:"action"`
}
