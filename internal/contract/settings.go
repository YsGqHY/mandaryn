package contract

// ProxyConfig 代理配置（API-9.3，DOC-03 §2.9.1）。
//
// 契约约束：
//   - Type=none 时其余字段必须为空，后端忽略；
//   - https 表示「以 TLS 连接代理」（CONNECT over TLS），与目标是否 https 无关；
//   - 凭据仅用于 Proxy-Authorization，禁止写入 URL query 或日志。
type ProxyConfig struct {
	// Type none | http | https | socks5。
	Type string `json:"type"`
	Host string `json:"host"`
	Port int `json:"port"`
	Username string `json:"username"`
	// Password 读取时掩码；写入时传明文（后端加密落库）。
	Password string `json:"password"`
	// UseForDownload 默认 true。
	UseForDownload bool `json:"useForDownload"`
	// UseForOnline 默认 true。
	UseForOnline bool `json:"useForOnline"`
	NoProxy string `json:"noProxy"`
}

// 代理类型取值。
const (
	ProxyNone   = "none"
	ProxyHTTP   = "http"
	ProxyHTTPS  = "https"
	ProxySOCKS5 = "socks5"
)

// AudioSettings 音频与录制设置（API-9.6，UX-SPEC §3.2）。
type AudioSettings struct {
	// SaveFormat wav | flac。
	SaveFormat string `json:"saveFormat"`
	// Sensitivity standard | noisy（替代旧 VAD 阈值滑块，UX-SPEC §4）。
	Sensitivity string `json:"sensitivity"`
	// MaxSpeakers 0 = auto，2–8。
	MaxSpeakers int `json:"maxSpeakers"`
	DefaultSourceID string `json:"defaultSourceId"`
	// AutoName 会话自动命名。
	AutoName bool `json:"autoName"`
}

// PrivacySettings 隐私设置（API-9.7）。
type PrivacySettings struct {
	// NoAudioPersist 在线模式内存直传、不落盘（P1）。
	NoAudioPersist bool `json:"noAudioPersist"`
	// LogLevel info | debug | off。
	LogLevel string `json:"logLevel"`
}

// OutboundEntry 出站记录（API-9.8 / DOC-04 TBL-12）。
type OutboundEntry struct {
	// At 毫秒时间戳。
	At int64 `json:"at"`
	// Host 目标域名（**不存完整 URL**，避免 query 泄露）。
	Host string `json:"host"`
	// Kind model-download | online-asr。
	Kind string `json:"kind"`
	// Audio 是否包含音频。
	Audio bool `json:"audio"`
	Bytes int64 `json:"bytes"`
	// Result ok 或错误码。
	Result string `json:"result"`
}

// 出站类型取值。
const (
	OutboundModelDownload = "model-download"
	OutboundOnlineASR     = "online-asr"
	// OutboundUpdateCheck 「检查更新」查版本清单。它同样受离线闸门约束并落 outbound_log ——
	// 用户有权知道应用何时联过网（FR-08）。
	OutboundUpdateCheck = "update-check"
)
