package logx

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// passSanitizer 原样返回属性（脱敏规则在 internal/privacy 测）。
func passSanitizer(_ []string, a slog.Attr) slog.Attr { return a }

// newTestLogger 构造一个与 doInit 同形的 logger：app 文件（Info）+ 调试日志。
func newTestLogger(t *testing.T) (*slog.Logger, *debugSink, string) {
	t.Helper()
	dir := t.TempDir()
	sink := newDebugSink(dir)
	app := &fileRotator{path: filepath.Join(dir, "app.log"), maxSize: 1 << 20, backups: 1}
	if err := app.open(); err != nil {
		t.Fatalf("open app.log: %v", err)
	}
	t.Cleanup(func() {
		_ = app.Close()
		_ = sink.setEnabled(false)
	})
	root := slog.New(&fanoutHandler{handlers: []slog.Handler{
		slog.NewJSONHandler(app, &slog.HandlerOptions{Level: slog.LevelInfo}),
		&debugHandler{sink: sink, inner: sink.withSource, origin: OriginBackend},
	}})
	return root, sink, dir
}

func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("非 JSON 行 %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestDebugSinkOffWritesNothing：默认关闭，不建文件也不写入；app.log 照常。
func TestDebugSinkOffWritesNothing(t *testing.T) {
	root, sink, dir := newTestLogger(t)
	sink.sanitizer.Store(ptr(Sanitizer(passSanitizer)))

	root.With("component", "asr").Debug("调试细节", "threads", 4)
	root.Info("普通信息")

	if _, err := os.Stat(filepath.Join(dir, DebugFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("调试模式关闭时不应创建 %s（err=%v）", DebugFileName, err)
	}
	if lines := readJSONLines(t, filepath.Join(dir, "app.log")); len(lines) != 1 {
		t.Fatalf("app.log 应只有 1 条 Info，实际 %d", len(lines))
	}
}

// TestDebugSinkWritesJSONLines：开启后所有级别都进 debug.log，字段齐全；app.log 不收 Debug。
func TestDebugSinkWritesJSONLines(t *testing.T) {
	root, sink, dir := newTestLogger(t)
	sink.sanitizer.Store(ptr(Sanitizer(passSanitizer)))
	if err := sink.setEnabled(true); err != nil {
		t.Fatalf("开启失败：%v", err)
	}

	l := root.With("component", "asr")
	l.Debug("按当前档位加载本地引擎", "threads", 4)
	l.Warn("预热失败", "code", "ERR_MODEL_NOT_FOUND")

	lines := readJSONLines(t, filepath.Join(dir, DebugFileName))
	if len(lines) != 2 {
		t.Fatalf("debug.log 应有 2 条，实际 %d", len(lines))
	}
	first := lines[0]
	for _, key := range []string{"time", "level", "source", "msg", "component", OriginKey} {
		if _, ok := first[key]; !ok {
			t.Fatalf("缺少字段 %q：%v", key, first)
		}
	}
	if first["level"] != "DEBUG" || first["component"] != "asr" || first[OriginKey] != OriginBackend {
		t.Fatalf("字段值不对：%v", first)
	}
	src, _ := first["source"].(string)
	if !strings.HasPrefix(src, "internal/utils/logx/debug_sink_test.go:") {
		t.Fatalf("source 应为仓库相对路径，实际 %q", src)
	}
	if appLines := readJSONLines(t, filepath.Join(dir, "app.log")); len(appLines) != 1 {
		t.Fatalf("app.log 只该收到 Warn 那 1 条，实际 %d", len(appLines))
	}
}

// TestDebugSinkDisableStopsWritingKeepsFile：关闭后句柄释放、文件保留可读、不再增长。
func TestDebugSinkDisableStopsWritingKeepsFile(t *testing.T) {
	root, sink, dir := newTestLogger(t)
	sink.sanitizer.Store(ptr(Sanitizer(passSanitizer)))
	if err := sink.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	root.Debug("关之前")
	if err := sink.setEnabled(false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, DebugFileName)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("关闭后旧日志应保留：%v", err)
	}
	root.Debug("关之后")
	root.Error("关之后的错误")
	after, _ := os.Stat(path)
	if after.Size() != before.Size() {
		t.Fatalf("关闭后不应再写入：%d → %d", before.Size(), after.Size())
	}
	// 句柄已释放：Windows 下也能直接改名（被占用时会失败）。
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatalf("关闭后文件句柄应已释放：%v", err)
	}
	if got := readJSONLines(t, path+".moved"); len(got) != 1 {
		t.Fatalf("旧日志应仍可读出 1 条，实际 %d", len(got))
	}
}

// TestDebugSinkRequiresSanitizer：未注入脱敏器时拒绝开启（失败即关闭）。
func TestDebugSinkRequiresSanitizer(t *testing.T) {
	_, sink, dir := newTestLogger(t)
	if err := sink.setEnabled(true); !errors.Is(err, errDebugNoSanitizer) {
		t.Fatalf("应返回 errDebugNoSanitizer，实际 %v", err)
	}
	if sink.open.Load() {
		t.Fatal("拒绝开启后状态必须是关")
	}
	if _, err := os.Stat(filepath.Join(dir, DebugFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("拒绝开启时不应创建文件")
	}
}

// TestDebugSinkSanitizerApplied：属性在写盘前经过脱敏函数（含 With 预格式化的属性）。
func TestDebugSinkSanitizerApplied(t *testing.T) {
	root, sink, dir := newTestLogger(t)
	sink.sanitizer.Store(ptr(Sanitizer(func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == "token" {
			return slog.String(a.Key, "***")
		}
		return a
	})))
	if err := sink.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	root.With("token", "with-secret").Debug("x", "token", "call-secret")
	raw, _ := os.ReadFile(filepath.Join(dir, DebugFileName))
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("敏感值不应落盘：%s", raw)
	}
}

// TestDebugSinkRotatesAndCapsFiles：按大小滚动，最多保留 DebugMaxFiles 个文件。
func TestDebugSinkRotatesAndCapsFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DebugFileName)
	rot := &fileRotator{path: path, maxSize: 200, backups: DebugMaxFiles - 1}
	if err := rot.open(); err != nil {
		t.Fatal(err)
	}
	defer rot.Close()
	line := []byte(strings.Repeat("x", 99) + "\n")
	for i := 0; i < 40; i++ {
		if _, err := rot.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != DebugMaxFiles {
		t.Fatalf("应正好保留 %d 个文件，实际 %d", DebugMaxFiles, len(entries))
	}
	for _, e := range entries {
		if info, _ := e.Info(); info.Size() > 200 {
			t.Fatalf("%s 超过滚动阈值：%d", e.Name(), info.Size())
		}
	}
}

// TestDebugSinkPrunesExpiredFiles：超过保留期的文件在打开时被删除，新的保留。
func TestDebugSinkPrunesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	sink := newDebugSink(dir)
	sink.sanitizer.Store(ptr(Sanitizer(passSanitizer)))
	now := time.Now()
	sink.now = func() time.Time { return now }

	old := filepath.Join(dir, DebugFileName+".2")
	fresh := filepath.Join(dir, DebugFileName+".1")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour))
	_ = os.Chtimes(fresh, now.Add(-time.Hour), now.Add(-time.Hour))

	if err := sink.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	defer sink.setEnabled(false)
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("8 天前的调试日志应被删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("1 小时前的调试日志应保留")
	}
}

// TestDebugSinkClearWhileEnabled：开启状态下清空，删完能继续写。
func TestDebugSinkClearWhileEnabled(t *testing.T) {
	root, sink, dir := newTestLogger(t)
	sink.sanitizer.Store(ptr(Sanitizer(passSanitizer)))
	if err := sink.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	root.Debug("清空前")
	if err := os.WriteFile(filepath.Join(dir, DebugFileName+".1"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sink.clear(); err != nil {
		t.Fatalf("清空失败：%v", err)
	}
	files, _ := sink.files()
	for _, f := range files {
		if f.Name != DebugFileName || f.SizeBytes != 0 {
			t.Fatalf("清空后只应剩空的当前文件，实际 %+v", files)
		}
	}
	root.Debug("清空后")
	if got := readJSONLines(t, filepath.Join(dir, DebugFileName)); len(got) != 1 || got[0]["msg"] != "清空后" {
		t.Fatalf("清空后应能继续写：%v", got)
	}
}

// TestRotateRenameFailureKeepsContent：当前文件改名失败时继续追加，不截断已有内容。
func TestRotateRenameFailureKeepsContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rot := &fileRotator{path: path, maxSize: 50, backups: 2}
	if err := rot.open(); err != nil {
		t.Fatal(err)
	}
	defer rot.Close()
	if _, err := rot.Write([]byte(strings.Repeat("a", 40) + "\n")); err != nil {
		t.Fatal(err)
	}

	orig := renameFn
	renameFn = func(string, string) error { return errors.New("file in use") }
	defer func() { renameFn = orig }()

	if _, err := rot.Write([]byte(strings.Repeat("b", 40) + "\n")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "aaaa") || !strings.Contains(string(raw), "bbbb") {
		t.Fatalf("改名失败时不应清掉当前内容：%q", raw)
	}
}

// TestDebugOnlyRespectsSwitch：DebugOnly logger 只写调试日志、跟随开关、不带 source。
func TestDebugOnlyRespectsSwitch(t *testing.T) {
	dir := t.TempDir()
	sink := newDebugSink(dir)
	sink.sanitizer.Store(ptr(Sanitizer(passSanitizer)))
	l := slog.New(&debugHandler{sink: sink, inner: sink.noSource, origin: OriginFrontend}).With("component", "frontend")

	l.Error("关闭时")
	if _, err := os.Stat(filepath.Join(dir, DebugFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("关闭时不应写入")
	}
	if err := sink.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	defer sink.setEnabled(false)
	l.Error("开启时", "route", "library")
	got := readJSONLines(t, filepath.Join(dir, DebugFileName))
	if len(got) != 1 || got[0][OriginKey] != OriginFrontend || got[0]["route"] != "library" {
		t.Fatalf("前端记录不对：%v", got)
	}
	if _, ok := got[0]["source"]; ok {
		t.Fatal("前端记录不应带后端 source")
	}
}

func TestTrimSourcePath(t *testing.T) {
	cases := map[string]string{
		`E:\Desktop\GoProject\mandaryn\internal\asr\router.go`:                            "internal/asr/router.go",
		"vip.oberon.mandaryn/internal/asr/router.go":                                      "internal/asr/router.go",
		"C:/Users/mayn/go/pkg/mod/github.com/wailsapp/wails/v3@v3.1.0/pkg/application/a.go": "github.com/wailsapp/wails/v3@v3.1.0/pkg/application/a.go",
		"E:/Desktop/GoProject/mandaryn/main.go":                                           "mandaryn/main.go",
	}
	for in, want := range cases {
		if got := trimSourcePath(in); got != want {
			t.Errorf("trimSourcePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// TestDebugSinkRenamesReservedKeys：与固定字段同名的上下文字段改名为 ctx_<key>，
// 不能覆盖真正的 level / source（perf 采样里的 "level":"L2" 曾被读成日志级别）。
func TestDebugSinkRenamesReservedKeys(t *testing.T) {
	root, sink, dir := newTestLogger(t)
	sink.sanitizer.Store(ptr(Sanitizer(passSanitizer)))
	if err := sink.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	root.With("component", "perf", "origin", "x").Debug("perf 采样", "level", "L2", "source", "user")
	root.WithGroup("g").Debug("分组", "level", "L3")

	lines := readJSONLines(t, filepath.Join(dir, DebugFileName))
	if len(lines) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(lines))
	}
	first := lines[0]
	if first["level"] != "DEBUG" || first["ctx_level"] != "L2" || first["ctx_source"] != "user" ||
		first[OriginKey] != OriginBackend || first["ctx_origin"] != "x" {
		t.Fatalf("同名字段应改名且不覆盖固定字段：%v", first)
	}
	if !strings.HasPrefix(first["source"].(string), "internal/") {
		t.Fatalf("source 应是代码位置：%v", first["source"])
	}
	second := lines[1]
	group, _ := second["g"].(map[string]any)
	if second["level"] != "DEBUG" || second[OriginKey] != OriginBackend || group["level"] != "L3" {
		t.Fatalf("组内字段不受影响、origin 留在顶层：%v", second)
	}
}

// TestDebugSinkConcurrentToggle：边写边开关、边清空，不死锁、不 panic、文件始终是合法 JSONL。
func TestDebugSinkConcurrentToggle(t *testing.T) {
	root, sink, dir := newTestLogger(t)
	sink.sanitizer.Store(ptr(Sanitizer(passSanitizer)))
	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func(id int) {
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				root.With("component", "w").Debug("tick", "worker", id, "i", i)
			}
		}(w)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		_ = sink.setEnabled(i%2 == 0)
		if i%7 == 0 {
			_ = sink.clear()
		}
	}
	close(done)
	_ = sink.setEnabled(false)
	if _, err := os.Stat(filepath.Join(dir, DebugFileName)); err == nil {
		readJSONLines(t, filepath.Join(dir, DebugFileName)) // 每行都必须能解析
	}
}
