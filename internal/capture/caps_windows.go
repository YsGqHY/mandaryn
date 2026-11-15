//go:build windows

package capture

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"

	"vip.oberon.mandaryn/internal/contract"
)

// minProcessLoopbackBuild 进程级回环捕获**官方文档标注**的最低系统版本。
//
// 依据：微软文档标注 AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK 需要
// Windows 10 Build 20348 或 Windows 11（ARCH §4.1.2）。
//
// 这个常量**只用于 Reason 文案**，不参与能力判定。原因见 processLoopbackImplemented
// 与 probe_windows.go 的说明：文档门槛是「头文件随 SDK 公开发布」的版本，
// 与运行时是否真的支持并不等价（本机 Build 19045 实测可用）。
const minProcessLoopbackBuild = 20348

// processLoopbackImplemented Windows 进程级捕获的**代码实现**状态。
//
// 刻意与「系统是否支持」分开表达：
//   - 系统支持 ≠ 本版本已实现。若这里为 false 而系统支持，Reason 必须说
//     「本版本尚未实现」，而不是拿系统版本当挡箭牌；
//   - 若这里返回 true 而 Open 实际失败，UI 会引导用户选一个必然失败的源，
//     属于欺骗性能力声明（FR-02-4）。
//
// 因此 Capabilities 取「**实测**探测通过 && 已实现」双条件。
const processLoopbackImplemented = true

// capabilities 实现 Capabilities（API-1.6）。
//
// 能力判定**只以真实探测结果为准**，绝不由版本号推断（FR-02-4）：
// 探测会真的发起一次 ActivateAudioInterfaceAsync + PROCESS_LOOPBACK 激活，
// 结论在进程内用 sync.Once 缓存。
func capabilities() contract.CaptureCaps {
	caps := contract.CaptureCaps{
		Platform: "windows",
		// WASAPI 共享模式回环从 Windows Vista 起就有，Win10 必然可用。
		DeviceLoopback: true,
	}

	probed, note := processLoopbackAvailable()
	caps.ProcessLoopback = probed && processLoopbackImplemented

	// 两种失败原因必须能被**同时**区分并表达，否则用户升级系统后会撞上
	// 「界面说版本不够、实际是代码没实现」的诊断遮蔽。
	switch {
	case !probed:
		caps.Reason = fmt.Sprintf(
			"本机实测不支持进程级捕获：%s。当前系统 Build %s，官方文档标注的最低要求是 Build %d；"+
				"当前仅支持设备回环（会包含全部应用声音）",
			note, windowsBuildLabel(), minProcessLoopbackBuild)
	case !processLoopbackImplemented:
		caps.Reason = fmt.Sprintf(
			"本机实测支持进程级捕获（Build %s，探测通过），但本版本尚未实现该能力；暂时只能使用设备回环",
			windowsBuildLabel())
	}
	// 探测通过且已实现时不写 Reason：支持就不需要解释。
	return caps
}

// currentWindowsBuild 读取 Windows Build 号（如 19045）。
//
// 为什么读注册表而不是 RtlGetVersion：注册表不需要 syscall 手写结构体，
// 且 CurrentBuildNumber 与 winver / 微软文档里的 Build 号口径一致。
func currentWindowsBuild() (int, error) {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return 0, err
	}
	defer key.Close()

	// CurrentBuildNumber 是 Build 号（REG_SZ）；CurrentBuild 是较新的别名，作兜底。
	for _, name := range []string{"CurrentBuildNumber", "CurrentBuild"} {
		raw, _, getErr := key.GetStringValue(name)
		if getErr != nil {
			continue
		}
		build, convErr := strconv.Atoi(strings.TrimSpace(raw))
		if convErr != nil {
			continue
		}
		return build, nil
	}
	return 0, fmt.Errorf("注册表中没有可解析的 CurrentBuildNumber")
}

// windowsBuildLabel 返回用于错误文案的版本描述，读取失败时给「未知」。
//
// 注意：Build 号现在只用于**解释**，不参与能力判定。
func windowsBuildLabel() string {
	build, err := currentWindowsBuild()
	if err != nil {
		return "未知"
	}
	return strconv.Itoa(build)
}
