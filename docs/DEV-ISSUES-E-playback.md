# Wave E-2 开发问题记录（后端音频流通道）

> **用途**：本阶段（给前端提供可播放的音频流通道）开发过程中遇到的问题**即时登记**：
> 现象 → 复现条件 → 根因 → 临时处置 → 状态。
>
> 状态取值：`未解决` / `已绕过` / `已解决` / `改设计`（不改代码，改设计说明）

---

## E2-01 会话音频路由此前没有任何既定约定

- **现象**：仓库里不存在 `/audio/` 前缀的任何约定（前端无对应 `src`、`docs/API.md` 无对应条目、
  `internal/` 无相关处理器），回放页即使做出来也没有可播地址。
- **复现条件**：`grep -rn "/audio/" docs/ frontend/src internal/` 只命中在线 ASR 的
  `/v1/audio/transcriptions`（出站），没有本机音频路由。
- **根因**：Wave E 之前没有回放需求，资源处理器只有 `application.AssetFileServerFS(assets)` 一条 SPA 链路。
- **临时处置**：本阶段新建 `internal/app/audio_route.go`，约定 URL 为 `/audio/<sessionId>`
  （可带 `?v=` 查询串用于破缓存，服务端忽略查询串）。**未**新增 `internal/contract` 常量，
  以免为一条纯后端路由改动共享契约包。
- **待办**：由主 agent 在 `app.go` 接线（一行，见下），并在 `docs/API.md` / `docs/UX-SPEC.md` 回放章节补记该 URL。
- **状态**：**未解决（等待接线）**。接线代码：

  ```go
  Assets: application.AssetOptions{
      Handler: NewAssetHandler(assets, holder, resolver),
  },
  ```

---

## E2-02 `go test ./internal/app/` 无法启动测试二进制（0xc0000135 / 127 / 0xc000007b）

- **现象**：`internal/app` 的测试二进制**编译成功但一律起不来**，三种退出码轮流出：
  - 直接跑：`exit status 0xc0000135`（或 Git Bash 报 `127`，`ldd` 显示
    `sherpa-onnx-c-api.dll => not found`）；
  - 把仓库 `bin/` 加进 `PATH` 后：`exit status 0xc000007b`（"unable to start correctly"）。
- **复现条件**：
  ```bash
  export PATH="/j/msys2/ucrt64/bin:$PATH" GOCACHE="$HOME/AppData/Local/go-build" CGO_ENABLED=1
  go test -count=1 -run Audio ./internal/app/            # 0xc0000135
  PATH="$PWD/bin:$PATH" go test -count=1 -run Audio ./internal/app/   # 0xc000007b
  ```
- **根因**：两段叠加。
  1. `internal/app` 经 `internal/asr/local` 间接链接 sherpa-onnx（CGO），运行期需要
     `sherpa-onnx-c-api.dll`；Windows DLL 搜索顺序里**没有**仓库 `bin/`，于是找不到 → `0xc0000135`。
  2. 把 `bin/` 放进 `PATH` 后，Windows 仍先搜 `System32`，而本机存在
     `C:\Windows\System32\onnxruntime.dll`（Windows ML 自带版本，**与 sherpa 编译期所对的版本 ABI 不兼容**）。
     它遮蔽了仓库 `bin/onnxruntime.dll` → 依赖加载失败 → `0xc000007b`。
     （两个文件都是 x86-64，位数不是原因。）
  3. 若把 `bin/` 用 MSYS 路径写法（`/e/Desktop/.../bin`）塞进 `PATH`，Windows 加载器解析不了该条目，
     等于没加。
- **临时处置**：**不经 `PATH`，而是把测试二进制产出到 DLL 同目录**——应用目录在 Windows 搜索顺序里
  优先于 `System32`：
  ```bash
  go test -c -o bin/app_audio_test.exe ./internal/app/
  ./bin/app_audio_test.exe -test.run "Audio|SafeJoin" -test.v
  ```
  跑完即删除该产物（不要提交二进制）。`go build` / `go vet` 不受影响，照常可用。
- **待办**：给 `internal/app` 补一个 Taskfile 目标（如 `task test:app`）自动做
  「build → 放到 `bin/` → 运行 → 清理」；或把 `bin/` 的 DLL 覆盖进 `System32`（**不做**，会污染整机）。
  另一条更彻底的路子是把 sherpa 的链接方式改成运行期 `LoadLibrary`，但那属于 `internal/asr/local` 的范围，
  不在本阶段。
- **状态**：**已绕过**（测试可稳定运行，但每次需手工两步）。

---

## E2-03 符号链接 / junction 越界：字面路径判定挡不住

- **现象**：只做 `filepath.Clean` + `filepath.Rel` 的越界判定时，若
  `dataRoot/sessions/<id>/audio.wav` 本身是一个指向根外文件的符号链接（或 Windows junction），
  字面路径完全合法，判定会通过并把根外文件读出来。
- **复现条件**：`mklink <dataRoot>\sessions\s_link\audio.wav C:\outside\secret.wav`
  （或 `os.Symlink`，需开发者模式），随后请求 `/audio/s_link`。
- **根因**：`filepath.Rel` 只比较**路径字符串**，不解析链接；链接的真实指向可能是任意位置。
- **临时处置**：在 `serveAudio` 里于 `os.Open` 之前调用 `ensureRealPathUnderRoot`：
  对 `dataRoot` 与目标文件各做一次 `filepath.EvalSymlinks`，再跑同一套 `isUnderRoot` 判定；
  解析失败（含根已不存在）按失败处理，**不回退**到字面判定。
  单测 `TestAudioRouteRejectsSymlinkEscape` 覆盖（建不出链接时自动 `t.Skip`；
  本机已验证未跳过、确实执行）。
- **状态**：**已解决**。

---

## E2-04 越界与「不存在」必须返回同一个 404

- **现象**：若越界返回 403 / 400，攻击者可以据此探测「某个路径是否存在于盘上」，
  也能通过错误体反推 dataRoot 位置。
- **复现条件**：请求 `/audio/s_xxx`，构造一条 `audio_path` 为 `../../etc/passwd` 的会话行。
- **根因**：不同错误码 + 详细错误消息本身就是信息泄漏面。
- **临时处置**：`miss()` 对所有否证情形（无会话 / 路径越界 / 符号链接越界 / 非 `.wav` /
  读库失败 / dataRoot 不可用）**统一回 404**，响应体只有 `http.NotFound` 的固定文本，
  并带 `Cache-Control: no-store`（避免「刚录完立刻可播」时吃到缓存的旧 404）。
  真实路径、库错误详情**只进日志的 `reason` 原因码**（`outside_root` / `outside_root_link` /
  `no_session` / `not_wav` / `lookup_failed` / `no_data_root`），不进响应体。
- **状态**：**已解决**。

---

## E2-05 `go vet` 偶发输出一行 `undefined`

- **现象**：`go build ./internal/app/ && go vet ./internal/app/ | head -20` 这种**串联 + 管道**写法下，
  `go vet` 位置偶尔打印一行 `undefined`（单独跑无输出、`exit=0`）。
- **复现条件**：仅在同一命令行里 `go build ... | head` 之后紧跟 `go vet ... | head` 时出现过两次；
  单独执行 `go vet ./internal/app/ > /tmp/vet.txt 2>&1` 时 `exit=0` 且文件 0 字节，**未复现**。
- **根因**：未确认。倾向判断为多段管道输出串扰 / 缓存诊断信息的残留输出，
  不是真实诊断（真实诊断会带 `# vip.oberon.mandaryn/internal/app` 与文件行列号）。
- **临时处置**：验收时把 `go vet` 单独执行并检查 `exit code` 与输出字节数，不只信终端里那一行。
- **状态**：**已绕过**（不阻塞，但若再次出现且带位置信息，应按真实诊断处理）。

---

## E2-06 `sessions.audio_path` 是相对 `dataRoot` 的正斜杠路径

- **现象**：最初不确定落库路径是绝对路径还是相对路径、分隔符是 `/` 还是 `\`，
  直接 `filepath.Join(dataRoot, row.AudioPath)` 在 Windows 上是否成立没有保证。
- **复现条件**：读取 `internal/recording/recorder.go`：
  `relPath := path.Join(paths.SessionsDirName, id, AudioFileName)`（`audio.wav`），
  同时写入 `absPath := filepath.Join(dataRoot, filepath.FromSlash(relPath))`。
- **根因**：设计如此（`storage.Session` 注释明确「`AudioPath` 存**相对 dataRoot** 的路径，
  数据目录迁移不需要改库」）。
- **临时处置**：`safeJoinUnderRoot` 内部先把 `\` 归一为 `/`、`path.Clean`，再
  `filepath.Join(rootAbs, filepath.FromSlash(cleaned))`，因此两种分隔符都能正确处理。
  单测覆盖了 `sessions/s_01/audio.wav` 与 `a\b.wav` 两种写法。
- **状态**：**已解决**（属事实确认，无代码遗留）。

---

## E2-07 不把音频读进内存 / 不 base64（实现约束回顾）

- **现象**：最省事的做法是把 WAV `os.ReadFile` 后 base64 走 IPC，但那会让几十分钟录音
  （数百 MB）在每次 seek 时整体重传，且 WebView 侧还要多一份解码副本。
- **复现条件**：任何 `audio.ReadFile` + `EncodeToString` 的回放实现。
- **根因**：IPC 是整值传输语义，天然不支持 Range。
- **临时处置**：改用 `http.ServeContent` + 浏览器原生 Range：
  响应 `Accept-Ranges: bytes`，请求 `Range: bytes=N-M` 得 `206` + `Content-Range`，
  播放器 seek 只需拉需要的片段。文件以 `os.Open` 的 `*os.File`（`io.ReadSeeker`）交给
  `ServeContent`，全程流式，内存占用与文件大小无关。
  `statusRecorder` 转发 `io.ReaderFrom`，避免大文件传输退化成 32KB 缓冲复制。
- **状态**：**已解决**（作为约束固化在 `audio_route.go` 的注释中）。
