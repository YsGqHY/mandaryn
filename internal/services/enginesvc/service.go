// Package enginesvc 是「模型来源与引擎」的 L3 薄壳（FR-01 / API-5.1~5.7）。
//
// 职责边界（铁律）：本层只做
//  1. 参数校验与错误码映射；
//  2. 持久化读写（委托 appsettings；密钥解密只用于组装在线引擎）；
//  3. 事件转发（EV: engine:status）；
//  4. 与录制侧的耦合点（activeSession）。
//
// **不做**任何转写 / 降级 / 重试决策 —— 那些在 internal/asr（L2）里，
// 因此本层方法都能被 Mock 掉去单测领域逻辑。
package enginesvc

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/events"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/services/appsettings"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/cryptox"
	"vip.oberon.mandaryn/internal/utils/logx"

	"github.com/wailsapp/wails/v3/pkg/application"
	"gorm.io/gorm"
)

// 在线配置的默认首字节超时（ARCH §4.2.4：8s 无首字节判超时）。
const defaultOnlineTimeoutMs int64 = 8000

// Service 暴露给前端：来源切换、热重载、端点配置与连通性测试。
type Service struct {
	holder   *storage.Holder
	settings *appsettings.Service
	router   *asr.Router
	out      *outbound.Recorder

	// activeSession 报告是否有活跃录制会话；nil 视为「永远空闲」。
	activeSession func() bool

	mu sync.Mutex // 串行化「读-改-写」型操作（SetSource / SetOnlineConfig / SetFallbackPolicy）
}

// New 构造服务。
//
// router 必填（装配在 internal/app：那里注入 BundleLocator 与 activeSession）；
// settings 用于落配置；out 用于出站留痕（可为 nil）。
//
// 构造时会把降级策略推给 router，避免「库里是 true、运行期还是零值」的漂移。
func New(holder *storage.Holder, settings *appsettings.Service, router *asr.Router,
	out *outbound.Recorder, activeSession func() bool) (*Service, error) {

	if router == nil {
		return nil, apierr.New(apierr.CodeEngineLoad).WithDetail("enginesvc 未拿到 Router")
	}
	if settings == nil {
		return nil, apierr.New(apierr.CodeEngineLoad).WithDetail("enginesvc 未拿到 appsettings")
	}
	if holder == nil {
		return nil, apierr.New(apierr.CodeDBWrite).WithDetail("enginesvc 未拿到 storage.Holder")
	}

	s := &Service{
		holder:        holder,
		settings:      settings,
		router:        router,
		out:           out,
		activeSession: activeSession,
	}
	// 状态变化 → EV: engine:status（L2 不发 Wails 事件，由这里转发）。
	router.SetOnStatus(func(st contract.EngineStatus) {
		if app := application.Get(); app != nil {
			app.Event.Emit(events.EngineStatus, st)
		}
	})

	// 启动即把持久化的降级策略灌进 router。
	if snap, err := settings.Get(context.Background()); err == nil {
		router.SetPolicy(contract.FallbackPolicy{
			FallbackToLocal: snap.FallbackToLocal,
			AutoSwitchBack:  snap.AutoSwitchBack,
			MaxRetries:      snap.MaxRetries,
		})
	}
	// 启动即注入在线工厂：否则在用户重新保存配置前，router 手里没有在线实现，
	// 降级矩阵里的「在线」一侧永远不可用（表现为杯测/降级路径失效）。
	router.SetOnlineFactory(s.onlineFactory())
	return s, nil
}

func (s *Service) db() *gorm.DB {
	return s.holder.Current().GORM
}

// —— API-5.1 ——

// GetStatus 返回引擎状态（ready/loading/failed/degraded + 详情）。
func (s *Service) GetStatus(_ context.Context) (contract.EngineStatus, error) {
	return s.router.Status(), nil
}

// —— API-5.2 ——

// SetSource 切换模型来源（local | online）。
//
// 顺序很关键：**先**热切换引擎，成功后再落库 ——
// 否则一次失败的切换会把用户设置改成一个跑不起来的来源。
//
// 可能错误：ERR_STATE_FORBIDDEN（录制中）、ERR_SETTINGS_CONSENT_REQUIRED（未确认在线隐私）、
// ERR_ENGINE_LOAD（目标引擎加载失败）、ERR_INVALID_ARGUMENT / ERR_MODEL_NOT_FOUND。
func (s *Service) SetSource(ctx context.Context, source string) (contract.EngineStatus, error) {
	source = strings.TrimSpace(source)
	switch source {
	case contract.ModelSourceLocal, contract.ModelSourceOnline:
	default:
		return contract.EngineStatus{}, apierr.Newf(apierr.CodeInvalidArgument,
			"model source must be local or online")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if source == contract.ModelSourceOnline {
		if err := s.requireOnlineReady(ctx); err != nil {
			return s.router.Status(), err
		}
	}

	if err := s.router.SetSource(source); err != nil {
		// router 已把状态置 failed 并回滚来源；错误码原样上抛。
		return s.router.Status(), apierr.From(err, apierr.CodeEngineLoad)
	}
	if err := s.settings.SetModelSource(ctx, source); err != nil {
		return s.router.Status(), err
	}
	logx.For("enginesvc").Info("模型来源已切换", "source", source,
		"engineId", s.router.ID())
	return s.router.Status(), nil
}

// —— API-5.3 ——

// Reload 强制重载当前引擎（换模型包 / 改端点后用）。
func (s *Service) Reload(ctx context.Context) (contract.EngineStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.router.Reload(); err != nil {
		return s.router.Status(), apierr.From(err, apierr.CodeEngineLoad)
	}
	return s.router.Status(), nil
}

// —— API-5.4 ——

// TestOnline 做一次连通性 + 延迟 + 错误分类测试。
//
// 走 httpx 并落 outbound 留痕；MANDARYN_OFFLINE=1 时**不发请求**，直接返回
// ERR_OFFLINE_ENFORCED（前端据此提示「已开启离线模式」）。
func (s *Service) TestOnline(ctx context.Context, cfg contract.OnlineConfig) (contract.TestResult, error) {
	if err := outbound.Guard(); err != nil {
		return contract.TestResult{
			OK:       false,
			Category: "unknown",
			Note:     "离线模式已开启，出站请求被拦截",
		}, apierr.From(err, apierr.CodeOfflineEnforced)
	}

	opt, err := s.onlineOptions(ctx, cfg)
	if err != nil {
		return contract.TestResult{OK: false, Category: "unknown", Note: err.Error()}, err
	}
	return asr.ProbeOnline(ctx, opt)
}

// —— API-5.5 ——

// SetOnlineConfig 保存在线端点（需已对当前域名做过隐私确认）。
//
// 校验与加密落库都委托 appsettings（它已实现 consent 绑定域名 + cryptox 加密），
// 本方法只多做一件事：让 router 丢掉缓存的旧在线引擎，避免继续用旧密钥 / 旧模型。
func (s *Service) SetOnlineConfig(ctx context.Context, cfg contract.OnlineConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.settings.SetOnlineConfig(ctx, cfg); err != nil {
		return apierr.From(err, apierr.CodeSettingsConsentRequired)
	}
	s.router.SetOnlineFactory(s.onlineFactory())
	logx.For("enginesvc").Info("在线端点已更新", "endpoint", safeHost(cfg.Endpoint),
		"model", cfg.Model)
	return nil
}

// —— API-5.6 ——

// GetOnlineConfig 返回在线端点配置（密钥为掩码 sk-***last4）。
func (s *Service) GetOnlineConfig(ctx context.Context) (contract.OnlineConfig, error) {
	cfg, err := s.settings.GetOnlineConfig(ctx)
	if err != nil {
		return contract.OnlineConfig{}, apierr.From(err, apierr.CodeDBWrite)
	}
	if cfg.TimeoutMs <= 0 {
		cfg.TimeoutMs = defaultOnlineTimeoutMs
	}
	return cfg, nil
}

// —— API-5.7 ——

// SetFallbackPolicy 保存降级策略。
//
// MaxRetries 会随另外两个布尔一起落库（app_settings.max_retries），
// 重启后经 Snapshot 读回并灌进 router，不再退回默认值。
func (s *Service) SetFallbackPolicy(ctx context.Context, p contract.FallbackPolicy) error {
	if p.MaxRetries < 0 || p.MaxRetries > asr.MaxRetriesCap {
		return apierr.Newf(apierr.CodeInvalidArgument,
			"maxRetries must be within 0-%d", asr.MaxRetriesCap)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.settings.SetFallbackPolicy(ctx, p); err != nil {
		return apierr.From(err, apierr.CodeInvalidArgument)
	}
	s.router.SetPolicy(p)
	return nil
}

// String 让 Wails 的服务名稳定（与其它 service 一致）。
func (s *Service) String() string { return "enginesvc" }

// —— 内部 ——

// requireOnlineReady 校验「切到在线」的前置条件：端点已配 + 当前域名已确认。
func (s *Service) requireOnlineReady(ctx context.Context) error {
	snap, err := s.settings.Get(ctx)
	if err != nil {
		return apierr.From(err, apierr.CodeDBWrite)
	}
	if strings.TrimSpace(snap.OnlineEndpoint) == "" {
		return apierr.New(apierr.CodeInvalidArgument).
			WithAction("configure-online").
			WithDetail("在线端点未配置")
	}
	host := safeHost(snap.OnlineEndpoint)
	if snap.OnlineConsentAt == 0 || !strings.EqualFold(snap.OnlineConsentHost, host) {
		// 未确认 / 域名变了：必须重新做隐私确认（FR-08-2）。
		return apierr.New(apierr.CodeSettingsConsentRequired).
			WithDetail("consent required for host %s", host)
	}
	return nil
}

// onlineOptions 组装在线引擎参数（密钥用明文，仅在内存中短暂存在）。
//
// cfg 可能是前端传来的（含掩码密钥 / 空密钥），规则：
//   - 端点 / 模型为空 → 用库里的值；
//   - 密钥为空或是掩码 → 用库里解密出的明文。
func (s *Service) onlineOptions(ctx context.Context, cfg contract.OnlineConfig) (asr.OnlineOptions, error) {
	row, err := s.loadRow(ctx)
	if err != nil {
		return asr.OnlineOptions{}, err
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = row.OnlineEndpoint
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = row.OnlineModel
	}
	language := strings.TrimSpace(cfg.Language)
	if language == "" {
		language = row.OnlineLanguage
	}
	if language == "" {
		language = "auto"
	}
	timeout := cfg.TimeoutMs
	if timeout <= 0 {
		timeout = defaultOnlineTimeoutMs
	}

	key, err := s.plainAPIKey(row, cfg.APIKey)
	if err != nil {
		return asr.OnlineOptions{}, err
	}

	return asr.OnlineOptions{
		Endpoint:  endpoint,
		Model:     model,
		Language:  language,
		APIKey:    key,
		TimeoutMs: timeout,
		ProxyURL:  proxyURLFor(row, true),
		Recorder:  s.out,
	}, nil
}

// onlineFactory 返回「每次调用都读最新库值」的在线引擎工厂。
//
// 必须是读取式闭包：换端点 / 换密钥后 Router 重建引擎才能拿到新配置。
func (s *Service) onlineFactory() asr.OnlineFactory {
	return func() (asr.Engine, error) {
		opt, err := s.onlineOptions(context.Background(), contract.OnlineConfig{})
		if err != nil {
			return nil, err
		}
		return asr.NewOnlineEngine(opt)
	}
}

// loadRow 读取 app_settings 单行表。
func (s *Service) loadRow(ctx context.Context) (storage.AppSettings, error) {
	var row storage.AppSettings
	if err := s.db().WithContext(ctx).First(&row, "id = ?", 1).Error; err != nil {
		return row, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return row, nil
}

// plainAPIKey 解析出明文密钥：优先用调用方新输入的明文，否则解密库里的密文。
func (s *Service) plainAPIKey(row storage.AppSettings, incoming string) (string, error) {
	incoming = strings.TrimSpace(incoming)
	if incoming != "" && !strings.Contains(incoming, "***") {
		return incoming, nil
	}
	if row.OnlineAPIKeyEnc == "" {
		return "", nil
	}
	plain, err := cryptox.DecryptString(row.OnlineAPIKeyEnc)
	if err != nil {
		// 坏密文不应让整个引擎不可用：当成未配置密钥，让端点自己回 401。
		logx.For("enginesvc").Warn("在线密钥解密失败，按未配置处理")
		return "", nil
	}
	return plain, nil
}

// proxyURLFor 把代理配置拼成 URL（密码只进 URL userinfo，不写日志）。
//
// useOnline=false 时表示「该代理不用于在线转写」，返回空。
func proxyURLFor(row storage.AppSettings, useOnline bool) string {
	if row.ProxyType == "" || row.ProxyType == contract.ProxyNone {
		return ""
	}
	if useOnline && !row.ProxyForOnline {
		return ""
	}
	scheme := row.ProxyType
	switch scheme {
	case contract.ProxyHTTP, contract.ProxyHTTPS, contract.ProxySOCKS5:
	default:
		return ""
	}
	u := url.URL{
		Scheme: scheme,
		Host:   row.ProxyHost + ":" + strconv.Itoa(row.ProxyPort),
	}
	if row.ProxyUsername != "" {
		pass := ""
		if row.ProxyPasswordEnc != "" {
			if p, err := cryptox.DecryptString(row.ProxyPasswordEnc); err == nil {
				pass = p
			}
		}
		u.User = url.UserPassword(row.ProxyUsername, pass)
	}
	return u.String()
}

// safeHost 取域名（用于日志 / 错误 Detail，避免把完整 URL 与 query 写出去）。
func safeHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
