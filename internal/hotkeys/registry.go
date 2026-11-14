package hotkeys

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"vip.oberon.mandaryn/internal/contract"
)

// Handler 是动作的处理函数。
//
// **只存在于 Go 侧**：服务对前端暴露的方法签名里不出现 func（Wails 绑定
// 生成器遇到函数类型会失败）。装配层用 Registry.SetHandler 注入真实实现。
type Handler func(ctx context.Context) error

// 内置动作 ID（稳定短标识，与 DOC-05 §2.2 的 `action` 列一一对应）。
//
// 铁律：ID 一旦发布就不可改（前端 i18n 键、日志、持久化 JSON 都以它为键）。
const (
	ActionRecordToggle     = "record.toggle"    // 开始 / 停止录制（global）
	ActionRecordPause      = "record.pause"     // 暂停 / 继续录制（global）
	ActionWindowShow       = "window.show"      // 显示 / 隐藏主窗口（global）
	ActionPlaybackPlay     = "playback.play"    // 播放 / 暂停（app）
	ActionPlaybackBack     = "playback.back"    // 后退 5 秒（app）
	ActionPlaybackForward  = "playback.forward" // 前进 5 秒（app）
	ActionPlaybackPrevSeg  = "playback.prevSegment"
	ActionPlaybackNextSeg  = "playback.nextSegment"
	ActionPlaybackRecenter = "playback.recenter" // 回到当前句
	ActionNavRecord        = "nav.record"
	ActionNavLibrary       = "nav.library"
	ActionNavModels        = "nav.models"
	ActionNavSettings      = "nav.settings"
	ActionSearchFocus      = "search.focus"
	ActionRoleRename       = "role.rename"
	ActionSessionDelete    = "session.delete"
	ActionModelSwitchSrc   = "model.switchSource" // 切换模型来源（录制中拒绝）
	ActionLeanMode         = "ux.leanMode"        // 精简模式开关（补充动作，默认不绑定）
)

// 动作分组（对应「允许的场景」，前端设置页按组分区、按场景决定「现在能不能用」）。
const (
	GroupRecording  = "recording"
	GroupPlayback   = "playback"
	GroupNavigation = "navigation"
	GroupSearch     = "search"
	GroupEditing    = "editing"
	GroupModel      = "model"
	GroupUX         = "ux"
)

// Action 是一个可绑定动作的元数据。
//
// 字段取舍：
//   - Scope 决定注册到系统级还是应用级（DOC-05 §2.1 两档作用域）；
//   - Group 是「场景」分类，前端据此分区并决定何时置灰；
//   - handler / guard 不导出，保证 Action 只会以「元数据」形态出现在任何 DTO 里。
type Action struct {
	// ID 稳定短标识（record.toggle）。
	ID string
	// LabelKey i18n 键（hotkeys.actions.<id>），人类可见文案一律走 i18n。
	LabelKey string
	// Group 场景分组。
	Group string
	// Default 默认加速键的规范形；空串表示「默认不绑定」（等于默认禁用）。
	Default string
	// Scope 声明作用域：global | app。
	Scope string

	handler Handler
	guard   func() error
}

// Handler 返回处理函数（可能为 nil，表示装配层尚未接线）。
func (a Action) Handler() Handler { return a.handler }

// Guard 返回前置守卫（可能为 nil）。
func (a Action) Guard() func() error { return a.guard }

// Registry 是动作注册表：内置动作 + 装配层注入的处理函数。
//
// 它**不是** Wails 服务（不会被注册进 app），因此可以放心持有 func 字段。
type Registry struct {
	mu      sync.RWMutex
	order   []string
	actions map[string]*Action
}

// NewRegistry 构造空注册表（测试用；生产用 DefaultRegistry）。
func NewRegistry() *Registry {
	return &Registry{actions: map[string]*Action{}}
}

// DefaultRegistry 返回装载了全部内置动作的注册表（处理函数待装配层注入）。
func DefaultRegistry() *Registry {
	r := NewRegistry()
	for _, a := range BuiltinActions() {
		// 内置动作 ID 由常量保证唯一，重复注册属于编程错误，
		// 这里显式忽略返回值而不是 panic（铁律：L2 不 panic）。
		_ = r.Register(a)
	}
	return r
}

// Register 注册一个动作。ID 为空、作用域非法、默认值不可解析、ID 重复都返回错误。
func (r *Registry) Register(a Action) error {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return fmt.Errorf("hotkeys: action id required")
	}
	if a.Scope != contract.HotkeyScopeGlobal && a.Scope != contract.HotkeyScopeApp {
		return fmt.Errorf("hotkeys: action %q has invalid scope %q", id, a.Scope)
	}
	if a.Default != "" {
		if _, err := Parse(a.Default); err != nil {
			return fmt.Errorf("hotkeys: action %q has invalid default: %w", id, err)
		}
	}
	label := a.LabelKey
	if label == "" {
		label = "hotkeys.actions." + id
	}
	a.ID = id
	a.LabelKey = label

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.actions[id]; exists {
		return fmt.Errorf("hotkeys: action %q already registered", id)
	}
	clone := a
	r.actions[id] = &clone
	r.order = append(r.order, id)
	return nil
}

// SetHandler 注入动作的处理函数（装配层专用）。
//
// 幂等：重复注入覆盖旧值，便于「先接线、后按真实服务替换」。
func (r *Registry) SetHandler(id string, fn Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.actions[id]
	if !ok {
		return fmt.Errorf("hotkeys: unknown action %q", id)
	}
	a.handler = fn
	return nil
}

// SetGuard 注入动作的前置守卫（例如「录制中拒绝切换模型源」）。
func (r *Registry) SetGuard(id string, guard func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.actions[id]
	if !ok {
		return fmt.Errorf("hotkeys: unknown action %q", id)
	}
	a.guard = guard
	return nil
}

// Get 取动作**副本**（调用方拿到的是快照，不能借它改注册表）。
func (r *Registry) Get(id string) (Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[id]
	if !ok {
		return Action{}, false
	}
	return *a, true
}

// All 按注册顺序返回全部动作副本。
func (r *Registry) All() []Action {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Action, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, *r.actions[id])
	}
	return out
}

// Len 动作总数。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// BuiltinActions 返回内置动作集（DOC-05 §2.2 的 17 项 + 精简模式开关 1 项）。
//
// 默认值一律写跨平台形（CmdOrCtrl 而非 Ctrl），这样 macOS 自动变成 ⌘，
// Windows / Linux 是 Ctrl —— 与 DOC-05 §2.2 两列完全一致。
func BuiltinActions() []Action {
	return []Action{
		// —— 全局（软件在后台也响应，只有「必须隔空操作」的 3 个）——
		{ID: ActionRecordToggle, Group: GroupRecording, Scope: contract.HotkeyScopeGlobal, Default: "CmdOrCtrl+Alt+R"},
		{ID: ActionRecordPause, Group: GroupRecording, Scope: contract.HotkeyScopeGlobal, Default: "CmdOrCtrl+Alt+P"},
		{ID: ActionWindowShow, Group: GroupNavigation, Scope: contract.HotkeyScopeGlobal, Default: "CmdOrCtrl+Alt+M"},

		// —— 回放（应用内，输入框聚焦时不拦截，由前端判定）——
		{ID: ActionPlaybackPlay, Group: GroupPlayback, Scope: contract.HotkeyScopeApp, Default: "Space"},
		{ID: ActionPlaybackBack, Group: GroupPlayback, Scope: contract.HotkeyScopeApp, Default: "Left"},
		{ID: ActionPlaybackForward, Group: GroupPlayback, Scope: contract.HotkeyScopeApp, Default: "Right"},
		{ID: ActionPlaybackPrevSeg, Group: GroupPlayback, Scope: contract.HotkeyScopeApp, Default: "Up"},
		{ID: ActionPlaybackNextSeg, Group: GroupPlayback, Scope: contract.HotkeyScopeApp, Default: "Down"},
		{ID: ActionPlaybackRecenter, Group: GroupPlayback, Scope: contract.HotkeyScopeApp, Default: "CmdOrCtrl+J"},

		// —— 导航 ——
		{ID: ActionNavRecord, Group: GroupNavigation, Scope: contract.HotkeyScopeApp, Default: "CmdOrCtrl+1"},
		{ID: ActionNavLibrary, Group: GroupNavigation, Scope: contract.HotkeyScopeApp, Default: "CmdOrCtrl+2"},
		{ID: ActionNavModels, Group: GroupNavigation, Scope: contract.HotkeyScopeApp, Default: "CmdOrCtrl+3"},
		{ID: ActionNavSettings, Group: GroupNavigation, Scope: contract.HotkeyScopeApp, Default: "CmdOrCtrl+,"},

		// —— 搜索与编辑 ——
		{ID: ActionSearchFocus, Group: GroupSearch, Scope: contract.HotkeyScopeApp, Default: "CmdOrCtrl+F"},
		{ID: ActionRoleRename, Group: GroupEditing, Scope: contract.HotkeyScopeApp, Default: "F2"},
		{ID: ActionSessionDelete, Group: GroupEditing, Scope: contract.HotkeyScopeApp, Default: "Delete"},

		// —— 模型 ——
		{ID: ActionModelSwitchSrc, Group: GroupModel, Scope: contract.HotkeyScopeApp, Default: "CmdOrCtrl+Shift+L"},

		// —— 精简 UX（补充动作：默认不绑定，用户想用才绑）——
		{ID: ActionLeanMode, Group: GroupUX, Scope: contract.HotkeyScopeApp, Default: ""},
	}
}
