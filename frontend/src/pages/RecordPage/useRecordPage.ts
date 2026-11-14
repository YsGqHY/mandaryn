import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  AppError,
  AppSettingsService,
  CaptureService,
  EngineService,
  LiveTranscriptService,
  RecordingService,
  RecordingStates,
  SILENCE_WARN_MS,
  isRecordingActive,
  isRecordingSettled,
  sourceKindOf,
  toAppError,
  type CaptureCaps,
  type EngineStatus,
  type LiveAvailability,
  type ModelSource,
  type RecordingLevel,
  type RecordingState,
  type RecordingTick,
  type Segment,
  type Source,
} from '@/services';

// 电平事件的 UI 节流：后端 10Hz 推送，页面只需约 4Hz 刷新（避免每帧重渲染）。
const LEVEL_UI_INTERVAL_MS = 250;

// RecordingMetrics 是「电平 + 节拍」两个事件的展示聚合（纯 UI 组装，不是契约类型）。
export interface RecordingMetrics {
  // Peak 峰值 0–1：用于削波指示。
  peak: number;
  // Rms 均方根 0–1：用于常规电平条。
  rms: number;
  // SilentMs 连续静音时长（后端给，前端不自算）。
  silentMs: number;
  // Silent 连续静音是否已超过阈值（FR-03-7 的「未检测到音频」提示依据）。
  silent: boolean;
  // RecordedMs 逻辑时长，由后端帧计数换算（**不用** Date.now() 自算）。
  recordedMs: number;
  // BytesWritten 已写字节。
  bytesWritten: number;
  // CharCount 仅边录边转时有意义。
  charCount: number;
}

// LiveTranscript 是边录边转（FR-03-10）的展示状态。
//
// 三个事实分开给出，View 才能说清「为什么没字」：
//   - enabled：用户开关（appsettings.live_transcribe，默认 false）；
//   - available / reason：能力（有没有流式模型包）；
//   - partial / finalized：实际收到的文字（只有录制中才会有）。
export interface LiveTranscript {
  // enabled 开关当前值。
  enabled: boolean;
  // available 能力是否可用。
  available: boolean;
  // reason 不可用原因（后端中文短说明）。
  reason: string;
  // errorCode 不可用原因的错误码。
  errorCode: string;
  // partial 当前句的实时文本（累积；空串表示还没有字）。
  partial: string;
  // finalized 已定稿的句子（本次录制内，最多保留最近若干条）。
  finalized: Segment[];
}

export interface UseRecordPageResult {
  // —— 平台能力 ——
  caps: CaptureCaps | null;
  // —— 源列表（分组在 VM 做，View 只渲染） ——
  applications: Source[];
  devices: Source[];
  selectedId: string;
  selectedSource: Source | null;
  selectedUnavailable: boolean;
  loading: boolean;
  refreshing: boolean;
  // —— 录制状态机（EV-01 / API-2.5） ——
  recording: RecordingState | null;
  recordingState: string;
  // 有活动会话（准备 / 录制 / 暂停 / 收尾）：禁止切源与切模型来源。
  recordingActive: boolean;
  // 已封存（completed / interrupted / failed）：可另存音频、可修补、可发起转写。
  recordingSettled: boolean;
  metrics: RecordingMetrics;
  busy: boolean;
  // —— 边录边转（FR-03-10，默认关）——
  live: LiveTranscript;
  // —— 引擎（模型来源） ——
  engine: EngineStatus | null;
  // —— 按钮可用性（严格按状态机推导，View 不再自己判断） ——
  canStart: boolean;
  canPause: boolean;
  canResume: boolean;
  canStop: boolean;
  canExportAudio: boolean;
  // —— 动作 ——
  start: () => Promise<void>;
  pause: () => Promise<void>;
  resume: () => Promise<void>;
  stop: () => Promise<void>;
  // 另存录音：targetPath 必须来自 NativeDialogs 的原生「另存为」。
  exportAudio: (targetPath: string) => Promise<string | null>;
  // 修补未正常结束的会话（UC-07）：状态为 interrupted 时可用。
  recover: () => Promise<boolean>;
  setModelSource: (source: ModelSource) => Promise<void>;
  refresh: () => Promise<void>;
  select: (source: Source) => Promise<void>;
  error: AppError | null;
  dismissError: () => void;
}

const EMPTY_METRICS: RecordingMetrics = {
  peak: 0,
  rms: 0,
  silentMs: 0,
  silent: false,
  recordedMs: 0,
  bytesWritten: 0,
  charCount: 0,
};

// 后端 RecordingState.lastError（ErrorDTO）→ 前端 AppError。
const fromErrorDTO = (dto: RecordingState['lastError']): AppError | null => {
  if (dto === null || dto === undefined || dto.code === '') return null;
  return new AppError({
    code: dto.code,
    message: dto.message,
    action: dto.action ?? '',
    detail: dto.detail ?? '',
    retryable: dto.retryable,
  });
};

// RecordPage ViewModel：
//   - 不持有任何"显示文案"（文案是 View + i18n 的责任）
//   - 源选择以「后端 SetPreferredSource 成功」为准：先乐观高亮，失败回滚并抛错给 View
//   - 录制状态以 event `recording:state` 为准；事件到达前用 GetState() 取初值
//     （窗口重开 / 热重载后 UI 能恢复成后端的真实状态）
//   - 时长只用后端 recordedMs（帧计数换算），不用前端 wall clock 自算
export const useRecordPage = (): UseRecordPageResult => {
  const [caps, setCaps] = useState<CaptureCaps | null>(null);
  const [sources, setSources] = useState<Source[]>([]);
  const [selectedId, setSelectedId] = useState<string>('');
  const [loading, setLoading] = useState<boolean>(true);
  const [refreshing, setRefreshing] = useState<boolean>(false);
  const [error, setError] = useState<AppError | null>(null);

  const [recording, setRecording] = useState<RecordingState | null>(null);
  const [level, setLevel] = useState<RecordingLevel | null>(null);
  const [tick, setTick] = useState<RecordingTick | null>(null);
  const [engine, setEngine] = useState<EngineStatus | null>(null);
  const [busy, setBusy] = useState<boolean>(false);

  // —— 边录边转状态（开关 + 能力 + 实时文本）——
  const [liveEnabled, setLiveEnabled] = useState<boolean>(false);
  const [liveCaps, setLiveCaps] = useState<LiveAvailability | null>(null);
  const [livePartial, setLivePartial] = useState<string>('');
  const [liveFinalized, setLiveFinalized] = useState<Segment[]>([]);

  const aliveRef = useRef<boolean>(true);
  // 电平节流用：上次提交到 state 的时间戳。
  const lastLevelAtRef = useRef<number>(0);
  // 上一次已知的会话 ID：事件里 sessionId 变化时必须清掉上一段的电平 / 节拍。
  const sessionIdRef = useRef<string>('');

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  // 落地一份后端状态快照：会话变化时清掉上一段的电平 / 节拍（否则新会话会先显示旧数值）。
  const applyState = useCallback((next: RecordingState): void => {
    if (sessionIdRef.current !== next.sessionId) {
      sessionIdRef.current = next.sessionId;
      setLevel(null);
      setTick(null);
      lastLevelAtRef.current = 0;
      // 边录边转的文本**绑定在会话上**：换会话必须清空，否则上一场的字会串到新一场。
      setLivePartial('');
      setLiveFinalized([]);
    }
    setRecording(next);
  }, []);

  // 首次加载：能力 + 源列表 + 上次选择 + 录制状态 + 引擎状态，互不阻塞
  //（一份失败不影响其余展示）。
  useEffect(() => {
    let cancelled = false;
    const load = async (): Promise<void> => {
      const [capsResult, sourcesResult, preferredResult, recordingResult, engineResult] =
        await Promise.allSettled([
          CaptureService.getCapabilities(),
          CaptureService.listSources(),
          CaptureService.getPreferredSource(),
          RecordingService.getState(),
          EngineService.getStatus(),
        ]);
      if (cancelled || !aliveRef.current) return;

      if (capsResult.status === 'fulfilled') setCaps(capsResult.value);
      if (sourcesResult.status === 'fulfilled') setSources(sourcesResult.value);
      if (preferredResult.status === 'fulfilled') setSelectedId(preferredResult.value ?? '');
      // 窗口重开：用后端真实状态恢复 UI（不能假设一定是 idle）。
      if (recordingResult.status === 'fulfilled') {
        const state = recordingResult.value;
        applyState(state);
        const stateError = fromErrorDTO(state.lastError);
        if (stateError !== null) setError(stateError);
      }
      if (engineResult.status === 'fulfilled') setEngine(engineResult.value);

      // 源列表或能力读取失败才报错；preferred / recording / engine 失败静默忽略。
      const failure =
        sourcesResult.status === 'rejected'
          ? sourcesResult.reason
          : capsResult.status === 'rejected'
            ? capsResult.reason
            : null;
      if (failure !== null) setError(toAppError(failure));
      setLoading(false);
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, []);

  // 订阅源列表变化（EV-12）：应用启动 / 退出、设备增减都走这里。
  useEffect(() => {
    const cancel = CaptureService.onSourcesChanged((next) => {
      if (!aliveRef.current) return;
      setSources(next);
    });
    return cancel;
  }, []);

  // 订阅录制状态机（EV-01）。会话切换时清掉上一段的电平 / 节拍，避免串台。
  useEffect(() => {
    const cancel = RecordingService.onState((next) => {
      if (!aliveRef.current) return;
      applyState(next);
      // 后端在状态转换里带出的错误（如磁盘满）：直接提示，不静默吞掉。
      const stateError = fromErrorDTO(next.lastError);
      if (stateError !== null) setError(stateError);
    });
    return cancel;
  }, [applyState]);

  // 订阅采集电平（EV-02）：后端 10Hz，这里节流到约 4Hz 再进 state。
  useEffect(() => {
    const cancel = RecordingService.onLevel((next) => {
      if (!aliveRef.current) return;
      const now = Date.now();
      if (now - lastLevelAtRef.current < LEVEL_UI_INTERVAL_MS) return;
      lastLevelAtRef.current = now;
      setLevel(next);
    });
    return cancel;
  }, []);

  // 订阅采集节拍（EV-03，1Hz）：只用于「已写字节」这类快变数值。
  useEffect(() => {
    const cancel = RecordingService.onTick((next) => {
      if (!aliveRef.current) return;
      setTick(next);
    });
    return cancel;
  }, []);

  // 订阅引擎状态（EV-08）：模型来源切换 / 降级后按钮文案与禁用要跟着变。
  useEffect(() => {
    const cancel = EngineService.onStatusChanged((next) => {
      if (!aliveRef.current) return;
      setEngine(next);
    });
    return cancel;
  }, []);

  // —— 边录边转（FR-03-10）：读开关 + 探能力 ——
  //
  // 两件事分开取：开关是用户意愿（appsettings），能力是本机有没有流式模型
  // （transcriptsvc.LiveAvailability，便宜探测、不加载模型）。任一失败都不阻塞录制页。
  const loadLiveInfo = useCallback(async (): Promise<void> => {
    const [settingsResult, availabilityResult] = await Promise.allSettled([
      AppSettingsService.get(),
      LiveTranscriptService.getAvailability(),
    ]);
    if (!aliveRef.current) return;
    if (settingsResult.status === 'fulfilled') {
      setLiveEnabled(settingsResult.value.liveTranscribe);
    }
    if (availabilityResult.status === 'fulfilled') {
      setLiveCaps(availabilityResult.value);
    }
  }, []);

  useEffect(() => {
    void loadLiveInfo();
  }, [loadLiveInfo]);

  // 跨窗口 / 设置页改开关后同步（EV-14 appsettings:changed）。
  useEffect(() => {
    const cancel = AppSettingsService.onChanged((field) => {
      if (field !== 'liveTranscribe') return;
      void loadLiveInfo();
    });
    return cancel;
  }, [loadLiveInfo]);

  // 订阅流式 partial（EV-04）：**整体替换**而不是追加 —— partial 是累积文本。
  useEffect(() => {
    const cancel = LiveTranscriptService.onPartial((seg) => {
      if (!aliveRef.current) return;
      // 只显示当前会话的 partial：窗口重开时可能收到上一场的残留事件。
      if (sessionIdRef.current !== '' && seg.sessionId !== sessionIdRef.current) return;
      setLivePartial(seg.text ?? '');
    });
    return cancel;
  }, []);

  // 订阅逐句定稿（EV-05）：定稿后清掉 partial（它已经被这一句取代）。
  useEffect(() => {
    const cancel = LiveTranscriptService.onSegment((seg) => {
      if (!aliveRef.current) return;
      if (sessionIdRef.current !== '' && seg.sessionId !== sessionIdRef.current) return;
      setLivePartial('');
      // 只保留最近 20 句：录制页是「正在发生什么」的视图，不是回放列表。
      setLiveFinalized((prev) => [...prev, seg].slice(-20));
    });
    return cancel;
  }, []);

  const refresh = useCallback(async (): Promise<void> => {
    setRefreshing(true);
    setError(null);
    try {
      const next = await CaptureService.refreshSources();
      if (!aliveRef.current) return;
      setSources(next);
    } catch (err) {
      if (!aliveRef.current) return;
      setError(toAppError(err));
    } finally {
      if (aliveRef.current) setRefreshing(false);
    }
  }, []);

  const recordingState = recording?.state ?? RecordingStates.Idle;
  const recordingActive = isRecordingActive(recordingState);
  const recordingSettled = isRecordingSettled(recordingState);

  const select = useCallback(
    async (source: Source): Promise<void> => {
      // 不可用的源不响应点击（View 里也做了 disabled 语义，这里再防一层）
      if (!source.present) return;
      // 录制中切源没有意义：当前会话已经绑定在采集流上，后端也不会迁移。
      if (recordingActive) return;
      const previousId = selectedId;
      setSelectedId(source.id);
      setError(null);
      try {
        await CaptureService.setPreferredSource(source.id);
      } catch (err) {
        if (!aliveRef.current) return;
        // 持久化失败则回滚高亮：不能让 UI 显示一个后端并不认识的"已选择"。
        setSelectedId(previousId);
        setError(toAppError(err));
      }
    },
    [selectedId, recordingActive],
  );

  // 统一的动作包装：busy 期间禁止重复点击，失败把结构化错误交给 View 展示。
  const runAction = useCallback(
    async (action: () => Promise<RecordingState>): Promise<void> => {
      setBusy(true);
      setError(null);
      try {
        const next = await action();
        if (!aliveRef.current) return;
        applyState(next);
      } catch (err) {
        if (!aliveRef.current) return;
        setError(toAppError(err));
      } finally {
        if (aliveRef.current) setBusy(false);
      }
    },
    [applyState],
  );

  const start = useCallback(async (): Promise<void> => {
    if (selectedId === '') {
      setError(
        new AppError({
          code: 'ERR_INVALID_ARGUMENT',
          message: 'no source selected',
          action: '',
          detail: '',
          retryable: false,
        }),
      );
      return;
    }
    // modelSource / title 留空 = 用后端当前设置（模型来源）与自动命名（日期 · 来源应用）。
    await runAction(() =>
      RecordingService.start({ sourceId: selectedId, modelSource: '', title: '' }),
    );
  }, [selectedId, runAction]);

  const pause = useCallback(async (): Promise<void> => {
    await runAction(() => RecordingService.pause());
  }, [runAction]);

  const resume = useCallback(async (): Promise<void> => {
    await runAction(() => RecordingService.resume());
  }, [runAction]);

  const stop = useCallback(async (): Promise<void> => {
    await runAction(() => RecordingService.stop());
  }, [runAction]);

  const exportAudio = useCallback(async (targetPath: string): Promise<string | null> => {
    const sessionId = recording?.sessionId ?? '';
    if (sessionId === '' || targetPath === '') return null;
    setError(null);
    try {
      const saved = await RecordingService.exportAudio(sessionId, targetPath);
      return aliveRef.current ? saved : null;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return null;
    }
  }, [recording]);

  const recover = useCallback(async (): Promise<boolean> => {
    const sessionId = recording?.sessionId ?? '';
    if (sessionId === '') return false;
    setError(null);
    try {
      const session = await RecordingService.recoverSession(sessionId);
      if (!aliveRef.current) return false;
      // 修补成功后用 GetState 对齐一次：会话状态可能已从 interrupted 变成可回放。
      const next = await RecordingService.getState();
      if (aliveRef.current) applyState(next);
      return session.id !== '';
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return false;
    }
  }, [recording, applyState]);

  const setModelSource = useCallback(
    async (source: ModelSource): Promise<void> => {
      // 录制中切换模型来源会被后端拒绝，前端先挡住（控件同时已 disabled）。
      if (isRecordingActive(recording?.state ?? '')) return;
      setError(null);
      try {
        const status = await EngineService.setSource(source);
        if (aliveRef.current) setEngine(status);
      } catch (err) {
        if (aliveRef.current) setError(toAppError(err));
      }
    },
    [recording],
  );

  const dismissError = useCallback((): void => setError(null), []);

  const applications = useMemo(
    () => sources.filter((s) => sourceKindOf(s) === 'application'),
    [sources],
  );
  const devices = useMemo(
    () => sources.filter((s) => sourceKindOf(s) === 'device'),
    [sources],
  );
  const selectedSource = useMemo(
    () => sources.find((s) => s.id === selectedId) ?? null,
    [sources, selectedId],
  );
  // 有记忆的选择、但当前列表里找不到它或它已不可用 → 需要提示而不是静默。
  const selectedUnavailable = selectedId !== '' && !(selectedSource?.present ?? false);

  const metrics = useMemo<RecordingMetrics>(() => {
    if (recording === null) return EMPTY_METRICS;
    const silentMs = level?.silentMs ?? 0;
    return {
      peak: level?.peak ?? 0,
      rms: level?.rms ?? 0,
      silentMs,
      // 只在真正录制时提示静音：暂停期间本来就没有音频。
      silent: recording.state === RecordingStates.Recording && silentMs >= SILENCE_WARN_MS,
      // 时长只用后端帧计数换算值：tick（1Hz）比状态事件更新更频繁。
      recordedMs: tick?.elapsedMs ?? recording.elapsedMs,
      bytesWritten: tick?.bytesWritten ?? 0,
      charCount: tick?.charCount ?? 0,
    };
  }, [recording, level, tick]);

  const canStart =
    !busy &&
    !recordingActive &&
    selectedId !== '' &&
    (selectedSource?.present ?? false) &&
    !loading;  const canPause = !busy && recordingState === RecordingStates.Recording;
  const canResume = !busy && recordingState === RecordingStates.Paused;
  const canStop = !busy && (recordingState === RecordingStates.Recording || recordingState === RecordingStates.Paused);
  const canExportAudio = !busy && !recordingActive && (recording?.sessionId ?? '') !== '';

  const live = useMemo<LiveTranscript>(
    () => ({
      enabled: liveEnabled,
      available: liveCaps?.available ?? false,
      reason: liveCaps?.reason ?? '',
      errorCode: liveCaps?.errorCode ?? '',
      partial: livePartial,
      finalized: liveFinalized,
    }),
    [liveEnabled, liveCaps, livePartial, liveFinalized],
  );

  return {
    caps,
    applications,
    devices,
    selectedId,
    selectedSource,
    selectedUnavailable,
    loading,
    refreshing,
    recording,
    recordingState,
    recordingActive,
    recordingSettled,
    metrics,
    busy,
    live,
    engine,
    canStart,
    canPause,
    canResume,
    canStop,
    canExportAudio,
    start,
    pause,
    resume,
    stop,
    exportAudio,
    recover,
    setModelSource,
    refresh,
    select,
    error,
    dismissError,
  };
};
