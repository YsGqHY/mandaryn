# Wave B 开发问题记录（S1 捕获 / S3 引擎 / S6 模型仓库）

> **用途**：本阶段开发过程中遇到的每个问题**即时登记**在此（现象 → 复现条件 → 根因 → 临时处置），
> 阶段结束时逐条回看并确认已解决或有明确结论。
>
> 状态取值：`未解决` / `已绕过` / `已解决` / `改设计`（不改代码，改设计说明）

---

## ENV-01 CGO 完全不可用：gcc 静默退出 1

- **现象**：任何带 cgo 的构建都失败，`go build` 只报
  `runtime/cgo: ...cgo.exe: exit status 2`，没有任何 gcc 报错细节。
- **复现条件**：
  1. `printf 'package main\nimport "C"\nfunc main(){ _ = C.int(1) }\n' > t.go && go build t.go` → 失败；
  2. 单独执行 `gcc -E empty.c` → **exit 1 且 stdout/stderr 全空**；
  3. `gcc --version` / `gcc -c x.c` 看起来又"正常"（`-c` 其实也失败，之前的 exit=0 是被管道 `| head` 吃掉的假象）。
- **根因**：
  - `gcc -v -E` 显示驱动正常，真正失败的是它调起的 `cc1.exe`（`J:/msys2/ucrt64/lib/gcc/x86_64-w64-mingw32/16.1.0/cc1.exe`）。
  - `ldd cc1.exe` 显示 ucrt64 的 cc1 **加载了 mingw64 的 DLL**：
    `libgmp-10.dll`、`libgcc_s_seh-1.dll`、`libwinpthread-1.dll`、`zlib1.dll` 全部来自 `/mingw64/bin`，且用的是
    `msvcrt.dll`（mingw64 的 CRT）而不是 `ucrtbase.dll`（ucrt64 的 CRT）。
  - 触发顺序原因：`PATH` 中 `/mingw64/bin` 排在 `/j/msys2/ucrt64/bin` **之前**，Windows DLL 搜索按 PATH 命中后者的
    同名库 → ucrt64 编译器套 mingw64 运行库 → ABI 不匹配 → cc1 静默崩溃（无输出，仅非零退出码）。
  - 这也解释了为什么 S0 一直没发现：S0 的代码（modernc SQLite 等）全是纯 Go，从未真正编译过 C。
- **临时处置**：构建前把 ucrt64 前置到 PATH：
  ```bash
  export PATH="/j/msys2/ucrt64/bin:$PATH"
  ```
  验证：最小 cgo 程序 `go build` 通过并可运行；sherpa-onnx 探针链接通过。
- **待办**：写入 Taskfile / 文档，让 `go build`、`wails3 build` 都自动带上正确 PATH（否则任何人第一次带 cgo 构建都会踩）。
- **状态**：**已解决**。已写入 [DEVELOPMENT.md](./DEVELOPMENT.md) §1.1（自查命令 + 修复 + `ldd` 复核三步），
  并把 `build/windows/Taskfile.yml` 的 `CGO_ENABLED` 默认值由 `0` 改为 `1`（与 ADR-01 对齐，
  否则引入 sherpa-onnx 后 `task build` 会直接编译失败）。仍属**开发者环境前提**，无法由代码自动化。

---

## ENV-02 sherpa-onnx 运行期缺 DLL（exit 127）

- **现象**：`sherpa-onnx-go-windows` 链接成功后，二进制一运行就退出 127，无任何输出。
- **复现条件**：`go build` 一个只 `import "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"` 的程序，直接运行产物。
- **根因**：该模块提供的是**导入库**（`lib/x86_64-pc-windows-gnu/*.dll.a` 形式），运行时依赖
  `sherpa-onnx-c-api.dll`、`sherpa-onnx-cxx-api.dll`、`onnxruntime.dll` 三个 DLL；`ldd` 明确报
  `sherpa-onnx-c-api.dll => not found`。
- **临时处置**：把
  `$GOMODCACHE/github.com/k2-fsa/sherpa-onnx-go-windows@<ver>/lib/x86_64-pc-windows-gnu/*.dll`
  拷到 exe 同目录后运行正常。
- **待办**：构建流水线必须自动拷贝这三个 DLL（开发运行时也要拷，否则 `wails3 dev` 起不来）；发版清单里补一条。
- **状态**：**已解决**。新增 `build/windows/Taskfile.yml` 的 `copy:runtime:dlls` 任务，挂在 `build:native`
  的 `go build` 之后自动拷贝（`wails3 dev` 内部也走 `build:native`，因此开发期一并覆盖）；
  同步写入 [DEVELOPMENT.md](./DEVELOPMENT.md) §1.2 说明症状与手工补救方式。

---

## ENV-03 `wails3 generate bindings` 在 Windows 上首次改名失败

- **现象**：生成 bindings 报
  `failed to install new bindings ...: rename .bindings-tmp-xxx frontend\bindings: Access is denied`，
  并提示「Old bindings have been removed. Re-run the command to regenerate them.」。
- **复现条件**：前一次生成的 `frontend/bindings` 存在时执行 `wails3 generate bindings -ts`。
- **根因**：工具先删除旧目录再把临时目录 rename 成 `bindings`；Windows 上目录删除有延迟（句柄/索引未完全释放），
  rename 与删除竞争导致 access denied。属工具在 Windows 上的时序缺陷，非本仓库代码问题。
- **临时处置**：再跑一次即可成功（旧目录已被删除，第二次 rename 没有竞争者）。
- **状态**：已绕过（重跑）。

---

## ENV-04 `go build ./...` 因 `build/ios` 失败

- **现象**：`go build ./...` 报 `build/ios` 相关错误（带 ios build tag、无 `func main`）。
- **根因**：脚手架自带的 iOS 资源目录里有 `.go` 文件被 `./...` 扫到，属**既有问题**，与本次改动无关。
- **临时处置**：统一用 `go build ./internal/... .` 做后端验证。
- **状态**：已绕过（改用显式包列表）。

---

## ENV-05 `storagesvc` 导出 `func` 类型方法 → 绑定生成警告

- **现象**：`wails3 generate bindings -ts` 每次提示
  `package .../services/storagesvc: function types are not supported by encoding/json`，
  且该方法会以畸形签名进入前端 API。
- **根因**：`storagesvc.Service` 上有导出的 `SetOnPathChanged(fn func())`，绑定生成器会扫所有导出方法。
- **处置**：改为构造注入 `New(holder, cfg, onPathChanged func())`，调用点在 `internal/app/app.go`；
  方法数 50 → 49，警告消失。
- **状态**：已解决。

---

## ENV-06 主题 hook 文件只改了内容没改文件名

- **现象**：前端 `pnpm typecheck` 报
  `src/styles/themes/index.ts(27,36): Cannot find module './useMandarynPalette'`。
- **根因**：上一轮的批量改名只替换了文件**内容**里的标识符，`useFoundationPalette.ts` 这个**文件名**没跟着改，
  而 `index.ts` 的 export 路径已指向新名。
- **处置**：`mv useFoundationPalette.ts useMandarynPalette.ts`。
- **状态**：已解决。

---

---

# 阶段收尾：三份问题记录逐条回看结论

本阶段共登记 **48 条**问题（S1 17 / S3 13 / S6 18），逐条回看后的处置分布与结论如下。

## 一、本次收尾已解决（5 条）

| 编号 | 问题 | 解决方式 |
|------|------|----------|
| S1-09 | 默认设备标记硬编码中文后缀，违反 i18n 铁律 | `contract.Source` 增 `IsDefault bool`；`internal/capture` 删除 `DefaultDeviceSuffix` 常量与 `displayName(isDefault)` 参数；前端 `RecordPage` 用 `t('record.sources.defaultDevice')` 渲染徽标；测试改断言结构化字段唯一性 |
| S1-17 | `go-wca`/`go-ole` 仍标 `// indirect` | `go mod tidy` 后已移入直接依赖块（与 S6-18 一并处理） |
| S3-04 | 端点 5xx 无专用错误码 | 新增 `ERR_ENGINE_ONLINE_SERVER`（可重试 + `action=switch-online`），`online.go` 5xx 分支改用它，`failureHint` 由 `unknown` 改 `server`；DOC-03 §5 补行；单测同步 |
| S3-06 | `max_retries` 无法持久化 | `AppSettings` 增列（`not null;default:2`）、`ensureSingletonSettings` 显式写 2、`appsettings` 落库并进快照、`enginesvc.New` 启动读库值；DOC-04 TBL-02 补行 |
| S6-18 | `golang.org/x/net` 仍标 `// indirect` | 同上 `go mod tidy` |

## 二、开发中已自行解决/绕过（本次未再动）

S1-01~06、S1-12~16（go-wca 封装缺陷、COM 初始化语义、HRESULT 常量、结构体布局、PCM 队列策略、Toolhelp32 细节等）
以及 S3-01、S3-09、S6-03~07、S6-11~16（sherpa API 实际形态、加密测试需设数据目录、ETag 清理、Windows rename 语义、
清单缓存并发、vet 格式串、交叉编译限制、校验结论粘住、`.trash` 落点、测试间缓存干扰、测试构造失败、urandom）——
均已在各自条目记录根因与修法，代码里留有注释，不再重复。

## 三、按设计接受、不改代码（改设计）

- **S1-07 进程级捕获未实现**：需 `ActivateAudioInterfaceAsync` + `AUDIOCLIENT_ACTIVATION_PARAMS`（go-wca 未暴露），
  且本机 Build 19045 < 官方要求 20348，**本机不可验证**。已按「诚实能力声明」处理：`ProcessLoopback=false`、
  Reason 含最低版本与实际版本、`Open` 对应用源明确拒绝而**不静默降级**（避免隐私事故）。开关常量 `processLoopbackImplemented` 已留。
- **S1-08 应用源依赖活跃音频会话**：Windows 无「潜在音频应用」API，属平台能力边界；用 `ProbeSource` 按进程树区分
  「未运行」与「运行但无声」弥补。
- **S1-10 / S3-03 / S6-07 Linux·darwin 与交叉编译**：非 Windows 采集与本地推理按 `ADR-01` 不做单机交叉编译，
  平台桩诚实返回 `ERR_CAPTURE_UNAVAILABLE` / `ERR_LOCAL_UNAVAILABLE`，能力矩阵不虚报。
- **S1-11 枚举开销 24ms**：2s 轮询约 1.2% 占空比，判定可接受；注释写明优化方向（先缩设备范围，不降轮询频率）。
- **S3-05 `ERR_ENGINE_UNAVAILABLE` 不存在**：改用现成 `ERR_LOCAL_UNAVAILABLE`，不新增码。
- **S3-11 同源再切为 no-op**：强制重载走 `Reload`，语义更清晰。
- **S6-08 无 `ERR_DOWNLOAD_PRECHECK`**：复用 `ERR_DOWNLOAD_DISK_FULL` / `ERR_PATH_UNWRITABLE`，不新增码。
- **S6-10 `SizeBytes` 双重含义**：`BundleInfo` 是下载体积、`InstalledBundle` 是磁盘占用，已在字段注释与 DOC 中区分。

## 四、遗留（需后续阶段决策或环境）

| 编号 | 遗留内容 | 归属 |
|------|----------|------|
| S3-08 | `httpx.HTTPError` 不带响应头，429 的 `Retry-After` 只能退化解析响应体 | 建议 `httpx` 增加「status+header 摘要」原语（S9 隐私/出站收口时一并做） |
| S3-10 | 本机无已装模型包，真实离线推理未做端到端验证 | 需下载任一 `sherpa-onnx-sense-voice-*` 包后手工跑（已留 `-tags sherpa_models` 入口与命令） |
| S3-13 | 「边录边转」流式路径未实现 | `FR-03-10` 落地时补 `internal/asr/streaming`（默认关，不阻塞当前阶段） |
| S6-09 | API-6.1 的 `(list, ERR_DOWNLOAD_NETWORK)` 双返回在 Promise 语义下丢列表 | 已用 `fallbackTTL=30s` 缓解；根治需改契约（清单来源作为字段），待 DOC-03 决策 |
| S6-17 | 下载终态只能靠事件，无轮询兜底 | 建议补只读查询方法 `GetDownloadState`，属 API 变更，待决策 |
| S3-07 | `appsettings` 不暴露明文密钥，`enginesvc` 自行解密 | 已绕过且不落日志；建议 `appsettings` 增「仅供 L3」的明文读取方法，消除两处维护漂移 |
