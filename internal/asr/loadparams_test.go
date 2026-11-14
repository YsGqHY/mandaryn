package asr

import (
	"context"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// TestLoadThreadsHelper 表驱动断言 asr.LoadThreads 的取值规则。
//
// 规则见 loadparams.go：nil → 0；≤0 → 0（交给引擎默认）；>0 原样透传；每次只读一次。
func TestLoadThreadsHelper(t *testing.T) {
	cases := []struct {
		name string
		gen  func() int
		want int
	}{
		{name: "nil 生成器", gen: nil, want: 0},
		{name: "正常值", gen: func() int { return 4 }, want: 4},
		{name: "单线程", gen: func() int { return 1 }, want: 1},
		{name: "零值", gen: func() int { return 0 }, want: 0},
		{name: "负值", gen: func() int { return -3 }, want: 0},
	}
	for _, c := range cases {
		if got := LoadThreads(c.gen); got != c.want {
			t.Errorf("%s：LoadThreads() = %d，期望 %d", c.name, got, c.want)
		}
	}

	// 每次调用只读一次：值从 8 → 2 的生成器，一次读只应得到 8。
	var calls int
	values := []int{8, 2}
	gen := func() int {
		v := values[len(values)-1]
		if calls < len(values) {
			v = values[calls]
		}
		calls++
		return v
	}
	if got := LoadThreads(gen); got != 8 {
		t.Fatalf("首次读取应拿到 8，实际 %d", got)
	}
	if calls != 1 {
		t.Fatalf("一次 LoadThreads 只应读一次生成器，实际 %d 次", calls)
	}
}

// TestRouterReadsThreadsOncePerLocalLoad 断言每次加载本地引擎读一次档位，
// 且在线加载不读（在线端点没有线程数概念）。
func TestRouterReadsThreadsOncePerLocalLoad(t *testing.T) {
	var calls int
	threads := func() int {
		calls++
		return 2 // 模拟 L2
	}
	list := &engineList{}
	r := NewRouter(RouterOptions{
		Source:        contract.ModelSourceLocal,
		Local:         factory(list, func(int) *fakeEngine { return &fakeEngine{id: "local"} }),
		Online:        func() (Engine, error) { return &fakeEngine{id: "online"}, nil },
		ActiveSession: func() bool { return false },
		Sleep:         (&sleepRecorder{}).fn,
		UnloadDelay:   time.Millisecond,
		LoadThreads:   threads,
	})

	if calls != 0 {
		t.Fatalf("构造 Router 时不该读档位，实际 %d 次", calls)
	}
	if got := r.LoadThreads(); got != 0 {
		t.Fatalf("尚未加载时 LoadThreads() 应为 0，实际 %d", got)
	}

	ctx := context.Background()
	if err := r.Warm(ctx); err != nil {
		t.Fatalf("预热本地失败：%v", err)
	}
	if calls != 1 {
		t.Fatalf("一次本地加载应读一次档位，实际 %d 次", calls)
	}
	if got := r.LoadThreads(); got != 2 {
		t.Fatalf("LoadThreads() 应记录本次读到的 2，实际 %d", got)
	}

	// 再次加载（等价 Reload）：又读一次，说明不是「构造时缓存」。
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload 失败：%v", err)
	}
	if calls != 2 {
		t.Fatalf("第二次本地加载应再读一次档位，实际 %d 次", calls)
	}

	// 切到在线：不该读档位（线程数是本地引擎的构造参数）。
	if err := r.SetSource(contract.ModelSourceOnline); err != nil {
		t.Fatalf("切换在线失败：%v", err)
	}
	if calls != 2 {
		t.Fatalf("在线加载不该读档位，实际累计 %d 次", calls)
	}
}

// TestRouterLoadThreadsNilKeepsLegacyBehavior 断言不注入 LoadThreads 时行为与旧版一致。
func TestRouterLoadThreadsNilKeepsLegacyBehavior(t *testing.T) {
	list := &engineList{}
	r := newRouter(t,
		factory(list, func(int) *fakeEngine { return &fakeEngine{id: "local"} }),
		nil, &sleepRecorder{}, contract.FallbackPolicy{}, func() bool { return false })

	if err := r.Warm(context.Background()); err != nil {
		t.Fatalf("预热失败：%v", err)
	}
	if got := r.LoadThreads(); got != 0 {
		t.Fatalf("未注入时 LoadThreads() 应为 0（引擎用默认值），实际 %d", got)
	}
	if r.Status().State != contract.EngineStateReady {
		t.Fatalf("状态应为 ready，实际 %s", r.Status().State)
	}
}

// TestRouterSetLoadThreadsAppliedOnNextLoad 断言后置接线同样「下次加载生效」。
func TestRouterSetLoadThreadsAppliedOnNextLoad(t *testing.T) {
	list := &engineList{}
	r := newRouter(t,
		factory(list, func(int) *fakeEngine { return &fakeEngine{id: "local"} }),
		nil, &sleepRecorder{}, contract.FallbackPolicy{}, func() bool { return false })

	if err := r.Warm(context.Background()); err != nil {
		t.Fatalf("预热失败：%v", err)
	}
	if got := r.LoadThreads(); got != 0 {
		t.Fatalf("预热时未注入，应为 0，实际 %d", got)
	}

	r.SetLoadThreads(func() int { return 1 }) // 模拟降到 L3
	if got := r.LoadThreads(); got != 0 {
		t.Fatalf("改注入点不该立刻改动已加载引擎的记录，实际 %d", got)
	}
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload 失败：%v", err)
	}
	if got := r.LoadThreads(); got != 1 {
		t.Fatalf("下一次加载应读到 1，实际 %d", got)
	}
}
