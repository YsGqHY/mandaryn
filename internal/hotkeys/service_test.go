package hotkeys

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// —— 测试替身 ——

// fakeRegistrar 记录注册行为，并可对指定组合注入「被系统占用」的失败。
type fakeRegistrar struct {
	mu sync.Mutex

	active   map[string]func() // scope|accel -> 回调
	regCalls int
	unregAll int

	// failGlobal 里列出的平台加速键在 global 作用域注册时失败。
	failGlobal map[string]bool
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{active: map[string]func(){}, failGlobal: map[string]bool{}}
}

func regKey(scope, accel string) string { return scope + "|" + accel }

func (f *fakeRegistrar) Register(scope, accelerator string, fn func()) error {
	if !validScope(scope) {
		return errors.New("bad scope")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regCalls++
	if scope == contract.HotkeyScopeGlobal && f.failGlobal[accelerator] {
		return ErrRegistrarUnavailable
	}
	f.active[regKey(scope, accelerator)] = fn
	return nil
}

func (f *fakeRegistrar) Unregister(scope, accelerator string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, regKey(scope, accelerator))
	return nil
}

func (f *fakeRegistrar) UnregisterAll() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregAll++
	f.active = map[string]func(){}
	return nil
}

// has 判断某作用域下是否注册了该加速键。
func (f *fakeRegistrar) has(scope, accelerator string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.active[regKey(scope, accelerator)]
	return ok
}

// count 当前注册总数。
func (f *fakeRegistrar) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.active)
}

// fire 模拟系统触发热键（回调走服务统一入口 Trigger）。
func (f *fakeRegistrar) fire(t *testing.T, scope, accelerator string) {
	t.Helper()
	f.mu.Lock()
	fn, ok := f.active[regKey(scope, accelerator)]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("热键未注册: %s %s", scope, accelerator)
	}
	fn()
}

// platformAccel 把规范形转成假注册表里使用的平台加速键。
func platformAccel(t *testing.T, canonical, os string) string {
	t.Helper()
	c, err := Parse(canonical)
	if err != nil {
		t.Fatalf("Parse(%q): %v", canonical, err)
	}
	return c.PlatformString(os)
}

// display 把规范形按服务所在平台转成显示形（测试跨平台可运行）。
func display(t *testing.T, os, canonical string) string {
	t.Helper()
	c, err := Parse(canonical)
	if err != nil {
		t.Fatalf("Parse(%q): %v", canonical, err)
	}
	return c.Display(os)
}

// findBinding 在列表里找某动作。
func findBinding(t *testing.T, list []contract.Binding, action string) contract.Binding {
	t.Helper()
	for _, b := range list {
		if b.Action == action {
			return b
		}
	}
	t.Fatalf("列表里没有动作 %s", action)
	return contract.Binding{}
}

// —— 测试 ——

// TestDefaultsRegisteredOnStartup 断言启动即注册全部启用的默认绑定，且作用域正确。
func TestDefaultsRegisteredOnStartup(t *testing.T) {
	reg := newFakeRegistrar()
	svc := New(nil, reg, NewMemoryStore(), Emit{})

	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 18 {
		t.Fatalf("动作数 = %d，期望 18", len(list))
	}

	// 17 个默认启用（精简模式开关默认不绑定）。
	enabled := 0
	for _, b := range list {
		if b.Enabled {
			enabled++
		}
		if b.RegisterErr != "" {
			t.Errorf("动作 %s 启动即报注册失败: %s", b.Action, b.RegisterErr)
		}
	}
	if enabled != 17 {
		t.Errorf("默认启用数 = %d，期望 17", enabled)
	}
	if got := reg.count(); got != 17 {
		t.Errorf("注册总数 = %d，期望 17", got)
	}

	globals := []string{ActionRecordToggle, ActionRecordPause, ActionWindowShow}
	for _, id := range globals {
		b := findBinding(t, list, id)
		if b.Scope != contract.HotkeyScopeGlobal {
			t.Errorf("%s 作用域 = %s，期望 global", id, b.Scope)
		}
		if !reg.has(contract.HotkeyScopeGlobal, platformAccel(t, b.Accelerator, svc.platform)) {
			// Accelerator 是显示形，需要先还原成规范形再转平台形。
			c, err := Parse(b.Accelerator)
			if err != nil {
				t.Fatalf("Parse(%q): %v", b.Accelerator, err)
			}
			if !reg.has(contract.HotkeyScopeGlobal, c.PlatformString(svc.platform)) {
				t.Errorf("%s 未以 global 注册", id)
			}
		}
	}

	// 应用内动作必须注册为 app 作用域。
	b := findBinding(t, list, ActionPlaybackPlay)
	if b.Scope != contract.HotkeyScopeApp {
		t.Errorf("playback.play 作用域 = %s，期望 app", b.Scope)
	}
}

// TestSetPersistsAcrossRestart 断言绑定落 preferences JSON、重建服务后仍在。
func TestSetPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	reg := newFakeRegistrar()
	svc := New(nil, reg, store, Emit{})

	if _, err := svc.Set(ctx, ActionPlaybackPlay, "Ctrl+Shift+P"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Disable(ctx, ActionPlaybackBack); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	raw, err := store.Get(ctx, KeyBindingOverride)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	var disk map[string]*string
	if err := json.Unmarshal([]byte(raw), &disk); err != nil {
		t.Fatalf("持久化内容不是 JSON: %v (%s)", err, raw)
	}
	if v, ok := disk[ActionPlaybackPlay]; !ok || v == nil || *v != "Ctrl+Shift+P" {
		t.Errorf("持久化里 playback.play = %v，期望 Ctrl+Shift+P", disk[ActionPlaybackPlay])
	}
	if v, ok := disk[ActionPlaybackBack]; !ok || v != nil {
		t.Errorf("禁用的动作必须存 null，实际 %v", disk[ActionPlaybackBack])
	}
	// 没改过的动作不应出现在覆盖表里（缺键 = 用默认）。
	if _, ok := disk[ActionNavRecord]; ok {
		t.Errorf("未修改的动作不该写进覆盖表")
	}

	// 模拟重启：同一份存储 + 新服务实例。
	reg2 := newFakeRegistrar()
	svc2 := New(nil, reg2, store, Emit{})
	list, err := svc2.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	play := findBinding(t, list, ActionPlaybackPlay)
	if !play.Enabled || play.Accelerator != display(t, svc2.platform, "Ctrl+Shift+P") {
		t.Errorf("重启后 playback.play = %+v，期望 Ctrl+Shift+P 且启用", play)
	}
	back := findBinding(t, list, ActionPlaybackBack)
	if back.Enabled || back.Accelerator != "" {
		t.Errorf("重启后 playback.back 应保持禁用，实际 %+v", back)
	}
}

// TestResetAndResetAll 断言恢复默认语义。
func TestResetAndResetAll(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := New(nil, newFakeRegistrar(), store, Emit{})

	// Reset 单条
	if _, err := svc.Set(ctx, ActionSearchFocus, "Ctrl+Shift+F"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Reset(ctx, ActionSearchFocus); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	list, _ := svc.List(ctx)
	if b := findBinding(t, list, ActionSearchFocus); b.Accelerator != b.Default {
		t.Errorf("Reset 后 %s = %q，期望回到默认 %q", b.Action, b.Accelerator, b.Default)
	}

	// ResetAll：多条改动一律回到默认
	if _, err := svc.Set(ctx, ActionSearchFocus, "Ctrl+Shift+F"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Disable(ctx, ActionRoleRename); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := svc.ResetAll(ctx); err != nil {
		t.Fatalf("ResetAll: %v", err)
	}
	list, _ = svc.List(ctx)
	for _, id := range []string{ActionSearchFocus, ActionRoleRename} {
		b := findBinding(t, list, id)
		if b.Accelerator != b.Default || !b.Enabled {
			t.Errorf("%s ResetAll 后 = %+v，期望回到默认且启用", id, b)
		}
	}
	raw, _ := store.Get(ctx, KeyBindingOverride)
	if raw != "{}" {
		t.Errorf("ResetAll 后持久化应为空覆盖表 {}，实际 %s", raw)
	}

	// 未知动作
	if err := svc.Reset(ctx, "no.such"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Errorf("Reset 未知动作错误 = %v，期望 ERR_NOT_FOUND", err)
	}
}

// TestConflictDetection 断言软件内重复绑定被拒，且第二个动作保持原样。
func TestConflictDetection(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, newFakeRegistrar(), NewMemoryStore(), Emit{})

	// nav.record 默认 CmdOrCtrl+1，nav.library 抢同一组合必须失败。
	report, err := svc.Set(ctx, ActionNavLibrary, "Ctrl+1")
	if err == nil {
		t.Fatal("重复绑定应当失败")
	}
	if !apierr.HasCode(err, apierr.CodeHotkeyConflict) {
		t.Errorf("错误码 = %v，期望 ERR_HOTKEY_CONFLICT", err)
	}
	if report.OK {
		t.Error("冲突报告的 OK 应为 false")
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("冲突条数 = %d，期望 1", len(report.Conflicts))
	}
	if report.Conflicts[0].Action != ActionNavRecord {
		t.Errorf("冲突方 = %s，期望 %s", report.Conflicts[0].Action, ActionNavRecord)
	}
	if report.Conflicts[0].Reason != ReasonDuplicate {
		t.Errorf("冲突原因 = %s，期望 %s", report.Conflicts[0].Reason, ReasonDuplicate)
	}

	list, _ := svc.List(ctx)
	if b := findBinding(t, list, ActionNavLibrary); b.Accelerator != b.Default {
		t.Errorf("冲突失败后 nav.library 应保持默认，实际 %q", b.Accelerator)
	}

	// DetectConflicts 单独探测，不写库
	rep, err := svc.DetectConflicts(ctx, "ctrl+1")
	if err != nil {
		t.Fatalf("DetectConflicts: %v", err)
	}
	if rep.OK || len(rep.Conflicts) != 1 {
		t.Errorf("DetectConflicts 结果 = %+v，期望 1 条冲突", rep)
	}
	if rep.Preview == "" {
		t.Error("DetectConflicts 应返回规范化预览")
	}

	// 换成没人用的组合即成功
	report, err = svc.Set(ctx, ActionNavLibrary, "Ctrl+Shift+2")
	if err != nil {
		t.Fatalf("Set 空闲组合: %v", err)
	}
	if !report.OK || report.Preview != display(t, svc.platform, "Ctrl+Shift+2") {
		t.Errorf("报告 = %+v，期望 OK 且预览为规范形", report)
	}
}

// TestPlatformPhysicalConflict 断言 macOS 上 ⌘⌥R 与 CmdOrCtrl+Alt+R 判为同一物理键。
func TestPlatformPhysicalConflict(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, newFakeRegistrar(), NewMemoryStore(), Emit{})
	svc.platform = "darwin"
	svc.mu.Lock()
	svc.applyLocked()
	svc.mu.Unlock()

	// record.toggle 默认 CmdOrCtrl+Alt+R（显示为 ⌘⌥R），用 ⌘⌥P 抢 record.pause 应冲突。
	if _, err := svc.Set(ctx, ActionRecordPause, "⌘⌥R"); !apierr.HasCode(err, apierr.CodeHotkeyConflict) {
		t.Errorf("macOS 上 ⌘⌥R 与 record.toggle 的默认值冲突，实际错误 = %v", err)
	}
}

// TestSetInvalidAccelerator 断言非法组合按 ERR_INVALID_ARGUMENT + 原因键返回。
func TestSetInvalidAccelerator(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, newFakeRegistrar(), NewMemoryStore(), Emit{})

	cases := []struct {
		in   string
		want Reason
	}{
		{"", ReasonEmpty},
		{"   ", ReasonEmpty},
		{"Ctrl", ReasonNoKey},
		{"Shift+Alt", ReasonNoKey},
		{"Ctrl+Ctrl", ReasonDuplicateModifier},
		{"Ctrl++", ReasonEmptyKey},
		{"Ctrl+Foo", ReasonUnknownKey},
	}
	for _, c := range cases {
		_, err := svc.Set(ctx, ActionPlaybackPlay, c.in)
		if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Errorf("Set(%q) 错误 = %v，期望 ERR_INVALID_ARGUMENT", c.in, err)
			continue
		}
		var e *apierr.Err
		if !errors.As(err, &e) {
			t.Errorf("Set(%q) 错误类型异常", c.in)
			continue
		}
		if !containsReason(e.Detail, c.want) {
			t.Errorf("Set(%q) 的 Detail = %q，期望包含 reason=%s", c.in, e.Detail, c.want)
		}
	}

	if _, err := svc.Set(ctx, "no.such", "Ctrl+K"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Errorf("未知动作错误 = %v，期望 ERR_NOT_FOUND", err)
	}
}

// containsReason 判断 Detail 里是否带指定原因键。
func containsReason(detail string, want Reason) bool {
	return detail != "" && (len(string(want)) == 0 || contains(detail, "reason="+string(want)))
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestGlobalRegisterFailureRejected 断言用户主动保存时系统占用要驳回并保留原绑定。
func TestGlobalRegisterFailureRejected(t *testing.T) {
	ctx := context.Background()
	reg := newFakeRegistrar()
	// 让目标组合在 global 作用域注册失败，且应用内回退也失败（模拟被别家程序占住）。
	blocked := "Ctrl+Alt+k"
	reg.failGlobal[blocked] = true
	// 回退到 app 作用域也失败：把 app 注册同样视为失败。
	reg.failGlobal[blocked] = true
	regAppFail := &appFailRegistrar{inner: reg, failAccel: blocked}

	svc := New(nil, regAppFail, NewMemoryStore(), Emit{})
	if _, err := svc.Set(ctx, ActionRecordToggle, "Ctrl+Alt+K"); !apierr.HasCode(err, apierr.CodeHotkeyRegister) {
		t.Fatalf("系统占用应返回 ERR_HOTKEY_REGISTER，实际 %v", err)
	}
	list, _ := svc.List(ctx)
	b := findBinding(t, list, ActionRecordToggle)
	if !b.Enabled || b.Accelerator != b.Default {
		t.Errorf("驳回后应保留原绑定，实际 %+v", b)
	}
}

// appFailRegistrar 让 app 作用域对指定组合也失败（模拟极端占用场景）。
type appFailRegistrar struct {
	inner     *fakeRegistrar
	failAccel string
}

func (a *appFailRegistrar) Register(scope, accel string, fn func()) error {
	if accel == a.failAccel {
		return ErrRegistrarUnavailable
	}
	return a.inner.Register(scope, accel, fn)
}

func (a *appFailRegistrar) Unregister(scope, accel string) error {
	return a.inner.Unregister(scope, accel)
}

func (a *appFailRegistrar) UnregisterAll() error { return a.inner.UnregisterAll() }

// TestStartupGlobalFailureDegrades 断言启动期全局注册失败会降级为 app 作用域并标红。
func TestStartupGlobalFailureDegrades(t *testing.T) {
	ctx := context.Background()
	reg := newFakeRegistrar()
	blocked := "Ctrl+Alt+r" // record.toggle 的 Windows 平台形
	reg.failGlobal[blocked] = true

	svc := New(nil, reg, NewMemoryStore(), Emit{})
	svc.platform = "windows"
	svc.mu.Lock()
	svc.applyLocked()
	svc.mu.Unlock()

	list, _ := svc.List(ctx)
	b := findBinding(t, list, ActionRecordToggle)
	if b.Scope != contract.HotkeyScopeApp {
		t.Errorf("降级后作用域 = %s，期望 app", b.Scope)
	}
	if b.RegisterErr != ReasonSystemTaken {
		t.Errorf("降级后 RegisterErr = %q，期望 %q", b.RegisterErr, ReasonSystemTaken)
	}
	if b.Accelerator != b.Default {
		t.Errorf("降级不改绑定，实际 %q", b.Accelerator)
	}
	if !reg.has(contract.HotkeyScopeApp, blocked) {
		t.Error("降级后应以 app 作用域注册")
	}
}

// TestReservedComboRejectedForGlobal 断言 global 动作不能占用系统保留组合。
func TestReservedComboRejectedForGlobal(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, newFakeRegistrar(), NewMemoryStore(), Emit{})
	svc.platform = "windows"

	if _, err := svc.Set(ctx, ActionRecordToggle, "Win+L"); !apierr.HasCode(err, apierr.CodeHotkeyConflict) {
		t.Errorf("Win+L 是系统保留组合，错误 = %v，期望 ERR_HOTKEY_CONFLICT", err)
	}
	// 应用内动作不受保留组合限制（不劫持系统键，由前端作用域隔离）。
	if _, err := svc.Set(ctx, ActionPlaybackPlay, "Win+L"); err != nil {
		t.Errorf("应用内动作不该被系统保留组合拦住: %v", err)
	}
}

// TestTriggerDispatch 断言动作分派：注册的动作被调用且只被调用一次。
func TestTriggerDispatch(t *testing.T) {
	ctx := context.Background()
	reg := DefaultRegistry()
	var mu sync.Mutex
	calls := 0
	if err := reg.SetHandler(ActionPlaybackPlay, func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil
	}); err != nil {
		t.Fatalf("SetHandler: %v", err)
	}

	fr := newFakeRegistrar()
	svc := New(reg, fr, NewMemoryStore(), Emit{})

	if err := svc.Trigger(ctx, ActionPlaybackPlay); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	mu.Lock()
	if calls != 1 {
		t.Errorf("调用次数 = %d，期望 1", calls)
	}
	mu.Unlock()

	// 通过热键回调触发同样只调一次（统一入口，不重复执行动作逻辑）。
	accel := platformAccel(t, "Space", svc.platform)
	fr.fire(t, contract.HotkeyScopeApp, accel)
	mu.Lock()
	if calls != 2 {
		t.Errorf("热键回调后调用次数 = %d，期望 2", calls)
	}
	mu.Unlock()

	// 未知动作
	if err := svc.Trigger(ctx, "no.such"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Errorf("未知动作错误 = %v，期望 ERR_NOT_FOUND", err)
	}

	// 禁用后拒绝
	if err := svc.Disable(ctx, ActionPlaybackPlay); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := svc.Trigger(ctx, ActionPlaybackPlay); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Errorf("禁用动作触发错误 = %v，期望 ERR_STATE_FORBIDDEN", err)
	}
	mu.Lock()
	if calls != 2 {
		t.Errorf("禁用动作不该执行，调用次数 = %d", calls)
	}
	mu.Unlock()
}

// TestTriggerGuard 断言守卫拒绝时不执行处理函数（场景限制）。
func TestTriggerGuard(t *testing.T) {
	ctx := context.Background()
	reg := DefaultRegistry()
	calls := 0
	_ = reg.SetHandler(ActionModelSwitchSrc, func(context.Context) error {
		calls++
		return nil
	})
	_ = reg.SetGuard(ActionModelSwitchSrc, func() error {
		return apierr.New(apierr.CodeStateForbidden).WithDetail("recording in progress")
	})
	svc := New(reg, newFakeRegistrar(), NewMemoryStore(), Emit{})

	if err := svc.Trigger(ctx, ActionModelSwitchSrc); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Errorf("守卫拒绝错误 = %v，期望 ERR_STATE_FORBIDDEN", err)
	}
	if calls != 0 {
		t.Errorf("守卫拒绝后不该执行处理函数，调用次数 = %d", calls)
	}
}

// TestDegradedWithoutRegistrar 断言没有平台注册能力时服务不崩且如实报告不可用。
func TestDegradedWithoutRegistrar(t *testing.T) {
	ctx := context.Background()
	reg := DefaultRegistry()
	calls := 0
	_ = reg.SetHandler(ActionNavLibrary, func(context.Context) error {
		calls++
		return nil
	})

	svc := New(reg, nil, NewMemoryStore(), Emit{})

	available, err := svc.Available(ctx)
	if err != nil {
		t.Fatalf("Available: %v", err)
	}
	if available {
		t.Error("未注入 Registrar 时应报告不可用")
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List 不该失败: %v", err)
	}
	if len(list) != 18 {
		t.Fatalf("动作数 = %d", len(list))
	}
	for _, b := range list {
		if b.Enabled && b.RegisterErr != ReasonUnavailable {
			t.Errorf("动作 %s 的 RegisterErr = %q，期望 %q", b.Action, b.RegisterErr, ReasonUnavailable)
		}
	}

	// 降级下仍可改绑定、仍可手动触发
	if _, err := svc.Set(ctx, ActionNavLibrary, "Ctrl+Shift+9"); err != nil {
		t.Fatalf("降级下 Set 应可用: %v", err)
	}
	if err := svc.Trigger(ctx, ActionNavLibrary); err != nil {
		t.Fatalf("降级下 Trigger 应可用: %v", err)
	}
	if calls != 1 {
		t.Errorf("调用次数 = %d，期望 1", calls)
	}
}

// TestNopRegistrarNeverPanics 断言 NopRegistrar 的行为：全部返回不可用错误。
func TestNopRegistrarNeverPanics(t *testing.T) {
	r := NopRegistrar()
	if err := r.Register(contract.HotkeyScopeGlobal, "Ctrl+x", func() {}); !errors.Is(err, ErrRegistrarUnavailable) {
		t.Errorf("NopRegistrar.Register 错误 = %v", err)
	}
	if err := r.Unregister(contract.HotkeyScopeGlobal, "Ctrl+x"); !errors.Is(err, ErrRegistrarUnavailable) {
		t.Errorf("NopRegistrar.Unregister 错误 = %v", err)
	}
	if err := r.UnregisterAll(); !errors.Is(err, ErrRegistrarUnavailable) {
		t.Errorf("NopRegistrar.UnregisterAll 错误 = %v", err)
	}
}

// TestEmitChanged 断言绑定变更会通知装配层（用于发 Wails 事件）。
func TestEmitChanged(t *testing.T) {
	ctx := context.Background()
	var got []contract.Binding
	var leanCalls int
	svc := New(nil, newFakeRegistrar(), NewMemoryStore(), Emit{
		Changed: func(b contract.Binding) { got = append(got, b) },
		Lean:    func(LeanState) { leanCalls++ },
	})

	if _, err := svc.Set(ctx, ActionRoleRename, "Ctrl+Shift+F2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("变更通知次数 = %d，期望 1", len(got))
	}
	if got[0].Action != ActionRoleRename || got[0].Accelerator != display(t, svc.platform, "Ctrl+Shift+F2") {
		t.Errorf("通知内容 = %+v", got[0])
	}

	if err := svc.Disable(ctx, ActionSessionDelete); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if len(got) != 2 || got[1].Enabled {
		t.Errorf("禁用通知 = %+v，期望 Enabled=false", got)
	}

	// ResetAll 对每条动作各通知一次
	got = nil
	if err := svc.ResetAll(ctx); err != nil {
		t.Fatalf("ResetAll: %v", err)
	}
	if len(got) != 18 {
		t.Errorf("ResetAll 通知次数 = %d，期望 18", len(got))
	}

	if _, err := svc.SetLeanMode(ctx, true); err != nil {
		t.Fatalf("SetLeanMode: %v", err)
	}
	if leanCalls != 1 {
		t.Errorf("精简模式通知次数 = %d，期望 1", leanCalls)
	}
}

// TestLoadIgnoresBadData 断言存量脏数据不阻断启动。
func TestLoadIgnoresBadData(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	_ = store.Set(ctx, KeyBindingOverride, `{"playback.play":"Ctrl+P","no.such":"Ctrl+Q","record.pause":"!!!bad!!!"}`)
	_ = store.Set(ctx, KeyLeanMode, `not-json`)

	svc := New(nil, newFakeRegistrar(), store, Emit{})
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	play := findBinding(t, list, ActionPlaybackPlay)
	if play.Accelerator != display(t, svc.platform, "Ctrl+P") {
		t.Errorf("合法覆盖未生效: %+v", play)
	}
	pause := findBinding(t, list, ActionRecordPause)
	if pause.Accelerator != pause.Default {
		t.Errorf("非法覆盖应回落默认，实际 %q", pause.Accelerator)
	}

	st, _ := svc.GetLeanState(ctx)
	if st.Enabled {
		t.Error("精简模式脏数据应按默认关闭处理")
	}
}

// TestUnregisterAllOnShutdown 断言退出前注销全部热键。
func TestUnregisterAllOnShutdown(t *testing.T) {
	reg := newFakeRegistrar()
	svc := New(nil, reg, NewMemoryStore(), Emit{})
	if reg.count() == 0 {
		t.Fatal("前置断言：启动应已注册")
	}
	if err := svc.ServiceShutdown(); err != nil {
		t.Fatalf("ServiceShutdown: %v", err)
	}
	if reg.count() != 0 {
		t.Errorf("退出后仍有 %d 个热键未注销", reg.count())
	}
}

// TestListActionsMetadata 断言动作元数据（分组 / i18n 键 / 是否有处理函数）。
func TestListActionsMetadata(t *testing.T) {
	reg := DefaultRegistry()
	_ = reg.SetHandler(ActionNavSettings, func(context.Context) error { return nil })
	svc := New(reg, newFakeRegistrar(), NewMemoryStore(), Emit{})

	infos, err := svc.ListActions(context.Background())
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(infos) != 18 {
		t.Fatalf("元数据条数 = %d，期望 18", len(infos))
	}
	byID := map[string]ActionInfo{}
	for _, in := range infos {
		byID[in.ID] = in
		if in.LabelKey == "" {
			t.Errorf("动作 %s 缺少 i18n 键", in.ID)
		}
		if in.Group == "" {
			t.Errorf("动作 %s 缺少场景分组", in.ID)
		}
	}
	if !byID[ActionNavSettings].HasHandler {
		t.Error("已接线的动作 HasHandler 应为 true")
	}
	if byID[ActionPlaybackPlay].HasHandler {
		t.Error("未接线的动作 HasHandler 应为 false")
	}
	if byID[ActionRecordToggle].Scope != contract.HotkeyScopeGlobal {
		t.Errorf("record.toggle 作用域 = %s", byID[ActionRecordToggle].Scope)
	}
}

// TestSetPhysicallyEqualIsNoop 断言「把显示形原样存回」不会产生平台化覆盖。
//
// 场景：macOS 上默认值 CmdOrCtrl+Alt+R 显示为 ⌘⌥R，前端不改动直接把 ⌘⌥R
// 传回 Set。若写成覆盖，就变成 Meta+Alt+R，此后默认值再变也不会跟随。
func TestSetPhysicallyEqualIsNoop(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := New(nil, newFakeRegistrar(), store, Emit{})
	svc.platform = "darwin"
	svc.mu.Lock()
	svc.applyLocked()
	svc.mu.Unlock()

	report, err := svc.Set(ctx, ActionRecordToggle, "⌘⌥R")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !report.OK {
		t.Errorf("物理等价应视为成功: %+v", report)
	}
	raw, _ := store.Get(ctx, KeyBindingOverride)
	if raw != "" && raw != "{}" {
		t.Errorf("物理等价不该写覆盖表，实际 %s", raw)
	}

	// 换成真的不同的组合则必须写覆盖。
	if _, err := svc.Set(ctx, ActionRecordToggle, "⌘⌥K"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw, _ = store.Get(ctx, KeyBindingOverride)
	if !contains(raw, ActionRecordToggle) {
		t.Errorf("不同组合应写覆盖表，实际 %s", raw)
	}
}
