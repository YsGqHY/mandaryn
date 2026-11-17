//go:build windows

package capture

import (
	"testing"
	"time"
)

// TestProbeFailureIsNotPermanent 锁死「探测失败不得被永久固化」这条回归。
//
// 背景（真实缺陷）：旧实现用 `sync.Once` 缓存探测结论，把**失败**也一起固化。
// 应用随系统开机自启时，AudioSrv 可能尚未就绪，首次探测拿到瞬时失败 →
// 整个进程生命周期都被判成「本机不支持按应用分路」→ 主页一直挂版本限制提示，
// 用户必须重启应用才能恢复。修复后：成功常驻，失败按 probeRetryInterval 重试。
func TestProbeFailureIsNotPermanent(t *testing.T) {
	// 用假的探测结果驱动缓存逻辑，避免依赖本机真实能力（真机探测恒为 true，
	// 无法覆盖「先失败、后成功」这条路径）。
	orig := probeFn
	t.Cleanup(func() { probeFn = orig })
	// 清空缓存，保证从干净状态开始。
	resetProbeCache()

	calls := 0
	probeFn = func() (bool, string) {
		calls++
		if calls == 1 {
			return false, "Windows 音频服务（AudioSrv）未就绪，稍后会自动重试"
		}
		return true, ""
	}

	// 第一次：失败，且结论如实为 false。
	if ok, _ := processLoopbackAvailable(); ok {
		t.Fatal("首次探测返回失败时不应判为可用")
	}
	// 节流窗口内重复调用不应再次探测。
	before := calls
	if ok, _ := processLoopbackAvailable(); ok {
		t.Fatal("节流窗口内应复用失败结论")
	}
	if calls != before {
		t.Fatalf("节流窗口内发生了额外探测：calls %d → %d", before, calls)
	}

	// 把上次探测时刻前移，模拟「等过了重试间隔」。
	processLoopbackMu.Lock()
	processLoopbackProbe.at = time.Now().Add(-probeRetryInterval - time.Second)
	processLoopbackMu.Unlock()

	// 第二次：应重新探测并转为可用（这就是「开机早期误报」的自愈路径）。
	if ok, note := processLoopbackAvailable(); !ok {
		t.Fatalf("失败不应被永久固化，重试后应转为可用（note=%q）", note)
	}
	// 成功之后常驻，不再探测。
	before = calls
	if ok, _ := processLoopbackAvailable(); !ok {
		t.Fatal("成功后应保持可用")
	}
	if calls != before {
		t.Fatalf("成功结论应常驻，不该重复探测：calls %d → %d", before, calls)
	}
}

// TestTransientProbeFailureClassification 锁死「瞬时失败不被说成版本限制」。
func TestTransientProbeFailureClassification(t *testing.T) {
	if !isTransientProbeFailure("Windows 音频服务（AudioSrv）未就绪，稍后会自动重试") {
		t.Error("音频服务未就绪应判为瞬时失败")
	}
	if isTransientProbeFailure("ActivateAudioInterfaceAsync 返回 HRESULT 0x8000000E") {
		t.Error("HRESULT 失败不能一律当瞬时失败，否则会掩盖真实不支持")
	}
	if isTransientProbeFailure("") {
		t.Error("空备注不应判为瞬时失败")
	}
}
