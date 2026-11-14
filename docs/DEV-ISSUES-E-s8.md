# E-1 开发问题记录：把 NFR-08 档位落到引擎内部 + 档位变化写入会话时间轴

> 本文件只记录 Wave E-1（`internal/perf` 档位换算 → `internal/asr` 加载期参数 →
> `internal/recording` 时间轴标记 → `internal/transcribe` 推理闸门）实施过程中
> 遇到的问题与**按设计不做**的结论，按发现顺序编号。
> 需求口径以 [docs/ARCHITECTURE.md](./ARCHITECTURE.md) §4.7.3–4.7.4 与
> [docs/PRD.md](./PRD.md) `NFR-08` 为准；`internal/perf` 自身的实施问题见
> [docs/DEV-ISSUES-S8.md](./DEV-ISSUES-S8.md)，不在此重复。

## E8-01 「二次校正」在当前代码里不存在，L1 的降级动作只能登记为「待边录边转接线」

- 现象：`ARCHITECTURE.md` §4.7.3 的档位表把 `L1` 的动作写成「关闭二次校正（第二遍离线校正）」。但把仓库翻遍也**找不到任何「第二遍推理」的实现**：`internal/asr` 的 `Finalize` 一次调用只做一遍离线解码，`internal/transcribe` 的 `Run` 对每个 VAD 分句也只调一次引擎。
- 复现条件：`grep -rn "Finalize" internal/asr internal/transcribe` —— 每条路径上都只有一次调用；`internal/asr/local` 只有一个 `OfflineRecognizer`，`Feed` 返回已关闭的空 channel（说明流式路径根本没接）。
- 根因：档位表的这一行描述的是**边录边转**（`FR-03-10`，默认关）路径上的动作：「流式先出即时结果，录音结束后再用非流式模型整段重解一遍」。边录边转本身尚未实现（见 `docs/ROADMAP.md`），所以「第二遍」在现网代码里根本没有可关的对象。这不是漏实现，而是档位表**超前于**功能落地。
- 临时处置：
  1. `perf.RefinementAllowed(level)` 给出**可测试的定义**（只有 `L0` 允许），并在注释里写明「当前无调用方，仅为 `FR-03-10` 接线预留」；
  2. **不伪造机制**：既没有为了「让 L1 看起来有动作」而给延后转写路径加一遍无意义的重复推理，也没有把 `L1` 改成某个别的动作去凑齐档位表；
  3. `L1` 与 `L0` 在**线程数**上刻意取同一个值（`max(1, cores/2)`），因为 `L1` 的降级动作与线程无关 —— 若为了「看起来降了一档」而顺手砍线程，等于凭空发明一条没人要求的策略。
- 状态：改设计（L1 的降级动作登记为「待边录边转接线」；`RefinementAllowed` 已就位，接线时只需在二次校正分支前调用它）

## E8-02 「跳帧」在整句批量解码的延后转写路径上没有对应机制，L2/L3 的实际效果是线程数递减

- 现象：档位表把 `L3` 写成「流式引擎隔帧处理（丢帧不丢音频）」。但当前转写是**整句批量解码**（`Runner.Run` 逐窗读 WAV → VAD 分句 → `Engine.Finalize(整个句子)`），没有「流式帧」这个粒度，也就没有「隔帧处理」可以实施。
- 复现条件：读 `internal/transcribe/task.go` 的 `Run` 主循环 —— 单位是「句」，不是「帧」；`internal/asr/local` 的 `Feed` 是空实现，`Finalize` 一次性 `AcceptWaveform(整段)`。
- 根因：跳帧是**流式**引擎（`RoleStreaming` 模型包 + online recognizer）才有的手段：每次 `Feed` 只喂一半帧，牺牲少量精度换取 CPU。延后转写用的是 offline recognizer，一次喂完整句，不存在可跳的中间粒度；若强行抽帧，等于对音频做有损降采样，会**改变音频语义**（而 `NFR-08` 要求降档不得改变已存音频与转写结果的正确性），不是预算该做的事。
- 临时处置：
  1. `ThreadsFor` 把 `L2 → cores/4`、`L3/P → 1` 明确定义出来，**降档的真实手段就是线程数递减**（`L2` 再砍一半，`L3` 收到单线程），并在注释里写清「跳帧不存在，别去找」；
  2. `L3` 与 `P` 在线程数上同为 `1`：`P` 的真实区分不是线程数，而是 `CheckInferenceAllowed()` 直接拒绝新任务（见 E8-03 与 §5 的接线），`L3` 则是「还在跑，但只给 1 个线程」；
  3. 全链路**不改动音频、不丢帧**：`MarkPerf` 与档位换算都不碰 PCM。
- 状态：改设计（`L2/L3` 的实际效果 = 线程数递减；跳帧登记为「需先有流式引擎」）

## E8-03 线程数只能在引擎构造时生效，「改档位立刻换线程」做不到

- 现象：`perf.Level` 从 `L0` 降到 `L2` 后，**正在运行的引擎线程数不变**；只有下一次加载引擎（热切换来源 / `Reload` / 冷启动）才带上新值。
- 复现条件：`perfSvc.Threads()` 返回 2 的同时，一个已 `Warm` 完成的本地引擎仍在用加载时的线程数跑推理。
- 根因：sherpa-onnx 的线程数是 `OfflineRecognizerConfig.ModelConfig.NumThreads`，写进 `NewOfflineRecognizer` 之后**不可修改**（库没有 setter，`OfflineRecognizer` 也不暴露线程配置）。而 `asr.LocalFactory` 是 `func() (Engine, error)`，由 `Router` 在加载时调用一次。要让线程数热生效只能整引擎重建：几秒加载 + 期间无引擎可用，代价远大于收益。
- 临时处置：把「加载期生效」做成**明确契约**而不是含糊的注释——
  1. `local.NewFactoryWithThreads(loc, threads)` 在**工厂被调用时**读一次 `threads()`（不是构造工厂时），于是「下次加载」自然带上新档位；
  2. `asr.RouterOptions.LoadThreads` 在每次加载本地引擎前读一次，`Router.LoadThreads()` 暴露本次加载实际读到的值，日志里带 `threads=` ——「档位改了但没人知道引擎用了几个线程」这种盲区不存在；
  3. 三处注释（`perf/policy.go`、`asr/loadparams.go`、`local/local_windows.go`）用同一句话写明「**不是热生效**，最早下次加载生效」，避免文档互相矛盾；
  4. 测试 `TestRouterReadsThreadsOncePerLocalLoad` 用计数器锁死「每次加载读一次、构造时不读、在线加载不读」。
- 状态：改设计（接受「加载期生效」；若要热生效需先实现引擎重建，属后续决策）

## E8-04 `internal/asr/local` 在 Windows + CGO 下不能有任何测试文件

- 现象：给 `internal/asr/local` 加一个最普通的 `_test.go`（只有一句 `t.Log`），`go test ./internal/asr/local/` 立刻 `exit status 0xc0000135`（DLL 缺失），一条测试都跑不起来。
- 复现条件：在该包内放任意测试文件后 `CGO_ENABLED=1 go test ./internal/asr/local/`。
- 根因：该包的测试二进制会链接 sherpa-onnx，需要运行时能找到 `sherpa-onnx-c-api.dll` / `sherpa-onnx-cxx-api.dll` / `onnxruntime.dll`（三者不在仓库里，见 `docs/DEV-ISSUES-S3.md` S3-02）。于是「有测试文件」比「没测试文件」更糟：没测试文件时 `go test` 只报 `[no test files]`（通过），有则整个包 FAIL。
- 临时处置：
  1. 本波新增的桩构建测试放在 `factory_stub_test.go`，带 `//go:build !windows || !cgo`，因此**不会**在本机（Windows + CGO）参与编译；
  2. 为了不把「工厂每次调用读一次线程数」这条契约押在一个跑不起来的平台上，把该逻辑抽到纯 Go 的 `asr.LoadThreads`，并在 `internal/asr`（无 CGO，测试照常跑）用计数器验证「一次 LoadThreads 只读一次生成器」「每次加载读一次」；
  3. 真实推理测试仍走既有 `sherpa_models` tag，与本波无关。
- 状态：已绕过（契约在 `internal/asr` 侧被测试覆盖；`local` 侧的用例在非 Windows / 非 CGO 构建下生效）

## E8-05 「暂停推理」的闸门此前没有任何调用方

- 现象：`perf.Service.CheckInferenceAllowed()` 在 `P` 档返回 `ERR_PERF_SUSPENDED`，但全仓库没有任何地方调用它 —— `P` 档只影响 `Idle()`（拦自动排队），手动发起的转写照样跑，把已经超额的整机压得更狠。
- 复现条件：`grep -rn "CheckInferenceAllowed" internal/` 只有定义与 `perf` 自己的单测。
- 根因：接线点本该在 L3（`transcriptsvc`），但那是薄壳层，不该承载「什么时候允许推理」的判断；而 `internal/transcribe.Runner` 当时只接受 `Idle/IdleFor`（优先级语义），没有「禁止」这一档。
- 临时处置：在 `internal/transcribe` 的 `Runner.Run` **最开头**加一道注入式闸门 `Options.Allowed func() error`（nil = 不限制），装配层传 `perfSvc.CheckInferenceAllowed`：
  - 拒绝是**可重试**错误（`ERR_PERF_SUSPENDED`），且**零副作用**：不置 `failed`、不写片段、不碰音频、不调引擎（`TestRunRejectedWhenInferenceSuspended` 用文件摘要断言音频未被改写）；
  - 每次 `Run` 都重读闸门，档位恢复后同一个 `Runner` 立刻能继续（`TestRunAllowedReadPerRun`）；
  - 与 `IdleFor` 分工写进 `Options.Allowed` 的注释：`IdleFor` 是优先级（让位、稍后自动重跑），`Allowed` 是禁止（明确拒绝、等用户重试）；
  - **只拦推理**：录制侧没有任何调用点（录制永不因预算中断，`NFR-08` 铁律）。
- 状态：已解决（闸门已接在转写入口；`internal/services/**` 未改动，装配层传 `perfSvc.CheckInferenceAllowed` 即可生效）

## E8-06 （既有缺陷）`Recorder.Metrics()` 与落盘 goroutine 竞争

- 现象：录制进行中调用 `rec.Metrics()`，`go test -race` 报 `Monitor.Feed`（落盘 goroutine）与 `Monitor.Snapshot`（调用方 goroutine）数据竞争。
- 复现条件：`Start()` 之后在测试 goroutine 里调一次 `rec.Metrics()`；`-race` 必现。
- 根因：`Recorder.Metrics()` 只用 `r.mu` 取出 `rn`，随后在**锁外**调 `rn.monitor.Snapshot(...)`；而落盘 goroutine 的 `monitor.Feed(...)` 也不持 `r.mu`。`Monitor` 的注释写着「其它 goroutine 读 `Metrics()` 时请通过 Recorder 的互斥拿快照」，但实现没做到 —— 文档与代码不一致，属既有缺陷，不是本波引入。
- 临时处置：本波的 `MarkPerf` 用例改成读**磁盘文件大小**来证明「落盘没倒退」，绕开这条竞争路径（顺带断言更强：文件真的在长）。竞争本身未修：正确修法要么给 `Monitor` 加互斥（需评估落盘热路径开销），要么让 `Recorder.Metrics()` 全程持 `r.mu` 并要求 `Feed` 同锁 —— 都超出「档位落地」这一波的范围，且会影响 L3 的 4Hz 指标上报路径。
- 状态：未解决（既有缺陷，已登记；本波不修、不掩盖，测试不依赖该路径）
