package app

import (
	"fmt"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/perf"
)

// budgetMarkerKind 把「档位变化」翻译成时间轴标记类型（TBL-10）。
//
// 只产出三种：暂停推理 / 降档 / 恢复；没有变化时返回空串（调用方跳过）。
// 判定顺序很重要：先看是否进入暂停（Suspended），再看是否恢复，最后才是降档 ——
// 「从 P 直接回到 L2」既是恢复也是降档，语义上应记为恢复（用户关心的是「又能跑了」）。
func budgetMarkerKind(prev perf.Level, b perf.BudgetState) string {
	if !b.Changed {
		return ""
	}
	switch {
	case b.Suspended:
		return contract.MarkerPerfSuspend
	case b.Resumed, prev == perf.LevelP:
		return contract.MarkerPerfResume
	case b.Throttled:
		return contract.MarkerPerfThrottle
	case b.Level == perf.LevelL0 && prev != "" && prev != perf.LevelL0:
		// 从任一降档档位回到 L0：算恢复（兜底 Resumed 未置位的实现）。
		return contract.MarkerPerfResume
	default:
		return ""
	}
}

// budgetMarkerNote 生成标记的附注。
//
// **只写机器可读 token，不拼人类文案**：界面展示由前端按 `kind` 走 i18n 渲染，
// 附注仅作诊断细节（后端不得产出人类可见中文，见项目铁律）。
func budgetMarkerNote(b perf.BudgetState) string {
	return fmt.Sprintf("level=%s cap=%d total=%.1f self=%.1f other=%.1f headroom=%.1f",
		b.Level, b.CapPct, b.TotalCpuPct, b.SelfCpuPct, b.OtherPct, b.HeadRoom)
}
