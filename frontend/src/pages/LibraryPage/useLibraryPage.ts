import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  CaptureService,
  SessionService,
  canStartTranscription,
  toAppError,
  type AppError,
  type DeleteResult,
  type ExportFormat,
  type ListQuery,
  type Session,
  type Source,
  type TranscriptionState,
} from '@/services';

// 关键词输入的防抖：每敲一个字都查一次会话库代价不值得（片段全文检索是 LIKE）。
const KEYWORD_DEBOUNCE_MS = 300;

export interface LibraryFilters {
  // 关键词：后端同时匹配标题与片段正文。
  keyword: string;
  // 时间范围：'YYYY-MM-DD' 或 ''（不限）。
  from: string;
  to: string;
  // 来源应用：sourceIdLike，'' = 全部。
  sourceId: string;
  // 转写状态：none|queued|running|done|failed，'' = 全部。
  transcriptionStatus: string;
  // 时长区间（秒）：**只在当前页内筛选**（后端 ListQuery 没有时长条件）。
  minSeconds: string;
  maxSeconds: string;
}

const EMPTY_FILTERS: LibraryFilters = {
  keyword: '',
  from: '',
  to: '',
  sourceId: '',
  transcriptionStatus: '',
  minSeconds: '',
  maxSeconds: '',
};

export interface LibrarySourceOption {
  value: string;
  label: string;
}

export interface UseLibraryPageResult {
  // —— 列表 ——
  items: Session[];
  // visibleItems = items 再经「时长区间」前端筛选后的结果（其余条件都走后端）。
  visibleItems: Session[];
  total: number;
  page: number;
  pageCount: number;
  pageSize: number;
  loading: boolean;
  error: AppError | null;
  // 时长筛选是否把当前页里的行筛掉了（UI 需要说明「仅当前页生效」）。
  durationFiltered: boolean;
  // —— 筛选 ——
  filters: LibraryFilters;
  // 来源应用下拉项（当前运行的源 + 历史会话里出现过的源合并）
  sourceOptions: LibrarySourceOption[];
  setFilter: (patch: Partial<LibraryFilters>) => void;
  resetFilters: () => void;
  setPage: (page: number) => void;
  setPageSize: (pageSize: number) => void;
  reload: () => Promise<void>;
  dismissError: () => void;
  // —— 改名（弹窗） ——
  renaming: Session | null;
  renameDraft: string;
  beginRename: (session: Session) => void;
  setRenameDraft: (value: string) => void;
  cancelRename: () => void;
  submitRename: () => Promise<boolean>;
  // —— 删除（弹窗 + 原生确认） ——
  deleting: Session | null;
  deleteAudio: boolean;
  setDeleteAudio: (value: boolean) => void;
  beginDelete: (session: Session) => void;
  cancelDelete: () => void;
  // 真正执行删除；返回逐项结果（音频删不掉的会话在 audioFailed 里）。
  confirmDelete: () => Promise<DeleteResult | null>;
  // —— 导出 ——
  exportSession: (sessionId: string, format: ExportFormat, targetPath: string) => Promise<string | null>;
  // —— 转写 ——
  // 每个会话的最新转写状态（事件驱动）；没有条目时以列表里的 transcriptionStatus 为准。
  transcripts: Record<string, TranscriptionState>;
  activeTranscription: (session: Session) => TranscriptionState | null;
  statusOf: (session: Session) => string;
  canTranscribe: (session: Session) => boolean;
  startTranscription: (session: Session) => Promise<void>;
  cancelTranscription: (sessionId: string) => Promise<void>;
  // 行级忙标记（改名 / 删除 / 转写 / 导出共用，避免重复点击）
  busyId: string | null;
  // —— 说话人 / 角色侧栏 ——
  // 当前在侧栏里管理说话人的会话；null = 侧栏关闭。
  speakerSession: Session | null;
  openSpeakers: (session: Session) => void;
  closeSpeakers: () => void;
  // —— 回放抽屉（FR-04） ——
  // 当前在回放抽屉里打开的会话；null = 抽屉关闭。与 speakerSession 互斥。
  playbackSession: Session | null;
  openPlayback: (session: Session) => void;
  closePlayback: () => void;
}

// 'YYYY-MM-DD' → 当天 00:00 / 23:59:59 的 RFC3339（本地时区，含端点）。
const dayStart = (date: string): string => {
  if (date === '') return '';
  const parsed = new Date(`${date}T00:00:00`);
  return Number.isNaN(parsed.getTime()) ? '' : parsed.toISOString();
};

const dayEnd = (date: string): string => {
  if (date === '') return '';
  const parsed = new Date(`${date}T23:59:59`);
  return Number.isNaN(parsed.getTime()) ? '' : parsed.toISOString();
};

// 秒 → 毫秒；空 / 非法返回 null（表示不限）。
const secondsToMs = (value: string): number | null => {
  if (value === '') return null;
  const seconds = Number(value);
  if (!Number.isFinite(seconds) || seconds < 0) return null;
  return Math.round(seconds * 1000);
};

// LibraryPage ViewModel：
//   - 除「时长区间」外，所有条件都交给后端 List（关键词 / 时间范围 / 来源 / 转写状态）；
//   - 时长区间后端契约里没有，只能对**当前页**做前端筛选，UI 必须说明这一点；
//   - 转写进度走 transcription:state 事件，类型转换与调用都在 services 层；
//   - 原生对话框（确认删除 / 另存为 / 提示）由 View 负责，VM 只做数据与调用。
export const useLibraryPage = (): UseLibraryPageResult => {
  const [items, setItems] = useState<Session[]>([]);
  const [total, setTotal] = useState<number>(0);
  const [page, setPageState] = useState<number>(1);
  const [pageSize, setPageSizeState] = useState<number>(20);
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<AppError | null>(null);
  const [filters, setFilters] = useState<LibraryFilters>(EMPTY_FILTERS);
  // 防抖后的关键词（真正进查询的那个）
  const [appliedKeyword, setAppliedKeyword] = useState<string>('');
  const [sourceOptions, setSourceOptions] = useState<LibrarySourceOption[]>([]);
  const [transcripts, setTranscripts] = useState<Record<string, TranscriptionState>>({});
  const transcriptEventRevisions = useRef(new Map<string, number>());
  const [busyId, setBusyId] = useState<string | null>(null);
  const [renaming, setRenaming] = useState<Session | null>(null);
  const [renameDraft, setRenameDraft] = useState<string>('');
  const [deleting, setDeleting] = useState<Session | null>(null);
  const [deleteAudio, setDeleteAudio] = useState<boolean>(true);
  // 事件驱动刷新：转写完成后让列表徽标跟上。
  const [reloadToken, setReloadToken] = useState<number>(0);
  // 说话人侧栏：只存被选中的会话（侧栏自己的数据由它的 ViewModel 拉）。
  const [speakerSession, setSpeakerSession] = useState<Session | null>(null);
  // 回放抽屉：同样只存被选中的会话（时间轴 / 音频由 playback 的 ViewModel 拉）。
  const [playbackSession, setPlaybackSession] = useState<Session | null>(null);
  const aliveRef = useRef<boolean>(true);

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  // 关键词防抖
  useEffect(() => {
    const timer = window.setTimeout(() => setAppliedKeyword(filters.keyword), KEYWORD_DEBOUNCE_MS);
    return () => window.clearTimeout(timer);
  }, [filters.keyword]);

  const buildQuery = useCallback(
    (): ListQuery => ({
      keyword: appliedKeyword,
      sourceIdLike: filters.sourceId,
      // 会话录制状态不做筛选（UI 未暴露），保持空 = 全部。
      status: '',
      modelSource: '',
      transcriptionStatus: filters.transcriptionStatus,
      from: dayStart(filters.from),
      to: dayEnd(filters.to),
      offset: (page - 1) * pageSize,
      limit: pageSize,
    }),
    [appliedKeyword, filters.sourceId, filters.transcriptionStatus, filters.from, filters.to, page, pageSize],
  );

  // 列表加载：条件 / 分页 / 事件刷新都走这里。
  useEffect(() => {
    let cancelled = false;
    const load = async (): Promise<void> => {
      setLoading(true);
      try {
        const result = await SessionService.list(buildQuery());
        if (cancelled || !aliveRef.current) return;
        setItems(result.items);
        setTotal(result.total);
        setError(null);
      } catch (err) {
        if (cancelled || !aliveRef.current) return;
        setItems([]);
        setTotal(0);
        setError(toAppError(err));
      } finally {
        if (!cancelled && aliveRef.current) setLoading(false);
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [buildQuery, reloadToken]);

  // 来源应用下拉项：当前运行的源 + 已加载会话里出现过的源（历史应用可能已经关了）。
  useEffect(() => {
    let cancelled = false;
    const load = async (): Promise<void> => {
      try {
        const sources = await CaptureService.listSources();
        if (cancelled || !aliveRef.current) return;
        setSourceOptions(
          mergeSourceOptions(sources.map((s: Source) => ({ value: s.id, label: s.displayName }))),
        );
      } catch {
        // 源枚举失败不影响会话库（只是少了个筛选项）
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
    // 只在挂载时合并一次；后续新出现的源由下面的 items 合并补齐
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 已加载行里出现过的源，补进下拉项（否则历史应用的筛选选不到）。
  useEffect(() => {
    setSourceOptions((prev) =>
      mergeSourceOptions(
        [...prev, ...items.map((item) => ({ value: item.sourceId, label: item.sourceName }))],
      ),
    );
  }, [items]);

  // 订阅转写任务状态（EV-16）：更新进度；进入终态后刷新列表让徽标同步。
  useEffect(() => {
    const cancel = SessionService.onTranscriptionState((state) => {
      if (!aliveRef.current) return;
      const revisions = transcriptEventRevisions.current;
      revisions.set(state.sessionId, (revisions.get(state.sessionId) ?? 0) + 1);
      setTranscripts((prev) => ({ ...prev, [state.sessionId]: state }));
      if (state.status === 'done' || state.status === 'failed') setReloadToken((token) => token + 1);
    });
    return cancel;
  }, []);

  const reload = useCallback(async (): Promise<void> => {
    setReloadToken((token) => token + 1);
  }, []);

  const setFilter = useCallback((patch: Partial<LibraryFilters>): void => {
    // 任何条件变化都回到第 1 页：否则会停在一个已不存在的页码上。
    setFilters((prev) => ({ ...prev, ...patch }));
    setPageState(1);
  }, []);

  const resetFilters = useCallback((): void => {
    setFilters(EMPTY_FILTERS);
    setPageState(1);
  }, []);

  const setPage = useCallback((next: number): void => setPageState(next), []);

  const setPageSize = useCallback((next: number): void => {
    setPageSizeState(next);
    setPageState(1);
  }, []);

  const dismissError = useCallback((): void => setError(null), []);

  // —— 改名 ——
  const beginRename = useCallback((session: Session): void => {
    setRenaming(session);
    setRenameDraft(session.title);
  }, []);

  const cancelRename = useCallback((): void => {
    setRenaming(null);
    setRenameDraft('');
  }, []);

  const submitRename = useCallback(async (): Promise<boolean> => {
    if (renaming === null) return false;
    const title = renameDraft.trim();
    // 空标题没有意义（后端也会拒），直接留在弹窗里。
    if (title === '') return false;
    setBusyId(renaming.id);
    setError(null);
    try {
      await SessionService.rename(renaming.id, title);
      if (!aliveRef.current) return false;
      // 本页即时反映新标题（列表里同一行），不必等整表刷新。
      setItems((prev) => prev.map((item) => (item.id === renaming.id ? { ...item, title } : item)));
      setRenaming(null);
      setRenameDraft('');
      return true;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return false;
    } finally {
      if (aliveRef.current) setBusyId(null);
    }
  }, [renaming, renameDraft]);

  // —— 删除 ——
  const beginDelete = useCallback((session: Session): void => {
    setDeleting(session);
    setDeleteAudio(true);
  }, []);

  const cancelDelete = useCallback((): void => {
    setDeleting(null);
  }, []);

  const confirmDelete = useCallback(async (): Promise<DeleteResult | null> => {
    if (deleting === null) return null;
    setBusyId(deleting.id);
    setError(null);
    try {
      const result = await SessionService.deleteDetailed([deleting.id], deleteAudio);
      if (!aliveRef.current) return null;
      setDeleting(null);
      // 被删的会话正开着说话人侧栏时一并关掉（否则侧栏会对一个已不存在的会话反复报错）。
      setSpeakerSession((prev) => (prev !== null && prev.id === deleting.id ? null : prev));
      // 回放抽屉同理：会话没了，音频与时间轴都不再成立。
      setPlaybackSession((prev) => (prev !== null && prev.id === deleting.id ? null : prev));
      // 删除后当前页可能空了：回到第 1 页重新拉（后端会返回剩余总数）。
      setReloadToken((token) => token + 1);
      return result;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return null;
    } finally {
      if (aliveRef.current) setBusyId(null);
    }
  }, [deleting, deleteAudio]);

  // —— 导出 ——
  const exportSession = useCallback(
    async (sessionId: string, format: ExportFormat, targetPath: string): Promise<string | null> => {
      if (targetPath === '') return null;
      setBusyId(sessionId);
      setError(null);
      try {
        const saved = await SessionService.export({
          sessionIds: [sessionId],
          format,
          // 时间戳 / 说话人默认带上：导出就是为了能读能对时间。
          includeTimestamps: true,
          includeSpeakers: true,
          targetPath,
        });
        return aliveRef.current ? saved : null;
      } catch (err) {
        if (aliveRef.current) setError(toAppError(err));
        return null;
      } finally {
        if (aliveRef.current) setBusyId(null);
      }
    },
    [],
  );

  // —— 转写 ——
  const activeTranscription = useCallback(
    (session: Session): TranscriptionState | null => transcripts[session.id] ?? null,
    [transcripts],
  );

  const statusOf = useCallback(
    (session: Session): string => transcripts[session.id]?.status ?? session.transcriptionStatus,
    [transcripts],
  );

  const canTranscribe = useCallback(
    (session: Session): boolean => session.hasAudio !== false && canStartTranscription(statusOf(session)),
    [statusOf],
  );

  const startTranscription = useCallback(async (session: Session): Promise<void> => {
    if (!canTranscribe(session)) return;
    const sessionId = session.id;
    setBusyId(sessionId);
    setError(null);
    try {
      const before = transcriptEventRevisions.current.get(sessionId);
      const state = await SessionService.startTranscription(sessionId);
      if (!aliveRef.current) return;
      // A fast task can emit its terminal state before the enqueue RPC resolves.
      if (transcriptEventRevisions.current.get(sessionId) === before) {
        setTranscripts((prev) => ({ ...prev, [state.sessionId]: state }));
      }
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    } finally {
      if (aliveRef.current) setBusyId(null);
    }
  }, [canTranscribe]);

  const cancelTranscription = useCallback(async (sessionId: string): Promise<void> => {
    setBusyId(sessionId);
    setError(null);
    try {
      const before = transcriptEventRevisions.current.get(sessionId);
      const state = await SessionService.cancelTranscription(sessionId);
      if (!aliveRef.current) return;
      if (transcriptEventRevisions.current.get(sessionId) === before) {
        setTranscripts((prev) => ({ ...prev, [state.sessionId]: state }));
      }
      setReloadToken((token) => token + 1);
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    } finally {
      if (aliveRef.current) setBusyId(null);
    }
  }, []);

  // —— 说话人侧栏 ——
  // 只切「当前侧栏会话」；侧栏内的说话人 / 角色数据由 speakers/useSpeakersSidebar 自己拉，
  // 本 hook 不持有角色数据（避免与 roles:changed 的重拉逻辑出现两份真相）。
  // 与回放抽屉互斥：右侧栏只有一份空间，同时开两个会互相挤。
  const openSpeakers = useCallback((session: Session): void => {
    setPlaybackSession(null);
    setSpeakerSession(session);
  }, []);
  const closeSpeakers = useCallback((): void => setSpeakerSession(null), []);

  // —— 回放抽屉 ——
  const openPlayback = useCallback((session: Session): void => {
    setSpeakerSession(null);
    setPlaybackSession(session);
  }, []);
  const closePlayback = useCallback((): void => setPlaybackSession(null), []);

  // 时长区间（秒）→ 当前页的前端筛选。
  const minMs = secondsToMs(filters.minSeconds);
  const maxMs = secondsToMs(filters.maxSeconds);
  const visibleItems = useMemo(() => {
    if (minMs === null && maxMs === null) return items;
    return items.filter((item) => {
      if (minMs !== null && item.durationMs < minMs) return false;
      if (maxMs !== null && item.durationMs > maxMs) return false;
      return true;
    });
  }, [items, minMs, maxMs]);

  const pageCount = Math.max(1, Math.ceil(total / pageSize));

  // 删掉最后一页的最后一行后，页码可能越界：夹回最后一页，避免停在空白页。
  useEffect(() => {
    if (page > pageCount) setPageState(pageCount);
  }, [page, pageCount]);

  return {
    items,
    visibleItems,
    total,
    page,
    pageCount,
    pageSize,
    loading,
    error,
    durationFiltered: visibleItems.length !== items.length,
    filters,
    sourceOptions,
    setFilter,
    resetFilters,
    setPage,
    setPageSize,
    reload,
    dismissError,
    renaming,
    renameDraft,
    beginRename,
    setRenameDraft,
    cancelRename,
    submitRename,
    deleting,
    deleteAudio,
    setDeleteAudio,
    beginDelete,
    cancelDelete,
    confirmDelete,
    exportSession,
    transcripts,
    activeTranscription,
    statusOf,
    canTranscribe,
    startTranscription,
    cancelTranscription,
    busyId,
    speakerSession,
    openSpeakers,
    closeSpeakers,
    playbackSession,
    openPlayback,
    closePlayback,
  };
};

// 合并下拉项：按 value 去重、丢弃空 value、按 label 排序（同名不同 id 都保留）。
const mergeSourceOptions = (options: LibrarySourceOption[]): LibrarySourceOption[] => {
  const seen = new Map<string, string>();
  for (const option of options) {
    if (option.value === '' || seen.has(option.value)) continue;
    seen.set(option.value, option.label !== '' ? option.label : option.value);
  }
  return [...seen.entries()]
    .map(([value, label]) => ({ value, label }))
    .sort((a, b) => a.label.localeCompare(b.label));
};
