// Package modelssvc 是模型仓库与迁移的 Wails 薄壳（API-6.1 ~ API-6.13 / FR-05 / FR-07）。
//
// 职责边界（与其它 L3 一致）：只做参数校验、错误码映射与事件转发；
// 清单 / 分片下载 / 校验 / 迁移状态机全部在 internal/models（L2），本层不得重写。
package modelssvc

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/events"
	"vip.oberon.mandaryn/internal/models"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
)

// 环境变量：允许企业内网 / 自建镜像替换清单来源。
const (
	// EnvCatalogURL 覆盖清单基址（目录或 .json 直链）。
	EnvCatalogURL = "MANDARYN_MODEL_CATALOG"
	// EnvCatalogMirrors 覆盖镜像基址列表（逗号分隔，最多 3 个生效）。
	EnvCatalogMirrors = "MANDARYN_MODEL_MIRRORS"
)

// Service 暴露模型仓库与迁移能力。
type Service struct {
	mgr      *models.Manager
	resolver *paths.Resolver
}

// New 构造服务并把 L2 的事件回调桥接到 Wails 事件总线。
//
// holder / resolver 必填；recorder 可为 nil（仅影响「最近出站点」面板）。
func New(holder *storage.Holder, resolver *paths.Resolver, recorder *outbound.Recorder) *Service {
	s := &Service{resolver: resolver}

	base, mirrors := catalogURL(), catalogMirrors()
	managedBy := ""
	if base != "" {
		managedBy = EnvCatalogURL
	} else if strings.TrimSpace(os.Getenv(EnvCatalogMirrors)) != "" {
		managedBy = EnvCatalogMirrors
	}
	mgr := models.New(models.Options{
		Holder:            holder,
		Recorder:          recorder,
		Root:              s.currentRoot,
		ManifestBaseURL:   base,
		UseStarterCatalog: managedBy == "",
		MirrorManagedBy:   managedBy,
		Mirrors:           mirrors,
		Proxy:             s.proxyConfig,
		OnDownload: func(st contract.DownloadState) {
			emit(events.ModelsDownload, st)
		},
		OnMigrate: func(st contract.MigrateState) {
			emit(events.ModelsMigrate, st)
		},
		OnChanged: func(payload contract.ModelChanged) {
			emit(events.ModelsChanged, payload)
		},
		OnModelRootChanged: func(string) error {
			// 迁移已把 model_root 写进 app_settings（single source）；
			// 这里只让解析器丢弃缓存，下次 Resolve 重新读库。
			s.resolver.Invalidate()
			return nil
		},
	})
	s.mgr = mgr
	return s
}

// Manager 暴露 L2 领域对象，供 S3 的本地引擎注入 models.BundleLocator。
func (s *Service) Manager() *models.Manager { return s.mgr }

// Cleanup 是启动钩子：清理残留 staging 目录、把卡在 downloading 的断点改为 paused。
func (s *Service) Cleanup(ctx context.Context) {
	s.mgr.Cleanup(ctx)
}

// currentRoot 返回当前生效的模型根（解析失败时退回平台默认值）。
func (s *Service) currentRoot() (string, error) {
	p, err := s.resolver.Resolve(paths.KeyModelRoot)
	if err != nil {
		return "", err
	}
	if p.Effective == "" {
		return paths.DefaultModelRoot()
	}
	return p.Effective, nil
}

// proxyConfig 读当前代理配置（下载用途）。读库失败时按「无代理」处理：
// 让用户至少还能下载，而不是因为一次读失败彻底不可用。
func (s *Service) proxyConfig() (contract.ProxyConfig, error) {
	if s.mgr == nil {
		// 构造期回调早于 mgr 赋值：此时按无代理处理。
		return contract.ProxyConfig{Type: contract.ProxyNone}, nil
	}
	db := s.mgr.DB()
	if db == nil {
		return contract.ProxyConfig{Type: contract.ProxyNone}, nil
	}
	var row storage.AppSettings
	if err := db.First(&row, 1).Error; err != nil {
		return contract.ProxyConfig{Type: contract.ProxyNone}, nil
	}
	return models.ProxyConfigFromSettings(&row, models.PurposeDownload)
}

// —— API-6.1 ~ API-6.8 ——

func (s *Service) GetModelMirror(ctx context.Context) (contract.ModelMirror, error) {
	return s.mgr.ModelMirror(ctx)
}

func (s *Service) SetModelMirror(ctx context.Context, value string) (contract.ModelMirror, error) {
	return s.mgr.SetModelMirror(ctx, value)
}

// ListAvailable 清单（远程缓存 + 内置兜底）。（API-6.1）
func (s *Service) ListAvailable(ctx context.Context, refresh bool) contract.BundleCatalog {
	if refresh {
		return catalogResult(s.mgr.RefreshAvailable(ctx))
	}
	return catalogResult(s.mgr.ListAvailable(ctx))
}

func catalogResult(list []contract.BundleInfo, err error) contract.BundleCatalog {
	result := contract.BundleCatalog{Bundles: list}
	if err != nil {
		e := mapErr(err, apierr.CodeDownloadNetwork)
		result.Warning = &contract.ErrorDTO{
			Code: e.Code, Message: e.Message, Action: e.Action, Detail: e.Detail, Retryable: e.Retryable,
		}
	}
	return result
}

// ListInstalled 已装包列表（含校验状态与占用）。（API-6.2）
func (s *Service) ListInstalled(ctx context.Context) ([]contract.InstalledBundle, error) {
	list, err := s.mgr.ListInstalled(ctx)
	if err != nil {
		return nil, mapErr(err, apierr.CodeModelNotFound)
	}
	return list, nil
}

// Download 开始 / 继续下载。（API-6.3）
func (s *Service) Download(ctx context.Context, bundleID string) (contract.DownloadState, error) {
	id, err := requireBundleID(bundleID)
	if err != nil {
		return contract.DownloadState{}, err
	}
	st, err := s.mgr.Download(ctx, id)
	if err != nil {
		return st, mapErr(err, apierr.CodeDownloadNetwork)
	}
	return st, nil
}

// PauseDownload 暂停下载（保留分片）。（API-6.4）
func (s *Service) PauseDownload(_ context.Context, bundleID string) error {
	id, err := requireBundleID(bundleID)
	if err != nil {
		return err
	}
	if err := s.mgr.PauseDownload(id); err != nil {
		return mapErr(err, apierr.CodeNotFound)
	}
	return nil
}

// CancelDownload 取消下载。（API-6.5）
func (s *Service) CancelDownload(_ context.Context, bundleID string, removeParts bool) error {
	id, err := requireBundleID(bundleID)
	if err != nil {
		return err
	}
	if err := s.mgr.CancelDownload(id, removeParts); err != nil {
		return mapErr(err, apierr.CodeNotFound)
	}
	return nil
}

// Verify 重新哈希校验。（API-6.6）
func (s *Service) Verify(ctx context.Context, bundleID string) (contract.VerifyResult, error) {
	id, err := requireBundleID(bundleID)
	if err != nil {
		return contract.VerifyResult{}, err
	}
	res, verr := s.mgr.Verify(ctx, id)
	if verr != nil {
		return contract.VerifyResult{}, mapErr(verr, apierr.CodeModelNotFound)
	}
	return res, nil
}

// Remove 删除已装包。（API-6.7）
func (s *Service) Remove(ctx context.Context, bundleID string) error {
	id, err := requireBundleID(bundleID)
	if err != nil {
		return err
	}
	if err := s.mgr.Remove(ctx, id); err != nil {
		return mapErr(err, apierr.CodeModelNotFound)
	}
	return nil
}

// GetStoreStats 目录 + 占用。（API-6.8）
func (s *Service) GetStoreStats(ctx context.Context) (contract.StoreStats, error) {
	stats, err := s.mgr.Stats(ctx)
	if err != nil {
		return contract.StoreStats{}, mapErr(err, apierr.CodePathUnwritable)
	}
	return stats, nil
}

// —— API-6.9 ~ API-6.12（FR-07 目录迁移）——

// SetModelRoot 预检并给出迁移计划（**不执行**）。（API-6.9）
//
// 注意与 pathsvc.SetModelRoot（API-9.12）的分工：那个只改用户层值与暂存值，
// 不做数据搬迁；这里负责「搬迁 + 切换」，两者由 UX 串起来。
func (s *Service) SetModelRoot(ctx context.Context, target string) (contract.MigratePlan, error) {
	plan, err := s.mgr.PlanMigrate(ctx, target)
	if err != nil {
		return plan, mapErr(err, apierr.CodeMigratePrecheck)
	}
	return plan, nil
}

// MigrateModelRoot 执行迁移。（API-6.10）
func (s *Service) MigrateModelRoot(ctx context.Context, req contract.MigrateRequest) (contract.MigrateState, error) {
	st, err := s.mgr.RunMigration(ctx, req)
	if err != nil {
		return st, mapErr(err, apierr.CodeMigratePrecheck)
	}
	return st, nil
}

// CancelMigration 取消并回滚。（API-6.11）
func (s *Service) CancelMigration(ctx context.Context) error {
	if err := s.mgr.CancelMigration(ctx); err != nil {
		return mapErr(err, apierr.CodeStateForbidden)
	}
	return nil
}

// GetMigrationState 轮询兜底（事件丢失时）。（API-6.12）
func (s *Service) GetMigrationState(ctx context.Context) (contract.MigrateState, error) {
	return s.mgr.MigrationState(ctx), nil
}

// ImportOffline 导入离线包（UC-08 内网部署）。（API-6.13）
func (s *Service) ImportOffline(ctx context.Context, archivePath string) (contract.InstalledBundle, error) {
	p := strings.TrimSpace(archivePath)
	if p == "" {
		return contract.InstalledBundle{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("archive path is required")
	}
	if _, err := os.Stat(p); err != nil {
		return contract.InstalledBundle{}, apierr.Newf(apierr.CodeNotFound,
			"archive not found: %s", p)
	}
	b, err := s.mgr.ImportOffline(ctx, p)
	if err != nil {
		return b, mapErr(err, apierr.CodeModelCorrupt)
	}
	return b, nil
}

// String 便于日志输出（避免把结构体打进日志）。
func (s *Service) String() string { return fmt.Sprintf("modelssvc") }

// —— 内部工具 ——

// requireBundleID 做统一的空值校验。
func requireBundleID(v string) (string, error) {
	id := strings.TrimSpace(v)
	if id == "" {
		return "", apierr.New(apierr.CodeInvalidArgument).WithDetail("bundleId is required")
	}
	return id, nil
}

// mapErr 把任意 error 归一成 *apierr.Err（已是 *apierr.Err 的原样透传）。
func mapErr(err error, fallbackCode string) *apierr.Err {
	return apierr.From(err, fallbackCode)
}

// emit 向 Wails 事件总线发事件；应用未就绪（启动早期 / 单测）时静默跳过。
func emit(name string, payload any) {
	app := application.Get()
	if app == nil {
		return
	}
	app.Event.Emit(name, payload)
}

// catalogURL 返回清单基址（环境变量优先）。
func catalogURL() string {
	if v := strings.TrimSpace(os.Getenv(EnvCatalogURL)); v != "" {
		return v
	}
	return ""
}

// catalogMirrors 返回镜像基址列表（仅显式配置）。
func catalogMirrors() []string {
	if v := strings.TrimSpace(os.Getenv(EnvCatalogMirrors)); v != "" {
		out := make([]string, 0, 3)
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}
