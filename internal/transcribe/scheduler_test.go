package transcribe

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
)

// newTestScheduler 构造一个轮询很短的调度器（测试要快，但不能 0）。
func newTestScheduler(t *testing.T, runner *Runner) *Scheduler {
	t.Helper()
	s, err := NewScheduler(runner, SchedulerOptions{Poll: 2 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// waitFor 等待条件成立（带超时，避免测试挂死）。
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("超时等待：%s", msg)
}

// TestSchedulerRejectsSameSessionTwice 同会话并发发起两个任务 → 第二个被拒。
func TestSchedulerRejectsSameSessionTwice(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	eng.onCall = func(call int, _ asr.AudioSegment) {
		if call == 1 {
			once.Do(func() { close(started) })
			<-release
		}
	}
	runner := mustRunner(t, st, eng, openerFor(path), Options{})
	sched := newTestScheduler(t, runner)

	ctx := context.Background()
	if _, err := sched.Enqueue(ctx, TaskRequest{SessionID: st.session.ID, Trigger: "manual"}); err != nil {
		t.Fatalf("第一次入队失败：%v", err)
	}
	// 等它真的开始跑（此时状态 running）。
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("任务未开始执行")
	}

	_, err := sched.Enqueue(ctx, TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("第二次入队错误 = %v, want ERR_STATE_FORBIDDEN", err)
	}
	close(release)
	waitFor(t, 2*time.Second, func() bool { return sched.Settled() }, "任务应跑完")
}

// TestSchedulerRejectsWhileQueued 排队阶段（还没开始跑）也必须拒绝同会话重复发起。
func TestSchedulerRejectsWhileQueued(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()
	// 前台一直忙：任务只能排队、不会开始 → 稳定复现「queued 期间重复发起」。
	busy := true
	runner := mustRunner(t, st, eng, openerFor(path), Options{Idle: func() bool { return !busy }})
	sched := newTestScheduler(t, runner)

	ctx := context.Background()
	if _, err := sched.Enqueue(ctx, TaskRequest{SessionID: st.session.ID}); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	waitFor(t, time.Second, func() bool { return sched.Settled() }, "应停在排队态")
	if got := len(sched.Queued()); got != 1 {
		t.Fatalf("队列长度 = %d, want 1", got)
	}
	_, err := sched.Enqueue(ctx, TaskRequest{SessionID: st.session.ID})
	if !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("重复发起错误 = %v, want ERR_STATE_FORBIDDEN", err)
	}

	session, _ := st.snapshot()
	if session.TranscriptionStatus != StatusQueued {
		t.Errorf("状态 = %q, want queued（前台忙，只排队不执行）", session.TranscriptionStatus)
	}
	busy = false
}

// TestSchedulerConcurrencyLimitOne 并发上限 1：两个会话不会同时跑。
func TestSchedulerConcurrencyLimitOne(t *testing.T) {
	const sr = 16000
	dir := t.TempDir()

	// 两个独立会话（同一段音频，不同 sessionId）。
	// 静音必须 ≥ SilenceMs(500ms) 才会切句，否则两段语音会合并成一句。
	audio := concat(tone(600, sr), silence(600, sr), tone(600, sr))
	path := writeWAV(t, dir, audio, sr, false)
	totalMs := int64(len(audio) * 1000 / sr)
	st1 := newFakeStore(SessionRow{ID: "s_a", AudioPath: "a.wav", DurationMs: totalMs})
	st2 := newFakeStore(SessionRow{ID: "s_b", AudioPath: "a.wav", DurationMs: totalMs})

	// 多存储路由：Store 侧按 sessionID 分派（模拟同一张表里的两条会话）。
	stores := map[string]*fakeStore{"s_a": st1, "s_b": st2}
	multi := &multiStore{stores: stores}
	eng := &overlapEngine{id: "fake@1"}
	runner := mustRunner(t, multi, eng, openerFor(path), Options{})
	sched := newTestScheduler(t, runner)

	ctx := context.Background()
	if _, err := sched.Enqueue(ctx, TaskRequest{SessionID: "s_a"}); err != nil {
		t.Fatalf("入队 A 失败：%v", err)
	}
	if _, err := sched.Enqueue(ctx, TaskRequest{SessionID: "s_b"}); err != nil {
		t.Fatalf("入队 B 失败：%v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return sched.Settled() }, "两个任务都应完成")

	if got := eng.maxConcurrent(); got != 1 {
		t.Fatalf("最大并发 = %d, want 1（并发上限为 1）", got)
	}
	for id, fs := range stores {
		session, segs := fs.snapshot()
		if session.TranscriptionStatus != StatusDone {
			t.Errorf("%s 状态 = %q, want done", id, session.TranscriptionStatus)
		}
		if len(segs) != 2 {
			t.Errorf("%s 落库 %d 句, want 2", id, len(segs))
		}
	}
}

// TestSchedulerYieldThenResumeWhenIdle 抢占让位：前台忙时回 queued 并保留进度，
// 空闲后自动续跑且只处理剩余句子。
func TestSchedulerYieldThenResumeWhenIdle(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	var mu sync.Mutex
	busy := false
	setBusy := func(v bool) {
		mu.Lock()
		busy = v
		mu.Unlock()
	}
	idle := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !busy
	}

	eng := newFakeEngine()
	eng.onCall = func(call int, _ asr.AudioSegment) {
		if call == 1 {
			setBusy(true) // 第 1 句之后前台开始忙
		}
	}
	runner := mustRunner(t, st, eng, openerFor(path), Options{Idle: idle})
	sched := newTestScheduler(t, runner)

	ctx := context.Background()
	if _, err := sched.Enqueue(ctx, TaskRequest{SessionID: st.session.ID, Trigger: "idle"}); err != nil {
		t.Fatalf("入队失败：%v", err)
	}

	// 让位后：停在 queued、进度保留、不再执行。
	waitFor(t, 3*time.Second, func() bool { return sched.Settled() }, "应让位并停下")
	session, segs := st.snapshot()
	if session.TranscriptionStatus != StatusQueued {
		t.Fatalf("让位后状态 = %q, want queued", session.TranscriptionStatus)
	}
	if len(segs) != 1 {
		t.Fatalf("让位后保留 %d 句, want 1", len(segs))
	}
	if session.TranscribeProgressMs != segs[0].EndMs {
		t.Fatalf("让位后进度 = %d, want %d（必须保留）",
			session.TranscribeProgressMs, segs[0].EndMs)
	}
	if eng.Calls() != 1 {
		t.Fatalf("让位前引擎调用 %d 次, want 1", eng.Calls())
	}
	if got := len(sched.Queued()); got != 1 {
		t.Fatalf("让位任务应留在队列里：%d", got)
	}

	// 前台空闲 → 自动续跑。
	eng.resetCalls()
	eng.onCall = nil
	setBusy(false)
	sched.signal()

	waitFor(t, 3*time.Second, func() bool {
		s, _ := st.snapshot()
		return s.TranscriptionStatus == StatusDone
	}, "空闲后应自动续跑到 done")

	if eng.Calls() != 1 {
		t.Fatalf("续跑引擎调用 %d 次, want 1（= 剩余句数）", eng.Calls())
	}
	_, segs2 := st.snapshot()
	if len(segs2) != 2 {
		t.Fatalf("续跑后落库 %d 句, want 2", len(segs2))
	}
}

// TestSchedulerCancelQueued 取消排队中的任务：状态回 none，队列清空。
func TestSchedulerCancelQueued(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()
	busy := true
	runner := mustRunner(t, st, eng, openerFor(path), Options{Idle: func() bool { return !busy }})
	sched := newTestScheduler(t, runner)
	ctx := context.Background()

	if _, err := sched.Enqueue(ctx, TaskRequest{SessionID: st.session.ID}); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	waitFor(t, time.Second, func() bool { return sched.Settled() }, "应停在排队态")

	ok, err := sched.Cancel(ctx, st.session.ID)
	if err != nil || !ok {
		t.Fatalf("取消排队任务失败：ok=%v err=%v", ok, err)
	}
	session, _ := st.snapshot()
	if session.TranscriptionStatus != StatusNone {
		t.Errorf("取消后状态 = %q, want none", session.TranscriptionStatus)
	}
	if got := len(sched.Queued()); got != 0 {
		t.Errorf("队列未清空：%d", got)
	}
	if eng.Calls() != 0 {
		t.Errorf("取消后不应调用引擎：%d", eng.Calls())
	}
	busy = false
}

// TestSchedulerCancelStaleQueued 清理残留：库里是 queued 但内存里没有任务（重启场景）。
func TestSchedulerCancelStaleQueued(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	if err := st.UpdateTranscription(context.Background(), TranscriptionUpdate{
		SessionID: st.session.ID, Status: StatusQueued,
	}); err != nil {
		t.Fatalf("预置状态失败：%v", err)
	}
	runner := mustRunner(t, st, newFakeEngine(), openerFor(path), Options{})
	sched := newTestScheduler(t, runner)
	ctx := context.Background()

	ok, err := sched.Cancel(ctx, st.session.ID)
	if err != nil || !ok {
		t.Fatalf("残留状态应可取消：ok=%v err=%v", ok, err)
	}
	session, _ := st.snapshot()
	if session.TranscriptionStatus != StatusNone {
		t.Errorf("残留状态清理后 = %q, want none", session.TranscriptionStatus)
	}
	// 清理后应能正常发起。
	if _, err := sched.Enqueue(ctx, TaskRequest{SessionID: st.session.ID}); err != nil {
		t.Fatalf("清理后入队失败：%v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return sched.Settled() }, "应能跑完")
}

// —— 辅助：按会话分派的 Store 与并发观测引擎 ——

// multiStore 把多个 fakeStore 组合成一个 Store（用来测「多会话但并发上限 1」）。
type multiStore struct {
	stores map[string]*fakeStore
}

func (m *multiStore) pick(sessionID string) *fakeStore { return m.stores[sessionID] }

func (m *multiStore) LoadSession(ctx context.Context, sessionID string) (SessionRow, error) {
	s := m.pick(sessionID)
	if s == nil {
		return SessionRow{}, apierr.New(apierr.CodeNotFound)
	}
	return s.LoadSession(ctx, sessionID)
}

func (m *multiStore) UpdateTranscription(ctx context.Context, u TranscriptionUpdate) error {
	s := m.pick(u.SessionID)
	if s == nil {
		return apierr.New(apierr.CodeNotFound)
	}
	return s.UpdateTranscription(ctx, u)
}

func (m *multiStore) UpsertSegment(ctx context.Context, row SegmentRow) (SegmentRow, error) {
	s := m.pick(row.SessionID)
	if s == nil {
		return SegmentRow{}, apierr.New(apierr.CodeNotFound)
	}
	return s.UpsertSegment(ctx, row)
}

func (m *multiStore) ListSegments(ctx context.Context, f SegmentFilter) (SegmentPage, error) {
	if f.SessionID != "" {
		s := m.pick(f.SessionID)
		if s == nil {
			return SegmentPage{}, apierr.New(apierr.CodeNotFound)
		}
		return s.ListSegments(ctx, f)
	}
	var all []SegmentRow
	total := 0
	for _, s := range m.stores {
		page, err := s.ListSegments(ctx, f)
		if err != nil {
			return SegmentPage{}, err
		}
		total += page.Total
		all = append(all, page.Rows...)
	}
	return SegmentPage{Total: total, Rows: all}, nil
}

func (m *multiStore) ListMarkers(ctx context.Context, sessionID string) ([]MarkerRow, error) {
	s := m.pick(sessionID)
	if s == nil {
		return nil, apierr.New(apierr.CodeNotFound)
	}
	return s.ListMarkers(ctx, sessionID)
}

func (m *multiStore) UpdateSegmentText(ctx context.Context, segmentID int64, text string) error {
	for _, s := range m.stores {
		if err := s.UpdateSegmentText(ctx, segmentID, text); err == nil {
			return nil
		}
	}
	return apierr.New(apierr.CodeNotFound)
}

func (m *multiStore) MaxSeq(ctx context.Context, sessionID string) (int, error) {
	s := m.pick(sessionID)
	if s == nil {
		return -1, apierr.New(apierr.CodeNotFound)
	}
	return s.MaxSeq(ctx, sessionID)
}

// overlapEngine 记录「同时在跑的 Finalize 数量」，用于验证并发上限 1。
type overlapEngine struct {
	id      string
	mu      sync.Mutex
	active  int
	maxSeen int
}

func (o *overlapEngine) ID() string { return o.id }

func (o *overlapEngine) Finalize(seg asr.AudioSegment) (asr.FinalResult, error) {
	o.mu.Lock()
	o.active++
	if o.active > o.maxSeen {
		o.maxSeen = o.active
	}
	o.mu.Unlock()

	// 故意逗留一下，让并发（如果有）有机会重叠。
	time.Sleep(2 * time.Millisecond)

	o.mu.Lock()
	o.active--
	o.mu.Unlock()
	return asr.FinalResult{Text: fmt.Sprintf("句%d", seg.Seq), EngineID: o.id, Confidence: 0.8}, nil
}

func (o *overlapEngine) maxConcurrent() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.maxSeen
}
