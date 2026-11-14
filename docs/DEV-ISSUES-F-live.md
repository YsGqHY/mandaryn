# Wave F-2 开发问题记录：边录边转（`FR-03-10`，默认关闭）

> 本文件只记录 Wave F-2（`internal/asr` 流式能力 → `internal/recording` 音频旁路 →
> `internal/transcribe.LiveRunner` → `app_settings.live_transcribe` → 前端开关与实时文本区）
> 实施过程中遇到的问题与**按设计不做**的结论，按发现顺序编号。
> 需求口径以 [docs/PRD.md](./PRD.md) `FR-03-10` 与 [docs/ARCHITECTURE.md](./ARCHITECTURE.md) §6.4 为准；
> 设计说明同时写在代码顶部注释里（`internal/asr/streaming.go`、`internal/recording/live.go`、
> `internal/transcribe/live.go`），不在此重复。

## FL-01 既有 `asr.Engine.Feed` 是空实现，不能当流式能力用

- 现象：`internal/asr/engine.go` 的 `Feed` 在 local / online 两条实现里都只返回**已关闭的空 channel**，调用方永远拿不到 partial。若把边录边转建在 `Feed` 上，界面会「显示成功但一句字都不出」。
- 复现条件：对任一已 `Warm` 的本地引擎调 `Feed`，channel 立即关闭、零载荷（即 `DEV-ISSUES-E-s8.md` 的 `E8-01` 结论）。
- 根因：`Feed` 是为「延后转写的整句批量解码」预留的形状，sherpa 侧接的是 offline recognizer，没有增量解码器；而边录边转需要 online recognizer 的 `AcceptWaveform + GetResult` 增量语义。两者不是同一套 API。
- 临时处置：**另立显式可选能力** `asr.StreamingEngine`（`Engine` + `Accept` + `Reset`），只在真的实现了流式解码时实现；缺能力就明确报 `ERR_LOCAL_UNAVAILABLE` / `ERR_MODEL_NOT_FOUND`，绝不拿 `Feed` 冒充。`Router` 只在一处惰性持有流式实例（`streamState`），**不进入** local↔online 降级矩阵。
- 状态：已闭环（改设计，不是修 `Feed`）

## FL-02 `Router.Streaming()` 会同步加载模型，而它被调用的位置在采集已经打开之后 —— 会阻塞采集

- 现象：录制一开始，会话**开头几秒**的音频没有进 WAV；`recording:` 日志里「开始录制」与第一帧落盘之间有明显空档。这是本 Wave 最严重的一条，直接撞「推理绝不阻塞采集/落盘」。
- 复现条件：开启边录边转 → 首次录制（流式模型尚未加载）→ `recording.Start` 同步调用 `Options.NewLiveSink` → 里面 `Router.Streaming()` 构造 + `Warm`（本地流式模型数秒）。而 `OpenChannel(sourceID)` 在 `Start` 更早的位置就执行了（`internal/recording/recorder.go` 的 `ch, err := r.opts.OpenChannel(sourceID)`），此时采集管道已在产样本、`writeLoop` 还没启动（`startReader` 在其后）。
- 根因：`Streaming()` 的设计目标是「返回可用实例」，加载 + 预热都在方法内同步完成；而装配层把它放在了 `Start` 的同步路径上。`Start` 阻塞期间捕获样本只能堆在管道缓冲里，缓冲满了就丢——丢的正是**会话开头**那段。
- 临时处置：新增 `asr.LazyStreaming(resolve func() (StreamingEngine, error))`，把加载推迟到**推理 goroutine 第一次 `Accept`**；`transcriptsvc.NewLiveSink` 只做便宜探测（`StreamingAvailable()`，纯查表、不加载）再包一层惰性代理。这样：
  1. `Start` 立即返回，采集与落盘零延迟；
  2. 加载成本落在 `LiveRunner` 的推理 goroutine 里，期间音频按「队满丢最旧」处理；
  3. 加载失败不再让 `NewLiveSink` 报错，而是由 `LiveDeps.OnState` 报 `failed`（录制照常）。
- 状态：已闭环（`LazyStreaming` + `TestLazyStreamingDoesNotLoadOnResetOrClose` / `TestNewLiveSinkDoesNotLoadEngine` 守护）

## FL-03 流式解码器不是并发安全的，而两个会话真的可能重叠

- 现象：`-race` 下「8 个 goroutine 同时喂同一个解码器」直接报数据竞争（这是写 FL-02 的并发用例时撞出来的）。真实后果比竞争报告更糟：sherpa 的 online recognizer 是 CGO 对象，并发 `AcceptWaveform` 是崩溃级风险。
- 复现条件：`LiveRunner.Close()` 是**非阻塞**契约（它必须在 `Stop`/`finalizing` 路径上立即返回）→ 上一会话的推理 goroutine 可能还在收尾 flush（仍在 `Accept`）→ 用户此时立刻开始下一段录制 → `NewLiveSink` 建新 runner → 两条 `Accept` 循环撞进同一个流式实例。
- 根因：`Router` 复用**同一个**流式实例（这样才能只加载一次、几千 MB 的模型不重复驻留），而它的内部状态是有状态的单消费者对象。所以并发保护必须加在「复用点」上，不能指望调用方不并发。
- 临时处置：`LazyStreaming` 内加 `callMu`，把 `Accept` / `Reset` / `Warm` / `Finalize` / `Close` **串行化**；加载本身用 `sync.Mutex` 保证只发生一次且失败结果被记住（不反复重试）。代价是后一个会话的开头要等前一个 flush 让出（每片百毫秒量级），换来「任何时刻只有一个调用在引擎里」。
  另外 `Reset` 在未加载时**只打标记**（`needReset`），等真正加载后再补——否则「清状态」这件事会把模型提前拉起来，等于绕回 FL-02。
- 状态：已闭环（`TestLazyStreamingSerializesEngineCalls` 用**非并发安全**的替身钉住：漏锁时 `-race` 必报）

## FL-04 没有流式引擎实现与流式模型包：本机跑起来默认就是「不可用」

- 现象：功能全链路可用，但真机上边录边转**永远不会出字**，`LiveAvailability` 返回 `ERR_LOCAL_UNAVAILABLE`。
- 复现条件：不注入 `asr.RouterOptions.Streaming`（当前 `internal/app` 就是如此）→ `Streaming()` 报「本构建不含流式引擎」。
- 根因：流式解码需要 sherpa-onnx 的 **online recognizer** + `zipformer-zh-streaming` 角色模型包，两者都还没有：`internal/asr/local` 目前只封装 offline recognizer；`modelssvc` 的模型清单里也没有该角色。按 `ADR-01`，这属于「本地推理能力缺口」，**不允许**用离线引擎伪装流式来凑绿。
- 临时处置：把缺口做成**显式、可提示**的状态而不是静默失败——
  1. `StreamingFactory` 为 nil 时 `Router.Streaming` / `StreamingAvailable` 明确报 `ERR_LOCAL_UNAVAILABLE`；
  2. `contract.LiveAvailability` 把「开关开着但没能力」与「开关没开」区分开，前端据此提示「去模型页下载流式模型」；
  3. 录制完全不受影响（`NewLiveSink` 失败被装配层忽略，`live_transcribed` 仍按开关置位——它记录的是**用户意愿**，不是「这台机器跑成功过」）。
- 状态：待办（需 sherpa online recognizer 封装 + `RoleStreaming` 模型包；本轮不实现）

## FL-05 装配顺序是环：`recording.New` 早于 `transcriptsvc.New`，而 sink 需要前者

- 现象：`recording.Options.NewLiveSink` 的天然实现是 `transSvc.NewLiveSink`，但 `app.go` 里 `recorder` 建在 `transSvc` **之前**（因为 `transSvc.Deps.ActiveSession` 依赖 `recSvc`，而 `recSvc` 又依赖 `recorder`）。
- 复现条件：直接按理想顺序写会得到 `undefined: transSvc`；强行重排则要拆 `activeSession` 判定，风险远大于收益。
- 根因：这是一个真实的构造环：`settingsSvc`（需 `activeSession`）← `recSvc` ← `recorder`（需 `NewLiveSink`）← `transSvc`（需 `activeSession`）。Wails 的装配是纯构造函数链，没有两阶段注入设施。
- 临时处置：**晚绑定闭包**——`recording.Options.NewLiveSink` 与 `appsettings.WithActiveSession` 都传闭包，闭包里读一个在装配后段才赋值的变量；闭包对 `nil` 做防御（装配未完成时按「不可用 / 无活动会话」处理）。这样无需重排任何既有构造顺序，也不改任何包的签名。
- 状态：待接线（片段见下节与汇报；`internal/app/**` 由主 agent 落地）

## FL-06 录制中**两个方向**都拒绝切换开关

- 现象：录制中把 `live_transcribe` 从关改开、或从开改关，都返回 `ERR_STATE_FORBIDDEN`。
- 复现条件：`recordingsvc.Start` 之后调 `appsettings.SetLiveTranscribe`。
- 根因：开关只在 `recording.Start` 读一次（决定本会话是否建旁路 + 是否置 `live_transcribed`）。会话中途改值会出现两种都不好的结果：开启 → 前半段没字、后半段有字，用户看到的是「刚才还好的怎么没了」；关闭 → 旁路已建，`live_transcribed` 与库里事实不一致。
- 临时处置：两个方向一律拒绝，前端在开关旁提示「录制中不可修改，下一个会话生效」。这与模型来源切换的既有规则同构（不要在录制中途换引擎）。
- 状态：按设计如此（`internal/services/appsettings/live_test.go` 守护）

## FL-07 `live_transcribe` 放 `Snapshot` 顶层而不是 `AudioSettings`

- 现象：`contract.AudioSettings` 已冻结（多域共用、改了要同步多个文档与前端），边录边转不属于「音频参数」（采样率/声道/最大说话人数），它是**功能开关**。
- 复现条件：试图把字段塞进 `AudioSettings` → 触发跨域改动清单。
- 根因：`AudioSettings` 的语义是「捕获与分离的参数」，而 `live_transcribe` 是「录制期是否并发推理」的策略开关，语义不同层。
- 临时处置：`appsettings.Snapshot.LiveTranscribe` 顶层字段（与 `AutoTranscribe` / `OfflineMode` 同级）；`app_settings` 表新增 `live_transcribe` 列，默认 `false`，并在 `ensureSingletonSettings` 里**显式写 `false`**（GORM 零值不会覆盖已有行，靠 AutoMigrate 补列后旧行是 NULL，必须显式落默认值）。
- 状态：已闭环

## FL-08 `seq` 与延后转写同口径，靠「跳过自动整段转写」避免撞号与重复推理

- 现象：`segments` 的唯一键是 `(session_id, seq)`。边录边转的定稿与延后转写都往这张表写，若两条路径都用「从 0 开始数」，就会互相覆盖。
- 复现条件：开边录边转录完一段 → `Stop` 后自动整段转写又跑一遍 → 同一会话出现两套 `seq`。
- 根因：`seq` 是「会话内片段序号」，天然会有两种生产者。彻底解耦需要给片段加来源列（改动 `segments` 表结构与回放、导出等多个读取方），成本远超收益。
- 临时处置：
  1. `LiveRunner` **复用** 延后转写的 `UtteranceSplitter` / `VadConfig`（分句口径一致，`seq` 递增顺序才一致）；`LiveDeps.Vad` 的注释明确要求「必须与延后转写一致」；
  2. `transcribe.ShouldAutoTranscribe(row)`：`live_transcribed=true` 的会话**不**进空闲自动整段转写（已经产出定稿，再跑一遍等于推理两遍）；
  3. 用户仍可**手动重转**：这是显式动作，按 `(session_id, seq)` 幂等覆盖，结果仍自洽。
- 状态：已闭环（`TestShouldAutoTranscribeSkipsLive` 守护）；残余风险——极端情况下（边录边转中途失败、只有前半段定稿）用户手动重转会覆盖成整段结果，这是**期望行为**，不是缺陷。

## FL-09 前端 bindings 未重生成，`liveTranscribe` 只能按结构取值

- 现象：`Snapshot` 的 TS 类型里没有 `liveTranscribe`（`frontend/bindings/**` 本轮禁改，且它由 `wails3 generate bindings -ts` 生成）。
- 复现条件：`pnpm typecheck` 对 `snap.liveTranscribe` 报「属性不存在」。
- 根因：绑定是生成物，字段新增后必须重跑生成命令；本轮不允许改该目录。
- 临时处置：`AppSettingsService` 用结构化取值兜底（`const extended = snap as unknown as { liveTranscribe?: boolean }`，缺省 `false`），并加注释说明「绑定重新生成后可去掉这层」。`LiveAvailability` 同理手写接口（`internal/contract/live.go` 不在生成的 models 里）。
- 状态：待办（主 agent 重生成绑定后可清理这两处兜底）

## FL-10 前端 `pnpm build` 当前会失败在别人的文件上（与本源无关）

- 现象：`pnpm build` = `tsc -b && vite build`，`tsc` 阶段报两个错误：`src/pages/LibraryPage/playback/usePagedTimeline.ts(211,18)` 与 `(212,18)` 的 `markers` / `durationMs` 不存在于 `TimelinePage`。
- 复现条件：直接 `cd frontend && pnpm build`。
- 根因：并行开发的 Wave F-3（分页回放）需要后端 `TimelinePage` 增加 `markers` / `durationMs`，见 [docs/DEV-ISSUES-F-paged.md](./DEV-ISSUES-F-paged.md) §1；该改动尚未落地。
- 临时处置：本源的验收用 `npx vite build --mode production` 单独验证打包（能过），`npx tsc -b --noEmit` 只报上述两条他域错误、**零条**来自本轮文件。按分工不修别人包。
- 状态：外部阻塞（等 F-3 的后端字段落地）

## FL-11 边录边转不写 `transcription_status` 进度，字数走 tick

- 现象：实时转写过程中库里 `transcription_status` 不逐句更新；录制页的字数来自 `recording:tick` 的 `CharCount`。
- 复现条件：边录边转录制中查库 / 看 `RecordingTick` 载荷。
- 根因：`contract.RecordingTick.CharCount` 已存在（注释即「仅边录边转时有意义」），这是既有通道；而逐句写 `transcription_status` 会让 SQLite 在录音主路径上被高频写，风险与收益不成比例。并且转写状态是**独立轴**，不能借录制 tick 表达转写状态机。
- 临时处置：`Recorder.LiveCharCount()` 由 tick 带出（在内存里累加，不查库）；`transcription_status` 只在**结束**时按库里实际片段数一次性回填（正常 → `done`，失败 → `failed`，ARCH §6.4「边录边转正常结束也置 done」）。
- 状态：按设计如此

## FL-12 停止时 `Flush` 的口径：够长就定稿，否则丢弃

- 现象：`Stop` 时不足一个最短句长的尾巴不会变成片段。
- 复现条件：录 1 秒就停（开着边录边转）。
- 根因：与延后转写保持一致——`UtteranceSplitter` 有最小句长（`VadConfig.MinUtteranceMs`），短于此的语音被视作噪声；若为「收尾尽量多出字」而放宽，边录边转与延后转写就会产出不同的分句口径，`seq` 也随之错位（见 FL-08）。
- 临时处置：`LiveRunner.finish()` 用**同一**阈值判断「够长就定稿」，否则丢弃并计数；`Stop` 不等待 flush 完成（非阻塞 `Close`，`Wait(ctx)` 仅供测试与退出时使用）。
- 状态：按设计如此

## FL-13 未做的验证：真机时延 / CPU 占用 / 预算联动

- 现象：本轮的边录边转用例全部是**替身驱动**（假引擎 + 假音频），没有一条在真流式模型上跑过。
- 复现条件：没有流式引擎实现（见 FL-04），无法验证「端到端延迟多少毫秒出字」「推理 goroutine 吃掉多少 CPU」。
- 根因：能力缺口在上游（无 online recognizer 封装、无流式模型包），不是本轮能补的。
- 临时处置：
  1. 预算联动只接了**一道闸**：`LiveDeps.RefineAllowed`（第二遍离线校正在 L1 及以上关闭，复用 `perf.RefinementAllowed`），边录边转的**流式解码本身**不因档位降速——因为流式解码是「延迟敏感」的，降速只会让字更晚出来、丢得更多，与 `NFR-08`「降档不中断录制」的意图相反；
  2. 队列容量按「约 8 秒音频」定，队满丢最旧并计数（`LiveStats.DroppedSamples`），真机上若发现丢得多，先看这个计数再谈调参；
  3. 没有为了「看起来有预算联动」而给流式路径加跳帧——见 `DEV-ISSUES-E-s8.md` `E8-02` 的同一结论。
- 状态：待办（依赖 FL-04）

## 附：`internal/app` 接线片段（由主 agent 落地）

见汇报正文，要点三条：

1. `recording.Options` 增 `LiveEnabled` / `NewLiveSink` 两个闭包，`NewLiveSink` 通过**晚绑定变量**在 `transSvc` 构造后再赋值（FL-05）；
2. `appsettings.New(...)` 追加 `appsettings.WithActiveSession(...)`，同样晚绑定（FL-05）；
3. `transcriptsvc.Deps.Live` 填 `OnPartial` / `OnSegment`（转发 EV-04 / EV-05）、`OnState`（仅日志）、`RefineAllowed`（接 `perfSvc.RefinementAllowed`）。
