package privacy

// 零出站自检测试：结论必须可判定 —— 离线 + 一条真实出站 = 违例；
// 只有被拦下的尝试 = 通过；无记录 = 通过。

import (
	"context"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// TestAuditPassesWithNoRecords 空库 + 离线 = 零出站成立。
func TestAuditPassesWithNoRecords(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	aud := NewAuditor(NewGate(GateOptions{Env: boolPtr(true)}), rec)

	report, err := aud.Run(context.Background(), contract.AuditFilter{})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if !report.ZeroOutbound {
		t.Fatalf("空库应判定零出站成立: %+v", report)
	}
	if report.Scanned != 0 || len(report.Violations) != 0 {
		t.Fatalf("空库不应有条目 / 违例: %+v", report)
	}
	if !report.Offline || report.OfflineSource != string(SourceEnv) {
		t.Fatalf("自检应带上离线状态: %+v", report)
	}
}

// TestAuditFlagsRealOutboundWhileOffline 离线模式 + 一条真实出站 = 违例。
func TestAuditFlagsRealOutboundWhileOffline(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	rec.Record(Record{Kind: "online-asr", Target: "https://api.example.com/v1/audio", Result: ResultOK})
	rec.Blocked("model-download", "catalog", "https://cdn.example.com/models.json", false)

	aud := NewAuditor(NewGate(GateOptions{Env: boolPtr(true)}), rec)
	report, err := aud.Run(context.Background(), contract.AuditFilter{})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if report.ZeroOutbound {
		t.Fatalf("离线期间有真实出站，零出站不能成立: %+v", report)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("期望 1 条违例，实际 %d: %+v", len(report.Violations), report.Violations)
	}
	v := report.Violations[0]
	if v.Reason != ReasonRealWhileOffline || v.Kind != "online-asr" {
		t.Fatalf("违例明细不正确: %+v", v)
	}
	if report.AllowedCount != 1 {
		t.Fatalf("被拦下的尝试应计入 AllowedCount，实际 %d", report.AllowedCount)
	}
}

// TestAuditIgnoresBlockedAttempts 只有 blocked 记录 → 零出站成立（拦截不算出站）。
func TestAuditIgnoresBlockedAttempts(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	rec.Blocked("online-asr", "finalize", "https://api.example.com/v1/audio", true)
	rec.Blocked("model-download", "bundle", "https://cdn.example.com/a.onnx", false)

	aud := NewAuditor(NewGate(GateOptions{Env: boolPtr(true)}), rec)
	report, err := aud.Run(context.Background(), contract.AuditFilter{})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if !report.ZeroOutbound || len(report.Violations) != 0 {
		t.Fatalf("只有被拦下的尝试时应判定零出站: %+v", report)
	}
	if report.AllowedCount != 2 || report.Scanned != 2 {
		t.Fatalf("计数不正确: %+v", report)
	}
}

// TestAuditFlagsPending 结果未知的 pending 记录按违例处理（无法证明没出站）。
func TestAuditFlagsPending(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	rec.Record(Record{Kind: "model-download", Target: "https://cdn.example.com/a.onnx", Result: ResultPending})

	aud := NewAuditor(NewGate(GateOptions{Env: boolPtr(false)}), rec)
	report, err := aud.Run(context.Background(), contract.AuditFilter{})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if report.ZeroOutbound {
		t.Fatalf("pending 记录不能算零出站: %+v", report)
	}
	if len(report.Violations) != 1 || report.Violations[0].Reason != ReasonUnfinished {
		t.Fatalf("期望 unfinished-attempt 违例: %+v", report.Violations)
	}
}

// TestAuditOnlineRealOutboundNotViolation 在线模式下真实出站只统计、不判违例。
func TestAuditOnlineRealOutboundNotViolation(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	rec.Record(Record{Kind: "online-asr", Target: "https://api.example.com/v1/audio", Result: ResultOK})

	aud := NewAuditor(NewGate(GateOptions{Env: boolPtr(false), User: boolPtr(false)}), rec)
	report, err := aud.Run(context.Background(), contract.AuditFilter{})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if report.ZeroOutbound {
		t.Fatal("在线模式有出站记录时零出站不成立")
	}
	if len(report.Violations) != 0 {
		t.Fatalf("未开启离线时不应判违例: %+v", report.Violations)
	}
	if len(report.Notes) == 0 {
		t.Fatal("不判违例时应给出说明")
	}
}

// TestAuditFilterByKindAndWindow 过滤条件生效（类型 + 时间窗）。
func TestAuditFilterByKindAndWindow(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	now := time.Now()
	// 一条「旧」记录（窗口外）+ 一条窗口内的下载记录。
	rec.Record(Record{At: now.Add(-48 * time.Hour), Kind: "online-asr", Target: "https://api.example.com/v1/audio", Result: ResultOK})
	rec.Record(Record{At: now.Add(-time.Hour), Kind: "model-download", Target: "https://cdn.example.com/a.onnx", Result: ResultOK})

	aud := NewAuditor(NewGate(GateOptions{Env: boolPtr(true)}), rec)

	// 只审 online-asr：窗口内没有 → 零出站成立（旧记录在窗口外）。
	report, err := aud.Run(context.Background(), contract.AuditFilter{
		Kinds: []string{"online-asr"},
	})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if !report.ZeroOutbound {
		t.Fatalf("窗口外的记录不应参与判定: %+v", report)
	}

	// 不限定类型：窗口内那条 model-download 构成违例。
	report, err = aud.Run(context.Background(), contract.AuditFilter{})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if report.ZeroOutbound || len(report.Violations) != 1 || report.Violations[0].Kind != "model-download" {
		t.Fatalf("期望 model-download 违例: %+v", report)
	}

	// 显式给更宽窗口 → 两条都进来。
	report, err = aud.Run(context.Background(), contract.AuditFilter{
		FromMs: now.Add(-72 * time.Hour).UnixMilli(),
		ToMs:   now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if report.Scanned != 2 || len(report.Violations) != 2 {
		t.Fatalf("显式窗口应覆盖两条: %+v", report)
	}
}

// TestAuditCanceledContext 已取消的 ctx 直接返回错误（不自检）。
func TestAuditCanceledContext(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	aud := NewAuditor(NewGate(GateOptions{Env: boolPtr(true)}), rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := aud.Run(ctx, contract.AuditFilter{}); err == nil {
		t.Fatal("已取消的 ctx 应返回错误")
	}
}

// TestAuditNilRecorder 无库时不 panic，且判定为「无记录」。
func TestAuditNilRecorder(t *testing.T) {
	aud := NewAuditor(NewGate(GateOptions{Env: boolPtr(true)}), NewRecorder(nil))
	report, err := aud.Run(context.Background(), contract.AuditFilter{})
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if !report.ZeroOutbound {
		t.Fatalf("无库时应判定零出站: %+v", report)
	}
}
