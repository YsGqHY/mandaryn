// 歌词列表的窗口化渲染（虚拟滚动）数学 —— 纯函数，无 React / 无 DOM。
//
// 为什么自己写而不复用 `components/x-pro/data-grid`：那是表格（列宽 / 行钉住 / 展开行），
// 与「可变高度的歌词流」不是一类问题；也没有别的现成虚拟列表可用。按项目铁律
// **不引入新依赖**，所以这里只放必要的几十行数学。
//
// 坐标模型：
//   - 列表是「槽位（slot）」的一维序列，槽位 i 对应全局片段下标 i（见 pagedTimeline.ts）；
//     一个槽位的 DOM 里可能包含「若干标记行 + 一行歌词」，整体高度由实测得到；
//   - `offsets[i]` = 槽位 i 的顶部绝对偏移，`offsets[count]` = 内容总高；
//   - 未渲染过的槽位没有实测高度，用 `slotHeightOf` 的估算值（ESTIMATE）顶上。
//
// 高度为什么必须实测：正文会折行（中英混排 + 说话人名 + 「识别中」角标），
// 行高不固定；固定行高会截断转写正文，而这个功能的用途正是读正文。
// 估算值只影响「还没渲染过的槽位」的滚动条长度，不参与已渲染行的布局，
// 所以滚动过程中不会出现抖动（已渲染行永远按实测高度定位）。

// 槽位高度的估算值（像素）：一行时间戳 / 说话人 + 1~2 行正文 + 内边距。
// 只用于未测量过的槽位；实测值一旦拿到就覆盖它。
export const SLOT_HEIGHT_ESTIMATE = 76;

// 可视区上下各多渲染几行：滚动时不至于露出空白，也不至于一次渲染太多。
export const SLOT_OVERSCAN = 8;

// 视口高度还没测出来（首帧）时的兜底值：按一屏 12 行估。
export const VIEWPORT_FALLBACK_PX = SLOT_HEIGHT_ESTIMATE * 12;

/** 渲染窗口：[start, end)，end 为开区间。 */
export interface SlotWindow {
  start: number;
  end: number;
}

/** 槽位高度读取器：已测量过返回实测，否则返回估算。 */
export interface SlotMetrics {
  readonly count: number;
  readonly heightOf: (index: number) => number;
}

/**
 * 由「实测高度表 + 估算值」构造高度读取器（`measured` 里没有的走 estimate）。
 * 返回的 `heightOf` 保证有限且非负，避免脏值把偏移算崩。
 */
export const slotMetricsOf = (
  count: number,
  measured: ReadonlyMap<number, number>,
  estimate: number = SLOT_HEIGHT_ESTIMATE,
): SlotMetrics => {
  const safeEstimate = Number.isFinite(estimate) && estimate > 0 ? estimate : SLOT_HEIGHT_ESTIMATE;
  return {
    count: Math.max(0, Math.floor(count)),
    heightOf: (index: number): number => {
      const measuredHeight = measured.get(index);
      if (measuredHeight === undefined || !Number.isFinite(measuredHeight) || measuredHeight <= 0) {
        return safeEstimate;
      }
      return measuredHeight;
    },
  };
};

/**
 * 前缀和：`offsets[i]` = 槽位 i 的顶部偏移，长度 = count + 1（最后一个元素是总高）。
 * O(count)，count 最多是会话句数（后端上限 2 万），一次几毫秒以内。
 */
export const offsetsOf = (metrics: SlotMetrics): number[] => {
  const offsets = new Array<number>(metrics.count + 1);
  let acc = 0;
  for (let i = 0; i < metrics.count; i++) {
    offsets[i] = acc;
    acc += metrics.heightOf(i);
  }
  offsets[metrics.count] = acc;
  return offsets;
};

/** 内容总高（= offsets 的最后一个元素）。 */
export const totalHeightOf = (offsets: readonly number[]): number => {
  const last = offsets[offsets.length - 1];
  return last !== undefined && Number.isFinite(last) && last > 0 ? last : 0;
};

/**
 * 二分：返回「偏移 <= y」的最后一个槽位下标（即 y 落在哪个槽位里）。
 * y 在第一槽之前 → -1；count=0 → -1；y 超过末尾 → count-1。
 */
export const indexAtOffset = (offsets: readonly number[], y: number): number => {
  const count = offsets.length - 1;
  if (count <= 0) return -1;
  if (!Number.isFinite(y) || y < 0) return -1;
  const total = offsets[count];
  if (total !== undefined && y >= total) return count - 1;
  let lo = 0;
  let hi = count - 1;
  let found = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const top = offsets[mid];
    if (top === undefined) break;
    if (top <= y) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found;
};

/**
 * 可视窗口：`[scrollTop, scrollTop + viewportHeight]` 覆盖的槽位，上下各加 overscan。
 *
 * 这是窗口化渲染的唯一入口，也是「滚动锚定要稳」的落点：窗口只由 scrollTop 决定，
 * 不含任何「上一次窗口」的记忆，因此不会自己漂移（不存在正反馈）。
 */
export const visibleRange = (
  offsets: readonly number[],
  scrollTop: number,
  viewportHeight: number,
  overscan: number = SLOT_OVERSCAN,
): SlotWindow => {
  const count = offsets.length - 1;
  if (count <= 0) return { start: 0, end: 0 };
  const pad = Math.max(0, Math.floor(overscan));
  const top = Number.isFinite(scrollTop) ? Math.max(0, scrollTop) : 0;
  const viewport = Number.isFinite(viewportHeight) && viewportHeight > 0 ? viewportHeight : VIEWPORT_FALLBACK_PX;

  const first = indexAtOffset(offsets, top);
  const last = indexAtOffset(offsets, top + viewport);

  const start = Math.max(0, Math.min(count, (first < 0 ? 0 : first) - pad));
  const end = Math.max(start, Math.min(count, (last < 0 ? 0 : last) + 1 + pad));
  return { start, end };
};

/**
 * 把槽位居中时应该设置的 scrollTop（FR-04-3：偏差 ≤ 15% 视口高度，实际居中为 0）。
 *
 * 与旧实现（查询 DOM 行再用 getBoundingClientRect 反推）不同，这里用偏移直接算：
 * 目标行**不必已经渲染**，因此「高亮跳到未渲染的页」时也能一次到位，
 * 且不会读到过期的布局（避免抖动）。
 */
export const centerScrollTop = (
  offsets: readonly number[],
  index: number,
  heightOf: (index: number) => number,
  viewportHeight: number,
  maxScrollTop: number,
): number => {
  const count = offsets.length - 1;
  if (count <= 0 || index < 0 || index >= count) return 0;
  const top = offsets[index];
  if (top === undefined) return 0;
  const viewport = Number.isFinite(viewportHeight) && viewportHeight > 0 ? viewportHeight : VIEWPORT_FALLBACK_PX;
  const raw = top + heightOf(index) / 2 - viewport / 2;
  const max = Number.isFinite(maxScrollTop) && maxScrollTop > 0 ? maxScrollTop : 0;
  return Math.min(Math.max(0, raw), max);
};

/**
 * 相邻页的预取窗口：返回需要加载的页号（已由调用方按「未加载」过滤）。
 * `padRows` 让高亮附近提前备好一段，避免播到页尾才开始请求。
 */
export const indexWindowAround = (
  index: number,
  total: number,
  padRows: number,
): { first: number; last: number } => {
  if (total <= 0) return { first: 0, last: -1 };
  const pad = Math.max(0, Math.floor(padRows));
  const anchor = index < 0 ? 0 : Math.min(index, total - 1);
  return { first: Math.max(0, anchor - pad), last: Math.min(total - 1, anchor + pad) };
};
