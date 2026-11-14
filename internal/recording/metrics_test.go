package recording

import (
	"math"
	"testing"
)

// TestMonitorReportCadence 断言上报节拍 = 250ms 音频（4Hz，帧驱动而非时钟驱动）。
func TestMonitorReportCadence(t *testing.T) {
	m := NewMonitor()
	const chunk = 1000 // 样本数
	buf := make([]float32, chunk)
	for i := range buf {
		buf[i] = 0.5
	}

	reports := 0
	fed := 0
	for fed < ReportWindowSamples*3 {
		if report, _ := m.Feed(buf, int64(fed)*2); report {
			reports++
		}
		fed += chunk
	}
	// 3 个窗口（750ms）= 3 次上报，允许最后一个窗口未填满。
	if reports < 3 || reports > 4 {
		t.Fatalf("750ms 音频应上报 3~4 次，实际 %d 次", reports)
	}
	if got := m.RecordedMs(); got < 740 || got > 760 {
		t.Fatalf("累计时长 %dms，期望约 750ms", got)
	}
}

// TestMonitorPeakAndRMS 断言峰值与 RMS 的计算。
func TestMonitorPeakAndRMS(t *testing.T) {
	m := NewMonitor()
	buf := make([]float32, ReportWindowSamples)
	for i := range buf {
		buf[i] = 0.5
	}
	report, _ := m.Feed(buf, 100)
	if !report {
		t.Fatal("填满一个窗口应触发上报")
	}
	got := m.Metrics()
	if math.Abs(float64(got.LevelPeak)-0.5) > 1e-6 {
		t.Fatalf("峰值 %.6f，期望 0.5", got.LevelPeak)
	}
	if math.Abs(float64(got.LevelRMS)-0.5) > 1e-6 {
		t.Fatalf("RMS %.6f，期望 0.5（恒值信号的 RMS 等于幅度）", got.LevelRMS)
	}
}

// TestMonitorZeroInputWarnsOnce 断言全零输入 10 秒后**只报一次**「未检测到音频」，
// 并断言它不改变状态、不产生空录音实例（状态由 Recorder 侧保证，这里先验指标侧）。
func TestMonitorZeroInputWarnsOnce(t *testing.T) {
	m := NewMonitor()
	buf := make([]float32, 1600) // 100ms
	var warns int
	var lastSilent int64
	fedMs := 0
	for fedMs < 12000 {
		_, noAudio := m.Feed(buf, 0)
		if noAudio {
			warns++
			lastSilent = m.SilentMs()
		}
		fedMs += 100
	}
	if warns != 1 {
		t.Fatalf("12 秒静音应只告警 1 次，实际 %d 次", warns)
	}
	if lastSilent < SilenceWarnMs {
		t.Fatalf("告警时静音时长 %dms，应 ≥ %dms", lastSilent, SilenceWarnMs)
	}
	if m.MaxPeak() != 0 {
		t.Fatalf("全零输入的峰值应为 0，实际 %v", m.MaxPeak())
	}
}

// TestMonitorRearmsAfterAudio 断言恢复有音频后静音计时归零、告警可再次触发。
func TestMonitorRearmsAfterAudio(t *testing.T) {
	m := NewMonitor()
	zeros := make([]float32, 1600)
	loud := make([]float32, 1600)
	for i := range loud {
		loud[i] = 0.3
	}

	var warns int
	for i := 0; i < 110; i++ { // 11 秒静音 → 第 1 次告警
		if _, noAudio := m.Feed(zeros, 0); noAudio {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("首次应告警 1 次，实际 %d", warns)
	}
	if m.SilentMs() < SilenceWarnMs {
		t.Fatalf("静音时长应 ≥ %dms，实际 %d", SilenceWarnMs, m.SilentMs())
	}

	// 有音频 → 静音计时归零。
	m.Feed(loud, 0)
	if m.SilentMs() != 0 {
		t.Fatalf("恢复音频后静音时长应归零，实际 %d", m.SilentMs())
	}
	if m.NoAudioWarned() {
		t.Fatal("恢复音频后告警应重新武装")
	}

	// 再静音 11 秒 → 再告警一次。
	for i := 0; i < 110; i++ {
		if _, noAudio := m.Feed(zeros, 0); noAudio {
			warns++
		}
	}
	if warns != 2 {
		t.Fatalf("第二次静音应再告警一次，累计 %d", warns)
	}
}

// TestMonitorSilenceThresholdIsNumeric 断言 -60dBFS 以下算静音、以上不算。
func TestMonitorSilenceThresholdIsNumeric(t *testing.T) {
	below := make([]float32, 1600)
	for i := range below {
		below[i] = 0.0005 // -66dBFS
	}
	m := NewMonitor()
	m.Feed(below, 0)
	if m.SilentMs() == 0 {
		t.Fatal("低于阈值的信号应计入静音")
	}

	above := make([]float32, 1600)
	for i := range above {
		above[i] = 0.01 // -40dBFS
	}
	m2 := NewMonitor()
	m2.Feed(above, 0)
	if m2.SilentMs() != 0 {
		t.Fatalf("高于阈值的信号不应计入静音，实际 %dms", m2.SilentMs())
	}
}
