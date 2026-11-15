# Mandaryn 部署与运维说明（DEPLOYMENT）

| 项 | 值 |
|----|----|
| 文档编号 | DOC-06 |
| 文档版本 | **v1.2.0** |
| 产品基线 | Mandaryn v0.1.0 |
| 状态 | 评审通过（Baseline） |
| 最后更新 | 2026-11-10 |
| 上游文档 | [ARCHITECTURE.md](./ARCHITECTURE.md)、[DATA-MODEL.md](./DATA-MODEL.md)、[API.md](./API.md) |
| 下游文档 | — |

> 本文覆盖构建、打包、运行时目录、环境变量、日志、升级回滚、模型分发、离线部署与发布检查。
> 与脚手架既有构建流程冲突处（尤其 CGO），本文为准。

---

## 1. 交付物形态

| 形态 | 平台 | 说明 | 是否含模型 |
|------|------|------|-----------|
| 标准安装包 | Windows `.exe`(NSIS) / `.msix`；macOS `.dmg`；Linux `.AppImage`/`.deb`/`.rpm` | 完整功能（本地 + 在线） | ❌（首次启动下载） |
| 精简安装包 `nollm` | 全平台 | `-tags nollm`：**不含本地推理**（sherpa-onnx / 声纹提取被 `!nollm` 排除，改链恒失败桩），只能走在线引擎；**可选产物，非 CGO 兜底** | ❌ |
| 离线模型包 | 通用 `.zip` | 可直接放入 `modelRoot` 或经「导入离线包」导入 | ✅ |
| Server 版二进制 | 全平台 | `-tags server,production`，无 GUI，纯 HTTP（E2E 与无头录制） | ❌ |
| 便携版 | Windows | 通过 `MANDARYN_DATA_DIR` 指向同目录实现 | ❌ |

**版本号**：语义化 `MAJOR.MINOR.PATCH`；唯一事实源为 `build/config.yml` 的 `info.version`（前端经 `__APP_VERSION__` 读取）。

---

## 2. 构建

### 2.1 前置依赖

| 平台 | 必需 | 说明 |
|------|------|------|
| 通用 | Go 1.26+、Node 20+、`pnpm`、`task`（wailsapp fork）、`wails3` CLI（**必须 v3.1.0，与 go.mod 对齐**） | `wails3` 版本不一致会生成 `.js` 绑定并破坏 TS 契约（见 §11 故障 #1） |
| Windows | MSYS2 mingw-w64（`gcc`）、WebView2 Runtime（运行期） | CGO 必需；建议 `CGO_ENABLED=1` |
| macOS | Xcode Command Line Tools、`codesign`/`notarytool` 凭据 | 需授权「系统音频」权限用于运行期 |
| Linux | `gcc`、`pkg-config`、`libwebkit2gtk-4.1-dev`、`libpipewire-0.3-dev`、`nfpm` | 打包 deb/rpm 需要 |

> **CGO 是既定基线（已决策，ADR-01，见 DOC-00 §6）**：本地推理（sherpa-onnx）与 macOS/Linux 捕获均依赖 CGO。
> 项目接受该依赖，因此**不提供纯 Go 等价路径**；构建机必须具备 C 工具链，发布走各平台原生构建。
> `nollm` 变体仅作为**可选产物**（体积敏感 / 无本地推理需求的场景），不是 CGO 的兜底方案。

### 2.2 构建命令

```bash
# 0) 环境（本机已知坑见 §11）
export CGO_ENABLED=1
export GOPROXY="https://goproxy.cn,direct" GOSUMDB="sum.golang.google.cn"   # 直连 proxy.golang.org 不稳时
export PATH="$(go env GOBIN):$PATH"                                          # wails3 / task 所在

# 1) 依赖 + 绑定 + 前端 + 后端（一条链）
task build            # 产物：bin/mandaryn[.exe]

# 2) 打包安装器
task package          # 产物：bin/ 下各平台安装包

# 3) server 模式（E2E / 无头）
task build:server     # 产物：bin/mandaryn-server

# 4) nollm 精简产物（仅在线引擎，体积敏感场景）
task build:nollm      # 产物：bin/mandaryn-nollm[.exe]，与标准产物互不覆盖
```

**构建链顺序不可颠倒**：`go mod tidy` → `generate:bindings`（**必须带 `-ts`**）→ `tsc -b` → `vite build` → `go build`（依赖 `frontend/dist` 供 `//go:embed all:frontend/dist`）。

### 2.2.1 `nollm` 精简产物

`task build:nollm` 与 `task build` 是**同一条 `go build` 命令派生**的（Windows 上为
`build:nollm` → `build:native`，只覆盖两个变量），差别仅两处：

| 差异 | 标准产物 | `nollm` 产物 |
|------|----------|--------------|
| 构建标签 | `-tags production` | `-tags production,nollm` |
| `CGO_ENABLED` | `1`（基线） | `0`（Windows 上；不链接本地推理，也不需要那 3 个运行时 DLL） |
| 产物名 | `bin/mandaryn[.exe]` | `bin/mandaryn-nollm[.exe]` |
| 运行时 DLL | 需 sherpa-onnx 3 个 DLL 与 exe 同目录 | **不需要**（`copy:runtime:dlls` 在 `CGO_ENABLED=0` 时自动跳过） |

**产物符号表里没有本地推理**：`internal/asr/local/{local_windows,streaming_windows}.go` 与
`internal/speaker/sherpa/sherpa_windows.go` 都带 `//go:build ... && !nollm`，`nollm` 下由同名桩替上。
桩恒返回 `ERR_LOCAL_UNAVAILABLE`（带「本构建不含本地引擎」的原因），
**不会**静默降级去调在线引擎 —— 本地来源需要用户显式切到在线。

实测体积（Windows amd64，`-trimpath -ldflags="-w -s -H windowsgui"`）：

| | exe | 必需 DLL | 部署总体积 |
|---|---|---|---|
| 标准 `bin/mandaryn.exe` | 23,709,184 B（约 22.6 MiB） | `sherpa-onnx-c-api.dll` + `sherpa-onnx-cxx-api.dll` + `onnxruntime.dll` = 22,664,704 B（约 21.6 MiB） | **约 44.2 MiB** |
| `nollm` `bin/mandaryn-nollm.exe` | 23,522,816 B（约 22.4 MiB） | 无 | **约 22.4 MiB**（−49%） |

> exe 本体只小约 182 KiB：Windows 上 sherpa-onnx 是**动态链接**，省下的体积主要落在那 3 个 DLL。
> `nollm` 产物的 PE 导入表只剩 `kernel32.dll`，标准产物还额外导入
> `sherpa-onnx-c-api.dll` 与 9 个 MSVC CRT api-set DLL。
> Linux / macOS 上 sherpa-onnx 为静态链接，exe/二进制本体的差会更明显（未实测，不作数字承诺）。

**红线**：`nollm` 不是 CGO 兜底（ADR-01）。它存在的理由是「体积敏感 + 不需要本地推理」，
**不得**用它规避任何构建/CI 问题；主发布物仍按 `CGO_ENABLED=1` 构建。
`CGO_ENABLED=0` 在 Windows 上实测可用，但**不承诺**其它平台（取决于该平台捕获层是否用 CGO）。

> 已在 `build/Taskfile.yml` / `build/android/Taskfile.yml` / `build/ios/Taskfile.yml` 的
> `generate:bindings` 中固定 `-ts`。若自定义构建脚本，**必须**同样带上，否则绑定会被覆盖成 `.js`（API.md §7）。

### 2.3 CI 矩阵（推荐）

| Job | Runner | 产出 |
|-----|--------|------|
| `lint-test` | ubuntu-latest | `gofmt -l`、`go vet ./internal/...`、`go test ./internal/...`、`pnpm typecheck` |
| `build-windows` | windows-latest | NSIS + msix |
| `build-macos` | macos-14（arm64）+ macos-13（amd64） | dmg（含签名/公证） |
| `build-linux` | ubuntu-22.04 | AppImage + deb + rpm |
| `build-nollm` | 任一 | 精简版 `task build:nollm`（冒烟测试：能启动、能切在线、本地来源返回 `ERR_LOCAL_UNAVAILABLE`） |
| `e2e` | windows-latest | Playwright 驱动 `mandaryn-server`（隔离数据目录） |

**不做单机交叉编译**（CGO 使然）。每个平台原生构建。

### 2.4 构建后自检（必须）

```bash
./bin/mandaryn-server --self-check   # 计划内命令（v0.1.0 实现）
# 校验项：数据目录可写、DB 可打开、modelRoot 有效、引擎库可加载、
#        捕获能力探测（进程级是否可用）、绑定文件与后端签名一致
```

---

## 3. 运行时目录与权限

| 路径 | 默认 | 创建者 | 权限要求 |
|------|------|--------|----------|
| `dataRoot` | Win `%AppData%\Mandaryn`；macOS `~/Library/Application Support/Mandaryn`；Linux `${XDG_DATA_HOME:-~/.local/share}/Mandaryn` | 应用首启 | 仅当前用户读写 |
| `dataRoot/mandaryn.db` | — | storage 层 | 同上 |
| `dataRoot/.master.key` | — | cryptox | POSIX `0600`；Windows 用户 ACL |
| `dataRoot/sessions/<id>/audio.wav` | — | recording | 同上 |
| `dataRoot/logs/` | — | logx | 同上 |
| `modelRoot` | **`~/.mandaryn`** | models 层 | 读写 + 可执行（模型目录无执行需求，仅读） |

> 两套根目录**不可混淆**（DATA-MODEL §1.1）。`dataRoot` 迁移走 `storagesvc`（需重启）；
> `modelRoot` 迁移走 `modelssvc`（即时，带校验与回滚）。

---

## 4. 环境变量

| 变量 | 默认 | 作用 | 备注 |
|------|------|------|------|
| `MANDARYN_DATA_DIR` | 平台默认 | 覆盖 `dataRoot`（数据库/日志/密钥/音频） | 便携版、E2E 隔离、多实例并存 |
| `MANDARYN_MODEL_ROOT` | `~/.mandaryn` | 覆盖 `modelRoot` | **优先于设置页的值**（便于运维强制），优先级见 §4.1 |
| `MANDARYN_LOG_LEVEL` | 未设 | `debug` 强制开启调试模式；`info`/`off` 强制关闭 | 覆盖设置页「调试模式」开关（设置页显示「由环境变量接管」并禁用开关，用户层值保留）；在日志初始化后立即生效，可记下启动阶段；其他值忽略并记一条警告 |
| `MANDARYN_OFFLINE` | 未设 | 置 `1` 时**禁止一切出站**（下载与在线转写均禁用） | 企业离线部署强制项（§8） |
| `MANDARYN_HTTP_PROXY` | 未设 | 全局 HTTP 代理（标准 URL 形式，可内嵌凭据） | **模型下载与模型清单源获取共用这一份**；优先级高于设置页，详见 §4.7 |
| `MANDARYN_HTTP_NO_PROXY` | 未设 | 全局代理的直连例外（逗号分隔，支持 `*` 与 `.suffix`） | 仅配合 `MANDARYN_HTTP_PROXY` 生效 |
| `MANDARYN_MODEL_CATALOG` | GitHub Releases | 覆盖模型清单基址（目录或 `.json` 直链） | 企业内网自建镜像；出站**仍受**代理与离线开关约束 |
| `MANDARYN_MODEL_MIRRORS` | 内置 hf-mirror | 覆盖清单 / 下载镜像基址（逗号分隔，最多 3 个生效） | 同上 |
| `MANDARYN_SERVER_PORT` | `8080` | server 模式监听端口 | 仅 `-tags server` |
| `E2E_DATA_DIR` / `E2E_PORT` / `E2E_CHANNEL` | — | Playwright E2E 专用 | 仅测试 |

> 落地注意：脚手架当前读取 `FOUNDATION_DATA_DIR`（`internal/storage/paths.go` 的 `DataDirEnv`）。
> 改名时必须同步 `storage` / `cryptox` / `logx` 三处常量，否则密钥与日志会落在不同根目录。

### 4.1 路径解析优先级（**已决策**）

作用于两个**路径类**变量（`MANDARYN_DATA_DIR` / `MANDARYN_MODEL_ROOT`），四级单向覆盖：

| 优先级 | 来源 | 标识 `pathSource` | 典型使用者 |
|--------|------|-------------------|-----------|
| 1（最高） | 命令行参数 `--data-dir` / `--model-root` | `cli` | 一次性排障、多实例 |
| 2 | 环境变量 | `env` | 运维统一管控、便携版、E2E |
| 3 | 用户在设置页选的值 | `user` | 日常使用 |
| 4 | 内置默认 | `default` | 首次安装 |

**这不是「静默接管」**：每个路径都必须能回答三个问题 —— 当前生效值是什么、来自哪一层、用户怎么改。方案由四部分组成。

### 4.2 可发现：来源徽章

设置页（「数据存储」「模型与网络」两处）在路径旁常驻显示：

```
模型目录   D:\mandaryn-models                       [环境变量]
           ~/.mandaryn（你的设置）← 未生效
```

- 徽章取值：`默认` / `你的设置` / `环境变量` / `命令行`；
- 被上层覆盖时，**同时显示下层值**并标注「未生效」，避免用户以为自己设置失败；
- 提供只读的生效值 + 「复制」按钮（用于填防火墙白名单、备份脚本）。

> 契约：`API-9.11 GetResolvedPaths` 返回每个路径的 `ResolvedPath`
> （`{key, effective, source, userValue, pendingValue, envName, overrideHint, writable, fallbackFrom}`），
> 字段语义见 DOC-03 §2.9.3。

### 4.3 可暂存：不让用户白操作

处于 `env`/`cli` 接管下时，设置页的「更改目录」**不置灰**，而是允许保存，产物写入暂存字段：

| 状态 | 点「更改目录」后发生什么 |
|------|--------------------------|
| 未被接管（`user`/`default`） | 正常流程：即刻生效（`modelRoot`）或提示重启（`dataRoot`） |
| 被 `env`/`cli` 接管 | 写入 `app_settings.model_root_pending`，提示「**已保存，待环境变量解除后生效**」；当前生效值不变 |

这样既尊重运维的强制值，又不让用户在受管环境里做无效操作、也不会误以为软件坏了。

### 4.4 可解除：一键说明 + 命令

「解除接管」按钮不擅自改系统环境，只给出**可复制的精确命令**（`overrideHint`）：

| 平台 | 操作 |
|------|------|
| Windows（用户级） | `[Environment]::SetEnvironmentVariable('MANDARYN_MODEL_ROOT', $null, 'User')` |
| Windows（系统级，需管理员） | `[Environment]::SetEnvironmentVariable('MANDARYN_MODEL_ROOT', $null, 'Machine')` |
| macOS / Linux（zsh） | 从 `~/.zshrc` 删除对应 `export` 行，然后 `exec zsh` |
| macOS / Linux（bash） | 从 `~/.bashrc` / `~/.profile` 删除对应 `export` 行 |
| systemd 服务 | 编辑 unit 的 `Environment=` 行后 `systemctl --user daemon-reload` |

操作完成后需**重启应用**（`dataRoot` 必须重启；`modelRoot` 也走重启，以保证与启动自检一致）。
解除后若存在 `*_pending` 值，则自动应用该暂存值并提示结果。

### 4.5 失效回退：环境变量指向不可用路径时

强制值不可用（目录不存在且无法创建、不可写、网络盘掉线）时，**不静默失败、不空跑**：

```
env 指定的 modelRoot 不可用
  → 记录 logx 警告（含变量名与实际路径）
  → 回退到 user 设置值（若无则 default）
  → 设置页红字：「环境变量指向的路径不可用，已临时回退」
  → 允许「仍使用该路径（重试）」——用于网络盘恢复后再试
```

`dataRoot` 回退必须额外提示数据归属：「**当前数据来自 `<实际路径>`**」，
因为切 `dataRoot` 等于换一整套会话库，用户最容易在这里误判「会话不见了」。

### 4.6 运维视角（受管部署）

```bash
# 企业统一管控：三个变量同时固定，用户无需配置
MANDARYN_DATA_DIR=/srv/mandaryn/data
MANDARYN_MODEL_ROOT=/srv/mandaryn/models
MANDARYN_OFFLINE=1
```

| 检查 | 期望 |
|------|------|
| 设置页两个路径均显示「环境变量」徽章 | ✅ |
| 用户改目录后显示「已保存，待解除生效」，生效值不变 | ✅ |
| 故意把变量指向只读目录 → 启动仍成功并回退 + 红字 | ✅ |
| 解除变量并重启 → 自动应用暂存值 | ✅ |

### 4.7 全局 HTTP 代理（`MANDARYN_HTTP_PROXY`）

企业网络里访问 GitHub Releases / 镜像站通常必须经代理。这里提供**一个**统一入口，
**模型下载**与**模型清单源获取**共用同一份设置 —— 不按用途分别配置，也不存在
「清单能拉下来、模型下不动」这种半通状态（两者走同一个 `Manager.httpClient()`）。

| 项 | 说明 |
|----|------|
| 变量 | `MANDARYN_HTTP_PROXY`（必填项为空 = 不启用） |
| 格式 | 标准 URL：`scheme://[user:pass@]host[:port]` |
| scheme | `http`（默认）、`https`（**以 TLS 连接代理**，CONNECT over TLS）、`socks5`（含 `socks5h` 别名） |
| 省略 scheme | `host:port` 按 `http` 处理（**不会**静默回落直连） |
| 内嵌凭据 | 支持 `http://user:pass@host:port`；密码含 `@` `:` 等字符须按 URL 规则转义（如 `p%40ss%3Aword`） |
| 默认端口 | `http`→80、`https`→443、`socks5`→1080 |
| 直连例外 | `MANDARYN_HTTP_NO_PROXY`：逗号分隔，支持 `*`（全部直连）、精确主机、`.suffix`（该域及其子域） |
| 作用范围 | **模型下载 + 模型清单源获取**；在线转写仍由设置页「用于在线转写」开关独立控制（`FR-05-5`） |
| 优先级 | `MANDARYN_HTTP_PROXY` > 设置页 `proxy_*` > 直连 |
| 生效时机 | 每次出站前重新读取该变量；环境变量在进程启动时定型，因此**修改变量后需重启应用**才生效（无需改任何配置项 / 重新登录） |
| 凭据保护 | 密码只在内存中明文存在；**不进日志、不进 `outbound_log`、不进错误详情/诊断包** |

**示例**

```bash
# 1) 普通 HTTP 代理
export MANDARYN_HTTP_PROXY="http://proxy.corp.lan:3128"

# 2) 带认证（密码里有特殊字符要转义）
export MANDARYN_HTTP_PROXY="http://alice:p%40ss%3Aword@proxy.corp.lan:3128"

# 3) 内网地址直连，其余走代理
export MANDARYN_HTTP_PROXY="http://proxy.corp.lan:3128"
export MANDARYN_HTTP_NO_PROXY=".corp.lan,localhost,127.0.0.1"

# 4) SOCKS5（DNS 交给代理解析）
export MANDARYN_HTTP_PROXY="socks5h://127.0.0.1:1080"

# 5) 不启用（保持原有直连行为）
unset MANDARYN_HTTP_PROXY
```

Windows（PowerShell）：

```powershell
[Environment]::SetEnvironmentVariable('MANDARYN_HTTP_PROXY', 'http://proxy.corp.lan:3128', 'User')
[Environment]::SetEnvironmentVariable('MANDARYN_HTTP_NO_PROXY', '.corp.lan,localhost', 'User')
```

**行为与排障**

| 场景 | 行为 |
|------|------|
| 变量未设 / 为空串 | 完全保持原有行为：直连，或按设置页代理配置出站 |
| 地址非法（未知 scheme / 无主机 / 端口越界 / 带 path 或 query / 只有密码没用户名） | **不静默直连**：启动时记一条 `WARN`（`env=MANDARYN_HTTP_PROXY`，不含凭据与 URL）；下载请求返回 `ERR_DOWNLOAD_PROXY`；清单获取因三级回退对外报 `ERR_DOWNLOAD_NETWORK`（附缓存或内置清单，界面不空白），代理错误保留在错误链里 |
| 代理不可达 / 认证失败 | `ERR_DOWNLOAD_PROXY`；清单获取按三级回退给出缓存或内置清单并附 `ERR_DOWNLOAD_NETWORK`，界面不会空白 |
| 同时开了 `MANDARYN_OFFLINE=1` | 离线优先，请求在发出前就被拒绝（`ERR_OFFLINE_ENFORCED`），代理不生效 |
| 只改了密码（主机/用户不变） | 代理 client 会重建，不会继续用旧凭据 |

> **为什么「非法即失败」而不是回落到直连**：静默直连会让运维以为流量已过代理，
> 实际裸奔出网 —— 与「代理密码坏密文不静默直连」同一条原则。

---

## 5. 日志与诊断

### 5.1 日志策略

两类日志都在 `<dataRoot>/logs/`（与数据库、密钥同根；`MANDARYN_DATA_DIR` 时落在该目录）：

| 项 | 运行日志 `app.log` | 调试日志 `debug.log`（调试模式） |
|----|-------------------|----------------------------------|
| 何时写 | 始终 | 仅调试模式开启时（设置 → 隐私边界 → 调试模式，默认关闭） |
| 级别 | `INFO` 及以上 | 全部级别（`DEBUG` 起），另含前端 error / warn / 未捕获异常 |
| 格式 | `slog` JSON | JSON Lines：`time`、`level`、`source`（仅后端，仓库相对 `file:line`）、`msg`、`component`、`origin`（`backend`/`frontend`），其余为上下文字段 |
| 滚动 | 单文件 8 MB，当前 + 3 份 | 单文件 10 MB，当前 + 4 份（`debug.log.1` … `.4`，总量约 50 MB） |
| 清理 | 滚动时覆盖最旧的 | 启动与每次滚动时删除修改时间超过 7 天的文件；设置页可一键清空 |
| 开关 | — | 用户层 `app_settings.log_level`（开 = `debug`）；`MANDARYN_LOG_LEVEL` 覆盖；切换**立即生效，无需重启** |

- **关闭调试模式**：立即停写并释放文件句柄；已有调试日志保留，可在设置页查看器、诊断包或文件管理器里读取。
- **用户开关的生效时机**：数据库打开后立刻应用，因此启动最早阶段（打开数据库之前）只有用 `MANDARYN_LOG_LEVEL=debug` 才会记录。
- **`log_level=off`**：视同「调试模式关」，不会关闭 `app.log`。
- **脱敏铁律**：禁止写入音频字节、转写正文、密钥、代理密码、完整 URL（只写 host）；违规视为缺陷。
  调试日志在写入前另有一层兜底脱敏（`privacy.SanitizeDebugAttr`，规则见 API.md §2.10.1），未注入脱敏器时拒绝开启。

### 5.2 诊断包导出

> 现状：「设置 → 隐私边界 → 导出诊断包」生成单个脱敏 JSON（`mandaryn-diagnostics-<ts>.json`），
> 含 `app.log` 与调试日志的尾部（各至多 512 KB，进包前脱敏）。下面的 `.zip` 结构是目标形态。

「设置 → 数据存储 → 导出诊断包」生成 `.zip`：

```
diagnostics-20261110-140312.zip
  system.json        # OS/版本/CPU/内存/架构/CGO 标记
  app.json           # 应用版本、引擎与模型包清单、能力探测结果
  metrics.json       # 最近 20 次会话的 FFD/SLD 分位（脱敏，无文本）
  logs/*.log         # 最近 2 天（已脱敏）
  db-schema.json     # 表结构与计数（无行内容）
```

**明确不含**：音频、转写文本、声纹向量、凭据。

### 5.3 崩溃处理

崩溃写 `<dataRoot>/logs/crash-<ts>.log`（仅调用栈 + 版本，无正文）；下次启动检测到则提示「上次异常退出，是否导出诊断包」。
不做任何自动上传（NFR-03-7）。

---

## 6. 升级与回滚

### 6.1 升级前（应用自动执行）

```
1. 检查磁盘空间（≥ 500MB）
2. 备份数据库：mandaryn.db → mandaryn.db.bak-<version>（保留最近 2 份）
3. 记录 modelRoot 与已装 bundle 清单
4. 应用版本比对 → 若需要，执行一次性迁移钩子（data_migrations 幂等）
5. AutoMigrate（加表/加列/加索引）
6. 校验：模型包 `state`、`audio_path` 存在性、FTS 重建（如 schema 变更）
```

### 6.2 兼容性规则

| 变更 | 前向兼容 | 回滚影响 |
|------|----------|----------|
| `PATCH` | ✅ | 无 |
| `MINOR`（加表/加列） | ✅（旧版忽略新列） | 旧版可运行，但新字段丢失（可接受） |
| `MAJOR`（改列/搬迁数据） | ⚠️ 单向 | **不可降级**，必须用 `mandaryn.db.bak-*` 恢复 |

### 6.3 回滚步骤（运维）

```bash
# 1) 退出应用
# 2) 恢复数据库备份
cp "$DATA/mandaryn.db.bak-<旧版本>" "$DATA/mandaryn.db"
rm -f "$DATA"/mandaryn.db-wal "$DATA"/mandaryn.db-shm
# 3) 安装旧版安装包（覆盖安装，保留 dataRoot）
# 4) 启动 → 自检 → 抽查最近 3 个会话可回放
# 5) 模型包无需回滚（manifest schema 向后兼容 v1）
```

**模型清单 schema**：固定 `schemaVersion: 1`；新增字段只增不改；旧版遇到未知字段必须忽略而非报错。

---

## 7. 模型分发

### 7.1 清单托管

```
GET https://<cdn>/mandaryn/manifest.json          # 应用在「模型」页刷新时拉取
{
  "schemaVersion": 1,
  "updatedAt": "2026-11-10T00:00:00Z",
  "bundles": [
    {
      "bundleId": "sensevoice-small", "version": "1.2.0",
      "name": "SenseVoice Small（中文）", "sizeBytes": 471859200,
      "sha256": "<整包>", "ramHintMb": 900,
      "languages": ["zh","en"], "license": "Apache-2.0",
      "roles": ["offline"],
      "files": [{ "path": "model.onnx", "sha256": "<文件>", "size": 470000000, "url": "..." }],
      "mirrors": ["https://<mirror>/sensevoice-small@1.2.0/"]
    }
  ]
}
```

| 要求 | 说明 |
|------|------|
| HTTPS + 完整性 | 清单自带 `sha256`；包内 `manifest.json` 二次校验（ARCH §4.6.2） |
| 签名 | 清单附 `signature`（Ed25519），公钥内置；签名无效 → 拒绝加载并提示 |
| CDN 约束 | 支持 `Range`（断点续传）、`ETag`/`If-Range`；单对象 ≤ 2GB |
| 镜像 | 每个 bundle 可列多个 `mirrors`，按顺序尝试（失败切下一个） |
| 版本共存 | `models/<bundleId>@<version>/` 并存，允许同时装两个版本 |
| 下架 | 清单移除 bundle 不影响已装用户；应用不主动删除 |

### 7.2 离线包

```bash
# 制作（发布侧）
zip -r sensevoice-small@1.2.0.zip sensevoice-small@1.2.0/ manifest.json
# 部署（用户侧）：设置 → 模型 → 导入离线包（API-6.13）
#   或直接解压到 <modelRoot>/models/
```

---

## 8. 企业 / 离线（air-gapped）部署

```bash
# 1) 目标机安装标准包（nollm 亦可，若允许在线则免）
# 2) 设置强制离线（禁止一切出站）
MANDARYN_OFFLINE=1
# 3) 导入离线模型包（U 盘/内网共享）
# 4) 验证无出站：看「设置 → 隐私 → 最近出站点」应为空，或用防火墙/抓包复核
```

| 检查 | 期望 |
|------|------|
| 启动后 5 分钟内无出站连接 | ✅（Wireshark / `Get-NetTCPConnection`） |
| 模型页显示「离线模式，已禁用下载」 | ✅ |
| 尝试切在线 → 被拒（`MANDARYN_OFFLINE`） | ✅ 且提示原因 |
| 会话 `model_source=local` 且无 degrade 记录 | ✅ |

---

## 9. 测试与验收

| 层级 | 覆盖 | 命令 |
|------|------|------|
| 单测 | 状态机（9 态全转换）、迁移四阶段与回滚、时间轴二分、哈希校验、代理配置解析 | `go test ./internal/...` |
| 契约 | 每个 service 方法签名 + 错误码与 DOC-03 一致（生成 bindings 后编译即验证） | `pnpm typecheck` |
| E2E | UC-01（首次）、UC-02（录制→回放）、UC-03（源异常）、UC-06（迁移失败回滚） | `cd frontend && pnpm e2e` |
| 性能基准 | FFD/SLD 分位（20 次会话）、回放高亮误差、列表首屏 | 脚本 + `metrics.json` |
| 隐私验证 | 本地模式抓包无出站；日志 grep 无正文/密钥 | 手工 + CI 断言（模式匹配） |
| 兼容性 | Win10 2004/20348+/Win11、macOS 14.2/15、Linux PipeWire/Pulse | 真机矩阵 |

**性能基准门限**（与 NFR-01 一致，任一不达标不得发布）：
FFD-engine 本地 P50 ≤ 1000ms / P95 ≤ 1500ms（起点=首个非静音帧）；FFD-e2e 仅观测不设门限；
在线 P50 ≤ 1200ms / P95 ≤ 2000ms；
SLD 本地 P95 ≤ 1500ms / 在线 P95 ≤ 2500ms；回放同步误差 ≤ 80ms。

---

## 10. 发布检查清单

| # | 检查 | 依据 |
|---|------|------|
| 1 | `task build` + `task package` 三平台均绿 | §2.2 |
| 2 | 绑定为 `.ts` 且 `pnpm typecheck` 通过 | API.md §7 |
| 3 | `--self-check` 三平台均通过 | §2.4 |
| 4 | 单测/E2E 全绿 | §9 |
| 5 | 性能门限达标（记录到 `metrics.json`） | §9 |
| 6 | 隐私：本地模式零出站、日志无正文 | §9 |
| 7 | 升级路径验证（旧版库 → 新版）与回滚验证 | §6 |
| 8 | 模型清单签名有效、镜像可下载、断点续传可用 | §7 |
| 9 | **文档版本联动**：本次变更涉及的文档已升版，DOC-INDEX §2.2 规则满足 | DOC-00 §2.2 |
| 10 | 术语一致性：新增 UI 文案与 DOC-00 §3 术语表一致 | DOC-00 §3 |
| 11 | 路径接管四态可见：默认/我的设置/环境变量/命令行徽章与实际 `source` 一致，受管时改目录写暂存且不报错（ADR-02） | §4.2、§4.3 ↔ API-9.11/9.12 |
| 12 | CGO 构建在 CI 三平台均可用；`nollm` 作为独立可选产物可构建（`task build:nollm`，裁剪体积/依赖，Windows 实测可 `CGO_ENABLED=0`，其它平台**不承诺**） | §2.1、§2.2.1 ↔ ADR-01 |

---

## 11. 故障排查

| # | 症状 | 原因 | 处理 |
|---|------|------|------|
| 1 | 前端报 `@bindings/...` 找不到 / 类型为空 | 用了非 v3.1.0 的 `wails3`，或漏 `-ts`，生成了 `.js` | 装 v3.1.0 CLI，`wails3 generate bindings -ts -clean=true` |
| 2 | `go build` 报 `function main is undeclared`（`build/ios`） | 上游模板自带的 iOS 专用包，只在 `go build ./...` 时暴露 | 用 `go build ./internal/...` 或 `go build .`；非缺陷 |
| 3 | `go: downloading ... dial tcp timeout` | `proxy.golang.org` 不可达 | 设 `GOPROXY=https://goproxy.cn,direct` |
| 4 | `build cache is required, but could not be located` | 环境无 `GOCACHE`/`%LocalAppData%` | `export GOCACHE="$HOME/.cache/go-build"` |
| 5 | 进程级捕获不可用（自动降级设备回环） | Windows < Build 20348，或 macOS < 14.2 | 提示用户升级，或接受整机混音（FR-02-3） |
| 6 | 录制中「未检测到音频」但应用在播 | 应用使用 WASAPI 独占模式 | 改用「全部应用声音」（ARCH §4.1.3） |
| 7 | 下载总在 99% 失败 | 镜像不支持 `Range` 或 ETag 变动 | 换镜像；确认 `If-Range` 语义（ARCH §4.6.2） |
| 8 | 迁移后提示「模型缺失」 | 目标为网络盘，rename 非原子 | 迁移到本地磁盘；网络盘仅支持「复制模式」（不删源） |
| 9 | 说话人改名后旧会话仍显示旧名 | 前端缓存了 `RoleName`，未订阅 `roles:changed` | 前端修复：订阅 EV-07 并重取 |
| 10 | 回放高亮偏移数百毫秒 | 用了 wall clock 或 `timeupdate` 驱动 | 按 ARCH §4.5（帧计数 + rAF）实现 |
| 11 | 密钥/日志泄露 | 未走 `cryptox` / `logx` 脱敏包装 | 回归 NFR-03-4/5，加 CI 断言 |

---

## 12. 变更记录

| 日期 | 版本 | 变更 | 作者 |
|------|------|------|------|
| 2026-11-10 | v1.0.0 | 首版：交付形态、CGO 构建与 CI 矩阵、运行时目录、6 个环境变量、日志脱敏与诊断包、升级回滚（含 MAJOR 不可降级）、模型清单 schema 与签名、离线部署验证、测试门限、发布检查清单、11 条故障排查 | Kether |
| 2026-11-10 | v1.1.0 | 仅版本同步（`NFR-08` 引入术语与编号变更，按 DOC-00 §2.2 需全文档升版）；本文无内容改动 —— 性能预算为运行时行为，不影响构建、目录、环境变量与分发 | Kether |
| 2026-11-10 | v1.2.0 | 新增 §4.1–4.6 环境变量与命令行**接管路径的完整方案**（四级单向覆盖 `cli`>`env`>`user`>`default`、来源徽章、受管时写 `model_root_pending` 暂存、可复制的解除命令、失效回退与数据归属提示、运维受管部署验证清单）；§4 表格澄清 `MANDARYN_MODEL_ROOT` 与环境变量的优先级关系；CGO 措辞改为「既定基线（已决策，ADR-01）」，`nollm` 标注为**可选产物**而非 CGO 兜底；路径契约改引 `API-9.11 GetResolvedPaths` 与 `ResolvedPath`（对齐 DOC-03 §2.9.3） | Kether |
| 2026-11-12 | v1.2.1 | `nollm` **由设计变为已实现**（此前本文按「计划」表述）：新增 §2.2.1 说明 `task build:nollm` 的派生方式、产物命名、运行时 DLL 是否随包、离散符号表与实测体积（Windows 部署总体积约 44.2 MiB → 22.4 MiB，−49%；exe 本体仅小约 182 KiB，省下的体积在 3 个 sherpa-onnx DLL）；§1 交付物表把「无进程级捕获依赖库」改为「不含本地推理（sherpa-onnx / 声纹提取被 `!nollm` 排除，只能走在线引擎）」；发布清单第 12 条补 `task build:nollm` 与 Windows 实测 `CGO_ENABLED=0` 的边界（其它平台不承诺） | NarraFork |
