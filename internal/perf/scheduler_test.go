package perf

import (
	"context"
	"io"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// quietLogger 丢掉日志，避免单测输出噪声。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// steps 生成一串「稳定负载」读数：整机 4 核，其它进程占 otherPct、本进程占 selfPct。
//
// otherPct + selfPct 必须 ≤ 100（否则整机占用会被夹到 100%，otherPct 就不受控了）。
// 每拍 ΔSystemTotal = 4000（4 核）。
func steps(otherPct, selfPct float64, count int) []rawCounters {
	const dTotal = 4000.0
	dSelf := selfPct / 100 * dTotal
	busy := (otherPct + selfPct) / 100 * dTotal
	idle := dTotal - busy
	if idle < 0 {
		idle = 0
	}

	out := make([]rawCounters, 0, count+1)
	var total, idleAcc, selfAcc float64
	for i := 0; i <= count; i++ {
		out = append(out, rawCounters{
			SystemTotal: uint64(total),
			SystemIdle:  uint64(idleAcc),
			Self:        uint64(selfAcc),
			At:          time.Unix(1_700_000_000, 0).Add(time.Duration(i) * time.Second),
		})
		total += dTotal
		idleAcc += idle
		selfAcc += dSelf
	}
	return out
}

// newTestScheduler 用固定负载构造调度器（1s 采样周期，便于逐拍推进）。
func newTestScheduler(t *testing.T, otherPct, selfPct float64, cap *atomic.Int64) *Scheduler {
	t.Helper()
	opts := SchedulerOptions{
		SampleInterval: time.Second,
		Window:         5 * time.Second, // 5 点
		Budget: BudgetOptions{
			SampleInterval: time.Second,
			DownHold:       2 * time.Second,
			UpHold:         3 * time.Second,
		},
		Logger: quietLogger(),
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	if cap != nil {
		opts.Cap = func() int { return int(cap.Load()) }
	}
	return newScheduler(newSamplerWithReader(seqReader(steps(otherPct, selfPct, 120)...)), opts, "")
}

// advance 推进 n 拍。
func advance(s *Scheduler, n int) BudgetState {
	var st BudgetState
	for i := 0; i < n; i++ {
		st, _ = s.Tick()
	}
	return st
}

// TestIdleSemantics Idle 的三条判定：低负载空闲、高负载不空闲、暂停时不空闲。
func TestIdleSemantics(t *testing.T) {
	// 低负载：otherPct = 5 < 10 ⇒ 空闲。
	low := newTestScheduler(t, 5, 5, nil)
	if !low.Idle() {
		t.Fatal("启动初期无读数时应返回 true（不阻塞自动转写）")
	}
	advance(low, 3)
	if !low.Idle() {
		t.Fatalf("otherPct=5 且档位 L0 时应空闲，实际状态 %+v", low.State())
	}

	// 高负载：otherPct = 55 ⇒ 档位降下来且 otherPct ≥ 10 ⇒ 不空闲。
	high := newTestScheduler(t, 55, 5, nil)
	advance(high, 5)
	if high.State().Level == LevelL0 {
		t.Fatalf("前置条件失败：otherPct=55 应触发降档，实际 %s", high.State().Level)
	}
	if high.Idle() {
		t.Fatalf("otherPct=55 时应判为不空闲（档位 %s）", high.State().Level)
	}

	// 暂停：otherPct = 90 > N=80 ⇒ P，明确不空闲。
	paused := newTestScheduler(t, 90, 0, nil)
	advance(paused, 5)
	if !paused.State().Suspended {
		t.Fatalf("前置条件失败：应进入暂停，实际 %+v", paused.State())
	}
	if paused.Idle() {
		t.Fatal("暂停时不应判为空闲")
	}
}

// TestIdleForManualAlwaysAllowed **手动发起的转写永不受预算空闲判定限制**（FR-03-4）。
//
// 这条是产品硬要求：用户点了「开始转写」却因为「别的程序正忙」而毫无反应，
// 会被读成软件卡死。自动排队才该让位。
func TestIdleForManualAlwaysAllowed(t *testing.T) {
	busy := newTestScheduler(t, 90, 0, nil)
	advance(busy, 5)
	if busy.Idle() {
		t.Fatal("前置条件失败：高负载下 Idle 应为 false")
	}
	if !busy.IdleFor(contract.TriggerManual) {
		t.Fatal("手动发起必须返回 true（不受 Idle 限制）")
	}
	if busy.IdleFor(contract.TriggerIdle) {
		t.Fatal("自动排队必须受 Idle 限制")
	}
}

// TestIdleOnDegradedNeverBlocks 采样不可用时 Idle 恒 true（不阻塞），且能报告原因。
func TestIdleOnDegradedNeverBlocks(t *testing.T) {
	s := newScheduler(nil, SchedulerOptions{Logger: quietLogger()}, "平台不支持")
	degraded, reason := s.Degraded()
	if !degraded || reason == "" {
		t.Fatalf("降级模式应报告原因，得到 degraded=%v reason=%q", degraded, reason)
	}
	if !s.Idle() {
		t.Fatal("降级模式不应阻塞自动转写")
	}
	st, err := s.Tick()
	if err != nil {
		t.Fatalf("降级模式的 Tick 不应返回错误: %v", err)
	}
	if st.Level != LevelL0 || st.Suspended {
		t.Fatalf("降级模式档位应恒为 L0，得到 %+v", st)
	}
}

// TestSchedulerReadsCapEveryTick 每拍重读 N，改设置立即生效（不重启、不重建）。
func TestSchedulerReadsCapEveryTick(t *testing.T) {
	var capPct atomic.Int64
	capPct.Store(80)
	s := newTestScheduler(t, 90, 0, &capPct) // otherPct 90 > N=80 ⇒ 额度耗尽

	advance(s, 4)
	before := s.State().Level
	if before != LevelP {
		t.Fatalf("N=80 otherPct=90 应进 P，得到 %s", before)
	}

	// 把预算拉到 100：otherPct 90 ⇒ headroom 10 ⇒ 10% ⇒ 目标 L3，
	// 且 90 ≤ N−5 ⇒ 满足从 P 恢复的滞回带，无需重启即可回升。
	capPct.Store(100)
	advance(s, 5)
	after := s.State().Level
	if after.Rank() >= before.Rank() {
		t.Fatalf("提高 N 后档位应放宽：before=%s after=%s", before, after)
	}
	if got := s.State().CapPct; got != 100 {
		t.Fatalf("CapPct = %d, 期望 100（每拍重读）", got)
	}
}

// TestSchedulerUsagePayload EV-15 的字段映射与取整。
func TestSchedulerUsagePayload(t *testing.T) {
	s := newTestScheduler(t, 50, 25, nil)
	advance(s, 3)

	u := s.Usage()
	if u.CapPct != DefaultCapPct {
		t.Fatalf("CapPct = %d, 期望默认 %d", u.CapPct, DefaultCapPct)
	}
	// otherPct 恒 50 ⇒ 滑动平均后仍是 50；self 恒 25。
	if math.Abs(float64(u.OtherPct)-50) > 1 {
		t.Fatalf("OtherPct = %d, 期望 ≈50", u.OtherPct)
	}
	if math.Abs(float64(u.SelfCpuPct)-25) > 1 {
		t.Fatalf("SelfCpuPct = %d, 期望 ≈25（整机归一）", u.SelfCpuPct)
	}
	if u.HeadroomPct != u.CapPct-u.OtherPct {
		t.Fatalf("HeadroomPct = %d, 期望 CapPct−OtherPct = %d", u.HeadroomPct, u.CapPct-u.OtherPct)
	}
	if u.WindowMs != s.opts.Window.Milliseconds() {
		t.Fatalf("WindowMs = %d, 期望 %d", u.WindowMs, s.opts.Window.Milliseconds())
	}
	if _, err := time.Parse(time.RFC3339Nano, u.SampledAt); err != nil {
		t.Fatalf("SampledAt 不是 RFC3339Nano: %q", u.SampledAt)
	}
}

// TestSchedulerOnBudgetOnlyOnChange 只在档位**变化**时回调，避免每 500ms 轰上层。
func TestSchedulerOnBudgetOnlyOnChange(t *testing.T) {
	opts := SchedulerOptions{
		SampleInterval: time.Second,
		Window:         time.Second,
		Budget:         BudgetOptions{SampleInterval: time.Second, DownHold: 2 * time.Second, UpHold: 2 * time.Second},
		Logger:         quietLogger(),
	}
	var calls atomic.Int64
	opts.OnBudget = func(BudgetState) { calls.Add(1) }

	s := newScheduler(newSamplerWithReader(seqReader(steps(90, 0, 60)...)), opts, "")
	advance(s, 10)

	if got := calls.Load(); got != 1 {
		t.Fatalf("档位只变化一次，OnBudget 却调了 %d 次", got)
	}
}

// TestSchedulerStartStop 循环可启动、可停止，且 Stop 幂等。
func TestSchedulerStartStop(t *testing.T) {
	opts := SchedulerOptions{
		SampleInterval: 5 * time.Millisecond,
		Window:         50 * time.Millisecond,
		Logger:         quietLogger(),
	}
	s := newScheduler(newSamplerWithReader(seqReader(steps(10, 0, 200)...)), opts, "")
	ctx := context.Background()
	s.Start(ctx)
	s.Start(ctx) // 重复启动无副作用

	// 等采样真的跑起来。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.SnapshotOf().Valid {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !s.SnapshotOf().Valid {
		t.Fatal("Start 之后应产生有效读数")
	}

	s.Stop()
	s.Stop() // 幂等
	// Stop 之后不再推进：读数时间戳停住。
	before := s.SnapshotOf().SampledAt
	time.Sleep(20 * time.Millisecond)
	if !s.SnapshotOf().SampledAt.Equal(before) {
		t.Fatal("Stop 之后不应再采样")
	}
}

// TestSchedulerResetClearsWindow Reset 后回到「无读数」状态。
func TestSchedulerResetClearsWindow(t *testing.T) {
	s := newTestScheduler(t, 50, 25, nil)
	advance(s, 3)
	if !s.SnapshotOf().Valid {
		t.Fatal("前置条件失败：应有有效读数")
	}
	s.Reset()
	if s.SnapshotOf().Valid {
		t.Fatal("Reset 后应回到无读数状态")
	}
	if got := s.State().Level; got != LevelL0 {
		t.Fatalf("Reset 后档位 = %s, 期望 L0", got)
	}
}

// TestSchedulerWindowPoints 默认窗口 5s / 500ms = 10 点。
func TestSchedulerWindowPoints(t *testing.T) {
	s := newScheduler(newSamplerWithReader(seqReader(steps(10, 0, 5)...)), SchedulerOptions{Logger: quietLogger()}, "")
	if got := s.WindowPoints(); got != 10 {
		t.Fatalf("窗口点数 = %d, 期望 10", got)
	}
}

// —— 结构性守护：预算不得表达「停止录制」——

// TestBudgetStateHasNoRecordingSemantics NFR-08 硬要求「任何降档与暂停都只影响推理，
// 采集与落盘继续」。这里用反射把「BudgetState 里出现停录字段」变成测试失败，
// 让后来者改不动而不是靠注释提醒。
func TestBudgetStateHasNoRecordingSemantics(t *testing.T) {
	forbidden := []string{"record", "capture", "stop", "halt", "flush", "close"}

	typ := reflect.TypeOf(BudgetState{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Fatalf("BudgetState 不应有与录制/停止相关的字段: %s（NFR-08：录制永不中断）", typ.Field(i).Name)
			}
		}
	}

	// 方法层面同样不该出现「停录」能力。
	for _, rt := range []reflect.Type{reflect.TypeOf(&BudgetState{}), reflect.TypeOf(&Scheduler{}), reflect.TypeOf(&Service{})} {
		for i := 0; i < rt.NumMethod(); i++ {
			name := strings.ToLower(rt.Method(i).Name)
			if strings.Contains(name, "stoprecord") || strings.Contains(name, "haltrecord") ||
				strings.Contains(name, "stopcapture") {
				t.Fatalf("%s 不应提供停止录制/采集的方法: %s", rt.Elem().Name(), rt.Method(i).Name)
			}
		}
	}

	// Suspended 的语义必须是「推理暂停」而不是「一切暂停」——注释里必须写明，
	// 这里顺带断言字段确实存在（名字改了要重新审视语义有没有被偷换）。
	if _, ok := typ.FieldByName("Suspended"); !ok {
		t.Fatal("BudgetState 应保留 Suspended 字段（语义 = 暂停推理）")
	}
}

// TestServiceCheckInferenceAllowed 暂停期间需要推理的操作返回 ERR_PERF_SUSPENDED（可重试）。
func TestServiceCheckInferenceAllowed(t *testing.T) {
	svc := &Service{}
	svc.sched = newScheduler(newSamplerWithReader(seqReader(steps(90, 0, 20)...)), SchedulerOptions{
		SampleInterval: time.Second,
		Window:         time.Second,
		Budget:         BudgetOptions{SampleInterval: time.Second, DownHold: 2 * time.Second},
		Logger:         quietLogger(),
	}, "")
	svc.metrics = NewMetrics(8, quietLogger())

	// 尚未进入暂停：放行。
	if err := svc.CheckInferenceAllowed(); err != nil {
		t.Fatalf("未暂停时应放行，得到 %v", err)
	}
	advance(svc.sched, 4)
	if !svc.State().Suspended {
		t.Fatalf("前置条件失败：应进入暂停，实际 %+v", svc.State())
	}

	err := svc.CheckInferenceAllowed()
	if err == nil {
		t.Fatal("暂停期间必须拒绝需要推理的操作")
	}
	var ae *apierr.Err
	if !asAPIErr(err, &ae) {
		t.Fatalf("错误类型应为 *apierr.Err，得到 %T", err)
	}
	if ae.Code != apierr.CodePerfSuspended {
		t.Fatalf("错误码 = %s, 期望 %s", ae.Code, apierr.CodePerfSuspended)
	}
	if !ae.Retryable {
		t.Fatal("ERR_PERF_SUSPENDED 必须标记为可重试")
	}
}

// TestServiceNewDegradesInsteadOfFailing 采样源不可用时 New 不失败，而是降级并暴露原因。
//
// 用 New() 而非注入：本机（Windows）能起采样器，因此直接验证正常路径；
// 降级路径由上一条 Degraded 用例覆盖（newScheduler(nil, ...)）。
func TestServiceNewDegradesInsteadOfFailing(t *testing.T) {
	svc := New(Options{
		Logger:          quietLogger(),
		MetricsCapacity: 8,
	})
	if svc == nil {
		t.Fatal("New 不应返回 nil")
	}
	if svc.Metrics() == nil {
		t.Fatal("Metrics 不应为 nil")
	}
	// 无论采样成功与否，CheckInferenceAllowed 在未暂停时都应放行。
	if err := svc.CheckInferenceAllowed(); err != nil {
		t.Fatalf("刚启动不应拒绝推理: %v", err)
	}
	// Usage 的窗口口径固定 5s（EV-15 契约）。
	if got := svc.Usage().WindowMs; got != DefaultWindow.Milliseconds() {
		t.Fatalf("WindowMs = %d, 期望 %d", got, DefaultWindow.Milliseconds())
	}
}

// asAPIErr 是 errors.As 的本地包装（避免测试里到处写泛型样板）。
func asAPIErr(err error, target **apierr.Err) bool {
	for err != nil {
		if ae, ok := err.(*apierr.Err); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
