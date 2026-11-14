import { useCallback, useEffect, useRef, useState } from 'react';
import { AppSettingsService, LiveTranscriptService, toAppError } from '@/services';

// 音频子页面 VM（FR-03-10「边录边转」开关）。
//
// 只做三件事：
//   1. 读开关（appsettings.liveTranscribe）与能力（LiveAvailability）；
//   2. 写开关（后端录制中会拒绝 → ERR_STATE_FORBIDDEN，这里映射成提示）；
//   3. 把「开关」与「能力」两个事实分别暴露给 View，由 View 决定怎么说话。
//
// 为什么不在 VM 里拼文案：i18n 归 View（与其它子页面一致）。

// SaveStatus 是保存反馈（与 performance 子页面的形态一致）。
export type SaveStatus =
  | { kind: 'idle' }
  | { kind: 'saving' }
  | { kind: 'saved' }
  | { kind: 'failed'; code: string; message: string };

export interface UseAudioResult {
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
    const [settingsResult, availabilityResult] = await Promise.allSettled([
      AppSettingsService.get(),
      LiveTranscriptService.getAvailability(),
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
    setLoading(false);
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // 跨窗口同步：任一 setter 写成功后后端广播 appsettings:changed（载荷=字段名）。
  useEffect(() => {
    const cancel = AppSettingsService.onChanged((field) => {
      if (field !== 'liveTranscribe') return;
      void load();
    });
    return cancel;
  }, [load]);

  const toggle = useCallback(
    async (next: boolean): Promise<void> => {
      // 乐观更新 + 失败回滚：开关是即时控件，等一次往返会让手感发黏。
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
      }
    },
    [enabled],
  );

  const refresh = useCallback(async (): Promise<void> => {
    setStatus({ kind: 'idle' });
    await load();
  }, [load]);

  return {
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
