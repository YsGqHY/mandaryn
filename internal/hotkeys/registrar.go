package hotkeys

import (
	"errors"

	"vip.oberon.mandaryn/internal/contract"
)

// 注册失败 / 冲突原因键（出现在 contract.Binding.RegisterErr 与
// contract.HotkeyConflict.Reason 里，前端按它映射 i18n 文案）。
const (
	// ReasonUnavailable 平台注册能力不可用（未注入 Registrar，例如 server 模式
	// 或尚未装配的启动早期）。此时热键域**不崩**，只是报告不可用。
	ReasonUnavailable = "unavailable"
	// ReasonSystemTaken 组合被系统 / 其它程序占用，注册失败。
	ReasonSystemTaken = "system"
	// ReasonDuplicate 软件内重复：同一组合被两个动作绑定。
	ReasonDuplicate = "duplicate"
	// ReasonInvalidStored 持久化里存了一个现在解析不出来的组合（旧版本 / 手改库）。
	ReasonInvalidStored = "invalid"
)

// ErrRegistrarUnavailable 是 NopRegistrar 的返回值。
var ErrRegistrarUnavailable = errors.New("hotkeys: platform registrar unavailable")

// Registrar 是平台注册能力（由装配层用 Wails 实现，L2 不 import Wails）。
//
// 为什么带 scope 参数：DOC-05 §2.1 有两档作用域，语义与失败行为都不同 ——
//   - scope=global → 装配层用 app.GlobalShortcut.Register/Unregister（系统级，
//     **会**因为被别的程序占用而失败，失败是正常业务分支）；
//   - scope=app    → 装配层用 app.KeyBinding.Add/Remove（应用聚焦时生效，
//     不劫持系统组合，基本不会失败）。
//
// accelerator 已经是**平台已解析**的串（见 Combo.PlatformString）：修饰键用
// Wails 认的 cmd/ctrl/option/alt/shift/super 词汇，主键小写。装配层原样转发即可，
// **不要**再自己做字符串处理。
//
// 实现约定：fn 只会在事件触发时被调用，**不得**在 Register 内部同步调用 fn
// （服务在持有锁的路径上调 Register）。
type Registrar interface {
	// Register 注册组合 → 回调。
	Register(scope, accelerator string, fn func()) error
	// Unregister 注销单个组合。
	Unregister(scope, accelerator string) error
	// UnregisterAll 注销本应用注册的全部热键（切换数据路径 / 退出时用）。
	UnregisterAll() error
}

// NopRegistrar 是「无平台能力」的降级实现：所有注册都失败，
// 但不会 panic，服务仍可读写绑定与手动 Trigger。
//
// 用在：server 模式构建、单元测试、以及装配层还没接好 Wails 的启动早期。
func NopRegistrar() Registrar { return nopRegistrar{} }

type nopRegistrar struct{}

func (nopRegistrar) Register(string, string, func()) error { return ErrRegistrarUnavailable }
func (nopRegistrar) Unregister(string, string) error       { return ErrRegistrarUnavailable }
func (nopRegistrar) UnregisterAll() error                  { return ErrRegistrarUnavailable }

// validScope 判断作用域是否合法（防止装配层传错字符串导致静默不注册）。
func validScope(scope string) bool {
	return scope == contract.HotkeyScopeGlobal || scope == contract.HotkeyScopeApp
}
