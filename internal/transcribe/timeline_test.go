package transcribe

import (
	"context"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// seededReader 造一个带片段与标记的会话，返回读取器。
func seededReader(t *testing.T) (*TimelineReader, *fakeStore) {
	t.Helper()
	st := newFakeStore(SessionRow{
		ID: "s_tl", DurationMs: 5000, TranscriptionStatus: StatusDone,
	})
	ctx := context.Background()
	for i, seg := range []struct {
		start, end int64
		text       string
	}{
		{0, 1000, "第一句"},
		{1200, 2400, "第二句"},
		{2600, 4000, "第三句"},
	} {
		if _, err := st.UpsertSegment(ctx, SegmentRow{
			SessionID: "s_tl", Seq: i, StartMs: seg.start, EndMs: seg.end,
			Text: seg.text, EngineID: "fake@1", IsFinal: true,
		}); err != nil {
			t.Fatalf("seed segment: %v", err)
		}
	}
	st.markers = []MarkerRow{
		{AtMs: 0, Kind: contract.MarkerDegrade, Note: "在线不可用，已回退本地"},
		{AtMs: 2000, Kind: contract.MarkerSourceLost, Note: "源暂时静音"},
	}
	reader, err := NewTimelineReader(st)
	if err != nil {
		t.Fatalf("NewTimelineReader: %v", err)
	}
	return reader, st
}

// TestTimelineFull 整段时间轴：片段按 seq 升序 + 标记齐全。
func TestTimelineFull(t *testing.T) {
	reader, _ := seededReader(t)
	tl, err := reader.Timeline(context.Background(), "s_tl")
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.SessionID != "s_tl" || tl.DurationMs != 5000 {
		t.Errorf("会话信息错误：%+v", tl)
	}
	if len(tl.Segments) != 3 {
		t.Fatalf("片段 %d 条, want 3", len(tl.Segments))
	}
	// 关键契约：seq 升序、startMs 单调不减（前端二分定位前提）。
	for i, s := range tl.Segments {
		if s.Seq != i {
			t.Errorf("第 %d 条 seq = %d", i, s.Seq)
		}
		if i > 0 && s.StartMs < tl.Segments[i-1].StartMs {
			t.Errorf("startMs 非单调：%d → %d", tl.Segments[i-1].StartMs, s.StartMs)
		}
		if !s.Finalized {
			t.Errorf("第 %d 条应为定稿", i)
		}
	}
	if len(tl.Markers) != 2 || tl.Markers[0].Kind != contract.MarkerDegrade {
		t.Errorf("标记异常：%+v", tl.Markers)
	}
}

// TestTimelinePage 分页 + 越界钳制。
func TestTimelinePage(t *testing.T) {
	reader, _ := seededReader(t)
	ctx := context.Background()

	p1, err := reader.Page(ctx, "s_tl", 0, 2)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if p1.Total != 3 || len(p1.Segments) != 2 || p1.Segments[0].Seq != 0 {
		t.Fatalf("第一页异常：%+v", p1)
	}
	p2, err := reader.Page(ctx, "s_tl", 2, 2)
	if err != nil {
		t.Fatalf("Page 2: %v", err)
	}
	if len(p2.Segments) != 1 || p2.Segments[0].Seq != 2 {
		t.Fatalf("第二页异常：%+v", p2)
	}
	// offset 越过总数：返回空页而不是报错（前端并发改库不该让列表报错）。
	p3, err := reader.Page(ctx, "s_tl", 99, 2)
	if err != nil {
		t.Fatalf("越界 offset 不应报错：%v", err)
	}
	if len(p3.Segments) != 0 || p3.Total != 3 {
		t.Fatalf("越界页异常：%+v", p3)
	}
	// limit 被钳到上限。
	p4, err := reader.Page(ctx, "s_tl", 0, MaxPageLimit+100)
	if err != nil {
		t.Fatalf("Page limit: %v", err)
	}
	if p4.Limit != MaxPageLimit {
		t.Errorf("limit = %d, want %d", p4.Limit, MaxPageLimit)
	}
}

// TestTimelinePageCarriesDurationAndMarkers 守住 API-4.2 的「一页够用」契约。
//
// 超大通话只能分页取片段，但播放器同时需要总时长与降档/暂停标记。
// 如果这两项只在整段 Timeline 里给，前端就不得不为了标记再发一次整段请求，
// 等于把分页省下的数据全拉回来（真实问题是 E-06 的核心动机）。
func TestTimelinePageCarriesDurationAndMarkers(t *testing.T) {
	reader, _ := seededReader(t)
	ctx := context.Background()

	first, err := reader.Page(ctx, "s_tl", 0, 2)
	if err != nil {
		t.Fatalf("Page 首页: %v", err)
	}
	if first.DurationMs != 5000 {
		t.Errorf("首页 DurationMs = %d, want 5000", first.DurationMs)
	}
	if len(first.Markers) != 2 {
		t.Fatalf("首页应带 %d 条标记，实际 %d 条", 2, len(first.Markers))
	}
	if first.Markers[0].Kind != contract.MarkerDegrade {
		t.Errorf("标记顺序异常：%+v", first.Markers)
	}

	// 非首页：不再重复下发标记，但时长照给（前端翻页时仍要画进度条）。
	second, err := reader.Page(ctx, "s_tl", 2, 2)
	if err != nil {
		t.Fatalf("Page 次页: %v", err)
	}
	if len(second.Markers) != 0 {
		t.Errorf("非首页不应重复下发标记，实际 %d 条", len(second.Markers))
	}
	if second.DurationMs != 5000 {
		t.Errorf("次页 DurationMs = %d, want 5000", second.DurationMs)
	}
}

// TestTimelineRange 时间区间查询用「区间重叠」语义。
func TestTimelineRange(t *testing.T) {
	reader, _ := seededReader(t)
	ctx := context.Background()

	page, err := reader.RangePage(ctx, "s_tl", 1000, 2500, 0, 10)
	if err != nil {
		t.Fatalf("RangePage: %v", err)
	}
	// 第 1 句(0-1000) 与区间相接、第 2 句(1200-2400) 完全在内、第 3 句(2600-4000) 在外。
	if page.Total != 2 {
		t.Fatalf("区间命中 %d 条, want 2（含相接的那句）：%+v", page.Total, page.Segments)
	}
	// 区间正好落在一句中间：必须包含这句（否则拖到句子中间看不到文字）。
	mid, err := reader.RangePage(ctx, "s_tl", 1300, 1400, 0, 10)
	if err != nil {
		t.Fatalf("RangePage mid: %v", err)
	}
	if mid.Total != 1 || mid.Segments[0].Seq != 1 {
		t.Fatalf("句内区间命中异常：%+v", mid)
	}
	if _, err := reader.RangePage(ctx, "s_tl", 3000, 1000, 0, 10); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Errorf("颠倒区间应报 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

// TestTimelineSearch 片段检索。
func TestTimelineSearch(t *testing.T) {
	reader, _ := seededReader(t)
	ctx := context.Background()

	got, err := reader.Search(ctx, contract.SegmentQuery{SessionID: "s_tl", Keyword: "第二"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Text != "第二句" {
		t.Fatalf("检索结果异常：%+v", got)
	}
	if got[0].StartMs != 1200 {
		t.Errorf("检索结果应带跳转时间，startMs = %d", got[0].StartMs)
	}
	// 无条件全局检索：拒绝（否则等于把整库片段拉给前端）。
	if _, err := reader.Search(ctx, contract.SegmentQuery{}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Errorf("无条件全局检索应被拒绝，实际 %v", err)
	}
}

// TestTimelineUpdateText 人工修正置 edited，并在重转时保留。
func TestTimelineUpdateText(t *testing.T) {
	st := newFakeStore(SessionRow{ID: "s_up", DurationMs: 1000, TranscriptionStatus: StatusDone})
	reader, err := NewTimelineReader(st)
	if err != nil {
		t.Fatalf("NewTimelineReader: %v", err)
	}
	ctx := context.Background()
	saved, err := st.UpsertSegment(ctx, SegmentRow{
		SessionID: "s_up", Seq: 0, StartMs: 0, EndMs: 900, Text: "原始文本", IsFinal: true,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := reader.UpdateSegmentText(ctx, saved.ID, "人工修正"); err != nil {
		t.Fatalf("UpdateSegmentText: %v", err)
	}
	_, segs := st.snapshot()
	if segs[0].Text != "人工修正" || !segs[0].Edited {
		t.Fatalf("修正未生效：%+v", segs[0])
	}
	// 重转覆盖时，人工修正的文本不能被抹掉（edited 行的 text 保护）。
	if _, err := st.UpsertSegment(ctx, SegmentRow{
		SessionID: "s_up", Seq: 0, StartMs: 0, EndMs: 900, Text: "重转文本", IsFinal: true,
	}); err != nil {
		t.Fatalf("重转覆盖: %v", err)
	}
	_, segs2 := st.snapshot()
	if segs2[0].Text != "人工修正" {
		t.Errorf("重转覆盖了人工修正：%q", segs2[0].Text)
	}
	if segs2[0].EndMs != 900 {
		t.Errorf("重转应更新区间：endMs = %d", segs2[0].EndMs)
	}
}

// TestTimelineNotFound 会话不存在 → ERR_NOT_FOUND。
func TestTimelineNotFound(t *testing.T) {
	reader, _ := seededReader(t)
	ctx := context.Background()
	if _, err := reader.Timeline(ctx, "s_none"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Errorf("Timeline 错误 = %v, want ERR_NOT_FOUND", err)
	}
	if _, err := reader.Page(ctx, "s_none", 0, 10); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Errorf("Page 错误 = %v, want ERR_NOT_FOUND", err)
	}
	if err := reader.UpdateSegmentText(ctx, 404, "x"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Errorf("UpdateSegmentText 错误 = %v, want ERR_NOT_FOUND", err)
	}
}
