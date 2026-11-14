// 分页时间轴模型（FR-04 / E-06：超大通话的分页回放）。
//
// 背景：回放抽屉最初一次性 `GetTimeline` 取回**全部**片段。千句以上会话首开慢、
// 内存占用高。这里把片段改造成「按页稀疏加载」的模型：
//
//   - 全局索引 = 片段在后端 `seq` 升序序列里的下标（0 .. total-1），是唯一真相；
//   - 页 = 全局索引的定长切分，`page = floor(globalIndex / pageSize)`；
//   - 已加载页存在 `pages: Map<page, Segment[]>` 里，未加载的索引**没有数据**；
//   - 首屏只请求第一页（`PLAYBACK_PAGE_SIZE` = 200 条），其余页由滚动 / 播放位置驱动补齐。
//
// 本文件是**纯函数 + 纯数据**：不 import React、不碰 DOM、不 import bindings 的
// 运行时值（只借类型），因此可以直接被单测驱动，也可以被列表渲染与 hook 共用。
//
// 契约事实（读 `internal/transcribe/timeline.go` 与 bindings 确认）：
//   - `GetTimelinePage(sessionId, offset, limit)` →
//     { sessionId, total, offset, limit, segments, durationMs, markers }
//     后端把 limit 钳到 [1, 2000]，默认 500。**durationMs 每页都给；
//     markers 只在 offset==0（首屏页）下发**，之后各页为空——标记数量少，
//     每页重复下发没有意义，前端首屏拿到后自行保留。
//   - `GetTimeline(sessionId)` → { sessionId, durationMs, segments, markers }，
//     后端把 segments 截到 20000 条。小会话一次性取全用它。
//
// 因此 markers 的取法（见 usePagedTimeline）：小会话（`segmentCount <= PLAYBACK_INLINE_MAX`）
// 一次 `GetTimeline` 拿全（与旧行为完全一致）；大会话只取首屏页，markers/durationMs
// 随该页一起回来，**不再**为了标记补发整段请求（这正是分页要省掉的开销）。
import type { Marker, Segment, Timeline } from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { findActiveIndex } from './PlaybackService';

// —— 可调参数（都在 UI 注释里说明过「首次只请求 N 条」） ——

// 一页的条数：首屏与增量页同尺寸（200 条 ≈ 3~5 屏歌词，太少会让滚动频繁触发请求，
// 太多则失去分页意义）。后端上限 2000，这里不逼近上限。
export const PLAYBACK_PAGE_SIZE = 200;

// 片段总数不超过这个值时**不分页**：一次 `GetTimeline` 拿全（含 markers / durationMs）。
// 这个量级（1500 行 ≈ 数千字）单次请求仍在几十毫秒内，保持旧行为最省事也最不易出错。
export const PLAYBACK_INLINE_MAX = 1500;

// 后端 `GetTimeline` 会把片段截到 20000 条；超过这个总数时补 markers 的代价过高，
// 直接放弃 markers（列表仍然可用），并在确认这是可接受的降级。
export const PLAYBACK_MARKER_MAX_TOTAL = 20000;

// 高亮附近预取的条数：播放时高亮每秒推进 1~2 句，提前 60 句（≈ 半页）足够，
// 避免「播到页尾才开始请求」导致高亮卡在旧句。
export const PLAYBACK_PREFETCH_ROWS = 60;

// 滚动窗口一次最多补齐几页：防止一次 layout 抖动引起 10 个并发请求。
export const PLAYBACK_MAX_PAGES_PER_ENSURE = 4;

// 单次定位最多探测几页（远端 seek 的二分探测预算）。
export const PLAYBACK_PROBE_BUDGET = 8;

// —— 数据模型 ——

/**
 * 分页时间轴快照。
 *
 * `pageSize` 是模型的一部分（而不只是常量）：小会话走「一次取全」时页面尺寸
 * 直接等于 total，于是全部分片落在 page 0，下游（定位 / 标记 / 渲染）都不必分支。
 */
export interface PagedTimeline {
  readonly sessionId: string;
  // 后端给的总句数（不是已加载条数）。
  readonly total: number;
  readonly pageSize: number;
  // 已加载页：page → 该页片段（长度通常 = pageSize，末页更短）。
  readonly pages: ReadonlyMap<number, readonly Segment[]>;
  // 时间轴标记（只有 GetTimeline 能提供；拿不到时为空数组）。
  readonly markers: readonly Marker[];
  // 会话时长（毫秒）；0 = 未知。
  readonly durationMs: number;
}

/** 时间 → 全局索引的定位结果。 */
export interface LocateResult {
  // 最佳猜测的全局片段下标；-1 = 第一句之前（或没有任何片段）。
  index: number;
  // true = 结论完全由已加载数据推得（可以放心更新高亮）；
  // false = 目标位置落在未加载页里，`index` 只是「最接近的已加载句」。
  exact: boolean;
  // 需要加载才能精确的页；null = 无需再探测。
  probePage: number | null;
}

export const emptyPagedTimeline = (
  sessionId: string,
  pageSize: number = PLAYBACK_PAGE_SIZE,
): PagedTimeline => ({
  sessionId,
  total: 0,
  pageSize: pageSize > 0 ? pageSize : PLAYBACK_PAGE_SIZE,
  pages: new Map<number, readonly Segment[]>(),
  markers: [],
  durationMs: 0,
});

// —— 全局索引 ↔ 页内索引的换算（纯函数，逐条单测的落点） ——

/** 全局索引 → 页号。越界（负数）返回 -1，便于调用方一次性排除非法输入。 */
export const pageForGlobalIndex = (index: number, pageSize: number = PLAYBACK_PAGE_SIZE): number => {
  if (!Number.isFinite(index) || index < 0) return -1;
  const size = pageSize > 0 ? pageSize : PLAYBACK_PAGE_SIZE;
  return Math.floor(index / size);
};

/** 页号 + 页内下标 → 全局索引。 */
export const globalIndexOf = (
  page: number,
  indexInPage: number,
  pageSize: number = PLAYBACK_PAGE_SIZE,
): number => {
  const size = pageSize > 0 ? pageSize : PLAYBACK_PAGE_SIZE;
  const p = Number.isFinite(page) && page > 0 ? Math.floor(page) : 0;
  const i = Number.isFinite(indexInPage) && indexInPage > 0 ? Math.floor(indexInPage) : 0;
  return p * size + i;
};

/** 总页数（total=0 时为 0）。 */
export const pageCountOf = (total: number, pageSize: number = PLAYBACK_PAGE_SIZE): number => {
  if (!Number.isFinite(total) || total <= 0) return 0;
  const size = pageSize > 0 ? pageSize : PLAYBACK_PAGE_SIZE;
  return Math.ceil(total / size);
};

/** 某页覆盖的全局索引闭区间（末页按 total 裁剪）。空页返回 null。 */
export const indexRangeOfPage = (
  page: number,
  total: number,
  pageSize: number = PLAYBACK_PAGE_SIZE,
): { first: number; last: number } | null => {
  if (page < 0 || total <= 0) return null;
  const first = globalIndexOf(page, 0, pageSize);
  if (first >= total) return null;
  const size = pageSize > 0 ? pageSize : PLAYBACK_PAGE_SIZE;
  return { first, last: Math.min(total, first + size) - 1 };
};

/** 覆盖 [firstIndex, lastIndex] 的页号列表（升序，可能为空）。 */
export const pagesCoveringIndexRange = (
  firstIndex: number,
  lastIndex: number,
  total: number,
  pageSize: number = PLAYBACK_PAGE_SIZE,
): number[] => {
  if (total <= 0) return [];
  const lo = Math.max(0, Math.min(firstIndex, lastIndex));
  const hi = Math.min(total - 1, Math.max(firstIndex, lastIndex));
  if (hi < lo) return [];
  const firstPage = pageForGlobalIndex(lo, pageSize);
  const lastPage = pageForGlobalIndex(hi, pageSize);
  if (firstPage < 0 || lastPage < firstPage) return [];
  const out: number[] = [];
  for (let page = firstPage; page <= lastPage; page++) out.push(page);
  return out;
};

// —— 读 / 写 ——

/** 已加载页的页号（升序）。 */
export const loadedPageNumbers = (state: PagedTimeline): number[] =>
  [...state.pages.keys()].sort((a, b) => a - b);

/** 全局索引 → 片段；未加载返回 undefined（区别于「索引越界」用 hasIndex 判断）。 */
export const segmentAt = (state: PagedTimeline, index: number): Segment | undefined => {
  if (index < 0 || index >= state.total) return undefined;
  const page = pageForGlobalIndex(index, state.pageSize);
  const bucket = state.pages.get(page);
  if (bucket === undefined) return undefined;
  return bucket[index - globalIndexOf(page, 0, state.pageSize)];
};

/** 该全局索引所在页是否已加载。 */
export const isIndexLoaded = (state: PagedTimeline, index: number): boolean => {
  if (index < 0 || index >= state.total) return false;
  return state.pages.has(pageForGlobalIndex(index, state.pageSize));
};

/** 已加载片段的全局索引（升序，最多 total 个）。 */
export const loadedIndexes = (state: PagedTimeline): number[] => {
  const out: number[] = [];
  for (const page of loadedPageNumbers(state)) {
    const bucket = state.pages.get(page);
    if (bucket === undefined) continue;
    const base = globalIndexOf(page, 0, state.pageSize);
    for (let i = 0; i < bucket.length; i++) {
      const index = base + i;
      if (index < state.total) out.push(index);
    }
  }
  return out;
};

/** 已加载片段（按全局顺序拼接，跨页缺口被跳过）。供「没有 total 也要列点东西」的兜底用。 */
export const loadedSegments = (state: PagedTimeline): Segment[] => {
  const out: Segment[] = [];
  for (const index of loadedIndexes(state)) {
    const seg = segmentAt(state, index);
    if (seg !== undefined) out.push(seg);
  }
  return out;
};

/** 合并一页结果。`page` 由调用方给出（后端 offset 会被钳，页号以请求为准）。 */
export const withPage = (
  state: PagedTimeline,
  page: number,
  segments: readonly Segment[],
  total: number,
): PagedTimeline => {
  const pages = new Map(state.pages);
  pages.set(page, segments);
  return {
    ...state,
    total: total >= 0 ? total : state.total,
    pages,
  };
};

/** 用一次性 GetTimeline 的结果填充模型：全部片段落在 page 0，pageSize = total。 */
export const withFullTimeline = (state: PagedTimeline, timeline: Timeline): PagedTimeline => {
  const segments = timeline.segments;
  const total = segments.length;
  return {
    sessionId: state.sessionId,
    // total 以实际拿到的条数为准；后端截断到 20000 时高亮与滚动仍然自洽。
    total,
    pageSize: total > 0 ? total : state.pageSize,
    pages: new Map<number, readonly Segment[]>([[0, segments]]),
    markers: timeline.markers,
    durationMs: timeline.durationMs,
  };
};

/** 只更新 markers / durationMs（大会话的空闲补取）。 */
export const withTimelineMeta = (
  state: PagedTimeline,
  markers: readonly Marker[],
  durationMs: number,
): PagedTimeline => ({
  ...state,
  markers,
  durationMs: durationMs > 0 ? durationMs : state.durationMs,
});

// —— 时间 → 全局索引（跨未加载页的定位） ——

// 页内二分：**直接复用** findActiveIndex（分页前的唯一高亮算法）。
// 语义必须与旧实现逐字一致：取 startMs <= positionMs 的最后一个片段，
// 位置在第一句之前返回 -1。页内下标与全局下标只差一个页基址。
const lastIndexAtOrBefore = (bucket: readonly Segment[], positionMs: number): number =>
  findActiveIndex(bucket, positionMs);

// 页的「时间边界」：首句 startMs 与末句 startMs（都是已加载页才有的信息）。
interface PageTimeBounds {
  page: number;
  firstMs: number;
  lastMs: number;
}

const pageTimeBounds = (state: PagedTimeline): PageTimeBounds[] => {
  const out: PageTimeBounds[] = [];
  for (const page of loadedPageNumbers(state)) {
    const bucket = state.pages.get(page);
    if (bucket === undefined || bucket.length === 0) continue;
    const first = bucket[0];
    const last = bucket[bucket.length - 1];
    if (first === undefined || last === undefined) continue;
    out.push({ page, firstMs: first.startMs, lastMs: last.startMs });
  }
  return out;
};

// 播放中的 O(1) 快车道：先用上一帧的下标（或 +1）判断，避免每帧都扫页表。
const locateNearHint = (
  state: PagedTimeline,
  positionMs: number,
  hintIndex: number,
): number | null => {
  if (hintIndex < 0 || hintIndex >= state.total) return null;
  // 只看 hint 与 hint+1：播放时位置单调前进，走远了两步内就会落到别的页，
  // 交给慢速路径处理（慢速路径结论等价，只是多几次比较）。
  for (const candidate of [hintIndex, hintIndex + 1]) {
    const seg = segmentAt(state, candidate);
    if (seg === undefined) continue;
    if (seg.startMs <= positionMs) {
      const next = segmentAt(state, candidate + 1);
      if (next === undefined) {
        // 下一句没加载：若 candidate 是最后一句则结论精确，否则需要下一页来确认。
        if (candidate + 1 >= state.total) return candidate;
        return null;
      }
      if (next.startMs > positionMs) return candidate;
      continue;
    }
  }
  return null;
};

/**
 * 把播放位置映射成**全局片段下标**。
 *
 * 语义与旧实现（`findActiveIndex`）保持一致：
 *   - 取 startMs <= positionMs 的最后一个片段 → 落在两句间隙时保持上一句（不抖动）；
 *   - 位置在第一句之前 → -1（UI 显示「尚未开始」）。
 *
 * 分页带来的新情况：目标位置可能落在**未加载**的页里。此时不能返回 -1（那会让
 * 高亮直接消失），而是返回「最接近的已加载句」并把要探测的页交给调用方去加载；
 * 页到达后重算即精确。
 */
export const locateIndexByTime = (
  state: PagedTimeline,
  positionMs: number,
  hintIndex: number = -1,
): LocateResult => {
  if (state.total <= 0) return { index: -1, exact: true, probePage: null };

  const near = locateNearHint(state, positionMs, hintIndex);
  if (near !== null) return { index: near, exact: true, probePage: null };

  const bounds = pageTimeBounds(state);
  if (bounds.length === 0) {
    // 一页都没有：只能请求第一页。
    return { index: -1, exact: false, probePage: state.total > 0 ? 0 : null };
  }

  // 位置在所有已加载页之前。
  const earliest = bounds[0];
  if (earliest !== undefined && positionMs < earliest.firstMs) {
    if (earliest.page === 0) {
      // 第一页已加载且位置在第一句之前：这就是「尚未开始」。
      return { index: -1, exact: true, probePage: null };
    }
    return { index: -1, exact: false, probePage: probePageForMs(state, positionMs) };
  }

  // 找到覆盖 positionMs 的页（firstMs <= pos <= lastMs），页内二分即可精确。
  let best: { page: number; indexInPage: number } | null = null;
  for (const bound of bounds) {
    if (bound.firstMs <= positionMs && positionMs <= bound.lastMs) {
      const bucket = state.pages.get(bound.page);
      if (bucket === undefined) continue;
      const indexInPage = lastIndexAtOrBefore(bucket, positionMs);
      if (indexInPage < 0) continue;
      best = { page: bound.page, indexInPage };
      break;
    }
  }

  if (best !== null) {
    const index = globalIndexOf(best.page, best.indexInPage, state.pageSize);
    // 页内精确，但「下一句是否属于同一页」决定这是否是**全局**精确：
    // 该页不是末页时，下一句必然在下一页（未加载也能确定 startMs > pos）。
    const bucket = state.pages.get(best.page);
    const isLastOfPage = bucket !== undefined && best.indexInPage === bucket.length - 1;
    const range = indexRangeOfPage(best.page, state.total, state.pageSize);
    const pageIsLast = range !== null && range.last === state.total - 1;
    if (!isLastOfPage || pageIsLast) {
      return { index, exact: true, probePage: null };
    }
    // 目标刚好落在页尾：位置可能已经越过了下一页的首句，需要下一页来确认。
    return { index, exact: false, probePage: best.page + 1 };
  }

  // 落在「已加载页之间的空档」或「最后一个已加载页之后」：
  // 用已加载页的时间边界夹出一个尽可能小的页区间，交给 probePageForMs 二分。
  let closestIndex = -1;
  let closestPage = -1;
  for (const bound of bounds) {
    if (bound.firstMs <= positionMs) {
      const bucket = state.pages.get(bound.page);
      if (bucket !== undefined) {
        const base = globalIndexOf(bound.page, 0, state.pageSize);
        closestIndex = base + bucket.length - 1;
        closestPage = bound.page;
      }
    }
  }
  if (closestIndex >= state.total) closestIndex = state.total - 1;

  // 已加载页把页号空间夹成 (below, above)。当两者**页号相邻**时，中间没有未知页，
  // 位置落在时间空档里 → 答案就是 below 页的最后一句，这就是精确结论（不必再探测）。
  const bracket = pageBracket(state, positionMs);
  if (bracket.below >= 0 && bracket.above === bracket.below + 1) {
    return { index: closestIndex, exact: true, probePage: null };
  }

  // 特例：位置在**整个会话**的最后一句之后（末页已加载）——这就是精确答案，
  // 否则高亮会在片尾永久停在「非精确」状态。
  if (closestPage >= 0) {
    const range = indexRangeOfPage(closestPage, state.total, state.pageSize);
    const isSessionTail = range !== null && range.last === state.total - 1;
    if (isSessionTail) return { index: closestIndex, exact: true, probePage: null };
  }

  return { index: closestIndex, exact: false, probePage: probePageForMs(state, positionMs) };
};

/**
 * 已加载页把页号空间夹出的区间：
 *   - `below` = 最后一个「首句时间 <= positionMs」的已加载页（-1 = 没有）；
 *   - `above` = 第一个「首句时间 > positionMs」的已加载页（-1 = 没有）。
 * 由于片段时间单调，答案所在页必落在 (below, above]。
 */
export const pageBracket = (
  state: PagedTimeline,
  positionMs: number,
): { below: number; above: number } => {
  let below = -1;
  let above = -1;
  for (const bound of pageTimeBounds(state)) {
    if (bound.firstMs <= positionMs) {
      below = Math.max(below, bound.page);
    } else {
      above = above < 0 ? bound.page : Math.min(above, bound.page);
    }
  }
  return { below, above };
};

/**
 * 下一个该探测的页号（纯函数）。
 *
 * 思路：已加载页把页号空间夹成 [below, above] 一段（`below` = 最后一个「首句时间 <= 位置」
 * 的已加载页，`above` = 第一个「首句时间 > 位置」的已加载页），答案必落在 (below, above]。
 * 在这一段里先用**时间线性插值**猜一页（绝大多数远端 seek 一步命中），猜中边界就退回二分中点。
 * 若算出来的页其实已经加载（无从收敛），返回 null —— 调用方据此停止探测，避免空转。
 */
export const probePageForMs = (state: PagedTimeline, positionMs: number): number | null => {
  const lastPage = pageCountOf(state.total, state.pageSize) - 1;
  if (lastPage <= 0) return null;

  const bounds = pageTimeBounds(state);
  const { below, above } = pageBracket(state, positionMs);

  const lo = below < 0 ? 0 : below;
  const hi = above < 0 ? lastPage : above;
  if (lo >= hi) return null;

  // 时间插值：包住目标位置的两个已加载页边界之间做线性映射。
  const belowBound = below >= 0 ? bounds.find((b) => b.page === below) : undefined;
  const aboveBound = above >= 0 ? bounds.find((b) => b.page === above) : undefined;

  let candidate: number;
  if (belowBound !== undefined && aboveBound !== undefined) {
    const span = aboveBound.firstMs - belowBound.lastMs;
    const ratio = span > 0 ? (positionMs - belowBound.lastMs) / span : 0.5;
    const guess = below + Math.round(ratio * (above - below));
    candidate = guess > lo && guess < hi ? guess : (lo + hi) >> 1;
  } else if (belowBound !== undefined) {
    // 目标在所有已加载页之后：按整体时长比例猜（durationMs 未知时保守 +1 页）。
    let guess = lo + 1;
    if (state.durationMs > 0) {
      const ratio = Math.min(0.999, Math.max(0, positionMs / state.durationMs));
      const byTime = Math.round(ratio * lastPage);
      if (byTime > lo && byTime <= hi) guess = byTime;
    }
    candidate = guess;
  } else if (aboveBound !== undefined) {
    // 目标在所有已加载页之前：按首句时间比例往前猜。
    const ratio = aboveBound.firstMs > 0 ? Math.min(0.999, Math.max(0, positionMs / aboveBound.firstMs)) : 0;
    const guess = Math.round(ratio * hi);
    candidate = guess < hi && guess >= lo ? guess : hi - 1;
  } else {
    candidate = (lo + hi) >> 1;
  }

  if (candidate < 0 || candidate > lastPage) return null;
  // 已经加载的页再探也不会改变结论（说明区间收敛到底了）：停止探测。
  if (state.pages.has(candidate)) {
    const mid = (lo + hi) >> 1;
    if (mid === candidate || state.pages.has(mid)) return null;
    return mid;
  }
  return candidate;
};

// —— 标记（markers）与片段混排 ——

/**
 * 把时间轴标记挂到「它前面那一句」的槽位上（槽位 = 全局片段下标）。
 *
 * 与旧 `buildTimelineRows` 的混排语义一致：标记按 atMs 插到「第一个 startMs >= atMs
 * 的片段」之前；atMs 落在第一句之前的标记挂到槽位 0。
 *
 * 分页下的差异（有意为之，已写进 DEV-ISSUES）：只能按**已加载**的片段定位，
 * 所以某个标记要等到它周围那一页到达才会出现；尾部（最后一页之后）的标记
 * 在最后一页加载前也不会出现。因为列表只渲染可视窗口、而窗口会驱动加载，
 * 用户滚动到标记附近时它已经在位。
 */
export const markersBySlot = (state: PagedTimeline): ReadonlyMap<number, readonly Marker[]> => {
  const out = new Map<number, Marker[]>();
  if (state.markers.length === 0 || state.total <= 0) return out;

  const sorted = [...state.markers].sort((a, b) => a.atMs - b.atMs);
  let mi = 0;
  for (const index of loadedIndexes(state)) {
    if (mi >= sorted.length) break;
    const seg = segmentAt(state, index);
    if (seg === undefined) continue;
    while (mi < sorted.length) {
      const marker = sorted[mi];
      if (marker === undefined || marker.atMs > seg.startMs) break;
      const bucket = out.get(index) ?? [];
      bucket.push(marker);
      out.set(index, bucket);
      mi += 1;
    }
  }
  return out;
};

/**
 * 已加载片段在 `positionMs` 之后的第一句（供「下一句」按钮越页使用）。
 * 只在已加载数据里找；找不到返回 undefined，调用方应触发加载。
 */
export const nextLoadedIndexAfter = (state: PagedTimeline, index: number): number | undefined => {
  let cursor = index + 1;
  while (cursor < state.total) {
    if (isIndexLoaded(state, cursor)) return cursor;
    // 跳页查找（避免逐条走 200 次）。
    const page = pageForGlobalIndex(cursor, state.pageSize);
    const bucket = state.pages.get(page);
    if (bucket === undefined) {
      const nextPage = page + 1;
      const range = indexRangeOfPage(nextPage, state.total, state.pageSize);
      if (range === null) return undefined;
      cursor = range.first;
      continue;
    }
    return cursor;
  }
  return undefined;
};

/** 已加载片段在 `positionMs` 之前的最后一句。 */
export const prevLoadedIndexBefore = (state: PagedTimeline, index: number): number | undefined => {
  let cursor = index - 1;
  while (cursor >= 0) {
    if (isIndexLoaded(state, cursor)) return cursor;
    const page = pageForGlobalIndex(cursor, state.pageSize);
    if (!state.pages.has(page)) {
      const range = indexRangeOfPage(page - 1, state.total, state.pageSize);
      if (range === null) return undefined;
      cursor = range.last;
      continue;
    }
    return cursor;
  }
  return undefined;
};
