// Package capturesvc 是「音频源」的 Wails 薄壳（API-1.1 ~ API-1.6 / FR-02）。
//
// 职责边界（CLAUDE.md 铁律）：
//   - 本层**只做**类型转换、错误码映射与参数校验的收口；
//   - 所有平台相关的采集逻辑都在 internal/capture（L2 领域包），
//     这里不许出现 build tag、COM、WASAPI 之类的字眼；
//   - 无状态：不缓存源列表（Present/Producing 必须实时），
//     只持久化「上次选择的源」这一个用户偏好。
package capturesvc

import (
	"context"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/services/preferences"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// prefKeyPreferredSource 上次选择的源 ID（FR-02-5）。
//
// 命名沿用 "领域.键" 的既有习惯（与 preferences 表里的布局、主题键同风格）。
const prefKeyPreferredSource = "capture.preferredSource"

// Service 暴露给前端的音频源服务。
type Service struct {
	holder *storage.Holder
	prefs  *preferences.Service

	// monitor「源可用性变化」的 2s 轮询器，仅在装配层注入回调后才启动。
	// 本层不直接 import events 包：events 依赖 Wails，用回调注入可以让
	// capturesvc 保持可单测。
	monitor *capture.Monitor
}

// New 构造服务。
//
// 依赖 *storage.Holder 而不是 *storage.DB：切换数据目录会替换 DB 句柄，
// 持有 holder 才能始终读到当前活跃实例（docs/PERSISTENCE.md）。
//
// 偏好读写直接复用 preferences 包，不另造一套 gorm 代码。
//
// onSourcesChanged 是装配期注入的「源列表变化」回调（应用启动/退出、设备增减）。
// 用构造注入而不是导出的 `SetSourcesChangedHandler(fn func(...))`，原因有两条：
//   - func 类型参数会触发 Wails 绑定生成器警告，并生成无法调用的畸形前端方法；
//   - 这类生命周期钩子不该出现在前端 API 面上。
//
// 传 nil 则不启动后台轮询，行为退化为「只在 UI 请求时枚举」。
func New(holder *storage.Holder, onSourcesChanged func([]contract.Source)) *Service {
	s := &Service{
		holder: holder,
		prefs:  preferences.New(holder),
	}
	if onSourcesChanged != nil {
		s.monitor = capture.NewMonitor(capture.DefaultPollInterval, onSourcesChanged)
		s.monitor.Start()
	}
	return s
}

// ServiceShutdown 是 Wails 的服务生命周期钩子（不是前端 API，绑定生成器会跳过它）。
// 应用退出时停掉后台轮询，幂等。
func (s *Service) ServiceShutdown() error {
	if s.monitor != nil {
		s.monitor.Stop()
	}
	return nil
}

// ListSources 枚举应用源 + 设备源；Present/Producing 为实时值（API-1.1）。
//
// 可能错误：ERR_CAPTURE_PERMISSION（macOS 未授权）、ERR_CAPTURE_UNAVAILABLE
// （音频服务异常 / 平台未实现）。
func (s *Service) ListSources(_ context.Context) ([]contract.Source, error) {
	return s.enumerate()
}

// RefreshSources 强制重新枚举（API-1.2，UI「刷新」按钮）。
//
// 与 ListSources 走同一条枚举路径（本层不缓存），区别只在于语义：
// 用户显式要求刷新，因此会打一条 info 日志便于排查「点了没反应」的反馈。
func (s *Service) RefreshSources(_ context.Context) ([]contract.Source, error) {
	sources, err := s.enumerate()
	if err != nil {
		return nil, err
	}
	logx.For("capturesvc").Info("手动刷新音频源", "count", len(sources))
	return sources, nil
}

// ProbeSource 探测单个源当前是否可用 / 有声（API-1.3）。
//
// 可能错误：ERR_INVALID_ARGUMENT（ID 格式非法）、ERR_SOURCE_NOT_FOUND（源已消失）。
func (s *Service) ProbeSource(_ context.Context, sourceID string) (contract.Source, error) {
	if strings.TrimSpace(sourceID) == "" {
		return contract.Source{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sourceId 不能为空")
	}
	src, err := capture.Probe(sourceID)
	if err != nil {
		return contract.Source{}, apierr.From(err, apierr.CodeCaptureUnavailable)
	}
	return src, nil
}

// SetPreferredSource 记住上次选择的源（API-1.4 / FR-02-5）。
//
// 可能错误：ERR_SOURCE_NOT_FOUND（源当前不可用）。
//
// 为什么要校验存在性：如果允许记忆一个不存在的源，下次启动会拿着一个
// 必然失败的 ID 去开录制，用户只会看到「录制失败」而不知道原因。
func (s *Service) SetPreferredSource(ctx context.Context, sourceID string) error {
	id := strings.TrimSpace(sourceID)
	if id == "" {
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sourceId 不能为空")
	}
	// 先校验格式（不依赖平台），再校验当前是否存在。
	if _, _, err := capture.ParseSourceID(id); err != nil {
		return err
	}
	if _, err := capture.Probe(id); err != nil {
		return apierr.From(err, apierr.CodeSourceNotFound)
	}
	if err := s.prefs.Set(ctx, prefKeyPreferredSource, id); err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return nil
}

// GetPreferredSource 返回上次选择的源 ID（API-1.5）；未设置过返回空字符串。
func (s *Service) GetPreferredSource(ctx context.Context) (string, error) {
	value, err := s.prefs.Get(ctx, prefKeyPreferredSource)
	if err != nil {
		return "", apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return value, nil
}

// GetCapabilities 返回平台能力矩阵（API-1.6）。
//
// 这是 UI 决定「是否展示按应用分路选项」的唯一依据，因此必须在
// Reason 里给出含系统版本号的具体原因（见 internal/capture/caps_*.go）。
func (s *Service) GetCapabilities(_ context.Context) (contract.CaptureCaps, error) {
	return capture.Capabilities(), nil
}

// enumerate 统一枚举入口 + 错误码映射。
func (s *Service) enumerate() ([]contract.Source, error) {
	sources, err := capture.Enumerate()
	if err != nil {
		return nil, apierr.From(err, apierr.CodeCaptureUnavailable)
	}
	// 空列表不是错误：本机确实可能没有任何活动输出设备（远程桌面 / 安全模式）。
	// 返回空切片而不是 nil，前端无需为 null 做额外判空。
	if sources == nil {
		sources = []contract.Source{}
	}
	return sources, nil
}
