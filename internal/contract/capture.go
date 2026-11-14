// Package contract 是前后端共享的 DTO 契约（DOC-03 §1.5 / §2.x）。
//
// 为什么单独成包：
//   - L2 领域层（capture / asr / recording / …）要返回这些结构，而 **L2 不许 import Wails**；
//   - `internal/events` 注册强类型事件时也要引用同一批类型；
//   - 若把 DTO 放进 events 包，L2 就会被迫依赖 Wails（因为 events 包 import 了 application）。
//
// 因此：本包**只放类型**，不 import Wails、不 import 业务包，任何层都可以安全依赖。
package contract

// Source 一个可捕获目标（应用或设备），DOC-03 §1.5。
type Source struct {
	// ID 稳定标识：app:<exe>|<pid> 或 dev:<deviceId>。
	ID string `json:"id"`
	// Kind application | device。
	Kind string `json:"kind"`
	// DisplayName KOOK / 全部应用声音。
	DisplayName string `json:"displayName"`
	// ExeName 进程可执行名（应用源）。
	ExeName string `json:"exeName"`
	// PIDs 进程树根（应用源）。
	PIDs []int `json:"pids"`
	// DeviceID 设备源。
	DeviceID string `json:"deviceId"`
	// Present 当前是否可用（进程存活 / 设备在线）。
	Present bool `json:"present"`
	// Producing 最近是否检测到音频。
	Producing bool `json:"producing"`
	// ChildCount 进程级捕获覆盖的子进程数（ARCH §4.1.3）。
	ChildCount int `json:"childCount"`
	// IsDefault 是否为当前系统默认输出设备（仅设备源有意义）。
	// 用字段而不是在 DisplayName 里拼「（默认）」：人类可见文案必须由前端 i18n 渲染。
	IsDefault bool `json:"isDefault"`
	// Warn 非阻塞警告键，如 "exclusive-mode"。
	Warn string `json:"warn"`
}

// SourceKind 取值。用常量避免前后端字符串漂移。
const (
	SourceKindApplication = "application"
	SourceKindDevice      = "device"
)

// CaptureCaps 平台能力矩阵（API-1.6）。
type CaptureCaps struct {
	// Platform windows | darwin | linux。
	Platform string `json:"platform"`
	// ProcessLoopback 是否支持进程级（按应用分路）捕获。
	ProcessLoopback bool `json:"processLoopback"`
	// DeviceLoopback 是否支持设备回环（整机混音）。
	DeviceLoopback bool `json:"deviceLoopback"`
	// Reason 不支持进程级时的人类可读原因（含系统版本要求）。
	Reason string `json:"reason"`
	// RequiresPermission macOS 需授予系统音频权限。
	RequiresPermission bool `json:"requiresPermission"`
}
