# Wave E 开发问题记录（S8 收尾 / 回放同步 / 模型迁移）

> **用途**：本阶段每个问题的即时登记（现象 → 复现条件 → 根因 → 临时处置），收尾逐条回看。
>
> 分域细粒度记录：`DEV-ISSUES-E-s8.md`（性能收尾）、`DEV-ISSUES-E-playback.md`（音频通道）、
> `DEV-ISSUES-E-fr07.md`（模型迁移）。本文件是汇总与**本 agent 亲自处理**的问题。
>
> 状态：`未解决` / `已绕过` / `已解决` / `改设计`

---

## 一、本次修掉的真实缺陷

| 编号 | 问题 | 处置 |
|---|---|---|
| **E-01** | **`Monitor` 存在真实数据竞争**（E8-06 由 subagent 发现但未修）：`Feed` 由落盘 goroutine 写，`Snapshot`/`RecordedMs`/`MaxPeak` 等由 L3 查询与事件回调从别的 goroutine 读，`Monitor` 自身无锁；文档里「请通过 Recorder 的互斥拿快照」的约定**实际不成立** —— `Recorder.Metrics()` 在调用 `Snapshot` 前就 `r.mu.Unlock()` 了 | 给 `Monitor` 加内部 `sync.Mutex`，7 个读写方法全部加锁；新增 `TestMonitorConcurrentFeedAndRead`（双 goroutine 并发读写 + `-race`）与 `TestMonitorFeedReportCadenceUnchanged`（确认加锁没改变 250ms 上报节拍）。原先「文档说安全、实现不安全」的状态被消除 |
| **E-02** | `go test ./internal/app/` **跑不起来**：该包经 `asr/local` 链到 sherpa（CGO），测试二进制缺 `sherpa-onnx-c-api.dll` 直接 `0xc0000135`；把 DLL 目录加进 `PATH` 也不行 —— Windows 会先命中 `System32\onnxruntime.dll`（同名不同版，`0xc000007b`） | 把音频路由**下沉到子包** `internal/app/audioroute`（只依赖 `application`/`storage`/`paths`，不链 sherpa），测试随包迁移。现在 `go test ./internal/...` 的 22 个包全部能直接跑，不再需要 `go test -c -o bin/...` 之类的绕法 |

## 二、装配层接线（三处，本次完成）

Wave E 的三个后端域都刻意不碰 `internal/app`，接线集中在本次：

1. `asrlocal.NewFactoryWithThreads(modelsSvc.Manager(), perfSvc.Threads)` + `RouterOptions.LoadThreads: perfSvc.Threads` —— 档位→线程数落到引擎。
2. `transcriptsvc.Deps.Allowed = perfSvc.CheckInferenceAllowed` —— P 档在 `Runner.Run` 入口就拒绝（此前 `CheckInferenceAllowed` **没有任何调用方**，是个死接口）。
3. `OnBudget` → `recorder.SetBudgetOverlay` + `recorder.MarkPerf` —— 状态快照与时间轴留痕分开写；新增 `budgetMarkerKind` 判定优先级：**进入暂停 > 恢复 > 降档**（「从 P 直接回 L2」既是恢复也是降档，语义上记为恢复）。

## 三、按设计接受 / 改设计

- **E8-01 二次校正不存在**：全仓每条转写路径只调一次引擎（`Feed` 是空实现），边录边转（`FR-03-10`）未落地。L1 **不伪造**一遍无意义推理，只把「是否允许二次校正」定义成可测函数留待接线，且 L1 与 L0 线程数相同。
- **E8-02 跳帧无对应机制**：延后转写是整句批量解码，抽帧等于有损改变音频语义，不是预算该做的事。L2/L3 的实际效果就是线程数递减；L3 与 P 线程数同为 1，区别是 P 直接拒绝新任务。
- **E8-03 线程数只能加载时生效**：sherpa 无 setter，热生效需整引擎重建 —— 接受，并在三处注释 + 文档统一口径。
- **E-03 角色配色映射重复**：回放抽屉与角色侧栏各自维护一份 `data-token → --role-color` 映射。已逐条核对**完全一致**（10 个令牌），因此「列表与详情颜色一致」成立；但复制两份是维护风险，正确做法是抽到共享 CSS 或主题层加 `--fd-role-1..8`。已登记，本轮不动（会同时改两个刚交付的模块）。

## 四、未解决（有归属，不阻塞）

| 编号 | 问题 | 归属 |
|---|---|---|
| **E-04** | `--fd-role-1..8` 主题令牌不存在，角色配色靠两处 CSS 各拼 `color-mix` | 主题层（`styles/themes`） |
| **E07-06** | `switching` 阶段 rename 半途失败后的回搬只有代码级保证，缺确定性注入测试（Windows 难稳定构造 rename 失败） | 迁移域（建议加 `renameFunc` 测试缝） |
| **E07-07** | 「目标空间不足」的 `ERR_MIGRATE_PRECHECK` 分支无法 mock 磁盘容量 | 迁移域 |
| **E07-08** | 预检不可行时 `(plan, err)` 的明细被薄壳 `throw` 抹平，页面只能显示错误码 | 需改 `internal/contract` 的返回结构 |
| **E-05** | 回放依赖 `/audio/<sessionId>` 同源路由；WebView2 之外（如 E2E 的纯浏览器模式）未验证 | 质量/发布 |
| **E-06** | 回放未做「超大通话分页」（`GetTimelinePage` 已存在但抽屉一次性取全量），千句以上会话首开可能偏慢 | 回放后续 |

## 五、Wave D 遗留的顺延项

- S8-05（降档执行）→ **本次解决**（线程数已落地；跳过项按设计说明）
- S8-06（时间轴打点）→ **本次解决**（`MarkPerf`）
- S9A-01 / S9A-04 / S9A-07 / S9B-10、S2-04 / S2-07 / S2-08 / S2-16 / S2-17 / S4-01 / S4-08 / S4-10 / S5-01 → 状态不变（本阶段未触及）

## 收尾结论

- **修掉 2 个真实缺陷**（数据竞争、测试不可运行），**补齐 3 处死接口接线**。
- **两个阶段转 `[x]`**（阶段 7 `FR-07`、阶段 8 `FR-04`），**阶段 10 `NFR-08` 转 `[x]`**。
- 未解决 6 条，全部有明确归属，无一条阻塞可用性。
- 阶段 7 的剩余两项（rename 失败注入、磁盘容量 mock）属测试完备性，不影响功能。
