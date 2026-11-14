//go:build darwin

package capture

import (
	"vip.oberon.mandaryn/internal/contract"
)

// 本文件是 macOS 的**诚实桩**：不假装支持，也不留未实现的占位实现。
//
// 为什么不返回「空切片 + nil」：
//   - 空切片会让 UI 显示「没有可用的音频源」，用户会以为是自己电脑的问题，
//     继续点刷新；而 ERR_CAPTURE_UNAVAILABLE 带 Reason，能明确说
//     「这个平台还没实现、前置条件是什么」，用户知道该等版本而不是折腾设置。
//   - 测试也更可靠：不会把「未实现」误当成「本机没声卡」。
//
// 待实现路线（ARCH §4.1.2）：
//   - 应用级：Core Audio process tap（macOS 14.2+），首次需在
//     「系统设置 → 隐私与安全性 → 屏幕与系统音频录制」授权；
//   - 设备级：安装虚拟声卡或创建聚合设备（会含全部应用声音）。

// open 桩：macOS 尚未实现捕获通道。
//
// 返回错误而不是假通道：一个「能 Open 但永远没有数据」的 Channel 会让上层
// 录出一段静音文件，用户以为录到了却什么都没有——比明确失败更糟。
func open(kind, _ string) (Channel, error) {
	return nil, sourceUnavailable(openUnavailableReason(kind))
}

// enumerateUnavailableReason macOS 枚举不可用的原因文案。
func enumerateUnavailableReason() string {
	return "macOS 音频源枚举尚未实现：按应用分路捕获需要 macOS 14.2 及以上的 Core Audio process tap 并授予系统音频录制权限；设备回环需要虚拟声卡或聚合设备。当前版本无法枚举音频源。"
}

// probeUnavailableReason macOS 探测不可用的原因文案。
func probeUnavailableReason(kind string) string {
	if kind == contract.SourceKindApplication {
		return "macOS 应用级捕获尚未实现（需要 macOS 14.2 及以上的 Core Audio process tap 与系统音频授权），无法探测该应用源。"
	}
	return "macOS 设备回环捕获尚未实现（需要虚拟声卡或聚合设备），无法探测该设备源。"
}

// openUnavailableReason macOS 打开通道不可用的原因文案。
func openUnavailableReason(kind string) string {
	if kind == contract.SourceKindApplication {
		return "macOS 应用级捕获尚未实现：需要 macOS 14.2 及以上的 Core Audio process tap，并在系统设置中授予系统音频录制权限。"
	}
	return "macOS 设备回环捕获尚未实现：需要先安装虚拟声卡或创建聚合设备（该路径会包含全部应用声音）。"
}
