//go:build darwin

package capture

import (
	"vip.oberon.mandaryn/internal/contract"
)

// capabilities 实现 Capabilities（API-1.6）的 macOS 分支。
//
// 取值原则与 Windows 分支一致：**能力 = 系统满足前提 && 本版本已实现**。
// macOS 尚未实现任何捕获路径，因此两项都报 false，并在 Reason 里同时说明
// 「系统侧前提」与「本版本未实现」，避免让用户以为是系统设置问题。
//
// 备注：ProcessLoopback / DeviceLoopback 都是 false 并不代表 macOS 永远不行，
// 只是当前版本不行（详见 ARCH §4.1.2 的能力矩阵）。
func capabilities() contract.CaptureCaps {
	return contract.CaptureCaps{
		Platform:        "darwin",
		ProcessLoopback: false,
		// macOS 没有内建的回环设备，必须先装虚拟声卡或建聚合设备。
		DeviceLoopback: false,
		Reason: "macOS 捕获尚未实现：按应用分路捕获需要 macOS 14.2 及以上的 Core Audio process tap，" +
			"并在「系统设置 → 隐私与安全性」中授予系统音频录制权限；" +
			"设备回环需要安装虚拟声卡或创建聚合设备（会包含全部应用声音）。" +
			"当前版本在 macOS 上无法录音。",
		RequiresPermission: true,
	}
}
