# DEV-ISSUES — F/实机模型验证

> Wave F 第一项：**用真实模型文件跑真实推理**，不再只靠桩件。
> 结论：链路已真机跑通；过程中发现 **2 个真实缺陷**（都会让真实模型包 100% 失败），均已修复并补测试。

---

## F-RM-01（已修）`DetectBundle` 返回的 `Tokens` 是裸文件名 → 真实模型必然加载失败

**现象**：真实 SenseVoice 包加载失败：

```
D:\a\sherpa-onnx\...\offline-model-config.cc:Validate:105 tokens: 'tokens.txt' does not exist
c-api.cc:SherpaOnnxCreateOfflineRecognizer:698 Errors in config
本地引擎失败 code=ERR_ENGINE_LOAD
```

**复现条件**：任何真实模型包（`asrlocal.Warm` → `buildConfig` → `sherpa.NewOfflineRecognizer`）。
只要走真实权重就 100% 复现；桩件/单元测试不会触发。

**根因**：[internal/asr/bundle.go](../internal/asr/bundle.go) 里

```go
spec.Tokens = pick(names, ...)   // 返回的是**文件名**
```

而其他字段都拼了 `filepath.Join(dir, ...)`：

```go
spec.Model = filepath.Join(dir, f)
```

`sherpa-onnx` 的 `OfflineModelConfig.Tokens` 直接吃该字符串，裸文件名以**进程 cwd** 为基准解析，于是找不到文件。
`BundleSpec.Tokens` 的注释写的是「tokens.txt 路径」，实现与注释不符。

**为什么长期没被发现**：既有断言只检查 `spec.Tokens != ""`（非空即可），裸文件名同样满足；而 `internal/asr/local` 此前从未跑过真实权重。

**修复**：`Tokens` 改为 `filepath.Join(dir, tok)`，并在注释里写明原因（这是真实事故，不是风格问题）。

**防回归**：[internal/asr/bundle_test.go](../internal/asr/bundle_test.go) 的启发式用例新增断言 ——
`Tokens` / `Model` / `Encoder` / `Decoder` / `Joiner` 凡非空者必须 `filepath.IsAbs` **且** `os.Stat` 成功。
这条断言才能抓住本缺陷（只判空抓不住）。

**状态**：已修复并验证（见下方实测）。

---

## F-RM-02（已修）官方 SenseVoice 包被误判为 `paraformer` → 走错配置分支

**现象**：真实包识别结果 `kind=paraformer`，而实际是 SenseVoice。

**复现条件**：包目录 `models/sensevoice-small@1.2.0/`，包内文件是官方命名 `model.int8.onnx` + `tokens.txt`。

**根因**：判定只扫描**文件名**，且规则要求文件名含 `sense`：

```go
return strings.Contains(lower, "sense") && strings.HasSuffix(lower, ".onnx")
```

但官方 sherpa-onnx 的 SenseVoice 包内 onnx 就叫 `model.int8.onnx`——与 paraformer 版式**完全相同**，名字里没有 `sense`。类型信息只存在于**包目录名**（`bundleId@version`）。

**后果**：[internal/asr/local/local_windows.go](../internal/asr/local/local_windows.go) 的 `buildConfig` 会把 SenseVoice 权重塞进 `cfg.ModelConfig.Paraformer.Model`，并且**丢掉 ITN 后处理**（`SenseVoice.UseInverseTextNormalization = 1` 不再生效）——要么加载失败，要么输出无标点/数字不规范。

**修复**：判定顺序改为「**先结构、后命名**」：

1. 三件套（encoder/decoder/joiner）→ transducer；两件套 → whisper（结构可靠，优先）
2. 文件名含 `sensevoice` / `sense-voice` → sensevoice
3. **新增**：包目录名含 `sensevoice` / `sense-voice`，且目录内存在 `model.int8.onnx`/`model.onnx` → sensevoice
4. 其余单文件 → paraformer

`senseHint` 只认 `sensevoice` / `sense-voice` 两种写法（裸 `sense` 会误伤无关命名）。
目录名线索仅对「目录内确有 `model.int8.onnx` / `model.onnx`」生效，避免混放目录被误判。

**防回归**：[internal/asr/bundle_dirname_test.go](../internal/asr/bundle_dirname_test.go) 四个用例：
目录名生效（两种写法）、中性目录名仍归 paraformer、**三件套优先于目录名提示**。

**状态**：已修复并验证。

---

## 实机验证证据

**环境**：Windows + CGO，模型为官方真实权重（非桩件）。

| 项 | 值 |
|---|---|
| ASR 模型 | `sensevoice-small@1.2.0`，`model.int8.onnx` **239,233,841 字节** |
| tokens | `tokens.txt` 315,894 字节 / **25,055 行** |
| 声纹模型 | `3dspeaker_speech_campplus_sv_zh-cn_16k-common.onnx` **28,281,138 字节** |
| 样本 | `zh.wav` 178,988B（5.59s）、`en.wav` 228,908B（7.15s），均 16k/单声道/16bit |

### 真实转写（中文）

```
包识别：kind=sensevoice model=...\sensevoice-small@1.2.0\model.int8.onnx
                 tokens=...\sensevoice-small@1.2.0\tokens.txt
本地引擎加载完成 engineId=sensevoice-small@1.2.0 kind=sensevoice loadMs=853
加载后引擎：状态=ready
样本：89472 采样点 = 5.59s
转写结果：text="开饭时间早上9点至下午5点。" lang="<|zh|>" elapsedMs=180
--- PASS: TestRealSenseVoiceTranscribe (1.07s)
```

### 真实转写（英文，验证语种自动识别 + 数字后处理）

```
转写结果：text="The tribal chieftain called for the boy and presented him with 50 pieces of code."
          lang="<|en|>" elapsedMs=228
--- PASS: TestRealSenseVoiceTranscribe (1.10s)
```

### 真实声纹

```
声纹模型标识：speaker:3dspeaker_speech_campplus_sv_zh-cn_16k-common
声纹维度：192
同段音频自相似：1.000000
跨音频相似度：0.0466（自相似 1.000000，仅观察不设硬阈值）
--- PASS: TestRealSpeakerEmbed (0.55s)
```

结论：**真实模型链路可用** —— 加载、自检、中文/英文转写、语种识别、ITN、声纹 192 维、
同段稳定（1.000000）与跨段区分（0.0466）全部实测通过。首次加载 853ms（int8，2 线程）。

---

## F-RM-03（未闭环）真实模型验证默认不进 CI

**现象**：真实权重 239MB + 28MB，不可能进仓库或每次 CI 下载。

**现状**：验证代码用构建标签 `mandaryn_realmodel` 隔离，默认 `go test ./internal/...` 不编译、
不下载、不依赖网络。运行方式见两个 `real_model_test.go` 文件头注释。

**待定项（需要产品/工程决策）**：

1. 是否提供「模型校验和清单」让用户手动放置的模型可验证完整性？（当前只校验能否加载）
2. 是否把该验证挂到一个**手动触发**的 CI job（如发版前跑一次），避免只在本机验证？

**状态**：待定，不阻塞。

---

## F-RM-04（观察）清单声明体积与实际不符

清单 `sensevoice-small` 声明 `SizeBytes: 228 << 20`（= 239,075,328），实际下载 239,233,841 字节，
差 158,513 字节（未压缩口径 / 不同打包批次）。下载器用的是 `Content-Length`，不影响校验，
但 UI 上的「预计大小」会与实际略有偏差。

**状态**：观察项，不修（远端清单可自校正）。

---

## 复现命令（人工）

```bash
export PATH="/j/msys2/ucrt64/bin:$PATH" GOCACHE="$HOME/AppData/Local/go-build" CGO_ENABLED=1

# 1) 准备模型（官方真实权重，放成产品布局）
#    <root>/asr/sensevoice-small@1.2.0/{model.int8.onnx,tokens.txt}
#    <root>/spk/3dspeaker-zh@1.1.0/3dspeaker_speech_campplus_sv_zh-cn_16k-common.onnx

# 2) 编译测试二进制到 bin/（DLL 与 exe 同目录，否则 0xc0000135）
go test -tags mandaryn_realmodel -c -o bin/asrreal.exe ./internal/asr/local/
go test -tags mandaryn_realmodel -c -o bin/spkreal.exe ./internal/speaker/sherpa/

# 3) 跑（路径必须是 Windows 形式）
cd bin
MANDARYN_REAL_ASR_DIR="<root>/asr/sensevoice-small@1.2.0" MANDARYN_REAL_WAV="<root>/zh.wav" \
  ./asrreal.exe -test.v
MANDARYN_REAL_SPK_DIR="<root>/spk/3dspeaker-zh@1.1.0" \
  MANDARYN_REAL_WAV_A="<root>/zh.wav" MANDARYN_REAL_WAV_B="<root>/en.wav" \
  ./spkreal.exe -test.v
```

> Windows 上 `go test` 直接跑会 `0xc0000135`（找不到 `sherpa-onnx-c-api.dll`），
> 且把 `bin/` 加 PATH 也无效（System32 同名 DLL 抢先 → `0xc000007b`）。
> **必须**把测试二进制编译进 `bin/` 并从那里运行。
