# S3（FR-01 模型来源与引擎路由域）开发问题记录

> 范围：`internal/asr`（含 `internal/asr/local`）与 `internal/services/enginesvc`。
> 只记 S3 实施过程中真实踩到的坑，每条都能复现或能指到具体代码位置。
> 状态取值：未解决 / 已绕过 / 已解决 / 改设计。

## S3-01 sherpa-onnx Go 绑定的实际 API 与「直觉写法」不一致

- 现象：按常见 Go 绑定习惯写 `stream.Decode()`、`recognizer.AcceptWaveform()` 都会编译失败。
- 复现条件：`grep -n "func (recognizer \*OfflineRecognizer) Decode" $HOME/go/pkg/mod/github.com/k2-fsa/sherpa-onnx-go-windows@v1.13.8/sherpa_onnx.go`
- 根因：`github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx` 包里**只有 3 个平台文件**，内容全是 `type X = sherpa.X` 类型别名 + `var NewX = sherpa.NewX` 变量转发，真正实现在 `sherpa-onnx-go-<os>`。真实签名是：
  - `NewOfflineRecognizer(cfg *OfflineRecognizerConfig) *OfflineRecognizer`（返回 nil 表示失败，不返回 error）
  - `stream.AcceptWaveform(sampleRate int, samples []float32)`、`stream.GetResult() *OfflineRecognizerResult`
  - `recognizer.Decode(stream)`（**Decode 在 recognizer 上**，不在 stream 上）
  - `DeleteOfflineRecognizer` / `DeleteOfflineStream` 必须手工调用，否则泄漏
  - `OfflineModelConfig` 是「一组子结构 + 一个 Tokens 路径」，库靠非空子结构判定模型类型
- 临时处置：全部按真实签名调用，模型类型识别改为纯文件名启发式（`internal/asr/bundle.go` → `DetectBundle`），再由 `internal/asr/local.buildConfig` 填对应子结构。
- 状态：已解决

## S3-02 `go test ./internal/asr/...` 直接 `exit status 0xc0000135`（无任何输出）

- 现象：加完本地引擎后测试二进制一行输出都没有，退出码 `0xc0000135`（STATUS_DLL_NOT_FOUND）。
- 复现条件：把 CGO 实现放在 `internal/asr` 包内，运行 `go test ./internal/asr/...`（DLL 不在测试工作目录）。
- 根因：CGO 实现 import 了 sherpa-onnx，测试二进制运行期需要 3 个 DLL 同目录/PATH：`sherpa-onnx-c-api.dll`、`sherpa-onnx-cxx-api.dll`、`onnxruntime.dll`。**链接能通过，运行才炸**，因此没有任何编译期提示。
- 临时处置：
  1. 把 CGO 实现拆成子包 `internal/asr/local`，`internal/asr` 保持纯 Go —— 路由/降级逻辑的单测不再被 DLL 绑架；
  2. 真实推理测试放在 `internal/asr/local/inference_test.go`，用 `//go:build windows && cgo && sherpa_models` 隔离，默认不参与 `go test`；
  3. 需要真跑时：`cp "$HOME/go/pkg/mod/github.com/k2-fsa/sherpa-onnx-go-windows@v1.13.8/lib/x86_64-pc-windows-gnu/"*.dll .` 再 `MANDARYN_TEST_MODEL_DIR=... go test -tags sherpa_models ./internal/asr/local/ -run TestRealInference -v`。
- 状态：已绕过（根治在打包环节：S9 必须保证 3 个 DLL 与 exe 同目录，属 DEPLOYMENT 范畴）

## S3-03 `GOOS=linux go build` + `CGO_ENABLED=1` 必然失败（darwin 不受影响）

- 现象：`CGO_ENABLED=1 GOOS=linux go build ./internal/asr/...` 报一堆 `grp.h: No such file or directory`、`'clearenv' undeclared`、`unknown type name 'sigset_t'`，全部来自 `runtime/cgo`；同一命令对 `GOOS=darwin` **能通过**（darwin 分支下本包没有 cgo 源文件，未触发 Linux 头文件依赖）。
- 复现条件：Windows 宿主 + `CGO_ENABLED=1` + `GOOS=linux`。
- 根因：CGO 交叉编译需要 Linux 交叉工具链；本机只有 mingw gcc，找不到 Linux 头文件。这与 `ADR-01`「不做单机交叉编译」一致。
- 临时处置：linux 的**可编译性**验证用 `CGO_ENABLED=0`（此时 `local` 走 `local_stub.go` 的不可用实现）：
  ```bash
  CGO_ENABLED=1 GOOS=darwin go build ./internal/asr/...   # 通过
  CGO_ENABLED=0 GOOS=linux  go build ./internal/asr/...   # 通过
  ```
  `enginesvc` 因为 import 了 Wails，`CGO_ENABLED=0` 需要额外加 `-tags purego`：
  ```bash
  CGO_ENABLED=0 GOOS=darwin go build -tags purego ./internal/services/enginesvc/...
  ```
- 状态：已绕过（非 Windows 产物里本地引擎返回 `ERR_LOCAL_UNAVAILABLE`，符合 `ADR-01`）

## S3-04 降级矩阵的「端点 5xx」没有专用错误码

- 现象：ARCH §4.2.4 矩阵里 5xx 是独立一行，但 `internal/apierr/apierr.go` 只有 `ERR_ENGINE_ONLINE_NETWORK / TIMEOUT / AUTH / RATE_LIMIT / REJECTED`。
- 复现条件：`grep -n "ERR_ENGINE_ONLINE" internal/apierr/apierr.go`
- 根因：错误码表（DOC-03 §5）尚未覆盖 5xx。
- 临时处置：5xx 复用 `ERR_ENGINE_ONLINE_NETWORK`（其默认语义 `retryable=true` + `action=check-network` 恰好吻合），同时在 `asr.FailureKind` 里保留独立的 `FailServer`，将来加码时只改映射一处。
- 状态：**已解决**（阶段收尾）。`apierr` 新增 `ERR_ENGINE_ONLINE_SERVER`（`retryable=true`、`action=switch-online`、
  message「online endpoint returned a server error」），`online.go` 的 5xx 分支改用该码并把
  `failureHint` 由 `unknown` 改为 `server`；DOC-03 §5 错误码表已补一行；对应单测断言同步更新。

## S3-05 任务书里的 `ERR_ENGINE_UNAVAILABLE` 在码表里不存在

- 现象：按任务描述给非 Windows 平台返回 `ERR_ENGINE_UNAVAILABLE`，但 `apierr` 里没有这个码。
- 复现条件：`grep -n "ERR_ENGINE_UNAVAILABLE" internal/apierr/apierr.go`（无结果）
- 根因：码表铁律是「错误码只在此处登记」，不允许业务自造字符串。
- 临时处置：改用现成的 `ERR_LOCAL_UNAVAILABLE`，其默认 `message` 正是 "this build does not include the local engine"，`action=switch-online` 也符合产品意图。
- 状态：改设计（不再新增码）

## S3-06 `app_settings` 没有 `max_retries` 列，`FallbackPolicy.MaxRetries` 无法持久化

- 现象：API-5.7 要求保存 `MaxRetries`，但 `storage.AppSettings` 只有 `FallbackToLocal` / `AutoSwitchBack` 两个布尔列。
- 复现条件：`grep -n "MaxRetries" internal/storage/models.go`（无匹配）
- 根因：DOC-04 TBL-02 当初只建了两列。
- 临时处置：`enginesvc.SetFallbackPolicy` 只把 `MaxRetries` 推给运行期（`router.SetPolicy`），重启后回到默认 2；范围校验仍然生效（0–10，越界 `ERR_INVALID_ARGUMENT`）。
- 状态：**已解决**（阶段收尾）。`storage.AppSettings` 新增 `MaxRetries int`（`not null;default:2`），
  `ensureSingletonSettings` 显式写非零默认值 2（避 GORM 零值覆盖 SQL 默认值的老坑）；
  `appsettings.SetFallbackPolicy` 落库该列、`Snapshot` 暴露 `MaxRetries`；
  `enginesvc.New` 启动时改读 `snap.MaxRetries`（不再用 `asr.DefaultMaxRetries`）。DOC-04 TBL-02 已补行。

## S3-07 `appsettings` 不暴露明文在线密钥，enginesvc 只能自己解密

- 现象：构造在线引擎需要明文 API Key，但 `appsettings.GetOnlineConfig` 只返回掩码，且该包本次禁止修改。
- 复现条件：读 `internal/services/appsettings/appsettings.go` 的 `GetOnlineConfig`，返回 `maskSecret(...)`。
- 根因：掩码是**对外**约束（NFR-03-3），缺一个「内部用途」的明文通道。
- 临时处置：`enginesvc.plainAPIKey` 直接读 `storage.AppSettings.OnlineAPIKeyEnc` 并用 `cryptox.DecryptString` 解密；解密只发生在内存、只用于拼 `Authorization` 头，绝不进日志/事件/错误 Detail。代理密码同理（`proxyURLFor`）。
- 状态：已绕过（建议 `appsettings` 增补一个仅供 L3 使用的明文读取方法，避免加密/掩码逻辑两处维护漂移）

## S3-08 429 的 `Retry-After` 拿不到响应头，只能退化为解析响应体

- 现象：`httpx.HTTPError` 只携带 `StatusCode / BodySnip`，不含 `Header`，因此标准做法「读 `Retry-After` 响应头」不可用。
- 复现条件：读 `internal/utils/httpx/httpx.go` 的 `HTTPError` 定义。
- 根因：`httpx.doJSON` 在返回前已经 `drain` 了响应，头信息被丢弃。
- 临时处置：`online.go` 绕开 `doJSON`，改用 `httpx.NewRequest` + 注入 client（仍是 httpx 路径，不用默认 client），自己读 body；`parseRetryAfter` 依次尝试 `retry_after` 顶层字段、`error.retry_after`、纯数字 body，最后钳到上限 30s（ARCH §4.2.4）。都拿不到时退回 1s/3s 默认退避。
- 状态：未解决（建议 `httpx` 增加「返回 status+header 摘要」的原语，或让 `HTTPError` 带 `Header`）

## S3-09 enginesvc 测试报 `ERR_DB_WRITE: %AppData% is not defined`

- 现象：测试里保存在线配置（要加密 API Key）失败，错误信息为 `%AppData% is not defined`。
- 复现条件：在 `APPDATA` 未导出的 shell（本机 Git Bash 里 `echo $APPDATA` 为空）运行 `go test ./internal/services/enginesvc/`。
- 根因：`cryptox.masterKeyDir()` 在没有 `MANDARYN_DATA_DIR` 且 `os.Getenv("AppData")` 为空时回落到 `os.UserConfigDir()`，后者在 Windows 上读 `%AppData%` 并报错。
- 临时处置：测试 `setup()` 里 `t.Setenv(paths.EnvDataRoot, t.TempDir())`，把密钥与库都指到临时目录，避免污染真实用户目录。
- 状态：已绕过（提请注意：任何依赖 `cryptox` 的新单测都要先设 `MANDARYN_DATA_DIR`）

## S3-10 本机没有已装模型包，真实推理链路未做端到端验证

- 现象：`find / -name tokens.txt -path "*sherpa*"` 逾 2 分钟无结果；`$APPDATA/Mandaryn` 不存在。
- 复现条件：全新机器、未下载模型包。
- 根因：S6（models 域）尚未实现，本机也没手工放过模型。
- 临时处置：`TestRealInference` 读 `MANDARYN_TEST_MODEL_DIR`，未设置时 `t.Skip` 并说明原因；`internal/asr/real_inference_skip_test.go` 在默认测试里显式打印「为何没跑」，避免误以为本地引擎已被真实验证。
- 状态：未解决（需下载任一 `sherpa-onnx-sense-voice-*` 或 paraformer 包后手工跑一次，并把结果补进本文件）

## S3-11 「同源再切」被定义为 no-op，强制重载走 Reload

- 现象：`SetSource("local")` 在当前已是 local 且 ready 时不重建引擎，直接返回当前状态。
- 复现条件：`router.SetSource` 在 `next == source && 已 ready` 时提前 return。
- 根因：API 文档没写「同源再切」的语义；若每次都重建，用户误点会白白付出 8s 加载 + 2s 卸载。
- 临时处置：同源切换 = 幂等 no-op；「换模型包后生效」用 API-5.3 `Reload`（强制重建，旧实例延迟 2s 卸载）。已写进 `Router.SetSource` 注释。
- 状态：改设计（建议 DOC-03 §2.5 补一句语义说明）

## S3-12 enginesvc 还没有在 `internal/app` 里注册

- 现象：`internal/app/app.go` 里没有 `enginesvc.New(...)`，服务方法暂时不会被 Wails 暴露。
- 复现条件：`grep -n "enginesvc" internal/app/app.go`（无匹配）
- 根因：本次任务明确禁止修改 `internal/app/app.go`；且装配还需要 `BundleLocator`（S6 的 models 包）才能给本地引擎工厂。
- 临时处置：`enginesvc.New(holder, settings, router, recorder, activeSession)` 签名已按「可被 app 直接调用」设计；本地工厂用 `local.NewFactory(locator)`，等 S6 的 locator 落地后接线。
- 状态：未解决（S9 装配阶段处理）

## S3-13 「边录边转」的流式路径未实现

- 现象：`local.Feed` 与 `online.Feed` 都返回**已关闭**的空 channel。
- 复现条件：读 `internal/asr/local/local_windows.go` 与 `internal/asr/online.go` 的 `Feed` 注释。
- 根因：延后转写（默认）用 offline recognizer，不需要流式；`FR-03-10`「边录边转」默认关闭，需要 `RoleStreaming` 模型包 + `OnlineRecognizer`，属后续开关。
- 临时处置：接口保留，实现显式返回空流并在注释写明归属；上层（S4 recording）在延后模式下不会调用 `Feed`。
- 状态：未解决（`FR-03-10` 落地时补 `internal/asr/streaming` 思路）
