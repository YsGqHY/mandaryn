# S8 开发问题记录：整机性能预算与自适应限流（`NFR-08`）

> 本文件只记录 `internal/perf`（S8）实施过程中遇到的具体问题与处置，按发现顺序编号。
> 需求口径以 [docs/ARCHITECTURE.md](./ARCHITECTURE.md) §4.7 / §7.1–7.3 与
> [docs/PRD.md](./PRD.md) `NFR-08` 为准；约定变更走 [docs/CHANGELOG.md](./CHANGELOG.md)。

## S8-01 转写调度器的空闲判定会连「手动发起」一起拦

- 现象：把 `perf.Scheduler.Idle()` 直接接到 `transcribe.Options.Idle` 后，整机忙时**用户手动点「开始转写」也不会执行**——任务一直停在 `queued`，界面看起来像没反应，直到整机负载回落才自己跑起来。
- 复现条件：`Options.Idle` 返回 `false`（其它程序占用高），`Schedule.Enqueue(..., Trigger: "manual")`；观察任务长期 `queued`。
- 根因：`internal/transcribe/scheduler.go` 的 `pick()` 在取任务时只判 `s.runner.idle()`，**不看 `req.Trigger`**；`Options.Idle` 的签名又是 `func() bool`，不带触发来源，所以装配层在自己这边没法区分。这违反 `FR-03-4`「手动发起立即执行」，而「让位」只应约束自动排队。
- 临时处置：`perf` 侧提供两个入口——`Idle()`（裸判定）与 `IdleFor(trigger)`（`contract.TriggerManual` 恒 `true`），并在注释里写明推荐接线方式；同时用单测 `TestIdleForManualAlwaysAllowed` 把「手动不受限」锁成契约。真正修好需要 transcribe 侧一行改动（`pick` 处 `if req.Trigger != contract.TriggerManual && !s.runner.idle() { ... }`）；本任务范围内禁止修改 `internal/transcribe/**`，故未改。
- 状态：未解决（待 transcribe 侧修复；`perf` 侧已备好正确语义的入口）

## S8-02 macOS 没有可用的无 cgo 整机 CPU 采样源

- 现象：macOS 上 `perf.NewSampler()` 返回错误，性能预算不工作。
- 复现条件：`GOOS=darwin` 构建后运行；或阅读 `golang.org/x/sys@v0.49.0/unix` 的 darwin 绑定。
- 根因：整机 CPU 需要 Mach 的 `host_processor_info` / `host_statistics(HOST_CPU_LOAD_INFO)`，而 `x/sys/unix` **没有导出**它们（只有 unexported 的 `host_statistics`）；只用 `getrusage(RUSAGE_SELF)` 能拿到本进程时间却拿不到整机总量，两分量缺一就算不出 `otherPct`。补实现需要 cgo 或手写 Mach trap，本域不值得为此引入平台汇编，也不许引第三方库。
- 临时处置：`sampler_darwin.go` **诚实降级**——`newPlatformReader` 返回 `ErrUnsupported`，`perf.New()` 不失败而是进入「不采样、不降档」模式（档位恒 `L0`、`Idle()` 恒 `true`），并通过 `Degraded() (bool, string)` 与一条 warn 日志把原因暴露出来。刻意**不**做成「假装空闲」：那样预算会静默失效，反而更危险。降级路径由 `TestIdleOnDegradedNeverBlocks` 覆盖。后续补实现只需替换 `sampler_darwin.go`，其余逻辑与测试不动。
- 状态：改设计（有意降级，已可查询、可诊断）

## S8-03 暂停反应时延比 PRD 的「3s」更长（约 8s）

- 现象：其它程序把 CPU 拉满后，进入 `P` 档约需 8s，而不是 PRD 验收条目写的「3s 内进入暂停」。
- 复现条件：让 `otherPct` 从低位阶跃到 ≥ N，用 `Scheduler.Tick()` 逐拍观察档位。
- 根因：两层滤波串联。判据取 **5s 滑动平均**（10 点，`DOC-02` §4.7.2，用来滤掉编译/杀毒的瞬时尖峰），确认环节又要求**连续 3s**（`PRD` 暂停判据）。阶跃负载要先被平均「吃掉」约 5s，再攒满 3s 确认。两者都是文档明确要求的行为，不是实现偏差。
- 临时处置：保留现有参数（`BudgetOptions.DownHold = 3s`），把两者口径写进 `Budget.Evaluate` 的注释；同时降档方向做成**一次落到目标档、可跨级**（否则逐级 4×3s 爬完要 12s，3s 目标彻底不可能）。若产品要求严格 3s，可把 `DownHold` 调到 0 并缩短窗口，但会重新引入抖动风险——这一步留给需求方决定，代码已参数化。
- 状态：已绕过（设计口径差异，参数可调）

## S8-04 `appsettings.SetResourceCap` 仍返回占位的 `PerfUsage`

- 现象：设置页改完 `N` 后，`SetResourceCap` 返回的 `PerfUsage` 里 `TotalCpuPct` / `SelfCpuPct` / `OtherPct` 全是 0，`HeadroomPct` 直接等于 `CapPct`，`Level` 恒 `L0`。
- 复现条件：调用 `appsettings.Service.SetResourceCap`。
- 根因：该方法的注释自己写着「调度器在 S8 落地后会填充真实三分量；此处先给配置值」。现在 S8 已落地，但 `internal/services/appsettings/**` 不在本任务可改范围内（且需要注入 `*perf.Service`）。
- 临时处置：`perf.Service.Usage()` 已按 `contract.PerfUsage` 产出真实三分量 + 档位 + `WindowMs=5000` + RFC3339Nano 时间戳，字段映射由 `TestSchedulerUsagePayload` 与 `TestNowRFC3339RoundTrip` 覆盖。装配层只需在 `SetResourceCap` 里返回 `perfSvc.Usage()`，并把 `perfSvc` 通过 `Service` 依赖注入进 `appsettings`。
- 状态：未解决（待装配层接线；`perf` 侧产出已就绪）

## S8-05 进程树枚举按 5s 缓存，短命子进程的 CPU 会漏算

- 现象：若某个吃 CPU 的子进程存活时间短于 5s，其占用可能系统性偏低。
- 复现条件：以子进程方式启动一个跑 2s 就退出的计算任务，观察 `selfCpuPct` 几乎不涨。
- 根因：枚举后代进程（Windows 要快照全系统进程表，Linux 要扫 `/proc`）比取一次 CPU 时间贵一到两个数量级，若每 500ms 全量枚举，采样自身开销会逼近 `DOC-02` §4.7.2 要求的 0.3% 上限。因此按 `treeRefresh = 5s` 缓存句柄/pid 表；其间新起的子进程要等下次刷新才计入。另外 Linux 侧额外累加了 `cutime/cstime`，已回收的子进程时间不会丢。
- 临时处置：保持 5s 缓存（与「推理子进程生命周期是分钟级」的现实相符）；`defaultTreeRefresh` 是文件级常量并有注释说明取舍。若将来出现大量短命推理进程，需要改成事件驱动（`procx` 已有进程组，可挂回调）。
- 状态：已绕过（有意取舍，有注释说明）

## S8-06 `GetSystemTimes` 不在 `golang.org/x/sys/windows` 里

- 现象：`windows.` 补全里找不到 `GetSystemTimes`，而 `GetProcessTimes` / `Process32First` / `Process32Next` 都有。
- 复现条件：在 `x/sys@v0.49.0/windows` 里 grep `func GetSystemTimes`，无结果。
- 根因：x/sys 只导出了常用子集，整机 CPU 时间不在其中（`GetSystemTimes` 是 `kernel32` 的普通导出函数）。
- 临时处置：用 `windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemTimes")` 绑定，只用项目已有依赖（`golang.org/x/sys`）+ 标准库 `unsafe`，不新增模块、不改 `go.mod`。真实平台由 `TestSamplerRealPlatform` 在本机实跑验证过。
- 状态：已解决

## S8-07 整机时间的口径：`kernel` 时间已包含 `idle`

- 现象：最初按「total = idle + kernel + user」写，整机占用率被系统性低估近一半，导致 `otherPct` 偏小、预算永远充足、**永不降档**。
- 复现条件：Windows 上 `GetSystemTimes` 返回三个 `FILETIME`；把 idle 重复计入 total。
- 根因：Windows 的 `kernelTime` 定义**已经包含** `idleTime`，所以可用总量是 `kernel + user`，`idle` 是它的子集。同理 Linux `/proc/stat` 有 guest 双重计入、`iowait` 该不该算闲等坑。
- 临时处置：Windows 侧 `total = kernel + user`；Linux 侧 `total` 只累加到 `steal`（不含 guest/guest_nice），`idle = idle + iowait`。两处都在代码注释里点明原因，并由 `TestSamplerTotalCpuPctIdleShare`（Δidle 占 3/4 ⇒ 占用 25%）与量纲单测共同锁定。
- 状态：已解决

## S8-08 计数器回绕 / 休眠唤醒会算出离谱占用率

- 现象：机器休眠唤醒、或 `FILETIME` 高 32 位丢失后，某一拍算出几百甚至上千个百分点的占用率，直接把推理误判进 `P` 档并停掉转写。
- 复现条件：注入「第二拍计数小于第一拍」的原始读数。
- 根因：累计计数器变小后，无符号差值下溢成巨大数值；若再除以 `Δtotal`，得到无物理意义的结果。
- 临时处置：`Sampler.Sample()` 里任一分量回退即返回 `ErrCounterReset` 并**用新值重建基准**，该拍不出读数；上层（`Scheduler.Tick`）把 `ErrCounterReset` / `ErrNoBaseline` / `ErrNoInterval` 统一视为「本拍无数据」，**保持档位不动且不清确认计数**。三种退化情形都有单测（`TestSamplerCounterReset` / `TestSamplerNoBaselineFirstSample` / `TestSamplerZeroInterval`）。
- 状态：已解决

## S8-09 零间隔采样会让滑动平均被 NaN 污染

- 现象：若两次采样之间整机 CPU 时间没有推进（`Δtotal == 0`），`(Δtotal − Δidle) / Δtotal` 变成 `0/0 = NaN`，NaN 一旦进入滑动平均，后续所有档位比较（`>` / `≤`）恒为 `false`，档位永久卡死。
- 复现条件：连续两次返回完全相同的原始计数器（单测即如此注入）。
- 根因：没有先判分母是否为零就做除法。
- 临时处置：`dTotal <= 0` 时返回 `ErrNoInterval` 且**不更新基准**（让窗口自然拉长到下一拍，避免把一次 0 间隔当成「有效但为 0」的读数）；上层按无数据拍处理。`TestSamplerZeroInterval` 验证零间隔后下一拍仍能算出正确值（37.5% / 12.5%）。
- 状态：已解决

## S8-10 `Idle()` 的实际触发条件比 PRD 的「headroom > 0」更严

- 现象：「空闲自动转写」可能长期不触发——浏览器 + 音乐播放器常驻的机器上，`otherPct` 很容易在 15%–30%，达不到实现里的「空载」门槛（默认 10%）。
- 复现条件：`N = 80`、`otherPct = 15`（档位仍是 `L0`、`headroom = 65`）；此时 `Scheduler.Idle()` 返回 `false`。
- 根因：`FR-03-4` 写的是「整机预算有余量（`headroom > 0`）且无录制进行时」，而实现采用两条更严的判定——档位 `≤ L1`（即 `headroom > 40% × N`）+ `otherPct < IdleOtherPctMax`（默认 10，沿用 `PRD` 对「空载」的定义）。前者是为了「已经在牺牲推理质量时不要再塞任务」，后者直接借用了空载口径。
- 临时处置：阈值已参数化（`SchedulerOptions.IdleOtherPctMax`），放宽只需调一个数（例如 25–30），无需改逻辑；当前默认值取「能追溯到 PRD 空载定义」的 10，而不是我自拟的数字。是否放宽属于产品口径决策，需求方定。另注：本域只约束**自动排队**，手动发起由 `IdleFor(trigger)` 保证不受限（见 S8-01）。
- 状态：未解决（待产品确认阈值；实现已参数化）

