import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  CLOCK_THROTTLE_MS,
  PLAYBACK_PREFETCH_ROWS,
  PlaybackService,
  ROLE_UNKNOWN_ID,
  SLOT_HEIGHT_ESTIMATE,
  centerScrollTop,
  clampSeek,
  indexWindowAround,
  markersBySlot,
  nextLoadedIndexAfter,
  offsetsOf,
  prevLoadedIndexBefore,
  roleIsUnidentified,
  secondsToMs,
  segmentAt,
  slotMetricsOf,
  toAppError,
  totalHeightOf,
  visibleRange,
  type AppError,
  type Marker,
  type Role,
  type Segment,
  type Session,
} from '@/services';
import { usePagedTimeline } from './usePagedTimeline';

// 播放器时长来源优先级：<audio>.duration（真实音频）> timeline.durationMs > 会话帧计数时长。
// 三者都可能缺失（音频没加载完 / 未转写），所以每一处都做 0 兜底。

// 拖动进度条时「是否算用户手动滚动」的判定窗口：程序滚动会在短时间连续触发 scroll，
// 这个窗口内的 scroll 事件一律归因于我们自己的滚动，不能暂停自动居中。
const PROGRAMMATIC_SCROLL_WINDOW_MS = 320;

// 片段说话人展示信息：name 可能为空（后端没 JOIN 到角色），由 View 用 t() 兜底。
export interface SpeakerInfo {
  name: string;
  colorToken: string;
  unidentified: boolean;
}

// 窗口化渲染的一行（槽位）：对应一个**全局片段下标**。
//   - segment === null 表示这一句所在页还没加载 → View 渲染占位行，同时滚动会触发加载；
//   - markers 是挂在这一句之前的标记行（与旧实现「标记按 atMs 插入」等价）。
export interface PlaybackSlot {
  key: string;
  index: number;
  top: number;
  segment: Segment | null;
  markers: readonly Marker[];
}

export interface UsePlaybackResult {
  // —— 数据 ——
  loading: boolean;
  error: AppError | null;
  dismissError: () => void;
  reload: () => Promise<void>;
  // 音频文件当前是否还在（false 时不渲染播放器，只提示缺失）。
  audioExists: boolean;
  // 'none' | 'queued' | ... —— 无片段时用来区分「没转写」与「转写中」。
  transcriptionStatus: string;
  // 整段会话的总句数（分页后仍有值，标题与 aria 用）。
  totalSegments: number;
  markers: readonly Marker[];
  hasSegments: boolean;
  // —— 窗口化歌词列表 ——
  // 当前可视窗口 [windowStart, windowEnd) 对应的槽位（只含可视区 ± 缓冲行）。
  slots: PlaybackSlot[];
  // 内容总高（含未加载页的估算高度）——用于撑开滚动条。
  contentHeight: number;
  // 可视窗口（全局片段下标闭开区间），View 用它做 aria / dev 标注。
  windowStart: number;
  windowEnd: number;
  listRef: React.RefObject<HTMLDivElement | null>;
  onListScroll: () => void;
  // 行高实测回报：未回报过的行按 SLOT_HEIGHT_ESTIMATE 估算。
  reportSlotHeight: (index: number, height: number) => void;
  // —— 音频元素 ——
  // 用**回调 ref** 而不是 RefObject：<audio> 只在抽屉打开后才挂载，而监听 effect
  // 首次运行时元素还不存在。若只依赖 [audioKey, syncFromAudio]，元素迟到挂载后
  // 依赖不变、effect 不会重跑，play/timeupdate 等监听就永远不会绑上 ——
  // 表现为「音频在播，但时钟与高亮全冻住」（真实缺陷，已由 E2E 复现）。
  // 回调 ref 把元素写进 state，使 effect 能感知它出现/消失。
  audioRef: (element: HTMLAudioElement | null) => void;
  audioSrc: string;
  // 递增即让 <audio> 重新挂载（重试加载）；本项目不用 fetch，重试只能靠换 key。
  audioKey: number;
  audioError: boolean;
  retryAudio: () => void;
  onAudioError: () => void;
  onAudioLoadedMetadata: () => void;
  onAudioEnded: () => void;
  // —— 播放状态 ——
  playing: boolean;
  // 播放位置（毫秒）：播放中由 rAF 从 audio.currentTime 读，不是自己累加。
  currentMs: number;
  durationMs: number;
  rate: number;
  volume: number;
  muted: boolean;
  togglePlay: () => void;
  seekTo: (positionMs: number) => void;
  seekBy: (deltaMs: number) => void;
  nextSegment: () => void;
  prevSegment: () => void;
  setRate: (rate: number) => void;
  setVolume: (volume: number) => void;
  toggleMute: () => void;
  // —— 高亮 ——
  // 真实播放位置对应的**全局**片段下标（-1 = 第一句之前 / 无片段）。
  activeIndex: number;
  // 需要高亮的下标：拖动中 = 预览位置对应的片段，否则 = activeIndex。
  highlightIndex: number;
  // —— 拖拽定位（不 seek、不解码，只改预览） ——
  scrubbing: boolean;
  scrubMs: number;
  beginScrub: () => void;
  scrubTo: (positionMs: number) => void;
  endScrub: () => void;
  // —— 自动居中 / 手动滚动 ——
  autoScroll: boolean;
  backToActive: () => void;
  // —— 说话人着色（roleId → 名字 / 颜色令牌） ——
  speakerOf: (segment: Segment) => SpeakerInfo;
}

export interface UsePlaybackOptions {
  session: Session;
}

// PlaybackDrawer 的 ViewModel：
//   - 时间基准只有 audio.currentTime（后端用帧计数换算 startMs/endMs，两者同一时间轴）；
//   - rAF 每帧读一次 currentTime 并定位当前片段：切换延迟 ≤ 1 帧（~17ms），
//     远小于 FR-04-2 的 80ms 上限；时钟 state 另有 60ms 节流，避免每帧重渲染大列表；
//   - **分页**：首屏只请求一页（PLAYBACK_PAGE_SIZE = 200 条，见 usePagedTimeline），
//     其余页由「可视窗口」与「高亮附近预取」驱动补齐；
//   - **窗口化**：只渲染可视区 ± SLOT_OVERSCAN 行（高度实测，未渲染过的按估算撑滚动条）；
//   - 高亮落在未加载页时不返回 -1（那会让高亮消失）：保留「最接近的已加载句」并请求该页，
//     页到达后重算即精确；
//   - 拖动进度条只改 scrubMs（预览高亮），释放时一次性 seek —— 拖动期间**不**触碰
//     audio.currentTime，因此不会触发解码 / 拉流；
//   - 用户手动滚动列表即暂停自动居中，直到点「回到当前句」或发生 seek（高亮归位）；
//   - 倍速 / 音量只存在内存 state 里（不写入任何浏览器持久化存储），关闭抽屉即复位；
//   - 所有监听（audio 事件、滚动、rAF、ResizeObserver）在 unmount 时清理。
export const usePlayback = ({ session }: UsePlaybackOptions): UsePlaybackResult => {
  const sessionId = session.id;

  const [detailLoading, setDetailLoading] = useState<boolean>(true);
  const [detailError, setDetailError] = useState<AppError | null>(null);
  const [audioExists, setAudioExists] = useState<boolean>(false);
  const [transcriptionStatus, setTranscriptionStatus] = useState<string>(session.transcriptionStatus);
  const [roles, setRoles] = useState<Role[]>([]);

  const [playing, setPlaying] = useState<boolean>(false);
  const [currentMs, setCurrentMs] = useState<number>(0);
  const [audioDurationMs, setAudioDurationMs] = useState<number>(0);
  const [activeIndex, setActiveIndex] = useState<number>(-1);
  const [rate, setRateState] = useState<number>(1);
  const [volume, setVolumeState] = useState<number>(1);
  const [muted, setMuted] = useState<boolean>(false);
  const [audioError, setAudioError] = useState<boolean>(false);
  const [audioKey, setAudioKey] = useState<number>(0);

  const [scrubbing, setScrubbing] = useState<boolean>(false);
  const [scrubMs, setScrubMs] = useState<number>(0);
  // 拖动中预览位置对应的全局下标（拖动时不去碰 activeIndex，避免松手前高亮乱跳）。
  const [scrubIndex, setScrubIndex] = useState<number>(-1);

  const [autoScroll, setAutoScroll] = useState<boolean>(true);

  // 分页数据源：首屏一页 + 按需增量。
  // 回调逐个解构出来（它们的 useCallback 身份跨渲染稳定），避免把整个对象塞进 effect 依赖
  // 导致「每次渲染都重跑一次加载」。
  const {
    timeline,
    loadingFirst: pagesLoadingFirst,
    error: pagesError,
    ensureIndexRange,
    locate,
    reload: reloadPages,
  } = usePagedTimeline({ sessionId, segmentCountHint: session.segmentCount });

  const audioRef = useRef<HTMLAudioElement | null>(null);
  // 元素实例的 state 副本：回调 ref 写入它，好让「绑监听」的 effect 能在
  // <audio> 迟到挂载后重新执行（详见结果接口上 audioRef 的注释）。
  const [audioNode, setAudioNode] = useState<HTMLAudioElement | null>(null);
  const attachAudio = useCallback((element: HTMLAudioElement | null): void => {
    audioRef.current = element;
    setAudioNode(element);
  }, []);
  const listRef = useRef<HTMLDivElement | null>(null);
  const aliveRef = useRef<boolean>(true);

  // rAF 闭包里要用的最新值：state 会因闭包过期，统一走 ref。
  const timelineRef = useRef(timeline);
  const activeIndexRef = useRef<number>(-1);
  const lastClockSyncRef = useRef<number>(0);
  const lastProgrammaticScrollRef = useRef<number>(0);
  // 指针是否正按在进度条上（拖动 = 只预览；键盘微调没有 pointerdown，直接 seek）。
  const scrubbingRef = useRef<boolean>(false);
  // 「下一句 / 上一句」的目标落在未加载页时先记下来，页到达后补一次 seek。
  const pendingSeekIndexRef = useRef<number | null>(null);
  // seek 目标落在未加载页时记下来，页到达后补一次「高亮归位」（暂停状态下也要纠正）。
  const pendingLocateMsRef = useRef<number | null>(null);

  useEffect(() => {
    timelineRef.current = timeline;
  }, [timeline]);

  useEffect(() => {
    activeIndexRef.current = activeIndex;
  }, [activeIndex]);

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  // —— 详情加载（音频是否还在 + 转写状态）；角色单独失败不影响回放 ——
  const loadDetail = useCallback(async (): Promise<void> => {
    setDetailLoading(true);
    setDetailError(null);
    try {
      const [detail, roleList] = await Promise.all([
        PlaybackService.getDetail(sessionId),
        PlaybackService.listRoles().catch(() => [] as Role[]),
      ]);
      if (!aliveRef.current) return;
      setAudioExists(detail.audioExists);
      setTranscriptionStatus(detail.session.transcriptionStatus);
      setRoles(roleList);
    } catch (err) {
      if (!aliveRef.current) return;
      setDetailError(toAppError(err));
    } finally {
      if (aliveRef.current) setDetailLoading(false);
    }
  }, [sessionId]);

  useEffect(() => {
    void loadDetail();
  }, [loadDetail]);

  const reload = useCallback(async (): Promise<void> => {
    reloadPages();
    await loadDetail();
  }, [loadDetail, reloadPages]);

  // 首屏数据（分页第一页）报错时才把错误抬到 UI；已有数据时单页失败不打断阅读。
  const error = detailError ?? (timeline.total === 0 ? pagesError : null);

  // —— 窗口化的尺寸模型 ——
  // 实测行高 + 估算值 → 偏移表。offsets[i] = 槽位 i 的顶部像素。
  const measuredRef = useRef<Map<number, number>>(new Map<number, number>());
  const [metricsVersion, setMetricsVersion] = useState<number>(0);
  const [scrollTop, setScrollTop] = useState<number>(0);
  const [viewportHeight, setViewportHeight] = useState<number>(0);

  const metrics = useMemo(
    () => slotMetricsOf(timeline.total, measuredRef.current, SLOT_HEIGHT_ESTIMATE),
    // metricsVersion 变化 = 有新的实测行高进来。
    [metricsVersion, timeline.total],
  );
  const offsets = useMemo(() => offsetsOf(metrics), [metrics]);
  const contentHeight = useMemo(() => totalHeightOf(offsets), [offsets]);
  const slotWindow = useMemo(
    () => visibleRange(offsets, scrollTop, viewportHeight),
    [offsets, scrollTop, viewportHeight],
  );

  // 标记挂到「它前面那一句」的槽位上（与旧 buildTimelineRows 的混排语义一致）。
  const markerMap = useMemo(() => markersBySlot(timeline), [timeline]);

  const slots = useMemo<PlaybackSlot[]>(() => {
    const out: PlaybackSlot[] = [];
    if (slotWindow.end <= slotWindow.start) return out;
    for (let index = slotWindow.start; index < slotWindow.end; index++) {
      const segment = segmentAt(timeline, index);
      out.push({
        key: `slot-${index}`,
        index,
        top: offsets[index] ?? 0,
        segment: segment ?? null,
        markers: markerMap.get(index) ?? [],
      });
    }
    return out;
  }, [markerMap, offsets, slotWindow.end, slotWindow.start, timeline]);

  // 行高实测回报：只有真的变了才触发重算（否则每次渲染都 setState 会自激）。
  const reportSlotHeight = useCallback((index: number, height: number): void => {
    if (!Number.isFinite(height) || height <= 0) return;
    const measured = measuredRef.current;
    const prev = measured.get(index);
    if (prev !== undefined && Math.abs(prev - height) < 0.5) return;
    measured.set(index, height);
    setMetricsVersion((prevVersion) => prevVersion + 1);
  }, []);

  // 视口高度：列表真正渲染出来之后才开始观察（抽屉三态里只有成功态有列表）。
  const hasSegments = timeline.total > 0;
  const listRendered = !detailLoading && error === null && hasSegments;
  useEffect(() => {
    if (!listRendered) return undefined;
    const list = listRef.current;
    if (list === null) return undefined;
    const sync = (): void => {
      const height = list.clientHeight;
      setViewportHeight((prev) => (Math.abs(prev - height) > 0.5 ? height : prev));
    };
    sync();
    if (typeof ResizeObserver === 'undefined') {
      window.addEventListener('resize', sync);
      return () => window.removeEventListener('resize', sync);
    }
    const observer = new ResizeObserver(sync);
    observer.observe(list);
    return () => observer.disconnect();
  }, [listRendered]);

  // —— 滚动驱动加载 ——
  // 可视窗口内的页 + 高亮附近的页：两个来源都幂等（已加载 / 在飞的页会被跳过）。
  useEffect(() => {
    if (slotWindow.end <= slotWindow.start) return;
    ensureIndexRange(slotWindow.start, slotWindow.end - 1);
  }, [ensureIndexRange, slotWindow.end, slotWindow.start]);

  useEffect(() => {
    if (activeIndex < 0 || timeline.total <= 0) return;
    const window = indexWindowAround(activeIndex, timeline.total, PLAYBACK_PREFETCH_ROWS);
    ensureIndexRange(window.first, window.last);
  }, [activeIndex, ensureIndexRange, timeline.total]);

  const durationMs =
    audioDurationMs > 0
      ? audioDurationMs
      : timeline.durationMs > 0
        ? timeline.durationMs
        : session.durationMs;

  // 从 audio.currentTime 重新计算「当前片段」与时钟。
  const syncFromAudio = useCallback(
    (forceClock: boolean): void => {
      const audio = audioRef.current;
      if (audio === null) return;
      const positionMs = secondsToMs(audio.currentTime);
      const located = locate(positionMs, activeIndexRef.current);
      // 非精确（目标落在未加载页）：**保留上一句**高亮，绝不清空；
      // locate 已经顺手请求了需要的那一页，到达后下一次 sync 就会纠正。
      if (located.exact && located.index !== activeIndexRef.current) {
        activeIndexRef.current = located.index;
        setActiveIndex(located.index);
      }
      const now = performance.now();
      if (forceClock || now - lastClockSyncRef.current >= CLOCK_THROTTLE_MS) {
        lastClockSyncRef.current = now;
        setCurrentMs(positionMs);
      }
    },
    [locate],
  );

  // 播放中每帧读 audio.currentTime：高亮判定不受 60ms 节流影响（误差 ≤ 1 帧）。
  useEffect(() => {
    if (!playing) return undefined;
    let frame = 0;
    const loop = (): void => {
      syncFromAudio(false);
      frame = window.requestAnimationFrame(loop);
    };
    frame = window.requestAnimationFrame(loop);
    return () => window.cancelAnimationFrame(frame);
  }, [playing, syncFromAudio]);

  // audio 事件监听：状态以元素为准（暂停 / 播放 / 结束 / 时长 / 错误）。
  // 依赖里必须含 audioNode：<audio> 在抽屉打开后才挂载，元素到位时要重跑本 effect，
  // 否则监听永远绑不上（音频在播、时钟与高亮却全冻住）。
  useEffect(() => {
    const audio = audioNode;
    if (audio === null) return undefined;
    const handlePlay = (): void => {
      setPlaying(true);
      syncFromAudio(true);
    };
    const handlePause = (): void => {
      setPlaying(false);
      syncFromAudio(true);
    };
    const handleTimeUpdate = (): void => syncFromAudio(true);
    const handleDurationChange = (): void => {
      if (Number.isFinite(audio.duration) && audio.duration > 0) {
        setAudioDurationMs(secondsToMs(audio.duration));
      }
    };
    const handleSeeked = (): void => syncFromAudio(true);
    const handleEnded = (): void => {
      setPlaying(false);
      syncFromAudio(true);
    };
    const handleError = (): void => {
      setAudioError(true);
      setPlaying(false);
    };

    audio.addEventListener('play', handlePlay);
    audio.addEventListener('pause', handlePause);
    audio.addEventListener('timeupdate', handleTimeUpdate);
    audio.addEventListener('loadedmetadata', handleDurationChange);
    audio.addEventListener('durationchange', handleDurationChange);
    audio.addEventListener('seeked', handleSeeked);
    audio.addEventListener('ended', handleEnded);
    audio.addEventListener('error', handleError);
    return () => {
      audio.removeEventListener('play', handlePlay);
      audio.removeEventListener('pause', handlePause);
      audio.removeEventListener('timeupdate', handleTimeUpdate);
      audio.removeEventListener('loadedmetadata', handleDurationChange);
      audio.removeEventListener('durationchange', handleDurationChange);
      audio.removeEventListener('seeked', handleSeeked);
      audio.removeEventListener('ended', handleEnded);
      audio.removeEventListener('error', handleError);
    };
    // audioKey 变化 = 元素被重挂载，监听要重新绑。
  }, [audioNode, audioKey, syncFromAudio]);

  // 卸载时暂停播放：避免抽屉关掉后声音还在响。
  useEffect(
    () => () => {
      const audio = audioRef.current;
      if (audio !== null) {
        audio.pause();
        audio.removeAttribute('src');
        audio.load();
      }
    },
    [],
  );

  // —— 播放控制 ——
  const togglePlay = useCallback((): void => {
    const audio = audioRef.current;
    if (audio === null || !audioExists) return;
    if (audio.paused) {
      // play() 返回 Promise：自动播放策略或被移除时会 reject，别让它成为未处理拒绝。
      void audio.play().catch(() => {
        if (aliveRef.current) setPlaying(false);
      });
      return;
    }
    audio.pause();
  }, [audioExists]);

  // seek 后高亮归位 + 恢复自动居中（FR-04-4：释放后恢复自动滚动）。
  const seekTo = useCallback(
    (positionMs: number): void => {
      const audio = audioRef.current;
      setAutoScroll(true);
      const target = clampSeek(positionMs, durationMs);
      if (audio !== null) {
        try {
          audio.currentTime = target / 1000;
        } catch {
          // 音频缺失 / 尚未有媒体（audioExists=false 时点片段仍要能移动高亮）：
          // 当前时间点不可写不是致命错误，下面照样把时钟与高亮对上。
        }
      }
      // 不等 seeked 事件：立即把时钟与高亮对上，避免等一帧才归位。
      // 目标可能落在未加载页：locate 会请求那一页，此处先保持「最接近的已加载句」。
      lastClockSyncRef.current = performance.now();
      setCurrentMs(target);
      const located = locate(target, -1);
      if (located.exact) {
        pendingLocateMsRef.current = null;
        activeIndexRef.current = located.index;
        setActiveIndex(located.index);
      } else {
        // 落在未加载页：locate 已请求该页；页到达后由下面的 effect 补一次归位，
        // 否则暂停状态下高亮会一直停在上一次的位置。
        pendingLocateMsRef.current = target;
      }
    },
    [durationMs, locate],
  );

  const seekBy = useCallback(
    (deltaMs: number): void => {
      const audio = audioRef.current;
      if (audio === null) return;
      seekTo(secondsToMs(audio.currentTime) + deltaMs);
    },
    [seekTo],
  );

  // 跳到某个**全局下标**：该句所在页未加载时先记下目标、请求页，到达后补 seek。
  const seekToIndex = useCallback(
    (index: number): void => {
      const current = timelineRef.current;
      if (index < 0 || index >= current.total) return;
      const segment = segmentAt(current, index);
      if (segment === undefined) {
        pendingSeekIndexRef.current = index;
        ensureIndexRange(index, index);
        return;
      }
      seekTo(segment.startMs);
    },
    [ensureIndexRange, seekTo],
  );

  // 页到达后补掉挂起的「高亮归位」（只补一次，直到结论变精确）。
  useEffect(() => {
    const target = pendingLocateMsRef.current;
    if (target === null) return;
    const located = locate(target, activeIndexRef.current);
    if (!located.exact) return;
    pendingLocateMsRef.current = null;
    if (located.index !== activeIndexRef.current) {
      activeIndexRef.current = located.index;
      setActiveIndex(located.index);
    }
  }, [timeline, locate]);

  // 页到达后补掉挂起的 seek（只补一次）。
  useEffect(() => {
    const pending = pendingSeekIndexRef.current;
    if (pending === null) return;
    const segment = segmentAt(timeline, pending);
    if (segment === undefined) return;
    pendingSeekIndexRef.current = null;
    seekTo(segment.startMs);
  }, [timeline, seekTo]);

  const nextSegment = useCallback((): void => {
    const current = timelineRef.current;
    if (current.total <= 0) return;
    const from = activeIndexRef.current;
    // 还没进第一句时「下一句」= 第一句。
    if (from < 0) {
      seekToIndex(0);
      return;
    }
    const next = nextLoadedIndexAfter(current, from);
    if (next === undefined) return;
    seekToIndex(next);
  }, [seekToIndex]);

  const prevSegment = useCallback((): void => {
    const current = timelineRef.current;
    if (current.total <= 0) return;
    const from = activeIndexRef.current;
    // 当前句已播过 2 秒以上时，「上一句」先回到本句开头（符合播放器直觉）。
    const audio = audioRef.current;
    if (from >= 0 && audio !== null) {
      const segment = segmentAt(current, from);
      if (segment !== undefined && secondsToMs(audio.currentTime) - segment.startMs > 2000) {
        seekToIndex(from);
        return;
      }
    }
    const prev = from > 0 ? prevLoadedIndexBefore(current, from) : undefined;
    seekToIndex(prev === undefined ? Math.max(0, from - 1) : prev);
  }, [seekToIndex]);

  const setRate = useCallback((next: number): void => {
    setRateState(next);
  }, []);

  const setVolume = useCallback((next: number): void => {
    const clamped = Math.min(1, Math.max(0, next));
    setVolumeState(clamped);
    // 调音量时若还处于静音，顺手解除静音（否则滑块动了却没声音）。
    if (clamped > 0) setMuted(false);
  }, []);

  const toggleMute = useCallback((): void => setMuted((prev) => !prev), []);

  // —— 拖拽定位：拖动期间只改预览，绝不 seek（否则会边拖边解码） ——
  const beginScrub = useCallback((): void => {
    scrubbingRef.current = true;
    setScrubbing(true);
    const audio = audioRef.current;
    const from = audio !== null ? secondsToMs(audio.currentTime) : currentMs;
    setScrubMs(from);
    setScrubIndex(activeIndexRef.current);
    // 拖动中自动居中关闭（UX-SPEC §5.5）。
    setAutoScroll(false);
  }, [currentMs]);

  const scrubTo = useCallback(
    (positionMs: number): void => {
      const target = clampSeek(positionMs, durationMs);
      if (!scrubbingRef.current) {
        // 键盘微调（←/→、Home/End）：没有「拖动中」这一说，直接 seek 更符合预期。
        seekTo(target);
        return;
      }
      setScrubMs(target);
      // 预览高亮：目标落在未加载页时保持上一个预览下标，同时请求该页（拖动也可能跨页）。
      const located = locate(target, scrubIndex);
      if (located.exact) setScrubIndex(located.index);
    },
    [durationMs, locate, scrubIndex, seekTo],
  );

  const endScrub = useCallback((): void => {
    scrubbingRef.current = false;
    setScrubbing(false);
    // 释放才真正 seek：一次定位、一次解码，并顺带恢复自动居中与高亮归位。
    seekTo(scrubMs);
  }, [scrubMs, seekTo]);

  const highlightIndex = scrubbing ? scrubIndex : activeIndex;

  // —— 自动居中：把高亮句滚到列表中线 ——
  // 用偏移表直接算目标 scrollTop（不查 DOM）：目标行即使还没渲染也能一次到位，
  // 也因此不会出现「读到过期布局 → 反复微调」的抖动。
  const centerOn = useCallback(
    (index: number): void => {
      const list = listRef.current;
      if (list === null || index < 0) return;
      const maxScrollTop = Math.max(0, contentHeight - list.clientHeight);
      const next = centerScrollTop(offsets, index, metrics.heightOf, list.clientHeight, maxScrollTop);
      lastProgrammaticScrollRef.current = performance.now();
      if (Math.abs(list.scrollTop - next) > 0.5) list.scrollTop = next;
      setScrollTop(next);
    },
    [contentHeight, metrics, offsets],
  );

  useEffect(() => {
    if (!autoScroll || scrubbing) return;
    if (highlightIndex < 0) return;
    centerOn(highlightIndex);
    // offsets 入依赖：行高实测到位后重算一次，保持「当前句在中线」。
  }, [autoScroll, centerOn, highlightIndex, offsets, scrubbing]);

  // 用户手动滚动 → 暂停自动居中（程序滚动在窗口期内忽略）；同时同步 scrollTop。
  const onListScroll = useCallback((): void => {
    const list = listRef.current;
    if (list === null) return;
    if (performance.now() - lastProgrammaticScrollRef.current >= PROGRAMMATIC_SCROLL_WINDOW_MS) {
      setAutoScroll(false);
    }
    // 不加节流：浏览器本身一帧最多派发一次 scroll；值没变时 React 会直接跳过重渲染，
    // 而漏掉最后一次 scroll 会让窗口停在错误区间（宁可多一次 setState）。
    setScrollTop(list.scrollTop);
  }, []);

  const backToActive = useCallback((): void => {
    const index = activeIndexRef.current;
    // 手动滚一下就能暂停自动居中，而「回到当前句」只是把开关打开并立即归位。
    setAutoScroll(true);
    if (index < 0) return;
    // 高亮句所在页可能被滚出了加载窗口：顺手请求一次。
    ensureIndexRange(index, index);
    centerOn(index);
  }, [centerOn, ensureIndexRange]);

  const retryAudio = useCallback((): void => {
    setAudioError(false);
    setAudioDurationMs(0);
    setCurrentMs(0);
    setActiveIndex(-1);
    activeIndexRef.current = -1;
    setAudioKey((prev) => prev + 1);
  }, []);

  const onAudioError = useCallback((): void => {
    setAudioError(true);
    setPlaying(false);
  }, []);

  const onAudioLoadedMetadata = useCallback((): void => {
    const audio = audioRef.current;
    if (audio === null) return;
    if (Number.isFinite(audio.duration) && audio.duration > 0) {
      setAudioDurationMs(secondsToMs(audio.duration));
    }
    // 重挂载后把当前设置补回元素（新元素是默认值）。
    audio.playbackRate = rate;
    audio.volume = volume;
    audio.muted = muted;
    syncFromAudio(true);
  }, [muted, rate, syncFromAudio, volume]);

  const onAudioEnded = useCallback((): void => {
    setPlaying(false);
    syncFromAudio(true);
  }, [syncFromAudio]);

  const dismissError = useCallback((): void => setDetailError(null), []);

  // 角色着色：roleId → 名字 / 颜色令牌。颜色令牌由后端给，前端样式层映射成 --fd-*。
  const roleById = useMemo(() => {
    const map = new Map<string, Role>();
    for (const role of roles) map.set(role.id, role);
    return map;
  }, [roles]);

  const speakerOf = useCallback(
    (segment: Segment): SpeakerInfo => {
      const unidentified =
        segment.speakerId === '' ||
        segment.roleId === '' ||
        segment.roleId === ROLE_UNKNOWN_ID ||
        roleIsUnidentified(segment.roleId);
      const role = roleById.get(segment.roleId);
      // 角色名以 roles JOIN 结果为准（改名对历史会话同步），片段上的名字只做兜底。
      const name = role !== undefined && role.name !== '' ? role.name : segment.roleName;
      // 颜色令牌由后端给（role-1..8 / neutral / accent）；拿不到就用中性色，不猜。
      const colorToken = role !== undefined && role.colorToken !== '' ? role.colorToken : 'neutral';
      return { name, colorToken, unidentified };
    },
    [roleById],
  );

  return {
    loading: detailLoading || pagesLoadingFirst,
    error,
    dismissError,
    reload,
    audioExists,
    transcriptionStatus,
    totalSegments: timeline.total,
    markers: timeline.markers,
    hasSegments,
    slots,
    contentHeight,
    windowStart: slotWindow.start,
    windowEnd: slotWindow.end,
    listRef,
    onListScroll,
    reportSlotHeight,
    audioRef: attachAudio,
    audioSrc: PlaybackService.audioSrc(sessionId),
    audioKey,
    audioError,
    retryAudio,
    onAudioError,
    onAudioLoadedMetadata,
    onAudioEnded,
    playing,
    currentMs: scrubbing ? scrubMs : currentMs,
    durationMs,
    rate,
    volume,
    muted,
    togglePlay,
    seekTo,
    seekBy,
    nextSegment,
    prevSegment,
    setRate,
    setVolume,
    toggleMute,
    activeIndex,
    highlightIndex,
    scrubbing,
    scrubMs,
    beginScrub,
    scrubTo,
    endScrub,
    autoScroll,
    backToActive,
    speakerOf,
  };
};
