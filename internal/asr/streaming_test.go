package asr

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// 边录边转的**能力契约**用例（FR-03-10）。
//
// 这里只验证「有没有、报什么错、能不能复用」，不跑真模型：
// 流式解码本身属 sherpa-onnx 封装（internal/asr/local），真推理用例走 sherpa_models tag。

// fakeStream 是满足 StreamingEngine 的最小实现。
type fakeStream struct {
	id      string
	accepts int
	resets  int
	closes  int
	warmErr error
}

func (f *fakeStream) ID() string                  { return f.id }
func (f *fakeStream) Warm(context.Context) error  { return f.warmErr }
func (f *fakeStream) Health(context.Context) HealthStatus {
	return HealthStatus{State: contract.EngineStateReady, EngineID: f.id}
}
func (f *fakeStream) Feed([]float32) (<-chan Partial, error) {
	ch := make(chan Partial)
	close(ch)
	return ch, nil
}
func (f *fakeStream) Finalize(AudioSegment) (FinalResult, error) {
	return FinalResult{Text: "offline", EngineID: f.id}, nil
}
func (f *fakeStream) Close() error { f.closes++; return nil }
func (f *fakeStream) Accept(samples []float32) (string, error) {
	f.accepts++
	if len(samples) == 0 {
		return "", nil
	}
	return "字", nil
}
func (f *fakeStream) Reset() error { f.resets++; return nil }

// TestRouterStreamingUnavailableByDefault 断言**默认（没有流式工厂）就是不可用**。
//
// 这条是「默认关闭」在引擎层的一半保证：不装流式包时，
// 边录边转必须明确报 ERR_LOCAL_UNAVAILABLE，而不是悄悄退回离线引擎。
func TestRouterStreamingUnavailableByDefault(t *testing.T) {
	r := NewRouter(RouterOptions{})

	eng, err := r.Streaming()
	if err == nil {
		t.Fatalf("无流式工厂时不应可用，实际拿到 %v", eng)
	}
	if !apierr.HasCode(err, apierr.CodeLocalUnavailable) {
		t.Fatalf("错误码应为 ERR_LOCAL_UNAVAILABLE，实际 %v", err)
	}
	if avail := r.StreamingAvailable(); !apierr.HasCode(avail, apierr.CodeLocalUnavailable) {
		t.Fatalf("StreamingAvailable 应报不可用，实际 %v", avail)
	}
}

// TestRouterStreamingFactoryOnce 断言流式引擎只构造一次并被复用（不重复加载模型）。
func TestRouterStreamingFactoryOnce(t *testing.T) {
	var calls atomic.Int32
	fake := &fakeStream{id: "zipformer-stream@test"}
	r := NewRouter(RouterOptions{
		Streaming: func() (StreamingEngine, error) {
			calls.Add(1)
			return fake, nil
		},
	})

	first, err := r.Streaming()
	if err != nil {
		t.Fatalf("Streaming 失败：%v", err)
	}
	second, err := r.Streaming()
	if err != nil {
		t.Fatalf("第二次 Streaming 失败：%v", err)
	}
	if first != second {
		t.Fatal("应复用同一个流式引擎实例")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("工厂被调用 %d 次，期望 1 次", got)
	}
	if id := r.StreamingEngineID(); id != "zipformer-stream@test" {
		t.Fatalf("StreamingEngineID = %q", id)
	}
	// CloseStreaming 释放并允许下次重新构造。
	if err := r.CloseStreaming(); err != nil {
		t.Fatalf("CloseStreaming: %v", err)
	}
	if fake.closes != 1 {
		t.Fatalf("关闭次数 = %d，期望 1", fake.closes)
	}
	if r.StreamingEngineID() != "" {
		t.Fatal("关闭后 StreamingEngineID 应为空")
	}
}

// TestRouterStreamingModelMissing 断言「模型包缺失」透传 ERR_MODEL_NOT_FOUND，不伪装可用。
func TestRouterStreamingModelMissing(t *testing.T) {
	r := NewRouter(RouterOptions{
		Streaming: func() (StreamingEngine, error) {
			return nil, apierr.New(apierr.CodeModelNotFound).
				WithDetail("流式模型包未安装")
		},
	})
	eng, err := r.Streaming()
	if eng != nil {
		t.Fatal("模型缺失时不应返回引擎")
	}
	if !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("错误码应为 ERR_MODEL_NOT_FOUND，实际 %v", err)
	}
}

// TestRouterStreamingProbeIsCheap 断言可用性探测**不构造引擎**（录制页每次挂载都会问）。
func TestRouterStreamingProbeIsCheap(t *testing.T) {
	var constructed atomic.Int32
	probeErr := apierr.New(apierr.CodeModelNotFound)

	r := NewRouter(RouterOptions{
		Streaming: func() (StreamingEngine, error) {
			constructed.Add(1)
			return &fakeStream{id: "x@1"}, nil
		},
		StreamingProbe: func() error { return probeErr },
	})
	if err := r.StreamingAvailable(); !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("探测应报 ERR_MODEL_NOT_FOUND，实际 %v", err)
	}
	if constructed.Load() != 0 {
		t.Fatalf("StreamingAvailable 不得构造引擎，实际构造了 %d 次", constructed.Load())
	}

	// 探测通过时不构造也算可用（真正加载时才付出代价）。
	ok := NewRouter(RouterOptions{
		Streaming:      func() (StreamingEngine, error) { constructed.Add(1); return &fakeStream{id: "x@1"}, nil },
		StreamingProbe: func() error { return nil },
	})
	if err := ok.StreamingAvailable(); err != nil {
		t.Fatalf("探测通过时应可用：%v", err)
	}
	if constructed.Load() != 0 {
		t.Fatal("探测通过也不得构造引擎")
	}
}

// TestRouterStreamingWarmFailure 断言预热失败报 ERR_ENGINE_LOAD，且不缓存坏实例。
func TestRouterStreamingWarmFailure(t *testing.T) {
	fake := &fakeStream{id: "bad@1", warmErr: apierr.New(apierr.CodeEngineLoad)}
	r := NewRouter(RouterOptions{
		Streaming: func() (StreamingEngine, error) { return fake, nil },
	})
	if _, err := r.Streaming(); !apierr.HasCode(err, apierr.CodeEngineLoad) {
		t.Fatalf("预热失败应报 ERR_ENGINE_LOAD，实际 %v", err)
	}
	if fake.closes != 1 {
		t.Fatalf("预热失败应关闭实例，实际关闭 %d 次", fake.closes)
	}
	if r.StreamingEngineID() != "" {
		t.Fatal("预热失败不应缓存引擎")
	}
}

// TestRouterDoesNotAffectExistingEnginePath 断言新增流式入口**没有**改变既有降级矩阵。
//
// 边录边转是旁路：它的存在（或缺失）不得影响 Warm / Finalize / Health 的行为。
func TestRouterDoesNotAffectExistingEnginePath(t *testing.T) {
	fake := &fakeStream{id: "stream@1"}
	r := NewRouter(RouterOptions{
		Streaming: func() (StreamingEngine, error) { return fake, nil },
	})
	// 只调用流式入口：既有来源状态必须仍是未加载态（没有被顺手 mark ready）。
	st := r.Health(context.Background())
	if st.State == contract.EngineStateReady {
		t.Fatalf("流式旁路不应把主引擎标成 ready：%+v", st)
	}
	if st.EngineID != "" {
		t.Fatalf("流式旁路不应写主引擎 ID：%q", st.EngineID)
	}
}

// —— LazyStreaming：装配层在录制 Start 路径上取实例，绝不能顺手加载模型 ——

// TestLazyStreamingDoesNotLoadOnResetOrClose 断言「只取实例 + Reset + Close」不触发加载。
//
// 这是铁律「推理不得阻塞采集」在 asr 层的守护：`recording.Start` 同步调用
// `NewLiveSink` 时采集管道已打开，若这里加载流式模型（数秒），
// 会话开头的音频会被顶掉。所以加载必须等到推理 goroutine 第一次 Accept。
func TestLazyStreamingDoesNotLoadOnResetOrClose(t *testing.T) {
	var loads int32
	fake := &fakeStream{id: "zipformer-stream@1"}
	l := LazyStreaming(func() (StreamingEngine, error) {
		atomic.AddInt32(&loads, 1)
		return fake, nil
	})

	// 未加载时的行为：ID 空、Reset 只打标记、Close 空操作。
	if id := l.ID(); id != "" {
		t.Fatalf("未加载时 ID 应为空（不撒谎），实际 %q", id)
	}
	if err := l.Reset(); err != nil {
		t.Fatalf("未加载时 Reset 应成功（只打标记）：%v", err)
	}
	if n := atomic.LoadInt32(&loads); n != 0 {
		t.Fatalf("Reset 触发了加载 %d 次（会阻塞采集）", n)
	}
	if fake.resets != 0 {
		t.Fatalf("未加载时不应调到真引擎 Reset：%d", fake.resets)
	}
	if st := l.Health(context.Background()); st.State == contract.EngineStateReady {
		t.Fatalf("未加载时不应报 ready：%+v", st)
	}
	if n := atomic.LoadInt32(&loads); n != 0 {
		t.Fatalf("Health 触发了加载 %d 次", n)
	}

	// 第一次 Accept（推理 goroutine 里）才加载，并把「加载后先 Reset」补上。
	if _, err := l.Accept([]float32{0.1, 0.2}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("Accept 应触发且仅触发一次加载，实际 %d", n)
	}
	if fake.resets != 1 {
		t.Fatalf("加载后应补一次 Reset（清上一会话解码状态），实际 %d", fake.resets)
	}
	if fake.accepts != 1 {
		t.Fatalf("Accept 应转发到真引擎，实际 %d", fake.accepts)
	}
	if id := l.ID(); id != "zipformer-stream@1" {
		t.Fatalf("加载后 ID = %q", id)
	}

	// 再 Accept 不重复加载、不重复 Reset。
	if _, err := l.Accept([]float32{0.3}); err != nil {
		t.Fatalf("Accept#2: %v", err)
	}
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("加载应只发生一次，实际 %d", n)
	}
	if fake.resets != 1 {
		t.Fatalf("Reset 不应重复：%d", fake.resets)
	}

	// Close 幂等。
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fake.closes != 1 {
		t.Fatalf("Close 应转发一次，实际 %d", fake.closes)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close 幂等：%v", err)
	}
}

// TestLazyStreamingCachesLoadFailure 断言加载失败只发生一次（不反复重试）。
//
// 失败语义：会话内的边录边转就此失效（LiveRunner 会标记 failed），
// 但录制照常；反复重试会在每个样本上都去敲一遍模型包，白白烧 CPU。
func TestLazyStreamingCachesLoadFailure(t *testing.T) {
	var loads int32
	l := LazyStreaming(func() (StreamingEngine, error) {
		atomic.AddInt32(&loads, 1)
		return nil, apierr.New(apierr.CodeModelNotFound).WithDetail("缺流式模型包")
	})

	for i := 0; i < 3; i++ {
		if _, err := l.Accept([]float32{0.1}); !apierr.HasCode(err, apierr.CodeModelNotFound) {
			t.Fatalf("第 %d 次应返回 ERR_MODEL_NOT_FOUND，实际 %v", i+1, err)
		}
	}
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("加载失败应被记住，实际尝试 %d 次", n)
	}
}

// TestLazyStreamingNilResolve 断言没有解析器时报不可用（而不是 panic）。
func TestLazyStreamingNilResolve(t *testing.T) {
	l := LazyStreaming(nil)
	if _, err := l.Accept([]float32{0.1}); !apierr.HasCode(err, apierr.CodeLocalUnavailable) {
		t.Fatalf("应返回 ERR_LOCAL_UNAVAILABLE，实际 %v", err)
	}
	if err := l.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestLazyStreamingSerializesEngineCalls 断言「并发首次使用只加载一次」**且**
// 「引擎调用被串行化」。
//
// 场景：上一个会话的 `Close` 是非阻塞的，它的推理 goroutine 可能还在收尾 flush
// （还在跑 Accept），用户此时就开始下一个会话 —— 两个 Accept 循环会撞进同一个
// 解码器。流式解码器不是并发安全的（CGO 层会崩），所以代理必须串行化。
// 这里用**非并发安全**的替身（`accepts++` 无原子）来钉住这一点：
// 若代理漏了锁，`-race` 会直接报数据竞争。
func TestLazyStreamingSerializesEngineCalls(t *testing.T) {
	var loads int32
	fake := &fakeStream{id: "stream@1"}
	l := LazyStreaming(func() (StreamingEngine, error) {
		atomic.AddInt32(&loads, 1)
		return fake, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Accept([]float32{0.1}); err != nil {
				t.Errorf("Accept: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("并发首次使用应只加载一次，实际 %d", n)
	}
	if fake.accepts != 8 {
		t.Fatalf("8 次 Accept 应全部转发，实际 %d", fake.accepts)
	}
}
