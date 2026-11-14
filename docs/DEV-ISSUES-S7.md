# S7 说话人分离与角色命名（FR-06）问题记录

> 记录 S7（`internal/speaker` + `internal/services/speakersvc`）实施过程中实际踩到的坑。
> 每条含现象 / 复现条件 / 根因 / 临时处置 / 状态。
> 状态取值：未解决 / 已绕过 / 已解决 / 改设计。

---

## S7-01 sherpa 声纹实现必须拆成子包，否则整个测试二进制起不来

- 现象：把 `github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx` 直接 import 进 `internal/speaker`
  后，`go test ./internal/speaker/...` 在 Windows 上以 `exit status 0xc0000135`
  （STATUS_DLL_NOT_FOUND）直接退出，**连一个测试都跑不起来**。
- 复现条件：Windows + CGO，包内任何文件（含 `_test.go`）直接引用 sherpa_onnx；
  运行时缺 `sherpa-onnx-c-api.dll` / `sherpa-onnx-cxx-api.dll` / `onnxruntime.dll`。
  实测最小复现：
  ```bash
  mkdir -p zz && cat > zz/main.go <<'EOF'
  package main
  import ("fmt"; sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx")
  func main() { ex := sherpa.NewSpeakerEmbeddingExtractor(&sherpa.SpeakerEmbeddingExtractorConfig{Model: "x.onnx"}); fmt.Println(ex == nil) }
  EOF
  go run ./zz    # → exit status 0xc0000135
  ```
- 根因：cgo 把 3 个 DLL 绑在**整个二进制**上（含测试二进制）。`go test` 对
  「有测试文件的包」一定会链接并执行测试二进制，因此 DLL 缺失时连 `-run` 过滤都无用。
  注意 `go test` 对「无测试文件的包」只编译不链接（输出 `? pkg [no test files]`），
  所以缺陷只在**有测试的包**里暴露。
- 临时处置：拆包（与 `internal/asr/local` 完全同构）：
  - `internal/speaker`：纯 Go，只有 `Extractor` 接口 + 聚类 / 归并 / 角色 / 编排；
  - `internal/speaker/sherpa`：`//go:build windows && cgo` 的 `sherpa_windows.go`（真实实现）
    与 `//go:build !windows || !cgo` 的 `sherpa_stub.go`（返回 `ERR_LOCAL_UNAVAILABLE`）。
  子包没有测试文件，因此 `go test ./internal/speaker/...` 只编译不链接它，测试可正常跑。
- 状态：已绕过（架构上已固化，后续不要「为了方便」把 cgo 代码搬回主包）。

## S7-02 sherpa 声纹 API 与记忆中的签名不一致（必须先查源码）

- 现象：按直觉写 `SpeakerEmbeddingExtractorConfig{Model, NumThreads, ...}` 与
  `extractor.Compute(samples)` 会编译不过。
- 复现条件：`grep -n "SpeakerEmbedding" $HOME/go/pkg/mod/github.com/k2-fsa/sherpa-onnx-go@v1.13.8/sherpa_onnx/*.go`
- 根因（v1.13.8 真实 API，与本文档对齐后使用）：
  - 配置结构只有 4 个字段：`SpeakerEmbeddingExtractorConfig{ Model string; NumThreads int; Debug int; Provider string }`；
    没有 `SampleRate` / `FeatureDim` / 语言字段 —— 采样率由**喂样本时**决定。
  - 提取流程是三段式：`CreateStream()` → `stream.AcceptWaveform(sampleRate, samples)` →
    `IsReady(stream)` → `Compute(stream)`；`Compute` 返回 `[]float32`，维度由 `extractor.Dim()` 给出。
  - stream 是 `OnlineStream` 类型，释放要用 `DeleteOnlineStream`（不是专门的
    `DeleteSpeakerEmbeddingStream`），否则内存泄漏。
  - **`AcceptWaveform` 内部直接取 `&samples[0]`**，传空切片会 panic（不是返回错误）。
- 临时处置：`sherpa_windows.go` 里按真实签名实现；`Embed` 入口先判
  `len(samples) == 0` 与 `sampleRate != 16000`，把 panic 变成可读错误码；
  每次 `Embed` 内 `CreateStream` + `defer DeleteOnlineStream`，并用 `mu` 串行化
  （extractor 与 stream 都不是并发安全的）。
- 状态：已解决。

## S7-03 本机没有声纹模型包时，整条链路必须诚实降级

- 现象：`NewSherpaExtractor` 在目录里找不到 `.onnx` 时若放任返回 nil extractor，
  上层 `Run` 会 nil-deref panic；若返回空向量则会把所有片段聚成 1 个人。
- 复现条件：本机未安装任何模型包（参见 `docs/DEV-ISSUES-D.md` ENV-10），
  `LocateBundle("speaker")` 失败或目录里没有声纹模型。
- 根因：声纹模型与转写模型是两条独立模型链，装了转写模型不代表有声纹模型；
  `speaker` 角色的包可能压根没下载。
- 临时处置：三层防御
  1. 定位器失败 → 原样返回 `ERR_MODEL_NOT_FOUND`（装配层若还要「先构造再降级」可用 `LocateAndBuild`）；
  2. 目录存在但认不出声纹模型 → `ERR_MODEL_NOT_FOUND`（无 `.onnx`）或 `ERR_MODEL_CORRUPT`（多个 `.onnx` 却认不出）；
  3. `Repo.extractor == nil` → `Run` 立刻返回 `ERR_LOCAL_UNAVAILABLE`（`action=download-model`），
     **不 panic、不写入任何行、不自动下载**；纯 DB 操作（改名 / 合并 / 拆分 / 指派）完全不受影响。
- 状态：已解决（这是设计意图，`TestRunWithoutExtractorDegrades` 守护）。

## S7-04 `*apierr.Err` 的 nil 装进 error 接口后不等于 nil（真实缺陷）

- 现象：分离流程本该成功，却返回一个**消息为空**的错误（`分离失败: `），
  且日志里没有任何失败线索。
- 复现条件：任何调用 `recomputeSpeakerCountersTx` 之类的辅助函数的成功路径。
- 根因：辅助函数签名是 `func wrapWrite(err error) *apierr.Err { return apierr.Wrap(err, code) }`。
  `apierr.Wrap(nil, code)` 返回 `(*apierr.Err)(nil)`，把它 `return` 到声明为 `error` 的位置时，
  得到的是**非 nil 的接口**（typed-nil），于是 `if err != nil` 成立，
  而 `(*apierr.Err)(nil).Error()` 返回空串 —— 表现为「成功流程报了一个空错误」。
- 临时处置：`wrapWrite` 改为返回 `error` 并在入口判 nil：
  ```go
  func wrapWrite(err error) error {
      if err == nil { return nil }
      return apierr.Wrap(err, apierr.CodeDBWrite)
  }
  ```
- 状态：已解决。**教训**：本仓库里任何「把 `*apierr.Err` 作为返回值交给 `error` 变量」的
  辅助函数都可能踩这个坑，返回值类型直接声明为 `error` 更安全。

## S7-05 拆分出来的说话人没有声纹向量（片段级向量不落库的必然代价）

- 现象：`SplitSpeaker` 拆出的新说话人 `dim=0`、`embedding_enc` 为空，
  下次对该会话重跑分离时，拆出去的片段又会被聚回原来那个人。
- 复现条件：拆分后重跑 `AnalyzeSession`。
- 根因：TBL-04 `segments` **没有 embedding 列**（刻意设计：声纹属于身份而不是句子，
  且落每句向量等于在库里留下可复原的声纹痕迹，与隐私边界冲突）。
  既然没有每句向量，就无法从原说话人的质心里「减掉」被拆走那部分，
  只能给新身份留空向量。
- 临时处置：
  - 新身份 `dim=0` / 空密文，`MatchGlobal` 对空向量条目直接跳过（不参与归并），
    所以它不会与任何人「误并」；
  - `MergeSpeakers` / `BlendCentroids` 对「一侧无向量」有显式分支（保留有向量那侧），
    手工合并回原身份不会把原身份质心抹掉；
  - 文档与代码注释都写明了「重跑会重新聚回，需要用户再拆一次」。
- 状态：改设计（接受此限制）。若要根治，需要给 segments 加加密 embedding 列，
  属于 DOC-04 变更，不在 S7 范围内。

## S7-06 幂等重跑会把角色翻倍（先删映射再查角色的顺序缺陷）

- 现象：同一会话跑两次分离，`roles` 表从 3 行变 5 行，每次重跑都新建一批「说话人 N」。
- 复现条件：`TestRunIdempotentCut`（单会话场景最明显）。
- 根因：重跑为了幂等先 `DELETE FROM session_speakers WHERE session_id=?`，
  之后的角色解析 `roleOfSpeakerTx(speakerID)` 只能查到**其它会话**的映射；
  单会话场景下必然查不到 → 走进「新建角色」分支。
- 临时处置：在删除**之前**快照本会话既有的 `(speaker → role)` 映射，角色解析顺序改为：
  本会话旧映射 → 其它会话映射 → 新建。这同时保住了「用户手工指派过的角色不被重跑抹掉」。
- 状态：已解决（`TestRunIdempotentCut` 守护 role / speaker / 映射行都不翻倍）。

## S7-07 `GOOS=linux` + `CGO_ENABLED=1` 在本机无法交叉编译（与本域无关的环境限制）

- 现象：`GOOS=linux CGO_ENABLED=1 go build ./internal/speaker/...` 失败：
  ```
  # runtime/cgo
  gcc_setenv.c: error: implicit declaration of function 'setenv'
  gcc_mmap.c: fatal error: sys/mman.h: No such file or directory
  ```
- 复现条件：Windows 上用 `CGO_ENABLED=1` 交叉编译到 linux。
- 根因：本机没有 linux 交叉 C 工具链，Go 拿 Windows 的 gcc 去编译 linux 的 `runtime/cgo`。
  **与 S7 代码无关**：既有包同样失败，例如
  `GOOS=linux CGO_ENABLED=1 go build ./internal/sessions/` 报一模一样的错。
  这也符合 `ADR-01`「不做单机交叉编译」的既定决策。
- 临时处置：跨平台**编译检查**改用 `CGO_ENABLED=0`（这正是验证「非 Windows 桩」的路径）：
  ```bash
  GOOS=darwin CGO_ENABLED=0 go build ./internal/speaker/...   # DARWIN_OK
  GOOS=linux  CGO_ENABLED=0 go build ./internal/speaker/...   # LINUX_OK
  ```
  并用 `go list -f '{{.GoFiles}}'` 确认选中的文件确实是 `sherpa_stub.go`。
  `GOOS=darwin CGO_ENABLED=1` 可以通过（stub 里没有 cgo，darwin 也不触发 runtime/cgo 重编）。
- 状态：已绕过。

## S7-08 内置「未识别」角色不允许改名 / 改配色

- 现象：`RenameRole("rl_unknown", "别名")` 若放行，侧栏里就再也看不出「未识别」这个标识。
- 复现条件：调用 API-7.2 / API-7.7 指向 `rl_unknown`。
- 根因：`rl_unknown` 的名字与配色是「未识别」语义的一部分（TBL-07 `is_system=1`），
  它是片段没有身份时的兜底展示；改名/改色会破坏这个约定。
- 临时处置：`RenameRole` / `SetRoleColor` 对 `IsSystem` 角色返回
  `ERR_INVALID_ARGUMENT`（Detail 说明原因），前端应据此禁用编辑入口。
- 状态：改设计（明确约束，写入代码注释与测试）。

## S7-09 `speakersvc.SetMaxSpeakers` 直写 `app_settings` 与「appsettings 唯一写入口」约定冲突

- 现象：`max_speakers` 属于 `app_settings` 表，而该表由 `internal/services/appsettings`
  声明为「唯一读写入口」；但 API-7.6 要求 `speakersvc` 提供该方法。
- 复现条件：`speakersvc` 实现 API-7.6。
- 根因：需求把「上限」放在说话人域暴露（用户是在角色侧栏里调），而存储放在设置表。
- 临时处置：`speakersvc` 只做**单列窄更新**
  （`UPDATE app_settings SET max_speakers=? WHERE id=1`），
  不读不写其它字段、不复制校验逻辑（2–8 / 0 的校验在 `speakersvc` 内完成，
  与 `appsettings` 已有的校验规则保持一致）。变更不广播 `appsettings:changed`
  （该事件的载荷语义是「字段名」，历史例外不扩散）。
- 状态：已绕过。若后续要统一，应把该写入收敛到 `appsettingssvc.SetMaxSpeakers`，
  由 `speakersvc` 转发调用 —— 需要改 `appsettings` 契约，不在 S7 范围。

## S7-10 WAV 片段读取在 speaker 域内重新实现了一份

- 现象：`internal/transcribe/wav.go` 已有完整的 WAV 解析，但 `openWAV` / `wavSource`
  都是**包内私有**，且它按「整段续读」设计；`internal/speaker` 需要的是
  「按片段毫秒区间随机取一段」。
- 复现条件：`internal/speaker` 想复用 `transcribe` 的音频读取。
- 根因：跨域复用要么导出实现（要改 transcribe，S7 明确禁止），
  要么依赖 `internal/recording`（那是 S1 的域，transcribe 自己也刻意不 import）。
- 临时处置：`internal/speaker/audio.go` 自带最小实现：RIFF/WAVE 块扫描、
  PCM16 / float32、单声道或立体声下混，采样率不是 16k 直接报错（不做重采样）。
  与 `transcribe` 的实现刻意保持同一套块扫描规则（含 `LIST` 等附加块跳读），
  避免两份解析器对同一文件给出不同结论。
- 状态：已绕过（重复实现约 200 行，属于可接受的域内自洽代价）。

## S7-11 会话内聚类算法取舍：单遍增量 + 阈值，而非层次聚类

- 现象：ARCH §4.4 写的是「片段级 embedding 做层次聚类」，实现却用了单遍增量聚类。
- 复现条件：`ClusterVectors`。
- 根因：层次聚类（agglomerative）的**合并顺序**会影响最终划分与编号，
  而本域的首要要求是**确定性**：同一输入必须每次得到同一份编号
  （用户给角色命名后重跑，编号漂移会把名字挂到别人身上）。
  单遍增量法按 `(Seq, SegmentID)` 稳定排序后逐条处理，
  簇编号按首次出现顺序分配，天然确定，且复杂度 O(n·k·d) 远低于层次聚类的 O(n³)。
- 临时处置：采用单遍增量 + 余弦阈值（0.72，闭区间）+ 质心加权更新；
  质心用 `float64` 累加和（float32 累积误差足以让阈值附近的判定抖动）。
  已知弱点（「第一句噪声固化成簇心」）由 pipeline 侧 1.2s 过短片段门槛缓解。
- 状态：改设计（与 ARCH 的描述差异需在 ARCH §4.4 同步措辞）。

## S7-12 合并说话人时「只遍历 session_speakers 行」会漏改片段

- 现象：`MergeSpeakers` 初版按 `session_speakers` 行逐条改片段，结果**另一场会话里
  该说话人的片段没有被改指向**（那场会话没有对应的映射行）。
- 复现条件：某会话的片段引用了 `sp_b`，但库里没有 `(session, sp_b)` 的映射行
  （上一次分离被打断 / 用户手工改过库）。
- 根因：片段与映射之间没有数据库级外键，二者可以不一致；把「映射行」当成片段归属的
  唯一真源是错的，真源在 `segments.speaker_id`。
- 临时处置：
  1. 片段改指向用**按 `speaker_id` 全量更新**（一条 SQL 覆盖所有会话）；
  2. 受影响会话 = 「有映射行的会话」∪「有该身份片段的会话」；
  3. 逐会话按片段实际情况重建 `to` 的映射行（`OnConflict` 只刷新计数，
     **不覆盖 `role_id`**，避免抹掉用户在该会话里的单独指派）；
  4. 最后统一重算 `sessions.speaker_count` 与 `speakers` 冗余计数。
- 状态：已解决。

## S7-13 GORM 关联与 upsert 的两个注意点

- 现象 / 注意点：
  1. `storage.SessionSpeaker` 上有 `Session` / `Speaker` 两个 `foreignKey` 关联，
     用 `tx.Create(&storage.SessionSpeaker{...})` 时**不要**给这两个字段赋值，
     GORM 允许 nil 关联（赋值会连带级联保存，产生意料外的 upsert）；
  2. upsert 必须显式写 `clause.OnConflict{Columns: []clause.Column{{Name:"session_id"},{Name:"speaker_id"}}}`，
     依赖 AUTOINCREMENT 主键是无效的（唯一键在业务列上）；
  3. `Distinct("speaker_id").Count()` 生成的 SQL 是 `COUNT(DISTINCT speaker_id)`，
     加 `Where` 时注意别把 `Distinct` 覆盖掉。
- 临时处置：三条都按上面写法落地（见 `roles.go` / `pipeline.go`）。
- 状态：已解决。

## S7-14 roles JOIN 的语义边界：一个 speaker 可能映射到多个角色

- 现象：`ListRoles` 里 `rl_1.SpeakerIDs` 可能有两个身份，同时同一个 `sp_x`
  也可能在不同会话里挂到不同角色上（用户手工指派的结果）。
- 复现条件：用户在不同会话里把同一身份指派给不同角色。
- 根因：TBL-06 的唯一键是 `(session_id, speaker_id)`，`role_id` 是**会话内**映射；
  而「角色是全局标签」意味着展示时是 `segments → session_speakers → roles`。
  两者并不矛盾：身份全局唯一，角色映射按会话存，展示按会话 JOIN。
  副作用是 `roleOfSpeakerTx`（取「该身份的默认角色」）在有多个答案时需要一个确定性规则。
- 临时处置：`roleOfSpeakerTx` 取 `id` 最小的那一行（确定性优先），
  并在注释里写明「想统一请调用 `AssignSpeaker`」。
  改名场景不受影响：改名只动 `roles.name`，所有 JOIN 结果立刻同步。
- 状态：已解决（语义已明确，不再视为缺陷）。
