package hotkeys

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// Emit 是事件转发素材（由装配层桥接到 app.Event.Emit，L2 不 import Wails）。
//
// 事件名建议（见 docs/DEV-ISSUES-S9b.md S9B-03：events.go 目前没有热键事件常量，
// 由装配层决定并补常量）：`hotkeys:changed` / `hotkeys:lean`。
type Emit struct {
	// Changed 单个动作的绑定发生变化（Set / Reset / Disable / ResetAll 逐条）。
	Changed func(contract.Binding)
	// Lean 精简模式开关或取舍清单发生变化。
	Lean func(LeanState)
}

// ActionInfo 是动作元数据（补充 API：前端设置页需要分组与 i18n 键才能渲染）。
//
// 为什么不在 contract.Binding 上加字段：API-8.1 的 Binding 结构已在
// docs/API.md 定稿，加字段会与前端约定漂移；元数据与「当前绑定」本就是两个
// 关注点，分开两个方法更清楚。
type ActionInfo struct {
	// ID 稳定短标识（record.toggle）。
	ID string `json:"id"`
	// LabelKey i18n 键（hotkeys.actions.<id>）。
	LabelKey string `json:"labelKey"`
	// Group 场景分组（recording / playback / navigation / search / editing / model / ux）。
	Group string `json:"group"`
	// Scope 声明作用域（global | app）。
	Scope string `json:"scope"`
	// Default 默认加速键的显示形式；空串表示默认不绑定。
	Default string `json:"default"`
	// HasHandler 装配层是否已接线（false 时 Trigger 只会空转）。
	HasHandler bool `json:"hasHandler"`
}

// Service 是快捷键服务（API-8.1 ~ API-8.6 + Trigger / 精简模式）。
//
// 状态划分：
//   - overrides：用户覆盖（持久化，nil 值 = 该动作被禁用）；
//   - effective/effScope/effErr：**最近一次注册尝试**的结果，只读派生数据；
//   - 生效加速键 = overrides 优先，缺键回落动作默认值。
type Service struct {
	reg       *Registry
	registrar Registrar
	store     Store
	emit      Emit
	platform  string

	mu        sync.RWMutex
	overrides map[string]*string // actionID -> 规范形；nil = 禁用
	effective map[string]string  // actionID -> 已注册的平台加速键
	effScope  map[string]string  // actionID -> 实际生效作用域（global 失败会降级为 app）
	effErr    map[string]string  // actionID -> 注册失败原因键
	lean      LeanState
}

// New 构造服务。
//
// 参数可空：reg 为空时用 DefaultRegistry()；registrar 为空表示平台能力不可用
// （降级，不 panic）；store 为空表示不持久化（测试 / server 模式）。
// 构造时就会从 store 恢复绑定并尝试注册一次，因此装配层应在 Wails Run 之前构造
// （Wails 会把 Run 之前的注册排队，Run 后生效）。
func New(reg *Registry, registrar Registrar, store Store, emit Emit) *Service {
	if reg == nil {
		reg = DefaultRegistry()
	}
	s := &Service{
		reg:       reg,
		registrar: registrar,
		store:     store,
		emit:      emit,
		platform:  platformDefault(),
		overrides: map[string]*string{},
		effective: map[string]string{},
		effScope:  map[string]string{},
		effErr:    map[string]string{},
		lean:      defaultLeanState(),
	}
	s.mu.Lock()
	s.loadLocked(context.Background())
	s.applyLocked()
	s.mu.Unlock()
	return s
}

// Available 报告平台注册能力是否可用（false 时 UI 应提示「快捷键不可用」）。
func (s *Service) Available(_ context.Context) (bool, error) {
	return s.registrar != nil, nil
}

// List 返回全部动作用户可见绑定快照（API-8.1）。
//
// Accelerator / Default 用**显示形**（macOS 为 ⌘⌥R，其它平台 Ctrl+Alt+R），
// 与 DOC-05 §2.2 的表格一致；Set 接受同一形态（字形会被 Parse 还原）。
func (s *Service) List(_ context.Context) ([]contract.Binding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	actions := s.reg.All()
	out := make([]contract.Binding, 0, len(actions))
	for _, a := range actions {
		out = append(out, s.bindingLocked(a))
	}
	return out, nil
}

// ListActions 返回动作元数据（补充 API，供设置页分区渲染）。
func (s *Service) ListActions(_ context.Context) ([]ActionInfo, error) {
	actions := s.reg.All()
	out := make([]ActionInfo, 0, len(actions))
	for _, a := range actions {
		out = append(out, ActionInfo{
			ID:         a.ID,
			LabelKey:   a.LabelKey,
			Group:      a.Group,
			Scope:      a.Scope,
			Default:    s.displayOf(a.Default),
			HasHandler: a.Handler() != nil,
		})
	}
	return out, nil
}

// Set 设置某动作的绑定（API-8.2）：先校验格式、再查冲突、再预注册，最后落库。
//
// 「冲突必须在保存前完成」是 DOC-05 §2.3 的硬要求：不允许先存后失败，
// 否则用户会处于「以为改好了」的假状态。
//
// 可能错误：ERR_NOT_FOUND / ERR_INVALID_ARGUMENT（含 Reason 明细）/
// ERR_HOTKEY_CONFLICT（软件内重复或系统保留）/ ERR_HOTKEY_REGISTER（系统占用，已回滚）。
func (s *Service) Set(ctx context.Context, action, accelerator string) (contract.ConflictReport, error) {
	a, ok := s.reg.Get(action)
	if !ok {
		return contract.ConflictReport{}, apierr.New(apierr.CodeNotFound).
			WithDetail("unknown action %q", action)
	}
	combo, err := Parse(accelerator)
	if err != nil {
		return contract.ConflictReport{}, mapAccelErr(err)
	}

	s.mu.Lock()

	report := s.reportLocked(combo, action, a.Scope == contract.HotkeyScopeGlobal)
	if !report.OK {
		s.mu.Unlock()
		return report, apierr.New(apierr.CodeHotkeyConflict).
			WithDetail("action=%s accel=%s conflicts=%d", action, combo.String(), len(report.Conflicts))
	}

	// 物理等价即无操作：前端把列表里的显示形原样存回来（macOS 的 ⌘⌥R 之于
	// 默认值 CmdOrCtrl+Alt+R）不该产生一条平台化覆盖 —— 否则「没改过的动作」
	// 会陆续变成显式覆盖，失去跨平台默认值的可移植性。
	if current, enabled := s.currentLocked(a); enabled {
		if cur, err := Parse(current); err == nil &&
			cur.PlatformKey(s.platform) == combo.PlatformKey(s.platform) {
			s.mu.Unlock()
			return report, nil
		}
	}

	prev := cloneOverrides(s.overrides)
	canonical := combo.String()
	s.overrides[action] = &canonical
	s.applyLocked()

	// 用户主动保存时不允许「静默降级」：注册失败要驳回并保留原绑定（DOC-05 §2.3）。
	// 系统保留组合在冲突检测阶段就已拦下（走 ERR_HOTKEY_CONFLICT），
	// 能走到这里的失败只可能是「被其它程序占用」。
	if reason := s.effErr[action]; reason != "" {
		s.overrides = prev
		s.applyLocked()
		s.mu.Unlock()
		// 报告里同步标出「与系统冲突」，前端不需要再猜为什么被拒。
		report.Conflicts = append(report.Conflicts, contract.HotkeyConflict{
			Action:      action,
			Accelerator: combo.Display(s.platform),
			Reason:      reason,
		})
		report.OK = false
		return report, apierr.New(apierr.CodeHotkeyRegister).
			WithDetail("action=%s accel=%s reason=%s", action, canonical, reason)
	}

	if err := s.persistLocked(ctx); err != nil {
		s.overrides = prev
		s.applyLocked()
		s.mu.Unlock()
		return report, err
	}

	binding := s.bindingLocked(a)
	s.mu.Unlock()

	s.emitBinding(binding)

	logx.For("hotkeys").Info("绑定已更新", "action", action, "accel", canonical, "scope", binding.Scope)
	return report, nil
}

// Disable 禁用单个动作（API-8.4）：绑定存 null（DOC-04 TBL-01.1）。
func (s *Service) Disable(ctx context.Context, action string) error {
	a, ok := s.reg.Get(action)
	if !ok {
		return apierr.New(apierr.CodeNotFound).WithDetail("unknown action %q", action)
	}

	s.mu.Lock()

	prev := cloneOverrides(s.overrides)
	s.overrides[action] = nil
	s.applyLocked()
	if err := s.persistLocked(ctx); err != nil {
		s.overrides = prev
		s.applyLocked()
		s.mu.Unlock()
		return err
	}
	binding := s.bindingLocked(a)
	s.mu.Unlock()

	s.emitBinding(binding)
	return nil
}

// Reset 恢复单个动作的默认绑定（API-8.3）。
func (s *Service) Reset(ctx context.Context, action string) error {
	a, ok := s.reg.Get(action)
	if !ok {
		return apierr.New(apierr.CodeNotFound).WithDetail("unknown action %q", action)
	}

	s.mu.Lock()

	prev := cloneOverrides(s.overrides)
	delete(s.overrides, action)
	s.applyLocked()

	// 恢复默认也可能失败（默认组合被别的程序占用）：此时仍保留默认值，
	// 但由 effErr 标红提示，不回滚 —— 用户点的是「恢复默认」，不是「一定要成功」。
	if err := s.persistLocked(ctx); err != nil {
		s.overrides = prev
		s.applyLocked()
		s.mu.Unlock()
		return err
	}
	binding := s.bindingLocked(a)
	s.mu.Unlock()

	s.emitBinding(binding)
	return nil
}

// ResetAll 全部恢复默认（API-8.6）。
func (s *Service) ResetAll(ctx context.Context) error {
	s.mu.Lock()
	prev := cloneOverrides(s.overrides)
	s.overrides = map[string]*string{}
	s.applyLocked()
	if err := s.persistLocked(ctx); err != nil {
		s.overrides = prev
		s.applyLocked()
		s.mu.Unlock()
		return err
	}
	bindings := make([]contract.Binding, 0, s.reg.Len())
	for _, a := range s.reg.All() {
		bindings = append(bindings, s.bindingLocked(a))
	}
	s.mu.Unlock()

	for _, b := range bindings {
		s.emitBinding(b)
	}
	logx.For("hotkeys").Info("全部快捷键已恢复默认", "count", len(bindings))
	return nil
}

// DetectConflicts 单独探测某组合的冲突（API-8.5）。
//
// 只做**软件内**冲突与系统保留组合判断，不做真实注册探测（注册探测是有副作用
// 的操作，只在 Set 的预注册阶段做）。
func (s *Service) DetectConflicts(_ context.Context, accelerator string) (contract.ConflictReport, error) {
	combo, err := Parse(accelerator)
	if err != nil {
		return contract.ConflictReport{Preview: strings.TrimSpace(accelerator)},
			mapAccelErr(err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reportLocked(combo, "", true), nil
}

// Trigger 是「热键 / 托盘菜单 / 前端按钮」的统一动作入口（补充 API）。
//
// 统一入口的价值：动作逻辑只写一份，热键、托盘、按钮三条路径行为一致，
// 也不会出现「托盘能停录制、热键不能」这类分叉 bug。
//
// 可能错误：ERR_NOT_FOUND（未知动作）/ ERR_STATE_FORBIDDEN（已禁用或守卫拒绝）。
func (s *Service) Trigger(ctx context.Context, action string) error {
	a, ok := s.reg.Get(action)
	if !ok {
		return apierr.New(apierr.CodeNotFound).WithDetail("unknown action %q", action)
	}

	s.mu.RLock()
	_, enabled := s.currentLocked(a)
	s.mu.RUnlock()

	if !enabled {
		return apierr.New(apierr.CodeStateForbidden).
			WithDetail("action %q is disabled", action)
	}
	if guard := a.Guard(); guard != nil {
		if err := guard(); err != nil {
			return apierr.From(err, apierr.CodeStateForbidden)
		}
	}
	handler := a.Handler()
	if handler == nil {
		// 装配层还没接线（启动早期 / 未实现的页面动作）：不算错误，但留痕。
		logx.For("hotkeys").Debug("动作未接线，忽略触发", "action", action)
		return nil
	}
	if err := handler(ctx); err != nil {
		logx.For("hotkeys").Warn("动作执行失败", "action", action)
		return apierr.From(err, apierr.CodeStateForbidden)
	}
	return nil
}

// ServiceShutdown 是 Wails 的服务生命周期钩子（不是前端 API）。
//
// 退出前注销全部热键：系统级热键若不注销，某些平台上会在进程退出后短暂
// 残留（表现为「按了没反应、别的程序也注册不上」）。
func (s *Service) ServiceShutdown() error {
	if s.registrar == nil {
		return nil
	}
	return s.registrar.UnregisterAll()
}

// —— 内部实现 ——

// bindingLocked 组装单个动作的对外快照（调用方持写锁或读锁）。
func (s *Service) bindingLocked(a Action) contract.Binding {
	b := contract.Binding{
		Action:  a.ID,
		Default: s.displayOf(a.Default),
		Scope:   a.Scope,
	}
	accel, enabled := s.currentLocked(a)
	b.Enabled = enabled
	if enabled {
		b.Accelerator = s.displayOf(accel)
		if scope, ok := s.effScope[a.ID]; ok && scope != "" {
			// global 注册失败后会降级为 app，这里如实反映**实际**生效作用域。
			b.Scope = scope
		}
		b.RegisterErr = s.effErr[a.ID]
		if b.RegisterErr == "" && s.registrar == nil {
			b.RegisterErr = ReasonUnavailable
		}
	}
	return b
}

// currentLocked 返回动作当前的**规范形**加速键与是否启用。
func (s *Service) currentLocked(a Action) (string, bool) {
	if v, ok := s.overrides[a.ID]; ok {
		if v == nil || strings.TrimSpace(*v) == "" {
			return "", false
		}
		return *v, true
	}
	if a.Default == "" {
		return "", false
	}
	return a.Default, true
}

// reportLocked 生成冲突报告。
//
// excludeID 用于「改自己的绑定」时排除自身；checkReserved 决定是否判断系统保留组合
// （只有 global 作用域的保存才需要，应用内不劫持系统键）。
func (s *Service) reportLocked(combo Combo, excludeID string, checkReserved bool) contract.ConflictReport {
	report := contract.ConflictReport{
		Preview:   combo.Display(s.platform),
		Conflicts: []contract.HotkeyConflict{},
	}
	target := combo.PlatformKey(s.platform)

	for _, a := range s.reg.All() {
		if a.ID == excludeID {
			continue
		}
		accel, enabled := s.currentLocked(a)
		if !enabled {
			continue
		}
		other, err := Parse(accel)
		if err != nil {
			continue // 存量脏数据不该阻塞用户改别的动作
		}
		if other.PlatformKey(s.platform) != target {
			continue
		}
		report.Conflicts = append(report.Conflicts, contract.HotkeyConflict{
			Action:      a.ID,
			Accelerator: other.Display(s.platform),
			Reason:      ReasonDuplicate,
		})
	}

	if checkReserved && combo.Reserved(s.platform) {
		// Action 留空表示「系统保留组合，与具体动作无关」。
		report.Conflicts = append(report.Conflicts, contract.HotkeyConflict{
			Action:      excludeID,
			Accelerator: report.Preview,
			Reason:      ReasonSystemTaken,
		})
	}
	report.OK = len(report.Conflicts) == 0
	return report
}

// applyLocked 重建全部注册（调用方持写锁）。
//
// 采用「全量重建」而不是增量：动作数只有 18 个，重建成本可忽略，
// 换来的是「不存在残留注册」这一强不变量 —— 增量注销漏一步就会出现
// 「旧组合还能触发旧动作」的幽灵行为。
func (s *Service) applyLocked() {
	s.effective = map[string]string{}
	s.effScope = map[string]string{}
	s.effErr = map[string]string{}

	if s.registrar == nil {
		return
	}
	if err := s.registrar.UnregisterAll(); err != nil {
		logx.For("hotkeys").Warn("清理旧热键失败，继续重建")
	}

	for _, a := range s.reg.All() {
		accel, enabled := s.currentLocked(a)
		if !enabled {
			continue
		}
		combo, err := Parse(accel)
		if err != nil {
			// 存量脏数据（旧版本写的 / 手改库）：不注册并标红，不影响其它动作。
			s.effErr[a.ID] = ReasonInvalidStored
			logx.For("hotkeys").Warn("绑定无法解析，已跳过注册", "action", a.ID)
			continue
		}
		platformAccel := combo.PlatformString(s.platform)
		fn := s.callbackFor(a.ID)

		err = s.registrar.Register(a.Scope, platformAccel, fn)
		if err != nil && a.Scope == contract.HotkeyScopeGlobal {
			// 系统级注册失败 → 降级为应用内作用域并标红（DOC-05 §2.3 启动路径）。
			if fallbackErr := s.registrar.Register(contract.HotkeyScopeApp, platformAccel, fn); fallbackErr == nil {
				s.effective[a.ID] = platformAccel
				s.effScope[a.ID] = contract.HotkeyScopeApp
				s.effErr[a.ID] = ReasonSystemTaken
				logx.For("hotkeys").Warn("全局热键注册失败，已降级为应用内", "action", a.ID)
				continue
			}
			s.effErr[a.ID] = ReasonSystemTaken
			logx.For("hotkeys").Warn("热键注册失败", "action", a.ID, "scope", a.Scope)
			continue
		}
		if err != nil {
			s.effErr[a.ID] = ReasonSystemTaken
			logx.For("hotkeys").Warn("热键注册失败", "action", a.ID, "scope", a.Scope)
			continue
		}
		s.effective[a.ID] = platformAccel
		s.effScope[a.ID] = a.Scope
	}
}

// callbackFor 返回热键触发时的回调：统一走 Trigger，避免动作逻辑重复。
func (s *Service) callbackFor(actionID string) func() {
	return func() {
		if err := s.Trigger(context.Background(), actionID); err != nil {
			logx.For("hotkeys").Warn("热键触发失败", "action", actionID)
		}
	}
}

// persistLocked 把覆盖表整体写回（调用方持写锁）。
//
// 整体覆盖而不是单键更新：覆盖表本身很小（≤18 键），一次写避免
// 「部分键写成功、部分失败」导致的半更新状态。
func (s *Service) persistLocked(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	raw := make(map[string]*string, len(s.overrides))
	for k, v := range s.overrides {
		raw[k] = v
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	if err := s.store.Set(ctx, KeyBindingOverride, string(buf)); err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return nil
}

// loadLocked 从存储恢复覆盖表与精简模式（坏数据一律忽略，不阻断启动）。
func (s *Service) loadLocked(ctx context.Context) {
	s.loadLeanLocked(ctx)
	if s.store == nil {
		return
	}
	raw, err := s.store.Get(ctx, KeyBindingOverride)
	if err != nil {
		logx.For("hotkeys").Warn("读取快捷键绑定失败，按默认值运行")
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	var parsed map[string]*string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		logx.For("hotkeys").Warn("快捷键绑定数据无法解析，按默认值运行")
		return
	}
	for id, v := range parsed {
		if _, ok := s.reg.Get(id); !ok {
			// 旧版本遗留 / 手改库：未知动作直接丢弃，不让它污染内存态。
			continue
		}
		if v == nil {
			s.overrides[id] = nil
			continue
		}
		// 存量数据可能是非规范形（旧版本写的）：能解析就规范化后收下。
		canonical := mustNormalize(*v)
		if canonical == "" {
			continue
		}
		c := canonical
		s.overrides[id] = &c
	}
}

// emitBinding 转发单条绑定变更（nil 安全）。
func (s *Service) emitBinding(b contract.Binding) {
	if s.emit.Changed != nil {
		s.emit.Changed(b)
	}
}

// displayOf 把规范形转成显示形；空串原样返回。
func (s *Service) displayOf(canonical string) string {
	if strings.TrimSpace(canonical) == "" {
		return ""
	}
	combo, err := Parse(canonical)
	if err != nil {
		return canonical
	}
	return combo.Display(s.platform)
}

// cloneOverrides 深拷贝覆盖表（回滚快照）。
func cloneOverrides(in map[string]*string) map[string]*string {
	out := make(map[string]*string, len(in))
	for k, v := range in {
		if v == nil {
			out[k] = nil
			continue
		}
		val := *v
		out[k] = &val
	}
	return out
}

// mustNormalize 规范化存量字符串；失败返回空串（调用方据此跳过）。
func mustNormalize(s string) string {
	combo, err := Parse(s)
	if err != nil {
		return ""
	}
	return combo.String()
}

// mapAccelErr 把解析错误映射为 ERR_INVALID_ARGUMENT，并把稳定原因键放进 Detail。
//
// 不新造 apierr 码（apierr 的码表是唯一权威，DOC-03 §5），前端按
// detail 里的 reason= 区分文案。
func mapAccelErr(err error) *apierr.Err {
	var ae *AccelError
	if errors.As(err, &ae) {
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("accelerator=%q reason=%s token=%q", ae.Input, ae.Reason, ae.Token).
			WithCause(ae)
	}
	return apierr.From(err, apierr.CodeInvalidArgument)
}

// platformDefault 返回当前平台标识（os 形态与 runtime.GOOS 一致）。
func platformDefault() string { return normalizeOS("") }
