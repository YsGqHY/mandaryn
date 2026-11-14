package hotkeys

import (
	"context"
	"errors"
	"testing"

	"vip.oberon.mandaryn/internal/contract"
)

// TestRegistryRejectsInvalid 断言注册表对非法动作直接报错而不是 panic。
func TestRegistryRejectsInvalid(t *testing.T) {
	r := NewRegistry()

	if err := r.Register(Action{ID: "", Scope: contract.HotkeyScopeApp}); err == nil {
		t.Error("空 ID 应报错")
	}
	if err := r.Register(Action{ID: "x.y", Scope: "nope"}); err == nil {
		t.Error("非法作用域应报错")
	}
	if err := r.Register(Action{ID: "x.y", Scope: contract.HotkeyScopeApp, Default: "Ctrl"}); err == nil {
		t.Error("默认值不可解析应报错")
	}
	if err := r.Register(Action{ID: "x.y", Scope: contract.HotkeyScopeApp, Default: "Ctrl+K"}); err != nil {
		t.Fatalf("合法动作注册失败: %v", err)
	}
	if err := r.Register(Action{ID: "x.y", Scope: contract.HotkeyScopeApp}); err == nil {
		t.Error("ID 重复应报错")
	}
}

// TestRegistryLabelKeyDefault 断言未指定 i18n 键时按 hotkeys.actions.<id> 补齐。
func TestRegistryLabelKeyDefault(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Action{ID: "demo.act", Scope: contract.HotkeyScopeApp}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	a, ok := r.Get("demo.act")
	if !ok {
		t.Fatal("Get 未命中")
	}
	if a.LabelKey != "hotkeys.actions.demo.act" {
		t.Errorf("LabelKey = %q", a.LabelKey)
	}
}

// TestRegistryHandlerGuards 断言处理函数与守卫的注入 / 覆盖 / 未知 ID 行为。
func TestRegistryHandlerGuards(t *testing.T) {
	r := DefaultRegistry()

	if err := r.SetHandler("no.such", func(context.Context) error { return nil }); err == nil {
		t.Error("未知动作注入处理函数应报错")
	}
	if err := r.SetGuard("no.such", func() error { return nil }); err == nil {
		t.Error("未知动作注入守卫应报错")
	}

	calls := 0
	if err := r.SetHandler(ActionNavRecord, func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("SetHandler: %v", err)
	}
	// 幂等覆盖
	if err := r.SetHandler(ActionNavRecord, func(context.Context) error {
		calls += 10
		return nil
	}); err != nil {
		t.Fatalf("SetHandler 覆盖: %v", err)
	}

	a, _ := r.Get(ActionNavRecord)
	if a.Handler() == nil {
		t.Fatal("Handler 未注入")
	}
	if err := a.Handler()(context.Background()); err != nil {
		t.Fatalf("Handler 调用失败: %v", err)
	}
	if calls != 10 {
		t.Errorf("处理函数应被覆盖为后者，calls = %d", calls)
	}
	if a.Guard() != nil {
		t.Error("未注入守卫时应为 nil")
	}

	if err := r.SetGuard(ActionNavRecord, func() error { return errors.New("blocked") }); err != nil {
		t.Fatalf("SetGuard: %v", err)
	}
	a, _ = r.Get(ActionNavRecord)
	if a.Guard() == nil {
		t.Fatal("守卫未生效")
	}
	if err := a.Guard()(); err == nil || err.Error() != "blocked" {
		t.Error("守卫返回值不符")
	}
}

// TestRegistrySnapshotIsCopy 断言 Get/All 返回副本，调用方改不到注册表内部状态。
func TestRegistrySnapshotIsCopy(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(Action{ID: "demo.act", Scope: contract.HotkeyScopeApp, Default: "Ctrl+K"})

	a, _ := r.Get("demo.act")
	a.Default = "Ctrl+X" // 改副本
	if again, _ := r.Get("demo.act"); again.Default != "Ctrl+K" {
		t.Error("Get 返回的不是副本，内部状态被篡改")
	}

	all := r.All()
	all[0].Default = "Ctrl+Z"
	if again, _ := r.Get("demo.act"); again.Default != "Ctrl+K" {
		t.Error("All 返回的不是副本，内部状态被篡改")
	}
}

// TestRegistryOrder 断言 All 保持注册顺序（默认值按 DOC-05 表格顺序排列，便于前端核对）。
func TestRegistryOrder(t *testing.T) {
	r := DefaultRegistry()
	all := r.All()
	if len(all) != 18 {
		t.Fatalf("动作数 = %d", len(all))
	}
	if all[0].ID != ActionRecordToggle {
		t.Errorf("首个动作 = %s，期望 %s", all[0].ID, ActionRecordToggle)
	}
	if all[2].ID != ActionWindowShow {
		t.Errorf("第 3 个应是全局动作 window.show，实际 %s", all[2].ID)
	}
	if r.Len() != len(all) {
		t.Errorf("Len = %d，All = %d", r.Len(), len(all))
	}
}

// TestNewFallsBackToDefaultRegistry 断言 New 传 nil 注册表时使用内置动作集。
func TestNewFallsBackToDefaultRegistry(t *testing.T) {
	svc := New(nil, newFakeRegistrar(), NewMemoryStore(), Emit{})
	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(BuiltinActions()) {
		t.Errorf("动作数 = %d，期望 %d", len(list), len(BuiltinActions()))
	}
}

// TestPreferenceStoreWithoutHolder 断言 Holder 缺失时返回错误而不是 panic。
func TestPreferenceStoreWithoutHolder(t *testing.T) {
	s := NewPreferenceStore(nil)
	if _, err := s.Get(context.Background(), "k"); err == nil {
		t.Error("无 Holder 时 Get 应报错")
	}
	if err := s.Set(context.Background(), "k", "v"); err == nil {
		t.Error("无 Holder 时 Set 应报错")
	}
	if err := s.Delete(context.Background(), "k"); err == nil {
		t.Error("无 Holder 时 Delete 应报错")
	}
}
