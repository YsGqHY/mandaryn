# S5 开发问题记录（会话库域 FR-04）

> 范围：`internal/sessions`（L2）+ `internal/services/sessionsvc`（L3）+ `internal/contract/session_ext.go`。
> 本文只记录**实测发现**，每条都有可复现条件与当前处置。未解决的问题数见文末统计。

## S5-01 FTS5 unicode61 把连续中文当作一个 token，中文子串检索落空
- 现象：`segments_fts` 用 `tokenize='unicode61'`，而 unicode61 把连续 CJK 字符视为**一个** token。
  片段正文「我们开始会议吧 HELLO」执行 `MATCH '会议'` / `MATCH '会议*'` 均命中 0 条；`MATCH 'hello*'` 命中 1 条。
- 复现条件：任意含中文片段的库，`SELECT count(*) FROM segments_fts WHERE segments_fts MATCH '会议*'` 返回 0。
- 根因：`internal/storage/fts.go` 的分词器选择（`unicode61`）。`docs/DATA-MODEL.md` §5 称「中文按字切分可用」，
  实测**不成立** —— unicode61 不做 CJK 逐字切分（那需要 trigram 或外部分词器）。
- 临时处置：`internal/sessions/search.go` 的 `planSearch` 对**含非 ASCII 字符**或**含空白（多词短语）**的关键词
  一律降级 LIKE 子串匹配；只有纯 ASCII 单词才走 FTS。宁可慢，不可静默少给结果。
  已在 `TestSearchCJKUsesLike`、`TestSearchForcedFTSStillDegradesForCJK` 固定行为。
- 状态：已绕过（根治需把 FTS5 分词器换成 trigram，或改存储层，均超出 S5 允许改动范围，需另立任务）

## S5-02 外部内容表上的 `count(*)` 读的是内容表，无法反映索引覆盖度
- 现象：为「历史片段未进索引」做覆盖度校验时，`SELECT count(*) FROM segments_fts` 与
  `SELECT count(*) FROM segments` **永远相等**（即便刚执行过 `delete-all` 把索引清空）。
- 复现条件：建外部内容表 `segments_fts(content='segments')` → `INSERT INTO segments_fts(segments_fts) VALUES('delete-all')`
  → 再数 `segments_fts`，仍等于片段数；但此时 `MATCH` 命中已归零。
- 根因：FTS5 外部内容表的列值来自内容表，`count(*)` 走的是内容表路径，不是索引。
  真正的已索引文档数在影子表 `segments_fts_docsize`。
- 临时处置：`ftsEnsure` 改为比较 `segments_fts_docsize` 与 `segments` 行数，不一致才 `rebuild`；
  影子表不可用（`columnsize=0` 编译）时**不动索引**，避免无法验证时盲目重建。
  `TestSearchIndexRebuild` 覆盖「索引被清空 → 自动重建 → 仍能命中」。
- 状态：已解决

## S5-03 「音频删除失败」分支无法稳定构造自动化用例
- 现象：想断言「库删成功但音频删不掉 → 进 `AudioFailures` 且不返回 error」，但无法在测试里稳定制造失败：
  在 Windows 上占住文件句柄 / 只读父目录的行为依赖 ACL 与权限，CI 与开发者机器结论不同。
- 复现条件：尝试对只读目录或已打开文件执行 `os.RemoveAll`。
- 根因：平台相关的文件系统权限语义差异，属于测试基础设施限制而非产品缺陷。
- 临时处置：只测**可确定性**的部分 —— 音频已不存在时视为删除成功（幂等，`TestDeleteReportsAudioFailure`）、
  越界路径只删文件不删目录（`TestDeleteKeepsForeignFilesSafe`）。失败分支由
  `DeleteOutcome.AudioFailures` 结构 + warn 日志 + `DeleteDetailed` 契约保证。
- 状态：已绕过

## S5-04 `paths.Resolver` 在 default 层不可解析时整体失败，即使 env/cli 层可用
- 现象：设置 `MANDARYN_DATA_DIR` 后仍报 `paths: cannot resolve user config dir: %AppData% is not defined`，
  `resolver.Effective(KeyDataRoot)` 返回错误。
- 复现条件：在 `%AppData%` / XDG 不可用的环境（如 Git Bash 精简环境）下调用 `paths.New(paths.Options{}).Effective(paths.KeyDataRoot)`。
- 根因：`internal/paths/resolver.go` 的 `resolve()` 无条件先调 `defaultFor(key)` 取默认层，
  该调用失败即 return error —— 但此时高优先级层（env/cli）其实完全可用，默认层值根本没被用到。
- 临时处置：S5 侧不绕过（改 resolver 属 S1 领域）。测试改用
  `paths.New(paths.Options{DefaultDataRoot: func() (string, error) { return tmp, nil }})` 提供可解析的默认层。
  桌面应用实际运行环境 `%AppData%` 一定存在，生产影响低。
- 状态：未解决（建议 S1 把 `defaultFor` 的调用移到「确实需要默认层」的分支内，或让默认层失败只降级不中止）

## S5-05 txt/srt 与 csv/xlsx 对「父目录不存在」的行为不一致
- 现象：导出到不存在的父目录时，`csv` / `xlsx` 报错，`txt` / `srt` 自动创建目录并成功。
- 复现条件：`Export(targetPath=<不存在目录>/a.csv)` 报错；同场景换成 `.txt` 成功。
- 根因：csv/xlsx 复用 `exportsvc`（`validatePath` 要求父目录已存在，前提是原生「另存为」给出的路径必然合法）；
  txt/srt 没有既有实现，用 `filex.WriteAtomic`（语义是「写文件，必要时建目录」，与写配置文件一致）。
- 临时处置：保留差异并写入测试（`TestExportValidation` 同时断言两者）。
  正常路径都由原生「另存为」产生，父目录必然存在，因此不影响真实使用。
- 状态：改设计（接受为有意差异；若将来统一，建议让 `exportsvc` 也自动建目录，而不是让 txt/srt 变严格）

## S5-06 FTS 词首前缀匹配 与 LIKE 任意子串匹配 的语义差异
- 现象：搜 `ell` 时 LIKE 命中 `hello`，FTS 不命中；搜 `hello` 时两者一致。
- 复现条件：`TestSearchFTSDivergesOnInfix`。
- 根因：FTS5 前缀查询（`"ell"*`）只匹配 token 开头；LIKE `%ell%` 匹配任意位置。这是 FTS 的固有语义。
- 临时处置：把差异写成显式断言固定下来（而不是假装两条路径永远等价）；
  auto 模式仍对纯 ASCII 单词用 FTS 以求速度，中文与短语走 LIKE 保正确性。
- 状态：改设计（有意行为，已文档化）

## S5-07 契约缺「音频是否存在」与「时长区间筛选」，用扩展文件绕过
- 现象：API-3.2 的 `Session` 无「音频是否仍在磁盘」字段，前端无法正确禁用回放；
  API-3.1 的 `ListQuery` 无时长区间字段，ROADMAP FR-04 要求的「按时长筛选」在契约上不可达。
- 复现条件：阅读 `internal/contract/session.go` 的 `Session` / `ListQuery`。
- 根因：契约在录制/转写解耦阶段定稿时未包含这两项 UI 需求。
- 临时处置：新建 `internal/contract/session_ext.go`（**只追加**）：
  `SessionDetail{Session, AudioExists}`、`DeleteResult`，并在 L3 增加
  `GetDetail` / `DeleteDetailed` 两个扩展方法（`Get` / `Delete` 保持契约原样，向后兼容）。
  时长区间在 L2 `Query{MinDurationMs, MaxDurationMs}` 完整实现并由测试覆盖，
  待 `ListQuery` 扩展后即可直通，无需改 L2。
- 状态：已绕过（待 DOC-03 §2.3 同步后即可删掉 GetDetail 合并进 Get）

---

## 统计

| 状态 | 条目 |
|------|------|
| 未解决 | 1（S5-04） |
| 已解决 | 1（S5-02） |
| 已绕过 | 3（S5-01、S5-03、S5-07） |
| 改设计 | 2（S5-05、S5-06） |
| 合计 | 7 |
