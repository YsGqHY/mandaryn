//go:build linux

package capture

import (
	"vip.oberon.mandaryn/internal/contract"
)

// enumerate 桩：Linux 尚未实现音频源枚举。
func enumerate() ([]contract.Source, error) {
	return nil, sourceUnavailable(enumerateUnavailableReason())
}

// probe 桩：Linux 尚未实现单源探测。
func probe(kind, _ string) (contract.Source, error) {
	return contract.Source{}, sourceUnavailable(probeUnavailableReason(kind))
}

// open 桩：Linux 尚未实现捕获通道。
//
// 同样不返回假通道：假通道会让上层录出静音文件，比明确失败更糟。
func open(kind, _ string) (Channel, error) {
	return nil, sourceUnavailable(openUnavailableReason(kind))
}
