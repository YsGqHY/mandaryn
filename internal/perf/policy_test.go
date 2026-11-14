package perf

import (
	"runtime"
	"testing"
)

// allLevels 是档位全集（顺序即「从宽松到保守」）。
var allLevels = []Level{LevelL0, LevelL1, LevelL2, LevelL3, LevelP}

// TestThreadsForTable 穷举 cores × level 的换算结果。
//
// 断言的是**具体数值**而不只是性质：性质（单调、非零）可以靠一个残缺实现蒙对，
// 数值表则是「8 核机器上 L2 到底几个线程」这种排障时会被人直接引用的事实。
func TestThreadsForTable(t *testing.T) {
	cases := []struct {
		cores int
		l0    int
		l1    int
		l2    int
		l3    int
		p     int
	}{
		// cores<=0 按 1 处理：half = 0 → 1，quarter = 0 → 1。
		{cores: 0, l0: 1, l1: 1, l2: 1, l3: 1, p: 1},
		{cores: -4, l0: 1, l1: 1, l2: 1, l3: 1, p: 1},
		{cores: 1, l0: 1, l1: 1, l2: 1, l3: 1, p: 1},
		{cores: 2, l0: 1, l1: 1, l2: 1, l3: 1, p: 1},
		{cores: 4, l0: 2, l1: 2, l2: 1, l3: 1, p: 1},
		{cores: 8, l0: 4, l1: 4, l2: 2, l3: 1, p: 1},
		{cores: 16, l0: 8, l1: 8, l2: 4, l3: 1, p: 1},
	}

	for _, c := range cases {
		got := map[Level]int{
			LevelL0: ThreadsFor(LevelL0, c.cores),
			LevelL1: ThreadsFor(LevelL1, c.cores),
			LevelL2: ThreadsFor(LevelL2, c.cores),
			LevelL3: ThreadsFor(LevelL3, c.cores),
			LevelP:  ThreadsFor(LevelP, c.cores),
		}
		want := map[Level]int{
			LevelL0: c.l0, LevelL1: c.l1, LevelL2: c.l2, LevelL3: c.l3, LevelP: c.p,
		}
		for _, lv := range allLevels {
			if got[lv] != want[lv] {
				t.Errorf("ThreadsFor(%s, %d) = %d，期望 %d", lv, c.cores, got[lv], want[lv])
			}
		}

		// 性质 1：永不为 0（0 会变成「用库默认值」，绕过预算约束）。
		for _, lv := range allLevels {
			if got[lv] < 1 {
				t.Fatalf("ThreadsFor(%s, %d) = %d，必须 ≥1", lv, c.cores, got[lv])
			}
		}
		// 性质 2：L0 == L1（L1 的降级动作是「关二次校正」，与线程数无关）。
		if got[LevelL0] != got[LevelL1] {
			t.Errorf("cores=%d 时 L0(%d) 与 L1(%d) 必须相等", c.cores, got[LevelL0], got[LevelL1])
		}
		// 性质 3：L3 与 P 恒为 1。
		if got[LevelL3] != 1 || got[LevelP] != 1 {
			t.Errorf("cores=%d 时 L3/P 必须是 1，实际 L3=%d P=%d", c.cores, got[LevelL3], got[LevelP])
		}
		// 性质 4：随档位变保守单调不增。
		for i := 1; i < len(allLevels); i++ {
			prev, cur := got[allLevels[i-1]], got[allLevels[i]]
			if cur > prev {
				t.Errorf("cores=%d：%s(%d) → %s(%d) 出现递增，降档不能更吃 CPU",
					c.cores, allLevels[i-1], prev, allLevels[i], cur)
			}
		}
	}
}

// TestThreadsForNeverZero 用一组更宽的核数扫描，确保任何输入都不会算出 0。
func TestThreadsForNeverZero(t *testing.T) {
	for cores := -8; cores <= 64; cores++ {
		for _, lv := range allLevels {
			if n := ThreadsFor(lv, cores); n < 1 {
				t.Fatalf("ThreadsFor(%s, %d) = %d，必须 ≥1", lv, cores, n)
			}
		}
	}
}

// TestThreadsForUnknownLevelFallsBackToNormal 未知档位按 L0 处理（与 Level.Rank 同取向）。
func TestThreadsForUnknownLevelFallsBackToNormal(t *testing.T) {
	unknown := Level("L9")
	if got, want := ThreadsFor(unknown, 8), ThreadsFor(LevelL0, 8); got != want {
		t.Fatalf("未知档位应退回 L0 的换算，实际 %d，期望 %d", got, want)
	}
}

// TestRefinementAllowedOnlyL0 断言「第二遍离线校正」只在 L0 允许。
//
// 语义见 policy.go 的注释：它是边录边转（FR-03-10）路径上的动作，
// 当前无调用方，这里锁死定义以免后续接线时各人对 L1 的理解漂移。
func TestRefinementAllowedOnlyL0(t *testing.T) {
	cases := map[Level]bool{
		LevelL0: true,
		LevelL1: false, // L1 的定义就是关掉它
		LevelL2: false,
		LevelL3: false,
		LevelP:  false, // 连第一遍都不该跑
	}
	for lv, want := range cases {
		if got := RefinementAllowed(lv); got != want {
			t.Errorf("RefinementAllowed(%s) = %v，期望 %v", lv, got, want)
		}
	}
	if RefinementAllowed(Level("L9")) {
		t.Error("未知档位不应允许第二遍校正")
	}
}

// setLevel 把服务的档位快照直接推到 level（测试用，绕过采样与滞回）。
//
// 两处都改是必要的：`Scheduler.State()` 读的是它自己缓存的快照（判定发生在
// 采样 goroutine 里），判定器的 `cur` 则是下一次判定的起点。只改一处会出现
// 「对外状态是 L2、下一次判定又从 L0 起」这种测试替身特有的撕裂。
func setLevel(svc *Service, level Level) {
	svc.sched.budget.mu.Lock()
	svc.sched.budget.cur = level
	svc.sched.budget.last = BudgetState{
		Level:     level,
		Throttled: level.Throttled(),
		Suspended: level.Paused(),
		Reason:    reasonFor(level),
	}
	svc.sched.budget.hasLast = true
	st := svc.sched.budget.last
	svc.sched.budget.mu.Unlock()

	svc.sched.mu.Lock()
	svc.sched.state = st
	svc.sched.hasState = true
	svc.sched.mu.Unlock()
}

// 这里直接压低层档位（不经采样），因此不依赖平台采样源，macOS/Linux 也能跑。
func TestServiceThreadsFollowsLevel(t *testing.T) {
	svc := New(Options{})
	cores := runtime.NumCPU()

	if got, want := svc.Threads(), ThreadsFor(svc.State().Level, cores); got != want {
		t.Fatalf("初始 Threads()=%d，期望 %d", got, want)
	}
	if svc.State().Level != LevelL0 {
		t.Fatalf("新服务应从 L0 起，实际 %s", svc.State().Level)
	}

	// 直接把判定器与服务快照推到 L2：模拟采样循环已判出降档。
	// 两处都要改 —— 调度器的 state 是 `hasState` 决定的一次快照，不是每次读 budget。
	setLevel(svc, LevelL2)

	if got, want := svc.Threads(), ThreadsFor(LevelL2, cores); got != want {
		t.Fatalf("L2 时 Threads()=%d，期望 %d", got, want)
	}
	if svc.Threads() > ThreadsFor(LevelL0, cores) {
		t.Fatal("L2 的线程数不应超过 L0")
	}

	// P 档：线程数 1，但**录制不受影响**（本函数只算线程，不表达任何录制语义）。
	setLevel(svc, LevelP)

	if got := svc.Threads(); got != 1 {
		t.Fatalf("P 档 Threads()=%d，期望 1", got)
	}
	if err := svc.CheckInferenceAllowed(); err == nil {
		t.Fatal("P 档 CheckInferenceAllowed 应返回错误")
	}
}
