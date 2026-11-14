package recording

import (
	"sync"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// TestLegalTransitions 穷举状态机：每个 (from, to) 组合都必须与文档 §6.2 一致。
func TestLegalTransitions(t *testing.T) {
	all := []string{
		contract.StateIdle, contract.StatePreparing, contract.StateRecording,
		contract.StatePaused, contract.StateFinalizing, contract.StateCompleted,
		contract.StateInterrupted, contract.StateFailed,
	}
	// 期望的合法转换（与 transitions 表逐条对应，改表必须改这里）。
	want := map[string]map[string]bool{
		contract.StateIdle:        {contract.StatePreparing: true},
		contract.StatePreparing:   {contract.StateRecording: true, contract.StateFailed: true},
		contract.StateRecording:   {contract.StatePaused: true, contract.StateFinalizing: true, contract.StateInterrupted: true, contract.StateFailed: true},
		contract.StatePaused:      {contract.StateRecording: true, contract.StateFinalizing: true, contract.StateInterrupted: true, contract.StateFailed: true},
		contract.StateFinalizing:  {contract.StateCompleted: true, contract.StateInterrupted: true, contract.StateFailed: true},
		contract.StateCompleted:   {contract.StatePreparing: true},
		contract.StateInterrupted: {contract.StatePreparing: true},
		contract.StateFailed:      {contract.StatePreparing: true},
	}

	for _, from := range all {
		for _, to := range all {
			got := ValidTransition(from, to)
			expect := want[from][to]
			if got != expect {
				t.Fatalf("ValidTransition(%s→%s)=%v，期望 %v", from, to, got, expect)
			}
		}
	}
}

// TestStateMachineLegalFlow 走一遍主流程，断言每次 Current 都正确。
func TestStateMachineLegalFlow(t *testing.T) {
	m := NewStateMachine()
	if m.Current() != contract.StateIdle {
		t.Fatalf("初始状态 %s，期望 idle", m.Current())
	}
	steps := []string{
		contract.StatePreparing, contract.StateRecording, contract.StatePaused,
		contract.StateRecording, contract.StateFinalizing, contract.StateCompleted,
		contract.StatePreparing, contract.StateRecording, contract.StateFinalizing,
		contract.StateInterrupted,
	}
	for _, next := range steps {
		if err := m.To(next); err != nil {
			t.Fatalf("合法转换 %s → %s 被拒：%v", m.Current(), next, err)
		}
		if m.Current() != next {
			t.Fatalf("Current()=%s，期望 %s", m.Current(), next)
		}
	}
}

// TestIllegalTransitionsReturnStateForbidden 覆盖文档点名的非法转换，逐一断言错误码与 detail。
func TestIllegalTransitionsReturnStateForbidden(t *testing.T) {
	cases := []struct {
		name string
		from string
		to   string
	}{
		{"重复开始 recording→recording", contract.StateRecording, contract.StateRecording},
		{"已完成不能暂停 completed→paused", contract.StateCompleted, contract.StatePaused},
		{"空闲不能暂停 idle→paused", contract.StateIdle, contract.StatePaused},
		{"收尾中不能开始 finalizing→recording", contract.StateFinalizing, contract.StateRecording},
		{"空闲不能直接收尾 idle→finalizing", contract.StateIdle, contract.StateFinalizing},
		{"空闲不能直接完成 idle→completed", contract.StateIdle, contract.StateCompleted},
		{"已完成不能重复完成 completed→completed", contract.StateCompleted, contract.StateCompleted},
		{"暂停不能重新准备 paused→preparing", contract.StatePaused, contract.StatePreparing},
		{"中断不能直接录制 interrupted→recording", contract.StateInterrupted, contract.StateRecording},
		{"失败不能直接完成 failed→completed", contract.StateFailed, contract.StateCompleted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewStateMachine()
			if err := m.Force(c.from); err != nil {
				t.Fatalf("准备状态失败：%v", err)
			}
			err := m.To(c.to)
			if err == nil {
				t.Fatalf("%s → %s 应被拒绝", c.from, c.to)
			}
			if !apierr.HasCode(err, apierr.CodeStateForbidden) {
				t.Fatalf("错误码应为 ERR_STATE_FORBIDDEN，实际 %v", err)
			}
			// detail 必须带 from/to，前端与日志都靠它定位。
			var e *apierr.Err
			if !asErr(err, &e) {
				t.Fatalf("错误应可断言为 *apierr.Err：%v", err)
			}
			if e.Detail == "" || !contains(e.Detail, c.from) || !contains(e.Detail, c.to) {
				t.Fatalf("detail 应包含 from/to（%s→%s），实际 %q", c.from, c.to, e.Detail)
			}
			// 拒绝后状态不得改变。
			if m.Current() != c.from {
				t.Fatalf("被拒绝后状态应保持 %s，实际 %s", c.from, m.Current())
			}
		})
	}
}

// TestStateMachineRejectsUnknownState 断言未知状态值（如把标记当状态）被拒绝。
func TestStateMachineRejectsUnknownState(t *testing.T) {
	m := NewStateMachine()
	for _, bad := range []string{"degraded", "throttled", "suspended", "ST-03", ""} {
		if err := m.To(bad); err == nil {
			t.Fatalf("%q 不是合法状态值，应被拒绝", bad)
		}
		if IsKnownState(bad) {
			t.Fatalf("%q 不应被 IsKnownState 认可", bad)
		}
	}
}

// TestStateMachineConcurrent 并发转换：不 panic、状态始终是已知值、最终落到登记过的目标。
func TestStateMachineConcurrent(t *testing.T) {
	m := NewStateMachine()
	if err := m.To(contract.StatePreparing); err != nil {
		t.Fatalf("准备失败：%v", err)
	}
	if err := m.To(contract.StateRecording); err != nil {
		t.Fatalf("进入录制失败：%v", err)
	}

	var wg sync.WaitGroup
	// 一半 goroutine 想暂停/继续，一半想收尾：真实场景就是用户连点与快捷键同时来。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				switch i % 3 {
				case 0:
					_ = m.To(contract.StatePaused)
				case 1:
					_ = m.To(contract.StateRecording)
				default:
					_ = m.To(contract.StateFinalizing)
				}
				if !IsKnownState(m.Current()) {
					t.Errorf("出现未知状态：%q", m.Current())
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if !IsKnownState(m.Current()) {
		t.Fatalf("并发后状态未知：%q", m.Current())
	}
}

// TestStateToContract 断言非法/未知值不会被投影给前端（避免 UI 落到未知分支）。
func TestStateToContract(t *testing.T) {
	for _, s := range []string{
		contract.StateIdle, contract.StatePreparing, contract.StateRecording,
		contract.StatePaused, contract.StateFinalizing, contract.StateCompleted,
		contract.StateInterrupted, contract.StateFailed,
	} {
		if got := StateToContract(s); got != s {
			t.Fatalf("StateToContract(%s)=%s", s, got)
		}
	}
	if got := StateToContract("degraded"); got != contract.StateIdle {
		t.Fatalf("未知值应回落到 idle，实际 %s", got)
	}
}

// asErr 是 errors.As 的薄封装（避免测试文件里到处 import errors）。
func asErr(err error, target **apierr.Err) bool {
	if e, ok := err.(*apierr.Err); ok {
		*target = e
		return true
	}
	return false
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
