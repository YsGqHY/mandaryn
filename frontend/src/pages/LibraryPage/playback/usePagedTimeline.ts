// 分页时间轴的数据源（ViewModel 下层）：负责「按页取片段」与「跨未加载页的定位」。
//
// 分层：View(PlaybackDrawer) → usePlayback(VM) → usePagedTimeline(本文件) → PlaybackService → bindings。
// 本文件不 import bindings，只经 PlaybackService 说话。
//
// 加载策略（对应 E-06「千句以上首开慢 / 内存高」）：
//   1) 小会话（segmentCount ≤ PLAYBACK_INLINE_MAX = 1500）**不分页**：一次 GetTimeline 拿全，
//      行为与分页前完全一致（markers / durationMs 都在这一次里）。
//   2) 大会话：**首屏只请求第一页**（PLAYBACK_PAGE_SIZE = 200 条）。拿到 `total` 后，
//      其余页由两件事驱动补齐：
//        - 列表可视窗口变化（滚动）→ ensureIndexRange；
//        - 播放位置移动 → locate 返回的 probePage，或高亮附近的预取窗口。
//   3) 大会话的 markers / durationMs 只能靠 GetTimeline（TimelinePage 契约里没有），
//      因此在首屏渲染完成后的**空闲时段**补取一次、只留 markers 与时长，片段丢弃。
//      这是当前后端能力的边界，记在 docs/DEV-ISSUES-F-paged.md。
//
// 并发与幂等：同一页永不同时请求两次（inflight 去重），已加载页不再请求，
// 同时在飞的请求数有上限（滚轴猛拖时不会一次打出几十个请求）。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  PLAYBACK_INLINE_MAX,
  PLAYBACK_MAX_PAGES_PER_ENSURE,
  PLAYBACK_PAGE_SIZE,
  PLAYBACK_PROBE_BUDGET,
  PlaybackService,
  emptyPagedTimeline,
  locateIndexByTime,
  pageForGlobalIndex,
  withFullTimeline,
  withPage,
  withTimelineMeta,
  type AppError,
  type LocateResult,
  type PagedTimeline,
  type Segment,
} from '@/services';

// 同时在飞的页请求上限：3 页 ≈ 600 行，足够跟上滚轴拖动，又不至于把桥打满。
const MAX_INFLIGHT_PAGES = 3;

// 播放位置跳变超过这个距离时，重置「探测预算」（用户做了新的远端 seek，值得再探几页）。
const PROBE_BUDGET_RESET_MS = 3000;

export interface UsePagedTimelineOptions {
  sessionId: string;
  // 会话列表里已知的句数（只是**决定走哪条加载路径**的提示，不参与分页算术）。
  segmentCountHint: number;
}

export interface UsePagedTimelineResult {
  timeline: PagedTimeline;
  // 首屏还没到（抽屉只该显示骨架）。
  loadingFirst: boolean;
  error: AppError | null;
  // 全部片段都在内存里（小会话，或恰好只有一页）：滚动不再需要请求。
  complete: boolean;
  // 幂等：已加载 / 在飞 / 超出上限的页会被跳过。
  ensurePages: (pages: readonly number[]) => void;
  // 按全局索引区间请求页（滚动窗口驱动）。
  ensureIndexRange: (firstIndex: number, lastIndex: number) => void;
  // 时间 → 全局索引；顺带请求定位所需的页（受探测预算约束）。
  locate: (positionMs: number, hintIndex: number) => LocateResult;
  reload: () => void;
}

export const usePagedTimeline = ({
  sessionId,
  segmentCountHint,
}: UsePagedTimelineOptions): UsePagedTimelineResult => {
  const [timeline, setTimeline] = useState<PagedTimeline>(() =>
    emptyPagedTimeline(sessionId, PLAYBACK_PAGE_SIZE),
  );
  const [loadingFirst, setLoadingFirst] = useState<boolean>(true);
  const [error, setError] = useState<AppError | null>(null);

  // 最新快照：locate 每帧都会调用，不能读闭包里的旧值。
  const timelineRef = useRef<PagedTimeline>(timeline);
  const inflightRef = useRef<Set<number>>(new Set<number>());
  const aliveRef = useRef<boolean>(true);
  // 每次 reload / 换会话自增：晚到的响应据此丢弃。
  const generationRef = useRef<number>(0);
  const probeBudgetRef = useRef<{ anchorMs: number; count: number }>({ anchorMs: -1, count: 0 });

  useEffect(() => {
    timelineRef.current = timeline;
  }, [timeline]);

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  // 换会话：整份状态归零（不同会话的页号空间没有可比性）。
  useEffect(() => {
    generationRef.current += 1;
    inflightRef.current = new Set<number>();
    probeBudgetRef.current = { anchorMs: -1, count: 0 };
    const fresh = emptyPagedTimeline(sessionId, PLAYBACK_PAGE_SIZE);
    timelineRef.current = fresh;
    setTimeline(fresh);
    setLoadingFirst(true);
    setError(null);
  }, [sessionId]);

  const applyPage = useCallback(
    (page: number, segments: readonly Segment[], total: number, generation: number): void => {
      if (!aliveRef.current || generation !== generationRef.current) return;
      setTimeline((prev) => {
        const next = withPage(prev, page, segments, total);
        timelineRef.current = next;
        return next;
      });
    },
    [],
  );

  const fetchPage = useCallback(
    (page: number): void => {
      if (page < 0) return;
      const current = timelineRef.current;
      if (current.pages.has(page)) return;
      if (inflightRef.current.has(page)) return;
      if (inflightRef.current.size >= MAX_INFLIGHT_PAGES) return;
      inflightRef.current.add(page);
      const generation = generationRef.current;
      const offset = page * current.pageSize;
      void PlaybackService.getTimelinePage(sessionId, offset, current.pageSize)
        .then((result) => {
          // 后端会把 offset 钳到合法区间：页号以**请求**为准，不用返回的 offset 反推。
          applyPage(page, result.segments, result.total, generation);
        })
        .catch((err: unknown) => {
          if (!aliveRef.current || generation !== generationRef.current) return;
          // 单页失败不炸整个抽屉：保留已有数据，把错误交给上层做非阻塞提示。
          setError(err as AppError);
        })
        .finally(() => {
          inflightRef.current.delete(page);
        });
    },
    [applyPage, sessionId],
  );

  const ensurePages = useCallback(
    (pages: readonly number[]): void => {
      const current = timelineRef.current;
      let issued = 0;
      for (const page of pages) {
        if (issued >= PLAYBACK_MAX_PAGES_PER_ENSURE) break;
        if (page < 0 || current.pages.has(page)) continue;
        if (inflightRef.current.has(page)) continue;
        fetchPage(page);
        issued += 1;
      }
    },
    [fetchPage],
  );

  const ensureIndexRange = useCallback(
    (firstIndex: number, lastIndex: number): void => {
      const current = timelineRef.current;
      if (current.total <= 0) return;
      const firstPage = pageForGlobalIndex(Math.max(0, firstIndex), current.pageSize);
      const lastPage = pageForGlobalIndex(Math.min(current.total - 1, lastIndex), current.pageSize);
      if (firstPage < 0) return;
      const pages: number[] = [];
      for (let page = firstPage; page <= lastPage; page++) pages.push(page);
      ensurePages(pages);
    },
    [ensurePages],
  );

  // —— 首屏 ——
  const loadFirst = useCallback((): void => {
    const generation = generationRef.current;
    setLoadingFirst(true);
    setError(null);

    if (segmentCountHint > 0 && segmentCountHint <= PLAYBACK_INLINE_MAX) {
      // 小会话：一次取全（含 markers / durationMs），与分页前的行为一致。
      void PlaybackService.getTimeline(sessionId)
        .then((full) => {
          if (!aliveRef.current || generation !== generationRef.current) return;
          setTimeline((prev) => {
            const next = withFullTimeline(prev, full);
            timelineRef.current = next;
            return next;
          });
        })
        .catch((err: unknown) => {
          if (!aliveRef.current || generation !== generationRef.current) return;
          setError(err as AppError);
        })
        .finally(() => {
          if (aliveRef.current && generation === generationRef.current) setLoadingFirst(false);
        });
      return;
    }

    // 大会话（或句数未知）：**只取第一页**（PLAYBACK_PAGE_SIZE = 200 条）。
    // 首屏页同时带回 durationMs 与 markers（API-4.2 offset==0 时下发），
    // 因此大会话不需要再发一次整段请求去拿标记。
    void PlaybackService.getTimelinePage(sessionId, 0, PLAYBACK_PAGE_SIZE)
      .then((page) => {
        if (!aliveRef.current || generation !== generationRef.current) return;
        setTimeline((prev) => {
          const next = withTimelineMeta(
            withPage(prev, 0, page.segments, page.total),
            page.markers ?? [],
            page.durationMs ?? 0,
          );
          timelineRef.current = next;
          return next;
        });
      })
      .catch((err: unknown) => {
        if (!aliveRef.current || generation !== generationRef.current) return;
        setError(err as AppError);
      })
      .finally(() => {
        if (aliveRef.current && generation === generationRef.current) setLoadingFirst(false);
      });
  }, [segmentCountHint, sessionId]);

  useEffect(() => {
    loadFirst();
  }, [loadFirst]);

  // —— 大会话的 markers / durationMs 由首屏页带回，无需额外请求 ——
  // 旧的实现在这里空闲时补发一次 GetTimeline 只为拿标记（因为 TimelinePage 契约
  // 曾经没有这两个字段）。API-4.2 补齐后该请求已删除：省掉一次整段拉取，
  // 也让「大会话」真正只付一页的代价。

  // —— 定位（每帧调用）：纯函数算结论，需要新页时顺手请求 ——
  const locate = useCallback(
    (positionMs: number, hintIndex: number): LocateResult => {
      const current = timelineRef.current;
      const result = locateIndexByTime(current, positionMs, hintIndex);
      if (result.probePage === null) return result;

      // 探测预算：同一段位置最多探 PLAYBACK_PROBE_BUDGET 页，防止病态数据下无限请求；
      // 位置大幅跳变（用户又 seek 了）时重新给预算。
      const budget = probeBudgetRef.current;
      if (Math.abs(positionMs - budget.anchorMs) > PROBE_BUDGET_RESET_MS) {
        probeBudgetRef.current = { anchorMs: positionMs, count: 0 };
      } else if (budget.count >= PLAYBACK_PROBE_BUDGET) {
        return result;
      }
      probeBudgetRef.current = {
        anchorMs: probeBudgetRef.current.anchorMs < 0 ? positionMs : probeBudgetRef.current.anchorMs,
        count: probeBudgetRef.current.count + 1,
      };
      fetchPage(result.probePage);
      return result;
    },
    [fetchPage],
  );

  const reload = useCallback((): void => {
    generationRef.current += 1;
    inflightRef.current = new Set<number>();
    probeBudgetRef.current = { anchorMs: -1, count: 0 };
    const fresh = emptyPagedTimeline(sessionId, PLAYBACK_PAGE_SIZE);
    timelineRef.current = fresh;
    setTimeline(fresh);
    loadFirst();
  }, [loadFirst, sessionId]);

  // 全部片段已在内存：不必再滚动加载（也用于「列表不必显示加载占位」的判断）。
  const complete = useMemo(() => {
    if (timeline.total <= 0) return true;
    const pageCount = Math.ceil(timeline.total / timeline.pageSize);
    return timeline.pages.size >= pageCount;
  }, [timeline.pageSize, timeline.pages, timeline.total]);

  return {
    timeline,
    loadingFirst,
    error,
    complete,
    ensurePages,
    ensureIndexRange,
    locate,
    reload,
  };
};
