package recording

import (
	"runtime"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
)

// TestRecordingMemoryBounded 长跑内存验收（ROADMAP 14.3「可靠性：连续长时间录制内存无明显增长」）。
//
// 这条验收的关键不是「跑得够久」，而是**证明内存不随录制时长线性增长**：
// 录制路径上唯一与时长相关的缓冲是固定容量的环形缓冲（`RingBuffer`），
// 样本经重采样、落盘后即释放，因此堆应当是**稳态**而非累积。
//
// 采样口径：
//   - 用假通道（比实时快得多）连续喂样本，几十秒即可覆盖「几十分钟真实录制」的样本量；
//   - 先预热让环形缓冲、写盘缓冲、GORM 语句缓存进入稳态，再取基线；
//   - 每轮 `runtime.GC()` 后读 `HeapAlloc`，断言「峰值 ≤ 基线 + 容差」。
//
// 容差取 8MiB：实测稳态波动在 1MiB 量级；而若真的按样本量泄漏，
// 本测试累计喂入的音频量（数十 MB）会让增长立即越过该容差。
func TestRecordingMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("长跑内存测试在 -short 下跳过")
	}

	const (
		warmupMs  = 1500 // 预热时长
		rounds    = 8    // 采样轮数
		roundMs   = 1500 // 每轮录制时长
		tolerance = 8 << 20
	)

	fake := newFakeChannel(
		capture.AudioFormat{SampleRate: 48000, Channels: 2, Float32: true},
		fillSine(48000, 2, 440, 0.3),
	)
	// 每块读取的模拟耗时：让采集以「比实时快若干倍」的速度推进，
	// 否则等真实时间会让测试变成分钟级。
	fake.delay = 100 * time.Microsecond

	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:mem"}); err != nil {
		t.Fatalf("开始录制失败：%v", err)
	}
	t.Cleanup(func() {
		_, _ = rec.Stop()
	})

	readHeap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}

	waitElapsed(t, rec, warmupMs)
	baseline := readHeap()
	t.Logf("基线：堆 %.2f MiB（已录 %dms）", float64(baseline)/(1<<20), rec.State().ElapsedMs)

	var peak uint64
	for r := 1; r <= rounds; r++ {
		waitElapsed(t, rec, warmupMs+int64(r)*roundMs)
		cur := readHeap()
		if cur > peak {
			peak = cur
		}
		st := rec.State()
		t.Logf("第 %d 轮：堆 %.2f MiB（已录 %.1fs）",
			r, float64(cur)/(1<<20), float64(st.ElapsedMs)/1000)
	}

	if peak > baseline+tolerance {
		t.Fatalf("堆增长超出容差：基线 %.2f MiB → 峰值 %.2f MiB（容差 %.0f MiB）；"+
			"录制路径疑似按样本量累积内存",
			float64(baseline)/(1<<20), float64(peak)/(1<<20), float64(tolerance)/(1<<20))
	}

	st := rec.State()
	if st.State != contract.StateRecording {
		t.Fatalf("长跑后状态 = %q，期望仍在录制", st.State)
	}
	// 用有符号差值：稳态下峰值可能略低于基线（GC 更彻底），
	// 直接对 uint64 相减会下溢成天文数字。
	growth := float64(int64(peak)-int64(baseline)) / (1 << 20)
	t.Logf("长跑结束：录制约 %.1fs，堆峰值 %.2f MiB（相对基线 %+.2f MiB）",
		float64(st.ElapsedMs)/1000, float64(peak)/(1<<20), growth)
}

// waitElapsed 等到录制器累计时长达到 targetMs（避免固定 sleep 造成的 flaky）。
func waitElapsed(t *testing.T, rec *Recorder, targetMs int64) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if rec.State().ElapsedMs >= targetMs {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待录制到 %dms 超时（当前 %dms）", targetMs, rec.State().ElapsedMs)
}
