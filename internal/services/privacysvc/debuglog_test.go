package privacysvc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/privacy"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// initDebugLogx 初始化进程内 logx（只能 Init 一次，本包测试共用同一个日志目录）。
var debugLogDir string

func initDebugLogx(t *testing.T) string {
	t.Helper()
	if debugLogDir == "" {
		dir, err := os.MkdirTemp("", "mandaryn-privacysvc-debuglog-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := logx.Init(logx.Config{Dir: dir}); err != nil {
			t.Fatalf("logx.Init: %v", err)
		}
		logx.SetDebugSanitizer(privacy.SanitizeDebugAttr)
		debugLogDir = dir
	}
	t.Cleanup(func() {
		_ = logx.SetDebugEnabled(false)
		_ = logx.ClearDebugLogs()
	})
	return debugLogDir
}

// TestReportFrontendLogRespectsSwitch：关闭时不落盘；开启时写入调试日志（origin=frontend），不进 app.log。
func TestReportFrontendLogRespectsSwitch(t *testing.T) {
	dir := initDebugLogx(t)
	svc := newService(t, newHolder(t))
	ctx := context.Background()
	entry := contract.FrontendLogEntry{Level: "error", Message: "TypeError: x is undefined", Stack: "at f (/src/a.ts:1:2)", Route: "library", Window: "main", Kind: "error"}

	if err := svc.ReportFrontendLog(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, logx.DebugFileName)); err == nil {
		t.Fatal("调试模式关闭时不应写调试日志")
	}

	if err := logx.SetDebugEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReportFrontendLog(ctx, entry); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ReadDebugLog(ctx, contract.DebugLogQuery{Origin: "frontend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("应有 1 条前端记录，实际 %d", len(page.Entries))
	}
	e := page.Entries[0]
	if e.Level != "ERROR" || e.Component != "frontend" || !strings.Contains(e.Fields, "TypeError") || !strings.Contains(e.Fields, `"route":"library"`) {
		t.Fatalf("前端记录不对：%+v", e)
	}
	app, _ := os.ReadFile(filepath.Join(dir, "app.log"))
	if strings.Contains(string(app), "TypeError") {
		t.Fatal("前端上报不应进 app.log")
	}

	// 关闭后旧记录仍可读。
	_ = logx.SetDebugEnabled(false)
	again, err := svc.ReadDebugLog(ctx, contract.DebugLogQuery{Origin: "frontend"})
	if err != nil || len(again.Entries) != 1 {
		t.Fatalf("关闭后旧日志应仍可读：%+v %v", again, err)
	}
}

// TestDebugLogStatusAndClear：状态里有文件与保留策略；清空后文件被删。
func TestDebugLogStatusAndClear(t *testing.T) {
	initDebugLogx(t)
	svc := newService(t, newHolder(t))
	ctx := context.Background()
	if err := logx.SetDebugEnabled(true); err != nil {
		t.Fatal(err)
	}
	logx.For("test").Debug("一条调试记录", "n", 1)

	st, err := svc.GetDebugLogStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Format != "jsonl" || st.MaxFiles != logx.DebugMaxFiles || st.RetentionDays != 7 || st.MaxFileBytes != logx.DebugMaxFileBytes {
		t.Fatalf("保留策略字段不对：%+v", st)
	}
	if len(st.Files) == 0 || st.TotalBytes == 0 || !st.Mode.Enabled {
		t.Fatalf("开启后应有非空文件：%+v", st)
	}

	cleared, err := svc.ClearDebugLog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 「调试日志已清空」这条本身会写进新文件，因此只断言旧内容不在了。
	page, _ := svc.ReadDebugLog(ctx, contract.DebugLogQuery{})
	for _, e := range page.Entries {
		if e.Msg == "一条调试记录" {
			t.Fatalf("清空后旧记录不应还在：%+v（status=%+v）", e, cleared)
		}
	}
}

func TestFrontendLimiter(t *testing.T) {
	var l frontendLimiter
	now := time.Unix(1_700_000_000, 0)
	allowed := 0
	for i := 0; i < frontendMaxPerSecond+5; i++ {
		if ok, _ := l.allow(now); ok {
			allowed++
		}
	}
	if allowed != frontendMaxPerSecond {
		t.Fatalf("每秒应放行 %d 条，实际 %d", frontendMaxPerSecond, allowed)
	}
	ok, carried := l.allow(now.Add(time.Second))
	if !ok || carried != 5 {
		t.Fatalf("下一秒应放行并带出丢弃数 5，实际 ok=%v carried=%d", ok, carried)
	}
}
