# Wave F-3 遗留问题：超大通话的分页回放（E-06）

本文只记录**本次改动暴露出来的、本轮不修的问题**与**验证方式**。设计说明写在代码顶部注释里
（`frontend/src/services/playback/pagedTimeline.ts`、`virtualRows.ts`、
`frontend/src/pages/LibraryPage/playback/usePagedTimeline.ts`）。

## 1. 后端契约缺口：`TimelinePage` 没有 `markers` / `durationMs`（主要问题）

`transcriptsvc.GetTimelinePage(sessionId, offset, limit)` 返回
`{ sessionId, total, offset, limit, segments }` —— **不含**时间轴标记与会话时长；这两项只在
`GetTimeline(sessionId)` 的返回值里（`internal/transcribe/timeline.go` 的 `Timeline` / `Markers`）。

前端因此只能这样绕（见 `usePagedTimeline`）：

| 会话规模 | 首屏请求 | markers / durationMs 来源 |
|---|---|---|
| `segmentCount ≤ PLAYBACK_INLINE_MAX`（1500） | 一次 `GetTimeline` | 同一次请求（与分页前完全一致） |
| 更大 | `GetTimelinePage(0, 200)`（只 200 条） | 首屏渲染完成后**空闲时**再补一次 `GetTimeline`，只留 `markers` + `durationMs`，片段丢弃 |
| `total > PLAYBACK_MARKER_MAX_TOTAL`（20000） | 同上 | **放弃**（为标记拉两万行不划算），列表与高亮不受影响 |

代价：大会话虽然首屏快了，但那一次空闲补取仍会把 ≤2 万行片段过一遍桥再丢掉，存在一次
瞬时内存/带宽尖峰。`durationMs` 另有 `GetDetail().session.durationMs` 兜底，所以真正只为了
markers。

**建议的后端修法**（本轮禁止改 Go，故未做）：

1. 给 `contract.TimelinePage` 加 `markers` 与 `durationMs`（`RangePage` 已经能拿到 session 行，
   成本几乎为零）；或
2. 新增 `transcriptsvc.GetTimelineMarkers(sessionId) → []contract.Marker`。

任一落地后，前端只需改 `usePagedTimeline` 的 `loadMarkers` 一处：首屏那次 `GetTimelinePage`
就能带回标记，空闲补取的整段逻辑可删。

## 2. 标记只能随「已加载页」出现

标记行的位置是「第一个 `startMs >= atMs` 的片段之前」，而分页模型里只有**已加载**片段能参与
这个判断（`markersBySlot`）。后果：

- 某条标记要等它周围那一页加载完才会出现（滚动 / 播放到附近时会触发加载，通常无感）；
- 会话**尾部**的标记在最后一页加载前不出现；
- 极端情况下（只加载了第 0 页，而标记都在很后面）标记会集体缺席。

因为列表只渲染可视窗口、而窗口本身会驱动加载，用户滚到标记附近时它已经在位，所以这是
「可能短暂缺席」而不是「永久丢失」。后端按 §1 修好后，标记与片段解耦，这个问题自动消失。

## 3. 虚拟化的高度是「实测 + 估算」，滚动条长度有误差

歌词行高度不固定（正文折行），所以行高靠 `offsetHeight` 实测；**从未渲染过**的槽位用
`SLOT_HEIGHT_ESTIMATE`（76px）估算，未加载页的占位行也按估算参与总高度。因此：

- 滚动条长度 / 拖动到某个位置时的高亮会有「按估算比例」的偏差，随滚动逐步校正；
- 已渲染的行永远按实测高度定位，所以**滚动过程中不会抖动**（不存在「测量→改总高→再测量」的正反馈）。

已知小偏差：`.list` 有 `padding-top: 8px`，而偏移表从 spacer 顶部算起，`scrollTop` 比偏移
基准多 8px。这点误差被 `SLOT_OVERSCAN`（8 行）完全吸收，未做修正以免引入反向偏移。

## 4. 未加前端单测（项目没有前端测试基建）

`package.json` 里没有 vitest / jest，也没有任何 `*.test.ts`。按交办要求**没有**擅自引入测试框架，
所以新增的纯函数只有类型检查与人工验证在线（函数本身是纯的，接入任何框架都只需直接调用）：

- `pageForGlobalIndex` / `globalIndexOf` / `pageCountOf` / `indexRangeOfPage` / `pagesCoveringIndexRange`
  —— 全局索引 ↔ 页内索引换算，互逆性可直接断言；
- `locateIndexByTime` / `probePageForMs` —— 时间 → 全局索引（含未加载页的探测收敛）；
- `visibleRange` / `indexAtOffset` / `offsetsOf` / `centerScrollTop` —— 窗口化数学。

## 5. 未做 E2E（千句级种子数据成本高）

`frontend/e2e/` 只有 `floating-dock.spec.ts` / `screenshots.spec.ts`，且需要 server 模式二进制 +
隔离数据目录；造一个千句级会话要绕过 UI 直接写库（`segments` 表），成本高于本轮收益，故未做。
现有 E2E 也没有引用回放抽屉，所以没有回归覆盖可用。

## 6. 如何人工验证（三步）

前置：需要一个「句数 > 1500」的会话（小于这个值走一次性取全，看不到分页行为）。

1. **造种子数据**：任选其一
   - 用应用转写一段长音频（> 40 分钟，中文连续讲话，通常就能上千句）；或
   - 直接用 SQLite 工具往数据目录的 `mandaryn.db` 的 `segments` 表插入：给某个已有会话
     `INSERT ... SELECT` 复制它自己的行并把 `seq` / `start_ms` / `end_ms` 依次后移，
     重复到 2000 行以上（`sessions.segment_count` 顺手改成相同值，让列表页显示一致）。
2. **打开抽屉**：进「会话库」，对这一行长按/右键或点「回放」。观察 DevTools → Network：
   只有**一次** `GetTimelinePage` 且请求参数形如 `offset=0, limit=200`；
   列表容器的 `data-playback-page-size="200"`、`data-playback-total="<总句数>"`、
   `data-playback-window="0-约20"`（`data-playback-window` 说明当前只渲染了二十来行）。
3. **滚动 / 播放验证**：
   - 把列表快速往下拖：断言「又出现新的 `GetTimelinePage`，每次 `offset` 递增 200」，
     且列表底部出现灰色占位条后立即被正文替换；
   - 点进度条跳到会话**后半段**：高亮**不会消失**（先停在最近的已加载句），约一次请求后
     落到正确那句，并且该句被滚到中线；
   - 播放中观察高亮切换是否跟得上（应与分页前一致，随句推进而不是按页跳）。

## 7. 本轮顺带移除的旧代码

分页 + 窗口化上线后，`PlaybackService` 里的 `buildTimelineRows` / `PlaybackTimelineRow`
（把片段与标记拼成一维行的旧模型）已无任何调用点 —— 标记混排改由 `markersBySlot` 按槽位
归属、由 `PlaybackSlotView` 渲染。为避免仓库里同时存在两套行模型，已删除（含 barrel 导出）。
`findActiveIndex` 保留：分页定位复用它做页内二分，语义与分页前逐字一致。
