# Mandaryn - 应用级音频转写桌面软件

## 项目定位

**Mandaryn** 把「我正在听的某个应用里的中文语音」变成**可检索、可回放、分得清是谁说的**文字，且**默认不联网**。

按应用分路捕获音频 → 存成**录音实例** → 空闲时再转写 → 声纹区分说话人 → 歌词式回放。桌面端基于 Wails v3 + Go 1.26 + React 19 + Mantine 9。

产品侧要交付**九大功能域** `FR-01`~`FR-09`（模型来源与切换、应用级音频捕获、录制与转写时机、回放与歌词式同步、离线模型下载与代理、说话人分离与角色命名、模型目录与迁移、隐私边界、快捷键与精简 UX），共 61 条需求 —— 编号、优先级与验收标准见 [docs/PRD.md](./docs/PRD.md) §3。

## ⚠️ 实现状态

**应用外壳与底座能力已可用**：分层 Go 后端、分平台窗口配置、前端 MVVM 规范、可切换布局系统（9 种内置预设 + 可注册自定义）、持久化、工具层、托盘、子窗口、导出、E2E。

**九大功能域尚未实现**：后端包 `capture` / `asr` / `diarize` / `models` / `recording` / `perf` / `paths` 与产品页面（Record / Library / Session / Models）**都不存在**。改代码时不要假设它们已存在 —— 当前模块清单与逐项状态见 [docs/DEVELOPMENT.md](./docs/DEVELOPMENT.md) §3，待做项见 [docs/ROADMAP.md](./docs/ROADMAP.md)。

## 本文件与 docs/ 的分工

本文件只保留**跨文档的硬约束**与**入口指引**，细节一律以下表文档为准（避免同一事实两处维护、逐渐不一致）：

| 想了解 | 看 |
|--------|-----|
| 需求编号、优先级、验收标准、用户流程、非功能指标 | [docs/PRD.md](./docs/PRD.md) |
| 分层与数据流、状态机、引擎降级、性能预算、隐私架构、**通信链路图与模块结构图** | [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) |
| 服务方法签名、事件契约、错误码、前端调用边界 | [docs/API.md](./docs/API.md) |
| 表结构与字段、索引、迁移与清理、文件布局 | [docs/DATA-MODEL.md](./docs/DATA-MODEL.md) |
| 快捷键体系、可自定义项、界面线框、精简原则 | [docs/UX-SPEC.md](./docs/UX-SPEC.md) |
| 构建打包、运行时目录、环境变量、日志诊断、发布清单 | [docs/DEPLOYMENT.md](./docs/DEPLOYMENT.md) |
| **加表三步、Holder 接入、PRAGMA、路径切换**等后端持久化实现 | [docs/PERSISTENCE.md](./docs/PERSISTENCE.md) |
| **本机开发命令、模块索引与实现状态、编码规范、测试策略** | [docs/DEVELOPMENT.md](./docs/DEVELOPMENT.md) |
| **应用标识收尾**：哪些代码位置仍是旧标识（`Foundation` / `foundation.db` / `FOUNDATION_DATA_DIR` 等） | [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) §10.1 |
| 开发计划与完成情况 | [docs/ROADMAP.md](./docs/ROADMAP.md) |
| 某个约定何时引入、为什么这么改 | [docs/CHANGELOG.md](./docs/CHANGELOG.md) |
| 术语表、文档编号体系、架构决策（`ADR-01` / `ADR-02`） | [docs/DOC-INDEX.md](./docs/DOC-INDEX.md) |
| 前端 MVVM、路由、布局预设、Icon、主题取色、设置页隔离 | [frontend/CLAUDE.md](./frontend/CLAUDE.md)、[frontend/DEVELOPMENT.md](./frontend/DEVELOPMENT.md) |
| 构建与打包脚本细节 | [build/CLAUDE.md](./build/CLAUDE.md) |

## 架构要点（只列不可违反的）

- **后端**：Go 1.26（Wails v3.1.0 要求），`main.go` 仅做 `embed.FS` + `app.Run`，装配集中在 `internal/app`。分层为 L2 领域包 + L3 Wails 薄壳，**薄壳只做类型转换与事件转发，领域逻辑放 L2 包**（L2 不 import Wails，因此可被单测直接驱动）。
- **构建固定 `CGO_ENABLED=1`**（`ADR-01`，已决策）：本地推理走 sherpa-onnx，macOS/Linux 捕获也依赖 CGO；不做单机交叉编译。`nollm`（`-tags nollm`，仅在线引擎）是**体积敏感场景的可选产物，不是 CGO 兜底**：本地推理实现文件按 `//go:build windows && cgo && !nollm` 排除、桩按 `nollm || !windows || !cgo` 兜住并恒返回 `ERR_LOCAL_UNAVAILABLE`，构建入口 `task build:nollm`（产物 `bin/mandaryn-nollm[.exe]`，与标准产物互不覆盖）。
- **通信**：`wails3 generate bindings -ts` 生成 `frontend/bindings/vip.oberon.mandaryn/...`，前端经 `services/` 层包装。**`-ts` 不可省**，否则覆盖成 `.js`，strict 类型检查失效。
- **录制与转写解耦**：录制只采集与落盘（默认不推理），转写是对已存录音实例的独立任务，用 `transcription_status` 表达，**与录制主状态 `state` 正交**。主状态 `ST-01`~`ST-09` 存 `state` 字段；`degraded` / `throttled` / `suspended` 是**正交布尔标记**，三者可同时为真。
- **性能预算**（`NFR-08`）：以**整机份额** `N` 为口径（`otherPct = totalCpuPct − selfCpuPct`，`headroom = N − otherPct`），不是「本应用上限」。

## AI 使用指引

### 后端与横切关注点

- **分层准则**：业务逻辑写到 `internal/services/<domain>/`，启动配置写到 `internal/app/`，事件名集中在 `internal/events/events.go`，持久化模型集中在 `internal/storage/models.go`，跨业务横切放 `internal/utils/<name>/`。`main.go` 不接受任何业务代码追加。
- **平台分支**：窗口配置走 `internal/app/window_<os>.go` 的 build tag 文件，不要在 `app.go` 里写 `runtime.GOOS` 判断。
- **持久化铁律**：跨会话状态**只能走 SQLite**，**禁止**前端用 `localStorage` / `sessionStorage`。业务 service 持有 `*storage.Holder`，**不要** capture `*storage.DB`（切换路径会替换它）。加表只需把模型追加到 `AllModels`。实现细节见 [docs/PERSISTENCE.md](./docs/PERSISTENCE.md)，范例与反例见 `.claude/skills/foundation-persistence/SKILL.md`。
- **工具层铁律**（详见 `.claude/skills/foundation-utils/SKILL.md`）：
  - **HTTP**：禁止 `net/http.DefaultClient` / `http.Get/Post`；用 `httpx.GetJSON` / `httpx.PostJSON` / `httpx.Do`
  - **子进程**：禁止 `os/exec.Cmd.Start` 直接用；用 `procx.StartCtx` / `procx.Run`（自动绑 Windows JobObject / Unix pgid）
  - **加密**：敏感字符串落 SQLite **必须**先 `cryptox.EncryptString`
  - **日志**：禁止 `log.Printf` / `fmt.Println`；用 `logx.For("component").Info(...)`
  - **配置文件**：禁止 `os.WriteFile` 写配置；用 `filex.WriteAtomic`
  - **前端调子进程**：必须经 `internal/services/subprocess` 白名单，**禁止**给前端开万能 `RunCommand`
- **前端调用边界**：组件 → ViewModel hook → `services/<Domain>Service.ts` → `@bindings/<module>/...`。View 与 ViewModel 都不许直接 import bindings。

### 前端约定

完整规范见 [frontend/CLAUDE.md](./frontend/CLAUDE.md)（MVVM、路由、布局预设、Icon、主题、原生对话框、设置页隔离）。以下几条最容易踩，列出便于自查：

- **原生对话框**：打开 / 保存 / 选目录 / 确认 / 错误提示必须走 `NativeDialogs`；**禁止** `alert` / `confirm` / `prompt` / `<input type="file">`。
- **Icon**：只用 `@tabler/icons-react` 组件（`<IconXxx size={20} stroke={1.75} />`）；**禁止** emoji、Unicode 符号、内联 `<svg>`、其它 icon 包。
- **样式取色**：组件样式写 CSS Modules，颜色只用 `var(--fd-*)`；JS 侧用 `MANDARYN_CSS_VARS.*`，必须拿真实色值才用 `useMandarynPalette()`。除 `styles/themes/presets/*.ts` 外禁止硬编码十六进制，也不要给 Mantine 组件传内置色名。
- **UI 组件**：只用 Mantine（图表 `@mantine/charts` / 表格 `components/x-pro/data-grid`）；**禁止**重新引入 `@mui/*` / `@emotion/*` / `sx`。
- **i18n**：任何人类可见字符串必须经 `t('key')`（含 `aria-label`、`placeholder`、Tooltip、错误文案）。
- **布局**：新导航形态 = 在 `layouts/presets.ts` 加预设（或 `layoutRegistry.register`）；**禁止**在页面里硬编码布局分支。

### 产品专属铁律（Mandaryn）

- **CGO 不绕过**（`ADR-01`）：**禁止**为了「让 CI 绿」把本地推理改成桩、或把 `nollm` 当成 CGO 兜底。
- **领域逻辑不进 service**：`internal/services/<x>svc` 只做类型转换、错误码映射与事件转发。
- **录制与转写必须解耦**（`FR-03`）：**禁止**在 `Stop`/`finalizing` 里同步跑转写（拖慢收尾会放大崩溃丢音频的风险）；转写失败**不得**删除或改写音频与其元数据。默认不开推理，「边录边转」是显式开关（`FR-03-10`，默认关）。
- **转写状态是独立轴**：`transcription_status` 与 `sessions.state` **正交**；**禁止**把它写进 `state` 或为它新增 `ST` 编号。同一会话同时只允许一个转写任务，重转按 `(session_id, seq)` 幂等覆盖。
- **状态标记用布尔，不扩枚举**：`degraded` / `throttled`（+`throttleLevel`）/ `suspended` 是 `ST-03` 上的正交标记，**禁止**新增 `ST-10+` 或把它们写进 `state` 字段。
- **降级有方向**：在线故障 → 可回退本地（标记 `degraded` + 打点 + 非阻塞提示）；**本地故障不得自动回退在线**（隐私默认），只提示下载/切换。
- **预算不中断录制**（`NFR-08`）：降档与暂停推理都必须让录制与落盘继续；用户可见状态必须说明原因。
- **路径必须带来源**（`ADR-02`）：任何路径都经 `internal/paths.Resolver` 取，返回值含 `source`（`cli`/`env`/`user`/`default`）；被上层接管时写暂存 `pending` 并提供解除提示——**禁止**静默丢弃用户设置，也**禁止**让前端自行推断优先级。
- **两个根目录不合并**：`dataRoot`（库/密钥/日志/音频）与 `modelRoot`（模型包）迁移语义不同，`storagesvc` 与 `modelssvc` 各自负责。
- **音频与正文永不进日志**：`logx` 只写元数据（时长 / 模型 ID / 错误码）；诊断包含日志也不含音频、正文、声纹向量、凭据。
- **凭据必须加密**：在线端点密钥、代理密码经 `cryptox.EncryptString` 落库，界面只显掩码。
- **角色名不冗余落 `segments`**：改名全局同步靠 `roles` 表 JOIN，禁止在片段里冗余存名字。
- **时间基用帧计数**：`startMs`/`endMs` 由 `frameCount / 16000 * 1000` 换算，**不要用 wall clock**。
- **出站要留痕**：新增任何出站请求必须落 `outbound_log` 并受 `MANDARYN_OFFLINE=1` 约束。
- **应用标识**：新增代码一律用 `Mandaryn` / `mandaryn.db` / `MANDARYN_*`；动 `AppDirName` / `DBFileName` / `DataDirEnv` 前先读 [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) §10.1，三处 `AppDirName` 必须同时改。

## 变更记录（Changelog）

本仓库的变更记录**不在本文件维护**，统一存放于 [docs/CHANGELOG.md](./docs/CHANGELOG.md)（项目唯一的变更历史来源，按时间升序）；需要追加或查阅「某个约定何时引入、为什么这么改」时改那里。
