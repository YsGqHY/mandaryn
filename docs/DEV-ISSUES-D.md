# Wave D 开发问题记录（S7 说话人 / S8 性能预算 / S9 隐私与快捷键）

> **用途**：本阶段开发过程中遇到的每个问题**即时登记**（现象 → 复现条件 → 根因 → 临时处置），
> 阶段结束时逐条回看并确认已解决或有明确结论。
>
> 本文件是 Wave D 的**汇总入口**；四个域各自的细粒度记录在同级分域文件
> （`DEV-ISSUES-S7.md` / `DEV-ISSUES-S8.md` / `DEV-ISSUES-S9a.md` / `DEV-ISSUES-S9b.md`），
> 收尾结论在此汇总。
>
> 状态取值：`未解决` / `已绕过` / `已解决` / `改设计`

---

## 基线（进入 Wave D 时）

- ROADMAP：阶段 4 `[~]`、阶段 8 `[~]`、阶段 3/5 `[~]`、阶段 6/13 `[x]`
- Wave C 遗留 9 条：S2-04（WAV 4GiB）、S2-07（`interrupted` marker kind）、S2-08（FLAC）、
  S2-16（设备重连退避）、S2-17（磁盘预警）、S4-01（音频错误码）、S4-08（重转残留行）、
  S4-10（边录边转）、S5-01（FTS5 unicode61 中文不分词）
- 基线校验：`go build ./internal/... .` 通过，`go test ./internal/...` 15 包全绿

---

## ENV-09 Wave C 遗留的环境前提继续生效

- **现象**：带 cgo 的构建直接失败，只报 `runtime/cgo: ...cgo.exe: exit status 2`。
- **根因**：`PATH` 中 `/mingw64/bin` 先于 `/j/msys2/ucrt64/bin`，ucrt64 的 `cc1.exe` 加载 mingw64 运行库导致静默崩溃。
- **临时处置**：所有构建命令前置 `export PATH="/j/msys2/ucrt64/bin:$PATH"`（已写入 DEVELOPMENT.md §1.1）。
- **状态**：已解决（文档化）。

---

## ENV-10 本机无模型包 → 说话人/推理类功能只能做契约级验证

- **现象**：S3（引擎）、Wave C（转写）都无法在本机跑真实推理；S7 的声纹提取同样需要
  speaker embedding 模型（sherpa-onnx `SpeakerEmbeddingExtractor`），本机尚未下载任何模型包。
- **根因**：模型仓库功能（S6）已就绪但**尚未实际下载模型**。
- **临时处置**：所有涉模型能力一律**接口注入 + 诚实降级**：无模型时返回明确错误码
  （`ERR_MODEL_NOT_FOUND` / `ERR_LOCAL_UNAVAILABLE`），不崩溃、不假装成功；
  单测用 fake 实现覆盖算法与状态逻辑。
- **待办**：端到端真实验证需先下载 `sherpa-onnx-sense-voice-*`（转写）与
  `3dspeaker_speech_campplus_sv_zh-cn_16k-common`（说话人）两个包。
- **状态**：已绕过（按设计降级）。**本阶段仍未消除**，因此 S7 的「声纹提取」标记为
  契约级完成。

---

## ENV-11 Linux 交叉编译必须关 CGO（本机没有 Linux 交叉 C 工具链）

- **现象**：`CGO_ENABLED=1 GOOS=linux go build ./internal/perf/...` 报
  `runtime/cgo: grp.h / sys/mman.h: No such file or directory`。
- **根因**：本机只装了 Windows gcc；`GOOS=linux` + cgo 需要 x86_64-linux-gnu-gcc 之类的交叉工具链。
- **临时处置**：跨平台编译验证一律用 `CGO_ENABLED=0 GOOS=linux`（纯 Go 包足够），
  与 `ADR-01`「不做单机交叉编译」一致；真实的 Linux/macOS 构建交给对应 CI 或实机。
- **状态**：已解决（验证方式约定）。

---

## 阶段内问题汇总

### 一、本次修复的真实缺陷（开发中暴露 → 已修 + 有测试守护）

| 编号 | 问题 | 处置 |
|---|---|---|
| **S8-01** | `transcribe` 判空闲时只看全局 `Idle()`，不看任务来源 —— 接上真实采样后会把**用户手动发起**的转写也拦掉（违反 `FR-03-4`） | 新增 `transcribe.Options.IdleFor(trigger)`，`Scheduler.pick`/`Settled` 改为**按来源逐个判定**（队首被拦时仍取后面的手动任务），未注入时回退旧语义；`app.go` 接 `perfSvc.IdleFor`。3 个新用例守护 |
| **S7-04** | `wrapWrite` 返回 `*apierr.Err` 造成 typed-nil 装进 `error` 后非 nil（成功流程报出空错误） | 返回 `error` 而非具体指针类型，与 Wave C 的同类缺陷一致 |
| **S7-12** | 合并说话人时只遍历 `session_speakers`，漏改「没有映射行」的会话片段 | 改为按片段实际引用集合处理，补用例 |
| **S7-02** | sherpa v1.13.8 `AcceptWaveform` 内部取 `&samples[0]`，空切片直接 panic | 在自己入口拦截空输入并返回明确错误 |
| **S8-02** | `GetSystemTimes` 的 `kernel` 时间**已包含** `idle`，若写成 `idle + kernel + user` 会让整机占用系统性低估近一半、永不降档 | 改为正确分解，并用单测锁定分量关系 |
| **S9A-00** | 出站留痕若「先执行再记录」，请求失败/panic 时就没有痕迹 | `Pipe.DoRecorded` 改为**先登记 pending 再执行**，失败与 panic 也补结果 |

### 二、按设计接受 / 改设计（有明确理由，非缺陷）

- **S7-05**：拆分出的新说话人暂无质心向量（无法凭空合成），首次重新分离时补齐。
- **S7-08**：内置角色禁改（`rl_unknown` 等是合成行，改名会破坏「未识别」语义）。
- **S7-11**：聚类选「稳定排序 + 阈值合并」而非完整层次聚类 —— 片段数在百级，O(n²) 足够且确定性更好。
- **S9A-02**：用户离线开关落 `preferences.privacy.offline`（环境变量仍最高优先，来源可见）。
- **S9A-03**：零出站自检按**执行时刻**判定离线，而非按记录时刻 —— 事后打开离线开关不应把历史合法出站算成违例。
- **S9B-05**：「标记关键时刻」动作未做 —— 录制域只有私有 `appendMarker`，没有导出的 `Mark`；需先在录制域补 API。
- **S9B-06**：`ux.leanMode` 是补充动作（`UX-SPEC` §2.2 未列），默认不绑定。

### 三、按项目铁律改掉的两处（避免绑定生成器警告）

| 问题 | 处置 |
|---|---|
| `appsettings.SetUsageProvider(func() contract.PerfUsage)` 是 L3 上的 func 型导出方法 → `wails3 generate bindings` 报 `function types are not supported by encoding/json` | 改为**构造注入**：`appsettings.New(holder, out, usage)`；`app.go` 内调整构造顺序（`settingsSvc`/`recorder` 先声明变量，闭包延迟求值，不成环） |
| `hotkeys.Service` 需要 `*application.App` 才能注册热键，而服务列表必须在 `application.New` 之前给出 | 沿用 `subprocess` 的「先占位、后 `Attach`」模式：`hotkeysvc.New()` → `Attach(inner)` |

### 四、未解决（有明确归属，不阻塞）

| 编号 | 问题 | 归属 |
|---|---|---|
| **S7-01** | 声纹提取在无模型时只能契约级验证（真实推理未跑过） | 待下载模型包（ENV-10） |
| **S7-07** | `GOOS=linux` + `CGO_ENABLED=1` 无法在本机验证 | CI / 实机（ENV-11） |
| **S8-04** | ~~`appsettings.SetResourceCap` 返回占位读数~~ | **本次已解决**（构造注入 `perf.Usage()`） |
| **S8-10** | `Idle()` 默认比 PRD 的 `headroom > 0` 更严（档位 ≤ L1 且 `otherPct < 10`），常驻浏览器打满的机器可能长期不触发自动转写 | 已参数化（`IdleOtherPctMax`），需产品确认取值 |
| **S8-05** | 降档到 L1/L2/L3 后的**实际执行**（引擎降线程 / 跳帧 / 关二次校正）未接引擎内部 | 引擎域专项（S3 后续） |
| **S8-06** | 降档 / 暂停 / 恢复未写入 `session_events` 时间轴 | 回放阶段（S8 剩余项） |
| **S9A-01** | `outbound_log` 缺 `purpose` / 方向字节 / `status` / `duration` 列 —— 记录只能塞进现有列 | `DATA-MODEL` 加列（DOC-04） |
| **S9A-04** | 诊断包的模块清单与日志副本需装配层注入（`LogDir` 未接） | 装配补齐（日志根目录尚无统一访问器） |
| **S9A-05** | ~~`docs/API.md` 缺 `privacysvc` 一节（6 个方法 + 8 个 DTO）~~ | **本次已解决**：新增 §2.10 `privacysvc`（7 个方法）+ 契约要点 + `ERR_OFFLINE_ENFORCED` 错误码 + 变更记录 v1.4.0 |
| **S9A-07** | 脱敏自检失败没有专用错误码 | `apierr` 增码 |
| **S9B-03** | ~~`events.go` 无热键事件常量~~ | **本次已解决**（新增 `EV-17 hotkey:trigger` / `EV-18 hotkeys:changed`） |
| **S9B-04** | ~~`docs/API.md` §2.8 缺 `Trigger`/`ListActions`/`Available`/精简模式方法~~ | **本次已解决**：补 `API-8.7~8.11` + 动作实现分工说明 |
| **S9B-10** | 精简模式「保留 / 隐去」清单需产品确认 | 产品决策 |

### 四之二、本次补齐的跨域缺口（前端反馈驱动）

| 编号 | 问题 | 处置 |
|---|---|---|
| **D-01** | 诊断包无法落盘：`ExportDiagnostics` 只返回字节，而后端没有任何「写任意字节到指定路径」的入口（`exportsvc` 只认 CSV/XLSX），前端只能把保存按钮做成「不支持」 | 新增 `privacysvc.WriteDiagnostics(ctx, targetPath)`，内部复用同一份自检通过的产物并用 `filex.WriteAtomic` 原子落盘（不用 `os.WriteFile`）；前端 `SAVE_DIAGNOSTICS_SUPPORTED` 置 true 并改为调用该方法 |
| **D-02** | 后端补了方法但前端标志位仍是「不支持」，会出现「后端有能力、界面说没有」的不一致 | 与前一条同批修复；`pnpm typecheck` / `pnpm build` 通过 |
| **D-03** | `-race` 抓到本阶段**自己新写的测试**里 `busy` 布尔量跨 goroutine 读写（测试代码缺陷，不是产品缺陷） | 改用 `atomic.Bool`，`go test -race ./internal/...` 21 包全绿 |

### 五、Wave C 遗留 9 条的本次状态

| 编号 | 状态 |
|---|---|
| S2-04 WAV 4GiB 上限 | 仍未解决（需 RF64 或自动分段） |
| S2-07 缺 `interrupted` marker kind | 仍未解决（待 DOC-04 增补） |
| S2-08 FLAC 导出 | 仍未解决 |
| S2-16 设备重连退避 | 仍未解决 |
| S2-17 磁盘将满预警 | 仍未解决 |
| S4-01 音频错误码 | 仍未解决 |
| S4-08 重转残留行 | 仍未解决 |
| S4-10 边录边转 | 仍未解决（本阶段未做） |
| S5-01 FTS5 中文分词 | 仍未解决（中文检索仍走 LIKE；`DATA-MODEL §5` 的说法需更正） |

> **本阶段新增遗留**：S8-05（降档执行）、S8-06（时间轴打点）、S8-10（空闲阈值待定）、
> S9A-01/S9A-04/S9A-05/S9A-07、S9B-04/S9B-10、S7-01/S7-07，共 11 条；
> 另有「`docs/API.md` 需要回填 `privacysvc` 整节 + `hotkeysvc` 新增方法」这一项文档债。

---

## 阶段收尾结论

- **修复的真实缺陷 6 个**（其中 S8-01 是跨域语义缺口，S8-02 会让限流永不生效），
  另有 **3 个本阶段引入/发现的缺口**在收尾时补齐（D-01 诊断包落盘、D-02 前后端标志一致、D-03 测试数据竞争）。
- **铁律相关改动 2 处**（func 型导出方法改构造注入、Attach 模式）。
- **文档债 2 条已还**（`API.md` §2.10 `privacysvc`、§2.8 `hotkeysvc` 新增方法）。
- **未解决 9 条**（S8-06/S8-10/S8-05、S9A-01/S9A-04/S9A-07、S9B-10、S7-01/S7-07），
  全部有明确归属；**无一条阻塞当前可用性**。
- 环境类 3 条（ENV-09/10/11）中，ENV-10 仍是「真实推理未验证」的根因。
