package logx

// 调试日志（设置 → 隐私边界 → 调试模式）。
//
// 与 app.log 的关系：app.log 照常按 Info 级写；调试模式开启时，**另外**把所有级别
// （DEBUG 起）的记录写进同目录的 debug.log，关闭即停写并释放文件句柄。
//
// 存储与保留（docs/DEPLOYMENT.md §5.1）：
//   - 位置：<dataRoot>/logs/debug.log，滚动文件 debug.log.1 … debug.log.4（数字越大越旧）；
//   - 格式：JSON Lines（一行一条 slog JSON），固定字段 time / level / source / msg /
//     component / origin，其余为上下文字段；
//   - 单文件 10MB 滚动，最多 5 个文件（约 50MB）；启动与每次滚动时删除超过 7 天的文件。
//
// 脱敏：所有属性都经 SetDebugSanitizer 注入的函数处理（规则归 internal/privacy，
// 与诊断包同源）。未注入时 SetDebugEnabled(true) 直接报错 —— 宁可不记，不能裸记。
//
// 并发与锁：写入路径是 slog handler → debugSink.Write（持 s.mu）→ fileRotator.Write
// （持 rotator.mu）。持 s.mu 期间**禁止打日志**（会递归进 Write 造成死锁）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DebugFileName 调试日志文件名（滚动文件在其后加 .1 … .N）。
	DebugFileName = "debug.log"
	// DebugMaxFileBytes 单个调试日志文件的滚动阈值。
	DebugMaxFileBytes int64 = 10 << 20
	// DebugMaxFiles 调试日志最多保留的文件数（含当前文件）。
	DebugMaxFiles = 5
	// DebugMaxAge 调试日志文件的最长保留时间（按修改时间）。
	DebugMaxAge = 7 * 24 * time.Hour

	// 调试日志里标记来源的字段（backend = Go 侧日志，frontend = 前端上报）。
	OriginKey      = "origin"
	OriginBackend  = "backend"
	OriginFrontend = "frontend"
)

// DebugFile 是一个调试日志文件的元数据（设置页展示占用用）。
type DebugFile struct {
	Name       string
	SizeBytes  int64
	ModifiedAt time.Time
}

// Sanitizer 是调试日志的属性脱敏函数，签名与 slog.HandlerOptions.ReplaceAttr 一致。
// 内置键 time / level / msg 也会传进来（source 已由本包改写成相对路径）。
type Sanitizer func(groups []string, a slog.Attr) slog.Attr

var (
	errDebugNoSanitizer = errors.New("logx: debug log sanitizer is not set")
	errDebugNotInit     = errors.New("logx: debug log directory is not initialised")
)

// debugSink 是调试日志的目标：开关、当前文件、脱敏函数。
type debugSink struct {
	mu   sync.Mutex
	dir  string
	rot  *fileRotator // nil = 未打开（调试模式关）
	now  func() time.Time
	open atomic.Bool // 对外的开关状态；Enabled 只读它，关闭时零分配

	sanitizer atomic.Pointer[Sanitizer]

	// withSource / noSource 只构造一次：它们写到 debugSink 自身（稳定的 io.Writer），
	// 文件开关只换 s.rot，因此 logger.With(...) 派生出来的实例在切换后依然有效。
	withSource slog.Handler
	noSource   slog.Handler
}

// newDebugSink 构造一个挂在 dir 上的调试日志目标（默认关闭）。
func newDebugSink(dir string) *debugSink {
	s := &debugSink{dir: dir, now: time.Now}
	s.withSource = slog.NewJSONHandler(s, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		AddSource:   true,
		ReplaceAttr: s.replaceAttr,
	})
	s.noSource = slog.NewJSONHandler(s, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: s.replaceAttr,
	})
	return s
}

// dbg 是进程内唯一的调试日志目标；doInit 时按日志目录创建。
var dbg atomic.Pointer[debugSink]

func (s *debugSink) path() string { return filepath.Join(s.dir, DebugFileName) }

// Write 实现 io.Writer：关闭状态下静默丢弃（在途的最后一两条记录不算错误）。
func (s *debugSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rot == nil {
		return len(p), nil
	}
	return s.rot.Write(p)
}

func (s *debugSink) replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.SourceKey {
		if src, ok := a.Value.Any().(*slog.Source); ok && src != nil {
			return slog.String(slog.SourceKey, fmt.Sprintf("%s:%d", trimSourcePath(src.File), src.Line))
		}
		return a
	}
	if fn := s.sanitizer.Load(); fn != nil && *fn != nil {
		return (*fn)(groups, a)
	}
	return a
}

// setEnabled 打开 / 关闭调试日志文件。
func (s *debugSink) setEnabled(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !on {
		s.open.Store(false)
		if s.rot != nil {
			_ = s.rot.Close()
			s.rot = nil
		}
		return nil
	}
	if fn := s.sanitizer.Load(); fn == nil || *fn == nil {
		return errDebugNoSanitizer
	}
	if s.dir == "" {
		return errDebugNotInit
	}
	if s.rot == nil {
		if err := s.openLocked(); err != nil {
			return err
		}
	}
	s.open.Store(true)
	return nil
}

func (s *debugSink) openLocked() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("logx: mkdir %s: %w", s.dir, err)
	}
	rot := &fileRotator{
		path:    s.path(),
		maxSize: DebugMaxFileBytes,
		backups: DebugMaxFiles - 1,
		maxAge:  DebugMaxAge,
		now:     s.now,
	}
	rot.pruneExpired(true)
	if err := rot.open(); err != nil {
		return err
	}
	s.rot = rot
	return nil
}

// files 列出现存的调试日志文件（当前文件在前，越旧越靠后）。
func (s *debugSink) files() ([]DebugFile, error) {
	if s.dir == "" {
		return nil, errDebugNotInit
	}
	out := make([]DebugFile, 0, DebugMaxFiles)
	for _, p := range rotationPaths(s.path(), DebugMaxFiles-1) {
		st, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return out, err
		}
		out = append(out, DebugFile{Name: filepath.Base(p), SizeBytes: st.Size(), ModifiedAt: st.ModTime()})
	}
	return out, nil
}

// clear 删除全部调试日志文件；开启状态下删完重新打开一个空文件继续写。
func (s *debugSink) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return errDebugNotInit
	}
	wasOpen := s.rot != nil
	if wasOpen {
		_ = s.rot.Close()
		s.rot = nil
	}
	var firstErr error
	for _, p := range rotationPaths(s.path(), DebugMaxFiles-1) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	if wasOpen {
		if err := s.openLocked(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// debugHandler 把记录交给调试日志，并补上 origin 字段。
//
// 与固定字段同名的上下文字段（如 perf 记录里的 "level":"L2"）会生成重复的 JSON 键，
// 读取时后者覆盖前者，查看器就会把档位当成日志级别。这里把它们改名为 ctx_<key>
// （只处理顶层；WithGroup 之后的字段嵌在组里，不会冲突）。
type debugHandler struct {
	sink    *debugSink
	inner   slog.Handler
	origin  string
	grouped bool
}

// reservedDebugKeys 是调试日志里的固定字段名。
var reservedDebugKeys = map[string]bool{
	slog.TimeKey: true, slog.LevelKey: true, slog.SourceKey: true, slog.MessageKey: true, OriginKey: true,
}

// ReservedKeyPrefix 是与固定字段同名的上下文字段改名后的前缀。
const ReservedKeyPrefix = "ctx_"

func (h *debugHandler) rename(a slog.Attr) slog.Attr {
	if !h.grouped && reservedDebugKeys[a.Key] {
		a.Key = ReservedKeyPrefix + a.Key
	}
	return a
}

func (h *debugHandler) Enabled(context.Context, slog.Level) bool { return h.sink.open.Load() }

func (h *debugHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.sink.open.Load() {
		return nil
	}
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.rename(a))
		return true
	})
	if !h.grouped {
		nr.AddAttrs(slog.String(OriginKey, h.origin))
	}
	return h.inner.Handle(ctx, nr)
}

func (h *debugHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	renamed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		renamed[i] = h.rename(a)
	}
	return &debugHandler{sink: h.sink, inner: h.inner.WithAttrs(renamed), origin: h.origin, grouped: h.grouped}
}

func (h *debugHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	// origin 必须留在顶层：组一开，后续字段都进组，origin 改在开组前加上。
	inner := h.inner
	if !h.grouped {
		inner = inner.WithAttrs([]slog.Attr{slog.String(OriginKey, h.origin)})
	}
	return &debugHandler{sink: h.sink, inner: inner.WithGroup(name), origin: h.origin, grouped: true}
}

// —— 对外 API（未 Init 时都安全：返回错误或空值）——

// SetDebugSanitizer 注入调试日志的脱敏函数。必须在 SetDebugEnabled(true) 之前调用；
// 也应尽早调用（logger.With 的属性在调用 With 时就被格式化并经过它）。
func SetDebugSanitizer(fn Sanitizer) {
	if s := dbg.Load(); s != nil {
		s.sanitizer.Store(&fn)
	}
}

// SetDebugEnabled 打开 / 关闭调试日志。立即生效，无需重启。
func SetDebugEnabled(on bool) error {
	s := dbg.Load()
	if s == nil {
		if !on {
			return nil
		}
		return errDebugNotInit
	}
	return s.setEnabled(on)
}

// DebugEnabled 报告调试日志当前是否在写。
func DebugEnabled() bool {
	s := dbg.Load()
	return s != nil && s.open.Load()
}

// DebugLogDir 返回调试日志所在目录（未 Init 时为空）。
func DebugLogDir() string {
	if s := dbg.Load(); s != nil {
		return s.dir
	}
	return ""
}

// DebugLogFiles 列出现存的调试日志文件。关闭状态下同样可用（不影响已有日志读取）。
func DebugLogFiles() ([]DebugFile, error) {
	s := dbg.Load()
	if s == nil {
		return nil, errDebugNotInit
	}
	return s.files()
}

// ClearDebugLogs 删除全部调试日志文件。
func ClearDebugLogs() error {
	s := dbg.Load()
	if s == nil {
		return errDebugNotInit
	}
	return s.clear()
}

// DebugOnly 返回只写调试日志的 logger（不进 app.log / 控制台、不带 source），
// 用于转写前端上报这类「只在调试时有意义」的记录。调试模式关闭时它什么都不写。
func DebugOnly(component, origin string) *slog.Logger {
	s := dbg.Load()
	if s == nil {
		return slog.New(slog.DiscardHandler)
	}
	l := slog.New(&debugHandler{sink: s, inner: s.noSource, origin: origin})
	if component != "" {
		l = l.With(slog.String("component", component))
	}
	return l
}

// rotationPaths 返回 path、path.1 … path.backups。
func rotationPaths(path string, backups int) []string {
	out := make([]string, 0, backups+1)
	out = append(out, path)
	for i := 1; i <= backups; i++ {
		out = append(out, fmt.Sprintf("%s.%d", path, i))
	}
	return out
}

// trimSourcePath 把编译期的源码路径改成仓库相对路径，去掉本机目录（可能含用户名）。
func trimSourcePath(file string) string {
	p := strings.ReplaceAll(file, `\`, "/")
	if i := strings.LastIndex(p, "/pkg/mod/"); i >= 0 {
		return p[i+len("/pkg/mod/"):]
	}
	if i := strings.Index(p, modulePath+"/"); i >= 0 {
		return p[i+len(modulePath)+1:]
	}
	if i := strings.LastIndex(p, "/internal/"); i >= 0 {
		return p[i+1:]
	}
	parts := strings.Split(p, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

// modulePath 是本仓库的 Go module 路径（-trimpath 构建里源码路径以它开头）。
const modulePath = "vip.oberon.mandaryn"
