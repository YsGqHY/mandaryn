//go:build windows

package capture

import (
	"testing"

	"vip.oberon.mandaryn/internal/contract"
)

// TestGateAppSource 锁死「能力缺失时应用源必须标为不可用」这条门控。
//
// 为什么值得单测：这是一处**纯函数**，而它守的是 FR-02-4 的诚实声明——
// 探测不到进程级捕获却仍把应用源列为可选，用户点下去必然在 Open 阶段失败
// （caps_windows.go 顶部明确禁止这种「引导用户选一个必然失败的源」）。
// 真机探测结果随机器变化，无法用它断言，纯函数才是可测的那一半。
func TestGateAppSource(t *testing.T) {
	app := func() contract.Source {
		return contract.Source{
			ID:          AppSourceID("kook.exe"),
			Kind:        contract.SourceKindApplication,
			DisplayName: "KOOK",
			ExeName:     "kook.exe",
			Present:     true,
		}
	}

	t.Run("能力可用时不改动", func(t *testing.T) {
		src := app()
		gateAppSource(&src, true)
		if !src.Present {
			t.Error("能力可用时不应把应用源标为不可用")
		}
		if src.Warn != "" {
			t.Errorf("能力可用时不应写 Warn，实际 %q", src.Warn)
		}
	})

	t.Run("能力缺失时标为不可用并带上键", func(t *testing.T) {
		src := app()
		gateAppSource(&src, false)
		if src.Present {
			t.Error("能力缺失时应用源必须是 Present=false")
		}
		if src.Warn != WarnProcessLoopbackUnavailable {
			t.Errorf("Warn 应为 %q，实际 %q", WarnProcessLoopbackUnavailable, src.Warn)
		}
		// 门控只表达「选不了」，不应丢掉识别源所需的信息。
		if src.ExeName != "kook.exe" || src.ID != AppSourceID("kook.exe") {
			t.Errorf("门控不该改动源的标识字段：%+v", src)
		}
	})

	t.Run("设备源不受牵连", func(t *testing.T) {
		src := contract.Source{
			ID:        DeviceSourceID("{0.0.0.00000000}.{guid}"),
			Kind:      contract.SourceKindDevice,
			Present:   true,
			DeviceID:  "{0.0.0.00000000}.{guid}",
			IsDefault: true,
		}
		gateAppSource(&src, false)
		if !src.Present {
			t.Error("进程级能力缺失不应影响设备回环源（FR-02-3 兜底路径必须仍然可选）")
		}
		if src.Warn != "" {
			t.Errorf("设备源不该被写入进程级警告，实际 %q", src.Warn)
		}
	})

	t.Run("nil 安全", func(t *testing.T) {
		gateAppSource(nil, false) // 不应 panic
	})
}

// TestProcessLoopbackCapableMatchesCaps 保证门控判据与能力矩阵口径一致。
//
// 两处若各写一遍「探测通过 && 已实现」，很容易改一处忘一处，导致
// GetCapabilities() 说支持、而枚举把源标灰（或反之）。
func TestProcessLoopbackCapableMatchesCaps(t *testing.T) {
	if got, want := processLoopbackCapable(), capabilities().ProcessLoopback; got != want {
		t.Fatalf("门控判据 %v 与 CaptureCaps.ProcessLoopback %v 不一致", got, want)
	}
}
