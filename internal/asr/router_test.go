package asr

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// —— 测试替身 ——

// fakeEngine 是可编程的引擎替身：可指定预热错误、逐次 Finalize 错误序列。
type fakeEngine struct {
	id string

	warmErrs   []error
	finalErrs  []error
	finalText  string
	finalID    string
	healthNote string

	warmCount  atomic.Int32
	finalCount atomic.Int32
	closeCount atomic.Int32

	mu       sync.Mutex
	warmSeen []int
}

func (f *fakeEngine) ID() string { return f.id }

func (f *fakeEngine) Warm(_ context.Context) error {
	n := int(f.warmCount.Add(1))
	f.mu.Lock()
	f.warmSeen = append(f.warmSeen, n)
	f.mu.Unlock()
	if n-1 < len(f.warmErrs) {
		return f.warmErrs[n-1]
	}
	return nil
}

func (f *fakeEngine) Feed(_ []float32) (<-chan Partial, error) {
	ch := make(chan Partial)
	close(ch)
	return ch, nil
}

func (f *fakeEngine) Finalize(_ AudioSegment) (FinalResult, error) {
	n := int(f.finalCount.Add(1))
	f.mu.Lock()
	var err error
	if n-1 < len(f.finalErrs) {
		err = f.finalErrs[n-1]
	}
	f.mu.Unlock()
	if err != nil {
		return FinalResult{}, err
	}
	id := f.finalID
	if id == "" {
		id = f.id
	}
	return FinalResult{Text: f.finalText, EngineID: id}, nil
}

func (f *fakeEngine) Health(_ context.Context) HealthStatus {
	return HealthStatus{State: contract.EngineStateReady, EngineID: f.id, Note: f.healthNote}
}

func (f *fakeEngine) Close() error {
	f.closeCount.Add(1)
	return nil
}

// —— 测试工具 ——

// engineList 记录工厂造出来的所有实例，便于断言「旧引擎被关闭」。
type engineList struct {
	mu    sync.Mutex
	items []*fakeEngine
}

func (l *engineList) add(e *fakeEngine) *fakeEngine {
	l.mu.Lock()
	l.items = append(l.items, e)
	l.mu.Unlock()
	return e
}

func (l *engineList) all() []*fakeEngine {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*fakeEngine, len(l.items))
	copy(out, l.items)
	return out
}

func (l *engineList) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}

// factory 生成「每次调用都新建实例」的工厂（模拟真实热切换）。
func factory(l *engineList, mk func(n int) *fakeEngine) func() (Engine, error) {
	return func() (Engine, error) {
		e := l.add(mk(l.len()))
		return e, nil
	}
}

// sleepRecorder 记录所有退避等待（不真的睡）。
type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRecorder) fn(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return nil
}

func (s *sleepRecorder) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.waits))
	copy(out, s.waits)
	return out
}

// onlineErr 造一个分类错误。
func onlineErr(kind FailureKind, code string, retryAfter time.Duration) error {
	return &EngineError{Kind: kind, Err: apierr.New(code), RetryAfter: retryAfter}
}

func segment() AudioSegment {
	return AudioSegment{Samples: make([]float32, 1600), SampleRate: 16000, Seq: 1}
}

// newRouter 构造带 fake 引擎的 Router；源固定 local，由测试再切。
func newRouter(t *testing.T, local, online func() (Engine, error),
	sleeper *sleepRecorder, policy contract.FallbackPolicy, active func() bool) *Router {

	t.Helper()
	return NewRouter(RouterOptions{
		Source:        contract.ModelSourceLocal,
		Local:         local,
		Online:        online,
		Policy:        policy,
		ActiveSession: active,
		Sleep:         sleeper.fn,
		UnloadDelay:   time.Millisecond,
	})
}

// warmLocalThenSwitchOnline 先预热本地（让降级有本地可用），再切到在线。
func warmLocalThenSwitchOnline(t *testing.T, r *Router) {
	t.Helper()
	ctx := context.Background()
	if err := r.Warm(ctx); err != nil {
		t.Fatalf("预热本地失败: %v", err)
	}
	if err := r.SetSource(contract.ModelSourceOnline); err != nil {
		t.Fatalf("切到在线失败: %v", err)
	}
}

type gatedOnlineEngine struct {
	*fakeEngine
	started chan struct{}
	resume  chan struct{}
}

func (e *gatedOnlineEngine) Finalize(seg AudioSegment) (FinalResult, error) {
	close(e.started)
	<-e.resume
	return e.fakeEngine.Finalize(seg)
}

func TestRouter_OnlineConfigRefreshWithoutProbe(t *testing.T) {
	old := &gatedOnlineEngine{
		fakeEngine: &fakeEngine{id: "online:old", finalErrs: []error{onlineErr(FailAuth, apierr.CodeEngineOnlineAuth, 0)}},
		started:    make(chan struct{}),
		resume:     make(chan struct{}),
	}
	finish := sync.OnceFunc(func() { close(old.resume) })
	defer finish()
	local := &fakeEngine{id: "local", finalText: "fallback"}
	fresh := &fakeEngine{id: "online:new", finalText: "updated"}
	r := newRouter(t,
		func() (Engine, error) { return local, nil },
		func() (Engine, error) { return old, nil },
		&sleepRecorder{}, contract.FallbackPolicy{FallbackToLocal: true}, nil)
	warmLocalThenSwitchOnline(t, r)
	defer r.Close()

	type outcome struct {
		result FinalResult
		err    error
	}
	pending := make(chan outcome, 1)
	go func() {
		result, err := r.Finalize(segment())
		pending <- outcome{result, err}
	}()
	select {
	case <-old.started:
	case <-time.After(3 * time.Second):
		t.Fatal("old engine did not start")
	}

	builds := 0
	r.SetOnlineFactory(func() (Engine, error) {
		builds++
		return fresh, nil
	})
	if builds != 0 || fresh.warmCount.Load() != 0 || old.closeCount.Load() != 0 || r.Source() != contract.ModelSourceOnline {
		t.Fatalf("saving must not build, probe, close in-flight work or change source: builds=%d status=%+v", builds, r.Status())
	}
	result, err := r.Finalize(segment())
	if err != nil || result.Text != "updated" || result.EngineID != "online:new" || builds != 1 || fresh.warmCount.Load() != 0 {
		t.Fatalf("next transcription must use updated configuration without warm: result=%+v err=%v builds=%d", result, err, builds)
	}
	if old.closeCount.Load() != 0 {
		t.Fatal("refresh closed a still-running engine")
	}

	finish()
	select {
	case got := <-pending:
		if got.err != nil || got.result.Text != "fallback" || old.closeCount.Load() != 1 {
			t.Fatalf("in-flight transcription must finish independently, then release old engine: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old transcription did not finish")
	}
	if status := r.Status(); status.Source != contract.ModelSourceOnline || status.EngineID != "online:new" || status.Degraded || status.LastError != nil {
		t.Fatalf("superseded failure modified the new route: %+v", status)
	}
}

func TestRouter_OnlineRefreshDoesNotBypassDegradedRoute(t *testing.T) {
	old := &fakeEngine{id: "online:old", finalErrs: []error{onlineErr(FailAuth, apierr.CodeEngineOnlineAuth, 0)}}
	local := &fakeEngine{id: "local", finalText: "fallback"}
	r := newRouter(t,
		func() (Engine, error) { return local, nil },
		func() (Engine, error) { return old, nil },
		&sleepRecorder{}, contract.FallbackPolicy{FallbackToLocal: true}, nil)
	warmLocalThenSwitchOnline(t, r)
	defer r.Close()

	if _, err := r.Finalize(segment()); err != nil || !r.Status().Degraded {
		t.Fatalf("expected local fallback; err=%v status=%+v", err, r.Status())
	}
	r.SetOnlineFactory(func() (Engine, error) {
		t.Fatal("saving must not re-enable a degraded online engine")
		return nil, nil
	})
	result, err := r.Finalize(segment())
	if err != nil || result.Text != "fallback" || !r.Status().Degraded || r.Source() != contract.ModelSourceOnline {
		t.Fatalf("save changed the degraded route: result=%+v err=%v status=%+v", result, err, r.Status())
	}
}

// —— 1. 在线故障矩阵 ——

func TestRouter_OnlineFailuresDegradeToLocal(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantOnline int32 // 在线引擎被调用次数（含重试）
	}{
		{"网络不可达重试2次", onlineErr(FailNetwork, apierr.CodeEngineOnlineNetwork, 0), 3},
		{"超时重试2次", onlineErr(FailTimeout, apierr.CodeEngineOnlineTimeout, 0), 3},
		{"5xx重试2次", onlineErr(FailServer, apierr.CodeEngineOnlineNetwork, 0), 3},
		{"429重试2次", onlineErr(FailRateLimit, apierr.CodeEngineOnlineRateLimit, 7*time.Second), 3},
		{"401不重试", onlineErr(FailAuth, apierr.CodeEngineOnlineAuth, 0), 1},
		{"内容被拒不重试", onlineErr(FailRejected, apierr.CodeEngineOnlineRejected, 0), 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			locals := &engineList{}
			onlines := &engineList{}
			sleeper := &sleepRecorder{}

			// 在线引擎每次都会失败：错误序列给足 3 次。
			onlineF := factory(onlines, func(n int) *fakeEngine {
				return &fakeEngine{
					id:        "online:m",
					finalErrs: []error{tc.err, tc.err, tc.err},
				}
			})
			localF := factory(locals, func(n int) *fakeEngine {
				return &fakeEngine{id: "local-1", finalText: "本地文本"}
			})

			r := newRouter(t, localF, onlineF, sleeper,
				contract.FallbackPolicy{FallbackToLocal: true, MaxRetries: 2}, nil)
			warmLocalThenSwitchOnline(t, r)

			onlineEng := onlines.all()[0]
			localEng := locals.all()[0]

			res, err := r.Finalize(segment())
			if err != nil {
				t.Fatalf("期望降级成功，实际返回错误: %v", err)
			}
			if res.Text != "本地文本" || res.EngineID != "local-1" {
				t.Fatalf("期望本地结果，实际 %+v", res)
			}
			if got := onlineEng.finalCount.Load(); got != tc.wantOnline {
				t.Fatalf("在线调用次数 = %d, 期望 %d", got, tc.wantOnline)
			}
			if got := localEng.finalCount.Load(); got != 1 {
				t.Fatalf("本地调用次数 = %d, 期望 1", got)
			}

			st := r.Status()
			if !st.Degraded {
				t.Fatalf("期望 Degraded=true，实际 %+v", st)
			}
			if st.State != contract.EngineStateDegraded {
				t.Fatalf("期望 State=degraded，实际 %q", st.State)
			}
			if st.DegradeNote == "" {
				t.Fatal("期望 DegradeNote 非空")
			}
			// 用户选择的来源不该被降级改写。
			if st.Source != contract.ModelSourceOnline {
				t.Fatalf("期望 Source 保持 online，实际 %q", st.Source)
			}

			// 401 / 内容被拒不该做退避等待；可重试类别应有 2 次退避。
			waits := sleeper.recorded()
			retryable := tc.wantOnline > 1
			if retryable && len(waits) != 2 {
				t.Fatalf("退避次数 = %d, 期望 2", len(waits))
			}
			if !retryable && len(waits) != 0 {
				t.Fatalf("不应退避，实际 %v", waits)
			}
		})
	}
}

func TestRouter_RateLimitHonorsRetryAfterCap(t *testing.T) {
	onlines := &engineList{}
	locals := &engineList{}
	sleeper := &sleepRecorder{}

	// Retry-After 60s，超过 30s 上限。
	rateErr := onlineErr(FailRateLimit, apierr.CodeEngineOnlineRateLimit, 60*time.Second)
	onlineF := factory(onlines, func(n int) *fakeEngine {
		return &fakeEngine{id: "online:m", finalErrs: []error{rateErr, rateErr, rateErr}}
	})
	localF := factory(locals, func(n int) *fakeEngine {
		return &fakeEngine{id: "local-1", finalText: "本地"}
	})

	r := NewRouter(RouterOptions{
		Source:      contract.ModelSourceLocal,
		Local:       localF,
		Online:      onlineF,
		Policy:      contract.FallbackPolicy{FallbackToLocal: true, MaxRetries: 2},
		Sleep:       sleeper.fn,
		UnloadDelay: time.Millisecond,
	})
	warmLocalThenSwitchOnline(t, r)

	if _, err := r.Finalize(segment()); err != nil {
		t.Fatalf("降级应成功: %v", err)
	}
	waits := sleeper.recorded()
	if len(waits) != 2 {
		t.Fatalf("退避次数 = %d, 期望 2", len(waits))
	}
	for i, w := range waits {
		if w != DefaultRetryAfterCap {
			t.Fatalf("第 %d 次退避 = %v, 期望被钳到 %v", i+1, w, DefaultRetryAfterCap)
		}
	}
}

// —— 2. 本地故障绝不降级在线 ——

func TestRouter_LocalFailureNeverFallsBackToOnline(t *testing.T) {
	locals := &engineList{}
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	localF := factory(locals, func(n int) *fakeEngine {
		return &fakeEngine{
			id:        "local-1",
			finalErrs: []error{apierr.New(apierr.CodeEngineLoad)},
		}
	})
	onlineF := factory(onlines, func(n int) *fakeEngine {
		return &fakeEngine{id: "online:m", finalText: "不该被调用"}
	})

	r := newRouter(t, localF, onlineF, sleeper,
		contract.FallbackPolicy{FallbackToLocal: true, MaxRetries: 2}, nil)
	if err := r.Warm(context.Background()); err != nil {
		t.Fatalf("预热本地失败: %v", err)
	}
	// 在线引擎也预热一下（存在也不能被用）。
	if err := r.SetSource(contract.ModelSourceOnline); err != nil {
		t.Fatalf("切在线失败: %v", err)
	}
	if err := r.SetSource(contract.ModelSourceLocal); err != nil {
		t.Fatalf("切回本地失败: %v", err)
	}

	if _, err := r.Finalize(segment()); err == nil {
		t.Fatal("本地失败时期望返回错误")
	}
	onlineEng := onlines.all()[0]
	if got := onlineEng.finalCount.Load(); got != 0 {
		t.Fatalf("本地失败时不得调用在线引擎，实际调用 %d 次", got)
	}
	if r.Status().Degraded {
		t.Fatal("本地失败不得置 Degraded（降级只用于在线失败）")
	}
}

// —— 3. 录制中拒绝切换 ——

func TestRouter_SetSourceRejectedWhileSessionActive(t *testing.T) {
	active := true
	locals := &engineList{}
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	localF := factory(locals, func(n int) *fakeEngine { return &fakeEngine{id: "local-1"} })
	onlineF := factory(onlines, func(n int) *fakeEngine { return &fakeEngine{id: "online:m"} })

	r := newRouter(t, localF, onlineF, sleeper, contract.FallbackPolicy{FallbackToLocal: true}, func() bool { return active })

	err := r.SetSource(contract.ModelSourceOnline)
	if err == nil {
		t.Fatal("录制中切换应被拒绝")
	}
	if !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("期望 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	// 不应因为一次被拒的切换去预热目标引擎。
	if n := onlines.len(); n != 0 {
		t.Fatalf("被拒绝时不应构造在线引擎，实际构造 %d 个", n)
	}

	// 空闲后允许切换。
	active = false
	if err := r.SetSource(contract.ModelSourceOnline); err != nil {
		t.Fatalf("空闲时应允许切换: %v", err)
	}
	if r.Status().State != contract.EngineStateReady {
		t.Fatalf("期望 ready，实际 %q", r.Status().State)
	}
}

func TestRouter_SetSourceRejectsUnknownSource(t *testing.T) {
	r := NewRouter(RouterOptions{})
	err := r.SetSource("cloud")
	if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

// —— 4. 热切换：成功关旧引擎 / 失败回滚 ——

func TestRouter_HotSwitchClosesOldEngine(t *testing.T) {
	locals := &engineList{}
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	localF := factory(locals, func(n int) *fakeEngine {
		return &fakeEngine{id: "local-" + string(rune('1'+n))}
	})
	onlineF := factory(onlines, func(n int) *fakeEngine { return &fakeEngine{id: "online:m"} })

	r := newRouter(t, localF, onlineF, sleeper, contract.FallbackPolicy{FallbackToLocal: true}, nil)

	if err := r.Warm(context.Background()); err != nil {
		t.Fatalf("首次预热失败: %v", err)
	}
	first := locals.all()[0]

	// 切到在线：在线槽是空的，所以本地实例**不该**被卸载（它是降级兜底）。
	if err := r.SetSource(contract.ModelSourceOnline); err != nil {
		t.Fatalf("切到在线失败: %v", err)
	}
	if first.closeCount.Load() != 0 {
		t.Fatal("切到在线时不该卸载本地引擎（降级要用它）")
	}

	// 再切回本地：此刻才替换本地槽实例 → 旧实例延迟关闭。
	if err := r.SetSource(contract.ModelSourceLocal); err != nil {
		t.Fatalf("切回本地失败: %v", err)
	}
	if locals.len() != 2 {
		t.Fatalf("期望构造 2 个本地引擎，实际 %d", locals.len())
	}
	second := locals.all()[1]

	waitFor(t, time.Second, func() bool { return first.closeCount.Load() == 1 },
		"旧引擎未被关闭")
	if second.closeCount.Load() != 0 {
		t.Fatal("新引擎不应被关闭")
	}
	st := r.Status()
	if st.State != contract.EngineStateReady {
		t.Fatalf("期望 ready，实际 %q", st.State)
	}
	if st.EngineID != "local-2" {
		t.Fatalf("EngineID = %q, 期望 local-2", st.EngineID)
	}
	if st.LoadMs < 0 {
		t.Fatal("LoadMs 不应为负")
	}
}

func TestRouter_HotSwitchFailureRollsBack(t *testing.T) {
	locals := &engineList{}
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	localF := factory(locals, func(n int) *fakeEngine {
		return &fakeEngine{id: "local-1", finalText: "本地仍可用"}
	})
	onlineF := func() (Engine, error) {
		e := onlines.add(&fakeEngine{
			id:       "online:bad",
			warmErrs: []error{apierr.New(apierr.CodeModelNotFound)},
		})
		return e, nil
	}

	r := newRouter(t, localF, onlineF, sleeper, contract.FallbackPolicy{FallbackToLocal: true}, nil)
	if err := r.Warm(context.Background()); err != nil {
		t.Fatalf("预热本地失败: %v", err)
	}

	err := r.SetSource(contract.ModelSourceOnline)
	if !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("期望 ERR_MODEL_NOT_FOUND 透传，实际 %v", err)
	}
	st := r.Status()
	if st.State != contract.EngineStateFailed {
		t.Fatalf("期望 failed，实际 %q", st.State)
	}
	if st.Source != contract.ModelSourceLocal {
		t.Fatalf("失败后应回滚来源为 local，实际 %q", st.Source)
	}
	// 半成品引擎必须被关掉，避免泄漏。
	if got := onlines.all()[0].closeCount.Load(); got != 1 {
		t.Fatalf("预热失败的引擎应被关闭，实际关闭 %d 次", got)
	}
	// 旧引擎仍可用。
	res, ferr := r.Finalize(segment())
	if ferr != nil || res.Text != "本地仍可用" {
		t.Fatalf("回滚后应继续可用，实际 res=%+v err=%v", res, ferr)
	}
}

// —— 5. 不自动切回在线 ——

func TestRouter_NoAutoSwitchBackByDefault(t *testing.T) {
	locals := &engineList{}
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	netErr := onlineErr(FailNetwork, apierr.CodeEngineOnlineNetwork, 0)
	onlineF := factory(onlines, func(n int) *fakeEngine {
		return &fakeEngine{id: "online:m", finalErrs: []error{netErr, netErr, netErr, netErr, netErr, netErr}}
	})
	localF := factory(locals, func(n int) *fakeEngine {
		return &fakeEngine{id: "local-1", finalText: "本地"}
	})

	r := newRouter(t, localF, onlineF, sleeper,
		contract.FallbackPolicy{FallbackToLocal: true, AutoSwitchBack: false, MaxRetries: 2}, nil)
	warmLocalThenSwitchOnline(t, r)

	onlineEng := onlines.all()[0]
	if _, err := r.Finalize(segment()); err != nil {
		t.Fatalf("第一次降级失败: %v", err)
	}
	after := onlineEng.finalCount.Load()

	// 再转两句：不应再碰在线引擎（degraded 期间直接走本地）。
	for i := 0; i < 2; i++ {
		if _, err := r.Finalize(segment()); err != nil {
			t.Fatalf("降级后转写失败: %v", err)
		}
	}
	if got := onlineEng.finalCount.Load(); got != after {
		t.Fatalf("降级后不应再调用在线引擎，%d -> %d", after, got)
	}
	if onlines.len() != 1 {
		t.Fatalf("AutoSwitchBack=false 不应重建在线引擎，实际构造 %d 个", onlines.len())
	}
}

// —— 6. 离线拦截不降级 ——

func TestRouter_OfflineBlockedDoesNotFallback(t *testing.T) {
	locals := &engineList{}
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	blocked := onlineErr(FailBlocked, apierr.CodeOfflineEnforced, 0)
	onlineF := factory(onlines, func(n int) *fakeEngine {
		return &fakeEngine{id: "online:m", finalErrs: []error{blocked, blocked, blocked}}
	})
	localF := factory(locals, func(n int) *fakeEngine {
		return &fakeEngine{id: "local-1", finalText: "本地"}
	})

	r := newRouter(t, localF, onlineF, sleeper, contract.FallbackPolicy{FallbackToLocal: true}, nil)
	warmLocalThenSwitchOnline(t, r)

	_, err := r.Finalize(segment())
	if !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("期望 ERR_OFFLINE_ENFORCED，实际 %v", err)
	}
	if r.Status().Degraded {
		t.Fatal("离线拦截是配置状态，不该置 Degraded")
	}
	if got := locals.all()[0].finalCount.Load(); got != 0 {
		t.Fatalf("离线拦截不该降级本地，本地调用 %d 次", got)
	}
}

// —— 7. 无本地时只报错 ——

func TestRouter_NoLocalEngineReportsErrorOnly(t *testing.T) {
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	authErr := onlineErr(FailAuth, apierr.CodeEngineOnlineAuth, 0)
	onlineF := factory(onlines, func(n int) *fakeEngine {
		return &fakeEngine{id: "online:m", finalErrs: []error{authErr, authErr, authErr}}
	})

	r := NewRouter(RouterOptions{
		Source:      contract.ModelSourceOnline,
		Local:       nil, // 本构建不含本地引擎
		Online:      onlineF,
		Policy:      contract.FallbackPolicy{FallbackToLocal: true},
		Sleep:       sleeper.fn,
		UnloadDelay: time.Millisecond,
	})
	if err := r.Warm(context.Background()); err != nil {
		t.Fatalf("在线预热失败: %v", err)
	}

	_, err := r.Finalize(segment())
	if !apierr.HasCode(err, apierr.CodeEngineOnlineAuth) {
		t.Fatalf("期望 ERR_ENGINE_ONLINE_AUTH，实际 %v", err)
	}
	if r.Status().Degraded {
		t.Fatal("无本地可用时不该置 Degraded")
	}
}

// —— 8. Warm 失败分类 ——

func TestRouter_WarmFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"模型缺失", apierr.New(apierr.CodeModelNotFound), apierr.CodeModelNotFound},
		{"校验失败", apierr.New(apierr.CodeModelCorrupt), apierr.CodeModelCorrupt},
		{"加载失败", apierr.New(apierr.CodeEngineLoad), apierr.CodeEngineLoad},
		// 非结构化错误统一归为 ERR_ENGINE_LOAD。
		{"裸错误", errors.New("boom"), apierr.CodeEngineLoad},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			localF := func() (Engine, error) {
				return &fakeEngine{id: "local-bad", warmErrs: []error{tc.err, tc.err, tc.err}}, nil
			}
			r := NewRouter(RouterOptions{Source: contract.ModelSourceLocal, Local: localF})
			err := r.Warm(context.Background())
			if !apierr.HasCode(err, tc.want) {
				t.Fatalf("期望 %s，实际 %v", tc.want, err)
			}
			st := r.Status()
			if st.State != contract.EngineStateFailed {
				t.Fatalf("期望 failed，实际 %q", st.State)
			}
			if st.LastError == nil || st.LastError.Code != tc.want {
				t.Fatalf("LastError = %+v, 期望 code %s", st.LastError, tc.want)
			}
		})
	}
}

// —— 9. 状态回调与降级打点 ——

func TestRouter_NotifiesStatusAndDegradeOnce(t *testing.T) {
	locals := &engineList{}
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	netErr := onlineErr(FailNetwork, apierr.CodeEngineOnlineNetwork, 0)
	onlineF := factory(onlines, func(n int) *fakeEngine {
		return &fakeEngine{id: "online:m", finalErrs: []error{netErr, netErr, netErr, netErr, netErr, netErr}}
	})
	localF := factory(locals, func(n int) *fakeEngine {
		return &fakeEngine{id: "local-1", finalText: "本地"}
	})

	r := newRouter(t, localF, onlineF, sleeper, contract.FallbackPolicy{FallbackToLocal: true}, nil)

	var mu sync.Mutex
	states := []string{}
	r.SetOnStatus(func(st contract.EngineStatus) {
		mu.Lock()
		states = append(states, st.State)
		mu.Unlock()
	})
	degradeNotes := 0
	r.SetOnDegrade(func(string) {
		mu.Lock()
		degradeNotes++
		mu.Unlock()
	})

	warmLocalThenSwitchOnline(t, r)
	if _, err := r.Finalize(segment()); err != nil {
		t.Fatalf("降级失败: %v", err)
	}
	if _, err := r.Finalize(segment()); err != nil {
		t.Fatalf("第二次转写失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if degradeNotes != 1 {
		t.Fatalf("降级打点应只发生 1 次（避免刷屏），实际 %d 次", degradeNotes)
	}
	sawDegraded := false
	for _, s := range states {
		if s == contract.EngineStateDegraded {
			sawDegraded = true
		}
	}
	if !sawDegraded {
		t.Fatalf("状态回调里应出现 degraded，实际 %v", states)
	}
}

// —— 10. 健康状态 ——

func TestRouter_HealthReflectsDegradedNote(t *testing.T) {
	locals := &engineList{}
	onlines := &engineList{}
	sleeper := &sleepRecorder{}

	authErr := onlineErr(FailAuth, apierr.CodeEngineOnlineAuth, 0)
	onlineF := factory(onlines, func(n int) *fakeEngine {
		return &fakeEngine{id: "online:m", finalErrs: []error{authErr}}
	})
	localF := factory(locals, func(n int) *fakeEngine {
		return &fakeEngine{id: "local-1", finalText: "本地"}
	})

	r := newRouter(t, localF, onlineF, sleeper, contract.FallbackPolicy{FallbackToLocal: true}, nil)
	warmLocalThenSwitchOnline(t, r)
	if _, err := r.Finalize(segment()); err != nil {
		t.Fatalf("降级失败: %v", err)
	}

	h := r.Health(context.Background())
	if h.State != contract.EngineStateDegraded {
		t.Fatalf("State = %q, 期望 degraded", h.State)
	}
	if h.Note == "" {
		t.Fatal("Note 应说明降级原因")
	}
	if !h.Ready() {
		t.Fatal("降级状态下仍应能承接转写")
	}
}

// waitFor 轮询等待条件成立（用于断言「延迟卸载」这类异步行为）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
