// Package apierr 定义跨前后端的错误契约（DOC-03 §1.4 / §5）。
//
// 设计要点：
//   - L2 领域层返回 *apierr.Error（带 ERR_* 码 + 上下文），不 panic；
//   - L3 薄壳把这个错误**原样**返回，Wails 通过 ServiceOptions.MarshalError
//     调 Marshal 序列化成 JSON，前端 Promise reject 收到
//     { code, message, action, detail, retryable } 结构体；
//   - 前端按 code 映射 i18n 文案 + 「下一步动作」按钮（action 键）。
//
// 铁律：错误码只在此处登记（DOC-03 §5 全表），业务代码禁止自造字符串错误码，
// 新增码必须同步 DOC-03 §5 与前端 i18n 的 errors.<code> 文案。
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
)

// 错误码（DOC-03 §5）。顺序与文档表格保持一致，便于逐条核对。
const (
	CodeInvalidArgument = "ERR_INVALID_ARGUMENT"
	CodeNotFound        = "ERR_NOT_FOUND"
	CodeStateForbidden  = "ERR_STATE_FORBIDDEN"

	CodeCaptureUnavailable = "ERR_CAPTURE_UNAVAILABLE"
	CodeCapturePermission  = "ERR_CAPTURE_PERMISSION"
	CodeCaptureDeviceLost  = "ERR_CAPTURE_DEVICE_LOST"
	CodeSourceNotFound     = "ERR_SOURCE_NOT_FOUND"

	CodeEngineNotReady         = "ERR_ENGINE_NOT_READY"
	CodeEngineLoad             = "ERR_ENGINE_LOAD"
	CodeEngineOnlineNetwork    = "ERR_ENGINE_ONLINE_NETWORK"
	CodeEngineOnlineTimeout    = "ERR_ENGINE_ONLINE_TIMEOUT"
	CodeEngineOnlineAuth       = "ERR_ENGINE_ONLINE_AUTH"
	CodeEngineOnlineRateLimit  = "ERR_ENGINE_ONLINE_RATE_LIMIT"
	CodeEngineOnlineRejected   = "ERR_ENGINE_ONLINE_REJECTED"
	CodeEngineOnlineServer     = "ERR_ENGINE_ONLINE_SERVER"
	CodeLocalUnavailable       = "ERR_LOCAL_UNAVAILABLE"

	CodeModelNotFound = "ERR_MODEL_NOT_FOUND"
	CodeModelCorrupt  = "ERR_MODEL_CORRUPT"
	CodeModelBusy     = "ERR_MODEL_BUSY"

	CodeDownloadNetwork  = "ERR_DOWNLOAD_NETWORK"
	CodeDownloadChecksum = "ERR_DOWNLOAD_CHECKSUM"
	CodeDownloadDiskFull = "ERR_DOWNLOAD_DISK_FULL"
	CodeDownloadProxy    = "ERR_DOWNLOAD_PROXY"

	CodeMigratePrecheck = "ERR_MIGRATE_PRECHECK"
	CodeMigrateVerify   = "ERR_MIGRATE_VERIFY"
	CodeMigrateSwitch   = "ERR_MIGRATE_SWITCH"

	CodeSettingsConsentRequired = "ERR_SETTINGS_CONSENT_REQUIRED"

	CodeHotkeyConflict = "ERR_HOTKEY_CONFLICT"
	CodeHotkeyRegister = "ERR_HOTKEY_REGISTER"

	CodeDiskFull = "ERR_DISK_FULL"
	CodeDBWrite  = "ERR_DB_WRITE"

	CodePerfCapRange  = "ERR_PERF_CAP_RANGE"
	CodePerfSuspended = "ERR_PERF_SUSPENDED"

	CodePathUnwritable = "ERR_PATH_UNWRITABLE"

	// CodeOfflineEnforced 是相对 DOC-03 §5 的补充码（FR-08 离线开关 /
	// NFR-03-1）：全局离线开关开启时，任何出站请求一律被拦截并返回此码。
	// 落文档时同步 DOC-03 §5。
	CodeOfflineEnforced = "ERR_OFFLINE_ENFORCED"
)

// Err 是跨前后端的结构化错误。
//
// 它同时实现 error（Go 侧）与 JSON 序列化（前端侧），
// 因此可以直接作为 Service 方法返回值交给 Wails。
type Err struct {
	// Code ERR_* 错误码，见上方常量。
	Code string `json:"code"`
	// Message 面向开发者的英文短句（不含正文 / 音频 / 凭据，NFR-03-5）。
	Message string `json:"message"`
	// Action 面向用户的下一步动作键，前端映射 i18n 文案 + 按钮。
	Action string `json:"action,omitempty"`
	// Detail 诊断信息（已脱敏），仅用于「查看诊断」。
	Detail string `json:"detail,omitempty"`
	// Retryable 是否值得让用户点「重试」。
	Retryable bool `json:"retryable"`
	// cause 保留原始 error 供 errors.Is / Unwrap，不进入 JSON。
	cause error
}

// Error 实现 error。前端拿到的 JSON 由 Marshal 负责，这里保持可读文本便于日志。
func (e *Err) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// Unwrap 支持 errors.Is / errors.As 追到根因。
func (e *Err) Unwrap() error { return e.cause }

// WithDetail 附加脱敏诊断信息（链式，返回自身副本）。
func (e *Err) WithDetail(format string, args ...any) *Err {
	clone := *e
	clone.Detail = fmt.Sprintf(format, args...)
	return &clone
}

// WithCause 挂上根因（不进入 JSON，但保留在日志与 errors.Is 链里）。
func (e *Err) WithCause(err error) *Err {
	clone := *e
	clone.cause = err
	return &clone
}

// WithAction 覆盖默认动作键（同一错误码在不同场景下可选不同动作）。
func (e *Err) WithAction(action string) *Err {
	clone := *e
	clone.Action = action
	return &clone
}

// New 按错误码构造错误，使用码表里的默认 retryable / action / message。
func New(code string) *Err {
	spec := lookup(code)
	return &Err{
		Code:      code,
		Message:   spec.message,
		Action:    spec.action,
		Retryable: spec.retryable,
	}
}

// Newf 同 New，但自定义 message（仍受「不写正文/凭据」约束）。
func Newf(code, format string, args ...any) *Err {
	e := New(code)
	e.Message = fmt.Sprintf(format, args...)
	return e
}

// Wrap 把普通 error 包装成指定错误码的 Err，保留根因。
func Wrap(err error, code string) *Err {
	if err == nil {
		return nil
	}
	e := New(code)
	e.Message = err.Error()
	e.cause = err
	return e
}

// From 把任意 error 归一化为 *Err：
//   - 已是 *Err → 原样返回；
//   - nil → nil；
//   - 其它 → 用 fallbackCode 包装（默认 ERR_INVALID_ARGUMENT 之外的兜底由调用方指定）。
func From(err error, fallbackCode string) *Err {
	if err == nil {
		return nil
	}
	var e *Err
	if errors.As(err, &e) {
		return e
	}
	return Wrap(err, fallbackCode)
}

// Is 让调用方按错误码判断（errors.Is(err, apierr.New(CodeNotFound))）。
func (e *Err) Is(target error) bool {
	var t *Err
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

// HasCode 便捷判断错误是否属于某码。
func HasCode(err error, code string) bool {
	var e *Err
	return errors.As(err, &e) && e.Code == code
}

// Marshal 由 Wails 的 ServiceOptions.MarshalError 调用：
// 把任意 error 序列化为前端可解析的 JSON。
// 返回 nil 表示交回 Wails 默认处理（这里始终返回合法 JSON，避免前端拿到纯字符串）。
func Marshal(err error) []byte {
	if err == nil {
		return nil
	}
	e := From(err, CodeInvalidArgument)
	// 防御：确保必然可序列化。Err 只有基础类型字段，不会失败。
	out, mErr := json.Marshal(e)
	if mErr != nil {
		return []byte(`{"code":"` + CodeInvalidArgument + `","message":"error marshaling failed","retryable":false}`)
	}
	return out
}

// spec 是每个错误码的默认语义（retryable + 用户动作键 + 英文短句）。
type spec struct {
	retryable bool
	action    string
	message   string
}

// table 与 DOC-03 §5 一一对应。Code 未登记时回落到通用参数错误语义。
var table = map[string]spec{
	CodeInvalidArgument: {false, "fix-input", "invalid argument"},
	CodeNotFound:        {false, "refresh-list", "resource not found"},
	CodeStateForbidden:  {false, "stop-recording", "operation not allowed in current state"},

	CodeCaptureUnavailable: {true, "refresh-sources", "capture source unavailable"},
	CodeCapturePermission:  {false, "open-permission", "missing system audio permission"},
	CodeCaptureDeviceLost:  {true, "reselect-source", "capture device lost"},
	CodeSourceNotFound:     {false, "refresh-sources", "source id is no longer valid"},

	CodeEngineNotReady:        {true, "wait-engine", "engine is not ready"},
	CodeEngineLoad:            {true, "switch-model", "engine failed to load"},
	CodeEngineOnlineNetwork:   {true, "check-network", "online endpoint unreachable"},
	CodeEngineOnlineTimeout:   {true, "switch-online", "online endpoint timed out"},
	CodeEngineOnlineAuth:      {false, "update-key", "online endpoint rejected the API key"},
	CodeEngineOnlineRateLimit: {true, "switch-online", "online endpoint rate limited"},
	CodeEngineOnlineRejected:  {false, "change-language", "online endpoint rejected the request"},
	CodeEngineOnlineServer:    {true, "switch-online", "online endpoint returned a server error"},
	CodeLocalUnavailable:      {false, "switch-online", "this build does not include the local engine"},

	CodeModelNotFound: {false, "download-model", "model bundle is not installed"},
	CodeModelCorrupt:  {true, "redownload-model", "model bundle failed verification"},
	CodeModelBusy:     {true, "wait-model-busy", "model store is busy (download or migration in progress)"},

	CodeDownloadNetwork:  {true, "retry", "download failed: network error"},
	CodeDownloadChecksum: {true, "retry", "download failed: checksum mismatch"},
	CodeDownloadDiskFull: {false, "free-space", "download failed: not enough disk space"},
	CodeDownloadProxy:    {true, "check-proxy", "download failed: proxy unavailable"},

	CodeMigratePrecheck: {false, "change-directory", "migration precheck failed"},
	CodeMigrateVerify:   {true, "retry", "migration rolled back: verification failed"},
	CodeMigrateSwitch:   {true, "retry", "migration rolled back: could not switch directory"},

	CodeSettingsConsentRequired: {false, "consent-online", "online privacy consent required"},

	CodeHotkeyConflict: {false, "change-hotkey", "shortcut already in use"},
	CodeHotkeyRegister: {true, "change-hotkey", "shortcut could not be registered"},

	CodeDiskFull: {false, "free-space", "not enough disk space"},
	CodeDBWrite:  {true, "retry", "database write failed"},

	CodePerfCapRange:  {false, "fix-input", "performance cap must be 10-100 in steps of 5"},
	CodePerfSuspended: {true, "raise-cap", "inference is paused: other apps are using the budget"},

	CodePathUnwritable: {false, "change-directory", "path is not writable"},

	CodeOfflineEnforced: {false, "disable-offline", "blocked: offline mode is enabled"},
}

func lookup(code string) spec {
	if s, ok := table[code]; ok {
		return s
	}
	return spec{false, "fix-input", code}
}
