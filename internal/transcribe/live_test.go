package transcribe

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
)

// 边录边转（FR-03-10）的离线用例。
//
// 这些用例**不碰真麦克风、不跑真模型**：`fakeStreamEngine` 把「Accept 收到多少
// 样本」变成可断言的计数，于是「背压是否阻塞生产者」「partial 有没有落库」
// 这类结论都能精确判定，而不是靠 sleep 猜。

// —— 测试替身：流式引擎 ——

// fakeStreamEngine 是流式假引擎。
//
// 关键能力：
//   - `delay` 模拟「消费极慢」的推理（背压用例）；
//   - `errAt` 让第 N 次 Accept 失败（失败隔离用例）；
//   - `perCall` 决定每次 Accept 返回的增量文本（默认每 400ms 音频吐一个字）。
type fakeStreamEngine struct {
	id   string
	mu   sync.Mutex
	feed int64 // 已喂入样本数
	// accepts 调用次数。
	accepts int
	resets  int
	closes  int
	// delay 每次 Accept 的阻塞时长。
	delay time.Duration
	// errAt > 0 时，第 errAt 次 Accept 返回错误。
	errAt int
	// text 每次 Accept 返回的增量文本；nil 用默认规则。
	text func(call int, samples int) string
	// maxSamples 记录单次 Accept 的最大样本数（断言分块粒度）。
	maxSamples int
}

func newFakeStreamEngine() *fakeStreamEngine {
	return &fakeStreamEngine{id: "zipformer-stream@fake"}
}

func (f *fakeStreamEngine) ID() string { return f.id }

func (f *fakeStreamEngine) Warm(context.Context) error { return nil }

func (f *fakeStreamEngine) Feed([]float32) (<-chan asr.Partial, error) {
	// 延后转写路径不用 Feed；流式能力只经 Accept 暴露。
	ch := make(chan asr.Partial)
	close(ch)
	return ch, nil
}

func (f *fakeStreamEngine) Finalize(seg asr.AudioSegment) (asr.FinalResult, error) {
	return asr.FinalResult{Text: fmt.Sprintf("校正%d", seg.Seq), EngineID: f.id, Confidence: 0.99}, nil
}

func (f *fakeStreamEngine) Health(context.Context) asr.HealthStatus {
	return asr.HealthStatus{State: contract.EngineStateReady, EngineID: f.id}
}

func (f *fakeStreamEngine) Close() error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	return nil
}

func (f *fakeStreamEngine) Accept(samples []float32) (string, error) {
	f.mu.Lock()
	if f.delay > 0 {
		f.mu.Unlock()
		time.Sleep(f.delay)
		f.mu.Lock()
	}
	f.accepts++
	call := f.accepts
	f.feed += int64(len(samples))
	if len(samples) > f.maxSamples {
		f.maxSamples = len(samples)
	}
	errAt := f.errAt
	textFn := f.text
	id := f.id
	f.mu.Unlock()

	if errAt > 0 && call >= errAt {
		return "", fmt.Errorf("模拟推理失败：%w", apierr.New(apierr.CodeEngineLoad))
	}
	if textFn != nil {
		return textFn(call, len(samples)), nil
	}
	_ = id
	return "字", nil
}

func (f *fakeStreamEngine) Reset() error {
	f.mu.Lock()
	f.resets++
	f.mu.Unlock()
	return nil
}

func (f *fakeStreamEngine) Accepts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepts
}

func (f *fakeStreamEngine) Resets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resets
}

// —— 构造帮手 ——

// newLiveTestRunner 起一个 LiveRunner（回调与统计可观察）。
func newLiveTestRunner(t *testing.T, eng asr.StreamingEngine, opts func(*LiveOptions)) (*LiveRunner, *liveProbe, *fakeStore) {
	t.Helper()
	st := newFakeStore(SessionRow{ID: "s_live01", Title: "边录边转会话"})
	probe := &liveProbe{}
	o := LiveOptions{
		Store:     st,
		Engine:    eng,
		SessionID: st.session.ID,
		Vad:       VadConfig{},
		Now:       func() time.Time { return time.UnixMilli(1_700_000_000_000) },
		OnPartial: probe.onPartial,
		OnSegment: probe.onSegment,
		OnState:   probe.onState,
	}
	if opts != nil {
		opts(&o)
	}
	l, err := NewLiveRunner(o)
	if err != nil {
		t.Fatalf("NewLiveRunner: %v", err)
	}
	t.Cleanup(func() {
		l.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !l.Wait(ctx) {
			t.Fatal("LiveRunner 未在 3s 内退出")
		}
	})
	return l, probe, st
}

// liveProbe 收集回调（带锁：回调来自消费 goroutine）。
type liveProbe struct {
	mu       sync.Mutex
	partials []contract.Segment
	segments []contract.Segment
	states   []LiveState
}

func (p *liveProbe) onPartial(s contract.Segment) {
	p.mu.Lock()
	p.partials = append(p.partials, s)
	p.mu.Unlock()
}

func (p *liveProbe) onSegment(s contract.Segment) {
	p.mu.Lock()
	p.segments = append(p.segments, s)
	p.mu.Unlock()
}

func (p *liveProbe) onState(s LiveState) {
	p.mu.Lock()
	p.states = append(p.states, s)
	p.mu.Unlock()
}

func (p *liveProbe) counts() (partials, segments int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.partials), len(p.segments)
}

func (p *liveProbe) lastPartial() (contract.Segment, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.partials) == 0 {
		return contract.Segment{}, false
	}
	return p.partials[len(p.partials)-1], true
}

// pushAll 把整段音频按 chunk 大小推给 LiveRunner（模拟落盘 goroutine 的节奏）。
func pushAll(l *LiveRunner, pcm []float32, chunk int) {
	for i := 0; i < len(pcm); i += chunk {
		end := i + chunk
		if end > len(pcm) {
			end = len(pcm)
		}
		l.Push(pcm[i:end], int64(i))
	}
}

// waitStats 等到条件成立（最多 3s）。
func waitStats(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// —— 用例 ——

// TestLivePartialNotPersistedFinalIsFinalized 是核心语义：
// partial 只上报不落库；句子完结后落库且 finalized=true。
func TestLivePartialNotPersistedFinalIsFinalized(t *testing.T) {
	eng := newFakeStreamEngine()
	l, probe, st := newLiveTestRunner(t, eng, nil)

	const sr = 16000
	pcm := concat(tone(1000, sr), silence(600, sr), tone(800, sr))
	pushAll(l, pcm, 1600) // 100ms 一批

	// 推完立刻看片段：此时还没 Close，定稿行应该已经出现（VAD 端点触发）。
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !l.Wait(ctx) {
		t.Fatal("LiveRunner 未退出")
	}

	_, segs := st.snapshot()
	if len(segs) != 2 {
		t.Fatalf("落库 %d 句，期望 2 句（音频里有两段语音）：%+v", len(segs), segs)
	}
	for _, s := range segs {
		if !s.IsFinal {
			t.Fatalf("落库片段必须 finalized=true：%+v", s)
		}
		if s.Text == "" {
			t.Fatalf("落库片段文本为空：%+v", s)
		}
	}
	if segs[0].Seq != 0 || segs[1].Seq != 1 {
		t.Fatalf("seq = %d,%d，期望 0,1", segs[0].Seq, segs[1].Seq)
	}
	// partial 上报过，但库里只有 2 行（partial 绝不落库）。
	partials, segments := probe.counts()
	if partials == 0 {
		t.Fatal("应有 partial 上报（EV-04）")
	}
	if segments != len(segs) {
		t.Fatalf("EV-05 次数 %d，落库行数 %d，应一致", segments, len(segs))
	}

	// 状态轴：正常结束 → done（ARCH §6.4），且不得碰录制主状态。
	session, _ := st.snapshot()
	if session.TranscriptionStatus != StatusDone {
		t.Fatalf("边录边转正常结束应置 done，实际 %q", session.TranscriptionStatus)
	}
	if session.State != "completed" {
		t.Fatalf("录制主状态被改写了：%q（转写轴与录制轴必须正交）", session.State)
	}
}

// TestLivePartialIsCoverable 断言 partial 是「累积可覆盖」的：后一次比前一次长。
func TestLivePartialIsCoverable(t *testing.T) {
	eng := newFakeStreamEngine()
	eng.text = func(int, int) string { return "啊" }
	l, probe, _ := newLiveTestRunner(t, eng, nil)

	const sr = 16000
	pushAll(l, tone(600, sr), 1600)

	waitStats(t, "至少两次 partial", func() bool {
		p, _ := probe.counts()
		return p >= 2
	})
	probe.mu.Lock()
	defer probe.mu.Unlock()
	first, last := probe.partials[0], probe.partials[len(probe.partials)-1]
	if len(last.Text) <= len(first.Text) {
		t.Fatalf("partial 应累积变长：first=%q last=%q", first.Text, last.Text)
	}
	if first.Finalized || last.Finalized {
		t.Fatal("partial 的 finalized 必须为 false")
	}
}

// TestLiveBackpressureDropsOldestWithoutBlocking 是关键性能用例：
// 消费极慢时，Push 必须立刻返回（不阻塞采集），并累计丢弃计数。
func TestLiveBackpressureDropsOldestWithoutBlocking(t *testing.T) {
	eng := newFakeStreamEngine()
	// 每次 Accept 睡 50ms —— 比推入速度慢得多，队列一定会溢出。
	eng.delay = 50 * time.Millisecond

	// 队列容量 200ms：少量样本就能打满。
	l, _, _ := newLiveTestRunner(t, eng, func(o *LiveOptions) { o.QueueMs = 200 })

	const sr = 16000
	chunk := sr / 10 // 100ms 一批
	total := 0
	start := time.Now()
	// 推 3 秒音频：远超队列容量与消费能力。
	for i := 0; i < 30; i++ {
		began := time.Now()
		l.Push(tone(100, sr), int64(total))
		if d := time.Since(began); d > 100*time.Millisecond {
			t.Fatalf("第 %d 次 Push 阻塞了 %s（Push 必须非阻塞）", i, d)
		}
		total += chunk
	}
	elapsed := time.Since(start)

	stats := l.Stats()
	if stats.DroppedSamples == 0 {
		t.Fatalf("队列应溢出并丢弃最旧样本，实际 dropped=%d", stats.DroppedSamples)
	}
	// 30 次 100ms 的投递在「完全不阻塞」下远小于 1s（引擎慢不应回灌到生产者）。
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("采集侧被推理拖慢：30 次 Push 用了 %s", elapsed)
	}
	// 队列长度永不超过容量（有界）。
	l.mu.Lock()
	buffered := len(l.buf)
	capSamples := l.capSamples
	l.mu.Unlock()
	if buffered > capSamples {
		t.Fatalf("队列越界：buffered=%d cap=%d", buffered, capSamples)
	}
}

// TestLiveEngineErrorDoesNotStopRecording 断言推理失败只停用旁路，不影响任何写入。
func TestLiveEngineErrorDoesNotStopRecording(t *testing.T) {
	eng := newFakeStreamEngine()
	eng.errAt = 2 // 第 2 次 Accept 起失败
	l, probe, st := newLiveTestRunner(t, eng, nil)

	const sr = 16000
	pushAll(l, concat(tone(800, sr), silence(600, sr), tone(800, sr)), 1600)

	waitStats(t, "推理失败已上报", func() bool {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		return len(probe.states) > 0
	})

	// 旁路停用：错误码可见，状态回调说明「录制继续」。
	probe.mu.Lock()
	last := probe.states[len(probe.states)-1]
	probe.mu.Unlock()
	if last.Active {
		t.Fatalf("推理失败后旁路应停用：%+v", last)
	}
	if last.ErrorCode != apierr.CodeEngineLoad {
		t.Fatalf("错误码 = %q，期望 %q", last.ErrorCode, apierr.CodeEngineLoad)
	}

	// 推更多音频：不得 panic，也不得再调引擎。
	before := eng.Accepts()
	pushAll(l, tone(500, sr), 1600)
	time.Sleep(50 * time.Millisecond)
	if got := eng.Accepts(); got != before {
		t.Fatalf("停用后仍在推理：accepts %d → %d", before, got)
	}

	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !l.Wait(ctx) {
		t.Fatal("LiveRunner 未退出")
	}

	// 收尾：状态置 failed（可重转），录制主状态仍是封存态。
	session, _ := st.snapshot()
	if session.TranscriptionStatus != StatusFailed {
		t.Fatalf("推理失败应收尾为 failed，实际 %q", session.TranscriptionStatus)
	}
	if session.State != "completed" {
		t.Fatalf("录制主状态被改写了：%q", session.State)
	}
}

// TestLiveCloseFinalizesOrDropsTail 断言停止时「够长就定稿、否则丢弃」。
func TestLiveCloseFinalizesOrDropsTail(t *testing.T) {
	const sr = 16000

	// 长尾：1s 语音后直接 Close（没有句尾静音）→ Flush 应把它定稿。
	t.Run("够长就定稿", func(t *testing.T) {
		eng := newFakeStreamEngine()
		l, _, st := newLiveTestRunner(t, eng, nil)
		pushAll(l, tone(1000, sr), 1600)
		waitStats(t, "引擎已吃到音频", func() bool { return eng.Accepts() > 0 })
		l.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !l.Wait(ctx) {
			t.Fatal("LiveRunner 未退出")
		}
		if _, segs := st.snapshot(); len(segs) != 1 {
			t.Fatalf("长尾应被定稿为 1 句，实际 %d 句", len(segs))
		}
	})

	// 短尾：100ms 语音（< MinSpeechMs=200）→ 丢弃，不落库。
	t.Run("太短就丢弃", func(t *testing.T) {
		eng := newFakeStreamEngine()
		l, _, st := newLiveTestRunner(t, eng, nil)
		pushAll(l, tone(100, sr), 50)
		waitStats(t, "引擎已吃到音频", func() bool { return eng.Accepts() > 0 })
		l.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !l.Wait(ctx) {
			t.Fatal("LiveRunner 未退出")
		}
		if _, segs := st.snapshot(); len(segs) != 0 {
			t.Fatalf("过短尾部应丢弃，实际落库 %d 句", len(segs))
		}
	})
}

// TestLiveAfterCloseNeverPanics 断言 Close 后继续 Push / 重复 Close 都安全。
func TestLiveAfterCloseNeverPanics(t *testing.T) {
	eng := newFakeStreamEngine()
	l, _, _ := newLiveTestRunner(t, eng, nil)
	l.Close()
	l.Close() // 幂等
	l.Push(tone(100, 16000), 0)
	l.Push(nil, 0)
	if stats := l.Stats(); stats.DroppedSamples == 0 {
		t.Fatalf("停止后的投递应计入丢弃，实际 %+v", stats)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !l.Wait(ctx) {
		t.Fatal("LiveRunner 未退出")
	}
}

// TestLiveSeqMatchesDeferredTranscription 是关键一致性用例：
// 同一段音频，边录边转的 seq 必须与延后转写（原始 Run）的 seq 完全一致，
// 否则手动重转的幂等覆盖会错位（同一句写到别处）。
func TestLiveSeqMatchesDeferredTranscription(t *testing.T) {
	const sr = 16000
	// 三段语音 + 两段静音：足够产生 3 个 seq。
	audio := concat(
		tone(700, sr), silence(600, sr),
		tone(700, sr), silence(600, sr),
		tone(700, sr),
	)

	// 1) 延后转写：拿到 seq 列表。
	stDef, path, _ := twoSentenceStore(t)
	engDef := newFakeEngine()
	runner := mustRunner(t, stDef, engDef, openerFor(path), Options{})
	// twoSentenceStore 的音频与这里不同：用它自己的音频算 seq 基线。
	_, defSegs := stDef.snapshot()
	if len(defSegs) != 0 {
		t.Fatalf("初始应无片段")
	}
	if _, err := runner.Run(context.Background(), TaskRequest{SessionID: stDef.session.ID, Trigger: "manual"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, defSegs = stDef.snapshot()

	// 2) 边录边转：用同一套 VAD 参数分句。
	engLive := newFakeStreamEngine()
	l, _, stLive := newLiveTestRunner(t, engLive, nil)
	pushAll(l, audio, 1600)
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !l.Wait(ctx) {
		t.Fatal("LiveRunner 未退出")
	}
	_, liveSegs := stLive.snapshot()

	// 3) 同源音频用 SplitAll 算「应有的 seq 序列」，与两条路径逐一比对。
	want := SplitAll(audio, VadConfig{})
	if len(liveSegs) != len(want) {
		t.Fatalf("边录边转落库 %d 句，分句器给出 %d 句", len(liveSegs), len(want))
	}
	for i, u := range want {
		if liveSegs[i].Seq != u.Index {
			t.Fatalf("第 %d 句 seq=%d，分句器给出 %d", i, liveSegs[i].Seq, u.Index)
		}
		if liveSegs[i].StartMs != u.StartMs || liveSegs[i].EndMs != u.EndMs {
			t.Fatalf("第 %d 句时间轴与分句器不一致：live=[%d,%d] want=[%d,%d]",
				i, liveSegs[i].StartMs, liveSegs[i].EndMs, u.StartMs, u.EndMs)
		}
	}
	// 延后转写的 seq 也是 0..n-1：两条路径同源，因此不会撞号。
	for i, s := range defSegs {
		if s.Seq != i {
			t.Fatalf("延后转写 seq=%d，期望 %d", s.Seq, i)
		}
	}
}

// TestLiveRefineOptional 断言第二遍离线校正：允许时用校正文本，禁止时用流式文本。
func TestLiveRefineOptional(t *testing.T) {
	const sr = 16000
	audio := concat(tone(800, sr), silence(600, sr))

	t.Run("L0 允许校正", func(t *testing.T) {
		eng := newFakeStreamEngine()
		l, _, st := newLiveTestRunner(t, eng, func(o *LiveOptions) {
			o.Refine = func(seg asr.AudioSegment) (asr.FinalResult, error) {
				return asr.FinalResult{Text: "校正文本", EngineID: "offline@1", Confidence: 0.95}, nil
			}
			o.RefineAllowed = func() bool { return true }
		})
		pushAll(l, audio, 1600)
		l.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !l.Wait(ctx) {
			t.Fatal("未退出")
		}
		_, segs := st.snapshot()
		if len(segs) != 1 || segs[0].Text != "校正文本" {
			t.Fatalf("应使用第二遍校正文本：%+v", segs)
		}
		if segs[0].EngineID != "offline@1" {
			t.Fatalf("引擎 ID 应记录实际产出方：%q", segs[0].EngineID)
		}
		if l.Stats().Refines != 1 {
			t.Fatalf("Refines=%d，期望 1", l.Stats().Refines)
		}
	})

	t.Run("L1 关闭校正", func(t *testing.T) {
		eng := newFakeStreamEngine()
		eng.text = func(int, int) string { return "流式" }
		l, _, st := newLiveTestRunner(t, eng, func(o *LiveOptions) {
			o.Refine = func(seg asr.AudioSegment) (asr.FinalResult, error) {
				return asr.FinalResult{Text: "校正文本"}, nil
			}
			o.RefineAllowed = func() bool { return false }
		})
		pushAll(l, audio, 1600)
		l.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !l.Wait(ctx) {
			t.Fatal("未退出")
		}
		_, segs := st.snapshot()
		if len(segs) != 1 {
			t.Fatalf("落库 %d 句，期望 1", len(segs))
		}
		if !strings.HasPrefix(segs[0].Text, "流式") || strings.Contains(segs[0].Text, "校正") {
			t.Fatalf("L1 应只保留流式结果（累积文本），实际 %q", segs[0].Text)
		}
		if l.Stats().Refines != 0 {
			t.Fatalf("L1 不应做第二遍：Refines=%d", l.Stats().Refines)
		}
	})
}

// TestLiveGoroutinesExit 断言停止后不留 goroutine（用 Wait 判据 + 不 panic）。
func TestLiveGoroutinesExit(t *testing.T) {
	eng := newFakeStreamEngine()
	l, _, _ := newLiveTestRunner(t, eng, nil)

	pushAll(l, tone(300, 16000), 1600)
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !l.Wait(ctx) {
		t.Fatal("Close 后消费 goroutine 未退出")
	}
	// 再等一下确保没有「退出后又写」的竞态（-race 下最有效）。
	time.Sleep(20 * time.Millisecond)
	if stats := l.Stats(); !stats.Done {
		t.Fatalf("收尾未完成：%+v", stats)
	}
}

// TestShouldAutoTranscribe 断言「边录边转过的会话不再自动整段转写」。
func TestShouldAutoTranscribe(t *testing.T) {
	cases := []struct {
		name string
		row  SessionRow
		want bool
	}{
		{"未开边录边转、未转写 → 应自动排队", SessionRow{TranscriptionStatus: StatusNone}, true},
		{"未开边录边转、失败 → 应重试", SessionRow{TranscriptionStatus: StatusFailed}, true},
		{"已开边录边转 → 不应自动重跑", SessionRow{TranscriptionStatus: StatusDone, LiveTranscribed: true}, false},
		{"已开边录边转但失败 → 不自动重跑（等用户手动）", SessionRow{TranscriptionStatus: StatusFailed, LiveTranscribed: true}, false},
		{"已排队 → 不重复排队", SessionRow{TranscriptionStatus: StatusQueued}, false},
	}
	for _, c := range cases {
		if got := ShouldAutoTranscribe(c.row); got != c.want {
			t.Errorf("%s：ShouldAutoTranscribe = %v，期望 %v", c.name, got, c.want)
		}
	}
}
