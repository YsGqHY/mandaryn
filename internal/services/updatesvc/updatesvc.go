// Package updatesvc 是「检查更新」的 L3 薄壳（API-11.1）。
//
// 职责边界（铁律）：
//  1. 出站必须走 httpx，并**先**过 outbound.Guard()（离线闸门，FR-08）；
//  2. 每次出站都落 outbound_log（用户有权知道应用何时联过网）；
//  3. 只回答"有没有新版本"，**不下载、不安装**（不做静默自更新，见 DEPLOYMENT §6.5）。
//
// 版本比较与清单解析在 internal/updates（纯函数，可单测）。
package updatesvc

import (
	"context"
	"net/url"
	"os"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/updates"
	"vip.oberon.mandaryn/internal/utils/httpx"
)

// defaultLookupEnv 是生产环境的 env 读取（测试可替换）。
var defaultLookupEnv = os.LookupEnv

// DefaultManifestURL 是默认的版本清单地址（GitHub Releases latest API）。
//
// **注意**：匿名访问只对公开仓库有效。私有仓库会返回 404（"Not Found"），
// 此时本服务会把错误如实报给界面，而不是假装"已是最新"。
// 私有仓库的部署应改用 EnvManifestURL 指向一个公开托管的清单。
const DefaultManifestURL = "https://api.github.com/repos/YsGqHY/mandaryn/releases/latest"

// EnvManifestURL 覆盖清单地址（企业内网 / 私有仓库改用公开托管清单时用）。
const EnvManifestURL = "MANDARYN_UPDATE_URL"

// Deps 是装配依赖。
type Deps struct {
	// Version 当前版本（internal/app 注入，取自 build/config.yml）。
	Version string
	// Recorder 出站留痕（可为 nil，此时不记录但仍受离线闸门约束）。
	Recorder *outbound.Recorder
	// ManifestURL 清单地址；空时用 DefaultManifestURL。
	ManifestURL string
	// LookupEnv 默认 os.LookupEnv，测试可替换。
	LookupEnv func(string) (string, bool)
}

// Service 是 Wails 服务。
type Service struct {
	version  string
	recorder *outbound.Recorder
	url      string
}

// New 构造服务。
func New(deps Deps) (*Service, error) {
	url := strings.TrimSpace(deps.ManifestURL)
	if url == "" {
		// 环境变量优先（企业内网 / 私有仓库改用公开托管清单）。
		lookup := deps.LookupEnv
		if lookup == nil {
			lookup = defaultLookupEnv
		}
		if v, ok := lookup(EnvManifestURL); ok && strings.TrimSpace(v) != "" {
			url = strings.TrimSpace(v)
		}
	}
	if url == "" {
		url = DefaultManifestURL
	}
	return &Service{
		version:  deps.Version,
		recorder: deps.Recorder,
		url:      url,
	}, nil
}

// Version 返回当前运行版本（API-11.2）。
//
// 单独一个方法而不是让界面从 Check 的返回值里读：显示版本号**不该**触发任何网络请求，
// 也不该受离线模式影响（离线时用户仍应能知道自己装的是哪个版本）。
func (s *Service) Version(_ context.Context) (string, error) {
	return s.version, nil
}

// Check 查询最新版本（API-11.1）。
//
// 可能错误：
//   - ERR_OFFLINE_ENFORCED：离线模式开启，**不发请求**（闸门在请求之前）；
//   - ERR_DOWNLOAD_NETWORK：网络不可达 / 4xx / 5xx（含私有仓库的 404）。
func (s *Service) Check(ctx context.Context) (contract.UpdateInfo, error) {
	// 1) 离线闸门：必须在发请求之前判断，不能"先发出去再报错"。
	if err := outbound.Guard(); err != nil {
		return contract.UpdateInfo{Current: s.version}, apierr.From(err, apierr.CodeOfflineEnforced)
	}

	// 2) 出站留痕（先登记，结果后补）——只有 host，不存完整 URL。
	host := hostOf(s.url)

	var rel updates.GitHubRelease
	if err := httpx.GetJSON(ctx, s.url, &rel); err != nil {
		result := "error"
		if _, isHTTP := err.(*httpx.HTTPError); isHTTP {
			result = "error"
		}
		s.record(host, result, 0)
		// 私有仓库的 404 也走这里：如实报"取不到清单"，不要伪装成"已是最新"。
		return contract.UpdateInfo{Current: s.version},
			apierr.From(err, apierr.CodeDownloadNetwork).WithDetail("无法获取版本清单：%s", host)
	}

	info := rel.ToInfo(s.version, host)
	s.record(host, "ok", 0)
	return info, nil
}

// record 落出站留痕；失败不影响业务（只留日志痕迹，不打扰用户）。
func (s *Service) record(host, result string, bytes int64) {
	if s.recorder == nil {
		return
	}
	// audio=false：版本检查不涉及任何音频数据。
	s.recorder.Record(host, contract.OutboundUpdateCheck, false, bytes, result)
}

// ManifestURL 返回当前生效的清单地址（诊断用；不含凭据，因为这里本来也没有）。
func (s *Service) ManifestURL() string { return s.url }

// hostOf 只取 host，避免把 query / 凭据写进 outbound_log（NFR-03）。
// 与 internal/models 的同名函数语义一致；不跨包引用是为了不让本服务依赖模型仓库。
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
