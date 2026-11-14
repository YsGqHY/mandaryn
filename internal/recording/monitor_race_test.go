package recording

import (
	"sync"
	"testing"
)

// TestMonitorConcurrentFeedAndRead 守护 E8-06：Feed（落盘 goroutine）与读取
// （L3 查询 / 事件回调，另一 goroutine）必须并发安全。
//
// 曾经 Monitor 没有互斥，而 `Recorder.Metrics()` 在调 `Snapshot` 前就释放了
// `r.mu`，文档里「请通过 Recorder 的互斥拿快照」的约定实际不成立 —— 用
// `-race` 跑这个用例即可复现；加上 Monitor 内部互斥后应干净。
func TestMonitorConcurrentFeedAndRead(t *testing.T) {
	m := NewMonitor()
	const rounds = 200
	batch := make([]float32, 160) // 10ms @16k
	for i := range batch {
		batch[i] = 0.25
	}

	var wg sync.WaitGroup

	// 写：模拟落盘 goroutine 持续喂样本。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			m.Feed(batch, int64(i*len(batch)*2))
		}
	}()

	// 读：模拟状态查询与 4Hz 指标读取，四种读法都覆盖到。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = m.Snapshot(int64(i))
			_ = m.Metrics()
			_ = m.RecordedMs()
			_ = m.RecordedFrames()
			_ = m.MaxPeak()
			_ = m.SilentMs()
			_ = m.NoAudioWarned()
		}
	}()

	wg.Wait()

	// 读完的总帧数必须等于喂进去的样本数（锁没把累加弄丢）。
	if got, want := m.RecordedFrames(), int64(rounds*len(batch)); got != want {
		t.Fatalf("累计帧数 = %d, want %d", got, want)
	}
}

// TestMonitorFeedReportCadenceUnchanged 确认加锁没有改变上报节拍语义。
func TestMonitorFeedReportCadenceUnchanged(t *testing.T) {
	m := NewMonitor()
	batch := make([]float32, 160) // 10ms
	for i := range batch {
		batch[i] = 0.5
	}
	// 250ms 窗口 = 25 个 10ms 批次 → 第 25 次上报。
	reports := 0
	for i := 0; i < 25; i++ {
		if report, _ := m.Feed(batch, 0); report {
			reports++
		}
	}
	if reports != 1 {
		t.Fatalf("250ms 窗口应恰好上报 1 次，实际 %d 次", reports)
	}
}
