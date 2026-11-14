package sessions

import (
	"context"
	"strings"
	"unicode"

	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// SearchMode 是关键词检索的执行路径。
//
// 生产只用 SearchAuto；SearchFTS / SearchLike 存在的原因有两条：
//   - 测试要能分别驱动两条路径并断言结果一致（不允许「只测到真实命中的那条」）；
//   - 排查「搜索结果不对」时需要强制某条路径复现。
type SearchMode string

const (
	// SearchAuto 按关键词与 FTS 可用性自动选择（默认）。
	SearchAuto SearchMode = "auto"
	// SearchFTS 优先全文索引。
	SearchFTS SearchMode = "fts"
	// SearchLike 强制子串匹配（慢但语义最完整）。
	SearchLike SearchMode = "like"
)

func (m SearchMode) valid() bool {
	switch m {
	case SearchAuto, SearchFTS, SearchLike:
		return true
	}
	return false
}

// searchPlan 描述一次关键词检索最终怎么执行。
type searchPlan struct {
	// mode 实际执行路径（auto 解析后的结果）。
	mode SearchMode
	// literal 给 LIKE 用的原始关键词（未转义）。
	literal string
	// match FTS5 的 MATCH 表达式（已按 FTS5 语法引用）。
	match string
	// reason 选择该路径的原因，仅用于日志与诊断（不含正文内容）。
	reason string
}

// usable 表示这次检索需要拼条件；空关键词直接跳过。
func (p searchPlan) usable() bool { return p.literal != "" && p.mode != "" }

// args 返回子查询占位符对应的参数（顺序与 subquery 中出现顺序一致）。
func (p searchPlan) args() []any {
	kw := "%" + escapeLike(p.literal) + "%"
	if p.mode == SearchFTS {
		return []any{p.match, kw}
	}
	return []any{kw}
}

// subquery 返回「命中该关键词的 session_id 集合」子查询。
//
// FTS 路径多一个 `text LIKE ?` 复核条件：FTS5 的前缀匹配（`"词"*`）在极端情况下
// 可能命中 token 前缀但正文实际不包含该串（例如索引与正文不一致），复核一次
// 保证不会出现「列表说命中、详情里搜不到」的错觉。
func (p searchPlan) subquery() string {
	if p.mode == SearchFTS {
		return `SELECT seg.session_id FROM segments_fts
			JOIN segments seg ON seg.id = segments_fts.rowid
			WHERE segments_fts MATCH ? AND seg.text LIKE ? ESCAPE '\'`
	}
	return `SELECT session_id FROM segments WHERE text LIKE ? ESCAPE '\'`
}

// planSearch 决定这次关键词检索走哪条路径。
//
// 关键约束（实测结论，见 docs/DEV-ISSUES-S5.md S5-01）：
// `segments_fts` 用的是 unicode61 分词器，它把**一整段连续中文当成一个 token**
// （「我们开始会议吧」是 1 个 token），所以：
//   - `MATCH '会议*'` 命中 0 条 —— 中文子串检索在 FTS 路径上是残缺的；
//   - 英文 `MATCH 'hello*'` 正常。
//
// 因此含非 ASCII 字符、或含空白（多词短语）的关键词一律走 LIKE：
// 宁可慢一点，也不能静默少给结果。纯 ASCII 单词才用 FTS 加速。
func (r *Repo) planSearch(keyword string) searchPlan {
	kw := strings.TrimSpace(keyword)
	if kw == "" {
		return searchPlan{}
	}
	if r.mode == SearchLike {
		return searchPlan{mode: SearchLike, literal: kw, reason: "forced-like"}
	}
	if r.mode == SearchFTS && !storage.FTSAvailable() {
		return searchPlan{mode: SearchLike, literal: kw, reason: "fts-unavailable"}
	}
	if why, ok := ftsUnsupported(kw); !ok {
		return searchPlan{mode: SearchLike, literal: kw, reason: why}
	}
	if !storage.FTSAvailable() {
		return searchPlan{mode: SearchLike, literal: kw, reason: "fts-unavailable"}
	}
	return searchPlan{mode: SearchFTS, literal: kw, match: ftsQuery(kw), reason: "fts"}
}

// ftsUnsupported 判断关键词是否不适合 FTS5 路径。
// 返回 (原因, 是否适合)。
func ftsUnsupported(kw string) (string, bool) {
	for _, r := range kw {
		if r > unicode.MaxASCII {
			return "non-ascii-keyword", false
		}
	}
	if strings.ContainsFunc(kw, unicode.IsSpace) {
		return "multi-token-keyword", false
	}
	return "", true
}

// ftsQuery 把单个 ASCII 词转成 FTS5 前缀查询表达式。
//
// 用双引号包裹（内部引号翻倍转义）再追加 `*`：既避免 `-` `:` `*` 等被当作
// FTS5 语法（用户输入 `a-b` 不该变成「NOT b」），又保留前缀匹配能力。
func ftsQuery(kw string) string {
	return `"` + strings.ReplaceAll(kw, `"`, `""`) + `"*`
}

// DescribeSearch 报告某个关键词最终会走哪条路径，供诊断与测试使用。
func (r *Repo) DescribeSearch(keyword string) (SearchMode, string) {
	p := r.planSearch(keyword)
	if !p.usable() {
		return "", "empty-keyword"
	}
	return p.mode, p.reason
}

// ftsEnsure 在首次使用 FTS 前校验索引覆盖度。
//
// 为什么需要：`segments_fts` 是**外部内容表**，历史数据不会自动进索引
// （触发器只覆盖增量）。若某个库是在 FTS 建立之前就有片段的（或索引曾损坏），
// 直接 MATCH 会静默少给结果 —— 这比慢更糟。这里比对「已索引文档数」与
// 「片段行数」，不一致就 rebuild。
//
// 关键细节（实测，见 docs/DEV-ISSUES-S5.md S5-02）：
// 对外部内容表执行 `count(*) FROM segments_fts` 读的是**内容表**（segments），
// 因此它永远等于片段数、完全无法反映索引状态。真正的索引文档数在 FTS5 的
// 影子表 `segments_fts_docsize` 里（columnsize 默认开启时存在）。
func (r *Repo) ftsEnsure(ctx context.Context) {
	r.ftsOnce.Do(func() {
		db := r.db().WithContext(ctx)
		var segCount, indexed int64
		if err := db.Raw(`SELECT count(*) FROM segments`).Scan(&segCount).Error; err != nil {
			return
		}
		if err := db.Raw(`SELECT count(*) FROM segments_fts_docsize`).Scan(&indexed).Error; err != nil {
			// 影子表不可用（FTS5 以 columnsize=0 编译）时无法判断覆盖度，
			// 不动索引：宁可沿用现状，也不要在无法验证的情形下重建。
			return
		}
		if segCount == indexed {
			return
		}
		if err := db.Exec(`INSERT INTO segments_fts(segments_fts) VALUES('rebuild')`).Error; err != nil {
			// rebuild 失败不阻断检索：只是结果可能不全，记一目志便于诊断。
			logx.For("sessions").Warn("FTS 索引重建失败", "segments", segCount, "indexed", indexed, "err", err)
			return
		}
		logx.For("sessions").Info("FTS 索引已重建", "segments", segCount, "indexedBefore", indexed)
	})
}
