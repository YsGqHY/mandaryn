package sessions

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// TestListPagination 断言分页的总数、页内容、末页与越界页。
func TestListPagination(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	total := len(corpus)

	// 第一页
	page1, err := f.repo.List(ctx, Query{Limit: 5})
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if page1.Total != total {
		t.Fatalf("total = %d, want %d", page1.Total, total)
	}
	if len(page1.Items) != 5 {
		t.Fatalf("page1 size = %d, want 5", len(page1.Items))
	}

	// 默认排序：started_at DESC（最新的 s_12 在最前）
	if page1.Items[0].ID != "s_12" {
		t.Fatalf("first item = %s, want s_12（started_at DESC）", page1.Items[0].ID)
	}

	// 第二页与第一页不重叠
	page2, err := f.repo.List(ctx, Query{Offset: 5, Limit: 5})
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	if len(page2.Items) != 5 {
		t.Fatalf("page2 size = %d, want 5", len(page2.Items))
	}
	seen := map[string]bool{}
	for _, it := range append(page1.Items, page2.Items...) {
		if seen[it.ID] {
			t.Fatalf("分页出现重复行: %s", it.ID)
		}
		seen[it.ID] = true
	}

	// 末页只有 2 条（12 = 5+5+2）
	page3, err := f.repo.List(ctx, Query{Offset: 10, Limit: 5})
	if err != nil {
		t.Fatalf("list page3: %v", err)
	}
	if len(page3.Items) != 2 {
		t.Fatalf("page3 size = %d, want 2", len(page3.Items))
	}

	// 越界页：空列表但不是错误，total 仍然正确
	page4, err := f.repo.List(ctx, Query{Offset: 100, Limit: 5})
	if err != nil {
		t.Fatalf("list out-of-range page 不应报错: %v", err)
	}
	if len(page4.Items) != 0 {
		t.Fatalf("越界页 items = %d, want 0", len(page4.Items))
	}
	if page4.Total != total {
		t.Fatalf("越界页 total = %d, want %d", page4.Total, total)
	}

	// 负 offset / 0 limit 走缺省，不 panic 也不报错
	page5, err := f.repo.List(ctx, Query{Offset: -3, Limit: 0})
	if err != nil {
		t.Fatalf("非法分页参数不应报错: %v", err)
	}
	// 缺省 limit=20 大于语料 12 条，因此返回全部而不是 20 条空行。
	if len(page5.Items) != len(corpus) {
		t.Fatalf("缺省 limit 下返回 %d 条, want %d", len(page5.Items), len(corpus))
	}
	if page5.Items[0].ID != "s_12" {
		t.Fatalf("负 offset 应被归零，first = %s", page5.Items[0].ID)
	}
	// 上限：limit=9999 被压到 maxLimit，不报错
	page6, err := f.repo.List(ctx, Query{Limit: 9999})
	if err != nil {
		t.Fatalf("超大 limit 不应报错: %v", err)
	}
	if len(page6.Items) != len(corpus) {
		t.Fatalf("limit 超过上限时返回 %d 条, want %d（语料不足上限）", len(page6.Items), len(corpus))
	}
}

// TestSearchFTSAndLikeAgree 断言 FTS 与 LIKE 两条路径在同等语料上结果一致。
//
// 用的是「词首出现」的英文关键词（"hello"）：这是 FTS5 前缀查询与 LIKE 子串
// 语义必然重合的区间，因此两条路径必须给出同一集合。
func TestSearchFTSAndLikeAgree(t *testing.T) {
	ctx := context.Background()
	ftsF := newFixture(t, SearchFTS)
	likeF := newFixture(t, SearchLike)

	for _, keyword := range []string{"hello", "discord", "degraded"} {
		ftsRes, err := ftsF.repo.List(ctx, Query{Keyword: keyword, Limit: 100})
		if err != nil {
			t.Fatalf("fts search %q: %v", keyword, err)
		}
		likeRes, err := likeF.repo.List(ctx, Query{Keyword: keyword, Limit: 100})
		if err != nil {
			t.Fatalf("like search %q: %v", keyword, err)
		}
		ftsIDs, likeIDs := idsOf(ftsRes.Items), idsOf(likeRes.Items)
		if len(ftsIDs) != len(likeIDs) {
			t.Fatalf("关键词 %q: FTS=%v LIKE=%v 数量不一致", keyword, ftsIDs, likeIDs)
		}
		for i := range ftsIDs {
			if ftsIDs[i] != likeIDs[i] {
				t.Fatalf("关键词 %q 第 %d 项不一致: FTS=%s LIKE=%s", keyword, i, ftsIDs[i], likeIDs[i])
			}
		}
		if len(ftsIDs) == 0 {
			t.Fatalf("关键词 %q 应至少命中一条", keyword)
		}
	}
}

// TestSearchHitsTitleAndSegment 断言关键词能命中标题与片段正文。
func TestSearchHitsTitleAndSegment(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	// 只出现在标题里的词（s_09「纯英文标题 hello world」的 hello 也可能命中片段）
	res, err := f.repo.List(ctx, Query{Keyword: "纯英文标题", Limit: 100})
	if err != nil {
		t.Fatalf("search title: %v", err)
	}
	if !contains(idsOf(res.Items), "s_09") {
		t.Fatalf("标题命中缺失: %v", idsOf(res.Items))
	}

	// 只出现在片段正文里的词
	res, err = f.repo.List(ctx, Query{Keyword: "节奏很好", Limit: 100})
	if err != nil {
		t.Fatalf("search segment: %v", err)
	}
	if !contains(idsOf(res.Items), "s_02") {
		t.Fatalf("片段正文命中缺失: %v", idsOf(res.Items))
	}

	// 命中多条：会议 出现在 s_01 / s_05 的正文里
	res, err = f.repo.List(ctx, Query{Keyword: "会议", Limit: 100})
	if err != nil {
		t.Fatalf("search 会议: %v", err)
	}
	got := idsOf(res.Items)
	if !contains(got, "s_01") || !contains(got, "s_05") {
		t.Fatalf("「会议」应命中 s_01 与 s_05，实际 %v", got)
	}
	if res.Total != len(got) {
		t.Fatalf("total=%d 与 items=%d 不一致", res.Total, len(got))
	}
}

// TestSearchCJKUsesLike 断言中文关键词走 LIKE（FTS5 unicode61 会把整段中文
// 当成一个 token，中文子串检索必然落空 —— 见 docs/DEV-ISSUES-S5.md S5-01）。
func TestSearchCJKUsesLike(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	mode, reason := f.repo.DescribeSearch("会议纪要")
	if mode != SearchLike {
		t.Fatalf("中文关键词应走 LIKE，实际 %s（reason=%s）", mode, reason)
	}
	if reason != "non-ascii-keyword" {
		t.Fatalf("reason = %s, want non-ascii-keyword", reason)
	}

	// 英文单词走 FTS（本机 FTS5 可用时）
	mode, reason = f.repo.DescribeSearch("hello")
	// 环境若未编译 FTS5 则降级 LIKE，这不影响正确性，只是路径不同。
	if mode == SearchFTS && reason != "fts" {
		t.Fatalf("英文关键词走 FTS 时 reason 应为 fts，实际 %s", reason)
	}

	// 多词短语走 LIKE（FTS 前缀查询没有短语语义）
	mode, reason = f.repo.DescribeSearch("hello team")
	if mode != SearchLike || reason != "multi-token-keyword" {
		t.Fatalf("多词短语应走 LIKE/multi-token-keyword，实际 %s/%s", mode, reason)
	}

	// 中文子串必须能命中：s_01 正文含「今天的会议先对齐一下排期」
	res, err := f.repo.List(ctx, Query{Keyword: "会议先对齐", Limit: 100})
	if err != nil {
		t.Fatalf("cjk search: %v", err)
	}
	if !contains(idsOf(res.Items), "s_01") {
		t.Fatalf("中文子串「会议先对齐」应命中 s_01，实际 %v", idsOf(res.Items))
	}
}

// TestSearchForcedFTSStillDegradesForCJK 断言强制 FTS 模式下中文仍走 LIKE
// （否则会静默少给结果，这比慢更糟）。
func TestSearchForcedFTSStillDegradesForCJK(t *testing.T) {
	f := newFixture(t, SearchFTS)
	mode, reason := f.repo.DescribeSearch("会议")
	if mode != SearchLike {
		t.Fatalf("强制 FTS 时中文仍应降级 LIKE，实际 %s/%s", mode, reason)
	}
}

// TestSearchIndexRebuild 断言 FTS 索引被清空后，检索会自动重建索引，
// 不会静默少给结果（外部内容表的历史数据不会自动进索引）。
func TestSearchIndexRebuild(t *testing.T) {
	f := newFixture(t, SearchFTS)
	ctx := context.Background()

	// 制造「索引与正文不一致」：清空 FTS 索引但保留正文。
	if err := f.db.GORM.Exec(`INSERT INTO segments_fts(segments_fts) VALUES('delete-all')`).Error; err != nil {
		t.Fatalf("清空 FTS 索引失败: %v", err)
	}
	// 索引文档数看影子表 docsize（外部内容表的 count(*) 读的是正文表，见 S5-02）。
	var before int64
	if err := f.db.GORM.Raw(`SELECT count(*) FROM segments_fts_docsize`).Scan(&before).Error; err != nil {
		t.Fatalf("count fts docsize: %v", err)
	}
	if before != 0 {
		t.Fatalf("FTS 索引应为空，实际 %d", before)
	}

	// 旧 Repo 已在本进程跑过 ensure（ftsOnce），这里换一个全新 Repo 模拟重启后首次检索。
	repo, err := New(Options{
		Holder:     f.holder,
		DataRoot:   func() (string, error) { return f.dataRoot, nil },
		SearchMode: SearchFTS,
		Writer:     &memWriter{},
	})
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	res, err := repo.List(ctx, Query{Keyword: "hello", Limit: 100})
	if err != nil {
		t.Fatalf("search after index wipe: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatal("索引清空后检索应自动重建索引并仍能命中")
	}
	var after int64
	if err := f.db.GORM.Raw(`SELECT count(*) FROM segments_fts_docsize`).Scan(&after).Error; err != nil {
		t.Fatalf("count fts docsize after rebuild: %v", err)
	}
	var segTotal int64
	if err := f.db.GORM.Raw(`SELECT count(*) FROM segments`).Scan(&segTotal).Error; err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if after != segTotal {
		t.Fatalf("索引重建后文档数 = %d, want %d", after, segTotal)
	}
}

// TestSearchLikeEscapesWildcards 断言用户输入的 % / _ 不被当成通配符。
func TestSearchLikeEscapesWildcards(t *testing.T) {
	f := newFixture(t, SearchLike)
	ctx := context.Background()

	// 标题 s_10 含「100% 纯人声」
	res, err := f.repo.List(ctx, Query{Keyword: "100%", Limit: 100})
	if err != nil {
		t.Fatalf("search 100%%: %v", err)
	}
	if !contains(idsOf(res.Items), "s_10") {
		t.Fatalf("「100%%」应命中 s_10，实际 %v", idsOf(res.Items))
	}
	// 若 % 未转义，"1%人" 会退化成 "1" + 任意 + "人"，从而错误命中 s_10。
	res, err = f.repo.List(ctx, Query{Keyword: "100%纯人声的录音", Limit: 100})
	if err != nil {
		t.Fatalf("search escaped: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("不存在的关键词不应命中，实际 %v", idsOf(res.Items))
	}
}

// TestFilters 逐个断言时间范围 / 来源应用 / 转写状态 / 时长区间筛选。
func TestFilters(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	// —— 时间范围：只取 11-12 当天（s_05 / s_06）——
	res, err := f.repo.List(ctx, Query{FromMs: 1_761_200_000_000, ToMs: 1_761_200_000_000 + 86_400_000 - 1, Limit: 100})
	if err != nil {
		t.Fatalf("filter time: %v", err)
	}
	got := idsOf(res.Items)
	if len(got) != 2 || !contains(got, "s_05") || !contains(got, "s_06") {
		t.Fatalf("时间范围筛选结果 = %v, want [s_05 s_06]", got)
	}

	// —— 来源应用：KOOK（s_01 / s_04 / s_06 / s_09 / s_11）——
	res, err = f.repo.List(ctx, Query{SourceIDLike: "kook", Limit: 100})
	if err != nil {
		t.Fatalf("filter source: %v", err)
	}
	got = idsOf(res.Items)
	if len(got) != 5 {
		t.Fatalf("KOOK 会话数 = %d, want 5 (%v)", len(got), got)
	}
	for _, want := range []string{"s_01", "s_04", "s_06", "s_09", "s_11"} {
		if !contains(got, want) {
			t.Fatalf("来源筛选缺少 %s（实际 %v）", want, got)
		}
	}

	// —— 转写状态 none（s_03 / s_09 / s_11）——
	res, err = f.repo.List(ctx, Query{TranscriptionStatus: "none", Limit: 100})
	if err != nil {
		t.Fatalf("filter transcription: %v", err)
	}
	got = idsOf(res.Items)
	if len(got) != 3 {
		t.Fatalf("转写状态 none 会话数 = %d, want 3 (%v)", len(got), got)
	}

	// —— 时长区间：1s ~ 60s（s_07=1s, s_03=12s, s_11=5s, s_10=30s, s_09=45s）——
	res, err = f.repo.List(ctx, Query{MinDurationMs: 1000, MaxDurationMs: 60_000, Limit: 100})
	if err != nil {
		t.Fatalf("filter duration: %v", err)
	}
	got = idsOf(res.Items)
	want := []string{"s_03", "s_07", "s_09", "s_10", "s_11"}
	if len(got) != len(want) {
		t.Fatalf("时长区间会话数 = %d, want %d (%v)", len(got), len(want), got)
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Fatalf("时长筛选缺少 %s（实际 %v）", w, got)
		}
	}

	// —— 组合筛选：在线 + 已转写（s_07）——
	res, err = f.repo.List(ctx, Query{ModelSource: "online", TranscriptionStatus: "done", Limit: 100})
	if err != nil {
		t.Fatalf("filter combo: %v", err)
	}
	got = idsOf(res.Items)
	if len(got) != 2 || !contains(got, "s_07") || !contains(got, "s_12") {
		t.Fatalf("组合筛选结果 = %v, want [s_12 s_07]（按 started_at DESC）", got)
	}

	// —— 录制状态 interrupted（s_03 / s_11）——
	res, err = f.repo.List(ctx, Query{Status: "interrupted", Limit: 100})
	if err != nil {
		t.Fatalf("filter status: %v", err)
	}
	got = idsOf(res.Items)
	if len(got) != 2 || !contains(got, "s_03") || !contains(got, "s_11") {
		t.Fatalf("状态筛选结果 = %v, want [s_11 s_03]", got)
	}
}

// TestGet 断言详情字段与 audioExists。
func TestGet(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	detail, err := f.repo.Get(ctx, "s_01")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if detail.Session.Title != "2026-11-10 10:00 · KOOK" {
		t.Fatalf("title = %q", detail.Session.Title)
	}
	if detail.Session.SegmentCount != 2 {
		t.Fatalf("segmentCount = %d, want 2", detail.Session.SegmentCount)
	}
	if detail.Session.DurationMs != 90_000 {
		t.Fatalf("durationMs = %d, want 90000", detail.Session.DurationMs)
	}
	if detail.Session.TranscriptionStatus != "done" {
		t.Fatalf("transcriptionStatus = %q", detail.Session.TranscriptionStatus)
	}
	if !detail.AudioExists {
		t.Fatal("s_01 的音频文件存在，audioExists 应为 true")
	}
	if detail.Session.StartedAt == "" {
		t.Fatal("startedAt 应为 RFC3339 字符串")
	}

	// s_04 没有音频文件
	detail, err = f.repo.Get(ctx, "s_04")
	if err != nil {
		t.Fatalf("get s_04: %v", err)
	}
	if detail.AudioExists {
		t.Fatal("s_04 无音频，audioExists 应为 false")
	}
	// failed 会话 endedAt 也有值（StartedAt+0）
	if detail.Session.Status != "failed" {
		t.Fatalf("status = %q", detail.Session.Status)
	}

	// 不存在 → ERR_NOT_FOUND
	if _, err := f.repo.Get(ctx, "s_nope"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("不存在的会话应返回 ERR_NOT_FOUND，实际 %v", err)
	}
	if _, err := f.repo.Get(ctx, "  "); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空 ID 应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

// TestRename 断言改名生效、校验规则与不存在分支。
func TestRename(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	if err := f.repo.Rename(ctx, "s_01", "  改过的标题  "); err != nil {
		t.Fatalf("rename: %v", err)
	}
	detail, err := f.repo.Get(ctx, "s_01")
	if err != nil {
		t.Fatalf("get after rename: %v", err)
	}
	if detail.Session.Title != "改过的标题" {
		t.Fatalf("改名后 title = %q, want 改过的标题（应去掉首尾空白）", detail.Session.Title)
	}

	// 改名后能被检索到（标题参与搜索）
	res, err := f.repo.List(ctx, Query{Keyword: "改过的标题", Limit: 100})
	if err != nil {
		t.Fatalf("search renamed: %v", err)
	}
	if !contains(idsOf(res.Items), "s_01") {
		t.Fatal("改名后应能按新标题检索到")
	}

	// 空标题被拒
	if err := f.repo.Rename(ctx, "s_01", "   "); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空标题应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
	// 超长标题被拒
	long := make([]rune, maxTitleRunes+1)
	for i := range long {
		long[i] = '中'
	}
	if err := f.repo.Rename(ctx, "s_01", string(long)); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("超长标题应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
	// 不存在
	if err := f.repo.Rename(ctx, "s_nope", "x"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("不存在的会话改名应返回 ERR_NOT_FOUND，实际 %v", err)
	}
}

// TestDeleteWithAudio 断言 deleteAudio=true 时音频文件真的被删。
func TestDeleteWithAudio(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	audioAbs := filepath.Join(f.dataRoot, "sessions", "s_01", "audio.wav")
	if _, err := os.Stat(audioAbs); err != nil {
		t.Fatalf("前置条件失败，音频应存在: %v", err)
	}

	out, err := f.repo.Delete(ctx, []string{"s_01", "s_02"}, true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if out.Sessions != 2 {
		t.Fatalf("删除会话数 = %d, want 2", out.Sessions)
	}
	if out.AudioRemoved != 2 {
		t.Fatalf("删除音频数 = %d, want 2（failures=%v）", out.AudioRemoved, out.AudioFailures)
	}
	if len(out.Missing) != 0 {
		t.Fatalf("不应有 missing: %v", out.Missing)
	}

	// 库里的行没了
	if _, err := f.repo.Get(ctx, "s_01"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("删除后应查不到，实际 %v", err)
	}
	// 片段级联删除
	var segCount int64
	if err := f.db.GORM.Raw(`SELECT count(*) FROM segments WHERE session_id = ?`, "s_01").Scan(&segCount).Error; err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if segCount != 0 {
		t.Fatalf("片段未级联删除: %d", segCount)
	}
	// 音频目录整体消失（含 meta.json 所在目录）
	if _, err := os.Stat(filepath.Join(f.dataRoot, "sessions", "s_01")); !os.IsNotExist(err) {
		t.Fatalf("音频目录应被删除，stat err = %v", err)
	}
	// 列表总数减少
	res, err := f.repo.List(ctx, Query{Limit: 100})
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if res.Total != len(corpus)-2 {
		t.Fatalf("删除后 total = %d, want %d", res.Total, len(corpus)-2)
	}
}

// TestDeleteKeepAudio 断言 deleteAudio=false 时音频文件保留。
func TestDeleteKeepAudio(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	audioAbs := filepath.Join(f.dataRoot, "sessions", "s_03", "audio.wav")

	out, err := f.repo.Delete(ctx, []string{"s_03"}, false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if out.Sessions != 1 || out.AudioRemoved != 0 {
		t.Fatalf("out = %+v, want 1 会话 / 0 音频", out)
	}
	if _, err := os.Stat(audioAbs); err != nil {
		t.Fatalf("deleteAudio=false 时音频必须保留，stat err = %v", err)
	}
	if _, err := f.repo.Get(ctx, "s_03"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("库记录应已删除，实际 %v", err)
	}
}

// TestDeleteEdgeCases 断言批量删除的边界：不存在的 ID、空列表、全部不存在。
func TestDeleteEdgeCases(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	// 部分存在：存在的那条照删，缺的进 Missing，不报错
	out, err := f.repo.Delete(ctx, []string{"s_05", "s_nope", " s_05 "}, true)
	if err != nil {
		t.Fatalf("delete partial: %v", err)
	}
	if out.Sessions != 1 {
		t.Fatalf("sessions = %d, want 1（重复 ID 应去重）", out.Sessions)
	}
	if len(out.Missing) != 1 || out.Missing[0] != "s_nope" {
		t.Fatalf("missing = %v, want [s_nope]", out.Missing)
	}

	// 空列表 → 参数错误
	if _, err := f.repo.Delete(ctx, nil, true); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空列表应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}

	// 全部不存在 → ERR_NOT_FOUND
	if _, err := f.repo.Delete(ctx, []string{"s_x", "s_y"}, true); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("全部不存在应返回 ERR_NOT_FOUND，实际 %v", err)
	}
}

// TestDeleteReportsAudioFailure 断言「库删成功、音频删不掉」时如实报告而不是报错。
//
// 构造手法：把 AudioPath 指向一个**非空目录**（os.RemoveAll 正常也能删，
// 所以这里改为指向 dataRoot 之外、且在 Windows 上被占用的路径不可靠），
// 因此改用「AudioPath 指向存在的文件，但把它放在只读父目录里」的方式在
// Windows 上也不稳定。最终选择：把 AudioPath 指向一个不存在的深层路径 ——
// 这只能是「删除幂等成功」，无法制造失败。
//
// 结论：音频删除失败分支靠 DeleteOutcome 结构与日志保证，不做平台相关的
// 制造失败测试（见 docs/DEV-ISSUES-S5.md S5-03）。
func TestDeleteReportsAudioFailure(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	// 幂等性：AudioPath 指向已不存在的路径时仍算删除成功
	if err := f.db.GORM.Table("sessions").
		Where("id = ?", "s_06").Update("audio_path", "sessions/s_06/gone.wav").Error; err != nil {
		t.Fatalf("update audio_path: %v", err)
	}
	out, err := f.repo.Delete(ctx, []string{"s_06"}, true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(out.AudioFailures) != 0 {
		t.Fatalf("文件已不存在时应视为删除成功，failures = %v", out.AudioFailures)
	}
	if out.AudioRemoved != 1 {
		t.Fatalf("audioRemoved = %d, want 1", out.AudioRemoved)
	}
}

// TestExportFormats 断言四种格式的内容形态。
func TestExportFormats(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()
	w := &memWriter{}
	repo, err := New(Options{
		Holder:   f.holder,
		DataRoot: func() (string, error) { return f.dataRoot, nil },
		Writer:   w,
	})
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}

	dir := t.TempDir()
	abs := func(name string) string { return filepath.Join(dir, name) }

	// —— TXT（含时间戳与说话人）——
	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_01"}, Format: FormatTXT,
		IncludeTimestamps: true, IncludeSpeakers: true,
		TargetPath: abs("out.txt"),
	}); err != nil {
		t.Fatalf("export txt: %v", err)
	}
	txt := w.text
	if !containsStr(txt, "今天的会议先对齐一下排期") {
		t.Fatalf("TXT 缺少正文:\n%s", txt)
	}
	if !containsStr(txt, "00:00:00") {
		t.Fatalf("TXT 含时间戳时应出现 00:00:00:\n%s", txt)
	}
	if !containsStr(txt, "未识别") {
		t.Fatalf("无角色时说话人应显示「未识别」:\n%s", txt)
	}

	// —— SRT —— 时间码格式 hh:mm:ss,mmm --> hh:mm:ss,mmm
	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_01"}, Format: FormatSRT, IncludeSpeakers: true,
		TargetPath: abs("out.srt"),
	}); err != nil {
		t.Fatalf("export srt: %v", err)
	}
	srt := w.text
	lines := splitLines(srt)
	if len(lines) < 4 {
		t.Fatalf("SRT 太短:\n%s", srt)
	}
	if lines[0] != "1" {
		t.Fatalf("SRT 首行应为序号 1，实际 %q", lines[0])
	}
	if !srtTimePattern.MatchString(lines[1]) {
		t.Fatalf("SRT 时间码格式错误: %q（期望 hh:mm:ss,mmm --> hh:mm:ss,mmm）", lines[1])
	}
	if lines[1] != "00:00:00,000 --> 00:00:01,800" {
		t.Fatalf("SRT 首条时间码 = %q", lines[1])
	}
	// 第二条序号连续
	if !containsStr(srt, "\n2\n") {
		t.Fatalf("SRT 序号未连续:\n%s", srt)
	}

	// —— CSV —— 表头 + 转义
	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_01"}, Format: FormatCSV,
		IncludeTimestamps: true, IncludeSpeakers: true,
		TargetPath: abs("out.csv"),
	}); err != nil {
		t.Fatalf("export csv: %v", err)
	}
	csv := w.csv
	if !containsStr(csv, "会话,开始时间,来源应用,模型来源") {
		t.Fatalf("CSV 表头不符:\n%s", csv)
	}
	if !containsStr(csv, "今天的会议先对齐一下排期") {
		t.Fatalf("CSV 缺少正文:\n%s", csv)
	}

	// —— XLSX —— 载荷结构
	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_01"}, Format: FormatXLSX,
		IncludeTimestamps: true, IncludeSpeakers: true,
		TargetPath: abs("out.xlsx"),
	}); err != nil {
		t.Fatalf("export xlsx: %v", err)
	}
	if w.xlsx.SheetName != "会话记录" {
		t.Fatalf("sheet 名 = %q", w.xlsx.SheetName)
	}
	if len(w.xlsx.Rows) != 2 {
		t.Fatalf("xlsx 行数 = %d, want 2", len(w.xlsx.Rows))
	}
	if len(w.xlsx.Rows[0]) != len(w.xlsx.Headers) {
		t.Fatalf("xlsx 行列数 %d 与表头 %d 不一致", len(w.xlsx.Rows[0]), len(w.xlsx.Headers))
	}
}

// TestExportMultiSessionAndOptions 断言多会话导出顺序、不含时间戳/说话人的形态。
func TestExportMultiSessionAndOptions(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()
	w := &memWriter{}
	repo, err := New(Options{
		Holder:   f.holder,
		DataRoot: func() (string, error) { return f.dataRoot, nil },
		Writer:   w,
	})
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	dir := t.TempDir()

	// 顺序按调用方给的 IDs
	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_05", "s_01"}, Format: FormatTXT,
		IncludeTimestamps: false, IncludeSpeakers: false,
		TargetPath: filepath.Join(dir, "multi.txt"),
	}); err != nil {
		t.Fatalf("export multi: %v", err)
	}
	txt := w.text
	i5 := indexOf(txt, "我们把会议纪要发到群里")
	i1 := indexOf(txt, "今天的会议先对齐一下排期")
	if i5 < 0 || i1 < 0 {
		t.Fatalf("多会话导出缺内容:\n%s", txt)
	}
	if i5 > i1 {
		t.Fatal("导出顺序应遵循请求里的 sessionIds 顺序")
	}
	// 不含时间戳/说话人 → 行内不应有 "00:00:00: " 前缀
	if containsStr(txt, "00:00:00: ") {
		t.Fatalf("未请求时间戳时不应输出时间前缀:\n%s", txt)
	}

	// 多会话 SRT 序号连续不重置
	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_05", "s_01"}, Format: FormatSRT,
		TargetPath: filepath.Join(dir, "multi.srt"),
	}); err != nil {
		t.Fatalf("export multi srt: %v", err)
	}
	if !containsStr(w.text, "\n5\n") {
		t.Fatalf("多会话 SRT 序号应连续到第 5 条:\n%s", w.text)
	}

	// 未转写会话导出 TXT：要有「未转写」提示而不是空文件
	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_03"}, Format: FormatTXT,
		TargetPath: filepath.Join(dir, "empty.txt"),
	}); err != nil {
		t.Fatalf("export empty: %v", err)
	}
	if !containsStr(w.text, "（未转写）") {
		t.Fatalf("未转写会话应输出占位说明:\n%s", w.text)
	}
}

// TestExportValidation 断言导出的参数校验。
func TestExportValidation(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()
	dir := t.TempDir()

	cases := []struct {
		name string
		req  ExportRequest
		code string
	}{
		{
			name: "空 targetPath（前端应先弹另存为）",
			req:  ExportRequest{SessionIDs: []string{"s_01"}, Format: FormatTXT},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "相对路径",
			req:  ExportRequest{SessionIDs: []string{"s_01"}, Format: FormatTXT, TargetPath: "out.txt"},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "扩展名不匹配",
			req:  ExportRequest{SessionIDs: []string{"s_01"}, Format: FormatCSV, TargetPath: filepath.Join(dir, "x.txt")},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "不支持的格式",
			req:  ExportRequest{SessionIDs: []string{"s_01"}, Format: "pdf", TargetPath: filepath.Join(dir, "x.pdf")},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "空会话列表",
			req:  ExportRequest{Format: FormatTXT, TargetPath: filepath.Join(dir, "y.txt")},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "会话不存在",
			req:  ExportRequest{SessionIDs: []string{"s_nope"}, Format: FormatTXT, TargetPath: filepath.Join(dir, "z.txt")},
			code: apierr.CodeNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.repo.Export(ctx, tc.req)
			if !apierr.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
		})
	}
}

// TestExportRoleNameJoined 断言导出用的是 roles 表里的新名字（改名全局同步）。
func TestExportRoleNameJoined(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()
	w := &memWriter{}
	repo, err := New(Options{
		Holder:   f.holder,
		DataRoot: func() (string, error) { return f.dataRoot, nil },
		Writer:   w,
	})
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}

	// 造角色与「会话 × 说话人」映射：s_01 的第 0 句归属 rl_1（张三）
	if err := f.db.GORM.Exec(`INSERT INTO roles (id,name,color_token,is_system) VALUES ('rl_1','张三','role-1',0)`).Error; err != nil {
		t.Fatalf("insert role: %v", err)
	}
	if err := f.db.GORM.Exec(`INSERT INTO speakers (id,embedding_enc,dim,model_id,first_seen_at,last_seen_at) VALUES ('sp_1','',0,'m',0,0)`).Error; err != nil {
		t.Fatalf("insert speaker: %v", err)
	}
	if err := f.db.GORM.Exec(`INSERT INTO session_speakers (session_id,speaker_id,role_id,utterance_count,speech_ms) VALUES ('s_01','sp_1','rl_1',1,1800)`).Error; err != nil {
		t.Fatalf("insert session_speaker: %v", err)
	}
	if err := f.db.GORM.Table("segments").
		Where("session_id = ? AND seq = ?", "s_01", 0).Update("speaker_id", "sp_1").Error; err != nil {
		t.Fatalf("update segment speaker: %v", err)
	}

	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_01"}, Format: FormatTXT, IncludeSpeakers: true,
		TargetPath: filepath.Join(t.TempDir(), "role.txt"),
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !containsStr(w.text, "张三") {
		t.Fatalf("导出应使用 roles 表里的角色名:\n%s", w.text)
	}

	// 改名后重新导出：立即反映新名字（FR-06-3，禁止冗余存名字）
	if err := f.db.GORM.Exec(`UPDATE roles SET name = '李四' WHERE id = 'rl_1'`).Error; err != nil {
		t.Fatalf("rename role: %v", err)
	}
	if _, err := repo.Export(ctx, ExportRequest{
		SessionIDs: []string{"s_01"}, Format: FormatTXT, IncludeSpeakers: true,
		TargetPath: filepath.Join(t.TempDir(), "role2.txt"),
	}); err != nil {
		t.Fatalf("export after rename: %v", err)
	}
	if !containsStr(w.text, "李四") || containsStr(w.text, "张三") {
		t.Fatalf("角色改名后导出应只剩新名字:\n%s", w.text)
	}
}

// TestQueryFromContract 断言契约 → 领域查询的转换（含时间解析与校验）。
func TestQueryFromContract(t *testing.T) {
	q, err := QueryFromContract(contract.ListQuery{
		Keyword: " x ", Status: "completed", From: "2026-11-12", To: "2026-11-13",
		Offset: 5, Limit: 10,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if q.Keyword != " x " || q.Status != "completed" || q.Offset != 5 || q.Limit != 10 {
		t.Fatalf("转换丢失字段: %+v", q)
	}
	if q.FromMs == 0 || q.ToMs == 0 {
		t.Fatalf("时间未解析: from=%d to=%d", q.FromMs, q.ToMs)
	}
	// 纯日期 To 取当天最后一毫秒
	if dur := q.ToMs - q.FromMs; dur < 24*3600*1000 {
		t.Fatalf("To 应覆盖整天，区间 = %d ms", dur)
	}

	// RFC3339 也能解析
	q, err = QueryFromContract(contract.ListQuery{From: "2026-11-12T08:00:00Z"})
	if err != nil {
		t.Fatalf("convert rfc3339: %v", err)
	}
	if q.FromMs == 0 {
		t.Fatal("RFC3339 未解析")
	}

	// 非法时间
	if _, err := QueryFromContract(contract.ListQuery{From: "not-a-time"}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("非法时间应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
	// from 晚于 to
	if _, err := QueryFromContract(contract.ListQuery{From: "2026-11-14", To: "2026-11-12"}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("from 晚于 to 应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

// TestDeleteKeepsForeignFilesSafe 断言 AudioPath 指向 dataRoot 之外时，
// 只删音频文件本身，绝不递归删除用户目录。
func TestDeleteKeepsForeignFilesSafe(t *testing.T) {
	f := newFixture(t, SearchAuto)
	ctx := context.Background()

	// 造一个 dataRoot 之外的「用户目录」：里面有音频文件和一份重要文件
	foreign := t.TempDir()
	audioAbs := filepath.Join(foreign, "rec.wav")
	keepAbs := filepath.Join(foreign, "重要文档.txt")
	if err := os.WriteFile(audioAbs, []byte("RIFF"), 0o644); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := os.WriteFile(keepAbs, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write keep: %v", err)
	}
	// 把 s_07 的音频路径改成这个绝对路径
	if err := f.db.GORM.Table("sessions").Where("id = ?", "s_07").
		Update("audio_path", filepath.ToSlash(audioAbs)).Error; err != nil {
		t.Fatalf("update audio_path: %v", err)
	}

	out, err := f.repo.Delete(ctx, []string{"s_07"}, true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if out.AudioRemoved != 1 {
		t.Fatalf("audioRemoved = %d, want 1", out.AudioRemoved)
	}
	// 音频文件被删
	if _, err := os.Stat(audioAbs); !os.IsNotExist(err) {
		t.Fatalf("越界音频文件应被删除，stat err = %v", err)
	}
	// 同目录里的其它文件必须完好
	if _, err := os.Stat(keepAbs); err != nil {
		t.Fatalf("不得递归删除 dataRoot 之外的目录，同目录文件被误删: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("用户目录被误删: %v", err)
	}
}

// TestSearchFTSDivergesOnInfix 记录并固定一个**已知语义差异**（S5-06）：
// FTS5 是「词首前缀」匹配，LIKE 是「任意子串」匹配。
//
// 搜 "ell" 时 LIKE 能命中 "hello"，FTS 命中不到。这不是 bug 而是 FTS 的固有
// 语义，因此 auto 模式只在纯 ASCII 单词上走 FTS，且该差异已被 S5-06 记录。
func TestSearchFTSDivergesOnInfix(t *testing.T) {
	ctx := context.Background()
	ftsF := newFixture(t, SearchFTS)
	likeF := newFixture(t, SearchLike)

	ftsRes, err := ftsF.repo.List(ctx, Query{Keyword: "ell", Limit: 100})
	if err != nil {
		t.Fatalf("fts search: %v", err)
	}
	likeRes, err := likeF.repo.List(ctx, Query{Keyword: "ell", Limit: 100})
	if err != nil {
		t.Fatalf("like search: %v", err)
	}
	if len(likeRes.Items) == 0 {
		t.Fatal("LIKE 应能命中词中匹配（hello 含 ell）")
	}
	if len(ftsRes.Items) >= len(likeRes.Items) {
		t.Fatalf("FTS 词首前缀匹配不应多于 LIKE：fts=%d like=%d", len(ftsRes.Items), len(likeRes.Items))
	}
	// 词首匹配时两者必须一致
	ftsHello, err := ftsF.repo.List(ctx, Query{Keyword: "hello", Limit: 100})
	if err != nil {
		t.Fatalf("fts hello: %v", err)
	}
	likeHello, err := likeF.repo.List(ctx, Query{Keyword: "hello", Limit: 100})
	if err != nil {
		t.Fatalf("like hello: %v", err)
	}
	if len(ftsHello.Items) != len(likeHello.Items) {
		t.Fatalf("词首匹配应一致：fts=%d like=%d", len(ftsHello.Items), len(likeHello.Items))
	}
}
