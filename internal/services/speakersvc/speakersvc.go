// Package speakersvc 是「说话人与角色」的 Wails 薄壳（API-7.1 ~ API-7.7 / FR-06）。
//
// 职责边界（CLAUDE.md「领域逻辑不进 service」）：
//  1. 参数校验与错误码映射；
//  2. 把 L2 需要的端口接起来（数据库句柄、音频路径解析、声纹提取器、装配层回调）；
//  3. 事件转发素材：L2 产出的 `contract.RoleChanged` 交给装配层注入的
//     `OnRolesChanged` 去 Emit —— **本层不直接发 Wails 事件**。
//
// **不做**任何领域决策：聚类、归并、幂等重跑、角色映射的语义全在 `internal/speaker`；
// 本层不拼一行聚类代码、不算一次相似度。
package speakersvc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"gorm.io/gorm"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/speaker"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// Deps 是装配层注入的依赖。
type Deps struct {
	// Holder 数据库句柄持有器（切数据目录后 Current() 会变，**不要**缓存 *DB）。
	Holder *storage.Holder
	// Resolver 路径裁决器（ADR-02）：把 `sessions.audio_path` 拼成绝对路径。
	// 为 nil 时声纹分离不可用（角色管理仍然可用）。
	Resolver *paths.Resolver

	// Extractor 声纹提取器。**允许为 nil**：本机未装声纹模型包时正常降级，
	// `AnalyzeSession` 返回 ERR_LOCAL_UNAVAILABLE，其余方法照常工作。
	Extractor speaker.Extractor
	// Locator + Factory 是可选的「按需构造提取器」路径：两者都非 nil 且未直接给
	// Extractor 时，构造期会用 `speaker.LocateAndBuild` 去装模型（role="speaker"）。
	//
	// 为什么要这一对而不是只给 Extractor：模型包可能在运行期才下载完成，
	// 而服务是启动期构造的；定位器每次调用都重新查目录，天然支持「装完就能用」。
	// 构造失败（模型没装）**不**让服务构造失败 —— 角色管理必须照常可用。
	Locator speaker.BundleLocator
	Factory speaker.ExtractorFactory
	// Audio 片段音频读取端口；nil 且 Resolver 非 nil 时自动装配 WAV 读取器。
	Audio speaker.AudioLoader
	// MaxSpeakers 会话内说话人数上限（默认读 app_settings.max_speakers）。
	MaxSpeakers func(ctx context.Context) (int, error)

	// OnRolesChanged 角色变更回调（EV-07 `roles:changed` 载荷）。装配层接成 Emit。
	OnRolesChanged func(contract.RoleChanged)
}

// Service 暴露说话人与角色能力。
type Service struct {
	repo   *speaker.Repo
	holder *storage.Holder
	// extractorErr 记录「尝试装配提取器但失败」的原因（模型未安装 / 本构建不含该能力）。
	//
	// 保留它是为了给用户**明确**的错误码（ERR_MODEL_NOT_FOUND 比笼统的
	// 「不可用」有用得多），而不是把差异吞掉。
	extractorErr *apierr.Err
}

// New 构造服务。
//
// 只强制要求 Holder：没有它连角色列表都读不出来。声纹提取器缺失是**正常状态**
// （本机未装模型），此时分离能力降级、纯角色操作不受影响。
func New(d Deps) (*Service, error) {
	if d.Holder == nil {
		return nil, apierr.New(apierr.CodeDBWrite).WithDetail("speakersvc 未拿到 storage.Holder")
	}

	extractor := d.Extractor
	var extractorErr *apierr.Err
	if extractor == nil && d.Locator != nil && d.Factory != nil {
		// 装得上就用；装不上只记原因，不让服务构造失败（角色管理不依赖推理）。
		built, err := speaker.LocateAndBuild(d.Locator, d.Factory)
		if err != nil {
			var e *apierr.Err
			if errors.As(err, &e) {
				extractorErr = e
			} else {
				extractorErr = apierr.From(err, apierr.CodeLocalUnavailable)
			}
			logx.For("speaker").Info("声纹提取器不可用，分离能力降级",
				"code", extractorErr.Code)
		} else {
			extractor = built
		}
	}

	audio := d.Audio
	if audio == nil && d.Resolver != nil {
		loader, err := speaker.NewWAVLoader(wavOpener(d.Holder, d.Resolver))
		if err != nil {
			return nil, err
		}
		audio = loader
	}

	repo, err := speaker.New(speaker.Options{
		Holder:         d.Holder,
		Extractor:      extractor,
		Audio:          audio,
		MaxSpeakers:    d.MaxSpeakers,
		OnRolesChanged: d.OnRolesChanged,
	})
	if err != nil {
		return nil, err
	}
	return &Service{repo: repo, holder: d.Holder, extractorErr: extractorErr}, nil
}

// NewWithRepo 供测试与需要自定义实现的装配方使用。
func NewWithRepo(repo *speaker.Repo, holder *storage.Holder) *Service {
	return &Service{repo: repo, holder: holder}
}

// String 让 Wails 的服务名稳定（与其它 service 一致）。
func (s *Service) String() string { return "speakersvc" }

// —— API-7.1 ——

// ListRoles 返回全局角色列表（含颜色、覆盖的声纹身份、命中会话数）。
func (s *Service) ListRoles(ctx context.Context) ([]contract.Role, error) {
	roles, err := s.repo.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	if roles == nil {
		// 空切片而非 nil：前端不必为 null 单独判空（与 sessionsvc 一致）。
		roles = []contract.Role{}
	}
	return roles, nil
}

// —— API-7.2 ——

// RenameRole 角色改名，**全局立即生效**（单行 UPDATE，历史会话靠 JOIN 同步）。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND。
func (s *Service) RenameRole(ctx context.Context, roleID, name string) error {
	return s.repo.RenameRole(ctx, roleID, name)
}

// —— API-7.3 ——

// ListSessionSpeakers 返回会话内说话人 → 角色映射；未识别片段作为合成行附在末尾
// （`roleId` = `rl_unknown`，见 contract.RoleIsUnidentified）。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND。
func (s *Service) ListSessionSpeakers(ctx context.Context, sessionID string) ([]contract.SessionSpeaker, error) {
	list, err := s.repo.ListSessionSpeakers(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []contract.SessionSpeaker{}
	}
	return list, nil
}

// —— API-7.4 ——

// MergeSpeakers 合并两个说话人（FR-06-5）：片段改指向、质心融合、冗余计数重算。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND、ERR_DB_WRITE。
func (s *Service) MergeSpeakers(ctx context.Context, fromSpeakerID, toSpeakerID string) error {
	return s.repo.MergeSpeakers(ctx, fromSpeakerID, toSpeakerID)
}

// —— API-7.5 ——

// SplitSpeaker 从误并中拆出指定片段，生成新说话人与新角色。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND。
func (s *Service) SplitSpeaker(ctx context.Context, sessionID, speakerID string, segmentIDs []int64) (contract.Role, error) {
	return s.repo.SplitSpeaker(ctx, sessionID, speakerID, segmentIDs)
}

// —— API-7.6 ——

// SetMaxSpeakers 设置会话内说话人数上限：0 = 自动，2–8 = 硬上限。
//
// 为什么允许 0：默认（0）让聚类按阈值自动定人数；上限只在用户明确要求
// 「这场最多 N 个人」时收窄（FR-06-6）。
//
// 可能错误：ERR_INVALID_ARGUMENT（越界）。
func (s *Service) SetMaxSpeakers(ctx context.Context, n int) error {
	if n != 0 && (n < 2 || n > 8) {
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("maxSpeakers 只能是 0（自动）或 2–8，实际 %d", n)
	}
	// 只改这一列：app_settings 是单例行（id=1），其它字段由 appsettings 域负责。
	res := s.holder.Current().GORM.WithContext(ctx).
		Model(&storage.AppSettings{}).
		Where("id = ?", 1).
		Update("max_speakers", n)
	if res.Error != nil {
		return apierr.Wrap(res.Error, apierr.CodeDBWrite)
	}
	if res.RowsAffected == 0 {
		return apierr.New(apierr.CodeDBWrite).WithDetail("app_settings 单例行不存在，无法写入上限")
	}
	logx.For("speaker").Info("会话内说话人上限已更新", "maxSpeakers", n)
	return nil
}

// —— API-7.7 ——

// SetRoleColor 设置角色配色令牌（只接受主题令牌，不接受十六进制）。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND。
func (s *Service) SetRoleColor(ctx context.Context, roleID, colorToken string) error {
	return s.repo.SetRoleColor(ctx, roleID, colorToken)
}

// —— 分离能力（API §2.7 之外的扩展，与 sessionsvc.GetDetail 同类处置）——

// AnalyzeSession 对某个会话跑一次说话人分离（重跑幂等覆盖）。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND、ERR_LOCAL_UNAVAILABLE
// （未装声纹模型）、ERR_MODEL_NOT_FOUND / ERR_MODEL_CORRUPT（构造期已知的模型问题）、
// ERR_STATE_FORBIDDEN（同一会话已有分离在跑）。
func (s *Service) AnalyzeSession(ctx context.Context, sessionID string) (contract.DiarizeResult, error) {
	// 构造期就知道「模型装不上」时，把具体原因透出来（比笼统的「不可用」有用）。
	if s.extractorErr != nil && !s.repo.HasExtractor() {
		return contract.DiarizeResult{}, s.extractorErr
	}
	return s.repo.Run(ctx, sessionID)
}

// HasDiarization 报告当前是否具备声纹分离能力（前端据此置灰「开始分离」按钮）。
//
// 返回 false 不代表出错：本机未安装声纹模型包时，角色管理仍完全可用。
func (s *Service) HasDiarization() bool { return s.repo.HasExtractor() }

// AssignSpeaker 把会话内某个声纹身份指派到指定角色（合并的轻量替代）。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND。
func (s *Service) AssignSpeaker(ctx context.Context, sessionID, speakerID, roleID string) error {
	return s.repo.AssignSpeaker(ctx, sessionID, speakerID, roleID)
}

// wavOpener 把「会话 ID → 音频文件」接成 L2 的 AudioOpener。
//
// 路径一律经 paths.Resolver（ADR-02），库里只存**相对 dataRoot** 的路径；
// 绝对路径原样使用（兼容迁移前的旧数据）。
func wavOpener(holder *storage.Holder, resolver *paths.Resolver) speaker.AudioOpener {
	return func(ctx context.Context, sessionID string) (speaker.AudioSource, error) {
		var row storage.Session
		if err := holder.Current().GORM.WithContext(ctx).
			Select("id, audio_path").
			First(&row, "id = ?", sessionID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, apierr.New(apierr.CodeNotFound).WithDetail("会话不存在：%s", sessionID)
			}
			return nil, apierr.Wrap(err, apierr.CodeDBWrite)
		}
		rel := strings.TrimSpace(row.AudioPath)
		if rel == "" {
			return nil, apierr.New(apierr.CodeNotFound).WithDetail("会话没有音频路径")
		}
		root, err := resolver.Effective(paths.KeyDataRoot)
		if err != nil {
			return nil, apierr.From(err, apierr.CodePathUnwritable)
		}
		full := rel
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, rel)
		}
		f, err := os.Open(full)
		if err != nil {
			// 只记相对路径（不含用户主目录，避免诊断包泄露路径信息）。
			return nil, apierr.New(apierr.CodeNotFound).
				WithAction("re-record").
				WithDetail("音频文件不可读：%s", rel).
				WithCause(err)
		}
		return f, nil
	}
}
