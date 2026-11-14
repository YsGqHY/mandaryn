package capture

import (
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// TestSnapshotSignature 验证变化指纹的语义：
// 只关心「有哪些源、是否可用」，不关心电平（Producing）。
func TestSnapshotSignature(t *testing.T) {
	base := []contract.Source{
		{ID: "app:kook.exe", Kind: contract.SourceKindApplication, Present: true, Producing: false},
		{ID: "dev:speaker", Kind: contract.SourceKindDevice, Present: true, Producing: false},
	}
	sig := snapshotSignature(base)
	if sig == "" {
		t.Fatal("非空列表的指纹不应为空")
	}

	// 1) 顺序不同 → 指纹相同（排序后比较）。
	reordered := []contract.Source{base[1], base[0]}
	if got := snapshotSignature(reordered); got != sig {
		t.Fatalf("顺序不应影响指纹：%q vs %q", got, sig)
	}

	// 2) 只有 Producing 变化 → 指纹不变（否则 UI 每秒都在收变化通知）。
	levelChanged := []contract.Source{
		{ID: "app:kook.exe", Kind: contract.SourceKindApplication, Present: true, Producing: true},
		{ID: "dev:speaker", Kind: contract.SourceKindDevice, Present: true, Producing: true},
	}
	if got := snapshotSignature(levelChanged); got != sig {
		t.Fatalf("Producing 变化不应改变指纹：%q vs %q", got, sig)
	}

	// 3) 源消失 → 指纹变化。
	fewer := base[:1]
	if got := snapshotSignature(fewer); got == sig {
		t.Fatal("源数量变化时指纹应改变")
	}

	// 4) Present 变化 → 指纹变化。
	gone := []contract.Source{
		{ID: "app:kook.exe", Kind: contract.SourceKindApplication, Present: false},
		{ID: "dev:speaker", Kind: contract.SourceKindDevice, Present: true},
	}
	if got := snapshotSignature(gone); got == sig {
		t.Fatal("Present 变化时指纹应改变")
	}

	// 5) 空列表 → 空指纹（用于「尚无基线」判断）。
	if got := snapshotSignature(nil); got != "" {
		t.Fatalf("空列表指纹应为空，实际 %q", got)
	}
}

// TestMonitorIdempotent 验证 Start/Stop 可重复调用、不会重复起 goroutine 或卡死。
//
// 不依赖真实枚举结果的形状：本机可能 0 个源（CI/无声卡），也可能一堆。
func TestMonitorIdempotent(t *testing.T) {
	var calls int
	m := NewMonitor(50*time.Millisecond, func([]contract.Source) { calls++ })

	m.Start()
	m.Start() // 幂等
	m.Stop()
	m.Stop() // 幂等

	// Stop 返回后 goroutine 必须已退出，快照可用（可能为空）。
	_ = m.Snapshot()

	// 首次基线不触发回调，因此即便发生了一次枚举，calls 也应为 0。
	// 这里不硬断言 equals 0：极少数情况下第二次 tick 恰好遇到真实变化。
	if calls > 1 {
		t.Fatalf("回调次数异常：%d", calls)
	}
}

// TestNewMonitorDefaultInterval 校验非法间隔被兜底为 2s。
func TestNewMonitorDefaultInterval(t *testing.T) {
	m := NewMonitor(0, nil)
	if m.interval != DefaultPollInterval {
		t.Fatalf("期望默认间隔 %v，实际 %v", DefaultPollInterval, m.interval)
	}
	if DefaultPollInterval != 2*time.Second {
		t.Fatalf("默认轮询间隔应为 2s，实际 %v", DefaultPollInterval)
	}
}
