package transcribe

import (
	"sync/atomic"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// gateRunner 构造一个「手动放行、空闲受控」的 runner。
func gateRunner(t *testing.T, busy *atomic.Bool) *Runner {
	t.Helper()
	st, path, _ := twoSentenceStore(t)
	return mustRunner(t, st, newFakeEngine(), openerFor(path), Options{
		IdleFor: func(trigger string) bool {
			// 手动发起永远放行；空闲自动受整机忙闲影响。
			if trigger == contract.TriggerManual {
				return true
			}
			return !busy.Load()
		},
	})
}

// TestSchedulerIdleGateKeepsManualRunnable 整机忙时只拦「空闲自动」任务。
//
// 这是 FR-03-4 / NFR-08 的接线守护：注入 IdleFor 后，
// ① 被拦的空闲任务留在 queued（不启动、不消耗引擎）；
// ② 恢复空闲后同一任务自动续跑。
//
// 注意：busy 必须是原子量 —— 它会同时被测试 goroutine（写）与调度器
// goroutine（读）访问，用普通 bool 会被 `-race` 判为数据竞争。
func TestSchedulerIdleGateKeepsManualRunnable(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true) // 一开始整机忙
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()
	runner := mustRunner(t, st, eng, openerFor(path), Options{
		IdleFor: func(trigger string) bool {
			return trigger == contract.TriggerManual || !busy.Load()
		},
	})
	sched := newTestScheduler(t, runner)

	if _, err := sched.Enqueue(t.Context(), TaskRequest{SessionID: st.session.ID, Trigger: contract.TriggerIdle}); err != nil {
		t.Fatalf("空闲任务入队失败：%v", err)
	}
	// ① 忙：任务停在 queued，引擎零调用。
	waitFor(t, 2e9, sched.Settled, "整机忙时空闲任务应停在 queued")
	if n := eng.Calls(); n != 0 {
		t.Fatalf("整机忙时不应启动推理，实际引擎调用 %d 次", n)
	}
	// ② 放开空闲：自动续跑直到两句都转完。
	busy.Store(false)
	waitFor(t, 2e9, func() bool { return eng.Calls() >= 2 }, "空闲后任务应自动续跑")
}

// TestSchedulerPickSkipsBlockedHead 队首被拦时，后面的手动任务仍要被取到。
//
// 直接构造队列：只判定 pick 的选取逻辑，不依赖两个会话的存储。
func TestSchedulerPickSkipsBlockedHead(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	runner := gateRunner(t, &busy)
	sched := &Scheduler{
		runner: runner,
		opts:   SchedulerOptions{Poll: time.Millisecond},
		queue: []TaskRequest{
			{SessionID: "s_idle", Trigger: contract.TriggerIdle},
			{SessionID: "s_manual", Trigger: contract.TriggerManual},
		},
	}

	req, ctx, ok := sched.pick()
	if !ok {
		t.Fatal("整机忙时手动任务仍应被取出")
	}
	if req.SessionID != "s_manual" {
		t.Fatalf("应跳过被拦的队首取到手动任务，实际取到 %q", req.SessionID)
	}
	if ctx == nil {
		t.Fatal("取到任务必须同时创建 context")
	}
	if len(sched.queue) != 1 || sched.queue[0].SessionID != "s_idle" {
		t.Fatalf("被拦的任务应留在队列里，实际队列 = %+v", sched.queue)
	}

	// 忙时再取：只剩被拦任务 → 取不到。
	sched.running = false
	if _, _, ok := sched.pick(); ok {
		t.Fatal("整机忙时不应取到空闲任务")
	}

	// 空闲后应能取到它。
	busy.Store(false)
	sched.running = false
	req, _, ok = sched.pick()
	if !ok || req.SessionID != "s_idle" {
		t.Fatalf("空闲后应取到空闲任务，实际 ok=%v req=%+v", ok, req)
	}
}

// TestRunnerIdleForFallsBackToIdle 未注入 IdleFor 时行为与旧版完全一致（回退到 Idle）。
func TestRunnerIdleForFallsBackToIdle(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	var busy atomic.Bool
	busy.Store(true)
	runner := mustRunner(t, st, newFakeEngine(), openerFor(path), Options{
		Idle: func() bool { return !busy.Load() },
	})

	// IdleFor 未设置 → 任何来源都看 Idle：手动任务同样被拦（保持 Wave C 既有语义）。
	if runner.idleFor(contract.TriggerManual) {
		t.Fatal("未注入 IdleFor 时应回退 Idle：手动任务不应被单独放行")
	}
	busy.Store(false)
	if !runner.idleFor(contract.TriggerManual) {
		t.Fatal("空闲时手动任务应允许执行")
	}
}
