package hotkeys

import (
	"context"
	"encoding/json"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 精简模式的后端侧支撑（FR-09-3 / DOC-05 §4、§5.1）。
//
// 边界：本文件**不做渲染**。它只回答两件前端无法自行裁决的事：
//  1. 精简模式现在是开还是关（跨会话持久化，只能走后端 SQLite）；
//  2. 开启时哪些区域 / 控件应当隐去 —— 这份清单是产品取舍（DOC-05 §4 的
//     「移除 7 项、合并 5 组」）的后端权威副本，前端不得各页各写一份。

// 可见区域键（精简模式**保留**的部分）。
//
// 依据 DOC-05 §5.1 三层信息层级：L1 全局状态 + L2 当前任务必须常在，
// L3 只留一个入口（设置），其余高级面板在精简模式下一律隐去。
const (
	RegionStatus  = "status"  // L1 顶部状态条（录制 / 降级 / 错误）
	RegionRecord  = "record"  // L2 录制页主体
	RegionLibrary = "library" // L2 会话库列表
	RegionSession = "session" // L2 会话详情（回放 + 歌词）
)

// 隐去区域键（精简模式**不显示**的部分）。
const (
	RegionSettingsAdvanced = "settings.advanced" // 设置页「高级」分区（采样率 / VAD / 分片等实现细节）
	RegionSettingsDiag     = "settings.diagnostics"
	RegionModelsAdmin      = "models.admin"     // 模型包内部信息（校验和 / 分片进度等）
	RegionLibraryInsights  = "library.insights" // 会话库统计与趋势
	RegionDiagnostics      = "diagnostics"      // 诊断面板（日志级别、出站记录）
)

// 隐去控件键（区域里仍保留、但精简模式收起的零散控件）。
const (
	ControlStatusBadges        = "status.secondaryBadges" // 状态条上的次要徽章（模型档位 / 队列长度）
	ControlLibraryBulk         = "library.bulkActions"    // 批量选择与批量删除
	ControlSessionExportOpts   = "session.exportOptions"  // 导出格式等次要选项（导出动作本身保留）
	ControlRecordSourceHints   = "record.sourceHints"     // 源列表的技术提示（采样率 / 通道数）
	ControlSettingsUpdateOpts  = "settings.updateOptions" // 更新检查频率等（DOC-05 §4.2 固定策略）
	ControlSettingsDbStats     = "settings.databaseStats" // 表行数统计
	ControlSettingsSecurityLog = "settings.securityNotes" // 安全说明长文
)

// LeanState 是精简模式的完整状态（API-8.x 补充：GetLeanState / SetLeanMode）。
//
// 三个清单**恒定返回**（与 Enabled 无关）：前端拿同一份清单，按 Enabled 决定
// 用不用它。这样「精简模式」的取舍只有一处定义，也不存在「开关来回切后
// 清单漂移」的可能。
type LeanState struct {
	// Enabled 精简模式是否开启（持久化在 preferences.ux.leanMode）。
	Enabled bool `json:"enabled"`
	// VisibleRegions 精简模式下保留的区域。
	VisibleRegions []string `json:"visibleRegions"`
	// HiddenRegions 精简模式下隐去的区域。
	HiddenRegions []string `json:"hiddenRegions"`
	// HiddenControls 精简模式下隐去的零散控件。
	HiddenControls []string `json:"hiddenControls"`
}

// leanStorage 是 LeanState 的持久化形态（只存开关，清单在代码里）。
type leanStorage struct {
	Enabled bool `json:"enabled"`
}

// defaultLeanState 默认关闭（P1：默认值即最佳实践，首次安装要看到完整能力）。
func defaultLeanState() LeanState {
	return LeanState{
		Enabled:        false,
		VisibleRegions: copyStrings(leanVisibleRegions),
		HiddenRegions:  copyStrings(leanHiddenRegions),
		HiddenControls: copyStrings(leanHiddenControls),
	}
}

var (
	leanVisibleRegions = []string{RegionStatus, RegionRecord, RegionLibrary, RegionSession}
	leanHiddenRegions  = []string{
		RegionSettingsAdvanced,
		RegionSettingsDiag,
		RegionModelsAdmin,
		RegionLibraryInsights,
		RegionDiagnostics,
	}
	leanHiddenControls = []string{
		ControlStatusBadges,
		ControlLibraryBulk,
		ControlSessionExportOpts,
		ControlRecordSourceHints,
		ControlSettingsUpdateOpts,
		ControlSettingsDbStats,
		ControlSettingsSecurityLog,
	}
)

// GetLeanState 返回精简模式状态与取舍清单。
func (s *Service) GetLeanState(_ context.Context) (LeanState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leanSnapshotLocked(), nil
}

// SetLeanMode 开关精简模式（幂等；立刻持久化并广播）。
//
// 可能错误：ERR_DB_WRITE（持久化失败时内存态回滚，避免出现
// 「界面上开着、重启后变了」的假状态）。
func (s *Service) SetLeanMode(ctx context.Context, enabled bool) (LeanState, error) {
	s.mu.Lock()
	prev := s.lean.Enabled
	s.lean.Enabled = enabled
	if err := s.persistLeanLocked(ctx); err != nil {
		// 持久化失败就回滚内存态：否则界面显示已开启、重启后又变回关闭。
		s.lean.Enabled = prev
		out := s.leanSnapshotLocked()
		s.mu.Unlock()
		return out, err
	}
	out := s.leanSnapshotLocked()
	s.mu.Unlock()

	if prev != enabled {
		logx.For("hotkeys").Info("精简模式已切换", "enabled", enabled)
	}
	s.emitLean(out)
	return out, nil
}

// leanSnapshotLocked 返回带清单副本的快照（调用方持有读锁）。
func (s *Service) leanSnapshotLocked() LeanState {
	out := s.lean
	out.VisibleRegions = copyStrings(leanVisibleRegions)
	out.HiddenRegions = copyStrings(leanHiddenRegions)
	out.HiddenControls = copyStrings(leanHiddenControls)
	return out
}

// persistLeanLocked 写 preferences.ux.leanMode（调用方持锁）。
func (s *Service) persistLeanLocked(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	raw, err := json.Marshal(leanStorage{Enabled: s.lean.Enabled})
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	if err := s.store.Set(ctx, KeyLeanMode, string(raw)); err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return nil
}

// loadLeanLocked 从存储恢复精简模式（坏数据按默认关闭处理，不阻断启动）。
func (s *Service) loadLeanLocked(ctx context.Context) {
	s.lean = defaultLeanState()
	if s.store == nil {
		return
	}
	raw, err := s.store.Get(ctx, KeyLeanMode)
	if err != nil {
		logx.For("hotkeys").Warn("读取精简模式失败，按默认关闭处理")
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	var st leanStorage
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		logx.For("hotkeys").Warn("精简模式数据无法解析，按默认关闭处理")
		return
	}
	s.lean.Enabled = st.Enabled
}

// emitLean 转发精简模式变更（nil 安全）。
func (s *Service) emitLean(st LeanState) {
	if s.emit.Lean != nil {
		s.emit.Lean(st)
	}
}

// copyStrings 返回切片副本（避免调用方改到包级常量表）。
func copyStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
