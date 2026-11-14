// Package logx 是 Mandaryn 的统一日志门面。
//
// 设计：基于标准库 log/slog（结构化日志），同时输出到：
//   - 控制台（dev 期）—— 文本格式，带颜色友好的级别
//   - 文件（持久）—— JSON 格式，自动按大小滚动
//   - 调试日志（仅调试模式开启时）—— 同目录 debug.log，JSON Lines，见 debug_sink.go
//
// 业务约束：
//   - 不要 fmt.Println / log.Printf —— 这些没结构化字段，dev 期看着方便，
//     但日志聚合 / 排查时几乎没法用
//   - 取 logger 用 logx.For("component-name")，不要在每个文件里 logx.New
//
// 与 procx 集成：在 procx.Spec 的 OnStdout / OnStderr 里直接 logx.Infof，
// 子进程的输出就自动落到统一日志里。
package logx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"vip.oberon.mandaryn/internal/paths"
)

// Config 控制日志输出。在 internal/app.Run 早期调一次 Init，业务代码后续用 For()。
type Config struct {
	// Level 默认 LevelInfo；dev 期可以传 LevelDebug。
	Level slog.Level
	// Dir 日志目录；空时取平台默认（与数据库同级目录下的 logs/）。
	Dir string
	// Filename 日志文件名（不含路径）；默认 "app.log"。
	Filename string
	// MaxSizeBytes 单文件大小阈值，超过滚动到 .1。默认 8MB。
	MaxSizeBytes int64
	// Backups 保留多少个滚动文件（不含当前）。默认 3。
	Backups int
	// ConsoleEnabled 是否同时打控制台。默认 true（dev 友好）。
	ConsoleEnabled bool
	// AddSource 在每条日志里加调用 file:line。默认 false（生产降噪）。
	AddSource bool
}

const (
	defaultFilename     = "app.log"
	defaultMaxSizeBytes = 8 << 20
	defaultBackups      = 3
)

// AppDirName 与应用标识保持一致（唯一来源 internal/paths）。
const AppDirName = paths.AppDirName

var (
	initOnce  sync.Once
	rootMu    sync.RWMutex
	root      *slog.Logger
	rotator   *fileRotator
	logCloser io.Closer
)

// Init 初始化全局 logger；幂等。返回错误时调用方可降级到只用控制台。
func Init(cfg Config) error {
	var err error
	initOnce.Do(func() {
		err = doInit(cfg)
	})
	return err
}

func doInit(cfg Config) error {
	if cfg.Filename == "" {
		cfg.Filename = defaultFilename
	}
	if cfg.MaxSizeBytes <= 0 {
		cfg.MaxSizeBytes = defaultMaxSizeBytes
	}
	if cfg.Backups <= 0 {
		cfg.Backups = defaultBackups
	}

	dir := cfg.Dir
	if dir == "" {
		d, err := defaultLogDir()
		if err != nil {
			return err
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("logx: mkdir %s: %w", dir, err)
	}

	rot := &fileRotator{
		path:    filepath.Join(dir, cfg.Filename),
		maxSize: cfg.MaxSizeBytes,
		backups: cfg.Backups,
	}
	if err := rot.open(); err != nil {
		return err
	}
	rotator = rot
	logCloser = rot

	// 调试日志与 app.log 同目录，默认关闭；启动时先按保留期清理旧文件（与开关无关）。
	sink := newDebugSink(dir)
	(&fileRotator{path: sink.path(), backups: DebugMaxFiles - 1, maxAge: DebugMaxAge}).pruneExpired(true)
	dbg.Store(sink)

	handlers := []slog.Handler{}
	handlers = append(handlers, slog.NewJSONHandler(rot, &slog.HandlerOptions{
		Level:     cfg.Level,
		AddSource: cfg.AddSource,
	}))
	if cfg.ConsoleEnabled {
		handlers = append(handlers, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:     cfg.Level,
			AddSource: cfg.AddSource,
		}))
	}
	handlers = append(handlers, &debugHandler{sink: sink, inner: sink.withSource, origin: OriginBackend})

	rootMu.Lock()
	root = slog.New(&fanoutHandler{handlers: handlers})
	rootMu.Unlock()
	slog.SetDefault(root)
	return nil
}

// Close 在 app 退出前调用一次（os.Exit 前），把缓冲的日志刷到磁盘。
func Close() {
	if logCloser != nil {
		_ = logCloser.Close()
	}
	// 调试日志只关文件、不改用户开关（开关的持久化值在数据库里）。
	if s := dbg.Load(); s != nil {
		_ = s.setEnabled(false)
	}
}

// For 返回带 component 字段的 logger。第一次调用前必须 Init；
// 否则返回一个写到 stderr 的临时 logger（避免 nil panic）。
func For(component string) *slog.Logger {
	rootMu.RLock()
	r := root
	rootMu.RUnlock()
	if r == nil {
		// fallback：开发者忘了 Init，至少别 panic
		r = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if component == "" {
		return r
	}
	return r.With(slog.String("component", component))
}

// 便捷函数（用 default logger）
func Info(msg string, args ...any)  { For("").Info(msg, args...) }
func Warn(msg string, args ...any)  { For("").Warn(msg, args...) }
func Error(msg string, args ...any) { For("").Error(msg, args...) }
func Debug(msg string, args ...any) { For("").Debug(msg, args...) }

func defaultLogDir() (string, error) {
	// 正常路径：由 internal/app 用 paths.Resolver 的结果填 Config.Dir 传进来
	// （保证日志与数据库、密钥同根）。这里是「未显式指定」时的兜底。
	if v := os.Getenv(paths.EnvDataRoot); v != "" {
		return filepath.Join(v, paths.LogsDirName), nil
	}
	base, err := paths.DefaultDataRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, paths.LogsDirName), nil
}

// fanoutHandler 把一条记录广播到多个 slog.Handler。
// slog 标准库没内置 multi-writer handler，所以在这一层自己合。
type fanoutHandler struct {
	handlers []slog.Handler
}

func (f *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range f.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		// 每个 handler 拿自己的 record 副本（slog 内部已是值类型，可直接复用）
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		out[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: out}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		out[i] = h.WithGroup(name)
	}
	return &fanoutHandler{handlers: out}
}

// fileRotator：朴素的 size-based 滚动 writer。
//
// 之所以不直接引入 lumberjack：
//   - 只为脚手架够用 + 零额外依赖
//   - 业务生产环境若有 ELK / Loki 等集中日志，应该走那条管道，不依赖本地 rotate
type fileRotator struct {
	path    string
	maxSize int64
	backups int
	// maxAge > 0 时，打开与每次滚动后删除修改时间早于 now-maxAge 的文件（调试日志用；app.log 不设）。
	maxAge time.Duration
	now    func() time.Time

	mu  sync.Mutex
	f   *os.File
	cur atomic.Int64
}

// renameFn 是滚动用的重命名函数（测试注入失败用）。
var renameFn = os.Rename

func (w *fileRotator) open() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("logx: open %s: %w", w.path, err)
	}
	w.f = f
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.cur.Store(st.Size())
	return nil
}

func (w *fileRotator) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, errFileClosed
	}
	if w.cur.Load()+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	if err != nil {
		return n, err
	}
	w.cur.Add(int64(n))
	return n, nil
}

func (w *fileRotator) rotateLocked() error {
	if w.f != nil {
		_ = w.f.Sync()
		_ = w.f.Close()
		w.f = nil
	}
	// path -> path.1, .1 -> .2, ... 滚到 backups（最旧的被覆盖）；backups == 0 时直接截断重写。
	if w.backups > 0 {
		for i := w.backups; i >= 2; i-- {
			_ = renameFn(fmt.Sprintf("%s.%d", w.path, i-1), fmt.Sprintf("%s.%d", w.path, i))
		}
		if err := renameFn(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			// 当前文件挪不走（Windows 上常见于正被别的进程打开读取）：继续往它后面追加，
			// 下次写入再试。绝不能像以前那样带 O_TRUNC 重开 —— 那会把当前文件的内容清掉。
			f, oerr := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if oerr != nil {
				return oerr
			}
			w.f = f
			if st, serr := f.Stat(); serr == nil {
				w.cur.Store(st.Size())
			}
			return nil
		}
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	w.cur.Store(0)
	w.pruneExpired(false)
	return nil
}

// pruneExpired 删除修改时间超过 maxAge 的滚动文件；includeCurrent 时当前文件也在其列
// （只在打开之前调用：长期没写过的旧调试日志不该被续写）。maxAge <= 0 时不做任何事。
func (w *fileRotator) pruneExpired(includeCurrent bool) {
	if w.maxAge <= 0 {
		return
	}
	now := time.Now
	if w.now != nil {
		now = w.now
	}
	cutoff := now().Add(-w.maxAge)
	for i, p := range rotationPaths(w.path, w.backups) {
		if i == 0 && !includeCurrent {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.ModTime().Before(cutoff) {
			_ = os.Remove(p)
		}
	}
}

func (w *fileRotator) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

var errFileClosed = fmt.Errorf("logx: writer is closed")
