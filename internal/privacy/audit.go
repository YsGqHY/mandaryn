package privacy

import (
	"context"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// 自检判定原因键（机器可读，前端按需 i18n）。
const (
	// ReasonRealWhileOffline 离线生效期间出现真实出站 —— 硬违例。
	ReasonRealWhileOffline = "real-outbound-while-offline"
	// ReasonUnfinished 记录停在 pending：无法证明没出站，按可疑处理。
	ReasonUnfinished = "unfinished-attempt"
	// ReasonRealOutbound 在线模式下的真实出站（不算违例，仅计入明细）。
	ReasonRealOutbound = "real-outbound"
)

// Auditor 执行「零出站」自检（FR-08：支撑「近期无外连」的自证）。
//
// 判定口径（可判定、可单测）：
//   - 只有 Result 为 blocked / ERR_OFFLINE_ENFORCED 的记录算「被拦下的尝试」，
//     它们**没有真的离开本机**，因此不破坏零出站；
//   - 其余记录（ok / http-4xx / ERR_* 业务码）都算真实出站；
//   - pending（进程崩溃遗留）无法证明没出站，按违例处理；
//   - 离线生效时，任何一条真实出站都是违例；离线未生效时只统计不判违例。
type Auditor struct {
	gate *Gate
	rec  *Recorder
}

// NewAuditor 构造自检器；gate 为 nil 时退化为只看环境变量。
func NewAuditor(gate *Gate, rec *Recorder) *Auditor {
	if gate == nil {
		gate = NewGate(GateOptions{})
	}
	return &Auditor{gate: gate, rec: rec}
}

// Run 执行自检。filter 为零值时用「最近 24h、全部类型」。
func (a *Auditor) Run(ctx context.Context, filter contract.AuditFilter) (contract.AuditReport, error) {
	now := time.Now()
	if err := ctx.Err(); err != nil {
		return contract.AuditReport{}, err
	}

	from, to := Window(now)
	if filter.FromMs > 0 {
		from = time.UnixMilli(filter.FromMs)
	}
	if filter.ToMs > 0 {
		to = time.UnixMilli(filter.ToMs)
	}

	st := a.gate.State()
	report := contract.AuditReport{
		GeneratedAtMs: now.UnixMilli(),
		FromMs:        from.UnixMilli(),
		ToMs:          to.UnixMilli(),
		Offline:       st.Offline,
		OfflineSource: string(st.Source),
		Violations:    []contract.AuditFinding{},
		Notes:         []string{},
	}

	if filter.FromMs == 0 && filter.ToMs == 0 {
		report.Notes = append(report.Notes, "window defaulted to last 24h (retention policy)")
	}
	report.Notes = append(report.Notes,
		"offline state is evaluated at audit time; outbound_log keeps no switch history")

	var rows []contract.OutboundRecord
	if a.rec != nil {
		scanned, err := a.rec.Scan(from, to, filter.Kinds, filter.Limit)
		if err != nil {
			return contract.AuditReport{}, err
		}
		rows = scanned
	}

	real := 0
	for _, row := range rows {
		switch {
		case row.Blocked:
			report.AllowedCount++
		case row.Pending:
			report.Violations = append(report.Violations, contract.AuditFinding{
				AtMs: row.At, Kind: row.Kind, Host: row.Host, Result: row.Result,
				Reason: ReasonUnfinished,
			})
		default:
			real++
			if st.Offline {
				report.Violations = append(report.Violations, contract.AuditFinding{
					AtMs: row.At, Kind: row.Kind, Host: row.Host, Result: row.Result,
					Reason: ReasonRealWhileOffline,
				})
			}
		}
	}

	report.Scanned = len(rows)
	report.ZeroOutbound = real == 0 && len(report.Violations) == 0

	switch {
	case !st.Offline && real > 0:
		report.Notes = append(report.Notes,
			"offline gate is not active; "+itoa(real)+" real outbound record(s) counted, not judged as violation")
	case st.Offline && real > 0:
		report.Notes = append(report.Notes,
			"offline gate is active but "+itoa(real)+" real outbound record(s) found")
	case report.ZeroOutbound:
		report.Notes = append(report.Notes, "no real outbound record in window")
	}

	return report, nil
}
