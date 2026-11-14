//go:build linux

package capture

// 本文件是 Linux 的**诚实桩**：不假装支持，也不留未实现的占位实现。
//
// 返回 ERR_CAPTURE_UNAVAILABLE 而不是空列表的理由见 capture_darwin.go 顶部注释
// （同一套判断：明确「未实现」比让 UI 显示「没有声卡」更诚实）。
//
// 待实现路线（ARCH §4.1.2）：
//   - 应用级：PipeWire 0.3.60+ 的 node 级捕获（按 node 分流）；
//   - 设备级：PulseAudio / PipeWire 的 monitor 源（整机混音，会含全部应用声音）；
//     这条路径系统层面本来就有，本版本未实现属于代码缺口，不是平台限制。

// enumerateUnavailableReason Linux 枚举不可用的原因文案。
func enumerateUnavailableReason() string {
	return "Linux 音频源枚举尚未实现：按应用分路捕获需要 PipeWire 0.3.60 及以上的 node 级捕获，" +
		"设备回环需要 PulseAudio/PipeWire 的 monitor 源（会包含全部应用声音）。当前版本无法枚举音频源。"
}

// probeUnavailableReason Linux 探测不可用的原因文案。
func probeUnavailableReason(kind string) string {
	if kind == "application" {
		return "Linux 应用级捕获尚未实现（需要 PipeWire 0.3.60 及以上的 node 级捕获），无法探测该应用源。"
	}
	return "Linux 设备回环捕获尚未实现（需要 PulseAudio/PipeWire 的 monitor 源），无法探测该设备源。"
}

// openUnavailableReason Linux 打开通道不可用的原因文案。
func openUnavailableReason(kind string) string {
	if kind == "application" {
		return "Linux 应用级捕获尚未实现：需要 PipeWire 0.3.60 及以上的 node 级捕获。"
	}
	return "Linux 设备回环捕获尚未实现：需要 PulseAudio/PipeWire 的 monitor 源（会包含全部应用声音）。"
}
