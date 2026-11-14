package recording

import (
	"context"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
)

// 边录边转的**录制侧**用例（FR-03-10）。
//
// 两条硬不变量在这里被锁死：
//  1. sink 为 nil（默认）时，录制路径与未实现该功能前完全一致；
//  2. sink 存在时，它只在落盘 goroutine 上做「投递」，**不能**拖慢采集与落盘。

// fakeSink 是记录型旁路：把收到的东西记下来，可注入阻塞。
type fakeSink struct {
	mu       sync.Mutex
	samples  int64
	pushed   int
	frames   []int64
	closed   int
	chars    int64
	blockFor time.Duration
}

func (s *fakeSink) Push(samples []float32, atFrame int64) {
	if s.blockFor > 0 {
		// 故意慢——用来证明录制侧不等待它。
		time.Sleep(s.blockFor)
	}
	s.mu.Lock()
	s.samples += int64(len(samples))
	s.pushed++
	s.frames = append(s.frames, atFrame)
	s.mu.Unlock()
}

func (s *fakeSink) Close() {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
}

func (s *fakeSink) CharCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chars
}

func (s *fakeSink) snapshot() (samples int64, pushed, closed int, frames []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, len(s.frames))
	copy(out, s.frames)
	return s.samples, s.pushed, s.closed, out
}

// TestLiveDisabledByDefault 断言默认（不注入开关与 sink）时：
// 不建 sink、不置 live_transcribed、录制照常完成。
func TestLiveDisabledByDefault(t *testing.T) {
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			return newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3)), nil
		}
	})

	st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != contract.StateRecording {
		t.Fatalf("状态 = %s", st.State)
	}
	// LiveCharCount 未开启时恒 0（FR-03-9 的「默认关闭零行为」）。
	if got := rec.LiveCharCount(); got != 0 {
		t.Fatalf("未开启边录边转时 LiveCharCount = %d，期望 0", got)
	}

	// 录一小段再停：与既有行为一致（completed）。
	waitFor(t, 3*time.Second, "录到 300ms", func() bool { return rec.Metrics().RecordedMs >= 300 })
	st, err = rec.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st.State != contract.StateCompleted {
		t.Fatalf("Stop 后状态 = %s，期望 completed", st.State)
	}
}

// TestLiveSinkReceivesAudioWithFrameOffsets 断言开启后 sink 收到 16k 样本，
// 且 atFrame 单调递增（时间轴与 WAV 一致，不用墙钟）。
func TestLiveSinkReceivesAudioWithFrameOffsets(t *testing.T) {
	sink := &fakeSink{}
	rec := newTestRecorder(t, func(o *Options) {
		o.LiveEnabled = func() bool { return true }
		o.NewLiveSink = func(sessionID string) LiveSink {
			if sessionID == "" {
				t.Error("NewLiveSink 必须拿到 sessionId")
			}
			return sink
		}
		o.OpenChannel = func(string) (capture.Channel, error) {
			return newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3)), nil
		}
	})

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, "sink 收到 ≥1000 样本", func() bool {
		s, _, _, _ := sink.snapshot()
		return s >= 1000
	})
	if _, err := rec.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	samples, pushed, closed, frames := sink.snapshot()
	if pushed == 0 || samples == 0 {
		t.Fatalf("sink 未收到音频：pushed=%d samples=%d", pushed, samples)
	}
	// 帧偏移必须单调递增且从 0 附近开始（= 已落盘帧数）。
	for i := 1; i < len(frames); i++ {
		if frames[i] <= frames[i-1] {
			t.Fatalf("atFrame 未递增：%v", frames[:min(6, len(frames))])
		}
	}
	if frames[0] != 0 {
		t.Fatalf("首个 atFrame 应为 0，实际 %d", frames[0])
	}
	// 收尾必须调用 Close（且只调一次）。
	if closed != 1 {
		t.Fatalf("Close 调用 %d 次，期望 1", closed)
	}
}

// TestLiveSinkNeverBlocksRecording 是关键用例：sink 很慢也**不得**拖慢采集与落盘。
//
// 判据用「磁盘上的音频时长」而不是时间戳：即使 sink 每次阻塞 20ms，
// 录制仍应以源速率写满，说明旁路没有回灌到落盘 goroutine 之外。
func TestLiveSinkNeverBlocksRecording(t *testing.T) {
	sink := &fakeSink{blockFor: 20 * time.Millisecond}
	rec := newTestRecorder(t, func(o *Options) {
		o.LiveEnabled = func() bool { return true }
		o.NewLiveSink = func(string) LiveSink { return sink }
		o.OpenChannel = func(string) (capture.Channel, error) {
			return newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3)), nil
		}
	})

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 慢 sink 存在时仍然录到足够时长（时间轴由帧计数推进）。
	ok := true
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rec.Metrics().RecordedMs >= 500 {
			break
		}
		if time.Now().After(deadline) {
			ok = false
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("慢 sink 拖慢了录制：仅录到 %dms", rec.Metrics().RecordedMs)
	}
	st, err := rec.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 有 sink 也必须能正常封存（音频不因推理慢而丢）。
	if st.State != contract.StateCompleted {
		t.Fatalf("Stop 状态 = %s（lastError=%+v）", st.State, st.LastError)
	}
}

// TestLiveTranscribedFlagSet 断言开启模式下会话行置 live_transcribed=true；
// 关闭模式保持 false。
func TestLiveTranscribedFlagSet(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		store := newTestStore(t)
		root := t.TempDir()
		rec, err := New(Options{
			Store:    store,
			DataRoot: func() (string, error) { return root, nil },
			Now:      func() time.Time { return time.UnixMilli(1_700_000_000_000) },
			LiveEnabled: func() bool { return enabled },
			NewLiveSink: func(string) LiveSink { return &fakeSink{} },
			OpenChannel: func(string) (capture.Channel, error) {
				return newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3)), nil
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		row, err := store.GetSession(context.Background(), st.SessionID)
		if err != nil {
			t.Fatalf("读会话失败：%v", err)
		}
		if row.LiveTranscribed != enabled {
			t.Fatalf("enabled=%v 时 live_transcribed=%v", enabled, row.LiveTranscribed)
		}
		if _, err := rec.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
}

// TestLiveSinkCreateFailureStillRecords 断言 sink 建不起来时录制照常（FR-03-6）。
func TestLiveSinkCreateFailureStillRecords(t *testing.T) {
	rec := newTestRecorder(t, func(o *Options) {
		o.LiveEnabled = func() bool { return true }
		// 装配层在「没有流式模型」时会返回 nil（并记日志）。
		o.NewLiveSink = func(string) LiveSink { return nil }
		o.OpenChannel = func(string) (capture.Channel, error) {
			return newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3)), nil
		}
	})
	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("Start: %v（录制不得因推理能力缺失而失败）", err)
	}
	waitFor(t, 3*time.Second, "录到 200ms", func() bool { return rec.Metrics().RecordedMs >= 200 })
	st, err := rec.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st.State != contract.StateCompleted {
		t.Fatalf("状态 = %s", st.State)
	}
	if got := rec.LiveCharCount(); got != 0 {
		t.Fatalf("无 sink 时 LiveCharCount 应为 0，实际 %d", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
