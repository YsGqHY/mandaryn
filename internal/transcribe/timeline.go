package transcribe

import (
	"context"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// 时间轴查询（API §2.4 的读取方法：API-4.1 / 4.2 / 4.3 / 4.4）。
//
// 消费方是前端「歌词式回放」：它靠**二分定位**把播放位置映射到句子，
// 因此后端必须给出 `seq` 升序且 `startMs` 单调不减的序列（API §2.4「对齐契约」，
// ARCH §4.5.2）。这里在返回前再断言一次，避免脏数据在 UI 上表现为「点哪跳哪」。
//
// 分页约定（本包自定义，文档未规定具体值）：
//   - 一次性取整段的上限 MaxTimelineSegments = 20000 行；
//   - 超出时**不报错**，返回前 20000 行并保持 Total 真实，前端改用 GetTimelinePage；
//   - GetTimelinePage 的 limit 默认 DefaultPageLimit=500，上限 MaxPageLimit=2000；
//   - 时间范围 `fromMs`/`toMs` 为闭区间，0 表示该端不限（小时级会话可直接按区间拉取）。
const (
	// MaxTimelineSegments 一次性时间轴的片段上限（API-4.1「≤2 万条」）。
	MaxTimelineSegments = 20000
	// DefaultPageLimit 分页默认条数。
	DefaultPageLimit = 500
	// MaxPageLimit 分页上限（防止前端一次拉爆内存）。
	MaxPageLimit = 2000
	// SearchDefaultLimit 片段检索默认条数。
	SearchDefaultLimit = 200
	// MaxSearchLimit 片段检索上限。
	MaxSearchLimit = 1000
	// MarkerKindDegrade 降级打点（时间轴上要能被回放看到）。
	MarkerKindDegrade = contract.MarkerDegrade
)

// TimelineReader 提供时间轴 / 分页 / 检索 / 人工修正。
//
// 与 Runner 分离：读取路径不应因为「有没有任务在跑」而不同，
// 也不该把调度器的锁带进只读查询里。
type TimelineReader struct {
	store Store
}

// NewTimelineReader 构造读取器。
func NewTimelineReader(store Store) (*TimelineReader, error) {
	if store == nil {
		return nil, apierr.New(apierr.CodeDBWrite).WithDetail("time-axis 未拿到 Store")
	}
	return &TimelineReader{store: store}, nil
}

// Timeline 返回整段会话的时间轴（片段 + 标记）。
//
// 可能错误：ERR_NOT_FOUND（会话不存在）。
func (t *TimelineReader) Timeline(ctx context.Context, sessionID string) (contract.Timeline, error) {
	session, err := t.loadSession(ctx, sessionID)
	if err != nil {
		return contract.Timeline{}, err
	}
	page, err := t.store.ListSegments(ctx, SegmentFilter{
		SessionID: sessionID,
		Limit:     MaxTimelineSegments,
	})
	if err != nil {
		return contract.Timeline{}, apierr.From(err, apierr.CodeDBWrite)
	}
	segs, err := toSegmentDTOs(page.Rows)
	if err != nil {
		return contract.Timeline{}, err
	}
	markers, err := t.Markers(ctx, sessionID)
	if err != nil {
		return contract.Timeline{}, err
	}
	return contract.Timeline{
		SessionID:  sessionID,
		DurationMs: session.DurationMs,
		Segments:   segs,
		Markers:    markers,
	}, nil
}

// Page 返回时间轴的一页（API-4.2）。
//
// offset/limit 越界会被钳到合法区间（不报错）：前端滚动到底再请求一页时
// 并发改库可能让 offset 越过总数，报错只会让列表闪烁。
func (t *TimelineReader) Page(ctx context.Context, sessionID string, offset, limit int) (contract.TimelinePage, error) {
	return t.RangePage(ctx, sessionID, 0, 0, offset, limit)
}

// RangePage 按时间区间 + 分页取片段（API-4.2 的扩展，供按需加载）。
//
// fromMs/toMs 为闭区间，0 表示该端不限。
func (t *TimelineReader) RangePage(ctx context.Context, sessionID string, fromMs, toMs int64,
	offset, limit int) (contract.TimelinePage, error) {

	session, err := t.loadSession(ctx, sessionID)
	if err != nil {
		return contract.TimelinePage{}, err
	}
	if fromMs < 0 || toMs < 0 {
		return contract.TimelinePage{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("时间范围不能为负")
	}
	if toMs > 0 && fromMs > 0 && toMs < fromMs {
		return contract.TimelinePage{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("时间范围颠倒：fromMs=%d toMs=%d", fromMs, toMs)
	}
	limit = clampLimit(limit, DefaultPageLimit, MaxPageLimit)
	if offset < 0 {
		offset = 0
	}

	page, err := t.store.ListSegments(ctx, SegmentFilter{
		SessionID: sessionID,
		FromMs:    fromMs,
		ToMs:      toMs,
		Offset:    offset,
		Limit:     limit,
	})
	if err != nil {
		return contract.TimelinePage{}, apierr.From(err, apierr.CodeDBWrite)
	}
	segs, err := toSegmentDTOs(page.Rows)
	if err != nil {
		return contract.TimelinePage{}, err
	}
	out := contract.TimelinePage{
		SessionID:  sessionID,
		Total:      page.Total,
		Offset:     offset,
		Limit:      limit,
		Segments:   segs,
		DurationMs: session.DurationMs,
	}
	// 标记只在首屏下发：数量少但不需要每页重复（翻页时机密集，重复下发
	// 会让标记数组在每页覆盖一次，前端反而要自己判断「这页的标记是否完整」）。
	if offset == 0 {
		markers, merr := t.Markers(ctx, sessionID)
		if merr != nil {
			return contract.TimelinePage{}, merr
		}
		out.Markers = markers
	}
	return out, nil
}

// Search 片段级检索（API-4.3）：返回带跳转时间（startMs）的片段。
//
// Keyword 为空时等价「按会话/说话人列举」，便于做「只看某人说的话」。
func (t *TimelineReader) Search(ctx context.Context, q contract.SegmentQuery) ([]contract.Segment, error) {
	if q.SessionID != "" {
		if _, err := t.loadSession(ctx, q.SessionID); err != nil {
			return nil, err
		}
	} else if strings.TrimSpace(q.Keyword) == "" {
		// 全局检索必须有条件，否则等于把整库片段拉给前端。
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("全局片段检索必须给 keyword 或 sessionId")
	}
	limit := clampLimit(q.Limit, SearchDefaultLimit, MaxSearchLimit)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	page, err := t.store.ListSegments(ctx, SegmentFilter{
		SessionID: q.SessionID,
		Keyword:   strings.TrimSpace(q.Keyword),
		SpeakerID: q.SpeakerID,
		Offset:    offset,
		Limit:     limit,
	})
	if err != nil {
		return nil, apierr.From(err, apierr.CodeDBWrite)
	}
	return toSegmentDTOs(page.Rows)
}

// Markers 返回会话事件标记（降级 / 源丢失 / 暂停恢复等）。
func (t *TimelineReader) Markers(ctx context.Context, sessionID string) ([]contract.Marker, error) {
	rows, err := t.store.ListMarkers(ctx, sessionID)
	if err != nil {
		return nil, apierr.From(err, apierr.CodeDBWrite)
	}
	out := make([]contract.Marker, 0, len(rows))
	for _, r := range rows {
		out = append(out, contract.Marker{AtMs: r.AtMs, Kind: r.Kind, Note: r.Note})
	}
	return out, nil
}

// UpdateSegmentText 人工修正片段文本（API-4.4），置 edited=true。
//
// 可能错误：ERR_NOT_FOUND（片段不存在）、ERR_INVALID_ARGUMENT（空文本）。
func (t *TimelineReader) UpdateSegmentText(ctx context.Context, segmentID int64, text string) error {
	if segmentID <= 0 {
		return apierr.New(apierr.CodeInvalidArgument).WithDetail("segmentId 非法")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return apierr.New(apierr.CodeInvalidArgument).WithDetail("修正后的文本为空")
	}
	if err := t.store.UpdateSegmentText(ctx, segmentID, text); err != nil {
		return apierr.From(err, apierr.CodeNotFound)
	}
	return nil
}

// loadSession 读取会话（统一把不存在映射为 ERR_NOT_FOUND）。
func (t *TimelineReader) loadSession(ctx context.Context, sessionID string) (SessionRow, error) {
	if strings.TrimSpace(sessionID) == "" {
		return SessionRow{}, apierr.New(apierr.CodeInvalidArgument).WithDetail("sessionId 为空")
	}
	session, err := t.store.LoadSession(ctx, sessionID)
	if err != nil {
		return SessionRow{}, apierr.From(err, apierr.CodeNotFound)
	}
	return session, nil
}

// toSegmentDTOs 把存储行转成对外的片段 DTO，并断言时间轴不变量。
func toSegmentDTOs(rows []SegmentRow) ([]contract.Segment, error) {
	out := make([]contract.Segment, 0, len(rows))
	var prev *SegmentRow
	for i := range rows {
		row := rows[i]
		if err := assertSegmentOrder(prev, row); err != nil {
			// 脏数据不下发给前端（否则二分定位会跳到错误句子）。
			return nil, apierr.From(err, apierr.CodeDBWrite)
		}
		prev = &row
		out = append(out, contract.Segment{
			ID:         row.ID,
			SessionID:  row.SessionID,
			Seq:        row.Seq,
			StartMs:    row.StartMs,
			EndMs:      row.EndMs,
			Text:       row.Text,
			SpeakerID:  row.SpeakerID,
			EngineID:   row.EngineID,
			Finalized:  row.IsFinal,
			Confidence: row.Confidence,
			// RoleID / RoleName 由 roles JOIN 得到（FR-06-3），本层不填。
		})
	}
	return out, nil
}

// clampLimit 把 limit 钳到 [1, max]（0 取默认值）。
func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}
