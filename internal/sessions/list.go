package sessions

import (
	"context"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// QueryFromContract 把契约查询转成领域查询。
//
// 放在 L2 而不是 L3：时间字符串（RFC3339 / 纯日期）→ 毫秒的解析规则属于
// 领域语义，前端不该关心「纯日期算整天」这种约定。
func QueryFromContract(q contract.ListQuery) (Query, error) {
	fromMs, err := ParseTimeToMs(q.From, false)
	if err != nil {
		return Query{}, apierr.Newf(apierr.CodeInvalidArgument, "invalid from").
			WithCause(err).WithDetail("from=%q", q.From)
	}
	// 纯日期时 To 取当天最后一毫秒，保证「筛到 11-10」包含 11-10 全天。
	toMs, err := ParseTimeToMs(q.To, true)
	if err != nil {
		return Query{}, apierr.Newf(apierr.CodeInvalidArgument, "invalid to").
			WithCause(err).WithDetail("to=%q", q.To)
	}
	if fromMs > 0 && toMs > 0 && fromMs > toMs {
		return Query{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("from 晚于 to")
	}
	return Query{
		Keyword:             q.Keyword,
		SourceIDLike:        q.SourceIDLike,
		Status:              q.Status,
		ModelSource:         q.ModelSource,
		TranscriptionStatus: q.TranscriptionStatus,
		FromMs:              fromMs,
		ToMs:                toMs,
		Offset:              q.Offset,
		Limit:               q.Limit,
	}, nil
}

// List 分页 + 关键词检索 + 筛选（API-3.1）。
//
// 排序固定为 `started_at DESC, id DESC`：加 id 是为了在同一毫秒内多次录制时
// 仍然稳定（否则翻页会漂移、同一行重复出现）。
func (r *Repo) List(ctx context.Context, q Query) (contract.ListResult, error) {
	q = q.normalized()

	db := r.db().WithContext(ctx).Model(&storage.Session{})

	// —— 筛选（全部走索引列，见 DOC-04 TBL-03 索引表）——
	if v := strings.TrimSpace(q.Status); v != "" {
		db = db.Where("status = ?", v)
	}
	if v := strings.TrimSpace(q.ModelSource); v != "" {
		db = db.Where("model_source = ?", v)
	}
	if v := strings.TrimSpace(q.TranscriptionStatus); v != "" {
		db = db.Where("transcription_status = ?", v)
	}
	if v := strings.TrimSpace(q.SourceIDLike); v != "" {
		db = db.Where("source_id LIKE ? ESCAPE '\\'", "%"+escapeLike(v)+"%")
	}
	if q.FromMs > 0 {
		db = db.Where("started_at >= ?", q.FromMs)
	}
	if q.ToMs > 0 {
		db = db.Where("started_at <= ?", q.ToMs)
	}
	if q.MinDurationMs > 0 {
		db = db.Where("duration_ms >= ?", q.MinDurationMs)
	}
	if q.MaxDurationMs > 0 {
		db = db.Where("duration_ms <= ?", q.MaxDurationMs)
	}

	// —— 关键词（标题 + 片段正文）——
	plan := r.planSearch(q.Keyword)
	if plan.usable() {
		if plan.mode == SearchFTS {
			// 首次走 FTS 前校正索引覆盖度，避免历史片段静默搜不到（S5-01 相关）。
			r.ftsEnsure(ctx)
		}
		logx.For("sessions").Debug("会话检索", "mode", plan.mode, "reason", plan.reason,
			"keywordRunes", utf8.RuneCountInString(plan.literal))
		db = applySearch(db, plan)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return contract.ListResult{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}

	items := []contract.Session{}
	if total == 0 {
		return contract.ListResult{Total: 0, Items: items}, nil
	}

	var rows []storage.Session
	if err := db.Order("started_at DESC, id DESC").
		Offset(q.Offset).Limit(q.Limit).
		Find(&rows).Error; err != nil {
		return contract.ListResult{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	items = make([]contract.Session, 0, len(rows))
	for _, row := range rows {
		items = append(items, toContract(row))
	}
	return contract.ListResult{Total: int(total), Items: items}, nil
}

// applySearch 把检索条件拼到查询上。
//
// 标题与正文是**或**关系：用户搜「KOOK」时既可能命中标题（默认命名含来源名），
// 也可能只出现在对话正文里。
func applySearch(db *gorm.DB, plan searchPlan) *gorm.DB {
	kw := "%" + escapeLike(plan.literal) + "%"
	args := append([]any{kw}, plan.args()...)
	return db.Where("(title LIKE ? ESCAPE '\\' OR id IN ("+plan.subquery()+"))", args...)
}

// escapeLike 转义 LIKE 通配符，避免用户输入的 % 和 _ 被当成通配符
// （否则搜「100%」会匹配任意以 100 开头的标题）。
func escapeLike(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
