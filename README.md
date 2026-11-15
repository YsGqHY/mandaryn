# Mandaryn

[![Wails v3](https://img.shields.io/badge/Wails-v3.1.0-red)](https://v3.wails.io)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](https://go.dev)
[![React](https://img.shields.io/badge/React-19.2-61DAFB?logo=react)](https://react.dev)
[![Mantine](https://img.shields.io/badge/Mantine-9.7-339AF0?logo=mantine)](https://mantine.dev)
[![TypeScript](https://img.shields.io/badge/TypeScript-strict-3178C6?logo=typescript)](https://www.typescriptlang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

**Mandaryn** 把「我正在听的某个应用里的中文语音」变成**可检索、可回放、分得清是谁说的**文字，且**默认不联网**。

按应用分路捕获音频 → 存成录音实例 → 空闲时再转写 → 声纹区分说话人 → 歌词式回放：

- 🎧 **应用级音频捕获**：只抓指定进程（KOOK / OOPZ / QQ / 微信）的输出，不混入其他应用；不支持进程级捕获的系统自动降级为设备回环
- 🎙 **录制与转写分开**：录制只采音、不抢算力；录完存成**录音实例**，你忙完再转写 —— 手动点一下，或等它在你**电脑空闲时**自己跑
- 📝 **可选边录边转**：需要边说边看时手动开启（默认关），句末自动定稿，本地首字延迟（`FFD-engine`，从开口算起、不含开口前的静音）P50 ≤ 1000ms
- 🗣 **说话人分离与角色命名**：会话内自动聚类，改名**全局同步**到历史会话（声纹归并）
- 📜 **歌词式回放**：文字随进度高亮居中滚动，点击任意片段跳转，同步误差 ≤ 80ms
- 🔒 **本地优先**：本地模式下音频、正文、声纹**不离开本机**；除模型下载外无出站请求、无遥测
- 📦 **离线模型管理**：分片下载 + 断点续传 + 哈希校验 + 代理 + 目录迁移回滚
- ⚡ **整机性能预算**：限制本软件可占用的整机份额，其他应用变忙时自动降档/暂停推理，**录制与落盘不中断**

桌面端基于 **Wails v3 + Go 1.26 + React 19 + Mantine 9**，Windows / macOS / Linux 三平台原生窗口。


### 实现状态（重要）

**当前仓库仍处于「应用外壳 + 文档基线」阶段，产品功能尚未落地。**
下表区分「现在就能用」与「按文档待开发」，避免把设计当成已完成：

| 范围 | 状态 | 说明 |
|------|------|------|
| 应用外壳（窗口 / 布局 / 主题 / i18n / 托盘 / 子窗口） | ✅ 可用 | 9 种布局预设、三套主题 + 自定义调色板、`zh-CN`/`en-US` |
| 底座能力（SQLite + GORM、工具层、原生对话框、导出、E2E） | ✅ 可用 | 见下方「底座能力」 |
| 九大功能域 `FR-01`~`FR-09` | 🚧 **待开发** | 见 [PRD §3](./docs/PRD.md)，61 条需求（P0 47 / P1 13 / P2 1） |
| 捕获 / 转写 / 声纹 / 模型管理 后端包 | 🚧 **待开发** | 见 [ARCHITECTURE §3](./docs/ARCHITECTURE.md) |
| 运行时标识统一（应用目录 / 数据库名 / 环境变量前缀） | 🚧 **收尾中** | module 已是 `vip.oberon.mandaryn`；目录、数据库文件名与环境变量前缀正在统一为 `Mandaryn` / `mandaryn.db` / `MANDARYN_*` |

> 命名约定：产品名 **Mandaryn**，Go module `vip.oberon.mandaryn`，数据目录 `Mandaryn`，数据库 `mandaryn.db`，
> 环境变量前缀 `MANDARYN_`。本文统一按上述命名书写；代码中尚未切换完成的运行时标识见
> [ARCHITECTURE.md](./docs/ARCHITECTURE.md) §10.1「应用标识收尾」。文档中的 `FR-xx` / `NFR-xx` / `TBL-xx` / `ST-xx` 编号
> 以 [DOC-00](./docs/DOC-INDEX.md) 为唯一事实源。

---

## 目录

- [Mandaryn](#mandaryn)
  - [实现状态](#实现状态重要)
  - [目录](#目录)
  - [✨ 特性一览](#-特性一览)
    - [九大功能域（产品目标）](#九大功能域产品目标)
    - [底座能力（已实现）](#底座能力已实现)
  - [🛠 安装 Wails CLI](#-安装-wails-cli)
    - [系统要求](#系统要求)
    - [必装依赖](#必装依赖)
    - [安装 wails3 CLI](#安装-wails3-cli)
    - [验证环境](#验证环境)
  - [🚀 快速开始](#-快速开始)
    - [1. 克隆](#1-克隆)
    - [2. 装依赖](#2-装依赖)
    - [3. 生成 bindings](#3-生成-bindings)
    - [4. 跑起来](#4-跑起来)
    - [数据与模型目录](#数据与模型目录)
  - [📂 项目结构](#-项目结构)
  - [常用命令速查](#常用命令速查)
  - [🧪 E2E 测试](#-e2e-测试)
  - [📐 工程规范](#-工程规范)
  - [🧩 加新功能：常见路径](#-加新功能常见路径)
    - [加一个页面](#加一个页面)
    - [加一张数据表](#加一张数据表)
    - [加一个后端 service](#加一个后端-service)
    - [发起 HTTP 请求](#发起-http-请求)
    - [启动子进程](#启动子进程)
  - [📚 文档](#-文档)
  - [📜 License](#-license)

---

## ✨ 特性一览

### 九大功能域（产品目标）

需求编号与验收要求见 [PRD §3](./docs/PRD.md)，实现方式见 [ARCHITECTURE](./docs/ARCHITECTURE.md)。

| 域 | 编号 | 能力 | 关键点 |
|----|------|------|--------|
| 模型来源与切换 | `FR-01` | 本地 / 在线双来源，可切换、可降级 | 会话中禁止切换；切换在**下一个会话**生效；在线故障自动回退本地并标记 `degraded`；**本地故障不自动回退在线**（隐私默认） |
| 应用级音频捕获 | `FR-02` | 只抓某个进程树的输出混音 | 枚举正在出声的应用；设备回环兜底（标注「会包含所有应用声音」）；应用退出 / 设备切换 / 拔插均有恢复路径 |
| 录制与转写时机 | `FR-03` | 快捷键启停、录完先存、空闲再转 | 9 状态机（`ST-01`~`ST-09`）只管采集与封存；转写是独立任务（`none`/`queued`/`running`/`done`/`failed`）；音频始终落盘，崩溃后标记 `interrupted` 并可稍后转写 |
| 回放与歌词式同步 | `FR-04` | 文字随进度高亮居中滚动 | 高亮误差 ≤ 80ms；拖拽时只预览不解码；手动滚动暂停自动居中并给「回到当前句」 |
| 离线模型下载与代理 | `FR-05` | 分片下载、断点续传、校验、代理 | 积级校验失败自动重下；`http`/`https`/`socks5` + 凭据加密；可分别开关「仅下载」「仅在线转写」走代理 |
| 说话人分离与角色命名 | `FR-06` | 声纹区分 + 改名**全局同步** | 跨会话声纹归并；改名不改历史数据（角色与片段解耦）；可手动合并 / 拆分 |
| 模型目录与迁移 | `FR-07` | 默认 `~/.mandaryn`，可迁移可回滚 | 校验→复制→切换→清理四步；任一步失败旧路径仍可用；路径**来源可见**（默认/我的设置/环境变量/命令行） |
| 隐私边界 | `FR-08` | 两种模式的数据边界与风险提示 | 本地零出站、无遥测；在线需**显式确认风险**才能保存；日志禁写正文；导出保留「在线」标记 |
| 快捷键与精简 UX | `FR-09` | 全局 + 应用内两档快捷键 | 可录制 / 恢复默认 / 单动作禁用，冲突实时校验；配置项收敛到 **25 项** |

### 底座能力（已实现）

| 类别 | 内容 |
|-----|------|
| **窗口** | Windows/Linux 完全无边框（前端自绘标题栏 + 窗口控制按钮）；macOS 沉浸式标题栏 + 系统红绿灯 |
| **布局** | 应用外壳为 **CSS Grid**，内置 **9 种布局预设**（经典图标栏 / 展开侧栏 / 顶部导航 / 融合标题栏 / 右侧栏 / 底部 Dock / 悬浮 Dock（可拖拽）/ 浮动卡片 / 专注抽屉），「设置 → 布局」实时切换并持久化；导航全部从路由表自动渲染；首页为响应式卡片网格 |
| **路由** | 唯一事实源 `routes.tsx` + `<RouterOutlet />` + `useRouter()`；支持 `keepAlive` 页面缓存与 `navigate(id, params)` |
| **UI 栈** | Mantine 9（`@mantine/core` / `@mantine/hooks`）+ CSS Modules + `@tabler/icons-react`；图表 `@mantine/charts`（recharts）+ 自实现高级图表（`components/x-pro/charts`）；数据表格 `components/x-pro/data-grid`（基于 `mantine-datatable`） |
| **主题** | 三套内置主题（明亮 / 黑暗 / 黑曜）+ 用户自定义调色板（16 个语义槽位），编译为 CSS 变量 `--fd-*` + Mantine 主色阶 |
| **i18n** | `zh-CN` / `en-US`（默认 zh-CN）；`'auto'` 跟随浏览器；公用 + 页面级语言包深合并 |
| **首屏优化** | `index.html` 内联静态骨架（消除白屏）+ React 骨架组件 + lazy import + Suspense |
| **持久化** | SQLite + GORM AutoMigrate；用户可在「设置 → 数据存储」改数据库位置（原生「另存为」对话框） |
| **数据可视化** | 设置页内置环形 + 条形图（`@mantine/charts`）展示各表占用 + 逐表清空 |
| **工具层** | `httpx` HTTP（代理感知）/ `procx` 子进程（Windows JobObject）/ `cryptox` AES-GCM / `logx` slog+rotate / `filex` 原子写 |
| **子进程** | 严格白名单子进程 service：前端可调，命令必须在 `commands.go` 注册并通过 `ArgPattern` 校验 |
| **原生对话框** | `NativeDialogs` 服务封装 Wails v3 `Dialogs.*`，禁用浏览器 `alert/confirm/prompt` |
| **系统托盘** | 运行时启用 / 禁用（开关持久化、启动恢复）+ Mantine 托盘面板 + 托盘菜单 i18n |
| **数据表格导出** | 原生「另存为」→ Go 后端写 CSV（UTF-8 BOM）/ 真 `.xlsx`（excelize，表头加粗冻结、数字保持数值）；产品侧计划扩到 `txt` / `srt` |
| **跨窗口同步** | 主窗口 / 子窗口 / 托盘面板的主题与语言经 `appsettings:changed` 事件实时一致 |
| **E2E** | Playwright 驱动 Wails server 模式（真实后端 + 隔离数据目录 `MANDARYN_DATA_DIR`），本 README 截图即由它生成 |

---

## 🛠 安装 Wails CLI

### 系统要求

| 平台 | 版本 |
|------|------|
| Windows | 10 / 11（AMD64 或 ARM64） |
| macOS | 10.15+（开发）/ 部署到 10.13+ |
| Linux | Ubuntu 24.04 AMD64/ARM64（其他发行版亦可） |

### 必装依赖

| 依赖 | 版本 | 验证 / 安装 |
|------|------|------------|
| **Go** | ≥ 1.26（Wails v3.1.0 要求） | `go version` |
| **Node.js** | ≥ 20 | `node --version` |
| **pnpm** | latest | `npm i -g pnpm` |
| **task** (go-task) | latest | `go install github.com/go-task/task/v3/cmd/task@latest` |
| **C 工具链** | — | **必需**：本地推理（sherpa-onnx）与 macOS/Linux 音频捕获依赖 CGO，构建固定 `CGO_ENABLED=1`（见 [ADR-01](./docs/DOC-INDEX.md)）。Windows: MSYS2 mingw-w64 `gcc`；macOS: Xcode CLT；Linux: `gcc` |
| **WebView2 Runtime** | Windows 必装 | Win10 ≥1803 内置；老系统从 [Microsoft 下载](https://developer.microsoft.com/microsoft-edge/webview2/) |
| **Xcode CLT** | macOS 必装 | `xcode-select --install` |
| **gcc + gtk4 + webkit2gtk-6.0** | Linux 必装 | `wails3 doctor` 会给出本机系统的精确安装命令 |

> **CGO 是既定基线，不是可选项**：不提供纯 Go 等价推理路径，也**不做单机交叉编译**（发布走各平台原生构建）。
> `nollm` 变体（`-tags nollm`，仅在线引擎）是**体积敏感场景的可选产物**，**不是** CGO 不可用时的兜底。
>
> Linux 旧栈用户：可以加 `-tags gtk3` 切回 GTK3 + WebKit2GTK 4.1（v3.1 之前保留兼容）。

### 安装 wails3 CLI

```sh
go install -v github.com/wailsapp/wails/v3/cmd/wails3@latest
```

确保 `$GOPATH/bin` 或 `$HOME/go/bin` 在 PATH 里：

```sh
# 临时
export PATH=$PATH:$(go env GOPATH)/bin

# 永久（bash/zsh）
echo 'export PATH=$PATH:$(go env GOPATH)/bin' >> ~/.bashrc

# Windows PowerShell：去"环境变量"图形界面把 %USERPROFILE%\go\bin 加进 PATH
```

### 验证环境

```sh
wails3 doctor
```

输出会列出所有检测项 + 缺失依赖的安装提示。**全绿之后再继续。**

---

## 🚀 快速开始

### 1. 克隆

```sh
git clone https://github.com/FxRayHughes/mandaryn.git
cd mandaryn

# Go module 已是 vip.oberon.mandaryn；应用标识（Mandaryn / mandaryn.db / MANDARYN_*）
# 仍在收尾，见 docs/ARCHITECTURE.md §10.1「应用标识收尾」
```

### 2. 装依赖

```sh
# Go 依赖
go mod tidy

# 前端依赖
cd frontend && pnpm install && cd ..
```

### 3. 生成 bindings

```sh
wails3 generate bindings -ts
```

> bindings 是 Wails 自动生成的前端 ↔ 后端 RPC 胶水代码，路径在 `frontend/bindings/<module-name>/...`。**改了 Go module 名或 service 后必须重新生成。** 已加入 `.gitignore`。
>
> **必须带 `-ts`**：不带该参数会覆盖生成 `.js` 产物，前端 `strict` 类型检查随之失效。

### 4. 跑起来

```sh
# 开发（前后端热重载，Vite dev server 默认端口 9245）
wails3 dev
# 等价：task dev

# 生产构建（当前平台；CGO_ENABLED=1）
task build

# 打包安装程序（Windows：NSIS，含 3 个运行时 DLL + 许可声明）
task package
# → bin/mandaryn-amd64-installer.exe
# 装到 %ProgramFiles%\Oberon\Mandaryn（需提权）；加 INSTALL_SCOPE=user 则装到
# %LOCALAPPDATA%\Programs\Mandaryn 且免 UAC
#
# 注意：NSIS 与 MSI 二选一安装（同时装会有两个「Mandaryn」卸载项）。
# MSI（FORMAT=msi）与便携 zip（FORMAT=portable）见 docs/DEPLOYMENT.md §1。
```

> **为什么不提供"单文件 exe"**：`mandaryn.exe` 的导入表直接引用
> `sherpa-onnx-c-api.dll`（它再依赖 `onnxruntime.dll`），而 Windows 加载器在 `main()`
> 之前就解析这些导入。因此这三个 DLL 必须与 exe 同目录，无法内嵌到单文件里运行时释放。
> 详见 [docs/DEPLOYMENT.md](./docs/DEPLOYMENT.md) §1.1。

### 数据与模型目录

两套根目录**严格分离**（不可混淆）：

| 目录 | 默认位置 | 内容 | 迁移动效 |
|------|----------|------|----------|
| `dataRoot` | Win `%AppData%\Mandaryn`；macOS `~/Library/Application Support/Mandaryn`；Linux `${XDG_DATA_HOME:-~/.local/share}/Mandaryn` | `mandaryn.db`、`.master.key`、`sessions/<id>/audio.wav`、`logs/` | 需**重启**生效（切换等于换一整套数据） |
| `modelRoot` | `~/.mandaryn` | `models/<bundleId>@<version>/` 离线模型包 | 走迁移向导，**重启**后生效 |

均可在应用内修改（原生「另存为」/「选择文件夹」对话框，模型目录带校验与回滚）。

**环境变量覆盖**（便携版 / 运维管控 / E2E 隔离）：

| 变量 | 作用 |
|------|------|
| `MANDARYN_DATA_DIR` | 覆盖 `dataRoot`（数据库 / 密钥 / 日志 / 音频） |
| `MANDARYN_MODEL_ROOT` | 覆盖 `modelRoot` |
| `MANDARYN_LOG_LEVEL` | `info` / `debug` / `off` |
| `MANDARYN_OFFLINE` | 置 `1` 时**禁止一切出站**（下载与在线转写均禁用） |
| `MANDARYN_HTTP_PROXY` | 全局 HTTP 代理（`http://[user:pass@]host:port`，也支持 `https` / `socks5`），**模型下载与清单源获取共用**；为空时保持直连 |
| `MANDARYN_HTTP_NO_PROXY` | 上述代理的直连例外（逗号分隔，支持 `*` 与 `.suffix`） |
| `MANDARYN_MODEL_CATALOG` | 覆盖模型清单基址（企业内网自建镜像） |
| `MANDARYN_MODEL_MIRRORS` | 覆盖清单 / 下载镜像基址（逗号分隔，最多 3 个） |
| `MANDARYN_SERVER_PORT` | server 模式监听端口（仅 `-tags server`） |

路径按 `cli` > `env` > `user` > `default` **四级单向覆盖**，且**来源永远可见**：设置页会同时显示「当前生效值 + 来源徽章」与「你的设置（未生效）」，
被接管时改目录写入暂存而非报错，并提供可复制的解除命令。详见 [DEPLOYMENT §4](./docs/DEPLOYMENT.md)。

---

## 📂 项目结构

```
mandaryn/
├── main.go                            # 仅 embed.FS + app.Run；不接受业务代码
├── go.mod                             # module vip.oberon.mandaryn
├── Taskfile.yml                       # 顶层任务（dev/build/package/server/nollm）
│
├── internal/                          # 后端业务（package private）
│   ├── app/
│   │   ├── app.go                     # 启动顺序：paths→logx→httpx UA→storage→service→app.Run
│   │   ├── window_windows.go          # 无边框窗口（build tag）
│   │   ├── window_darwin.go           # 沉浸式 + 红绿灯
│   │   └── window_linux.go            # 无边框窗口
│   ├── paths/                         # 🚧 路径解析器 ResolvedPath（ADR-02，单点裁决）
│   ├── capture/                       # 🚧 音频源枚举 / 平台捕获 / 设备变化监听
│   ├── asr/                           # 🚧 引擎抽象与路由
│   │   ├── local/                     #     sherpa-onnx 封装（流式 + 离线 + VAD）
│   │   └── online/                    #     OpenAI 兼容客户端
│   ├── diarize/                       # 🚧 声纹提取 / 聚类 / 全局归并 / 角色映射
│   ├── models/                        # 🚧 模型清单 / 下载器 / 校验 / 根目录与迁移
│   ├── recording/                     # 🚧 状态机 / 会话编排 / 落盘 / 时间轴
│   ├── perf/                          # 🚧 分平台 CPU 采样 + 整机预算调度（NFR-08）
│   ├── events/events.go               # 集中注册类型化事件
│   ├── services/                      # Wails Service（自动暴露给前端）
│   │   ├── greet/                     # 示例 service
│   │   ├── preferences/               # KV 偏好（持久化）
│   │   ├── appsettings/               # 单行表（主题/语言/预算/代理/在线端点）
│   │   ├── storagesvc/                # 数据库路径管理 + 表统计 + 清空
│   │   ├── exportsvc/                 # 表格导出 CSV / XLSX（待扩 txt / srt）
│   │   ├── subprocess/                # 子进程白名单（前端可调）
│   │   └── 🚧 capturesvc / recordingsvc / transcriptsvc / sessionsvc
│   │      modelssvc / enginesvc / speakersvc / hotkeysvc / pathsvc
│   ├── storage/                       # SQLite 持久化层
│   │   ├── db.go                      # Open / Holder（热替换）
│   │   ├── pragma.go                  # WAL / busy_timeout / cache / mmap
│   │   ├── models.go                  # AllModels（加表唯一注册中心）
│   │   ├── usage.go                   # dbstat 表占用统计
│   │   └── ...                        # 既有 2 表；目标 12 表（TBL-01~TBL-12）
│   └── utils/                         # 跨业务工具层
│       ├── httpx/                     # HTTP 客户端（代理感知）+ JSON helper + retry
│       ├── procx/                     # 子进程 + Windows JobObject
│       ├── cryptox/                   # AES-GCM 对称加密
│       ├── logx/                      # slog + 控制台 + 文件 rotate
│       └── filex/                     # 原子写
│
├── frontend/                          # React 19 + Mantine 9 + Vite 8
│   ├── index.html                     # 内联首屏骨架（消除白屏）
│   ├── src/
│   │   ├── main.tsx
│   │   ├── App.tsx                    # Provider 链装配
│   │   ├── routes.tsx                 # 路由表（唯一事实源）
│   │   ├── components/                # AppLayout / TitleBar / Navigation / Skeleton / x-pro
│   │   ├── layouts/                   # 🚧→已实现：9 种布局预设 + layoutRegistry
│   │   ├── pages/
│   │   │   ├── HomePage/              # 一个页面 = View+VM+Style(.module.css)+lang+Skeleton
│   │   │   ├── 🚧 RecordPage/         #   选源 + 录制控制 + 录音实例（无转写）
│   │   │   ├── 🚧 LibraryPage/        #   会话列表 + 搜索 + 批量导出/删除
│   │   │   ├── 🚧 SessionPage/        #   播放条 + 歌词式同步 + 角色侧栏
│   │   │   ├── 🚧 ModelsPage/         #   已装/可下载/进度/迁移/代理
│   │   │   └── SettingsPage/
│   │   │       ├── SettingsPage.tsx   # 框架：左列入口 + 右列分发
│   │   │       ├── personalization/   # 子页：主题 + 偏好 + 自定义主题
│   │   │       ├── layout/            # 子页：布局预设
│   │   │       ├── language/          # 子页：语言切换
│   │   │       ├── database/          # 子页：数据存储路径 / 图表 / 清空
│   │   │       └── 🚧 audio/ asr/ privacy/ hotkeys/ performance/
│   │   ├── router/                    # RouteDefinition / RouterOutlet / useRouter
│   │   ├── services/                  # 包装 bindings（View/VM 不直接 import bindings）
│   │   ├── styles/                    # global.css + themes/（Mantine 主题，含 customTheme）
│   │   ├── i18n/                      # 注册式 i18n
│   │   └── preferences/               # PreferencesProvider（异步对接 SQLite）
│   ├── bindings/                      # wails3 自动生成（gitignore）
│   └── package.json
│
├── build/                             # 多平台构建配置 + 资源
│   ├── config.yml                     # productName / identifier / version
│   ├── windows/ darwin/ linux/ ios/ android/ docker/
│   └── ...
│
├── docs/                              # 产品与工程文档（DOC-00 ~ DOC-10）
├── .claude/                           # AI 协作工作区
│   └── skills/                        # Wails v3 官方文档（wails-*）+ 内部规范（主题 / Mantine / i18n / 持久化 / 布局 / 工具层）
│
├── CLAUDE.md                          # 项目级 AI 协作约定
├── README.md                          # 本文
└── .gitignore
```

> `🚧` 标记表示按文档规划、**尚未实现**的目录 / 文件；未标记的均为当前已存在。

---

## 常用命令速查

| 命令 | 作用 |
|------|------|
| `wails3 dev` | 前后端热重载开发模式 |
| `wails3 build` | 直接编译当前平台二进制（无打包） |
| `wails3 generate bindings -ts` | 重新生成 `frontend/bindings/`（**必须带 `-ts`**） |
| `wails3 doctor` | 检测开发环境依赖（含 C 工具链） |
| `task dev` | = `wails3 dev` |
| `task build` | 当前平台生产构建（`CGO_ENABLED=1`） |
| `task package` | Windows：打 NSIS 安装程序 `bin/mandaryn-amd64-installer.exe`（含 3 个运行时 DLL 与许可声明）；`INSTALL_SCOPE=user` 免 UAC；`FORMAT=msi` 出 MSI、`FORMAT=portable` 出便携 zip |
| `task build:nollm` | 精简产物 `bin/mandaryn-nollm[.exe]`（`-tags nollm`，仅在线引擎，体积敏感场景；**不是** CGO 兜底） |
| `task run` | 跑已构建的二进制 |
| `task <os>:build` | 指定平台构建（windows/darwin/linux） |
| `task build:server` | 服务器模式 `bin/mandaryn-server`（无 GUI，纯 HTTP；E2E 前置） |
| `cd frontend && pnpm typecheck` | 单独跑前端类型检查（不出文件） |
| `cd frontend && pnpm build` | 单独构建前端（产物到 `frontend/dist`） |
| `go build .` | 单独构建后端（需要 `frontend/dist` 已存在） |
| `go vet ./...` | 后端静态检查 |
| `go test ./internal/...` | 后端单元测试 |
| `cd frontend && pnpm e2e` | E2E 测试（需先 `wails3 task build:server`） |
| `cd frontend && pnpm e2e:screenshots` | 生成 UI 截图到 `docs/screenshots/`（本地开发用，产物不入库） |

---

## 🧪 E2E 测试

E2E 用 [Playwright](https://playwright.dev) 驱动 **Wails server 模式**：`-tags server` 构建出的二进制不开原生窗口，直接以 HTTP 提供同一套前端 + 全部 Go service / 事件，所以测试覆盖的是真实后端，而不是 mock。

```bash
# 1. 构建 server 模式二进制（产物 bin/mandaryn-server）
wails3 task build:server

# 2. 跑全部 E2E（含悬浮 Dock 拖拽回归测试）
cd frontend && pnpm e2e

# 3. 只跑截图用例（写到 docs/screenshots/，产物不入库）
pnpm e2e:screenshots
```

| 设计点 | 说明 |
|-------|------|
| **数据隔离** | `playwright.config.ts` 每次以空目录启动后端，经 `MANDARYN_DATA_DIR` 让数据库 / 主密钥 / 日志都写进仓库下的 `.e2e-data/`（已 gitignore），**绝不触碰你的真实数据** |
| **确定性** | 截图脚本直接把布局 / 主题写入 E2E 数据库再刷新，不依赖点击路径；就绪以启动骨架移除为准（偏好已从后端读回） |
| **浏览器** | 默认使用本机已装的 Microsoft Edge（`channel: 'msedge'`），免下载；CI 可设 `E2E_CHANNEL=chromium` 并先 `npx playwright install chromium` |
| **端口** | 默认 `9346`，可用 `E2E_PORT` 覆盖（Wails 读取 `WAILS_SERVER_PORT`） |
| **局限** | 浏览器里没有原生窗口：托盘图标、红绿灯、原生对话框需在桌面构建中手动验证 |

`MANDARYN_DATA_DIR` 同样适用于**便携版**：设置后应用把所有数据放在该目录下（不再使用系统用户目录）。

---

## 📐 工程规范

下面这些规范在 `CLAUDE.md` 与 `frontend/CLAUDE.md` 里都有详细说明。简要列出：

| 类别 | 铁律 |
|------|------|
| **MVVM** | View 不写业务（只消费 ViewModel）；ViewModel 不返回 JSX、不直接 import bindings；Service 层包装 bindings |
| **i18n** | 任何人类可见字符串必须经 `t()`（含 `aria-label` / `placeholder` / 对话框文案） |
| **持久化** | 跨会话状态只走 SQLite（`PreferencesService` / `AppSettingsService` / 自定义 service）；**禁止** `localStorage` / `sessionStorage` |
| **业务 service** | 持有 `*storage.Holder`，**不要** capture `*storage.DB`（切换路径会替换） |
| **HTTP** | **禁止** `net/http.DefaultClient`；用 `httpx.GetJSON / PostJSON / Do`（代理感知） |
| **子进程** | **禁止** `os/exec.Cmd.Start`；用 `procx.StartCtx`（自动绑 JobObject / pgid） |
| **加密** | 敏感字符串落 SQLite 必须 `cryptox.EncryptString`（在线密钥、代理密码） |
| **日志** | **禁止** `log.Printf` / `fmt.Println`；用 `logx.For("component").Info(...)`；**禁止**写入音频字节 / 转写正文 / 凭据 |
| **配置文件** | **禁止** `os.WriteFile` 写配置；用 `filex.WriteAtomic` |
| **原生对话框** | 文件 / 文件夹 / 确认 / 错误 提示必须走 `NativeDialogs`，**禁止** `alert/confirm/prompt` |
| **UI 组件** | 只用 Mantine（`@mantine/*`）+ CSS Modules；**禁止**引入 `@mui/*` / `@emotion/*` / `sx` 写法 |
| **Icon** | UI 图标必须用 `@tabler/icons-react` 的 React 组件（`<IconXxx size={20} stroke={1.75} />`）；**禁止** emoji / Unicode / 其它 icon 包 |
| **样式取色** | 样式写 `<Name>.module.css`，颜色永远用 `var(--fd-*)`（JS 侧走主题色常量或取色 hook）；圆角语言：按钮 4–6px / 容器 8px |
| **平台分支** | 走 `internal/app/window_<os>.go` 的 build tag 文件，**禁止** `runtime.GOOS` 判断 |
| **CGO** | 构建固定 `CGO_ENABLED=1`；**不要**为绕过构建问题而改用 `nollm`（它是可选产物，不是兜底） |
| **性能预算** | 资源控制一律围绕**整机预算** `N`（`otherPct = totalCpuPct − selfCpuPct`，`headroom = N − otherPct`）；降档 / 暂停**不得中断录制与落盘** |
| **状态机** | `degraded` / `throttled` / `suspended` 是**正交标记**，禁止塞进 `state` 枚举（否则状态爆炸） |
| **路径解析** | 路径只由 `paths.Resolver` 单点裁决，禁止各模块自行读环境变量；结果必须携带来源（`cli`/`env`/`user`/`default`），**不得静默接管**用户设置 |

---


## 🧩 加新功能：常见路径

### 加一个页面

详见 `frontend/CLAUDE.md` 的「路由规范」。

```ts
// 1. 写页面：frontend/src/pages/AccountPage/{AccountPage.tsx, useAccountPage.ts, AccountPage.module.css, AccountPage.skeleton.tsx, index.ts}
//    若有人类可见文案：再加 lang/{zh-CN.ts, en-US.ts, index.ts} + register<Name>Locales

// 2. 在 src/routes.tsx 追加一项
import { IconUser } from '@tabler/icons-react';
import { AccountPage } from '@/pages/AccountPage';
import { AccountPageSkeleton } from '@/pages/AccountPage/AccountPage.skeleton';

export const routes: RouteDefinition[] = [
  // ...
  {
    id: 'account',
    labelKey: 'route.account',
    label: 'Account',
    icon: IconUser,
    element: <AccountPage />,
    fallback: <AccountPageSkeleton />,
    slot: 'primary',
  },
];

// 3. 公用 locale 加 route.account
// 4. App.tsx 调一次 registerAccountPageLocales()
```

### 加一张数据表

详见 `.claude/skills/` 下的持久化规范。

```go
// 1. internal/storage/models.go
type Message struct {
    ID        uint   `gorm:"primaryKey;autoIncrement"`
    Content   string `gorm:"type:text;not null"`
    CreatedAt int64  `gorm:"autoCreateTime:milli;index"`
}

// 2. 同文件 AllModels 数组追加
var AllModels = []ModelDescriptor{
    // ... 已有
    { Model: &Message{}, TableName: "messages", LabelKey: "messages", Clearable: true },
}

// 3. 重启 → AutoMigrate 自动建表 + 设置页双图自动展示
```

### 加一个后端 service

```go
// 1. internal/services/messages/messages.go
type Service struct { holder *storage.Holder }
func New(holder *storage.Holder) *Service { return &Service{holder: holder} }
func (s *Service) Send(ctx context.Context, content string) error { /* ... */ }

// 2. internal/app/app.go 注册
Services: []application.Service{
    // ... 已有
    application.NewService(messages.New(holder)),
},

// 3. wails3 generate bindings

// 4. frontend/src/services/messages/MessagesService.ts 包装
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/messages';
export const MessagesService = {
    async send(content: string) { return Binding.Send(content); },
};
```

### 发起 HTTP 请求

详见 `.claude/skills/` 下的工具层规范。

```go
import "vip.oberon.mandaryn/internal/utils/httpx"

var resp WeatherResp
if err := httpx.GetJSON(ctx, url, &resp); err != nil {
    if he, ok := httpx.IsHTTPError(err); ok && he.StatusCode == 404 {
        return ErrNotFound
    }
    return err
}
```

### 启动子进程

```go
import "vip.oberon.mandaryn/internal/utils/procx"

err := procx.Run(ctx, procx.Spec{
    Name: "ffmpeg",
    Args: []string{"-i", input, output},
    CaptureOutput: true,
    OnStdout: func(line string) { logx.For("ffmpeg").Debug(line) },
})
// 父进程崩溃时子进程自动连带 kill（Windows JobObject / Unix pgid）
```

要让前端能调，必须经过 `internal/services/subprocess/commands.go` 的白名单。

---

## 📚 文档

**产品与工程文档**（术语、编号、版本以 `DOC-00` 为唯一事实源）：

| 编号 | 文档 | 内容 |
|------|------|------|
| DOC-00 | [DOC-INDEX.md](./docs/DOC-INDEX.md) | 文档索引、术语表、编号体系、版本联动规则、ADR（`ADR-01` CGO / `ADR-02` 路径接管）、仓库基线差异 |
| DOC-01 | [PRD.md](./docs/PRD.md) | 产品定位、九大功能域 `FR-01`~`FR-09`（61 条需求）、8 个用户流程 `UC-01`~`UC-08`、`NFR-01`~`NFR-08` |
| DOC-02 | [ARCHITECTURE.md](./docs/ARCHITECTURE.md) | 技术选型、模块分层、捕获层、引擎抽象与降级、时间轴、**状态机 `ST-01`~`ST-09`**、性能预算调度、路径解析、隐私架构 |
| DOC-03 | [API.md](./docs/API.md) | Wails service 接口（70 方法）、事件契约（15 事件）、错误码（33 个） |
| DOC-04 | [DATA-MODEL.md](./docs/DATA-MODEL.md) | SQLite 表结构 `TBL-01`~`TBL-12`、模型包清单、文件布局 |
| DOC-05 | [UX-SPEC.md](./docs/UX-SPEC.md) | 17 个快捷键、25 项可自定义项、精简原则、关键界面线框、性能与路径来源 UI |
| DOC-06 | [DEPLOYMENT.md](./docs/DEPLOYMENT.md) | 构建打包、运行时目录、环境变量与路径接管、日志诊断、升级回滚、模型分发 |
| DOC-07 | [CHANGELOG.md](./docs/CHANGELOG.md) | 项目变更历史（唯一变更来源，按时间升序） |
| DOC-08 | [ROADMAP.md](./docs/ROADMAP.md) | 开发路线图：按阶段 / 功能域列出已完成与待完成开发项，带完成标记 |
| DOC-09 | [PERSISTENCE.md](./docs/PERSISTENCE.md) | 后端持久化实现指南：数据位置、驱动与 PRAGMA、加表三步、Holder 接入、表统计与清空 |
| DOC-10 | [DEVELOPMENT.md](./docs/DEVELOPMENT.md) | 开发指南：前置条件、常用命令、模块索引与实现状态、编码规范、测试策略 |

**工程与协作**：

| 文档 | 内容 |
|------|------|
| [CLAUDE.md](./CLAUDE.md) | 项目级 AI 协作约定（实现状态 + 跨文档硬约束 + 入口指引，细节指向 `docs/`） |
| [frontend/CLAUDE.md](./frontend/CLAUDE.md) | 前端模块详细规范（MVVM / 路由 / 持久化 / 原生对话框 / Icon / 子目录隔离） |
| [frontend/DEVELOPMENT.md](./frontend/DEVELOPMENT.md) | 前端 MVVM 落地细则 |
| [build/CLAUDE.md](./build/CLAUDE.md) | 多平台构建配置说明 |
| `.claude/skills/wails-*/` | Wails v3 官方文档（按主题拆分的 13 个 skill） |
| `.claude/skills/`（内部规范） | 主题 / Mantine / i18n / 持久化 / 布局 / 工具层使用指南 |
| [Wails v3 官网](https://v3.wails.io) | 上游文档 |
| [Mantine 官网](https://mantine.dev) | UI 组件库（`@mantine/core` / `@mantine/charts`） |
| [mantine-datatable](https://icflorescu.github.io/mantine-datatable/) | 数据表格 |
| [Tabler Icons](https://tabler.io/icons) | 图标库 |

---

## 📜 License

本项目以 **MIT** 协议开源，全文见 [LICENSE](./LICENSE)。

```
Copyright (c) <year> <copyright holders>

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software ... （完整条款见 LICENSE 文件）
```

**第三方组件不与本项目同协议**，各自遵循原许可；再分发构建产物时需一并保留其声明：

| 组件 | 许可 | 说明 |
|------|------|------|
| sherpa-onnx / sherpa-onnx-go | Apache-2.0 | 本地推理运行时，产物内含 `sherpa-onnx-c-api.dll`、`sherpa-onnx-cxx-api.dll` |
| onnxruntime | MIT | 产物内含 `onnxruntime.dll` |
| 语音模型权重（如 zipformer-zh-streaming） | 依模型发布方声明 | 经应用内模型下载获取，不随仓库分发 |

> MIT 只约束本项目自身代码，不改变上述组件的许可义务；`nollm` 精简产物不链接 sherpa-onnx，因而无需附带其 DLL 与声明。
