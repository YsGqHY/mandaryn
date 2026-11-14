package perf

import (
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// newTestMetrics 用小环形缓冲构造收集器，便于验证覆盖行为。
func newTestMetrics(capacity int) *Metrics {
	return NewMetrics(capacity, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestMetricsLevelRecording 档位变化打点：计数、事件内容与标记映射。
func TestMetricsLevelRecording(t *testing.T) {
	m := newTestMetrics(16)

	st := BudgetState{Level: LevelL1, Reason: ReasonNarrowing, Changed: true, Throttled: true, CapPct: 80,
		OtherPct: 40, HeadRoom: 40}
	m.RecordLevel(st, "s1", 1234)

	events := m.Events()
	if len(events) != 1 {
		t.Fatalf("打点数 = %d, 期望 1", len(events))
	}
	e := events[0]
	if e.Kind != PointLevel || e.Level != string(LevelL1) || e.Reason != ReasonNarrowing ||
		e.SessionID != "s1" || e.AtMs != 1234 {
		t.Fatalf("打点内容不符: %+v", e)
	}

	s := m.Summary()
	if s.LevelChanges != 1 {
		t.Fatalf("LevelChanges = %d, 期望 1", s.LevelChanges)
	}
	if s.SuspendCount != 0 {
		t.Fatalf("SuspendCount = %d, 期望 0", s.SuspendCount)
	}

	// 进入暂停要单独计数。
	m.RecordLevel(BudgetState{Level: LevelP, Reason: ReasonExhausted, Changed: true, Suspended: true}, "s1", 2000)
	if got := m.Summary().SuspendCount; got != 1 {
		t.Fatalf("SuspendCount = %d, 期望 1", got)
	}
}

// TestMetricsFFDSLDAggregation FFD/SLD 聚合：P50/P95 与最大值。
func TestMetricsFFDSLDAggregation(t *testing.T) {
	m := newTestMetrics(64)

	m.RecordFFD("s1", 800, 0)
	m.RecordFFD("s2", 1200, 0)

	// SLD 样本故意用乱序，验证聚合前会排序。
	for _, v := range []float64{900, 100, 500, 300, 700} {
		m.RecordSLD("s1", v, 4000, "bundle@1", 0)
	}

	s := m.Summary()
	if s.FFDCount != 2 || math.Abs(s.FFDMaxMs-1200) > 1e-9 {
		t.Fatalf("FFD 聚合不符: %+v", s)
	}
	if s.SLDCount != 5 {
		t.Fatalf("SLDCount = %d, 期望 5", s.SLDCount)
	}
	// 升序后 [100 300 500 700 900]：P50 = 500；P95 落在下标 3.8
	// ⇒ 700 + (900−700)×0.8 = 860（线性插值）。
	if math.Abs(s.SLDP50Ms-500) > 1e-9 {
		t.Fatalf("SLDP50 = %.1f, 期望 500", s.SLDP50Ms)
	}
	if math.Abs(s.SLDP95Ms-860) > 1e-9 {
		t.Fatalf("SLDP95 = %.1f, 期望 860", s.SLDP95Ms)
	}
}

// TestMetricsRingBuffer 环形缓冲满后覆盖最旧、且 Events 仍按时间顺序返回。
func TestMetricsRingBuffer(t *testing.T) {
	m := newTestMetrics(3)
	for i := 1; i <= 5; i++ {
		m.RecordFFD("s", float64(i), int64(i))
	}
	events := m.Events()
	if len(events) != 3 {
		t.Fatalf("事件数 = %d, 期望被限制在 3", len(events))
	}
	want := []float64{3, 4, 5}
	for i, e := range events {
		if math.Abs(e.ValueMs-want[i]) > 1e-9 {
			t.Fatalf("第 %d 条 = %.0f, 期望 %.0f（最旧的应被覆盖）", i, e.ValueMs, want[i])
		}
	}
	// 聚合样本不因缓冲覆盖而丢失（聚合是独立累计的）。
	if got := m.Summary().FFDCount; got != 5 {
		t.Fatalf("FFDCount = %d, 期望 5", got)
	}
}

// TestMetricsReset 清空后聚合归零。
func TestMetricsReset(t *testing.T) {
	m := newTestMetrics(8)
	m.RecordLevel(BudgetState{Level: LevelL1, Changed: true}, "", 0)
	m.RecordFFD("s", 900, 0)
	m.RecordSLD("s", 500, 1000, "e", 0)

	m.Reset()
	s := m.Summary()
	if s.LevelChanges != 0 || s.FFDCount != 0 || s.SLDCount != 0 {
		t.Fatalf("Reset 后聚合应归零: %+v", s)
	}
	if len(m.Events()) != 0 {
		t.Fatal("Reset 后事件应为空")
	}
}

// TestMetricsNilSafety nil 收集器不得 panic（装配层没接打点时的兜底）。
func TestMetricsNilSafety(t *testing.T) {
	var m *Metrics
	m.RecordLevel(BudgetState{Level: LevelL1}, "", 0)
	m.RecordFFD("s", 1, 0)
	m.RecordSLD("s", 1, 1, "e", 0)
	m.Reset()
	if len(m.Events()) != 0 {
		t.Fatal("nil 收集器应返回空事件")
	}
	if got := m.Summary(); got != (Summary{}) {
		t.Fatalf("nil 收集器应返回零值聚合，得到 %+v", got)
	}
}

// TestMarkerFor 给装配层的标记转换：只有「变化」才有标记。
func TestMarkerFor(t *testing.T) {
	if _, _, ok := MarkerFor(BudgetState{Level: LevelL1, Changed: false}); ok {
		t.Fatal("未变化不应产生标记")
	}
	kind, note, ok := MarkerFor(BudgetState{Level: LevelL2, Reason: ReasonNarrowing, Changed: true})
	if !ok || kind != contract.MarkerPerfThrottle || note != ReasonNarrowing {
		t.Fatalf("MarkerFor = (%q,%q,%v), 期望 (%q,%q,true)",
			kind, note, ok, contract.MarkerPerfThrottle, ReasonNarrowing)
	}
	kind, _, ok = MarkerFor(BudgetState{Level: LevelP, Reason: ReasonExhausted, Changed: true, Suspended: true})
	if !ok || kind != contract.MarkerPerfSuspend {
		t.Fatalf("暂停标记 = %q, 期望 %q", kind, contract.MarkerPerfSuspend)
	}
	kind, _, ok = MarkerFor(BudgetState{Level: LevelL3, Reason: ReasonRecovered, Changed: true, Resumed: true})
	if !ok || kind != contract.MarkerPerfResume {
		t.Fatalf("恢复标记 = %q, 期望 %q", kind, contract.MarkerPerfResume)
	}
}

// TestPercentileEdges 分位的边界（空、单样本、两端）。
func TestPercentileEdges(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("空样本应为 0，得到 %v", got)
	}
	if got := percentile([]float64{42}, 0.95); got != 42 {
		t.Fatalf("单样本应原样返回，得到 %v", got)
	}
	xs := []float64{1, 2, 3, 4}
	if got := percentile(xs, 0); got != 1 {
		t.Fatalf("P0 = %v, 期望 1", got)
	}
	if got := percentile(xs, 1); got != 4 {
		t.Fatalf("P100 = %v, 期望 4", got)
	}
}

// TestNowRFC3339RoundTrip 保证 Usage.SampledAt 可被前端解析（EV-15 契约）。
func TestNowRFC3339RoundTrip(t *testing.T) {
	at := time.Unix(1_700_000_000, 123_456_789)
	s := newScheduler(newSamplerWithReader(seqReader(steps(10, 0, 5)...)), SchedulerOptions{
		SampleInterval: time.Second,
		Window:         time.Second,
		Logger:         quietLogger(),
		Now:            func() time.Time { return at },
	}, "")
	advance(s, 2)
	if _, err := time.Parse(time.RFC3339Nano, s.Usage().SampledAt); err != nil {
		t.Fatalf("SampledAt 解析失败: %v", err)
	}
}
