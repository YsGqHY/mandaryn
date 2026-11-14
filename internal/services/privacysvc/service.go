// Package privacysvc 是隐私边界（FR-08）的 Wails 薄壳（L3）。
//
// 职责边界（与 CLAUDE.md「领域逻辑不进 service」一致）：
//   - 只做类型转换、错误码映射、依赖装配；
//   - 判定 / 脱敏 / 自检逻辑全部在 internal/privacy（L2，不依赖 Wails）；
//   - 诊断包有两种出口：`ExportDiagnostics` 回字节（前端自取自用）、
//     `WriteDiagnostics` 原子落盘到用户经原生「另存为」选定的路径。
//
// 不碰音频：本包只读写 outbound_log 与偏好键，不打开、不扫描音频目录。
package privacysvc

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/privacy"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/filex"
	"vip.oberon.mandaryn/internal/utils/logx"

	"gorm.io/gorm"
)

// OfflinePrefKey 是用户离线开关在 preferences 表里的键。
//
// 说明：app_settings 目前没有 offline 列（DOC-04 TBL-02 未登记该字段），
// 因此用户层开关沿用开放键值表，键名与 systray.enabled 同类风格。
// 详见 docs/DEV-ISSUES-S9a.md（S9A-02）。
const OfflinePrefKey = "privacy.offline"

// logFileName 是诊断包要附带的日志文件名（与 logx 默认一致）。
const logFileName = "app.log"

// Deps 是构造依赖。
type Deps struct {
	// Holder 持久化句柄（必填）。
	Holder *storage.Holder
	// Gate 离线闸门；nil 时用「环境变量 + preferences 用户开关」自建。
	Gate *privacy.Gate
	// Version / Build 进诊断包。
	Version string
	Build   string
	// Modules 返回已装配模块清单（可选；nil 时诊断包不含模块清单）。
	Modules func() []string
	// ErrorCounts 返回错误码计数（可选；nil 时由 outbound_log 的 result 列推导）。
	ErrorCounts func() map[string]int
	// LogDir 返回日志目录（可选；空时诊断包不含日志副本）。
	LogDir func() string
	// Debug 调试模式裁决（可选；nil 时状态只反映 logx 当前是否在写）。
	Debug *privacy.DebugMode
	// Now 时间注入点（测试用）；nil 用 time.Now。
	Now func() time.Time
}

// Service 是 privacysvc 的 Wails 服务。
type Service struct {
	holder *storage.Holder
	gate   *privacy.Gate
	rec    *privacy.Recorder
	aud    *privacy.Auditor
	pipe   *privacy.Pipe

	version     string
	build       string
	modules     func() []string
	errorCounts func() map[string]int
	logDir      func() string
	now         func() time.Time

	debug   *privacy.DebugMode
	feLimit frontendLimiter
}

// New 装配服务。Holder 缺失直接报错（不允许半残服务）。
func New(deps Deps) (*Service, error) {
	if deps.Holder == nil {
		return nil, apierr.New(apierr.CodeDBWrite).WithDetail("privacysvc: storage holder is required")
	}
	s := &Service{
		holder:      deps.Holder,
		version:     deps.Version,
		build:       deps.Build,
		modules:     deps.Modules,
		errorCounts: deps.ErrorCounts,
		logDir:      deps.LogDir,
		now:         deps.Now,
		debug:       deps.Debug,
	}
	s.gate = deps.Gate
	if s.gate == nil {
		s.gate = privacy.NewGate(privacy.GateOptions{User: s.userOffline})
	}
	// 把用户层离线开关注册到 internal/outbound 的全局闸门：
	// 既有出站调用方（asr / models / enginesvc）只调 outbound.Guard，
	// 这样它们无需改动就遵守「设置页的离线开关」。
	s.gate.InstallGlobal()
	s.rec = privacy.NewRecorder(deps.Holder)
	s.aud = privacy.NewAuditor(s.gate, s.rec)
	s.pipe = privacy.NewPipe(s.gate, s.rec)
	return s, nil
}

func (s *Service) db() *gorm.DB {
	if s.holder == nil {
		return nil
	}
	db := s.holder.Current()
	if db == nil {
		return nil
	}
	return db.GORM
}

func (s *Service) nowFn() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// userOffline 读用户离线开关；读不到（无库 / 无行）按关闭处理。
//
// 刻意**不返回错误**：闸门调用方只需要一个布尔，读库失败时按「用户没开」处理
// 会让离线保护失效，所以这里失败也按关闭走，但会打一条 warn 留痕。
func (s *Service) userOffline() bool {
	db := s.db()
	if db == nil {
		return false
	}
	var row storage.Preference
	err := db.First(&row, "key = ?", OfflinePrefKey).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			logx.For("privacy").Warn("read offline preference failed", "error", err.Error())
		}
		return false
	}
	return parseBool(row.Value)
}

// parseBool 解析 preferences 里的布尔值（JSON true / "true" / 1 都算开）。
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "\"true\"", "on", "yes":
		return true
	default:
		return false
	}
}

// GetStatus 返回离线状态与最近 24h 出站汇总（设置页启动时拉一次）。
func (s *Service) GetStatus(ctx context.Context) (contract.PrivacyStatus, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.PrivacyStatus{}, err
	}
	st := s.gate.State()
	from, to := privacy.Window(s.nowFn())
	stats, err := s.rec.Stats(from, to)
	if err != nil {
		return contract.PrivacyStatus{}, err
	}
	return contract.PrivacyStatus{
		Offline:        st.Offline,
		OfflineSource:  string(st.Source),
		OfflineEnvVar:  st.EnvVar,
		EnvOffline:     st.Env,
		UserOffline:    st.User,
		WindowStartMs:  from.UnixMilli(),
		WindowEndMs:    to.UnixMilli(),
		RecentCount:    stats.Total,
		BlockedCount:   stats.Blocked,
		RealCount:      stats.Real,
		PendingCount:   stats.Pending,
		LastOutboundAt: stats.LastAt,
		RetentionHours: int(privacy.Retention.Hours()),
	}, nil
}

// SetOffline 切换用户层离线开关（FR-08 离线开关）。
//
// 环境变量层的接管无法在此解除：env 开启时 Offline 仍是 true，
// 返回值的 OfflineSource 会说明「env」或「env+user」。
func (s *Service) SetOffline(ctx context.Context, enabled bool) (contract.PrivacyStatus, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.PrivacyStatus{}, err
	}
	db := s.db()
	if db == nil {
		return contract.PrivacyStatus{}, apierr.New(apierr.CodeDBWrite).WithDetail("database unavailable")
	}
	value := "false"
	if enabled {
		value = "true"
	}
	if err := db.WithContext(ctx).Save(&storage.Preference{Key: OfflinePrefKey, Value: value}).Error; err != nil {
		return contract.PrivacyStatus{}, apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("save offline preference failed")
	}
	// 只写开关状态，不写任何内容字段（NFR-03）。
	logx.For("privacy").Info("offline switch changed", "enabled", enabled)
	return s.GetStatus(ctx)
}

// ListOutbound 分页读取最近出站记录（新的在前）。
func (s *Service) ListOutbound(ctx context.Context, limit, offset int) (contract.OutboundPage, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.OutboundPage{}, err
	}
	records, total, err := s.rec.List(privacy.ListOptions{Limit: limit, Offset: offset})
	if err != nil {
		return contract.OutboundPage{}, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return contract.OutboundPage{Records: records, Total: total, Offset: offset, Limit: limit}, nil
}

// ClearOutbound 清空出站记录（用户主动「清空留痕」）。
func (s *Service) ClearOutbound(ctx context.Context) (contract.ClearResult, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.ClearResult{}, err
	}
	deleted, err := s.rec.Clear()
	if err != nil {
		return contract.ClearResult{}, err
	}
	logx.For("privacy").Info("outbound log cleared", "deleted", deleted)
	return contract.ClearResult{Deleted: deleted}, nil
}

// RunAudit 执行零出站自检（FR-08：支撑「近期无外连」的自证）。
func (s *Service) RunAudit(ctx context.Context, filter contract.AuditFilter) (contract.AuditReport, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.AuditReport{}, err
	}
	report, err := s.aud.Run(ctx, filter)
	if err != nil {
		return contract.AuditReport{}, err
	}
	logx.For("privacy").Info("zero-outbound audit finished",
		"offline", report.Offline, "scanned", report.Scanned,
		"zeroOutbound", report.ZeroOutbound, "violations", len(report.Violations))
	return report, nil
}

// ExportDiagnostics 生成脱敏诊断包，字节回给前端另存。
//
// 不含音频 / 正文 / 声纹向量 / 凭据；产物内部还有一层自检，
// 自检不过则返回 ERR_STATE_FORBIDDEN（宁可没有诊断包也不泄漏）。
func (s *Service) ExportDiagnostics(ctx context.Context) (contract.DiagnosticsArtifact, error) {
	if err := ctxErr(ctx); err != nil {
		return contract.DiagnosticsArtifact{}, err
	}
	now := s.nowFn()
	from, to := privacy.Window(now)

	rows, err := s.rec.Scan(from, to, nil, 0)
	if err != nil {
		return contract.DiagnosticsArtifact{}, err
	}

	in := privacy.BundleInput{
		AppVersion:  s.version,
		Build:       s.build,
		Platform:    runtime.GOOS,
		Arch:        runtime.GOARCH,
		GoVersion:   runtime.Version(),
		Outbound:    rows,
		ErrorCounts: s.errorCountAll(rows),
		Now:         now,
	}
	st := s.gate.State()
	in.Offline = st.Offline
	in.OfflineSource = string(st.Source)
	in.OfflineEnvVar = st.EnvVar
	if s.modules != nil {
		in.Modules = s.modules()
	}
	if s.logDir != nil {
		if dir := strings.TrimSpace(s.logDir()); dir != "" {
			lines, err := privacy.ReadLogTail(logPath(dir), 0)
			if err != nil {
				logx.For("privacy").Warn("read log tail failed", "error", err.Error())
			} else {
				in.LogLines = lines
			}
			// 调试日志（开过调试模式才有）：同样只取尾部，进包前统一脱敏。
			in.DebugLogLines = privacy.ReadDebugLogTail(dir, 0)
		}
	}

	art, err := privacy.BuildBundle(in)
	if err != nil {
		logx.For("privacy").Error("diagnostics redaction self-check failed", "error", err.Error())
		return contract.DiagnosticsArtifact{}, apierr.New(apierr.CodeStateForbidden).
			WithAction("view-diagnostics").
			WithDetail("diagnostics redaction self-check failed; bundle withheld")
	}
	logx.For("privacy").Info("diagnostics bundle built",
		"bytes", len(art.Bytes), "redactions", art.Redactions, "dropped", art.Dropped)
	return art, nil
}

// WriteDiagnostics 生成脱敏诊断包并**写到用户选定的路径**（API：诊断包另存）。
//
// 为什么不复用 exportsvc：那套服务只认 CSV/XLSX 表格（`WriteCSV` 强制 `.csv` 且加 BOM），
// 诊断包是已脱敏的 JSON 字节，语义不同。这里仍走 `filex.WriteAtomic`（原子落盘），
// 不用 `os.WriteFile`（工具层铁律）。
//
// 返回落到磁盘的绝对路径。脱敏自检不过时**不落盘**，直接返回 ERR_STATE_FORBIDDEN。
func (s *Service) WriteDiagnostics(ctx context.Context, targetPath string) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	target := strings.TrimSpace(targetPath)
	if target == "" {
		return "", apierr.New(apierr.CodeInvalidArgument).WithDetail("targetPath 不能为空")
	}
	art, err := s.ExportDiagnostics(ctx)
	if err != nil {
		return "", err
	}
	if err := filex.WriteAtomic(target, art.Bytes, 0o644); err != nil {
		return "", apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("诊断包写入失败")
	}
	logx.For("privacy").Info("diagnostics bundle written",
		"bytes", len(art.Bytes), "redactions", art.Redactions, "dropped", art.Dropped)
	return target, nil
}

// errorCountAll 汇总错误码计数：优先用注入的统计，否则从出站记录推导。
func (s *Service) errorCountAll(rows []contract.OutboundRecord) map[string]int {
	if s.errorCounts != nil {
		if counts := s.errorCounts(); counts != nil {
			return counts
		}
	}
	counts := make(map[string]int)
	for _, row := range rows {
		result := strings.TrimSpace(row.Result)
		if !strings.HasPrefix(result, "ERR_") {
			continue
		}
		counts[result]++
	}
	return counts
}

func logPath(dir string) string {
	return filepath.Join(strings.TrimRight(dir, `/\`), logFileName)
}

// ctxErr 统一处理「已取消 / 已超时」。
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return apierr.Wrap(err, apierr.CodeStateForbidden).WithDetail("privacy operation cancelled")
	}
	return nil
}
