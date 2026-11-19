import { useCallback, useEffect, useRef, useState } from 'react';
import { AppSettingsService, CaptureService, LiveTranscriptService, toAppError, type Source } from '@/services';
import type { AudioSettingsDTO } from '@/services/appsettings/AppSettingsService';

// 音频设置 VM：默认来源、说话人上限、自动命名与边录边转。
// 设置与能力分别读取；完整读回后只合并修改字段，不制造隐藏字段默认值。
// 文案由 View 翻译。

// SaveStatus 是保存反馈（与 performance 子页面的形态一致）。
export type SaveStatus =
  | { kind: 'idle' }
  | { kind: 'saving' }
  | { kind: 'saved' }
  | { kind: 'failed'; code: string; message: string };

export interface UseAudioResult {
  audio: AudioSettingsDTO | null;
  sources: Source[];
  sourcesFailed: boolean;
  updateAudio: (patch: Partial<Pick<AudioSettingsDTO, 'defaultSourceId' | 'maxSpeakers' | 'autoName' | 'saveFormat' | 'sensitivity'>>) => Promise<void>;
  // —— 状态 ——
  // enabled 开关当前值（默认 false）。
  enabled: boolean;
  // available 能力是否可用（缺流式模型包时为 false）。
  available: boolean;
  // reason 不可用原因（后端已给中文短说明；空串表示未探测到）。
  reason: string;
  // errorCode 不可用原因的错误码（用于 i18n 精确文案）。
  errorCode: string;
  // engineId 可用时会用到的流式引擎。
  engineId: string;
  loading: boolean;
  status: SaveStatus;
  // —— 动作 ——
  toggle: (next: boolean) => Promise<void>;
  refresh: () => Promise<void>;
}

export const useAudio = (): UseAudioResult => {
  const [audio, setAudio] = useState<AudioSettingsDTO | null>(null);
  const [sources, setSources] = useState<Source[]>([]);
  const [sourcesFailed, setSourcesFailed] = useState(false);
  const savingRef = useRef(false);
  const [enabled, setEnabled] = useState<boolean>(false);
  const [available, setAvailable] = useState<boolean>(false);
  const [reason, setReason] = useState<string>('');
  const [errorCode, setErrorCode] = useState<string>('');
  const [engineId, setEngineId] = useState<string>('');
  const [loading, setLoading] = useState<boolean>(true);
  const [status, setStatus] = useState<SaveStatus>({ kind: 'idle' });

  const aliveRef = useRef<boolean>(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const load = useCallback(async (): Promise<void> => {
    setLoading(true);
    // 两个来源互不阻塞：设置读不到也要能显示能力状态（反之亦然）。
    const [settingsResult, availabilityResult, audioResult, sourcesResult] = await Promise.allSettled([
      AppSettingsService.get(),
      LiveTranscriptService.getAvailability(),
      AppSettingsService.getAudio(),
      CaptureService.listSources(),
    ]);
    if (!aliveRef.current) return;

    if (settingsResult.status === 'fulfilled') {
      setEnabled(settingsResult.value.liveTranscribe);
    } else {
      setStatus({ kind: 'failed', code: '', message: toAppError(settingsResult.reason).message });
    }
    if (availabilityResult.status === 'fulfilled') {
      const a = availabilityResult.value;
      setAvailable(a.available);
      setReason(a.reason);
      setErrorCode(a.errorCode);
      setEngineId(a.engineId);
    }
    if (audioResult.status === 'fulfilled') {
      setAudio(audioResult.value);
    } else {
      setAudio(null);
      const err = toAppError(audioResult.reason);
      setStatus({ kind: 'failed', code: err.code, message: err.message });
    }
    setSourcesFailed(sourcesResult.status === 'rejected');
    if (sourcesResult.status === 'fulfilled') setSources(sourcesResult.value);
    setLoading(false);
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // 跨窗口同步：任一 setter 写成功后后端广播 appsettings:changed（载荷=字段名）。
  useEffect(() => {
    const cancel = AppSettingsService.onChanged((field) => {
      if (field !== 'liveTranscribe' && field !== 'audio') return;
      void load();
    });
    return cancel;
  }, [load]);

  useEffect(() => CaptureService.onSourcesChanged(setSources), []);

  const updateAudio = useCallback(async (patch: Partial<Pick<AudioSettingsDTO, 'defaultSourceId' | 'maxSpeakers' | 'autoName' | 'saveFormat' | 'sensitivity'>>): Promise<void> => {
    if (savingRef.current || audio === null) return;
    savingRef.current = true;
    setStatus({ kind: 'saving' });
    try {
      await AppSettingsService.updateAudio(patch);
      const next = await AppSettingsService.getAudio();
      if (!aliveRef.current) return;
      setAudio(next);
      setStatus({ kind: 'saved' });
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setStatus({ kind: 'failed', code: appErr.code, message: appErr.message });
    } finally {
      savingRef.current = false;
    }
  }, [audio]);

  const toggle = useCallback(
    async (next: boolean): Promise<void> => {
      // 乐观更新 + 失败回滚：开关是即时控件，等一次往返会让手感发黏。
      if (savingRef.current) return;
      savingRef.current = true;
      const previous = enabled;
      setEnabled(next);
      setStatus({ kind: 'saving' });
      try {
        await AppSettingsService.setLiveTranscribe(next);
        if (!aliveRef.current) return;
        setStatus({ kind: 'saved' });
      } catch (err) {
        if (!aliveRef.current) return;
        setEnabled(previous);
        const appErr = toAppError(err);
        setStatus({ kind: 'failed', code: appErr.code, message: appErr.message });
      } finally {
        savingRef.current = false;
      }
    },
    [enabled],
  );

  const refresh = useCallback(async (): Promise<void> => {
    setStatus({ kind: 'idle' });
    await load();
  }, [load]);

  return {
    audio,
    sources,
    sourcesFailed,
    updateAudio,
    enabled,
    available,
    reason,
    errorCode,
    engineId,
    loading,
    status,
    toggle,
    refresh,
  };
};
