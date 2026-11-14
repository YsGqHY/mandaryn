import { useCallback, useEffect, useRef, useState } from 'react';
import {
  AppSettingsService,
  PerfService,
  RESOURCE_CAP_MAX,
  RESOURCE_CAP_MIN,
  RESOURCE_CAP_STEP,
  hasCode,
  toAppError,
  type PerfUsage,
} from '@/services';

// 预算提交的反馈状态（只用于文案，不参与判断）。
export type PerformanceStatus =
  | { kind: 'idle' }
  | { kind: 'saving' }
  | { kind: 'saved' }
  | { kind: 'failed'; code: string; message: string };

export interface UsePerformanceResult {
  // cap 是后端已确认的预算 N；draft 是滑块当前显示值（拖动中领先于 cap）。
  cap: number;
  draft: number;
  usage: PerfUsage | null;
  status: PerformanceStatus;
  // setDraft 只动本地显示；commit 才落库（避免拖动过程中反复写库）。
  setDraft: (value: number) => void;
  commit: () => Promise<void>;
}

// 后端默认预算（与 storage 模型 default:80 对齐），首屏未读回时兜底。
const FALLBACK_CAP = 80;

// 把任意数值夹到合法预算（10–100，步进 5）。
export const clampCap = (value: number): number => {
  const clamped = Math.min(RESOURCE_CAP_MAX, Math.max(RESOURCE_CAP_MIN, value));
  const stepped = Math.round(clamped / RESOURCE_CAP_STEP) * RESOURCE_CAP_STEP;
  return Math.min(RESOURCE_CAP_MAX, Math.max(RESOURCE_CAP_MIN, stepped));
};

// 性能预算子页面 ViewModel：
//   - mount 读一次已存预算 + 订阅 EV-15 perf:usage（1Hz）
//   - commit 写 appsettings.SetResourceCap；越界错误码 ERR_PERF_CAP_RANGE 交给视图提示
//   - unmount 取消订阅（事件订阅必须成对）
export const usePerformance = (): UsePerformanceResult => {
  const [cap, setCap] = useState<number>(FALLBACK_CAP);
  const [draft, setDraftState] = useState<number>(FALLBACK_CAP);
  const [usage, setUsage] = useState<PerfUsage | null>(null);
  const [status, setStatus] = useState<PerformanceStatus>({ kind: 'idle' });
  const aliveRef = useRef(true);

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  // 已存预算：走 appsettings 快照（写入口在 PerfService，读入口在同一行表）。
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const snap = await AppSettingsService.get();
        if (cancelled || !aliveRef.current) return;
        if (snap.resourceCapPct > 0) {
          setCap(snap.resourceCapPct);
          setDraftState(snap.resourceCapPct);
        }
      } catch {
        // 静默：保留默认值，实时读数仍可工作
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // 实时三分量读数（EV-15，1Hz）。卸载时取消订阅。
  useEffect(() => {
    const off = PerfService.onUsage((next) => {
      if (!aliveRef.current) return;
      setUsage(next);
    });
    return off;
  }, []);

  const setDraft = useCallback((value: number) => {
    setDraftState(value);
  }, []);

  const commit = useCallback(async (): Promise<void> => {
    const target = clampCap(draft);
    if (target === cap) {
      // 没变化就不打后端（SetResourceCap 每次都会落库并广播）。
      setDraftState(cap);
      return;
    }
    setStatus({ kind: 'saving' });
    try {
      const next = await PerfService.setCap(target);
      if (!aliveRef.current) return;
      setCap(target);
      setDraftState(target);
      // 后端成功时回当前读数，直接用它刷新展示（不必等下一次采样）。
      setUsage(next);
      setStatus({ kind: 'saved' });
    } catch (err) {
      if (!aliveRef.current) return;
      // 越界要给「10–100，步进 5」的明确提示，不能只说「参数错误」。
      if (hasCode(err, 'ERR_PERF_CAP_RANGE')) {
        setDraftState(cap);
        setStatus({ kind: 'failed', code: 'ERR_PERF_CAP_RANGE', message: toAppError(err).message });
        return;
      }
      const appErr = toAppError(err);
      setDraftState(cap);
      setStatus({ kind: 'failed', code: appErr.code, message: appErr.message });
    }
  }, [cap, draft]);

  return { cap, draft, usage, status, setDraft, commit };
};
