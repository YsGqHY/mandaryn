# Tabler 风格品牌图标母版

本目录下 9 个 SVG 是**品牌资产 / 设计母版**，按 `@tabler/icons-react` v3.50.0 的 outline 参数手绘：

- `viewBox="0 0 24 24"`、`fill="none"`
- `stroke="currentColor"`、`stroke-width="2"`
- `stroke-linecap="round"`、`stroke-linejoin="round"`

## ⚠️ 不用于 UI 渲染

UI 图标一律使用 `@tabler/icons-react` 的现成 React 组件（铁律见 [frontend/CLAUDE.md](../../../CLAUDE.md) 的「Icon 规范」）。
这些文件只用于品牌物料、设计参考与文档插图，**不得接入导航、工具栏或任何 UI 组件**。

功能位应使用的 Tabler 图标：

| 母版 | UI 中应使用 |
|------|-------------|
| mic | `IconMicrophone` |
| bars | `IconChartBar` |
| cube | `IconCube` |
| window | `IconBrowser` |
| wave | `IconWaveSine` |
| shield | `IconShieldLock` |
| gauge | `IconGauge` |
| pause | `IconPlayerPause` |
| export | `IconUpload` |

权威清单与「品牌母版 / UI 图标」边界说明见 [brand/icons/manifest.json](../../../../brand/icons/manifest.json) 的 `uiBoundary` 字段。
