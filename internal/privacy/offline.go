// Package privacy 是 FR-08 隐私边界的 L2 领域包，四件事：
//
//  1. 离线闸门（offline.go）：MANDARYN_OFFLINE=1 **或**用户离线开关开启时，
//     在真正发起请求前拒绝一切出站，返回 ERR_OFFLINE_ENFORCED；
//  2. 出站留痕（outbound.go）：统一写 outbound_log，失败也留痕，URL 必须脱敏；
//  3. 零出站自检（audit.go）：给定时间窗与过滤器，产出可判定的结论与明细；
//  4. 诊断包脱敏（diagnostics.go）：产物只保留元数据，正文 / 音频 / 声纹 / 凭据一律剔除。
//
// 铁律（与 ARCHITECTURE §8、CLAUDE.md 产品铁律一致）：
//   - 本包**不 import Wails**，可被单测直接驱动；
//   - 不允许「先发请求再报错」：Check 必须在 httpx 调用之前完成；
//   - 出站记录里**永不**出现完整 URL、query 值、Authorization 头、正文、音频、声纹、凭据；
//   - 日志只写元数据，统一用 logx.For("privacy")。
package privacy

import (
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// Retention 是出站记录的保留时长，与 DOC-04 TBL-12 / internal/outbound 一致（仅最近 24h）。
const Retention = outbound.Retention

// EnvOffline 是全局离线开关的环境变量名（与 internal/outbound 同一来源，避免两套口径）。
const EnvOffline = outbound.EnvOffline

// OfflineSource 表示离线判定的来源层。
type OfflineSource string

const (
	// SourceNone 未开启离线。
	SourceNone OfflineSource = ""
	// SourceEnv 由环境变量 MANDARYN_OFFLINE 开启。
	SourceEnv OfflineSource = "env"
	// SourceUser 由用户设置（设置页离线开关）开启。
	SourceUser OfflineSource = "user"
	// SourceEnvAndUser 两层同时开启。
	SourceEnvAndUser OfflineSource = "env+user"
)

// OfflineState 是离线判定的完整快照（来源可见，便于设置页解释「为什么被拦」）。
type OfflineState struct {
	// Offline 最终是否生效（env 与 user 取或）。
	Offline bool
	// Env 环境变量是否开启。
	Env bool
	// User 用户设置是否开启。
	User bool
	// Source 生效来源；未开启时为 SourceNone。
	Source OfflineSource
	// EnvVar 环境变量名（便于 UI 提示「由 MANDARYN_OFFLINE 接管」）。
	EnvVar string
}

// GateOptions 是 Gate 的依赖注入点；两个函数都是「读一次当前值」，因此
// 用户改设置后无需重建 Gate（与 paths.Resolver 的取值习惯一致）。
type GateOptions struct {
	// Env 报告环境变量是否开启离线；nil 时读 MANDARYN_OFFLINE（与 internal/outbound 同口径）。
	Env func() bool
	// User 报告用户设置是否开启离线；nil 时视为关闭（单测 / 无库场景）。
	User func() bool
}

// Gate 是离线闸门：一个进程一份，注入到所有需要出站的领域包与 L3 服务。
//
// 并发安全：State 只读取注入的两个函数，不加锁；注入的 User 通常读 SQLite，
// 调用频率等于出站频率（很低），不必缓存。
type Gate struct {
	opt GateOptions
}

// NewGate 构造闸门。opt.Env 为 nil 时退化为读环境变量（**只看 env 层**，
// 不含用户在 preferences 里的开关——两层分开取值，来源才不会被重复计数）。
func NewGate(opt GateOptions) *Gate {
	if opt.Env == nil {
		opt.Env = outbound.EnvOfflineEnabled
	}
	return &Gate{opt: opt}
}

// State 返回当前离线判定快照。nil Gate 视为「只看环境变量」。
func (g *Gate) State() OfflineState {
	var st OfflineState
	st.EnvVar = EnvOffline
	if g == nil {
		st.Env = outbound.EnvOfflineEnabled()
		st.Offline = st.Env
		if st.Env {
			st.Source = SourceEnv
		}
		return st
	}
	if g.opt.Env != nil {
		st.Env = g.opt.Env()
	}
	if g.opt.User != nil {
		st.User = g.opt.User()
	}
	switch {
	case st.Env && st.User:
		st.Source = SourceEnvAndUser
	case st.Env:
		st.Source = SourceEnv
	case st.User:
		st.Source = SourceUser
	default:
		st.Source = SourceNone
	}
	st.Offline = st.Env || st.User
	return st
}

// IsOffline 报告当前是否处于离线模式（env 与 user 取或）。
func (g *Gate) IsOffline() bool {
	return g.State().Offline
}

// InstallGlobal 把本闸门的**用户层**读取函数注册到 internal/outbound 的全局闸门，
// 使既有出站调用方（asr / models / enginesvc）无需改动就遵守用户离线开关。
//
// 装配阶段调用一次（privacysvc.New 里调）。传 nil Gate 表示解除注册。
func (g *Gate) InstallGlobal() {
	if g == nil {
		outbound.SetUserOffline(nil)
		return
	}
	outbound.SetUserOffline(g.opt.User)
}

// Check 在发起出站请求**之前**调用。离线时返回 ERR_OFFLINE_ENFORCED，
// 调用方必须立即返回、不得再碰网络。
//
// kind 是业务类型（model-download / online-asr 等），target 是目标 URL；
// 两者只用于诊断明细，写入前都经脱敏（target 只保留 host）。
func (g *Gate) Check(kind, target string) error {
	st := g.State()
	if !st.Offline {
		return nil
	}
	host := HostOf(target)
	logx.For("privacy").Warn("outbound blocked by offline gate",
		"kind", kindLabel(kind), "host", host, "source", string(st.Source))
	return ErrBlocked(kind, target, st.Source)
}

// ErrBlocked 构造离线拒绝错误。detail 只含业务类型与目标 host，
// 不含 query / 凭据 / 正文（NFR-03）。
func ErrBlocked(kind, target string, source OfflineSource) *apierr.Err {
	detail := "blocked " + kindLabel(kind) + " -> " + HostOf(target)
	if source != SourceNone {
		detail += " (offline source: " + string(source) + ")"
	} else {
		detail += " (offline mode enabled)"
	}
	return apierr.New(apierr.CodeOfflineEnforced).
		WithAction("disable-offline").
		WithDetail("%s", detail)
}

// kindLabel 归一化业务类型，用于日志与错误 detail（限制字符集，避免把 URL 塞进来）。
func kindLabel(kind string) string {
	k := normalizeKind(kind)
	if k == "" {
		return "outbound"
	}
	return k
}

// Window 是自检 / 统计默认时间窗（最近 24h，与保留策略一致）。
func Window(now time.Time) (time.Time, time.Time) {
	return now.Add(-Retention), now
}
