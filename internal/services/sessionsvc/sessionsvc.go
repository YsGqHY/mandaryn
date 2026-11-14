// Package sessionsvc 是「会话库」的 Wails 薄壳（API-3.1 ~ API-3.5 / FR-04）。
//
// 职责边界（CLAUDE.md「领域逻辑不进 service」）：
//   - 本层只做 DTO 转换、参数校验收口、错误码映射与日志；
//   - 分页 SQL、FTS/LIKE 检索、批量删除顺序、导出内容格式全部在
//     internal/sessions（L2），本层不拼一行 SQL、不写一个时间码；
//   - 导出落盘复用既有的 exportsvc（excelize 写 xlsx、UTF-8 BOM 写 CSV），
//     不另造一套写出逻辑 —— 见 adapter 说明。
package sessionsvc

import (
	"context"
	"strings"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/services/exportsvc"
	"vip.oberon.mandaryn/internal/sessions"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/filex"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// Service 暴露会话库能力。
type Service struct {
	repo *sessions.Repo
}

// New 构造服务。
//
// holder 必填（禁止持有 *storage.DB：切数据路径会换句柄）；
// resolver 不是必填，但它决定了「音频是否存在 / 删除音频」能否定位文件 ——
// 传 nil 时这些能力降级（Get 的 audioExists 恒为 false）。
func New(holder *storage.Holder, resolver *paths.Resolver) (*Service, error) {
	repo, err := sessions.New(sessions.Options{
		Holder:   holder,
		DataRoot: dataRootProvider(resolver),
		Writer:   &exportAdapter{svc: exportsvc.New()},
	})
	if err != nil {
		return nil, err
	}
	return &Service{repo: repo}, nil
}

// NewWithRepo 供测试与需要自定义检索引擎的装配方使用。
func NewWithRepo(repo *sessions.Repo) *Service { return &Service{repo: repo} }

// dataRootProvider 把 resolver 包成「懒取值」函数。
//
// 必须懒取：启动早期的解析结果可能因用户在设置页改目录而变化，
// 每次导出 / 删除时重新 Resolve 才能拿到当前生效值（ADR-02）。
func dataRootProvider(resolver *paths.Resolver) func() (string, error) {
	if resolver == nil {
		return func() (string, error) { return "", nil }
	}
	return func() (string, error) { return resolver.Effective(paths.KeyDataRoot) }
}

// List 分页 + 关键词检索 + 筛选（API-3.1）。
//
// 可能错误：ERR_INVALID_ARGUMENT（from/to 无法解析、from 晚于 to）。
func (s *Service) List(ctx context.Context, q contract.ListQuery) (contract.ListResult, error) {
	query, err := sessions.QueryFromContract(q)
	if err != nil {
		return contract.ListResult{}, err
	}
	out, err := s.repo.List(ctx, query)
	if err != nil {
		return contract.ListResult{}, err
	}
	if out.Items == nil {
		// 空切片而非 nil：前端不必为 null 单独判空（与 capturesvc 一致）。
		out.Items = []contract.Session{}
	}
	return out, nil
}

// Get 返回会话详情（API-3.2）；可能错误：ERR_NOT_FOUND。
func (s *Service) Get(ctx context.Context, sessionID string) (contract.Session, error) {
	detail, err := s.repo.Get(ctx, sessionID)
	if err != nil {
		return contract.Session{}, err
	}
	return detail.Session, nil
}

// GetDetail 返回会话详情 + 音频是否存在（API-3.2 扩展，见 contract/session_ext.go）。
//
// 单列一个方法的理由：API-3.2 的契约形态一旦加字段就要改前端 bindings，
// 而详情页需要 audioExists 才能正确禁用回放按钮；等 API.md 同步后
// 两者可以合并，届时删掉这个方法即可（Get 保持不变，向后兼容）。
func (s *Service) GetDetail(ctx context.Context, sessionID string) (contract.SessionDetail, error) {
	return s.repo.Get(ctx, sessionID)
}

// Rename 改标题（API-3.3）；可能错误：ERR_NOT_FOUND、ERR_INVALID_ARGUMENT。
func (s *Service) Rename(ctx context.Context, sessionID, title string) error {
	return s.repo.Rename(ctx, sessionID, title)
}

// Delete 批量删除（API-3.4）：默认连音频一起删。
//
// 可能错误：ERR_NOT_FOUND（请求的 ID 全都不存在）、ERR_INVALID_ARGUMENT、ERR_DB_WRITE。
//
// 音频删除失败**不**返回错误：数据库已一致（记录确实删了），失败只意味磁盘上
// 留下孤儿文件；把它变成 error 会让前端显示「删除失败」，而用户重试也不会
// 更成功。需要逐项结果时用 DeleteDetailed。
func (s *Service) Delete(ctx context.Context, sessionIDs []string, deleteAudio bool) error {
	out, err := s.repo.Delete(ctx, sessionIDs, deleteAudio)
	if err != nil {
		return err
	}
	if len(out.AudioFailures) > 0 {
		logx.For("sessions").Warn("部分会话音频未能删除",
			"sessions", out.Sessions, "audioFailed", len(out.AudioFailures))
	}
	return nil
}

// DeleteDetailed 批量删除并返回逐项结果（API-3.4 扩展）。
//
// 前端需要据此提示「记录已删，但 N 个音频文件仍在磁盘上」，因此保留失败清单。
func (s *Service) DeleteDetailed(ctx context.Context, sessionIDs []string, deleteAudio bool) (contract.DeleteResult, error) {
	out, err := s.repo.Delete(ctx, sessionIDs, deleteAudio)
	if err != nil {
		return contract.DeleteResult{}, err
	}
	return contract.DeleteResult{
		Deleted:      out.Sessions,
		AudioDeleted: out.AudioRemoved,
		Missing:      emptyIfNil(out.Missing),
		AudioFailed:  emptyIfNil(out.AudioFailures),
	}, nil
}

// Export 导出会话文本（API-3.5），返回实际落盘路径。
//
// 可能错误：ERR_INVALID_ARGUMENT（格式 / 路径非法、targetPath 为空）、
// ERR_NOT_FOUND（会话不存在）、ERR_DISK_FULL（写失败）。
//
// targetPath 为空时的原生「另存为」由前端 NativeDialogs 负责 ——
// 后端在导出这条纯数据路径上不弹窗（也拿不到窗口上下文）。
func (s *Service) Export(ctx context.Context, req contract.ExportRequest) (string, error) {
	path, err := s.repo.Export(ctx, sessions.ExportRequest{
		SessionIDs:        req.SessionIDs,
		Format:            req.Format,
		IncludeTimestamps: req.IncludeTimestamps,
		IncludeSpeakers:   req.IncludeSpeakers,
		TargetPath:        req.TargetPath,
	})
	if err != nil {
		return "", err
	}
	// 只记格式与会话数，不记正文、不记完整路径里的用户名之外的敏感信息（NFR-03-5）。
	logx.For("sessions").Info("会话已导出",
		"format", strings.ToLower(req.Format), "sessions", len(req.SessionIDs), "path", path)
	return path, nil
}

// exportAdapter 把 exportsvc 适配成 L2 的 ExportWriter 端口。
//
// 为什么用适配器而不是让 L2 直接 import exportsvc：L2 领域包不该依赖任何
// service 包（否则 L2 与 L3 的依赖方向就反了）。文本类（txt/srt）没有既有实现，
// 这里用 filex.WriteAtomic 落盘：崩溃不会留下半截文件覆盖用户的同名文件。
type exportAdapter struct {
	svc *exportsvc.Service
}

func (a *exportAdapter) WriteCSV(path, content string) error {
	return a.svc.WriteCSV(path, content)
}

func (a *exportAdapter) WriteXLSX(path string, table sessions.Table) error {
	return a.svc.WriteXLSX(path, exportsvc.Table{
		SheetName: table.SheetName,
		Headers:   table.Headers,
		Rows:      table.Rows,
	})
}

func (a *exportAdapter) WriteText(path, content string) error {
	return filex.WriteAtomic(path, []byte(content), 0o644)
}

// emptyIfNil 保证 JSON 里是 [] 而不是 null（前端不必判空）。
func emptyIfNil(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	return in
}
