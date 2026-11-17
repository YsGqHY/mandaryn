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

	// 三种失败原因必须能被**同时**区分并表达：
	//   - 本机真的不支持（升级系统才有用）；
	//   - 本版本没实现（升级应用才有用）；
	//   - 音频服务瞬时未就绪（等一会儿就好，别让用户去查版本）。
	// 混在一起会让用户沿着错的方向折腾（S1-07 教训）。
	switch {
	case !processLoopbackImplemented && probed:
		caps.Reason = fmt.Sprintf(
			"本机实测支持进程级捕获（Build %s，探测通过），但本版本尚未实现该能力；暂时只能使用设备回环",
			windowsBuildLabel())
	case !probed && isTransientProbeFailure(note):
		// 瞬时原因不写「不支持」，也不提版本要求 —— 那是误导。
		caps.Reason = fmt.Sprintf(
			"进程级捕获暂时不可用：%s；当前只能使用设备回环（会包含全部应用声音），稍后会自动重试",
			note)
	case !probed:
		caps.Reason = fmt.Sprintf(
			"本机实测不支持进程级捕获：%s。当前系统 Build %s，官方文档标注的最低要求是 Build %d；"+
				"当前仅支持设备回环（会包含全部应用声音）",
			note, windowsBuildLabel(), minProcessLoopbackBuild)
	}
	// 探测通过且已实现时不写 Reason：支持就不需要解释。
	return caps
}

// isTransientProbeFailure 判断探测失败是否属于「等一会儿可能就好」的瞬时原因。
//
// 目前只有一类：音频服务（AudioSrv）未就绪。这类失败**不该**被表述成
// 「系统版本不支持」—— 那会把一个可自愈的状态说成硬性限制，诱导用户去升级系统。
func isTransientProbeFailure(note string) bool {
	return strings.Contains(note, "AudioSrv") || strings.Contains(note, "音频服务")
}

// WarnProcessLoopbackUnavailable 是「本机不支持进程级捕获」的源警告键。
//
// 用键而不是整句文案：人类可见文本必须由前端 i18n 渲染（NFR-01 / DOC-05）。
const WarnProcessLoopbackUnavailable = "process-loopback-unavailable"

// processLoopbackCapable 是 capabilities() 里 ProcessLoopback 的同一判据，
// 供枚举 / 探测路径复用，避免两处各写一遍「探测通过 && 已实现」。
func processLoopbackCapable() bool {
	probed, _ := processLoopbackAvailable()
	return probed && processLoopbackImplemented
}

// gateAppSource 把「本机不支持进程级捕获」这一事实**落到应用源上**。
//
// 为什么必须这么做（FR-02-4 的诚实声明原则）：能力探测为 false 时，
// 若枚举仍把应用源列为 Present=true，用户会看到一堆可点的源、点下去必然
// 在 Open 阶段失败 —— 这与 caps_windows.go 顶部「不要引导用户选一个必然
// 失败的源」是同一条约束的另一半。能力缺失是**源本身不可用**，所以置
// Present=false 并带上 Warn 键，让 UI 直接禁用并说明原因。
//
// 只处理应用源：设备回环与进程级能力无关，不能被这条门控牵连。
func gateAppSource(src *contract.Source, capable bool) {
	if src == nil || capable || src.Kind != contract.SourceKindApplication {
		return
	}
	src.Present = false
	src.Warn = WarnProcessLoopbackUnavailable
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
