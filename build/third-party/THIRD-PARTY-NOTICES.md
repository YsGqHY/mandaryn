# 第三方组件许可声明

安装包随附以下第三方二进制，其许可声明一并装入安装目录（与 `mandaryn.exe` 同级）。
本文件说明来源与版本，具体许可全文见同目录下的各 `*-LICENSE` 文件。

| 文件 | 组件 | 许可 | 来源 |
|------|------|------|------|
| `sherpa-onnx-c-api.dll` | [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) | Apache-2.0 | Go 模块 `github.com/k2-fsa/sherpa-onnx-go-windows@v1.13.8` 的 `lib/x86_64-pc-windows-gnu/`，上游为 k2-fsa/sherpa-onnx 官方发布 |
| `sherpa-onnx-cxx-api.dll` | sherpa-onnx（C++ API） | Apache-2.0 | 同上 |
| `onnxruntime.dll` | [ONNX Runtime](https://github.com/microsoft/onnxruntime) | MIT | 由 sherpa-onnx 官方发布包一并分发（非本项目单独引入） |

- `sherpa-onnx-go-windows-LICENSE`：sherpa-onnx-go-windows 模块随附的 Apache-2.0 全文。
- `onnxruntime-LICENSE`：ONNX Runtime 的 MIT 全文（出处：`microsoft/onnxruntime` 仓库 `LICENSE`）。

> 为什么随包分发：这三个 DLL 是 `mandaryn.exe` 的**静态导入依赖**（导入表直接引用
> `sherpa-onnx-c-api.dll`，它再依赖 `onnxruntime.dll`），必须与 exe 同目录才能启动。
> 详见 [docs/DEPLOYMENT.md](../../docs/DEPLOYMENT.md) 的「运行时 DLL 布局」一节。
