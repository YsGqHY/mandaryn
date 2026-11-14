//go:build linux

package capture

import (
	"vip.oberon.mandaryn/internal/contract"
)

// capabilities 实现 Capabilities（API-1.6）的 Linux 分支。
//
// 取值原则与 Windows/darwin 分支一致：**能力 = 系统满足前提 && 本版本已实现**。
// Linux 上 monitor 源（设备回环）在系统层面几乎总是可用，但本版本尚未实现
// 采集路径，因此这里同样报 false，并在 Reason 里区分「平台限制」与「代码缺口」——
// 让用户知道等版本升级就会好，而不是去装声卡。
func capabilities() contract.CaptureCaps {
	return contract.CaptureCaps{
		Platform:        "linux",
		ProcessLoopback: false,
		DeviceLoopback:  false,
		Reason: "Linux 捕获尚未实现：按应用分路捕获需要 PipeWire 0.3.60 及以上的 node 级捕获；" +
			"设备回环本可通过 PulseAudio/PipeWire 的 monitor 源实现（会包含全部应用声音），" +
			"但当前版本尚未接入，暂时无法录音。",
		RequiresPermission: false,
	}
}
