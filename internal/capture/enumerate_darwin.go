//go:build darwin

package capture

import (
	"vip.oberon.mandaryn/internal/contract"
)

// enumerate 桩：macOS 尚未实现音频源枚举。
//
// 返回 ERR_CAPTURE_UNAVAILABLE 而不是空列表，理由见 capture_darwin.go 顶部注释。
func enumerate() ([]contract.Source, error) {
	return nil, sourceUnavailable(enumerateUnavailableReason())
}

// probe 桩：macOS 尚未实现单源探测。
func probe(kind, _ string) (contract.Source, error) {
	return contract.Source{}, sourceUnavailable(probeUnavailableReason(kind))
}
