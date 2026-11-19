package hotkeys

import (
	"context"
	"encoding/json"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 精简模式（FR-09-3 / S9B-10）。仅声明现有界面的对应区域，不配置或删除业务数据。
const (
	RegionStatus          = "status"
	RegionRecord          = "record"
	RegionLibrary         = "library"
	RegionSession         = "session"
	RegionSettingsHotkeys = "settings.hotkeys"
	RegionSettingsPrivacy = "settings.privacy"
	RegionSettingsProxy   = "settings.proxy"
	RegionModelsDownloads = "models.downloads"

	RegionSettingsAdvanced = "settings.advanced"      // 性能公式说明和采样读数，保留预算滑杆与暂停提示
	RegionSettingsDiag     = "settings.diagnostics"   // 调试日志与诊断包，保留离线控制、出站记录与自检
	RegionModelsAdmin      = "models.admin"           // 仓库占用和下载临时文件等统计，保留目录、迁移和正常下载
	ControlSettingsDbStats = "settings.databaseStats" // 分表图表与元数据，保留目录和清空操作
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
	leanVisibleRegions = []string{
		RegionStatus, RegionRecord, RegionLibrary, RegionSession,
		RegionSettingsHotkeys, RegionSettingsPrivacy, RegionSettingsProxy, RegionModelsDownloads,
	}
	leanHiddenRegions  = []string{RegionSettingsAdvanced, RegionSettingsDiag, RegionModelsAdmin}
	leanHiddenControls = []string{ControlSettingsDbStats}
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
	return s.changeLeanMode(ctx, &enabled)
}

// ToggleLeanMode 由应用快捷键调用；在后端原子翻转，避免多个窗口各翻转一次。
func (s *Service) ToggleLeanMode(ctx context.Context) error {
	_, err := s.changeLeanMode(ctx, nil)
	return err
}

func (s *Service) changeLeanMode(ctx context.Context, desired *bool) (LeanState, error) {
	s.mu.Lock()
	prev := s.lean.Enabled
	enabled := !prev
	if desired != nil {
		enabled = *desired
	}
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
