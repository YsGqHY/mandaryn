package transcribe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
)

// mustRunner 构造执行器（默认永不抢占）。
func mustRunner(t *testing.T, st Store, eng Recognizer, open AudioOpener, opts Options) *Runner {
	t.Helper()
	r, err := NewRunner(st, eng, open, opts)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// TestRunWritesSegments 逐句转写：seq / startMs / endMs / text 与引擎调用次数。
func TestRunWritesSegments(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()
	runner := mustRunner(t, st, eng, openerFor(path), Options{})

	res, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeDone {
		t.Fatalf("outcome = %q, want %q", res.Outcome, OutcomeDone)
	}
	if eng.Calls() != 2 {
		t.Fatalf("引擎被调用 %d 次, want 2", eng.Calls())
	}

	session, segs := st.snapshot()
	if len(segs) != 2 {
		t.Fatalf("落库 %d 句, want 2", len(segs))
	}
	if segs[0].Seq != 0 || segs[1].Seq != 1 {
		t.Errorf("seq = %d,%d, want 0,1", segs[0].Seq, segs[1].Seq)
	}
	if segs[0].Text != "句0" || segs[1].Text != "句1" {
		t.Errorf("text = %q,%q, want 句0,句1", segs[0].Text, segs[1].Text)
	}
	if segs[0].StartMs > 50 || segs[1].StartMs < 1400 {
		t.Errorf("时间轴异常：%+v", segs)
	}
	if segs[0].EndMs > segs[1].StartMs {
		t.Errorf("句子区间重叠：%+v", segs)
	}
	if !segs[0].IsFinal || segs[0].EngineID == "" {
		t.Errorf("片段应为定稿且带 engineId：%+v", segs[0])
	}

	// 转写轴收尾：done + 进度到音频末尾 + 计数回填。
	if session.TranscriptionStatus != StatusDone {
		t.Errorf("transcriptionStatus = %q, want %q", session.TranscriptionStatus, StatusDone)
	}
	if session.TranscribeProgressMs != session.DurationMs {
		t.Errorf("progressMs = %d, want %d", session.TranscribeProgressMs, session.DurationMs)
	}
	if session.SegmentCount != 2 {
		t.Errorf("segmentCount = %d, want 2", session.SegmentCount)
	}
	if session.TranscribedAt == 0 || session.TranscribeEngineID == "" {
		t.Errorf("收尾未回填：%+v", session)
	}
	if res.State.Status != StatusDone || res.State.ProgressMs != session.DurationMs {
		t.Errorf("结果状态异常：%+v", res.State)
	}
}

// TestRunIsIdempotentOnRerun 同会话转两次：片段不翻倍，按 seq 覆盖。
func TestRunIsIdempotentOnRerun(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()
	runner := mustRunner(t, st, eng, openerFor(path), Options{})
	ctx := context.Background()

	if _, err := runner.Run(ctx, TaskRequest{SessionID: st.session.ID, Trigger: "manual"}); err != nil {
		t.Fatalf("第一次 Run: %v", err)
	}
	_, first := st.snapshot()
	if len(first) != 2 {
		t.Fatalf("第一次落库 %d 句, want 2", len(first))
	}
	firstIDs := []int64{first[0].ID, first[1].ID}

	// 第二次：引擎换成不同文本，验证是「覆盖」而不是「新增」。
	eng2 := newFakeEngine()
	eng2.text = func(seg asr.AudioSegment) string {
		return fmt.Sprintf("重转后的句%d", seg.Seq)
	}
	runner2 := mustRunner(t, st, eng2, openerFor(path), Options{})
	res, err := runner2.Run(ctx, TaskRequest{SessionID: st.session.ID, Trigger: "manual", Restart: true})
	if err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	if res.Outcome != OutcomeDone {
		t.Fatalf("第二次 outcome = %q, want done", res.Outcome)
	}
	if eng2.Calls() != 2 {
		t.Fatalf("重转时引擎被调用 %d 次, want 2（Restart 应清进度，不能一句都不转）", eng2.Calls())
	}

	_, second := st.snapshot()
	if len(second) != 2 {
		t.Fatalf("重转后落库 %d 句, want 2（幂等覆盖，不得翻倍）", len(second))
	}
	if second[0].ID != firstIDs[0] || second[1].ID != firstIDs[1] {
		t.Errorf("重转应覆盖同一行：before=%v after=[%d,%d]",
			firstIDs, second[0].ID, second[1].ID)
	}
	if second[0].Text != "重转后的句0" || second[1].Text != "重转后的句1" {
		t.Errorf("文本未被覆盖：%q,%q", second[0].Text, second[1].Text)
	}
}

// TestRunResumesFromBreakpoint 断点续跑：跑到第 1 句后取消，重启只处理剩余句。
func TestRunResumesFromBreakpoint(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	eng := newFakeEngine()
	// 第 1 句转完后立刻取消（模拟用户点「取消」）。
	eng.onCall = func(call int, _ asr.AudioSegment) {
		if call == 1 {
			cancel1()
		}
	}
	runner := mustRunner(t, st, eng, openerFor(path), Options{})

	res, err := runner.Run(ctx1, TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("第一次 Run: %v", err)
	}
	if res.Outcome != OutcomeCanceled {
		t.Fatalf("第一次 outcome = %q, want %q", res.Outcome, OutcomeCanceled)
	}
	if eng.Calls() != 1 {
		t.Fatalf("取消前引擎调用 %d 次, want 1", eng.Calls())
	}

	session, segs := st.snapshot()
	if len(segs) != 1 {
		t.Fatalf("取消后保留 %d 句, want 1（已转句子必须保留）", len(segs))
	}
	if session.TranscriptionStatus != StatusNone {
		t.Errorf("取消后状态 = %q, want none（API-4.6）", session.TranscriptionStatus)
	}
	if session.TranscribeProgressMs != segs[0].EndMs {
		t.Errorf("取消后进度 = %d, want %d（保留断点）",
			session.TranscribeProgressMs, segs[0].EndMs)
	}

	// 续跑：引擎计数清零，只应处理第 2 句。
	eng.resetCalls()
	eng.onCall = nil
	res2, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("续跑 Run: %v", err)
	}
	if res2.Outcome != OutcomeDone {
		t.Fatalf("续跑 outcome = %q, want done", res2.Outcome)
	}
	if eng.Calls() != 1 {
		t.Fatalf("续跑引擎被调用 %d 次, want 1（= 剩余句数，已完成句子不得重转）", eng.Calls())
	}
	if res2.Skipped != 1 {
		t.Errorf("续跑跳过 %d 句, want 1", res2.Skipped)
	}
	_, segs2 := st.snapshot()
	if len(segs2) != 2 {
		t.Fatalf("续跑后落库 %d 句, want 2", len(segs2))
	}
	if segs2[1].Seq != 1 {
		t.Errorf("续跑新句 seq = %d, want 1", segs2[1].Seq)
	}
}

// TestRunRejectsSecondTaskOnSameSession 并发限制：同会话第二个任务被拒。
func TestRunRejectsSecondTaskOnSameSession(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	eng.onCall = func(call int, _ asr.AudioSegment) {
		if call == 1 {
			once.Do(func() { close(started) })
			<-release // 挡住第一句，让第二个请求在这期间进来
		}
	}
	runner := mustRunner(t, st, eng, openerFor(path), Options{})

	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
		done <- err
	}()
	<-started

	_, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if err == nil {
		t.Fatal("第二个并发任务应当被拒绝")
	}
	if !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("错误码 = %v, want ERR_STATE_FORBIDDEN", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("第一个任务应正常完成：%v", err)
	}
	if runner.Lock().HeldCount() != 0 {
		t.Errorf("会话锁未释放：%d", runner.Lock().HeldCount())
	}
}

// TestRunYieldsPreservingProgress 抢占让位：idle=false 时回 queued 且进度保留。
func TestRunYieldsPreservingProgress(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()

	var busy sync.Mutex
	frontBusy := false
	setBusy := func(v bool) {
		busy.Lock()
		frontBusy = v
		busy.Unlock()
	}
	idle := func() bool {
		busy.Lock()
		defer busy.Unlock()
		return !frontBusy
	}
	// 第 1 句转完后前台开始忙 → 下一次句间检查点应让位。
	eng.onCall = func(call int, _ asr.AudioSegment) {
		if call == 1 {
			setBusy(true)
		}
	}
	runner := mustRunner(t, st, eng, openerFor(path), Options{Idle: idle})

	res, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "idle"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeYielded {
		t.Fatalf("outcome = %q, want %q", res.Outcome, OutcomeYielded)
	}
	if eng.Calls() != 1 {
		t.Fatalf("让位前引擎调用 %d 次, want 1", eng.Calls())
	}
	session, segs := st.snapshot()
	if session.TranscriptionStatus != StatusQueued {
		t.Errorf("让位后状态 = %q, want queued", session.TranscriptionStatus)
	}
	if session.TranscribeProgressMs == 0 || session.TranscribeProgressMs != segs[0].EndMs {
		t.Errorf("让位后进度 = %d, want %d（必须保留）",
			session.TranscribeProgressMs, segs[0].EndMs)
	}
	if res.State.ErrorCode != "" {
		t.Errorf("让位不是错误，不应带错误码：%q", res.State.ErrorCode)
	}

	// 前台空闲后重跑：只做剩余句。
	setBusy(false)
	eng.resetCalls()
	eng.onCall = nil
	res2, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "idle"})
	if err != nil {
		t.Fatalf("恢复后 Run: %v", err)
	}
	if res2.Outcome != OutcomeDone || eng.Calls() != 1 {
		t.Fatalf("恢复后 outcome = %q, calls = %d, want done / 1", res2.Outcome, eng.Calls())
	}
}

// TestRunNeverTouchesRecordingState 状态正交：任何转写状态变化都不改 sessions.state。
func TestRunNeverTouchesRecordingState(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, st *fakeStore, path string) (*Runner, TaskRequest, context.Context)
	}{
		{
			name: "完成",
			setup: func(t *testing.T, st *fakeStore, path string) (*Runner, TaskRequest, context.Context) {
				return mustRunner(t, st, newFakeEngine(), openerFor(path), Options{}),
					TaskRequest{SessionID: st.session.ID}, context.Background()
			},
		},
		{
			name: "取消",
			setup: func(t *testing.T, st *fakeStore, path string) (*Runner, TaskRequest, context.Context) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return mustRunner(t, st, newFakeEngine(), openerFor(path), Options{}),
					TaskRequest{SessionID: st.session.ID}, ctx
			},
		},
		{
			name: "引擎失败",
			setup: func(t *testing.T, st *fakeStore, path string) (*Runner, TaskRequest, context.Context) {
				eng := newFakeEngine()
				eng.err = func(_ asr.AudioSegment) error {
					return apierr.New(apierr.CodeEngineLoad)
				}
				return mustRunner(t, st, eng, openerFor(path), Options{}),
					TaskRequest{SessionID: st.session.ID}, context.Background()
			},
		},
		{
			name: "音频缺失",
			setup: func(t *testing.T, st *fakeStore, _ string) (*Runner, TaskRequest, context.Context) {
				open := func(_ context.Context, _ SessionRow) (AudioSource, error) {
					return nil, apierr.New(apierr.CodeNotFound)
				}
				return mustRunner(t, st, newFakeEngine(), open, Options{}),
					TaskRequest{SessionID: st.session.ID}, context.Background()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, path, _ := twoSentenceStore(t)
			st.setState("completed") // ST-06：录音已封存
			runner, req, ctx := tc.setup(t, st, path)
			res, _ := runner.Run(ctx, req) // 结果不重要，只看状态正交

			session, _ := st.snapshot()
			if session.State != "completed" {
				t.Fatalf("转写改了录制主状态 state=%q（必须保持 completed）", session.State)
			}
			// 转写轴取值必须属于本轴集合。
			if !ensureNotRecordingState(session.TranscriptionStatus) {
				t.Fatalf("转写状态越界：%q", session.TranscriptionStatus)
			}
			if res.State.Status != session.TranscriptionStatus &&
				res.State.Status != "" && session.TranscriptionStatus != "" {
				// 允许「执行中 running → 结束后 done」这类时序差，但不允许越界值。
				if !ensureNotRecordingState(res.State.Status) {
					t.Fatalf("结果状态越界：%q", res.State.Status)
				}
			}
		})
	}
}

// TestRunFailureKeepsAudioAndSegments 失败不改音频、不删已转片段、带错误码。
func TestRunFailureKeepsAudioAndSegments(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	eng := newFakeEngine()
	// 第 2 句失败。
	eng.err = func(seg asr.AudioSegment) error {
		if seg.Seq == 1 {
			return apierr.New(apierr.CodeEngineLoad)
		}
		return nil
	}
	runner := mustRunner(t, st, eng, openerFor(path), Options{})

	res, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if res.State.ErrorCode != apierr.CodeEngineLoad {
		t.Errorf("errorCode = %q, want %q", res.State.ErrorCode, apierr.CodeEngineLoad)
	}

	session, segs := st.snapshot()
	if session.TranscriptionStatus != StatusFailed {
		t.Errorf("状态 = %q, want failed", session.TranscriptionStatus)
	}
	if len(segs) != 1 {
		t.Errorf("失败后已转片段被改动：%d 句, want 1", len(segs))
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("失败后音频不可访问：%v", err)
	}
	if after.Size() != before.Size() {
		t.Errorf("失败后音频被改写：%d → %d 字节", before.Size(), after.Size())
	}
}

// TestRunSegmentsStayOrdered 断言时间轴不变量（seq 升序、startMs 单调不减）。
func TestRunSegmentsStayOrdered(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	runner := mustRunner(t, st, newFakeEngine(), openerFor(path), Options{})
	if _, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, segs := st.snapshot()
	var prev *SegmentRow
	for i := range segs {
		row := segs[i]
		if err := assertSegmentOrder(prev, row); err != nil {
			t.Fatalf("时间轴不变量被破坏：%v", err)
		}
		prev = &row
	}
}

// TestRunMissingSession 会话不存在 → ERR_NOT_FOUND。
func TestRunMissingSession(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	runner := mustRunner(t, st, newFakeEngine(), openerFor(path), Options{})
	_, err := runner.Run(context.Background(), TaskRequest{SessionID: "s_nope"})
	if !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("错误 = %v, want ERR_NOT_FOUND", err)
	}
}

// TestCanStartMatrix 状态轴准入矩阵。
func TestCanStartMatrix(t *testing.T) {
	cases := map[string]bool{
		StatusNone:   true,
		StatusFailed: true,
		StatusDone:   true,
		StatusQueued: false,
		StatusRunning: false,
		"":           true, // 老库兼容：空串视为 none
	}
	for status, want := range cases {
		if got := CanStart(status); got != want {
			t.Errorf("CanStart(%q) = %v, want %v", status, got, want)
		}
	}
	if !ShouldRestart(StatusDone) {
		t.Error("done 状态应等价重转")
	}
	if ShouldRestart(StatusNone) || ShouldRestart(StatusFailed) {
		t.Error("none/failed 不应触发整段重转")
	}
	if cancelState() != StatusNone {
		t.Errorf("取消后状态应为 none（API-4.6），实际 %q", cancelState())
	}
}

// TestSegmentOrderAssertion 直接验证不变量断言本身。
func TestSegmentOrderAssertion(t *testing.T) {
	prev := &SegmentRow{Seq: 0, StartMs: 0, EndMs: 1000}
	if err := assertSegmentOrder(prev, SegmentRow{Seq: 1, StartMs: 1000, EndMs: 2000}); err != nil {
		t.Fatalf("合法顺序被判错：%v", err)
	}
	// startMs 真的回退（比上一句起点更早）才该被拒绝。
	prev2 := &SegmentRow{Seq: 0, StartMs: 1000, EndMs: 2000}
	if err := assertSegmentOrder(prev2, SegmentRow{Seq: 1, StartMs: 500, EndMs: 600}); err == nil {
		t.Error("startMs 回退应被拒绝")
	}
	if err := assertSegmentOrder(prev, SegmentRow{Seq: 0, StartMs: 1000, EndMs: 2000}); err == nil {
		t.Error("seq 未递增应被拒绝")
	}
	if err := assertSegmentOrder(nil, SegmentRow{Seq: 0, StartMs: 900, EndMs: 800}); err == nil {
		t.Error("startMs > endMs 应被拒绝")
	}
}

// TestPlanResumeSemantics 断点续跑判定语义。
func TestPlanResumeSemantics(t *testing.T) {
	session := SessionRow{TranscribeProgressMs: 1000}
	plan := PlanResume(session, 0, false)
	if !plan.Completed(Utterance{EndMs: 1000}) {
		t.Error("EndMs == progress 应视为已完成")
	}
	if plan.Completed(Utterance{StartMs: 800, EndMs: 1200}) {
		t.Error("跨越断点的句子必须重转（否则丢断点之后的文字）")
	}
	restart := PlanResume(session, 5, true)
	if restart.ProgressMs != 0 {
		t.Errorf("重转应清进度，实际 %d", restart.ProgressMs)
	}
	if restart.Completed(Utterance{EndMs: 100}) {
		t.Error("重转时不得跳过任何句子")
	}
}

// TestExecutionErrorIsNotSwallowed 确认内部信号错误可被 errors.Is 识别。
func TestExecutionErrorIsNotSwallowed(t *testing.T) {
	if !errors.Is(errYielded, errYielded) || !errors.Is(errCanceled, errCanceled) {
		t.Fatal("哨兵错误不可识别")
	}
	if !strings.Contains(errYielded.Error(), apierr.CodePerfSuspended) {
		t.Errorf("让位信号应带 ERR_PERF_SUSPENDED：%v", errYielded)
	}
}
