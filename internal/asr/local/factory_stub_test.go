//go:build nollm || !windows || !cgo

// 桩构建下的工厂契约测试。
//
// 为什么单独加 build tag：`internal/asr/local` 的测试二进制一旦存在，Windows + CGO
// 下会绑定 sherpa-onnx 的 3 个 DLL，缺 DLL 时连测试都起不来（0xc0000135）。
// 因此**Windows + CGO 下本包不编译任何测试**（真实推理测试另有 `sherpa_models` tag）；
// 「工厂每次调用读一次线程数」这条契约在纯 Go 的 `internal/asr` 里也有同形断言。
//
// `-tags nollm` 走的是桩实现，不绑任何 DLL，因此这里可以安全地覆盖到：
// 桩构建的工厂契约（恒失败 + 不 panic + 明确错误码）是 nollm 变体的唯一自动化保障。
package local

import (
	"sync/atomic"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
)

// fakeLocator 是 BundleLocator 的假实现（只回答「装了这个包」）。
type fakeLocator struct {
	dir      string
	bundleID string
	err      error
}

func (f *fakeLocator) LocateBundle(string) (string, string, error) {
	return f.dir, f.bundleID, f.err
}

// TestNewFactoryWithThreadsStubReturnsUnavailable 断言桩构建不 panic、给明确错误码。
func TestNewFactoryWithThreadsStubReturnsUnavailable(t *testing.T) {
	calls := atomic.Int64{}
	f := NewFactoryWithThreads(&fakeLocator{dir: "/tmp/models/x", bundleID: "x@1.0.0"}, func() int {
		calls.Add(1)
		return 4
	})
	if f == nil {
		t.Fatal("NewFactoryWithThreads 不能返回 nil 工厂")
	}
	eng, err := f()
	if eng != nil {
		t.Fatalf("桩构建不应返回引擎，实际 %T", eng)
	}
	if !apierr.HasCode(err, apierr.CodeLocalUnavailable) {
		t.Fatalf("期望 ERR_LOCAL_UNAVAILABLE，实际 %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("一次调用应读一次线程数，实际 %d 次", got)
	}
}

// TestNewFactoryWithThreadsStubReadsPerCall 断言「加载两次读两次」（不是构造时读一次）。
func TestNewFactoryWithThreadsStubReadsPerCall(t *testing.T) {
	var calls int
	f := NewFactoryWithThreads(&fakeLocator{dir: "/tmp/models/x", bundleID: "x"}, func() int {
		calls++
		return 2
	})
	if calls != 0 {
		t.Fatalf("构造工厂时不该读线程数，实际读了 %d 次", calls)
	}
	_, _ = f()
	_, _ = f()
	if calls != 2 {
		t.Fatalf("工厂被调用两次应读两次档位，实际 %d 次", calls)
	}
}

// TestNewFactoryWithoutThreadsStub 断言旧入口（NewFactory）行为不变。
func TestNewFactoryWithoutThreadsStub(t *testing.T) {
	_, err := NewFactory(&fakeLocator{dir: "/tmp/models/x", bundleID: "x"})()
	if !apierr.HasCode(err, apierr.CodeLocalUnavailable) {
		t.Fatalf("期望 ERR_LOCAL_UNAVAILABLE，实际 %v", err)
	}
	_, err = NewFactory(nil)()
	if !apierr.HasCode(err, apierr.CodeLocalUnavailable) {
		t.Fatalf("定位器为 nil 时应返回 ERR_LOCAL_UNAVAILABLE，实际 %v", err)
	}
}

// 编译期断言：两种工厂都产出 asr.LocalFactory。
var (
	_ asr.LocalFactory = NewFactory(nil)
	_ asr.LocalFactory = NewFactoryWithThreads(nil, nil)
)
