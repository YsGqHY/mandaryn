# Wave G-2：`nollm` 精简变体 —— 问题记录

记录日期：2026-11-12

## G2-01 `nollm` 此前只是文档，代码里零命中

**现象**：`CLAUDE.md` / `docs/ARCHITECTURE.md` §2.2 / `docs/DEPLOYMENT.md` 都把 `nollm`
写成像已实现（"`internal/asr/local/` 与 `capture_darwin.go` 等文件加 `//go:build !nollm`"），
而 `grep -rn nollm --include=*.go .` **零命中**，`-tags nollm` 构建出来的仍是完整本地引擎。

**处理**：按「落地实现 + 文档纠偏」两条一起做，见 G2-02 ~ G2-04。文档改成落地现状，
不再用「已加」这类与代码不符的措辞。

## G2-02 真正的裁剪点：三个实现文件 + 三处桩标签

| 文件 | 改前 | 改后 |
|------|------|------|
| `internal/asr/local/local_windows.go` | `windows && cgo` | `windows && cgo && !nollm` |
| `internal/asr/local/streaming_windows.go` | `windows && cgo` | `windows && cgo && !nollm` |
| `internal/speaker/sherpa/sherpa_windows.go` | `windows && cgo` | `windows && cgo && !nollm` |
| `internal/asr/local/local_stub.go` | `!windows \|\| !cgo` | `nollm \|\| !windows \|\| !cgo` |
| `internal/speaker/sherpa/sherpa_stub.go` | `!windows \|\| !cgo` | `nollm \|\| !windows \|\| !cgo` |
| `internal/asr/local/factory_stub_test.go` | `!windows \|\| !cgo` | `nollm \|\| !windows \|\| !cgo` |

桩语义**不变**：`ERR_LOCAL_UNAVAILABLE` + 原因文案，**不**静默降级去调在线引擎
（与「本地故障不得自动回退在线」一致）。只把文案从「仅 Windows + CGO 提供」改成
「需 Windows + CGO 且非 nollm 精简产物」，让 nollm 用户看到的是准确原因。

`factory_stub_test.go` 顺带获得价值：默认 `windows && cgo` 下该包不编译任何测试
（避免测试二进制绑 DLL），而 nollm 下不绑 DLL，于是**桩契约测试在 nollm 下真的会跑**
（`ok vip.oberon.mandaryn/internal/asr/local`），默认构建仍是 `no test files`，行为不变。

真机推理测试（`inference_test.go` / `real_model_test.go` / 声纹 `real_model_test.go`）
补 `&& !nollm`：它们的实现文件在 nollm 下被排除，留着标签会得到「测试能编过、符号不存在」的误导性失败。

## G2-03 `internal/capture/` 不需要 `nollm` 标签（纠文档，不是漏做）

文档原话把 `capture_darwin.go` 也列进 `!nollm`，与事实不符：

- Windows 捕获走 `github.com/moutend/go-wca`（纯 Go，`golang.org/x/sys` 系统调用），**不依赖 CGO**；
- macOS / Linux 捕获目前是**诚实桩**（`capture_darwin.go` / `capture_linux.go`，返回
  `ERR_CAPTURE_UNAVAILABLE`），同样没有 CGO 依赖。

因此现在给 capture 加 `!nollm` 是给不存在的依赖打标签。等 macOS（Core Audio process tap）
/Linux（PipeWire）的 CGO 捕获实现落地时再补，已写进 ARCHITECTURE §2.2 的落地说明。

## G2-04 `CGO_ENABLED=0` 本来就能过 —— 它不是本变体的判据

改动前实测：`CGO_ENABLED=0 go build ./internal/asr/...` 就是 exit 0。
原因：`cgo` 本身是 Go 的**隐式构建约束**，`CGO_ENABLED=0` 时 `windows && cgo` 两个实现文件
和 `!windows || !cgo` 的桩自动切换，`nollm` 标签根本没参与。

所以「`CGO_ENABLED=0` 能否 build」**不能**证明 nollm 已实现。真正的判据是：

```bash
go list -deps -tags nollm . | grep -i sherpa
# 改动前（CGO_ENABLED=1）：命中 github.com/k2-fsa/sherpa-onnx-go-windows / sherpa_onnx
# 改动后：只剩 vip.oberon.mandaryn/internal/speaker/sherpa（本仓库的桩包）
```

以及 Windows 产物的 PE 导入表：标准产物导入 `sherpa-onnx-c-api.dll` + 9 个 MSVC CRT api-set，
nollm 产物只导入 `kernel32.dll`。

## G2-05 Windows 上「体积」省在 DLL 上，不省在 exe 上

实测（`-tags production,nollm -trimpath -ldflags="-w -s -H windowsgui"`）：

| | exe | 必需 DLL |
|---|---|---|
| 标准 | 23,709,184 B | 22,664,704 B（3 个） |
| nollm | 23,522,816 B | 0 |

exe 只小 186,368 B（约 182 KiB），因为 Windows 上 sherpa-onnx 是**动态链接**，
cgo 胶水本身很小。所以对外讲体积收益时必须按「部署总体积 44.2 MiB → 22.4 MiB（−49%）」说，
**不能**只说「exe 从 23.7 MB 降到 23.5 MB」——后者会让人以为变体没生效。
Linux/macOS 静态链接下二进制本体差会明显得多（未实测，文档不作数字承诺）。

## G2-06 跨 agent 协作点：`streaming_windows.go` 的 `!nollm` 需保持

`internal/asr/local/streaming_windows.go` 是并行的流式引擎 agent 新增的实现文件
（import sherpa-onnx，导出 `NewStreaming` / `NewStreamingFactoryWithThreads`）。
它原本只有 `//go:build windows && cgo`，**会**让 nollm 继续链入 sherpa —— 本次已补 `&& !nollm`。

**已闭环**：该 agent 随后自行补了 `internal/asr/local/streaming_stub.go`
（标签同为 `nollm || !windows || !cgo`，`NewStreaming` / `NewStreamingFactoryWithThreads`
恒返回 `ERR_LOCAL_UNAVAILABLE`），并把 `real_streaming_test.go` 标成
`windows && cgo && !nollm && mandaryn_realmodel`。于是：
- nollm 下流式引擎与离线引擎**一起**被裁剪，符号仍在（走桩），装配层无需分叉；
- 默认 `windows && cgo` 构建不受影响。

遗留风险（仅一条）：若 `streaming_windows.go` 被整体重写，`&& !nollm` 可能被覆盖。
判据很便宜，合入前复查即可：

```bash
head -1 internal/asr/local/streaming_windows.go   # 期望：//go:build windows && cgo && !nollm
go list -deps -tags nollm . | grep -i sherpa     # 期望：只剩本仓库的 internal/speaker/sherpa
```

注：`streaming_stub.go` 的顶部注释仍写「非 Windows / 非 CGO 构建下」，未提 `nollm`
（与 `local_stub.go` 已更新的措辞不完全一致）。属该文件所有者的注释措辞，本次未改动以免与其编辑冲突。

## G2-07 与质量门限相关的旁证（非本波次范围）

`go vet ./internal/...` 在 `internal/recording/memory_test.go:43` 报
`unknown field BitsPerSample in struct literal of type capture.AudioFormat`
—— 属并行的 capture/recording 波次在途改动，与本变体无关，按要求未处理。
本波次相关包（`./internal/asr/...`、`./internal/speaker/...`、`./internal/capture/...`、
`./internal/app/...`）`go vet` 全绿。
