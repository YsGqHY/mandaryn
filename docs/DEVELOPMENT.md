# Mandaryn 开发指南（DEVELOPMENT）

| 项 | 值 |
|----|----|
| 文档编号 | DOC-10 |
| 文档版本 | **v1.0.0** |
| 产品基线 | Mandaryn v0.1.0 |
| 状态 | 生效（开发指南） |
| 上游文档 | [ARCHITECTURE.md](./ARCHITECTURE.md)、[ROADMAP.md](./ROADMAP.md) |
| 下游文档 | — |

> 本文覆盖**本机开发**所需的一切操作信息：前置条件、常用命令、当前模块索引与实现状态、编码规范、测试策略。
> 后端分层与设计意图见 [ARCHITECTURE.md](./ARCHITECTURE.md)；构建打包与发布见 [DEPLOYMENT.md](./DEPLOYMENT.md)；
> 前端专属规范见 [frontend/CLAUDE.md](../frontend/CLAUDE.md) 与 [frontend/DEVELOPMENT.md](../frontend/DEVELOPMENT.md)。

---

## 1. 前置条件

Go 1.26+、Node.js + pnpm、`wails3` CLI、`task` (go-task)、**C 工具链**（CGO 必需：Windows MSYS2 mingw-w64 / macOS Xcode CLT / Linux gcc）。

### 1.1 Windows + MSYS2：CGO 必须让 ucrt64 排在 mingw64 之前（**否则所有 cgo 构建静默失败**）

MSYS2 常同时装有 `/mingw64`（msvcrt 运行时）与 `/ucrt64`（UCRT 运行时）。若 `PATH` 里 `/mingw64/bin`
排在 `/j/msys2/ucrt64/bin` 前面，ucrt64 的 `cc1.exe` 会加载 mingw64 的同名运行库
（`libgmp-10.dll` / `libgcc_s_seh-1.dll` / `zlib1.dll` / `msvcrt.dll`），ABI 不匹配导致 **cc1 静默崩溃**。

症状很有迷惑性：`go build` 只报 `runtime/cgo: ...cgo.exe: exit status 2`，而 `gcc --version` 看起来正常；
单独跑 `gcc -E xxx.c` 会 **exit 1 且 stdout/stderr 全空**。

自查与修复：

```bash
# 自查：正常应输出上千行宏定义；空输出即中招
echo '' | gcc -E -dM -xc -O2 - | wc -l

# 修复：构建前把 ucrt64 前置
export PATH="/j/msys2/ucrt64/bin:$PATH"

# 复核：cc1 依赖应全部来自 ucrt64/bin
ldd /j/msys2/ucrt64/lib/gcc/x86_64-w64-mingw32/*/cc1.exe | grep -E 'mingw64|ucrt64'
```

### 1.2 CGO 运行时 DLL 必须随产物分发

本地推理依赖 `sherpa-onnx-go-windows`，它只提供**导入库**，运行时需要三个 DLL 与 exe 同目录：

`sherpa-onnx-c-api.dll`、`sherpa-onnx-cxx-api.dll`、`onnxruntime.dll`

不拷贝的表现是**双击无反应 / 进程 exit 127 且无任何输出**（很隐蔽）。
`task build` 已内置 `windows:copy:runtime:dlls` 自动拷贝；手工 `go build` 时需自己从
`$(go env GOMODCACHE)/github.com/k2-fsa/sherpa-onnx-go-windows@<ver>/lib/x86_64-pc-windows-gnu/` 拷一份。


## 2. 常用命令

| 命令 | 说明 |
|------|------|
| `wails3 dev` 或 `task dev` | 前后端热重载（Vite 端口默认 9245） |
| `wails3 generate bindings -ts` | 重新生成 `frontend/bindings/<module>/...`（**`-ts` 必带**） |
| `task build` | 当前 OS 生产构建（`CGO_ENABLED=1`） |
| `task package` | 当前 OS 打包 |
| `task build:nollm` | 精简产物（`-tags nollm`，仅在线引擎；产物 `bin/mandaryn-nollm[.exe]`；可选产物，非 CGO 兜底） |
| `cd frontend && pnpm install` | 首次或更新依赖后 |
| `cd frontend && pnpm typecheck` | TypeScript 类型检查 |
| `cd frontend && pnpm build` | 前端单独生产构建 |
| `go build .` | 后端单独构建（需要 `frontend/dist` 已存在） |
| `go test ./internal/...` | Go 单元测试 |
| `wails3 task build:server` | 构建 server 模式二进制（无 GUI，纯 HTTP；E2E 前置）。产物名由 `Taskfile.yml` 的 `APP_NAME` 决定，当前为 `bin/mandaryn` |
| `cd frontend && pnpm e2e` | Playwright E2E（自动启动 server 二进制，数据目录隔离到 `.e2e-data/`） |
| `cd frontend && pnpm e2e:screenshots` | 生成 UI 截图（本地开发用，产物不入库） |

## 3. 模块索引与实现状态

`🚧 待实现` 标记表示该模块尚未落地，改动代码时不要假设它已存在。目标设计见 [ARCHITECTURE.md](./ARCHITECTURE.md) §3。

| 模块 | 路径 | 语言/技术 | 一句话职责 |
|------|------|-----------|------------|
| 应用入口 | `main.go` | Go | 仅含 `embed.FS` 与 `app.Run(assets)`，业务下沉到 `internal/` |
| 应用启动 | `internal/app/` | Go | 服务注册、平台窗口配置、事件循环、子窗口（ChildWindowService）、系统托盘（SystrayService，`systray*.go` 分平台） |
| 路径解析 | `internal/paths/` | Go | 🚧 待实现：`Resolver` 单点裁决 `dataRoot`/`modelRoot`，输出 `ResolvedPath`（含来源层），见 `ADR-02` |
| 领域层（L2） | `internal/{capture,asr,diarize,models,recording,perf}/` | Go（CGO） | 🚧 待实现：捕获 / 引擎与路由 / 声纹 / 模型仓库 / 会话编排 / 资源采样。**领域逻辑都在这层**，不进 service |
| 业务服务 | `internal/services/` | Go | Wails Service（每个领域一个子包：greet / preferences / appsettings / storagesvc / exportsvc（CSV / XLSX）/ subprocess；🚧 待加 capture / recording / transcript / sessions / models / engine / speakers / hotkeys / path） |
| 持久化层 | `internal/storage/` | Go + GORM | SQLite 打开 / PRAGMA / AutoMigrate / 路径配置（含数据目录环境变量覆盖）/ Holder。详见 [PERSISTENCE.md](./PERSISTENCE.md) |
| 工具层 | `internal/utils/` | Go | httpx（HTTP，代理感知）/ procx（子进程 + JobObject）/ cryptox（AES-GCM）/ logx（slog + rotate）/ filex（原子写） |
| 事件契约 | `internal/events/` | Go | 集中注册类型化事件，前后端共享名称 |
| 前端 UI | `frontend/` | React 19 + Mantine 9 + TS | 用户界面、调用 Service、订阅事件 |
| 布局系统 | `frontend/src/layouts/` + `components/AppLayout` + `components/Navigation` | TS + CSS Grid | 布局预设注册 / 外壳网格 / 导航形态（详见 `.claude/skills/foundation-layout/SKILL.md`） |
| E2E 测试 | `frontend/e2e/` + `frontend/playwright.config.ts` | Playwright | 驱动 server 模式真实后端 + 前端；含 UI 截图用例（产物不入库） |
| 文档基线 | `docs/` | Markdown | DOC-00 索引 / DOC-01 PRD / DOC-02 架构 / DOC-03 接口 / DOC-04 数据模型 / DOC-05 UX / DOC-06 部署 / DOC-07 变更记录 / DOC-08 路线图 / DOC-09 持久化 / DOC-10 开发指南 |
| 多平台构建 | `build/` | Taskfile / Gradle / Shell / nfpm | 各平台打包脚本与资源 |

## 4. 编码规范

- Go：`gofmt`、`go vet`，按 `internal/` 分层，跨文件夹引用走 import 而非相对路径。
- 前端：见 [frontend/DEVELOPMENT.md](../frontend/DEVELOPMENT.md)（MVVM、文件夹规约、theme-only 配色、方圆设计）。
- 文件路径：仓库内统一正斜杠。`.gitignore` 已忽略 `bin`、`frontend/dist`、`frontend/node_modules`、`frontend/bindings`、`.task`、`.e2e-data`、`frontend/test-results`、`frontend/playwright-report`、`.narrafork`（NarraFork 工作目录）。

## 5. 测试策略

- **Go 单元测试**：`internal/services/exportsvc/exportsvc_test.go`（CSV / XLSX 导出）。新增测试放 `internal/services/<domain>/<name>_test.go`，`go test ./internal/...`。
- **E2E（Playwright）**：`frontend/playwright.config.ts` + `frontend/e2e/`
  - 被测对象是 Wails **server 模式**二进制（`wails3 task build:server`，`-tags server`，无原生窗口，纯 HTTP），即真实 Go 后端 + 真实前端；Playwright `webServer` 每次先清空再启动，数据目录指向隔离目录 `.e2e-data/`（已 gitignore），绝不碰用户真实数据库。
  - 浏览器默认 `channel: 'msedge'`（用本机已装的 Edge，免 `playwright install`）；环境变量 `E2E_CHANNEL`（CI 可设 `chromium` 并预装）/ `E2E_PORT`（默认 9346，传给 `WAILS_SERVER_PORT`）/ `E2E_DATA_DIR`。串行执行（`workers: 1`，共用一个后端）。
  - `e2e/helpers.ts`：`openApp`（等 `#startup-skeleton` 消失 = 偏好已读回）、`chooseLayout`（经设置页切换布局预设）。
  - `e2e/floating-dock.spec.ts`：悬浮 Dock 拖到任意位置都留在内容区内（不进标题栏 / 红绿灯）、松手后手柄仍可拖、刷新后位置持久化。
  - `e2e/screenshots.spec.ts`：用 `sqlite3` 直接写 E2E 库的 preferences / app_settings 后刷新，生成 9 种布局 / 页面 / 主题 / 托盘面板 / 子窗口截图（输出目录不入库）（`pnpm e2e:screenshots`；需本机有 `sqlite3`，标题栏按 Windows 渲染）。
- 尚无前端单元测试；如需可加 Vitest（配 React Testing Library）覆盖 ViewModel hook。
- 测试的**验收口径**（哪些链路必须覆盖、性能门限）见 [DEPLOYMENT.md](./DEPLOYMENT.md) §9；待实现项见 [ROADMAP.md](./ROADMAP.md) §14。

## 6. 变更记录

| 日期 | 版本 | 变更 | 作者 |
|------|------|------|------|
| 2026-11-11 | v1.0.0 | 首版：从根 `CLAUDE.md` 迁出开发操作信息（前置条件、常用命令表、模块索引与实现状态、编码规范、测试策略），`CLAUDE.md` 改为引用本文 | Kether |
