# S4 开发问题记录（延后转写任务域）

> 范围：`FR-03-4` / `FR-03-10` 的**延后转写**部分与 `API §2.4 transcriptsvc` 时间轴。
> 新代码：`internal/transcribe`（L2 领域层）、`internal/services/transcriptsvc`（L3 薄壳）。
> 记录原则：**只记真实踩到的问题**，含临时处置与最终状态；不把「设计说明」写成问题。

---

## S4-01 错误码表缺「音频不可用」类码

- 现象：`docs/API.md` §5 没有音频相关的 `ERR_*`（只有会话/引擎/模型/下载/迁移/性能/路径）。
  转写任务会遇到两类音频问题：文件缺失、文件损坏或采样格式不支持。
- 复现条件：让 `sessions.audio_path` 指向不存在的文件，或指向一个非 WAV / 非 16bit 的文件，
  再调 `StartTranscription`。
- 根因：错误码表定义时转写路径尚未落地（`transcript_status` 与 `transcriptsvc` 都是 v1.3.0 才加的），
  音频侧的失败形态没有被编号。
- 临时处置：
  - 文件缺失 / 不可读 → `ERR_NOT_FOUND`（并把 `action` 覆盖为 `re-record`，detail 只写**相对**路径）；
  - 容器或采样格式不支持、头损坏 → `ERR_INVALID_ARGUMENT`（不可重试，detail 给具体原因）。
  两者都不会删除或改写音频，任务只置 `failed` 并带错误码，用户可重录或修复后重试。
- 状态：未解决（等错误码表补 `ERR_AUDIO_MISSING` / `ERR_AUDIO_CORRUPT` 这类专属码后替换）

---

## S4-02 任务书里的 `canceled` 状态与已冻结的五值集合冲突

- 现象：任务书要求转写状态轴为 `none → queued → running → done / failed / canceled`；
  但 `docs/API.md` API-4.6 明确「取消后状态回 `none`」，`docs/DATA-MODEL.md` TBL-03 与
  `internal/contract/recording.go` 的 `TranscriptionXxx` 常量都只冻结了五个值
  （`none/queued/running/done/failed`），且 `contract/**` 本期禁止修改。
- 复现条件：读任务书状态清单与 `contract.TranscriptionCanceled`（后者不存在）。
- 根因：两处口径不同步。`canceled` 若落库，会同时破坏「DB 列取值集合」与「契约常量」，
  且让「取消后能否重新发起」失去统一判据。
- 临时处置：**以 API / TBL-03 为准**——取消落库回 `none`（已转句子保留，进度保留）。
  `canceled` 只作为**运行期结论**存在：`transcribe.OutcomeCanceled`（不落库）。
  同理「被抢占」是 `OutcomeYielded`，落库回 `queued`。
- 状态：改设计（按既有文档收敛；`state.go` 顶部注释与 `Outcome*` 常量记录了这层区分）

---

## S4-03 缺「转写任务忙」错误码

- 现象：同一会话同时只允许一个转写任务（ARCH §6.4），第二个请求需要错误码。
  API-4.5 的可能错误列里没有专门的「忙」码，只有 `ERR_STATE_FORBIDDEN` 语义最接近。
- 复现条件：任务运行中（或排队中）再次调 `StartTranscription`。
- 根因：错误码表里没有并发/占用类码；`ERR_MODEL_BUSY` 是模型库专用（下载/迁移），
  借给转写会让前端提示跑偏（会引导用户去等模型下载）。
- 临时处置：复用 `ERR_STATE_FORBIDDEN`，并把 `action` 覆盖为 `wait-transcription`，
  detail 写明「会话已有排队或运行中的转写任务」。前端据 action 显示「稍候」而不是「停止录制」
  （`ERR_STATE_FORBIDDEN` 的默认 action 是 `stop-recording`，与转写场景不符）。
- 状态：已绕过

---

## S4-04 `paths` 的默认层解析失败会连带否决 CLI / env 接管

- 现象：无 `%AppData%`（或 `$HOME`）的环境下，`paths.Resolver.Effective(dataRoot)` 直接返回
  `paths: cannot resolve user config dir: %AppData% is not defined`，
  即使调用方用 `Options.CLI` 明确指定了 `dataRoot` 也一样失败。
- 复现条件：清掉 `AppData` 环境变量后调用 `paths.New(Options{CLI: {dataRoot: 某临时目录}}).Effective(dataRoot)`。
  CI / 沙箱 / 服务账号环境常见。
- 根因：`resolver.resolve` 里 `def, err := r.defaultFor(key)` 在**选层之前**无条件执行并返回错误，
  于是「高优先级层可用、默认层不可用」这种完全正常的情形被判成整体失败。
  违反 ADR-02「cli > env > user > default 单向覆盖」——默认层只是兜底，不该能否决上层。
- 临时处置：`internal/paths` 属其它域的代码，本期不改；`transcriptsvc` 的单测通过
  `paths.Options.DefaultDataRoot/DefaultModelRoot` 注入可用默认值绕过。
  顺带说明：本仓 `transcriptsvc_test.go` 的夹具注释里指向本条。
- 状态：未解决（建议：`defaultFor` 失败时只把默认层从候选里剔除，若最终无层可选再报错）

---

## S4-05 「让位后回 `queued`」与「发起侧拒绝 `queued`」互相锁死

- 现象：实现抢占让位后，任务回到 `queued` 等待重跑；但执行侧准入当时复用了发起侧规则
  `CanStart`（`none/failed/done` 才允许），于是同一个任务被自己的准入检查拒绝，
  表现是任务在「排队 → 被拒 → 再排队」之间打转，永远不执行。写单测时立刻暴露。
- 复现条件：`idle()` 由 true 变 false 让任务让位，随后恢复 idle 让调度器重跑该任务。
- 根因：混淆了两种准入语义——「用户能不能再发起一个任务」与「调度器能不能执行这个任务」。
  前者要排除 `queued`（避免同会话两个任务），后者恰恰必须允许 `queued`（那就是排队的本义）。
- 临时处置：拆成两个函数：`CanStart`（发起侧，供 `Enqueue` 用）与
  `CanExecute`（执行侧，仅 `running` 拒绝，供 `Runner.Run` 用），
  并把真正防并发的能力交给内存 `SessionLock`。
- 状态：已解决（`state.go` 两个函数的注释写明差异；调度器让位/续跑单测覆盖）

---

## S4-06 取消是异步的，`CancelTranscription` 返回时状态可能还停在 `running`

- 现象：点取消后立刻查询，仍看到 `running`/`queued` 残影；极端情况下取消像「没生效」。
- 复现条件：任务正在跑时调 `CancelTranscription`，紧接着读状态。
- 根因：两层异步叠加：
  1. `Scheduler.Cancel` 只是 `cancel()` context，执行侧要到下一个**句间检查点**才收尾落库；
  2. 原先 `Scheduler.execute` 在 `pick` 之后、`runner.Run` 之前才登记 `cancel` 函数，
     存在「已置 `running` 但 `cancel == nil`」的空窗——正好落在这段时间里的取消会退化成空操作。
- 临时处置：
  - 在 `pick()` 的同一把锁内创建并登记 context/cancel，消除空窗；
  - `CancelTranscription` 在返回前调用 `Scheduler.WaitStopped(sessionID)`（上限 3s），
    让「取消返回」与「状态已落库」对齐；超时只告警不报错（执行侧终会收尾）。
- 状态：已解决（单测 `TestCancelTranscription`、`TestSchedulerCancelQueued` 覆盖）

---

## S4-07 取消 / 让位的落库不能跟着请求 context 一起取消

- 现象：取消瞬间落库报 `context canceled` / SQLite `interrupted (9)`，状态停在 `queued`，
  于是「取消」后既跑不下去、也发起不了（要再取消一次才能恢复）。
- 复现条件：任务执行中取消，紧接着看 `sessions.transcription_status`。
- 根因：转写任务的所有状态写入都用了调用方 ctx；ctx 一旦取消，收尾写入自己也失败。
  而收尾写入恰恰是**取消之后**必须完成的事。
- 临时处置：任务内区分两个 context：`ctx` 只用于「是否该停」的检查点判定；
  `writeCtx = context.WithoutCancel(ctx)` 用于全部账务写入（状态 / 进度 / 片段），
  取消动作用 `writeCtx` + 3s 超时。
- 状态：已解决（`execution.writeCtx` 字段注释写明理由）

---

## S4-08 「重转后句子数变少」会留下旧片段残留行

- 现象：重转按 `(session_id, seq)` 覆盖，不删旧行。若同一会话重转产出的句子数少于上次，
  比新句子数大的那些 `seq` 会保留上一轮的文本与时间。
- 复现条件：同一音频、同样参数下分句是确定性的，**正常不会发生**；
  只有音频被外部替换、或 VAD 参数改变（未来可配）时才可能出现。
- 根因：幂等策略选择「只覆盖、不删除」——因为 `ARCH §6.4` 要求转写失败不得删除或改写音频与元数据，
  中途失败时删旧片段会让「上次转好的结果」凭空消失。
- 临时处置：保持「只覆盖不删」，`done` 时以 `COUNT(*)` 重算 `segment_count` 让计数自洽；
  日志记录 `written/skipped` 便于发现异常。若将来支持可调 VAD 参数，需要补一次
  「按 seq >= N 清理」的收尾（仅在本次执行**成功**时做）。
- 状态：未解决（当前不可能触发，留作已知边界）

---

## S4-09 转写队列只在内存，重启后 `queued` 残留需要装配层兜底

- 现象：进程退出时队列里的任务不会持久化；重启后 `sessions.transcription_status` 仍是 `queued`，
  但没有任务在跑。此时「发起」被状态挡住（`queued` 视为忙）。
- 复现条件：任务排队中直接杀掉进程，再启动。
- 根因：调度队列刻意不入库（避免「启动即跑推理」），而启动扫描属装配层职责（S9 启动自检）。
- 临时处置：给 `Scheduler.Cancel` 加了**残留清理分支**——内存里没有该任务但库里仍是
  `queued`/`running` 时，允许取消并落 `none`，把状态修好；否则该会话会卡在
  「不能发起也不能取消」的死角（单测 `TestSchedulerCancelStaleQueued` 覆盖）。
  真正的启动重入队仍建议在 S9 做：启动时扫 `transcription_status IN ('queued','running')`
  → 置 `queued` 并入队（或按产品决定置 `none` 让用户手动发起）。
- 状态：已绕过（残留可自愈；自动重入队待 S9）

---

## S4-10 「边录边转」开关（FR-03-10 的实时路径）本期未实现

- 现象：ROADMAP 4.3 含「边录边转开关（默认关闭）」，但本期交付只覆盖延后转写；
  `asr.Engine.Feed` 在延后模式下从未被调用。
- 复现条件：无——功能尚未接线。
- 根因：实时路径依赖录制侧（S1 `internal/recording`）在采集过程中把帧同时喂给引擎，
  且需要 `live_transcribed` 标记与 `EV-04 transcript:partial`；跨两个域，属分期实现。
- 临时处置：`internal/transcribe` 只做「对已封存录音实例的转写」，`Recognizer` 接口刻意只要求
  `ID` + `Finalize`，不依赖 `Feed`；状态轴已预留 `sessions.live_transcribed` 字段语义
  （边录边转完成时也置 `done`，见 ARCH §6.4 表格）。
- 状态：未解决（按 ROADMAP 分层，留给边录边转专项）
