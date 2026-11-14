// Package app 的装配细节：把 Wails 的快捷键能力适配成 hotkeys.Registrar。
package app

import (
	"fmt"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/hotkeys"
	"vip.oberon.mandaryn/internal/utils/logx"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// wailsRegistrar 把 `hotkeys.Registrar` 落到 Wails v3 的两个管理器上：
//
//   - `global` → `app.GlobalShortcut`（系统级，应用不在前台也生效）
//   - `app`    → `app.KeyBinding`（应用内，仅窗口聚焦时生效）
//
// 为什么不把两个管理器直接塞给 L2：L2 不许 import Wails；而且两边的回调签名
// 不同（KeyBinding 带 window 参数），差异必须在这里抹平。
type wailsRegistrar struct {
	app *application.App
}

// newWailsRegistrar 构造注册器；app 为 nil 时返回 nil（调用方应改用 NopRegistrar）。
func newWailsRegistrar(app *application.App) *wailsRegistrar {
	if app == nil {
		return nil
	}
	return &wailsRegistrar{app: app}
}

// Register 注册一个加速键。accelerator 已是平台解析形（`Ctrl+Alt+r` / `Cmd+Option+r`）。
func (r *wailsRegistrar) Register(scope, accelerator string, fn func()) error {
	if fn == nil {
		return fmt.Errorf("hotkeys: 回调为空")
	}
	switch scope {
	case contract.HotkeyScopeGlobal:
		if err := r.app.GlobalShortcut.Register(accelerator, fn); err != nil {
			// 常见原因：该组合已被系统或别的程序占用。上层会把它显示在绑定行上。
			return fmt.Errorf("全局快捷键注册失败：%w", err)
		}
		return nil
	case contract.HotkeyScopeApp:
		// 应用内绑定不返回错误（Wails 的 Add 无返回值）；重复添加时后者覆盖前者，
		// 因此上层的「先 Unregister 再 Register」顺序不能反。
		r.app.KeyBinding.Add(accelerator, func(_ application.Window) { fn() })
		return nil
	default:
		return fmt.Errorf("hotkeys: 未知作用域 %q", scope)
	}
}

// Unregister 注销一个加速键（未注册不报错：注销必须幂等）。
func (r *wailsRegistrar) Unregister(scope, accelerator string) error {
	switch scope {
	case contract.HotkeyScopeGlobal:
		if err := r.app.GlobalShortcut.Unregister(accelerator); err != nil {
			logx.For("hotkeys").Debug("注销全局热键失败", "accel", accelerator, "err", err.Error())
		}
		return nil
	case contract.HotkeyScopeApp:
		r.app.KeyBinding.Remove(accelerator)
		return nil
	default:
		return fmt.Errorf("hotkeys: 未知作用域 %q", scope)
	}
}

// UnregisterAll 注销全部。
//
// 注意：`GlobalShortcut.UnregisterAll` 只清全局；应用内绑定需要按记录逐个移除，
// 由 hotkeys.Service 自己遍历（它知道当前绑定集合）。
func (r *wailsRegistrar) UnregisterAll() error {
	if err := r.app.GlobalShortcut.UnregisterAll(); err != nil {
		return fmt.Errorf("注销全局快捷键失败：%w", err)
	}
	return nil
}

// 编译期断言：接口实现漂移会在这里暴露。
var _ hotkeys.Registrar = (*wailsRegistrar)(nil)
