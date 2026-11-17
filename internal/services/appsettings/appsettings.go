// Package appsettings 提供「应用设置」单行表的读写（API-9.x）。
//
// 单行表（CHECK id=1）保存「必有且唯一」的配置项：主题、语言、模型来源与在线端点、
// 代理、音频与录制、隐私、整机性能预算。
//
// 与 preferences 的区别：
//   - 这里字段是已知有限集，schema 强校验（DOC-04 TBL-02）
//   - preferences 是开放键值，业务方可以随时塞新键
//
// 安全约束（FR-08 / NFR-03）：
//   - 在线密钥与代理密码必须经 cryptox 加密后落库，界面只显掩码；
//   - 在线端点变更后需要重新做风险确认（onlineConsentHost 绑定域名）；
//   - 任何写入都不得把密文 / 明文写进日志。
package appsettings

import (
	"context"
	"net/url"
	"os"
	"strings"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/events"
	"vip.oberon.mandaryn/internal/models"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/cryptox"
	"vip.oberon.mandaryn/internal/utils/logx"

	"github.com/wailsapp/wails/v3/pkg/application"
	"gorm.io/gorm"
)

// Service 是 app_settings 的唯一读写入口。
type Service struct {
	holder *storage.Holder
	out    *outbound.Recorder
	// usage 读整机性能读数（NFR-08）。装配层注入 perf.Service.Usage；
	// 未注入时 API-9.9 只能返回配置值（本域不自行采样，避免两套 CPU 口径）。
	//
	// 为什么用**构造注入**而不是导出 SetUsageProvider(func)：L3 服务上任何
	// func 型导出方法都会被绑定生成器扫进前端 API，并触发
	// 「function types are not supported by encoding/json」警告。
	usage func() contract.PerfUsage
	// activeSession 报告是否有活跃录制会话（FR-03-10：录制中禁止切边录边转）。
	//
	// 与 usage 同理用构造注入。nil = 不做该检查（单测与早期装配可用）。
	activeSession func() bool
	// debugMode 把用户层的调试模式开关落实到运行时（装配层接 privacy.DebugMode.SetUser），
	// 返回生效状态（可能被环境变量 MANDARYN_LOG_LEVEL 接管）。nil = 只落库。
	debugMode func(userEnabled bool) (contract.DebugModeState, error)
	// proxyTest 测试代理连通（API-9.4）。nil = 未接入模型域，返回 ERR_UNSUPPORTED。
	proxyTest func(ctx context.Context, p contract.ProxyConfig) (contract.TestResult, error)
}

// Option 是可选依赖的注入方式。
//
// 为什么用变参而不是加构造参数：`New` 已被装配层调用，改签名会让
// 所有调用点（含测试）一起改；变参保证既有调用**零改动**且行为不变。
type Option func(*Service)

// WithActiveSession 注入「是否有活跃录制会话」判定。
//
// 装配层传与 enginesvc 同一个闭包（recording/暂停都算活跃）：
// 边录边转开关只在**下一个会话**生效，录制中切换必须被拒绝（ERR_STATE_FORBIDDEN）。
func WithActiveSession(fn func() bool) Option {
	return func(s *Service) { s.activeSession = fn }
}

// WithDebugMode 注入调试模式的运行时落实函数（见 Service.debugMode）。
func WithDebugMode(fn func(userEnabled bool) (contract.DebugModeState, error)) Option {
	return func(s *Service) { s.debugMode = fn }
}

// WithProxyTester 注入代理连通性测试（API-9.4）。
//
// 装配层接 `modelsSvc.Manager().TestProxy`。为什么用构造注入而不是在这里
// 自己发请求：测试必须复用**下载链路同一套** Transport 与同一份清单地址，
// 否则「测通了但下载不通」的假阳性就无从发现（见 internal/models/proxytest.go）。
//
// 与 usage / debugMode 同理用构造注入：L3 服务上的 func 型导出方法会被绑定
// 生成器扫进前端 API 并触发「function types are not supported」警告。
func WithProxyTester(fn func(ctx context.Context, p contract.ProxyConfig) (contract.TestResult, error)) Option {
	return func(s *Service) { s.proxyTest = fn }
}

// New 构造服务；outbound 记录器可为 nil（仅影响「最近出站点」面板）。
// usage 可为 nil（未接入预算域时 SetResourceCap 只回配置值）。
func New(holder *storage.Holder, out *outbound.Recorder, usage func() contract.PerfUsage,
	opts ...Option) *Service {
	s := &Service{holder: holder, out: out, usage: usage}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

func (s *Service) db() *gorm.DB {
	return s.holder.Current().GORM
}

// Snapshot 是对外暴露的整体快照，前端启动时一次拉取。
//
// 嵌套结构复用了 contract 里的配置 DTO，保持与写入接口同一套字段名，
// 避免「读一个结构、写另一个结构」的漂移。
type Snapshot struct {
	// —— 既有：主题与语言 ——
	ThemeChoice  string `json:"themeChoice"`
	CustomTheme  string `json:"customTheme"` // JSON: { mode, palette }；空字符串=未设置
	LocaleChoice string `json:"localeChoice"`

	// —— 模型来源与在线端点 ——
	ModelSource        string `json:"modelSource"`
	OnlineEndpoint     string `json:"onlineEndpoint"`
	OnlineModel        string `json:"onlineModel"`
	OnlineLanguage     string `json:"onlineLanguage"`
	OnlineAPIKeyMasked string `json:"onlineApiKeyMasked"`
	OnlineConsentAt    int64  `json:"onlineConsentAt"`
	OnlineConsentHost  string `json:"onlineConsentHost"`
	FallbackToLocal    bool   `json:"fallbackToLocal"`
	AutoSwitchBack     bool   `json:"autoSwitchBack"`
	MaxRetries         int    `json:"maxRetries"`

	// —— 代理（密码为掩码）——
	Proxy contract.ProxyConfig `json:"proxy"`

	// ProxyEnvVar 是**接管代理配置的环境变量名**（`MANDARYN_HTTP_PROXY`）。
	//
	// 非空即表示设置页的代理配置当前**不生效** —— 模型下载与清单获取走该变量。
	// 为什么由后端给而不是前端写死：变量名属于后端契约（models.EnvHTTPProxy），
	// 前端硬编码会在改名时静默失配，用户看到的提示就变成谎话。
	// 沿用 privacy 域 `OfflineEnvVar` 的同一做法。
	ProxyEnvVar string `json:"proxyEnvVar"`

	// —— 音频与录制 ——
	Audio contract.AudioSettings `json:"audio"`

	// LiveTranscribe 边录边转开关（FR-03-10，默认 false）。
	//
	// 为什么单独放在快照顶层而不是塞进 AudioSettings：`contract.AudioSettings`
	// 是已冻结的 API-9.6 载荷（前端按它整体回写 SetAudio），把这个开关混进去
	// 会让「只想改格式」的调用顺手把开关重置掉。这里只读，写入走 SetLiveTranscribe。
	LiveTranscribe bool `json:"liveTranscribe"`

	// —— 隐私 ——
	Privacy contract.PrivacySettings `json:"privacy"`

	// —— 性能（NFR-08）——
	ResourceCapPct int `json:"resourceCapPct"`
}

// SetLiveTranscribe 切换「边录边转」（API-9.10 / FR-03-10，默认关）。
//
// 语义（三条，都会影响用户预期）：
//  1. **只在下一个会话生效**：录制中不重读开关（Recorder 在 Start 时读一次），
//     因此录制期间**两个方向都拒绝**，返回 ERR_STATE_FORBIDDEN —— 与模型来源切换同规则。
//     若静默接受，用户会看到「开关开了/关了，但当前这场没变」，那是更糟的体验；
//     而且录制中关掉也无法真正停掉已经在跑的旁路（它绑在会话上）。
//  2. 关掉它**不影响录完再转**：延后转写是独立任务，与这条开关无关。
//
// 可用性（有没有流式模型包）不在这里判定：配置是配置，能力是能力。
// 前端通过 transcriptsvc.LiveAvailability 拿能力，两者分开显示。
func (s *Service) SetLiveTranscribe(ctx context.Context, enabled bool) error {
	if s.activeSession != nil && s.activeSession() {
		return apierr.New(apierr.CodeStateForbidden).
			WithAction("stop-recording").
			WithDetail("录制中不能切换边录边转，请停止录制后设置（只对下一个会话生效）")
	}
	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Update("live_transcribe", enabled).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	s.emit("liveTranscribe")
	logx.For("appsettings").Info("边录边转开关已更新", "enabled", enabled)
	return nil
}

// Get 一次性返回完整快照（密钥 / 密码只返回掩码）。
func (s *Service) Get(ctx context.Context) (Snapshot, error) {
	var row storage.AppSettings
	if err := s.db().WithContext(ctx).First(&row, "id = ?", 1).Error; err != nil {
		return Snapshot{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return Snapshot{
		ThemeChoice:        row.ThemeChoice,
		CustomTheme:        row.CustomTheme,
		LocaleChoice:       row.LocaleChoice,
		ModelSource:        row.ModelSource,
		OnlineEndpoint:     row.OnlineEndpoint,
		OnlineModel:        row.OnlineModel,
		OnlineLanguage:     row.OnlineLanguage,
		OnlineAPIKeyMasked: maskSecret(mustDecrypt(row.OnlineAPIKeyEnc)),
		OnlineConsentAt:    row.OnlineConsentAt,
		OnlineConsentHost:  row.OnlineConsentHost,
		FallbackToLocal:    row.FallbackToLocal,
		AutoSwitchBack:     row.AutoSwitchBack,
		MaxRetries:         row.MaxRetries,
		Proxy: contract.ProxyConfig{
			Type:           row.ProxyType,
			Host:           row.ProxyHost,
			Port:           row.ProxyPort,
			Username:       row.ProxyUsername,
			Password:       maskSecret(mustDecrypt(row.ProxyPasswordEnc)),
			UseForDownload: row.ProxyForDownload,
			UseForOnline:   row.ProxyForOnline,
			NoProxy:        row.ProxyNoProxy,
		},
		Audio: contract.AudioSettings{
			SaveFormat:      row.SaveFormat,
			Sensitivity:     row.Sensitivity,
			MaxSpeakers:     row.MaxSpeakers,
			DefaultSourceID: row.DefaultSourceID,
			AutoName:        row.AutoName,
		},
		Privacy: contract.PrivacySettings{
			NoAudioPersist: row.NoAudioPersist,
			LogLevel:       row.LogLevel,
		},
		// 只在变量**真的设了值**时才算接管：空串等同于未配置（与 models 侧
		// globalProxyFromEnv 的判定一致），否则界面会误报「已由环境变量接管」。
		ProxyEnvVar: proxyEnvVar(),
		LiveTranscribe: row.LiveTranscribe,
		ResourceCapPct: row.ResourceCapPct,
	}, nil
}

// —— 既有：主题与语言 ——

// SetThemeChoice 仅更新主题选择。
func (s *Service) SetThemeChoice(ctx context.Context, choice string) error {
	return s.update(ctx, "theme_choice", "themeChoice", choice)
}

// SetCustomTheme 写入自定义主题 JSON（前端 stringify 后传入；空串 = 未设置）。
func (s *Service) SetCustomTheme(ctx context.Context, json string) error {
	return s.update(ctx, "custom_theme", "customTheme", json)
}

// ResetCustomTheme 清空自定义主题。
func (s *Service) ResetCustomTheme(ctx context.Context) error {
	return s.SetCustomTheme(ctx, "")
}

// SetLocaleChoice 'auto' | LocaleCode。
func (s *Service) SetLocaleChoice(ctx context.Context, choice string) error {
	return s.update(ctx, "locale_choice", "localeChoice", choice)
}

// —— 模型来源与在线端点（FR-01 / FR-08-2）——

// SetModelSource 切换模型来源（API-9.2）。
//
// 真正的生效（引擎预热、录制中拒绝）在 enginesvc（FR-01-2）；本方法只落配置，
// 因此这里只做取值校验。录制中禁用由 enginesvc 判定。
func (s *Service) SetModelSource(ctx context.Context, source string) error {
	if source != contract.ModelSourceLocal && source != contract.ModelSourceOnline {
		return apierr.Newf(apierr.CodeInvalidArgument, "model source must be local or online")
	}
	return s.update(ctx, "model_source", "modelSource", source)
}

// SetOnlineConfig 保存在线端点（API-5.5）。
//
// 强制约束（NFR-03-3）：未对**当前端点域名**做过风险确认时拒绝保存，
// 返回 ERR_SETTINGS_CONSENT_REQUIRED。端点域名变更后需重新确认。
func (s *Service) SetOnlineConfig(ctx context.Context, cfg contract.OnlineConfig) error {
	if cfg.Endpoint == "" {
		return apierr.Newf(apierr.CodeInvalidArgument, "endpoint is required")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return apierr.Newf(apierr.CodeInvalidArgument, "endpoint must be a valid URL")
	}
	if u.Scheme != "https" {
		// 不允许关闭传输层保护（ARCH §8.2：证书校验不可关闭）。
		return apierr.Newf(apierr.CodeInvalidArgument, "endpoint must use https")
	}

	var row storage.AppSettings
	if err := s.db().WithContext(ctx).First(&row, "id = ?", 1).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	if row.OnlineConsentAt == 0 || !strings.EqualFold(row.OnlineConsentHost, u.Host) {
		return apierr.New(apierr.CodeSettingsConsentRequired).
			WithDetail("consent required for host %s", u.Host)
	}

	// 密钥：传空或传回掩码时保留原值（前端只拿到掩码，不应要求重新输入）。
	keyEnc := row.OnlineAPIKeyEnc
	if cfg.APIKey != "" && !isMasked(cfg.APIKey) {
		enc, encErr := cryptox.EncryptString(cfg.APIKey)
		if encErr != nil {
			return apierr.Wrap(encErr, apierr.CodeDBWrite)
		}
		keyEnc = enc
	}

	language := cfg.Language
	if language == "" {
		language = "auto"
	}

	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Updates(map[string]any{
			"online_endpoint":    cfg.Endpoint,
			"online_model":       cfg.Model,
			"online_language":    language,
			"online_api_key_enc": keyEnc,
		}).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	s.emit("onlineEndpoint")
	return nil
}

// GetOnlineConfig 返回在线端点配置（密钥掩码，API-5.6）。
func (s *Service) GetOnlineConfig(ctx context.Context) (contract.OnlineConfig, error) {
	var row storage.AppSettings
	if err := s.db().WithContext(ctx).First(&row, "id = ?", 1).Error; err != nil {
		return contract.OnlineConfig{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	timeout := int64(8000)
	return contract.OnlineConfig{
		Endpoint:  row.OnlineEndpoint,
		Model:     row.OnlineModel,
		Language:  row.OnlineLanguage,
		APIKey:    maskSecret(mustDecrypt(row.OnlineAPIKeyEnc)),
		TimeoutMs: timeout,
	}, nil
}

// SetConsentOnline 记录在线隐私风险确认（API-9.5 / FR-08-2）。
//
// 只接受 https 域名；记录时间戳与域名，端点域名变化后自动失效。
func (s *Service) SetConsentOnline(ctx context.Context, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return apierr.Newf(apierr.CodeInvalidArgument, "endpoint must be a valid URL")
	}
	if u.Scheme != "https" {
		return apierr.Newf(apierr.CodeInvalidArgument, "endpoint must use https")
	}
	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Updates(map[string]any{
			"online_consent_at":   nowMs(),
			"online_consent_host": u.Host,
		}).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	s.emit("onlineConsentAt")
	logx.For("appsettings").Info("online consent recorded", "host", u.Host)
	return nil
}

// SetFallbackPolicy 保存降级策略（API-5.7）。
func (s *Service) SetFallbackPolicy(ctx context.Context, p contract.FallbackPolicy) error {
	if p.MaxRetries < 0 || p.MaxRetries > 10 {
		return apierr.Newf(apierr.CodeInvalidArgument, "maxRetries out of range")
	}
	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Updates(map[string]any{
			"fallback_to_local": p.FallbackToLocal,
			"auto_switch_back":  p.AutoSwitchBack,
			"max_retries":       p.MaxRetries,
		}).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	s.emit("fallbackToLocal")
	return nil
}

// —— 代理（FR-05-5）——

// SetProxy 保存代理配置（API-9.3）。
//
// 契约（DOC-03 §2.9.1）：
//   - Type=none 时其余字段必须为空，后端忽略；
//   - 密码仅用于 Proxy-Authorization，加密落库，不写日志。
func (s *Service) SetProxy(ctx context.Context, p contract.ProxyConfig) error {
	typ := p.Type
	if typ == "" {
		typ = contract.ProxyNone
	}
	switch typ {
	case contract.ProxyNone, contract.ProxyHTTP, contract.ProxyHTTPS, contract.ProxySOCKS5:
	default:
		return apierr.Newf(apierr.CodeInvalidArgument, "invalid proxy type: %s", typ)
	}

	updates := map[string]any{"proxy_type": typ}

	if typ == contract.ProxyNone {
		// none：清空其余字段，避免残留旧主机/凭据。
		updates["proxy_host"] = ""
		updates["proxy_port"] = 0
		updates["proxy_username"] = ""
		updates["proxy_password_enc"] = ""
		updates["proxy_no_proxy"] = ""
	} else {
		if p.Host == "" {
			return apierr.Newf(apierr.CodeInvalidArgument, "proxy host is required")
		}
		if p.Port <= 0 || p.Port > 65535 {
			return apierr.Newf(apierr.CodeInvalidArgument, "proxy port out of range")
		}
		updates["proxy_host"] = p.Host
		updates["proxy_port"] = p.Port
		updates["proxy_username"] = p.Username
		updates["proxy_no_proxy"] = p.NoProxy
		updates["proxy_for_download"] = p.UseForDownload
		updates["proxy_for_online"] = p.UseForOnline

		// 密码：传空或掩码时保留原值。
		if p.Password != "" && !isMasked(p.Password) {
			enc, err := cryptox.EncryptString(p.Password)
			if err != nil {
				return apierr.Wrap(err, apierr.CodeDBWrite)
			}
			updates["proxy_password_enc"] = enc
		}
	}

	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Updates(updates).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	s.emit("proxy")
	return nil
}

// TestProxy 测试代理连通（API-9.4）。
//
// 契约（DOC-03 §2.9）：
//   - 传的是**待测试**的配置，本方法**不落库**（保存走 SetProxy，职责分开）；
//   - 只测「能否经该代理拿到目标响应」，返回 OK / 延迟 / 错误分类 / 脱敏说明；
//   - 网络失败以 `TestResult{OK:false, Category:...}` 表达（error 为 nil），
//     只有配置本身非法（未知 scheme、缺主机）才返回 error —— 那才是必须先改的。
//
// 密码掩码处理：前端回传的密码可能是掩码（界面上不回显真实密码）。带掩码时
// 用库里已存的明文去测，否则会拿字面量「••••」去认证，测出来必然 407。
func (s *Service) TestProxy(ctx context.Context, p contract.ProxyConfig) (contract.TestResult, error) {
	if s.proxyTest == nil {
		return contract.TestResult{Category: "unknown", Note: "模型域未接入，无法测试代理连通性"},
			apierr.New(apierr.CodeInvalidArgument).WithDetail("模型域未接入，无法测试代理连通性")
	}

	cfg := p
	if cfg.Type == "" {
		cfg.Type = contract.ProxyNone
	}
	if isMasked(cfg.Password) || cfg.Password == "" {
		// 掩码 / 空：沿用库里已加密保存的那份（解密失败由调用方得到明确错误）。
		var row storage.AppSettings
		if err := s.db().WithContext(ctx).Where("id = ?", 1).First(&row).Error; err == nil {
			if row.ProxyPasswordEnc != "" {
				if plain, derr := cryptox.DecryptString(row.ProxyPasswordEnc); derr == nil {
					cfg.Password = plain
				}
			}
		}
	}
	return s.proxyTest(ctx, cfg)
}

// —— 音频与录制（FR-03）——

// SetAudio 保存音频与录制设置（API-9.6）。
func (s *Service) SetAudio(ctx context.Context, a contract.AudioSettings) error {
	format := a.SaveFormat
	if format == "" {
		format = "wav"
	}
	if format != "wav" && format != "flac" {
		return apierr.Newf(apierr.CodeInvalidArgument, "save format must be wav or flac")
	}
	sensitivity := a.Sensitivity
	if sensitivity == "" {
		sensitivity = "standard"
	}
	if sensitivity != "standard" && sensitivity != "noisy" {
		return apierr.Newf(apierr.CodeInvalidArgument, "sensitivity must be standard or noisy")
	}
	// 0 = 自动；否则必须落在 2–8（FR-06-6）。
	if a.MaxSpeakers != 0 && (a.MaxSpeakers < 2 || a.MaxSpeakers > 8) {
		return apierr.Newf(apierr.CodeInvalidArgument, "maxSpeakers must be 0 (auto) or 2-8")
	}

	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Updates(map[string]any{
			"save_format":       format,
			"sensitivity":       sensitivity,
			"max_speakers":      a.MaxSpeakers,
			"default_source_id": a.DefaultSourceID,
			"auto_name":         a.AutoName,
		}).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	s.emit("audio")
	return nil
}

// —— 隐私 ——

// SetPrivacy 保存隐私设置（API-9.7）。
//
// noAudioPersist 只在在线模式下有意义（PRD §6.2 缓解措施），此处不强制。
func (s *Service) SetPrivacy(ctx context.Context, p contract.PrivacySettings) error {
	level := p.LogLevel
	if level == "" {
		level = "info"
	}
	if level != "info" && level != "debug" && level != "off" {
		return apierr.Newf(apierr.CodeInvalidArgument, "log level must be info, debug or off")
	}
	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Updates(map[string]any{
			"no_audio_persist": p.NoAudioPersist,
			"log_level":        level,
		}).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	// log_level 也是调试模式的用户层：写库后同步运行时，否则要等重启才和库一致。
	if s.debugMode != nil {
		if _, err := s.debugMode(level == "debug"); err != nil {
			logx.For("appsettings").Warn("调试模式未能按隐私设置生效", "err", err.Error())
		}
		s.emit("debugMode")
	}
	s.emit("privacy")
	return nil
}

// SetDebugMode 切换调试模式（API-9.16）。
//
// 持久化在 app_settings.log_level（开 = debug，关 = info），立即生效、无需重启。
// 开启后所有级别的日志额外写入 <dataRoot>/logs/debug.log；关闭即停写，已有文件保留可读。
// 被环境变量 MANDARYN_LOG_LEVEL 接管时仍保存用户层的值（不静默丢弃），
// 返回的生效状态以环境变量为准（前端据 source=env 禁用开关并说明）。
//
// 可能错误：ERR_DB_WRITE（落库失败）、ERR_PATH_UNWRITABLE（调试日志文件打不开，开关回落为关）。
func (s *Service) SetDebugMode(ctx context.Context, enabled bool) (contract.DebugModeState, error) {
	level := "info"
	if enabled {
		level = "debug"
	}
	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Update("log_level", level).Error; err != nil {
		return contract.DebugModeState{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	st := contract.DebugModeState{Enabled: enabled, UserEnabled: enabled, Source: contract.DebugModeSourceUser}
	if s.debugMode != nil {
		var err error
		st, err = s.debugMode(enabled)
		if err != nil {
			s.emit("debugMode")
			return st, apierr.From(err, apierr.CodePathUnwritable).WithDetail("调试日志文件无法打开")
		}
	}
	s.emit("debugMode")
	// 只写开关元数据；开启时这条也是调试日志里的第一条记录。
	logx.For("appsettings").Info("调试模式开关已更新",
		"enabled", st.Enabled, "userEnabled", enabled, "source", st.Source)
	return st, nil
}

// GetOutboundLog 返回最近出站记录（API-9.8），供「最近出站点」面板自证。
func (s *Service) GetOutboundLog(_ context.Context) ([]contract.OutboundEntry, error) {
	if s.out == nil {
		return nil, nil
	}
	entries, err := s.out.List(200)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return entries, nil
}

// OfflineMode 报告全局离线开关状态（FR-08 离线开关，前端据此禁用相关入口）。
func (s *Service) OfflineMode(_ context.Context) (bool, error) {
	return outbound.Offline(), nil
}

// —— 性能预算（NFR-08）——

// SetResourceCap 设置整机性能预算 N（API-9.9）。
//
// 校验：10–100 且为 5 的倍数；越界返回 ERR_PERF_CAP_RANGE（而非通用参数错误），
// 便于 UI 直接提示「10–100，步进 5」。
//
// 语义提醒：capPct 是**整机预算**，不是「本应用上限」（NFR-08 注 2）。
func (s *Service) SetResourceCap(ctx context.Context, capPct int) (contract.PerfUsage, error) {
	if capPct < 10 || capPct > 100 || capPct%5 != 0 {
		return contract.PerfUsage{}, apierr.New(apierr.CodePerfCapRange).
			WithDetail("capPct=%d", capPct)
	}
	if err := s.db().WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Update("resource_cap_pct", capPct).Error; err != nil {
		return contract.PerfUsage{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	s.emit("resourceCapPct")

	// 返回当前读数：已接入预算域时给真实三分量（S8 / EV-15 同一口径），
	// 否则退回配置值——**绝不**在本域另算一套 CPU 口径。
	if s.usage != nil {
		if u := s.usage(); u.SampledAt != "" {
			u.CapPct = capPct
			return u, nil
		}
	}
	return contract.PerfUsage{
		CapPct:      capPct,
		HeadroomPct: capPct,
		Level:       contract.LevelL0,
		WindowMs:    5000,
		SampledAt:   nowRFC3339(),
	}, nil
}

// —— 内部工具 ——

// update 写单个字段，成功后广播 AppSettingsChanged（payload=字段名）。
func (s *Service) update(ctx context.Context, column, field string, value string) error {
	if err := s.db().WithContext(ctx).
		Model(&storage.AppSettings{}).
		Where("id = ?", 1).
		Update(column, value).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	s.emit(field)
	return nil
}

func (s *Service) emit(field string) {
	if app := application.Get(); app != nil {
		app.Event.Emit(events.AppSettingsChanged, field)
	}
}

// maskSecret 把密钥变成 `sk-***1234` 形态；空值返回空。
func maskSecret(plain string) string {
	if plain == "" {
		return ""
	}
	const tail = 4
	if len(plain) <= tail {
		return "***"
	}
	prefix := ""
	if idx := strings.IndexAny(plain, "-_"); idx > 0 && idx < 6 {
		prefix = plain[:idx+1]
	}
	return prefix + "***" + plain[len(plain)-tail:]
}

// isMasked 判断前端是否把掩码原样回传（此时应保留原密钥）。
// proxyEnvVar 返回当前**接管**代理配置的环境变量名；未接管时返回空串。
//
// 判定与 models.globalProxyFromEnv 保持同一口径：变量存在且去空白后非空才算接管。
// 两处若不一致，界面就会出现「提示说被接管、实际走的却是设置页」的自相矛盾。
func proxyEnvVar() string {
	if strings.TrimSpace(os.Getenv(models.EnvHTTPProxy)) == "" {
		return ""
	}
	return models.EnvHTTPProxy
}

// isMasked 判断一个字符串是否为掩码（含 `***`）。
func isMasked(v string) bool {
	return strings.Contains(v, "***")
}

// mustDecrypt 解密失败时返回空串：坏密文不应让整个设置页打不开。
func mustDecrypt(enc string) string {
	if enc == "" {
		return ""
	}
	plain, err := cryptox.DecryptString(enc)
	if err != nil {
		logx.For("appsettings").Warn("decrypt failed, treating as unset")
		return ""
	}
	return plain
}

// nowMs 当前毫秒时间戳。
func nowMs() int64 { return time.Now().UnixMilli() }

// nowRFC3339 当前时间（RFC3339Nano），用于 PerfUsage.SampledAt 等展示字段。
func nowRFC3339() string { return time.Now().Format(time.RFC3339Nano) }
