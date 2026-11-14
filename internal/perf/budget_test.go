package perf

import (
	"math"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// snap 构造一拍有效读数（只关心 otherPct 的用例用）。
func snap(other float64) Snapshot {
	return Snapshot{
		TotalCpuPct: other, // 只关心 other，total/self 由它派生，不影响档位判定
		SelfCpuPct:  0,
		OtherPct:    other,
		SampledAt:   time.Unix(1_700_000_000, 0),
		Valid:       true,
	}
}

// TestLevelForTable 逐条核对 DOC-02 §4.7.3 的档位阈值表（N = 80）。
//
// 阈值比例是相对 N 的：L0 > 60%、L1 > 40%、L2 > 20%、L3 > 0、其余 P。
// 边界值按文档的「≤」语义归类（例如 headroom 恰好 60% 属于 L1 而非 L0）。
func TestLevelForTable(t *testing.T) {
	const n = 80.0
	cases := []struct {
		name     string
		otherPct float64
		want     Level
	}{
		{"空载", 0, LevelL0},
		{"headroom=100%N", 0, LevelL0},
		{"headroom 略高于 60%N", 31.9, LevelL0},      // 48.1 / 80 = 60.1%
		{"headroom 恰为 60%N（≤ 归 L1）", 32, LevelL1}, // 48 / 80 = 60%
		{"headroom 50%N", 40, LevelL1},            // 40 / 80 = 50%
		{"headroom 恰为 40%N（≤ 归 L2）", 48, LevelL2}, // 32 / 80 = 40%
		{"headroom 30%N", 56, LevelL2},            // 24 / 80 = 30%
		{"headroom 恰为 20%N（≤ 归 L3）", 64, LevelL3}, // 16 / 80 = 20%
		{"headroom 极小", 79, LevelL3},              // 1 / 80 = 1.25%
		{"headroom 恰为 0（额度耗尽）", 80, LevelP},       // 0 / 80 = 0
		{"otherPct 超过 N（超额）", 95, LevelP},         // 负数 headroom
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := levelFor(n-c.otherPct, n)
			if got != c.want {
				t.Fatalf("otherPct=%.1f N=%.0f ⇒ %s, 期望 %s（headroom=%.1f）",
					c.otherPct, n, got, c.want, n-c.otherPct)
			}
		})
	}
}

// TestLevelForScalesWithCap 阈值必须按 N 缩放：N 变小不应让档位倒挂。
func TestLevelForScalesWithCap(t *testing.T) {
	// N = 40：headroom 24 ⇒ 60% ⇒ 恰好 L1（若用绝对百分点则会误判成 L0）。
	if got := levelFor(24, 40); got != LevelL1 {
		t.Fatalf("N=40 headroom=24 ⇒ %s, 期望 L1", got)
	}
	// N = 10：headroom 2 ⇒ 20% ⇒ 恰好 L3。
	if got := levelFor(2, 10); got != LevelL3 {
		t.Fatalf("N=10 headroom=2 ⇒ %s, 期望 L3", got)
	}
}

// TestLevelRankAndFlags 档位序关系与两个正交标记。
func TestLevelRankAndFlags(t *testing.T) {
	order := []Level{LevelL0, LevelL1, LevelL2, LevelL3, LevelP}
	for i := 1; i < len(order); i++ {
		if order[i].Rank() <= order[i-1].Rank() {
			t.Fatalf("%s 的 Rank 应大于 %s", order[i], order[i-1])
		}
	}
	if LevelL0.Throttled() || LevelL0.Paused() {
		t.Fatal("L0 不应有降档或暂停标记")
	}
	if !LevelL2.Throttled() || LevelL2.Paused() {
		t.Fatal("L2 应为降档但未暂停")
	}
	if !LevelP.Paused() {
		t.Fatal("P 必须带暂停标记")
	}
	// 逐级回升顺序：P→L3→L2→L1→L0。
	cur := LevelP
	want := []Level{LevelL3, LevelL2, LevelL1, LevelL0, LevelL0}
	for _, w := range want {
		cur = cur.stepUp()
		if cur != w {
			t.Fatalf("stepUp 得到 %s, 期望 %s", cur, w)
		}
	}
}

// newTestBudget 用 1s 采样周期构造判定器：DownHold 3s ⇒ 3 次确认，UpHold 15s ⇒ 15 次。
func newTestBudget() *Budget {
	return NewBudget(BudgetOptions{
		SampleInterval: time.Second,
		DownHold:       3 * time.Second,
		UpHold:         15 * time.Second,
	})
}

// TestBudgetDowngradeNeedsConfirmation 降档要连续确认：2 次不动，第 3 次才降。
func TestBudgetDowngradeNeedsConfirmation(t *testing.T) {
	b := newTestBudget()
	const n = 80
	const other = 40 // headroom 40 ⇒ 50% ⇒ L1

	for i := 1; i <= 2; i++ {
		st := b.Evaluate(snap(other), n)
		if st.Level != LevelL0 {
			t.Fatalf("第 %d 次判定就降档了（%s），应至少等 3 次确认", i, st.Level)
		}
		if st.Changed {
			t.Fatalf("第 %d 次判定不应报告 Changed", i)
		}
	}
	st := b.Evaluate(snap(other), n)
	if st.Level != LevelL1 {
		t.Fatalf("第 3 次判定应降到 L1，得到 %s", st.Level)
	}
	if !st.Changed || !st.Throttled {
		t.Fatalf("降档应报告 Changed/Throttled，得到 %+v", st)
	}
}

// TestBudgetDowngradeJumpsToTarget 额度突然被打光时一次落到 P（而不是每级 3s 爬 12s），
// 保证 PRD「3s 内进入暂停」可达。
func TestBudgetDowngradeJumpsToTarget(t *testing.T) {
	b := newTestBudget()
	const n = 80
	var st BudgetState
	for i := 0; i < 3; i++ {
		st = b.Evaluate(snap(90), n) // otherPct 90 > N 80 ⇒ headroom −10 ⇒ P
	}
	if st.Level != LevelP {
		t.Fatalf("3 次确认后应直接到 P，得到 %s", st.Level)
	}
	if !st.Suspended {
		t.Fatal("P 档必须带 Suspended 标记")
	}
	if st.Reason != ReasonExhausted {
		t.Fatalf("原因应为 %s，得到 %s", ReasonExhausted, st.Reason)
	}
	if st.HeadRoom >= 0 {
		t.Fatalf("超额时 HeadRoom 必须为负，得到 %.1f", st.HeadRoom)
	}
}

// TestBudgetHysteresisNoFlap 是**抗抖动**的核心断言：
// 输入在每个 tick 于「该降档」和「该升档」之间来回跳，档位必须一动不动，
// 而不是跟着每拍升降（那样用户会看到转写反复中断）。
func TestBudgetHysteresisNoFlap(t *testing.T) {
	b := newTestBudget()
	const n = 80

	for i := 0; i < 20; i++ {
		other := 40.0 // ⇒ L1 目标
		if i%2 == 1 {
			other = 20.0 // ⇒ L0 目标
		}
		st := b.Evaluate(snap(other), n)
		if st.Level != LevelL0 {
			t.Fatalf("第 %d 拍抖动后档位变成了 %s，应保持 L0（确认计数须清零）", i, st.Level)
		}
	}
}

// TestBudgetUpgradeNeedsLongerConfirmation 升档更保守：需要 15 次连续确认，且只升一级。
func TestBudgetUpgradeNeedsLongerConfirmation(t *testing.T) {
	b := newTestBudget()
	const n = 80

	// 先稳定降到 L2（other=56 ⇒ headroom 24 ⇒ 30% ⇒ L2）。
	for i := 0; i < 3; i++ {
		b.Evaluate(snap(56), n)
	}
	if got := b.Level(); got != LevelL2 {
		t.Fatalf("前置条件失败：档位 = %s, 期望 L2", got)
	}

	// other=20 ⇒ headroom 60 ⇒ 75% > 60%+10% ⇒ 满足升一档到 L1 的门槛。
	for i := 1; i < 15; i++ {
		st := b.Evaluate(snap(20), n)
		if st.Level != LevelL2 {
			t.Fatalf("第 %d 次确认就升档了（%s），应等满 15 次", i, st.Level)
		}
	}
	st := b.Evaluate(snap(20), n)
	if st.Level != LevelL1 {
		t.Fatalf("第 15 次确认应升到 L1，得到 %s", st.Level)
	}
	if !st.Changed || st.Reason != ReasonRecovered {
		t.Fatalf("升档应报告 Changed 且原因为 %s，得到 %+v", ReasonRecovered, st)
	}
	if st.Level == LevelL0 {
		t.Fatal("升档一次只能升一级（L2 → L1），不能直接跳回 L0")
	}
}

// TestBudgetUpgradeNeedsExtraMargin 升档与降档用**不同阈值**：
// headroom 62.5%（已高于 L0 门槛 60%，但没超过 60%+10% 的余量带）时必须留在 L1。
func TestBudgetUpgradeNeedsExtraMargin(t *testing.T) {
	b := newTestBudget()
	const n = 80

	for i := 0; i < 3; i++ {
		b.Evaluate(snap(60), n) // headroom 20 ⇒ 25% ⇒ L2
	}
	if got := b.Level(); got != LevelL2 {
		t.Fatalf("前置条件失败：档位 = %s, 期望 L2", got)
	}
	// 连续 15 次到 L1：other=38 ⇒ headroom 42 ⇒ 52.5%，刚好越过 L1 的升档门槛
	// （40% + 10% 余量 = 50%），但**够不到** L0（需 > 70%）。
	for i := 0; i < 15; i++ {
		b.Evaluate(snap(38), n)
	}
	if got := b.Level(); got != LevelL1 {
		t.Fatalf("应升到 L1，得到 %s", got)
	}

	// other=30 ⇒ headroom 50 ⇒ 62.5%：降档方向看是 L0（>60%），
	// 但升档方向要求 > 70% 才计数 ⇒ 必须稳定留在 L1。
	for i := 0; i < 100; i++ {
		if st := b.Evaluate(snap(30), n); st.Level != LevelL1 {
			t.Fatalf("第 %d 拍就升到 %s 了；升档必须越过 60%%+10%% 的余量带", i, st.Level)
		}
	}
}

// TestBudgetResumeFromP 从 P 恢复用「otherPct ≤ N − 5」的百分点滞回带（PRD 恢复判据）。
func TestBudgetResumeFromP(t *testing.T) {
	b := newTestBudget()
	const n = 80

	for i := 0; i < 3; i++ {
		b.Evaluate(snap(85), n) // 超额 ⇒ P
	}
	if got := b.Level(); got != LevelP {
		t.Fatalf("前置条件失败：档位 = %s, 期望 P", got)
	}

	// other = 76 > 75（= N−5）⇒ 不够格恢复，无论等多久。
	for i := 0; i < 50; i++ {
		if st := b.Evaluate(snap(76), n); st.Level != LevelP {
			t.Fatalf("otherPct=76 > N−5=75 时不应恢复，第 %d 拍变成了 %s", i, st.Level)
		}
	}

	// other = 74 ≤ 75 ⇒ 允许计数，但仍需 15 次确认（比降档保守）。
	for i := 1; i < 15; i++ {
		if st := b.Evaluate(snap(74), n); st.Level != LevelP {
			t.Fatalf("第 %d 次确认就恢复了（%s），应等满 15 次", i, st.Level)
		}
	}
	st := b.Evaluate(snap(74), n)
	if st.Level != LevelL3 {
		t.Fatalf("恢复后应到 L3（一次只升一级），得到 %s", st.Level)
	}
	if !st.Resumed {
		t.Fatal("从 P 恢复必须带 Resumed 标记（用于区分 perf-resume 与普通降档）")
	}
	if st.Suspended {
		t.Fatal("恢复后不应再有 Suspended")
	}
	if got := st.Marker(); got != contract.MarkerPerfResume {
		t.Fatalf("Marker = %s, 期望 %s", got, contract.MarkerPerfResume)
	}
}

// TestBudgetInvalidSampleKeepsLevel 无数据拍（首次 / 间隔 0 / 计数器回绕）不得改变档位，
// 也不得把已累积的确认计数清零。
func TestBudgetInvalidSampleKeepsLevel(t *testing.T) {
	b := newTestBudget()
	const n = 80

	for i := 0; i < 3; i++ {
		b.Evaluate(snap(56), n) // ⇒ L2
	}
	if got := b.Level(); got != LevelL2 {
		t.Fatalf("前置条件失败：档位 = %s, 期望 L2", got)
	}

	for i := 0; i < 10; i++ {
		st := b.Evaluate(Snapshot{Valid: false}, n)
		if st.Level != LevelL2 {
			t.Fatalf("无数据拍不应改档位，第 %d 拍变成了 %s", i, st.Level)
		}
		if st.Reason != ReasonNoData {
			t.Fatalf("无数据拍原因应为 %s，得到 %s", ReasonNoData, st.Reason)
		}
		if st.Changed {
			t.Fatal("无数据拍不应报告 Changed")
		}
	}
}

// TestBudgetCapChangeTakesEffect 改 N 立即影响档位：同一个 otherPct，
// N 从 80 调到 100 后额度变宽，档位应当回升（说明每拍都重读 N 而不是缓存）。
func TestBudgetCapChangeTakesEffect(t *testing.T) {
	b := newTestBudget()
	// otherPct 恒为 60。
	for i := 0; i < 3; i++ {
		b.Evaluate(snap(60), 80) // headroom 20 ⇒ 25% ⇒ L2
	}
	if got := b.Level(); got != LevelL2 {
		t.Fatalf("N=80 时档位 = %s, 期望 L2", got)
	}
	// N 提到 100：headroom 40 ⇒ 40% ⇒ 仍不到 L1 的升档门槛（>50%）。
	// 关键是它不能被当成「额度耗尽」继续降档。
	for i := 0; i < 5; i++ {
		b.Evaluate(snap(60), 100)
	}
	if got := b.Level(); got != LevelL2 {
		t.Fatalf("N=100 时档位 = %s, 期望保持 L2", got)
	}
	// N 降到 20：headroom = −40 ⇒ 必须立即（3 次确认后）进 P。
	for i := 0; i < 3; i++ {
		b.Evaluate(snap(60), 20)
	}
	if got := b.Level(); got != LevelP {
		t.Fatalf("N=20 otherPct=60 应进 P，得到 %s", got)
	}
}

// TestBudgetMarkerMapping 档位变化到时间轴标记的映射（DOC-04 TBL-10）。
func TestBudgetMarkerMapping(t *testing.T) {
	b := newTestBudget()
	const n = 80

	if got := b.Evaluate(snap(0), n).Marker(); got != "" {
		t.Fatalf("未变化时 Marker 应为空，得到 %s", got)
	}
	var st BudgetState
	for i := 0; i < 3; i++ {
		st = b.Evaluate(snap(40), n) // ⇒ L1
	}
	if got := st.Marker(); got != contract.MarkerPerfThrottle {
		t.Fatalf("降档 Marker = %s, 期望 %s", got, contract.MarkerPerfThrottle)
	}
	for i := 0; i < 3; i++ {
		st = b.Evaluate(snap(90), n) // ⇒ P
	}
	if got := st.Marker(); got != contract.MarkerPerfSuspend {
		t.Fatalf("暂停 Marker = %s, 期望 %s", got, contract.MarkerPerfSuspend)
	}
}

// TestBudgetOptionsDefaults 零值参数走默认（3s / 15s / 10% / 5 个百分点）。
func TestBudgetOptionsDefaults(t *testing.T) {
	b := NewBudget(BudgetOptions{})
	o := b.Options()
	if o.SampleInterval != DefaultSampleInterval || o.DownHold != 3*time.Second ||
		o.UpHold != 15*time.Second || math.Abs(o.UpMarginRatio-0.10) > 1e-9 ||
		math.Abs(o.ResumeMarginPct-5) > 1e-9 {
		t.Fatalf("默认参数不符: %+v", o)
	}
	if b.downTicks != 6 || b.upTicks != 30 { // 500ms 周期下 3s/15s
		t.Fatalf("确认次数 = %d/%d, 期望 6/30", b.downTicks, b.upTicks)
	}
}

// TestBudgetReset 复位回 L0 并清空计数。
func TestBudgetReset(t *testing.T) {
	b := newTestBudget()
	for i := 0; i < 3; i++ {
		b.Evaluate(snap(56), 80)
	}
	b.Reset()
	if got := b.Level(); got != LevelL0 {
		t.Fatalf("Reset 后档位 = %s, 期望 L0", got)
	}
	if got := b.State(); got.Level != LevelL0 || got.Throttled || got.Suspended {
		t.Fatalf("Reset 后状态异常: %+v", got)
	}
}

// TestNormalizeCap N 的兜底夹紧（越界值不能把档位算成 NaN）。
func TestNormalizeCap(t *testing.T) {
	cases := map[int]int{0: DefaultCapPct, -5: DefaultCapPct, 5: MinCapPct, 10: 10, 80: 80, 100: 100, 150: MaxCapPct}
	for in, want := range cases {
		if got := normalizeCap(in); got != want {
			t.Fatalf("normalizeCap(%d) = %d, 期望 %d", in, got, want)
		}
	}
}
