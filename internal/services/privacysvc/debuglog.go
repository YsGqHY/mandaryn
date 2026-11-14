package privacysvc

// 调试模式与调试日志（API-10.8 ~ 10.11）。开关本身由 appsettings.SetDebugMode 负责（API-9.16），
// 这里只做状态查询、查看器读取、清空与前端上报。规则在 internal/privacy，机制在 logx。

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/privacy"
	"vip.oberon.mandaryn/internal/utils/logx"
)

const (
	// frontendMaxPerSecond 后端对前端上报的限流（前端另有每分钟 60 条的限流与去重）。
	frontendMaxPerSecond = 20
	// 前端上报字段的截断上限（脱敏器还会再按字段规则截断）。
	frontendMessageMax = 500
	frontendStackMax   = 2000
)

// frontendLimiter 是按秒计数的简单限流器；超限丢弃并在下一秒补记一条丢弃数。
type frontendLimiter struct {
	mu      sync.Mutex
	second  int64
	count   int
	dropped int
}

// allow 返回本条是否放行，以及上一秒被丢弃的条数（>0 时调用方补记一条汇总）。
func (l *frontendLimiter) allow(now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sec := now.Unix()
	carried := 0
	if sec != l.second {
		carried = l.dropped
		l.second, l.count, l.dropped = sec, 0, 0
	}
	if l.count >= frontendMaxPerSecond {
		l.dropped++
		return false, carried
	}
	l.count++
	return true, carried
}

func (s *Service) debugState() contract.DebugModeState {
	if s.debug == nil {
		return contract.DebugModeState{Enabled: logx.DebugEnabled(), Source: contract.DebugModeSourceUser}
	}
	return s.debug.State()
}

// GetDebugLogStatus 返回调试模式状态与调试日志文件信息（API-10.8）。
func (s *Service) GetDebugLogStatus(ctx context.Context) (contract.DebugLogStatus, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.DebugLogStatus{}, err
	}
	st, err := privacy.DebugLogStatusFor(s.debugState())
	if err != nil {
		return st, apierr.Wrap(err, apierr.CodePathUnwritable)
	}
	return st, nil
}

// ReadDebugLog 按条件读取调试日志，新的在前（API-10.9）。调试模式关闭时同样可读。
//
// 可能错误：ERR_INVALID_ARGUMENT（minLevel / origin 非法）。
func (s *Service) ReadDebugLog(ctx context.Context, q contract.DebugLogQuery) (contract.DebugLogPage, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.DebugLogPage{}, err
	}
	return privacy.ReadDebugLog(logx.DebugLogDir(), q)
}

// ClearDebugLog 删除全部调试日志文件，返回清空后的状态（API-10.10）。
//
// 开启状态下清空后继续写入新文件；只删调试日志，不碰 app.log。
func (s *Service) ClearDebugLog(ctx context.Context) (contract.DebugLogStatus, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.DebugLogStatus{}, err
	}
	if err := logx.ClearDebugLogs(); err != nil {
		return contract.DebugLogStatus{}, apierr.Wrap(err, apierr.CodePathUnwritable)
	}
	logx.For("privacy").Info("调试日志已清空")
	return s.GetDebugLogStatus(ctx)
}

// ReportFrontendLog 记录一条前端错误 / 警告（API-10.11）。
//
// 调试模式关闭时直接返回（前端在关闭状态下本就不上报，这里是兜底）；
// 只写调试日志，不进 app.log。正文类内容由调试日志的脱敏器按字段规则处理。
func (s *Service) ReportFrontendLog(ctx context.Context, e contract.FrontendLogEntry) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if !logx.DebugEnabled() {
		return nil
	}
	ok, carried := s.feLimit.allow(s.nowFn())
	l := logx.DebugOnly("frontend", logx.OriginFrontend)
	if carried > 0 {
		l.Warn("前端日志超出限流，已丢弃", "dropped", carried, "limitPerSecond", frontendMaxPerSecond)
	}
	if !ok {
		return nil
	}

	level := slog.LevelWarn
	if strings.EqualFold(strings.TrimSpace(e.Level), "error") {
		level = slog.LevelError
	}
	kind := strings.ToLower(strings.TrimSpace(e.Kind))
	msg := "前端控制台输出"
	switch kind {
	case "error":
		msg = "前端未捕获异常"
	case "rejection":
		msg = "前端未处理的 Promise 拒绝"
	default:
		kind = "console"
	}
	l.Log(ctx, level, msg,
		"message", clip(e.Message, frontendMessageMax),
		"stack", clip(e.Stack, frontendStackMax),
		"route", clip(e.Route, 64),
		"window", clip(e.Window, 16),
		"kind", kind,
	)
	return nil
}

// clip 按字节截断（保证 UTF-8 边界），空白直接丢弃。
func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
