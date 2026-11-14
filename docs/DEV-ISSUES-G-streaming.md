# Wave G-1：真实流式引擎实现（边录边转解锁）

> 状态：**已闭环**（真实模型实测通过；FFD 结论见 §6，未全部达标，原因已定位）
> 范围：`internal/asr/local`（流式引擎与工厂）、`internal/asr/delta.go`（增量裁剪）、真机验收
> 关联：`FR-03-10`、`NFR-01`、ARCH §2.3 两遍转写、ARCH §4.2 引擎矩阵、ARCH §7.3 FFD/SLD 测量定义
> 前置：`docs/DEV-ISSUES-F-live.md`（FL-04：缺真实流式引擎，真机恒为 `ERR_LOCAL_UNAVAILABLE`）

## 1. 本次交付

| 文件 | 内容 |
|------|------|
| `internal/asr/local/streaming_windows.go` | `StreamingEngine`：`sherpa_onnx.OnlineRecognizer` 封装（`Warm` / `Accept` / `Reset` / `Close` / `Health` / `ID`）＋ `NewStreaming` ＋ `NewStreamingFactoryWithThreads` |
| `internal/asr/local/streaming_stub.go` | 非 Windows / 无 CGO / `nollm` 构建的桩：`NewStreaming` 与工厂恒返回 `ERR_LOCAL_UNAVAILABLE` |
| `internal/asr/delta.go` | `StreamingDelta`：把「全量文本」裁成 `Accept` 需要的「增量文本」（纯 Go，引擎无关） |
| `internal/asr/delta_test.go` | 增量裁剪的单测（单调增长 / 改写 / 缩短改写 / Reset / rune 边界） |
| `internal/asr/local/real_streaming_test.go` | `-tags mandaryn_realmodel` 真机验证：Warm＋累积出字、离线对照、Reset、FFD/SLD 20 次采样、块长敏感度 |

**没有改动**任何既有文件：`internal/asr/streaming.go`（接口与 `LazyStreaming`）、`internal/transcribe/live.go`、
`internal/recording/live.go`、`internal/app/**` 全部保持原样 —— 接线片段见 §7，由负责装配的一方落地。

## 2. 增量文本怎么裁（关键取舍一）

`sherpa_onnx.OnlineRecognizer.GetResult` 返回的是「自上次 Reset 以来的**全量**文本」，
而 `asr.StreamingEngine.Accept` 的契约是「本次**新增**」。换算放在 `asr.StreamingDelta`：

1. 记住已发出的前缀 `emitted`；
2. **单调增长**（`strings.HasPrefix(full, emitted)`）→ 返回尾巴，更新 `emitted`。这是常态；
3. **改写**（模型改动了已发出的字）→ 取两者**最长公共前缀**（按 rune），把差异部分当新增返回，
   `Revisions()` 计数 +1，`emitted` 前进到新全量；
4. 全量文本为空串时**不**清 `emitted`（流式模型常常先给空再给内容，清了就整句重复）。

**增量接口无法撤回**，所以改写只能退化成「多字但不丢字」：定稿里可能留下被改写前的旧字。
这条假设在真机上成立得非常好 —— 实测整段 5.59s 中文音频 `Revisions() == 0`（见 §5）。

`CommonPrefixBytes` **必须按 rune 取长度**：中文一字 3 字节，按字节比会截在半个汉字上，
切出非法 UTF-8，前端显示成「�」。`delta_test.go` 专门钉了这条边界。

## 3. 线程安全与并发 Close（关键取舍二）

三层防护，各自解决不同问题：

| 层 | 机制 | 解决什么 |
|----|------|----------|
| 语义层 | `StreamingEngine` 约定单一消费者 | 解码器有内部状态，不能多路并发喂 |
| 上层 | `asr.LazyStreaming.callMu` | 上一个会话收尾 flush 与下一个会话开头重叠时串行化 |
| 实现层 | `StreamingEngine.mu` ＋ `closed` 标志 | **`Close` 与 `Accept` 真并发时不进悬垂指针** |

实现层的做法：`Accept` 全程持锁；`Close` 也持锁、先摘 `recognizer` / `stream` 指针再释放 C 资源。
于是并发的 `Accept` 要么在释放前跑完，要么看到 `closed` 直接返回 `ERR_ENGINE_NOT_READY`。
`Accept` 拿到空切片时**直接返回**（`OnlineStream.AcceptWaveform` 内部取 `&samples[0]`，空切片会 CGO 越界 panic）。

`Reset` 选「换一条新 stream」而不是 `recognizer.Reset(stream)`：多清掉特征缓冲里尚未解码的尾巴，
上一会话的残余音频不会漏进下一会话首句。

## 4. 平台与构建

- 真实实现：`//go:build windows && cgo && !nollm`
- 桩：`//go:build nollm || !windows || !cgo`
- 已校验：`go build ./internal/asr/...`、`go build -tags nollm ./internal/asr/...`、
  `GOSUMDB` 无关的 `CGO_ENABLED=0 GOOS=linux|darwin go build ./internal/asr/...` 全过。
- **本机无法交叉编译带 CGO 的 linux 目标**（宿主 gcc 缺 linux 头文件 `grp.h` / `sys/mman.h`），
  这是环境限制，与本次改动无关；产物语义由无 CGO 桩构建覆盖。

**不冒充**：流式引擎只认 `BundleKindTransducer`（三件套），
单文件模型（SenseVoice / Paraformer）在 `Warm` 阶段就报 `ERR_MODEL_CORRUPT` 并说明原因 ——
不会构造一个「能加载但永不出字」的 recognizer。模型包缺失时工厂**调用时**返回
`ERR_MODEL_NOT_FOUND`，绝不返回 nil 接口。

## 5. 真机实测（官方真实权重）

**模型**：`sherpa-onnx-streaming-zipformer-bilingual-zh-en-2023-02-20`
（经 `hf-mirror.com` 下载；`encoder-epoch-99-avg-1.int8.onnx` 181.9MB / `decoder-epoch-99-avg-1.onnx` 13.9MB /
`joiner-epoch-99-avg-1.int8.onnx` 3.2MB / `tokens.txt` 56KB）
**布局**：`<tmp>/zipformer-zh-streaming@1.0.3/`（`DetectBundle` 认成 `transducer`）
**样本**：`zh.wav`（16k / 单声道 / 16bit，89472 采样点 = 5.59s，前导静音实测 690–700ms，见 §6.4）
**机器**：本机，不设线程上限时按档位取 4 线程（对齐 PRD「4 核 CPU、medium 档」）

### 5.1 加载

| 项 | 值 |
|----|-----|
| `Warm` 加载耗时 | **979 / 987 / 1009 / 1020 / 1035 ms**（多次运行稳定在 ~1.0s） |
| `Health.State` | `ready` |
| `engineId` | `zipformer-zh-streaming@1.0.3` |

### 5.2 累积出字与离线对照

```
增量条数=7  累计文本="开放时间早上九点至下午五点"
离线对照：流式="开放时间早上九点至下午五点"
离线对照：离线="开饭时间早上9点至下午5点。"
字符数 流式=13 离线=14 最佳子序列相似度=0.71
流式改写次数 Revisions=0
```

- 流式逐块（100ms/块）**稳定累积出非空增量**，7 条增量合成一句完整中文；
- 与离线 SenseVoice 结果**语义一致**（差异在「开放/开饭」这一处近音字 + 离线的 ITN 把「九点」写成「9点」并加标点）；
- 这正是 §2.3 两遍法要解决的差距：流式先出字，第二遍离线校正把近音字纠回来。

### 5.3 Reset

```
第一轮：首增量="开放时间" 累计="开放时间早上九点至下午五点"
第二轮：首增量="开放时间" 累计="开放时间早上九点至下午五点"
```

Reset 后重新喂同一段音频，两轮**逐字相同** —— 解码状态与增量前缀都清干净了，没有重复累积。

## 6. FFD / SLD 采样（20 次，PRD 口径）

采样方式：**实时节拍**喂音频（每 100ms 音频等 100ms 再送）。全速灌样本会让 FFD 退化成纯算力耗时，毫无意义。

| 指标 | P50 | P95 | PRD 阈值（本地，**v1.4.0 双指标口径**） | 判定 |
|------|-----|-----|------------------------------------------|------|
| **FFD-engine**（起点=首个非静音帧，实测 700ms） | **912–913ms** | **915–916ms** | P50 ≤ 1000 / P95 ≤ 1500 | **达标** |
| FFD-e2e（起点=用户触发，含前导静音） | 1612–1613ms | 1615–1616ms | **不设阈值，仅观测** | — |
| SLD（相邻非空增量间隔，出字节奏代理） | 302–303ms | 1300–1302ms | P95 ≤ 1500 | 达标 |
| 解码积压（已喂音频时长 − 已耗墙钟） | 16ms | 19–21ms | P95 ≤ 1500 | 达标 |

> 数值给的是**两次 20 次采样的区间**（2026-11-12 落地时与随后的复核重跑），差异 1–3ms 属正常
> 运行波动，不是口径差异。判定余量远大于波动幅度（引擎指标距阈值 ~85ms），结论不受影响。

> 口径演进见 §6.4：v1.4.0 前为单指标 `FFD`（起点=用户触发，P50 ≤ 900），下表历史数据保留原口径以便追溯。

| 指标（**旧口径，已废止**） | P50 | P95 | 旧阈值 | 判定 |
|------|-----|-----|--------|------|
| FFD（含样本前导静音） | 1612ms | 1614ms | P50 ≤ 900 / P95 ≤ 1500 | 不达标 |
| FFD（扣掉前导静音，720ms 估算） | 892ms | 894ms | P50 ≤ 900 | 口径废止时认定无效，见 §6.4 |

### 结论与原因分析（如实报告，未调参数凑数）

**按 v1.4.0 双指标口径，FFD-engine 达标**：P50 912–913ms / P95 915–916ms vs 阈值 1000/1500。

**注意达标来自口径拆分，不是整体变快**：若沿用旧单指标口径（起点=用户触发、含前导静音），
实测 `FFD` 仍为 P50 1612–1613ms / P95 1615–1616ms —— 即便按新阈值 1000ms 衡量也仍不达标，
与拆分前同值，**没有任何性能改善**。变的只是把源音频前导静音从引擎责任中剥离。

拆开看，`FFD-e2e` 1612–1613ms 由两段组成：**源音频前导静音 700ms（实测，见 §6.4）+ 引擎首字 912–913ms**。

1. **前导静音是源音频自带的**（实测 690–700ms），不是引擎慢。它是**用户感知**的一部分
   （用户点开始后等了 1.6s 才看到字），所以必须如实呈现 → 这就是 `FFD-e2e` 存在的理由；
   但它不是引擎的责任，因此**不纳入达标判据**。旧口径把两者混为一个数，导致指标实际在测
   「样本开头静音多长」（静音占 43%），引擎变快也压不动它。
2. **912–913ms 是模型结构的固有延迟**：`zipformer` 流式编码器按 chunk（约 320ms 音频）推进，
   首字要积累若干 chunk 才提交（≈ 2.8 个 chunk）。所以这个数字接近该模型的
   理论下限，靠调参压不下去。**注意：它并不由 chunk 大小单独决定 —— 换更小 chunk 的模型
   未必更快，已实测证伪，见 §6.3。**
3. **实现本身不是瓶颈**：解码积压 P95 = **19–21ms**（每 100ms 音频只花 ~16ms 解码，比实时快 6 倍），
   内存与线程都在预算内。也就是说首字延迟与「实现效率」无关，纯粹是「模型出字晚」。

**两个已排除的方向（勿重走）**：
- ~~换 chunk 更小的流式模型~~ —— **已实测否证，见 §6.3**。若要换模型，判据必须是
  「实测出字更早」，而不是「chunk 更小」或包名里的 ms 数。
- ~~把起点改成语音起始就能达标~~ —— 实测起点 690–700ms、净首字 912–922ms，
  **仍需阈值上调**才达标，只改起点不解决问题。见 §6.4。

**产品侧已决定（2026-11-12）**：采纳双指标 —— `FFD-engine`（P50 ≤ 1000ms，达标判据）
+ `FFD-e2e`（仅观测），并保留 `FR-03-10` 默认关。

**另外一条会影响真机 FFD 的实现细节**：`asr.LazyStreaming` 把加载推迟到推理 goroutine 的
**首次 `Accept`**（为避免阻塞采集）。因此**每个进程的第一次**边录边转，其首字延迟还要再加
**约 1.0s**（实测 `Warm` = 979–1035ms）。若要保住 `NFR-01`，装配层应在录制开始前显式预热
（`Router.Streaming()` 或 `LazyStreaming.Warm`），例如在用户打开「边录边转」开关时后台预热。

### 6.1 块长敏感度（`Accept` 一次送多长音频）

| 每次 `Accept` 块长 | FFD P50 | FFD P95 | 扣前导静音后 P50（按实测起点 690ms 折算） |
|--------------------|---------|---------|------------------------------------------|
| 20ms | 1690ms | 1692ms | 1000ms |
| 100ms | 1612ms | 1613ms | 922ms |
| 200ms | 1612ms | 1618ms | 922ms |

- **20ms 反而更差**（+78ms）：调用次数变多，每次 CGO + `IsReady` 的固定开销被放大；
- **100ms 与 200ms 无差异**：说明 `transcribe.LiveRunner.take()` 的 **200ms 上限不影响 FFD**，
  接线时不需要为 FFD 去改 `take()` 的分块大小。

### 6.2 与 ARCH §7.3 正式定义的两处差异（必须说明，别混用）

| 指标 | ARCH §7.3 正式定义 | 本文实测 |
|------|--------------------|----------|
| FFD | `t(首个 partial 渲染到前端)` − `t(用户触发开始)` | 引擎层 `t(首个非空增量返回)` − `t(开始送音频)`，**不含 IPC 与前端渲染**（下界） |
| SLD | `t(片段落库且 finalized)` − `t(VAD 判定句末帧)` | 引擎层「相邻非空增量的墙钟间隔」，反映**出字节奏**，不反映定稿延迟 |

本测试是被测引擎的进程内验证，没有 GUI、VAD 与数据库，因此这两项只能测到引擎那一段。
补的「解码积压」指标才是引擎侧对 SLD 预算的真实判据（积压不增长 ⇒ 正式 SLD 不会因引擎而爆表）。
正式 FFD/SLD 仍需在有前端与落库的整机上用 `metrics.json`（DEPLOYMENT §「发布清单」）复测。

### 6.3 「换小 chunk 模型」能否降低 FFD —— 实测证伪

为回答「方案二是否可行」，下载并实测了模型库中现成的多档 chunk 中文流式包
`sherpa-onnx-x-asr-160ms-streaming-zipformer-transducer-zh-en-int8-2026-06-05`
（来源 [Gilgamesh-J/X-ASR](https://github.com/Gilgamesh-J/X-ASR)，icefall 训练，sherpa-onnx 部署）。

**元数据核对**（直接读 ONNX `metadata_props`，非推测）：

| 字段 | X-ASR 160ms | 当前 zipformer-zh-streaming@1.0.3 |
|------|-------------|-----------------------------------|
| `model_type` | zipformer2 | zipformer |
| `decode_chunk_len` | **16**（=160ms） | **32**（=320ms） |
| `left_context_len` | **256,128,64,32,64,128** | 64,32,16,8,32 |
| `encoder_dims` | 192,256,512,768,512,256 | 384×5 |
| 词表 | 5000（BPE） | 5000 |

该家族四档与导出公式 `decode_chunk_len = chunk_size[0] × 2` 完全自洽
（训练配置 `--chunk-size "8,24,48,96,-1"` → 8→160ms / 24→480ms / 48→960ms / 96→1920ms）。

**实测对照**（同一测试二进制、同一 `zh.wav`、同一口径，基线用同二进制复跑重现 1611/891ms）：
下表「扣前导静音」列按当时的 720ms 估算折算，仅用于**同口径横向比较**；绝对值应以 §6.4 的
实测起点（690ms）重新折算，即各行统一再加约 30ms。

| 模型 | 内部 chunk | 喂块 | FFD P50 | 扣前导静音（720ms 估算口径） | 加载耗时 |
|------|-----------|------|---------|-----------|---------|
| 当前模型 | 320ms | 100ms | **1611ms** | **891ms** | 933ms |
| X-ASR 160ms | 160ms | 100ms | 1718ms | 998ms | **2481ms** |
| X-ASR 160ms | 160ms | 200ms | 1615ms | 895ms | — |
| X-ASR 160ms | 160ms | 20ms | 1754ms | 1034ms | — |

**结论：chunk 减半并未降低 FFD**。最优喂块（200ms）下为 895ms，与当前模型 891ms
持平（噪声级差异），且加载慢 2.7 倍。

**原因**：FFD 不由 `decode_chunk_len` 单独决定，而由「出第一个 token 需要消耗多少音频」决定：

| | chunk | 首字消耗的 chunk 数 |
|---|---|---|
| 当前模型 | 320ms | 891 ÷ 320 ≈ **2.8 个** |
| X-ASR | 160ms | 996 ÷ 160 ≈ **6.2 个** |

chunk 减半但所需 chunk 数翻倍以上，净效果相抵。机制线索是 `left_context_len` 长 4 倍
——该模型定位「offline-streaming unified」（一套权重兼顾离线精度与流式），为离线质量
保留了更长历史依赖，代价是流式首字并不更快。

**对方案选择的直接影响**：
- 「换小 chunk 模型」作为 FFD 解法**在本机实测无效**，不应再作为候选方案；
- 若仍要换模型，判据必须是**实测首字更早**，不能看 chunk 参数或包名；
- 未测：X-ASR 在本项目中文字音上的识别质量（未做 CER 对比）、480/960/1920ms 档
  （基于上述机制，它们只会更差）。

### 6.4 前导静音的实测修正（影响「改口径」方案是否成立）

§6 此前用「前导静音约 720ms」估算，据此得出「扣掉静音后 892ms、勉强达标」。
该数值**未经实测**，而它直接决定「按语音起始计时」这一改口径方案能否成立，故补测。

**方法**：对 `zh.wav` 逐 10ms 帧算能量（dBFS），以前 600ms 中位数作底噪，再按
「超过底噪 +N dB 且连续 M 帧」判定语音起点。

`zh.wav`：16k / 单声道 / 16bit，89472 采样点 = 5.592s，**底噪中位数 -67.0 dBFS**。

| 判定条件 | 语音起点 | 扣静音后 FFD P50 | 对 900 阈值 |
|---|---|---|---|
| +10dB 连续 5/10/20 帧 | 690ms | **922ms** | 不通过 |
| +12dB 连续 3/5/10/20 帧 | 690ms | **922ms** | 不通过 |
| +15dB 连续 3/5/10/20 帧 | 700ms | **912ms** | 不通过 |
| 仅「超阈值」不要求持续 | 30ms | 1582ms | 不通过 |

说明：30ms 那一档被样本开头的瞬态尖峰（-41.8 dB 单帧，随后立即回落到 -60 dB 底噪）
误判，因此**必须带持续时长约束**；加上约束后各合理阈值一致收敛到 690–700ms。

**结论（重要）**：
- 真实前导静音是 **690–700ms**，此前「720ms」偏大约 20–30ms；
- 于是扣静音后的引擎首字延迟是 **912–922ms，不是 892ms** —— **超过了 `P50 ≤ 900`**；
- 也就是说 **「把计时起点改成语音起始」并不能让 FFD 达标**，它只是把超阈幅度
  从 79%（1612 vs 900）缩小到 1.3–2.4%（912–922 vs 900），仍然是超阈。
- 若产品要采纳「按语音起始计时」，则阈值本身仍需上调（例如 900 → 950/1000），
  或改用 P95 口径，否则改口径后依然不通过。

## 7. app.go 接线片段（由装配方落地）

`internal/app/app.go` 里现有的 `asr.NewRouter(asr.RouterOptions{...})` 只加两个字段即可：

```go
asrRouter := asr.NewRouter(asr.RouterOptions{
    // 线程数按当前预算档位取（NFR-08）：只在**引擎加载时**生效，
    // 改档位后要等下次加载（热切换/重启），不是热生效。
    Local:       asrlocal.NewFactoryWithThreads(modelsSvc.Manager(), perfSvc.Threads),
    LoadThreads: perfSvc.Threads,

    // —— Wave G-1：边录边转的流式引擎（FR-03-10，默认关）——
    // 与 Local 同一套语义：按 streaming 角色取包、工厂被调用时读一次档位。
    // 流式包没装时 Router.Streaming 返回 ERR_MODEL_NOT_FOUND，
    // 前端据此显示「需下载流式模型」；**不会**退回离线引擎伪装流式。
    Streaming: asrlocal.NewStreamingFactoryWithThreads(modelsSvc.Manager(), perfSvc.Threads),
    // 便宜的可用性探测（录制页每次挂载都会问一次，不能加载模型）：
    // 只查「streaming 角色的包在不在」，LocateBundle 是纯查表。
    StreamingProbe: func() error {
        _, _, err := modelsSvc.Manager().LocateBundle(asr.RoleStreaming)
        return err
    },

    ActiveSession: activeSession,
})
```

装配侧还需注意两点（已在 §6 结论里说明，这里汇总）：

1. **首会话 FFD**：`LazyStreaming` 把加载推迟到首次 `Accept`，第一次边录边转首字要多等 ~1.0s。
   建议在用户打开「边录边转」开关时后台预热一次（`Router.Streaming()` 会同步加载并 `Warm`，
   或对 `LazyStreaming` 实例调 `Warm(context.Background())`）。
2. **`Refine` 第二遍**：流式引擎**不**做离线定稿（`Finalize` 明确返回 `ERR_LOCAL_UNAVAILABLE`），
   第二遍离线校正由 `transcriptsvc.NewLiveSink` 侧的 `LiveDeps.Refine` 注入 —— 该路径已在
   Wave F 落地，本次未改动。

## 8. 未闭环事项

| # | 事项 | 归属 |
|---|------|------|
| 1 | FFD 按 PRD 正式口径不达标（P50 1612ms vs 900ms）；需产品决定：接受 / 重定义口径。**「换小 chunk 模型」已于 §6.3 实测证伪，不再是候选** | 产品 + `NFR-01` |
| 2 | `internal/app` 接线（§7 片段）尚未落地；落地后需整机复测 FFD/SLD（含前端渲染与落库） | 装配方 |
| 3 | 「打开开关时预热流式引擎」尚未接线，首会话首字延迟 +~1.0s | 装配方 |
| 4 | 正式 FFD/SLD（含前端渲染、VAD 句末→落库）需整机 + `metrics.json` 复测 | 整机验收 |
| 5 | 真机验证脚本依赖人工下载模型（`MANDARYN_REAL_STREAM_DIR`），未纳入 CI | 后续 |

## 9. 复现步骤

```bash
export PATH="/j/msys2/ucrt64/bin:$PATH" GOCACHE="$HOME/AppData/Local/go-build" CGO_ENABLED=1

# 1) 常规验收（纯 Go，不加载模型）
go build ./internal/asr/... && go vet ./internal/asr/...
go test -count=1 ./internal/asr/...
go test -race -count=1 ./internal/asr/...

# 2) 真机：先编译再从 bin/ 运行（DLL 与 exe 同目录，否则 0xc0000135）
go test -tags sherpa_models -c -o bin/streambench.exe ./internal/asr/local/
cd bin
MANDARYN_REAL_STREAM_DIR="C:\...\zipformer-zh-streaming@1.0.3" \
MANDARYN_REAL_WAV="C:\...\zh.wav" \
MANDARYN_REAL_OFFLINE_DIR="C:\...\sensevoice-small@1.2.0" \
  ./streambench.exe -test.v -test.run 'TestRealStreaming'

# 3) 只跑 20 次 FFD/SLD 采样（约 100 秒）
MANDARYN_STREAM_BENCH_ROUNDS=20 ./streambench.exe -test.v -test.run TestRealStreamingLatency

# 4) 块长敏感度（约 25 秒，默认跳过）
MANDARYN_STREAM_BENCH_CHUNKS=1 ./streambench.exe -test.v -test.run TestRealStreamingChunkSensitivity

# 5) 对比不同模型（§6.3 的证伪实测）：把模型目录换成目标包的三件套目录即可
#    X-ASR 包解压后文件名是 encoder.int8.onnx / decoder.onnx / joiner.int8.onnx，
#    DetectBundle 按文件名含 encoder|decoder|joiner 识别，无需改代码。
MANDARYN_REAL_STREAM_DIR="C:\...\xasr160\bundle" \
MANDARYN_REAL_WAV="C:\...\zh.wav" \
MANDARYN_STREAM_BENCH_ROUNDS=20 \
  ./streambench.exe -test.v -test.run TestRealStreamingLatency
```

环境变量一览：

| 变量 | 必需 | 含义 |
|------|------|------|
| `MANDARYN_REAL_STREAM_DIR` | 是 | 流式模型包目录（三件套 + tokens.txt） |
| `MANDARYN_REAL_WAV` | 是 | 16k/单声道/16bit 中文语音样本 |
| `MANDARYN_REAL_OFFLINE_DIR` | 否 | 离线包目录，设了才做离线对照 |
| `MANDARYN_STREAM_BENCH_ROUNDS` | 否 | 采样轮数，默认 20 |
| `MANDARYN_STREAM_BENCH_VERBOSE` | 否 | 逐轮打印 FFD / 最大积压 |
| `MANDARYN_STREAM_BENCH_CHUNKS` | 否 | 打开块长敏感度用例 |
