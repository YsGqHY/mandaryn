// Package hotkeysvc 是快捷键与精简 UX 的 Wails 薄壳。
//
// 为什么只做委托：`internal/hotkeys` 已经把动作注册、冲突检测、持久化与平台
// 注册抽象做完了，且它不 import Wails。这里只补一个稳定的服务名、包边界与
// **延迟挂载**能力。
//
// 为什么需要挂载：真正的领域对象要拿到 `*application.App`（注册热键），而服务
// 列表必须在 `application.New` 之前给出 —— 与服务列表里的 subprocess 服务同一
// 制约。所以这里先占位、`application.New` 之后再 `Attach`。
package hotkeysvc

import (
	"vip.oberon.mandaryn/internal/hotkeys"
)

// Service 是 hotkeys 的薄壳（方法全部由嵌入的领域对象提升获得）。
//
// 未挂载时方法集为空指针，只会在「应用启动早期」被调用时表现为 panic；
// 前端只能在 Run 之后调用服务方法，因此这个窗口不存在。
type Service struct {
	*hotkeys.Service
}

// New 构造占位薄壳（挂载前调用）。
func New() *Service {
	return &Service{}
}

// Attach 挂载真正的领域对象（`application.New` 之后调用一次）。
func (s *Service) Attach(inner *hotkeys.Service) {
	s.Service = inner
}

// String 让 Wails 的服务名稳定（与其它 service 一致）。
func (s *Service) String() string { return "hotkeysvc" }
