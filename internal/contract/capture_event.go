package contract

// CaptureSourcesChanged 是 EV-12 capture:sources 的载荷（应用启动/退出、设备增减）。
type CaptureSourcesChanged struct {
	Sources []Source `json:"sources"`
}

// CaptureInvalid 是 EV-13 capture:invalid 的载荷（源失效 / 重连中，UC-03）。
type CaptureInvalid struct {
	SourceID string `json:"sourceId"`
	// Reason 原因键：app-exited | device-lost | device-switched | permission-denied。
	Reason string `json:"reason"`
	// Recovering true = 正在重连（UI 静默），false = 已放弃（需提示用户）。
	Recovering bool `json:"recovering"`
}

// 源失效原因键。
const (
	CaptureReasonAppExited       = "app-exited"
	CaptureReasonDeviceLost      = "device-lost"
	CaptureReasonDeviceSwitched  = "device-switched"
	CaptureReasonPermissionDenied = "permission-denied"
)
