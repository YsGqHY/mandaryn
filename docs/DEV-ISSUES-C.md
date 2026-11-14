# Wave C 开发问题记录（S2 录制 / S4 转写任务 / S5 会话库）

> **用途**：本阶段开发过程中遇到的每个问题**即时登记**（现象 → 复现条件 → 根因 → 临时处置），
> 阶段结束时逐条回看并确认已解决或有明确结论。
>
> 本文件是 Wave C 的**汇总入口**；三个域各自的细粒度记录在同级分域文件
> （`DEV-ISSUES-S2.md` / `DEV-ISSUES-S4.md` / `DEV-ISSUES-S5.md`），收尾时在此汇总处置结论。
>
> 状态取值：`未解决` / `已绕过` / `已解决` / `改设计`

---

## ENV-07 上一阶段（Wave B）遗留的环境前提，本阶段继续生效

- **现象**：直接 `go build` / `go test` 带 cgo 的包会失败，报 `runtime/cgo: ...cgo.exe: exit status 2`，无任何 gcc 细节。
- **复现条件**：`PATH` 中 `/mingw64/bin` 排在 `/j/msys2/ucrt64/bin` 之前时，任何 cgo 编译。
- **根因**：ucrt64 的 `cc1.exe` 会加载 mingw64 的同名运行库（`libgmp-10.dll`/`libgcc_s_seh-1.dll`/`zlib1.dll`/`msvcrt.dll`），
  ABI 不匹配导致 cc1 静默崩溃（`gcc -E x.c` 表现为 exit 1 且输出全空）。
- **临时处置**：所有构建命令前置 `export PATH="/j/msys2/ucrt64/bin:$PATH"`；已写入
  [DEVELOPMENT.md](./DEVELOPMENT.md) §1.1，并在 `build/windows/Taskfile.yml` 把 `CGO_ENABLED` 默认值改为 `1`。
- **状态**：已解决（文档化 + Taskfile 对齐；属开发者环境前提，无法由代码自动化）。

---

## ENV-08 同一包内多个 subagent 并行时的接口前置依赖

- **现象**：本阶段三个域（S2/S4/S5）并行开发，其中 S4 依赖 S3 的 `asr.Engine`、S2 依赖 S1 的 `capture.Channel`，
  若这些接口在并行期间被改动，会导致某一方编译失败。
- **复现条件**：多 agent 同时修改同一仓库、且消费对方正在演进的接口。
- **根因**：共享工作区，接口是跨域的隐式契约。
- **临时处置**：Wave B 已把 `internal/{capture,asr,models,contract,storage,events}` 与 `internal/app/app.go`
  列为**禁改区**，三个 subagent 只能在自己的新包内实现；`app.go` 装配与 bindings 生成由主 agent 收口。
  接口若确需扩展，只能在各自新建的 `internal/contract/*_ext.go` 里追加，不动既有文件。
- **状态**：已绕过（靠禁改清单 + 分域命名空间隔离）。

---

---

# 阶段收尾：三份分域记录逐条回看结论

本阶段共登记 **43 条**（S2 18 / S4 10 / S5 15），另有本文件 ENV-07/08 两条。

## 一、本次收尾已解决

| 编号 | 问题 | 解决方式 |
|------|------|----------|
| S4-04 / S5-04（同源） | **`paths.Resolver` 的默认层解析失败会让整个 `Resolve` 失败**，即使 cli/env/user 层已有可用值——一次 `%AppData%` / `HOME` 缺失就会否决用户显式指定的路径 | `internal/paths/resolver.go`：默认层改为**可选兜底层**，解析失败只把该层从候选中剔除并保留 `defErr`；仅当「没有任何可用层」时才把原始错误报出。已通过 `internal/paths` 全量单测 |
| ENV-08 相关 | `transcriptsvc.SetOnState(fn func(...))` 是导出的 func 类型方法 → 触发绑定生成器警告并生成畸形前端方法（与 ENV-05、capturesvc 同类问题） | 改为**不导出** `setOnState`，装配走 `Deps.OnState`；bindings 重新生成后**0 警告**（15 服务 / 103 方法） |

## 二、开发中已自行解决（各自条目内有根因与修法）

S2：`WriteAt` 不移动文件偏移导致头被数据覆盖、收尾漏关 Writer 句柄导致文件被占、`*apierr.Err` 型 nil 装进 `error` 变非 nil（成功被判失败 + `HasCode` panic）——三条都是**测试抓出来的真 bug**，已修。
S4：取消/让位语义、`writeCtx` 与 `ctx` 分离、单飞锁、断点锚点。
S5：`count(*)` 对外部内容表读的是正文表（改为查 `segments_fts_docsize` 影子表）、LIKE 通配符转义、SRT 时间码、CSV BOM。

## 三、按设计接受（改设计，需同步文档）

- **S2-05 空录音标 `failed` 并保留**（不删除），`PRD`/`ARCH` 需明确落库口径。
- **S2-10 状态机补 6 条转换**、**S2-12 暂停保留已采音频**、**S2-14 `targetPath` 必填由前端弹窗取得** → 需补进 `ARCH §6.2` / `API-2.7`。
- **S4 的 `canceled` 按 API-4.6 收敛回 `none`**，运行期结论由 `OutcomeCanceled/Yielded` 区分。
- **S5 导出目录语义**：`exportsvc` 自动建目录、txt/srt 走 `filex.WriteAtomic`（差异已文档化）。

## 四、遗留（不阻塞本阶段，均有明确归属）

| 编号 | 遗留内容 | 归属 |
|------|----------|------|
| S2-04 | 单会话 WAV 4GiB ≈ 37 小时上限 | 极端场景；需 RF64 或按 4GiB 自动分段 |
| S2-07 | TBL-10 缺 `interrupted` marker kind，暂复用 `source-lost` | 需 DOC-04 增补 + `contract.Marker*` 常量 + 前端文案 |
| S2-08 | FLAC 导出未实现（当前只 `wav`） | 需引入编码器或外部工具链；UI 应只给 `.wav` |
| S2-16 | 设备重连无退避重试 | 设备韧性专项 |
| S2-17 | 磁盘将满无提前预警 | 产品决策（NFR-08 只管 CPU） |
| S4-01 | 缺 `ERR_AUDIO_MISSING` / `ERR_AUDIO_CORRUPT` 专属码 | 等错误码表补 |
| S4-08 | 重转句子数变少时残留旧行 | 已知边界 |
| S4-10 | 边录边转实时路径未做（`FR-03-10` 默认关） | 边录边转专项 |
| S5-01 | FTS5 的 unicode61 把整段中文当一个 token，中文检索一律走 LIKE | 根治需换 trigram 分词器或改存储层；`DATA-MODEL §5` 的说法需更正 |
