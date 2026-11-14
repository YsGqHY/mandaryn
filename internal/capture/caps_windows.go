//go:build windows

package capture

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"

	"vip.oberon.mandaryn/internal/contract"
)

// minProcessLoopbackBuild 进程级回环捕获的官方最低系统版本。
//
// 依据：微软文档标注 AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK
// 需要 Windows 10 Build 20348 或 Windows 11（ARCH §4.1.2）。
const minProcessLoopbackBuild = 20348

// processLoopbackImplemented Windows 进程级捕获的**代码实现**状态。
//
// 刻意与「系统是否支持」分开表达：
//   - 系统支持 ≠ 本版本已实现。进程级回环必须走 ActivateAudioInterfaceAsync +
//     AUDIOCLIENT_ACTIVATION_PARAMS，go-wca v0.3.0 没有暴露这套异步激活 API，
//     S1 未实现（见 docs/DEV-ISSUES-S1.md）；
//   - 若这里返回 true 而 Open 实际失败，UI 会引导用户选一个必然失败的源，
//     属于欺骗性能力声明。因此 Capabilities 取「系统版本满足 && 已实现」。
//
// 实现进程级捕获时把这里改成 true 即可，其余逻辑无需改动。
const processLoopbackImplemented = false

// capabilities 实现 Capabilities（API-1.6）。
func capabilities() contract.CaptureCaps {
	caps := contract.CaptureCaps{
		Platform: "windows",
		// WASAPI 共享模式回环从 Windows Vista 起就有，Win10 必然可用。
		DeviceLoopback: true,
	}

	build, err := currentWindowsBuild()
	if err != nil {
		caps.Reason = fmt.Sprintf(
			"无法读取系统版本号（%v），按最低能力上报：进程级捕获需要 Windows 10 Build %d 或更高，当前仅支持设备回环（会包含全部应用声音）",
			err, minProcessLoopbackBuild)
		return caps
	}

	systemOK := build >= minProcessLoopbackBuild
	caps.ProcessLoopback = systemOK && processLoopbackImplemented

	switch {
	case !systemOK:
		caps.Reason = fmt.Sprintf(
			"进程级捕获需要 Windows 10 Build %d 或更高（当前 %d），当前系统仅支持设备回环（会包含全部应用声音）",
			minProcessLoopbackBuild, build)
	case !processLoopbackImplemented:
		caps.Reason = fmt.Sprintf(
			"当前系统（Build %d）满足进程级捕获要求，但本版本尚未实现该能力（需要 ActivateAudioInterfaceAsync + AUDIOCLIENT_ACTIVATION_PARAMS），暂时只能使用设备回环",
			build)
	}
	// systemOK && implemented 时不写 Reason：支持就不需要解释。
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
func windowsBuildLabel() string {
	build, err := currentWindowsBuild()
	if err != nil {
		return "未知"
	}
	return strconv.Itoa(build)
}
