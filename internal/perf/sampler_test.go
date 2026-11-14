package perf

import (
	"errors"
	"math"
	"testing"
	"time"
)

// seqReader 按给定顺序返回读数，用完后重复最后一个（模拟计数器持续前进）。
func seqReader(steps ...rawCounters) readFunc {
	i := 0
	return func() (rawCounters, error) {
		if i >= len(steps) {
			return steps[len(steps)-1], nil
		}
		v := steps[i]
		i++
		return v, nil
	}
}

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %.3f, 期望 %.3f（容差 %.3f）", name, got, want, tol)
	}
}

// TestSamplerSelfCpuPctNormalizedByMachine 是**本域最关键的一测**：
// 4 核机器、本进程占满 1 核 ⇒ selfCpuPct 必须是 25（整机归一），而不是 100。
//
// 若把分母写成「本进程可用核数」，selfCpuPct 会是 100，otherPct = total − self
// 立即变成大负数，headRoom 永远充足，整个预算域静默失效（永不降档）。
func TestSamplerSelfCpuPctNormalizedByMachine(t *testing.T) {
	const (
		// 4 核算力的一段窗口：ΔSystemTotal 代表 4 个核 × 窗口时长。
		dTotal = 4000
		// 本进程占满 1 核。
		dSelf = 1000
	)
	s := newSamplerWithReader(seqReader(
		rawCounters{SystemTotal: 10_000, SystemIdle: 8_000, Self: 500},
		// Δidle = 0 ⇒ 整机满负荷；Δself = 1000 ⇒ 1 核。
		rawCounters{SystemTotal: 10_000 + dTotal, SystemIdle: 8_000, Self: 500 + dSelf},
	))

	if _, err := s.Sample(); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("首次采样应返回 ErrNoBaseline，得到 %v", err)
	}
	snap, err := s.Sample()
	if err != nil {
		t.Fatalf("第二次采样失败: %v", err)
	}
	if !snap.Valid {
		t.Fatal("有效采样应置 Valid")
	}

	approx(t, "TotalCpuPct", snap.TotalCpuPct, 100, 0.001)
	approx(t, "SelfCpuPct", snap.SelfCpuPct, 25, 0.001)
	approx(t, "OtherPct", snap.OtherPct, 75, 0.001)
}

// TestSamplerTotalCpuPctIdleShare 验证整机占用率按 idle 份额算：
// Δidle 占 Δtotal 的 3/4 ⇒ 整机占用 25%。
func TestSamplerTotalCpuPctIdleShare(t *testing.T) {
	s := newSamplerWithReader(seqReader(
		rawCounters{SystemTotal: 1_000, SystemIdle: 0, Self: 0},
		rawCounters{SystemTotal: 5_000, SystemIdle: 3_000, Self: 0},
	))
	_, _ = s.Sample()
	snap, err := s.Sample()
	if err != nil {
		t.Fatalf("采样失败: %v", err)
	}
	approx(t, "TotalCpuPct", snap.TotalCpuPct, 25, 0.001)
	approx(t, "OtherPct", snap.OtherPct, 25, 0.001)
}

// TestSamplerNoBaselineFirstSample 首次采样没有基准，必须不 panic 且返回可解释结果。
func TestSamplerNoBaselineFirstSample(t *testing.T) {
	s := newSamplerWithReader(seqReader(rawCounters{SystemTotal: 100, SystemIdle: 50, Self: 10}))
	snap, err := s.Sample()
	if !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("err = %v, 期望 ErrNoBaseline", err)
	}
	if snap.Valid {
		t.Fatal("无基准时 Valid 必须为 false")
	}
	if snap.SampledAt.IsZero() {
		t.Fatal("即便无基准也应带上采样时刻")
	}
}

// TestSamplerZeroInterval 两次读数完全相同（间隔为 0 / 计数器粒度未到）：
// 必须返回 ErrNoInterval，且**不能**污染基准——下一拍仍能算出正确占用率。
func TestSamplerZeroInterval(t *testing.T) {
	same := rawCounters{SystemTotal: 1_000, SystemIdle: 500, Self: 100}
	s := newSamplerWithReader(seqReader(
		same,
		same, // ΔSystemTotal = 0
		rawCounters{SystemTotal: 5_000, SystemIdle: 3_000, Self: 600}, // 跨过两拍的真实增量
	))

	_, _ = s.Sample()
	if _, err := s.Sample(); !errors.Is(err, ErrNoInterval) {
		t.Fatalf("err = %v, 期望 ErrNoInterval", err)
	}
	snap, err := s.Sample()
	if err != nil {
		t.Fatalf("零间隔后应能恢复采样: %v", err)
	}
	// 基准仍是第一拍 ⇒ Δtotal = 4000，Δidle = 2500，Δself = 500。
	approx(t, "TotalCpuPct", snap.TotalCpuPct, 37.5, 0.001)
	approx(t, "SelfCpuPct", snap.SelfCpuPct, 12.5, 0.001)
}

// TestSamplerCounterReset 计数器回退（休眠 / 回绕）：返回 ErrCounterReset 并以新值重建基准，
// 绝不拿无意义的负差值算出离谱占用率（那会直接把推理误判到 P 档）。
func TestSamplerCounterReset(t *testing.T) {
	s := newSamplerWithReader(seqReader(
		rawCounters{SystemTotal: 1_000_000, SystemIdle: 900_000, Self: 50_000},
		// FILETIME 高 32 位丢失后的典型形态：计数凭空变小。
		rawCounters{SystemTotal: 500, SystemIdle: 400, Self: 20},
		rawCounters{SystemTotal: 4_500, SystemIdle: 3_400, Self: 120}, // 相对新基准 +4000/+3000/+100
	))

	_, _ = s.Sample()
	if _, err := s.Sample(); !errors.Is(err, ErrCounterReset) {
		t.Fatalf("err = %v, 期望 ErrCounterReset", err)
	}
	snap, err := s.Sample()
	if err != nil {
		t.Fatalf("回退后应重建基准并继续采样: %v", err)
	}
	approx(t, "TotalCpuPct", snap.TotalCpuPct, 25, 0.001)
	approx(t, "SelfCpuPct", snap.SelfCpuPct, 2.5, 0.001)
}

// TestSamplerSelfClampedToTotal 本进程时间粒度比整机细，极短窗口下可能算出 self > total；
// 必须夹住，否则 otherPct 为负会凭空造出额度。
func TestSamplerSelfClampedToTotal(t *testing.T) {
	s := newSamplerWithReader(seqReader(
		rawCounters{SystemTotal: 0, SystemIdle: 0, Self: 0},
		rawCounters{SystemTotal: 100, SystemIdle: 90, Self: 500}, // self 远超 total
	))
	_, _ = s.Sample()
	snap, err := s.Sample()
	if err != nil {
		t.Fatalf("采样失败: %v", err)
	}
	if snap.SelfCpuPct > snap.TotalCpuPct {
		t.Fatalf("SelfCpuPct(%.1f) 不应超过 TotalCpuPct(%.1f)", snap.SelfCpuPct, snap.TotalCpuPct)
	}
	if snap.OtherPct < 0 {
		t.Fatalf("OtherPct 不应为负，得到 %.1f", snap.OtherPct)
	}
}

// TestSamplerReaderErrorPropagates 读数系统调用失败要冒泡（区别于「无数据」）。
func TestSamplerReaderErrorPropagates(t *testing.T) {
	boom := errors.New("读数炸了")
	s := newSamplerWithReader(func() (rawCounters, error) { return rawCounters{}, boom })
	if _, err := s.Sample(); !errors.Is(err, boom) {
		t.Fatalf("err = %v, 期望透传原始错误", err)
	}
}

// TestSamplerNilSafety 零值采样器不得 panic（装配层判空没做全时的兜底）。
func TestSamplerNilSafety(t *testing.T) {
	var s *Sampler
	snap, err := s.Sample()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, 期望 ErrUnsupported", err)
	}
	if snap.Valid {
		t.Fatal("nil 采样器不应产出有效读数")
	}
}

// TestSamplerResetClearsBaseline Reset 后回到「无基准」状态。
func TestSamplerResetClearsBaseline(t *testing.T) {
	s := newSamplerWithReader(seqReader(
		rawCounters{SystemTotal: 1_000, SystemIdle: 500, Self: 100},
		rawCounters{SystemTotal: 2_000, SystemIdle: 1_000, Self: 200},
	))
	_, _ = s.Sample()
	s.Reset()
	if _, err := s.Sample(); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("Reset 后首拍应为 ErrNoBaseline，得到 %v", err)
	}
}

// TestAvgWindowPoints 5s / 500ms ⇒ 10 点（DOC-02 §4.7.2），并验证取的是平均值而非瞬时值。
func TestAvgWindowPoints(t *testing.T) {
	w := newAvgWindow(5*time.Second, 500*time.Millisecond)
	if w.Size() != 10 {
		t.Fatalf("窗口点数 = %d, 期望 10", w.Size())
	}

	// 前 9 拍 otherPct = 0，第 10 拍 = 100：平均值应为 10，瞬时值会是 100。
	for i := 0; i < 9; i++ {
		w.Add(Snapshot{TotalCpuPct: 0, SelfCpuPct: 0, OtherPct: 0, Valid: true})
	}
	got := w.Add(Snapshot{TotalCpuPct: 100, SelfCpuPct: 0, OtherPct: 100, Valid: true})
	if !got.Valid {
		t.Fatal("有样本时 Valid 应为 true")
	}
	approx(t, "OtherPct 平均", got.OtherPct, 10, 0.001)

	// 再灌 10 拍 100：窗口被完全覆盖，平均值应为 100。
	for i := 0; i < 10; i++ {
		got = w.Add(Snapshot{TotalCpuPct: 100, SelfCpuPct: 0, OtherPct: 100, Valid: true})
	}
	approx(t, "OtherPct 覆盖后平均", got.OtherPct, 100, 0.001)
}

// TestAvgWindowDefaults 非法参数退化为默认值，不 panic。
func TestAvgWindowDefaults(t *testing.T) {
	w := newAvgWindow(0, 0)
	if w.Size() != int(DefaultWindow/DefaultSampleInterval) {
		t.Fatalf("窗口点数 = %d, 期望默认 %d", w.Size(), int(DefaultWindow/DefaultSampleInterval))
	}
}

// TestSnapshotWithCap HeadRoom = N − OtherPct，且允许为负（超额）。
func TestSnapshotWithCap(t *testing.T) {
	s := Snapshot{OtherPct: 30, Valid: true}
	if got := s.WithCap(80).HeadRoom; math.Abs(got-50) > 1e-9 {
		t.Fatalf("HeadRoom = %.1f, 期望 50", got)
	}
	if got := s.WithCap(80).WithCap(30).HeadRoom; math.Abs(got) > 1e-9 {
		t.Fatalf("otherPct == N 时 HeadRoom 应为 0，得到 %.1f", got)
	}
	if got := (Snapshot{OtherPct: 90}).WithCap(80).HeadRoom; got >= 0 {
		t.Fatalf("otherPct > N 时 HeadRoom 必须为负，得到 %.1f", got)
	}
}

// TestSamplerRealPlatform 用**真实**平台采样源跑一拍（本机 Windows 必跑，
// 无采样源的平台 Skip）。它验证的是「平台绑定本身能跑通且给出合理量级」：
// 假计数器测不出 GetSystemTimes 手写结构、/proc 解析这类平台错误。
func TestSamplerRealPlatform(t *testing.T) {
	s, err := NewSampler()
	if err != nil {
		t.Skipf("本平台无可靠采样源，按设计降级：%v", err)
	}
	if _, err := s.Sample(); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("首拍应为 ErrNoBaseline，得到 %v", err)
	}

	// 真实计数器需要一点墙钟时间才有增量；Windows 的 GetSystemTimes 粒度约 15.6ms。
	time.Sleep(150 * time.Millisecond)

	snap, err := s.Sample()
	if err != nil {
		t.Fatalf("真实采样失败: %v", err)
	}
	if !snap.Valid {
		t.Fatal("真实采样应得到有效读数")
	}
	for name, v := range map[string]float64{
		"TotalCpuPct": snap.TotalCpuPct,
		"SelfCpuPct":  snap.SelfCpuPct,
		"OtherPct":    snap.OtherPct,
	} {
		if v < 0 || v > 100 {
			t.Fatalf("%s = %.2f, 越出 [0,100]（量纲或平台绑定有误）", name, v)
		}
	}
	if snap.SelfCpuPct > snap.TotalCpuPct+1e-9 {
		t.Fatalf("SelfCpuPct(%.2f) 不应超过 TotalCpuPct(%.2f)", snap.SelfCpuPct, snap.TotalCpuPct)
	}
	if math.Abs(snap.OtherPct-(snap.TotalCpuPct-snap.SelfCpuPct)) > 1e-9 {
		t.Fatalf("OtherPct 应等于 Total−Self，得到 %.2f vs %.2f",
			snap.OtherPct, snap.TotalCpuPct-snap.SelfCpuPct)
	}
	if s.Clocks() < 1 {
		t.Fatalf("核数 = %d, 至少为 1", s.Clocks())
	}
}
