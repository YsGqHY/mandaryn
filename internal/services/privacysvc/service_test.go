package privacysvc

// L3 薄壳测试：只验证「参数校验 / 错误码映射 / 依赖装配 / 偏好读写」，
// 判定与脱敏逻辑由 internal/privacy 的测试覆盖。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/privacy"
	"vip.oberon.mandaryn/internal/storage"
)

// newHolder 建临时库。
func newHolder(t *testing.T) *storage.Holder {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "mandaryn.db"))
	if err != nil {
		t.Fatalf("打开临时数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return storage.NewHolder(db)
}

// fixedNow 固定时钟，避免记录时间抖动影响窗口断言。
var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// newService 构造带固定时钟的服务（离线闸门只用 preferences 用户开关）。
func newService(t *testing.T, holder *storage.Holder) *Service {
	t.Helper()
	svc, err := New(Deps{
		Holder:  holder,
		Version: "0.9.0",
		Build:   "test",
		Modules: func() []string { return []string{"asr", "privacy"} },
		Now:     func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	// 构造会注册进程级离线钩子；测试结束解除，避免影响同包其它用例。
	t.Cleanup(func() { outbound.SetUserOffline(nil) })
	return svc
}

// TestNewRequiresHolder 缺 Holder 直接报错（不允许半残服务）。
func TestNewRequiresHolder(t *testing.T) {
	if _, err := New(Deps{}); !apierr.HasCode(err, apierr.CodeDBWrite) {
		t.Fatalf("期望 ERR_DB_WRITE，实际 %v", err)
	}
}

// TestGetStatusDefaults 默认状态：未离线、保留 24h、窗口内无记录。
func TestGetStatusDefaults(t *testing.T) {
	t.Setenv(privacy.EnvOffline, "")
	svc := newService(t, newHolder(t))

	st, err := svc.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus 失败: %v", err)
	}
	if st.Offline || st.UserOffline || st.EnvOffline {
		t.Fatalf("默认应非离线: %+v", st)
	}
	if st.OfflineEnvVar != privacy.EnvOffline {
		t.Fatalf("应回显接管变量名: %+v", st)
	}
	if st.RetentionHours != 24 {
		t.Fatalf("保留时长期望 24h，实际 %d", st.RetentionHours)
	}
	if st.RecentCount != 0 {
		t.Fatalf("初始应无记录: %+v", st)
	}
	if st.WindowEndMs != fixedNow.UnixMilli() {
		t.Fatalf("窗口结束应等于注入时钟: %+v", st)
	}
}

// TestSetOfflinePersistsAndGates 用户开关落库，并且真的让闸门生效。
func TestSetOfflinePersistsAndGates(t *testing.T) {
	t.Setenv(privacy.EnvOffline, "")
	holder := newHolder(t)
	svc := newService(t, holder)
	ctx := context.Background()

	st, err := svc.SetOffline(ctx, true)
	if err != nil {
		t.Fatalf("SetOffline 失败: %v", err)
	}
	if !st.Offline || !st.UserOffline || st.OfflineSource != "user" {
		t.Fatalf("开启后应为 user 来源离线: %+v", st)
	}

	// 落库可读回（新服务实例也能读到，证明不是内存态）。
	again := newService(t, holder)
	st2, err := again.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus 失败: %v", err)
	}
	if !st2.UserOffline {
		t.Fatalf("用户开关未持久化: %+v", st2)
	}

	// 闸门确实拦下（ZeroOutbound 判定依赖它）。
	gate := privacy.NewGate(privacy.GateOptions{User: again.userOffline})
	if err := gate.Check("online-asr", "https://api.example.com/v1/audio"); !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("用户开关应拦住出站，实际 %v", err)
	}

	// 关掉后恢复在线。
	st3, err := svc.SetOffline(ctx, false)
	if err != nil {
		t.Fatalf("SetOffline 失败: %v", err)
	}
	if st3.Offline {
		t.Fatalf("关闭后应恢复在线: %+v", st3)
	}
}

// TestListOutboundPagination 分页与总数。
func TestListOutboundPagination(t *testing.T) {
	holder := newHolder(t)
	rec := privacy.NewRecorder(holder)
	for i := 0; i < 5; i++ {
		rec.Record(privacy.Record{Kind: "model-download", Target: "https://cdn.example.com/a" + itoa(i) + ".onnx", Result: privacy.ResultOK})
	}
	svc := newService(t, holder)

	page, err := svc.ListOutbound(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("ListOutbound 失败: %v", err)
	}
	if page.Total != 5 || len(page.Records) != 2 || page.Limit != 2 || page.Offset != 0 {
		t.Fatalf("分页结果不正确: %+v", page)
	}

	page2, err := svc.ListOutbound(context.Background(), 2, 4)
	if err != nil {
		t.Fatalf("ListOutbound 失败: %v", err)
	}
	if len(page2.Records) != 1 {
		t.Fatalf("第二页应剩 1 条: %+v", page2)
	}

	// 非法 limit 归一到默认 100，且不报错。
	page3, err := svc.ListOutbound(context.Background(), -5, -1)
	if err != nil {
		t.Fatalf("ListOutbound 失败: %v", err)
	}
	if page3.Limit != 100 || page3.Offset != 0 || len(page3.Records) != 5 {
		t.Fatalf("参数归一化不正确: %+v", page3)
	}
}

// TestClearOutbound 清空返回删除条数。
func TestClearOutbound(t *testing.T) {
	holder := newHolder(t)
	rec := privacy.NewRecorder(holder)
	rec.Record(privacy.Record{Kind: "online-asr", Target: "https://api.example.com/x", Result: privacy.ResultOK})
	rec.Blocked("model-download", "catalog", "https://cdn.example.com/m.json", false)
	svc := newService(t, holder)

	res, err := svc.ClearOutbound(context.Background())
	if err != nil {
		t.Fatalf("ClearOutbound 失败: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("期望删除 2 条，实际 %d", res.Deleted)
	}
	page, err := svc.ListOutbound(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("ListOutbound 失败: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("清空后应为空: %+v", page)
	}
}

// TestRunAuditDetectsViolation 薄壳把 L2 结论原样返回（离线 + 真实出站 = 违例）。
func TestRunAuditDetectsViolation(t *testing.T) {
	t.Setenv(privacy.EnvOffline, "")
	holder := newHolder(t)
	svc := newService(t, holder)
	ctx := context.Background()

	if _, err := svc.SetOffline(ctx, true); err != nil {
		t.Fatalf("SetOffline 失败: %v", err)
	}
	// 人为制造一条真实出站记录（模拟「离线期间漏了闸门」）。
	privacy.NewRecorder(holder).Record(privacy.Record{
		At: fixedNow.Add(-time.Minute), Kind: "online-asr",
		Target: "https://api.example.com/v1/audio", Result: privacy.ResultOK,
	})

	report, err := svc.RunAudit(ctx, contract.AuditFilter{
		// 显式给窗口并与注入时钟对齐（Auditor 的默认窗口用真实时间）。
		FromMs: fixedNow.Add(-time.Hour).UnixMilli(),
		ToMs:   fixedNow.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("RunAudit 失败: %v", err)
	}
	if report.ZeroOutbound {
		t.Fatalf("离线期间有真实出站，零出站不能成立: %+v", report)
	}
	if len(report.Violations) != 1 || report.Violations[0].Kind != "online-asr" {
		t.Fatalf("违例明细不正确: %+v", report.Violations)
	}
}

// TestRunAuditPassesWhenClean 无记录时通过。
func TestRunAuditPassesWhenClean(t *testing.T) {
	t.Setenv(privacy.EnvOffline, "")
	svc := newService(t, newHolder(t))
	ctx := context.Background()
	if _, err := svc.SetOffline(ctx, true); err != nil {
		t.Fatalf("SetOffline 失败: %v", err)
	}
	report, err := svc.RunAudit(ctx, contract.AuditFilter{})
	if err != nil {
		t.Fatalf("RunAudit 失败: %v", err)
	}
	if !report.ZeroOutbound || len(report.Violations) != 0 {
		t.Fatalf("干净状态应判定零出站: %+v", report)
	}
}

// TestExportDiagnosticsIsRedacted 诊断包：元数据在，凭据 / 正文不在。
func TestExportDiagnosticsIsRedacted(t *testing.T) {
	holder := newHolder(t)
	rec := privacy.NewRecorder(holder)
	rec.Record(privacy.Record{
		At: fixedNow.Add(-time.Minute), Kind: "online-asr",
		Target: "https://api.example.com/v1/audio/transcriptions?token=sk-live-abcdef1234567890",
		Result: privacy.ResultOK, BytesOut: 128,
	})
	svc := newService(t, holder)

	art, err := svc.ExportDiagnostics(context.Background())
	if err != nil {
		t.Fatalf("ExportDiagnostics 失败: %v", err)
	}
	text := string(art.Bytes)
	if strings.Contains(text, "sk-live-abcdef1234567890") {
		t.Fatalf("诊断包泄漏 query 凭据: %s", text)
	}
	if !strings.Contains(text, "0.9.0") || !strings.Contains(text, "privacy") {
		t.Fatalf("元数据缺失: %s", text)
	}
	if art.MIME != "application/json" || len(art.Bytes) == 0 {
		t.Fatalf("产物不合预期: %+v", art)
	}
	var doc map[string]any
	if err := json.Unmarshal(art.Bytes, &doc); err != nil {
		t.Fatalf("产物不是合法 JSON: %v", err)
	}
	if out, _ := doc["outbound"].([]any); len(out) != 1 {
		t.Fatalf("出站记录应进包: %v", doc["outbound"])
	}
}

// TestExportDiagnosticsWithLogs 注入日志目录时读取并脱敏（含中文正文行）。
func TestExportDiagnosticsWithLogs(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	content := strings.Join([]string{
		`level=INFO msg="engine ready" component=asr`,
		`level=DEBUG msg="转写结果：今天会议讨论预算"`,
	}, "\n")
	if err := writeFile(logPath, content); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}

	svc, err := New(Deps{
		Holder:  newHolder(t),
		Version: "1.0.0",
		LogDir:  func() string { return dir },
		Now:     func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}

	art, err := svc.ExportDiagnostics(context.Background())
	if err != nil {
		t.Fatalf("ExportDiagnostics 失败: %v", err)
	}
	text := string(art.Bytes)
	if strings.Contains(text, "今天会议讨论预算") {
		t.Fatalf("日志里的正文未脱敏: %s", text)
	}
	if !strings.Contains(text, "engine ready") {
		t.Fatalf("无害日志行应保留: %s", text)
	}
}

// TestExportDiagnosticsWithoutLogDir 未注入日志目录时照常产出（不含日志副本）。
func TestExportDiagnosticsWithoutLogDir(t *testing.T) {
	svc := newService(t, newHolder(t))
	art, err := svc.ExportDiagnostics(context.Background())
	if err != nil {
		t.Fatalf("ExportDiagnostics 失败: %v", err)
	}
	if len(art.Bytes) == 0 {
		t.Fatal("应产出诊断包")
	}
}

// TestCanceledContext 已取消的 ctx 映射成 ERR_STATE_FORBIDDEN。
func TestCanceledContext(t *testing.T) {
	svc := newService(t, newHolder(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.GetStatus(ctx); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("GetStatus 期望 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if _, err := svc.ListOutbound(ctx, 10, 0); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("ListOutbound 期望 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if _, err := svc.RunAudit(ctx, contract.AuditFilter{}); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("RunAudit 期望 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if _, err := svc.ExportDiagnostics(ctx); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("ExportDiagnostics 期望 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if _, err := svc.SetOffline(ctx, true); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("SetOffline 期望 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
}

// TestInjectGate 注入自定义闸门时以注入值为准（不被 preferences 覆盖）。
func TestInjectGate(t *testing.T) {
	t.Setenv(privacy.EnvOffline, "")
	svc, err := New(Deps{
		Holder: newHolder(t),
		Gate:   privacy.NewGate(privacy.GateOptions{Env: func() bool { return true }}),
		Now:    func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	st, err := svc.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus 失败: %v", err)
	}
	if !st.Offline || st.OfflineSource != "env" {
		t.Fatalf("应以注入闸门为准: %+v", st)
	}
}

// TestUserSwitchCoversLegacyGuard 用户开关必须覆盖既有 outbound.Guard 调用方
// （asr / models / enginesvc 只调 Guard，不经 privacysvc）。
func TestUserSwitchCoversLegacyGuard(t *testing.T) {
	t.Setenv(privacy.EnvOffline, "")
	svc := newService(t, newHolder(t))
	ctx := context.Background()

	if err := outbound.Guard(); err != nil {
		t.Fatalf("默认应放行: %v", err)
	}
	if _, err := svc.SetOffline(ctx, true); err != nil {
		t.Fatalf("SetOffline 失败: %v", err)
	}
	if err := outbound.Guard(); !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("用户开关应让 outbound.Guard 拒绝，实际 %v", err)
	}
	if !outbound.Offline() {
		t.Fatal("outbound.Offline 应报告离线")
	}
	if _, err := svc.SetOffline(ctx, false); err != nil {
		t.Fatalf("SetOffline 失败: %v", err)
	}
	if err := outbound.Guard(); err != nil {
		t.Fatalf("关闭后应放行: %v", err)
	}
}

// TestParseBool preferences 里的布尔口径。
func TestParseBool(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "\"true\"", "1", "on", " yes "} {
		if !parseBool(v) {
			t.Fatalf("%q 应解析为 true", v)
		}
	}
	for _, v := range []string{"false", "", "0", "no", "off", "null"} {
		if parseBool(v) {
			t.Fatalf("%q 应解析为 false", v)
		}
	}
}
