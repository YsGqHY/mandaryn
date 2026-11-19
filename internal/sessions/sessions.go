// Package sessions 是「会话库」的 L2 领域包（FR-04 库 / 检索 / 导出数据准备）。
//
// 分层理由（CLAUDE.md「领域逻辑不进 service」）：
//   - 分页 SQL、关键词检索（FTS5 / LIKE 双路径）、筛选、改名、批量删除与
//     导出内容生成全部在这里；
//   - `internal/services/sessionsvc` 只做参数校验、错误码映射与 DTO 转换，
//     它不拼一行 SQL；
//   - 本包**不 import Wails**，因此可以被单测直接驱动（临时 SQLite 即可）。
//
// 与持久化的关系：
//   - 只依赖 *storage.Holder（切路径会换 DB 句柄，禁止 capture *storage.DB）；
//   - 会话音频按 DOC-04 §6.1 布局落在 `<dataRoot>/sessions/<sessionID>/`，
//     库里只存**相对 dataRoot** 的路径，因此本包需要注入 dataRoot 取值函数。
package sessions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 分页缺省与上限。
//
// 上限刻意压低到 200：会话列表行带标题与来源名，一屏放不下 200 行，
// 超过这个数一定是调用方误用（应当用搜索而不是「翻到很后面」）。
const (
	defaultLimit = 20
	maxLimit     = 200
	// maxTitleRunes 与 storage.Session.Title 的 size:255 对齐（按字符而非字节，
	// 避免中文标题被 SQLite 的 TEXT 长度语义坑到）。
	maxTitleRunes = 255
)

// Options 是领域包的依赖注入点。
type Options struct {
	// Holder 必填；每次查询都取 Current()，路径切换后自动指向新库。
	Holder *storage.Holder
	// DataRoot 返回当前生效的 dataRoot（绝对路径），用于把库里的相对
	// AudioPath 还原成绝对路径。必填：缺失时删除/导出无法定位音频。
	DataRoot func() (string, error)
	// Writer 是导出落盘端口，由 L3 注入（复用 exportsvc 的原子写与 excelize）。
	// 为 nil 时导出返回 ERR_INVALID_ARGUMENT，便于在纯查询场景省略它。
	Writer ExportWriter
	// SearchMode 仅用于测试与诊断：强制走 fts / like 路径。
	// 生产留空（SearchAuto）即可，auto 会按关键词与 FTS 可用性自行选择。
	SearchMode SearchMode
}

// Repo 是会话库的领域实现。
type Repo struct {
	holder   *storage.Holder
	dataRoot func() (string, error)
	writer   ExportWriter
	mode     SearchMode

	// ftsOnce 让「索引覆盖度校验」在本实例内只做一次（见 search.go ftsEnsure）。
	ftsOnce sync.Once
}

// New 构造会话库领域对象。
func New(opts Options) (*Repo, error) {
	if opts.Holder == nil {
		return nil, errors.New("sessions: holder is required")
	}
	if opts.DataRoot == nil {
		return nil, errors.New("sessions: dataRoot provider is required")
	}
	mode := opts.SearchMode
	if mode == "" {
		mode = SearchAuto
	}
	if !mode.valid() {
		return nil, fmt.Errorf("sessions: unknown search mode %q", mode)
	}
	return &Repo{
		holder:   opts.Holder,
		dataRoot: opts.DataRoot,
		writer:   opts.Writer,
		mode:     mode,
	}, nil
}

// Query 是 List 的结构化条件（L2 形态；L3 从 contract.ListQuery 转换而来）。
//
// 为什么不在 L2 直接用 contract.ListQuery：契约里没有时长区间与「来源 ID
// 模糊匹配」的语义开关，而领域层需要把它们表达清楚；转换留在 L3，
// 这样契约演进时只改一处。
type Query struct {
	// Keyword 标题 + 片段正文的关键词（子串语义，见 search.go）。
	Keyword string
	// SourceIDLike 来源 ID 模糊匹配（应用筛选）。
	SourceIDLike string
	// Status 录制状态，空 = 全部（completed / interrupted / failed / degraded）。
	Status string
	// ModelSource local|online|""。
	ModelSource string
	// TranscriptionStatus none|queued|running|done|failed|""。
	TranscriptionStatus string
	// FromMs / ToMs 录制开始时间范围（毫秒，含端点；0 = 不限）。
	FromMs int64
	ToMs   int64
	// MinDurationMs / MaxDurationMs 时长区间（毫秒，含端点；0 = 不限）。
	MinDurationMs int64
	MaxDurationMs int64
	// Offset / Limit 分页；Limit<=0 用缺省 20，上限 200。
	Offset int
	Limit  int
}

// normalized 修正非法分页参数。越界 offset 不算错误：返回空列表是正确语义。
func (q Query) normalized() Query {
	if q.Offset < 0 {
		q.Offset = 0
	}
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	if q.Limit > maxLimit {
		q.Limit = maxLimit
	}
	return q
}

// Get 返回单条会话详情（API-3.2）。
//
// AudioExists 一并返回：前端要据此决定「回放按钮是否可用 / 是否提示音频已丢失」，
// 让前端自己去 stat 文件既跨平台麻烦又多一次 RPC。
func (r *Repo) Get(ctx context.Context, sessionID string) (contract.SessionDetail, error) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return contract.SessionDetail{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sessionId 不能为空")
	}
	var row storage.Session
	err := r.db().WithContext(ctx).Where("id = ?", id).Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return contract.SessionDetail{}, apierr.Newf(apierr.CodeNotFound, "session not found").
				WithDetail("sessionId=%s", id)
		}
		return contract.SessionDetail{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	out := contract.SessionDetail{Session: toContract(row)}
	out.AudioExists = r.audioExists(row.AudioPath)
	return out, nil
}

// Rename 改会话标题（API-3.3）。
func (r *Repo) Rename(ctx context.Context, sessionID, title string) error {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return apierr.New(apierr.CodeInvalidArgument).WithDetail("sessionId 不能为空")
	}
	// 标题必须落成「非空、无首尾空白、≤255 字符」：空标题会让列表出现无标识行。
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return apierr.New(apierr.CodeInvalidArgument).WithDetail("title 不能为空")
	}
	if utf8.RuneCountInString(trimmed) > maxTitleRunes {
		return apierr.Newf(apierr.CodeInvalidArgument, "title is too long").
			WithDetail("title 长度 %d 超过上限 %d", utf8.RuneCountInString(trimmed), maxTitleRunes)
	}
	res := r.db().WithContext(ctx).
		Model(&storage.Session{}).
		Where("id = ?", id).
		Update("title", trimmed)
	if res.Error != nil {
		return apierr.Wrap(res.Error, apierr.CodeDBWrite)
	}
	if res.RowsAffected == 0 {
		return apierr.Newf(apierr.CodeNotFound, "session not found").WithDetail("sessionId=%s", id)
	}
	logx.For("sessions").Info("会话已改名", "sessionId", id, "titleRunes", utf8.RuneCountInString(trimmed))
	return nil
}

// DeleteOutcome 是批量删除的逐项结果。
type DeleteOutcome struct {
	// Sessions 实际删除的会话行数。
	Sessions int
	// AudioRemoved 成功删除音频文件的会话数。
	AudioRemoved int
	// Missing 请求里不存在（或已被并发删除）的会话 ID。
	Missing []string
	// AudioFailures 数据库行已删但音频未能删除的会话 ID（孤儿文件，见下）。
	AudioFailures []string
}

// Delete 批量删除会话（API-3.4）。
//
// 顺序取舍（重要）：**先删库、后删文件**。
//
//   - 若先删文件再删库，中途失败会留下「库里有行、文件已没了」的脏引用，
//     列表里仍能看到记录，点回放才炸 —— 用户无法自愈；
//   - 反过来先删库，最坏结果是磁盘上留下孤儿音频目录（占用空间但无脏引用），
//     可以被后续「孤儿清理」任务安全回收，且用户视角已经「删掉了」。
//
// 因此音频删除失败**不**回滚、也不返回错误（库已一致），而是记进
// DeleteOutcome.AudioFailures 并打 warn 日志，由调用方决定是否提示用户。
func (r *Repo) Delete(ctx context.Context, sessionIDs []string, deleteAudio bool) (DeleteOutcome, error) {
	ids := normalizeIDs(sessionIDs)
	if len(ids) == 0 {
		return DeleteOutcome{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sessionIds 不能为空")
	}

	db := r.db().WithContext(ctx)
	var rows []storage.Session
	if err := db.Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return DeleteOutcome{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}

	found := make(map[string]storage.Session, len(rows))
	for _, row := range rows {
		found[row.ID] = row
	}
	out := DeleteOutcome{Sessions: len(rows)}
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			out.Missing = append(out.Missing, id)
		}
	}
	if len(rows) == 0 {
		return out, apierr.Newf(apierr.CodeNotFound, "no session matched").
			WithDetail("requested=%d", len(ids))
	}

	// 第一步：删除数据库行。segments / session_events / session_speakers
	// 由 SQLite 的 ON DELETE CASCADE 连带删除（segments_fts 靠触发器同步）。
	if err := db.Where("id IN ?", keysOf(found)).Delete(&storage.Session{}).Error; err != nil {
		return DeleteOutcome{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}

	if !deleteAudio {
		logx.For("sessions").Info("批量删除会话（保留音频）", "sessions", out.Sessions, "missing", len(out.Missing))
		return out, nil
	}

	// 第二步：删除音频。整目录删（<dataRoot>/sessions/<id>/），因为目录里还有
	// meta.json 快照；库里只记 audio.wav 的相对路径。
	for _, row := range found {
		if err := r.removeSessionAudio(row); err != nil {
			out.AudioFailures = append(out.AudioFailures, row.ID)
			// 只记错误码与路径，不记音频内容（NFR-03-5）。
			logx.For("sessions").Warn("会话音频删除失败（留下孤儿文件）",
				"sessionId", row.ID, "audioPath", row.AudioPath, "err", err)
			continue
		}
		out.AudioRemoved++
	}
	logx.For("sessions").Info("批量删除会话",
		"sessions", out.Sessions, "audioRemoved", out.AudioRemoved,
		"audioFailed", len(out.AudioFailures), "missing", len(out.Missing))
	return out, nil
}

// removeSessionAudio 删除某个会话的音频目录。
//
// 目录不存在视为成功（用户可能已手工清理过）：删除是幂等动作，
// 「已经不在了」不是失败。
//
// 安全护栏（重要）：只有当音频位于 `<dataRoot>/sessions/` 之内时才整目录删除。
// AudioPath 允许是绝对路径（兼容迁移前的旧数据），若它指向 dataRoot 之外，
// 整目录 RemoveAll 可能连带删掉用户自己的文件夹 —— 这种情况只删那个音频文件本身。
func (r *Repo) removeSessionAudio(row storage.Session) error {
	dir, err := r.sessionDir(row)
	if err != nil {
		return err
	}
	if dir == "" {
		return nil
	}
	if !r.withinSessionsRoot(dir) {
		// 越界：只删文件，绝不递归删目录。
		if abs := r.audioAbs(row.AudioPath); abs != "" {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// withinSessionsRoot 判断 dir 是否落在 `<dataRoot>/sessions` 之下。
func (r *Repo) withinSessionsRoot(dir string) bool {
	root, err := r.dataRoot()
	if err != nil {
		return false
	}
	sessionsRoot := filepath.Join(root, "sessions")
	rel, err := filepath.Rel(sessionsRoot, dir)
	if err != nil {
		return false
	}
	// rel 为 ".." 或以 ".."+分隔符开头表示越界；"." 表示正好是 sessions 根
	// （也不该被整目录删除）。
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// sessionDir 返回会话音频目录的绝对路径。
//
// 优先用库里的 AudioPath（相对 dataRoot）：它能指向用户迁移前写入的位置；
// AudioPath 为空（未持久化音频 / NoAudioPersist）时退回 dataRoot/sessions/<id>。
func (r *Repo) sessionDir(row storage.Session) (string, error) {
	root, err := r.dataRoot()
	if err != nil {
		return "", fmt.Errorf("sessions: resolve dataRoot: %w", err)
	}
	if strings.TrimSpace(row.AudioPath) == "" {
		return filepath.Join(root, "sessions", row.ID), nil
	}
	abs := r.audioAbs(row.AudioPath)
	if abs == "" {
		return "", nil
	}
	return filepath.Dir(abs), nil
}

// audioAbs 把库里的相对路径还原成绝对路径；绝对路径原样返回（兼容旧数据）。
func (r *Repo) audioAbs(rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}
	root, err := r.dataRoot()
	if err != nil {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// audioExists 报告音频文件是否真实存在。
func (r *Repo) audioExists(rel string) bool {
	abs := r.audioAbs(rel)
	if abs == "" {
		return false
	}
	st, err := os.Stat(abs)
	return err == nil && !st.IsDir()
}

// db 取当前活跃连接（禁止缓存 *storage.DB：切路径会换句柄）。
func (r *Repo) db() *gorm.DB { return r.holder.Current().GORM }

// toContract 把持久化模型转成契约 DTO。
//
// 时间戳在 DTO 里是 RFC3339 字符串（DOC-03 §1.2 的映射约定），小时/分钟
// 由前端按 locale 排版；后端不猜时区，用本地时区带偏移量输出。
func toContract(row storage.Session) contract.Session {
	return contract.Session{
		ID:                   row.ID,
		Title:                row.Title,
		SourceID:             row.SourceID,
		SourceName:           row.SourceName,
		ModelSource:          row.ModelSource,
		EngineID:             row.EngineID,
		Status:               row.Status,
		Degraded:             row.Degraded,
		StartedAt:            formatMs(row.StartedAt),
		EndedAt:              formatMs(row.EndedAt),
		DurationMs:           row.DurationMs,
		SegmentCount:         row.SegmentCount,
		SpeakerCount:         row.SpeakerCount,
		AudioPath:            row.AudioPath,
		HasAudio:             strings.TrimSpace(row.AudioPath) != "",
		SizeBytes:            row.SizeBytes,
		TranscriptionStatus:  row.TranscriptionStatus,
		TranscribeProgressMs: row.TranscribeProgressMs,
		CharCount:            row.CharCount,
	}
}

// formatMs 把毫秒时间戳格式化为 RFC3339；0 表示「未发生」，输出空串
// （前端据此显示「—」，而不是 1970 年）。
func formatMs(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}

// ParseTimeToMs 把 RFC3339 / 「2006-01-02」解析成毫秒时间戳，供 L3 复用。
//
// 允许纯日期是刻意的：日期选择器给出的是「这一天」，用户预期是整天，
// 因此 From 取当天 00:00:00，To 取当天 23:59:59.999（由 endOfDay 控制）。
func ParseTimeToMs(value string, endOfDay bool) (int64, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, nil
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UnixMilli(), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Millisecond)
		}
		return t.UnixMilli(), nil
	}
	return 0, fmt.Errorf("sessions: cannot parse time %q", v)
}

// normalizeIDs 去空白、去重、保序。
func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// keysOf 取 map 的键（顺序不重要，仅用于 IN 查询）。
func keysOf(m map[string]storage.Session) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
