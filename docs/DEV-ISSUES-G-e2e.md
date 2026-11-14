# DEV-ISSUES — G-3（E2E 主流程与异常场景）

> 本次开发范围：`frontend/e2e/**`、`frontend/playwright.config.ts`。
> **未改动**任何 Go 代码、`frontend/src/**`、`frontend/bindings/**`。
> 对应 `ROADMAP` §14.1 的两条：产品主流程 E2E、异常场景 E2E。
> 日期：2026-11-12

## 结论速览

| 场景 | 结论 | 用例 |
|------|------|------|
| 录制 → 生成录音实例（真实 WAV + 库行） | ✅ 自动化（录制期间页面自播测试音） | `product-flow.spec.ts`「录制设备回环…」 |
| 发起转写（无模型 → 失败路径） | ✅ 自动化 | `product-flow.spec.ts`「无模型时发起转写…」 |
| 回放高亮 / 点句跳转 | ✅ 自动化（片段为夹具） | `playback-highlight.spec.ts` |
| 崩溃恢复（强杀 → 重启 → 可回放） | ✅ 自动化 | `crash-recovery.spec.ts`「强杀后端后重启…」 |
| 崩溃恢复（状态置 `interrupted` + WAV 头回填） | ✅ 自动化并通过（G-E2E-01 修复后由 `test.fail` 转真红） | `crash-recovery.spec.ts` |
| 目标应用退出 | ⛔ 不自动化（本机不支持进程级捕获 + 需杀用户进程） | `not-automatable.spec.ts` |
| 录制中设备切换 / 拔出 | ⛔ 不自动化（需改动系统音频设备） | `not-automatable.spec.ts` |
| 导出（另存为） | ⛔ 不自动化（server 模式无原生对话框） | `not-automatable.spec.ts` |
| 转写成功 → 真实台词回放 | ⛔ 不自动化（无模型包 + 无语音输入） | `not-automatable.spec.ts` |
| 边录边转 / 说话人分离 | ⛔ 不自动化（同样依赖模型包） | `not-automatable.spec.ts` |

运行方式：`cd frontend && pnpm e2e`（需先 `wails3 task build:server`）。

## E2E 结构（本次新增）

- **project 划分**：`shared`（复用 `playwright.config.ts` 的 `webServer`，跑既有 `floating-dock` / `screenshots`）与 `isolated`（用例自己用 `e2e/serverctl.ts` 拉起 server 进程，各自占不同端口与数据目录）。
- **数据目录**：共享 `frontend/.e2e-data`；自管用例 `frontend/.e2e-data/{product,crash,playback}`。全部位于被 `.gitignore` 忽略的 `.e2e-data` 之下 —— 用例会录进真实设备回环音频，不能留在仓库里。
- **启动器**：`e2e/serve.mjs` 负责「清空数据目录 → 启动二进制 → 转发信号」。
- **素材**：回放高亮用真实中文语音 `zh.wav`（16kHz/单声道/5.592s，取自本机临时目录）；找不到时该用例 `test.skip` 并说明，不凭空造静音冒充。

## G-E2E-01（**已修复**）启动自检没有接线：崩溃后会话状态与 WAV 头都不会被修补

- 现象：录制中强杀后端 → 重启后会话仍显示在库中、音频也能播，但库里 `status` 仍是 `recording`、`duration_ms` 仍是 0，且 WAV 头里 `data` 长度仍是 0（正常停止才会 `Seal()` 回填）。
- 复现条件：`crash-recovery.spec.ts` 第二条用例（`test.fail` 标注）。
- 根因：`RecoverUnfinished` / `RecoverSession` 与 `RepairWAV` 都已实现（`internal/recording/recover.go`、`writer.go`），`recordingsvc.RecoverUnfinished`（`internal/services/recordingsvc/recordingsvc.go:172`）也导出为绑定，但 **`internal/app/app.go` 中没有任何调用点** —— 启动时从不做自检扫描。
- 影响：
  1. 会话无法被识别为「异常结束」（`interrupted`），用户在库中看到的是一个永远「看起来还在录」的会话；
  2. 时长显示 0:00（元数据没回填），与音频实际长度矛盾；
  3. 回放之所以还能听，是因为 Chromium 容忍「`data` 长度 0 + 后面有数据」的残缺头 **按文件长度兜底**；换成样本数严格按头解读的播放器、或把文件交给第三方工具，就会是「0 秒空文件」。
- 建议修法（一行装配，不改本 E2E 任何断言）：在 `internal/app` 装配完成后（`recSvc` 就绪处）调用一次
  `recSvc.RecoverUnfinished(context.Background())`，与 `modelsSvc.Cleanup(...)` 同位置即可。
- 状态：**未解决**。E2E 已用 `test.fail()` 把这个契约钉住：接线后该用例会立刻报「Expected to fail, but passed」，提醒把 `.fail` 去掉。

## G-E2E-02 设备源会话标题里残留设备 ID 片段

- 现象：自动命名的标题形如 `2026-11-12 00:29 · 音频设备 e5854bb}`，尾部带着设备 ID 的碎片与一个多余的 `}`。
- 复现条件：用设备回环录一段（`product-flow` / `crash-recovery` 用例都会产生）。
- 根因（初步）：设备显示名从源 ID（`dev:{0.0.0.00000000}.{12931ccb-…-f49efe5854bb}`）里截取，截断位置没有去掉 `{…}` 包裹。
- 影响：仅影响观感与会话标题可读性；不影响录音、转写、回放。
- 处置（2026-11-13）：`recording.ResolveSourceWith(capture.Probe)` 在装配层注入，设备源自动命名改用系统友好名（如「扬声器 (HECATE G2000)」）；探测失败回落到去掉 `{}` 的短 ID（`音频设备 fe5854bb`）。单测见 `internal/recording/resolve_source_test.go`，`product-flow.spec.ts` 断言标题里不再出现 `}` 碎片。已有会话的标题存在库里，不会回溯修改。
- 状态：已修复

## G-E2E-03 无模型时提示「推理引擎尚未就绪，请稍后重试」，把用户导向无限重试

- 现象：全新数据目录（无任何模型包）点「发起转写」，界面显示「推理引擎尚未就绪，请稍后重试。」。
- 复现条件：`product-flow.spec.ts` 第二条用例。
- 根因：`transcriptsvc.requireEngine`（`internal/services/transcriptsvc/service.go:457`）在外层统一返回 `ERR_ENGINE_NOT_READY`，真正的 cause（如 `ERR_MODEL_NOT_FOUND`）只进 `detail`，前端按外层码取文案，于是丢掉了「去模型页下载」这条可行动信息。
- 影响：用户会反复重试而永远失败（本机没有模型时重试不可能成功）。
- 处置（2026-11-13）：`requireEngine` 在引擎未就绪时先补一次 `Warm`，仍失败且原因是 `ERR_MODEL_NOT_FOUND` / `ERR_MODEL_CORRUPT` 时把该码作为外层码透出，界面显示「当前模型不可用，请到「模型」页下载或切换。」。用例断言已收紧为这条文案。
- 状态：已修复

## G-E2E-04 Web Server 启动串在 Windows 上会留下 `-p` 脏目录并让第二次运行失败

- 现象：连续跑两次 `pnpm e2e`，第二次报 `Process from config.webServer was not able to start. Exit code: 1`；`frontend/` 下多出一个名为 `-p` 的空目录。
- 根因：原配置用 `rm -rf "$DATA_DIR" && mkdir -p "$DATA_DIR" && …`，而 Playwright 的 `webServer.command` 在 Windows 上由 `cmd.exe` 执行：`mkdir -p` 被当成「创建一个名为 `-p` 的目录」。首次运行成功（两个目录都建了），第二次 `mkdir` 因目标已存在而报错退出。
- 处置：改为 `node e2e/serve.mjs`（Node 里做 `rmSync` + `mkdirSync`），跨平台语义一致，不依赖 shell 工具。已删除遗留的 `-p` 目录。
- 状态：已修复

## 2026-11-13 复测新增（开发模式浏览器走查 + 完整 E2E）

开发模式：Vite（`pnpm dev`，9245）+ server 模式 dev 构建（`go build -tags server`，不带 `production`，以 `FRONTEND_DEVSERVER_URL` 代理 Vite），浏览器访问后端端口。

### G-E2E-10 录制页两条能力提示条被压成细条（已修复）

- 现象：「平台捕获能力」与降级警告只剩 34px 高的标题条，正文被裁掉（scrollHeight 133 / 87px）。
- 根因：`.body` 是可滚动的 flex 列，内容超出视口时子项参与收缩；Mantine Alert 是 `overflow:hidden`，flex 最小高度算成 0 被压扁（Paper 是 `overflow:visible`，不受影响）。
- 处置：`RecordPage.module.css` 加 `.body > * { flex-shrink: 0; }`。

### G-E2E-11 引擎从未预热：录制页永远「引擎加载中」，装了模型也转写不了（已修复）

- 根因：ARCH §4.2.2 要求启动时后台 `Warm`，但 `internal/app` 没有调用点；Router 初始状态 `loading` 一直不变，`requireEngine` 永远报 `ERR_ENGINE_NOT_READY`。Router 的初始来源也没读持久化的 `modelSource`。
- 处置：装配层按设置的来源构造 Router，并在 `application.New` 之后后台 `Warm`（失败只置 `failed` + 原因，本地失败不回退在线）；`requireEngine` 未就绪时补一次 `Warm`（模型是启动后才下载的也能直接转写）。录制页在引擎 `failed` 时显示原因（缺模型 → 引导去「模型」页）。

### G-E2E-12 没有任何应用发声时，设备回环 0 帧（已修复）

- 现象：本机静音时录 4 秒，`frames=0`，会话被判空录音；首轮 E2E 能过只是因为当时恰好在放音乐。
- 根因：WASAPI 共享模式回环只在端点上有活动渲染流时出包，静默时段直接从帧计数时间轴上消失。
- 处置：`capture_windows.go` 在回环期间对同一端点开一个只写一次静音缓冲的保活渲染流（`silentRender`，OBS 同款做法），打不开只降级为旧行为并记一条日志。录制类 E2E 用例在录制期间用页面播放 440Hz、增益 0.03 的测试音（`helpers.playTestTone`，spec 需 `test.use({ launchOptions: TEST_TONE_LAUNCH })` 去掉 Playwright 默认的 `--mute-audio`），不再依赖外部声音。

### G-E2E-13 空录音停止后提示「当前状态下不允许该操作」（已修复）

- 根因：空录音沿用 `ERR_STATE_FORBIDDEN`，前端按码取文案。
- 处置：后端给这种情况加动作键 `check-source`（`recording.EmptyRecordingAction`），前端据此提示「这次没有录到声音……会话已保留在会话库」。

### G-E2E-14 `capture:sources` 事件从未送达前端（已修复）

- 现象：后端日志每隔几秒刷 `data of type []contract.Source for event 'capture:sources' does not match registered data type`。
- 根因：事件注册为 `contract.CaptureSourcesChanged`，装配层却直接发 `[]Source`，Wails 校验失败后取消事件。
- 处置：改发 `contract.CaptureSourcesChanged{Sources: srcs}`。

### G-E2E-15 模型页：清单失败显示成「下载时网络中断」；清单源 404（部分修复）

- 已修复：清单失败不再进页头错误条，改在「可下载模型」卡片里说明「无法获取模型清单……」（`models.available.loadErrors.*`）。
- 未解决：`github.com/oberon-vip/mandaryn-models` 与 hf-mirror 对应仓库都返回 404，模型下载这条主流程目前走不通（需要先发布清单）。另外 API-6.1 约定「失败时返回缓存 + `ERR_DOWNLOAD_NETWORK`」，但 Wails 在返回 error 时会丢掉返回值，前端拿不到兜底列表；而 30s 内的第二次调用命中内存缓存、不带错误，于是同一页面会在「没有可下载的模型」与「4 个内置条目（无下载地址）」之间切换。需要产品决定兜底清单该怎么呈现。

### G-E2E-16 数据存储页 React「unknown prop cursorFill」警告（已修复）

- 根因：Mantine 9.7.5 的 BarChart 没有解构 `cursorFill`，它经 `...others` 漏到 `<div>` 上。
- 处置：改用 CSS Module 设置 `--chart-cursor-fill`。

## 人工验证步骤（无法自动化的部分）

自动化缺口都写在 `frontend/e2e/not-automatable.spec.ts` 里（每条一次 skip + 原因），对应步骤如下。

### G-E2E-05 目标应用退出（应用级捕获中断）

前提：Windows **Build 20348+**（本机 19045 只支持设备回环；产品在录制页会直接说明这一点）。

1. 打开一个会发声的应用（如音乐/会议软件），确认它在播放。
2. 「录制」页 → 在「应用」分组里选中它 → 开始录制。
3. 录制中直接退出该应用。
4. 期望：录制不停（或按设计转入 `interrupted`/`failed`），回放时间轴出现 `source-lost` 标记；音频可播放、崩溃前已产出的片段保留。

### G-E2E-06 录制中设备切换 / 设备拔出

1. 「录制」页选中「默认设备」→ 开始录制。
2. 在系统声音设置里把默认输出设备切到另一个（或禁用当前设备）。
3. 期望：录制不断、音频连续；时间轴出现 `device-switch` 标记；停止后会话可回放。

### G-E2E-07 导出（另存为）

server 模式下运行时明确抛 `file dialogs not available in server mode`，所以必须用**桌面模式**（`wails3 dev` 或 `wails3 build`）验证：

1. 会话库任选一行 → 导出文本 → 分别试 `.txt / .srt / .csv / .xlsx`，期望弹出原生「另存为」并落盘，内容含时间戳与说话人。
2. 「录制」页对已结束会话点「另存录音文件」→ 期望得到可播放的 WAV，且原会话仍可回放（不改库）。

### G-E2E-08 转写成功路径（真实语音 → 片段 → 按句回放）

1. 「模型」页下载本地中文模型（并确认 `modelRoot` 就位）。
2. 播放一段中文语音，同时用设备回环录下来（这样录到的是真实人声），停止录制。
3. 会话库对这一行点「发起转写」，期望进度推进到 100%、徽标变「已转写」、句数 > 0。
4. 打开回放，期望播放时按句高亮、点句跳转、`×` 倍速与音量可用。

### G-E2E-09 边录边转 / 说话人分离

1. 下载流式中文模型 → 设置里打开「边录边转」→ 录制说话，期望录制页实时出字（草稿 + 定稿）。
2. 对已转写会话点「说话人与角色」→「发起分离」，期望按声纹聚类出多个说话人、可改名（改名对历史会话同步生效）。

## 已知限制（本 E2E 的诚实边界）

- **录制源**：只用设备回环（进程级捕获在本机不可用）。录到的是用例自播的正弦测试音而不是语音，`segments` 必然为 0 —— 用例因此断言「0 句」而不是假装有台词。录制类用例会从默认扬声器发出约 5 秒的轻微 440Hz 提示音。
- **片段为夹具**：`playback-highlight.spec.ts` 的片段文本是测试夹具，验证的是「时间轴 → 高亮 → 跳转」同步逻辑，不是识别准确率。
- **`test.fail` 的代价**：G-E2E-01 那条用例在缺陷修复前一直「预期失败」，因此它内部若出现**其它**原因导致的失败，也会被同样吞掉。修改该用例上下文时请留意这一点。
- **非确定性输入**：崩溃恢复用例断言「`data` 长度为 0 时仍可播放」依赖 Chromium 的容错行为；换浏览器 channel（`E2E_CHANNEL=chromium`）时行为一致，但换成严格解读 WAV 头的播放器则可能不同。
