import { useCallback, useEffect, useRef, useState } from 'react';
import {
  AppSettingsService,
  PrivacyService,
  toAppError,
  type AppError,
  type DebugLogLevel,
  type DebugLogOrigin,
  type DebugLogPage,
  type DebugLogStatus,
} from '@/services';
import { PathsService } from '@/services/models/PathsService';

// 查看器筛选条件（'' = 不过滤）。
export interface DebugLogFilter {
  minLevel: DebugLogLevel;
  origin: DebugLogOrigin;
  component: string;
}

export interface UseDebugLogResult {
  // —— 开关 ——
  status: DebugLogStatus | null;
  statusLoading: boolean;
  toggling: boolean;
  // envManaged：由环境变量 MANDARYN_LOG_LEVEL 接管 —— 开关必须禁用，不得假装能改。
  envManaged: boolean;
  toggle: (enabled: boolean) => Promise<void>;

  // —— 查看器 ——
  page: DebugLogPage | null;
  pageLoading: boolean;
  filter: DebugLogFilter;
  setFilter: (patch: Partial<DebugLogFilter>) => void;
  refresh: () => Promise<void>;
  clearing: boolean;
  clear: () => Promise<boolean>;
  revealDir: () => Promise<void>;

  error: AppError | null;
  dismissError: () => void;
}

const EMPTY_FILTER: DebugLogFilter = { minLevel: '', origin: '', component: '' };

// 「调试模式」卡片的 ViewModel（设置 → 隐私边界）：
//   - 开关写 app_settings.log_level（AppSettingsService.setDebugMode），立即生效、无需重启；
//   - 状态 / 查看器 / 清空走 PrivacyService（调试模式关闭时已有日志照样能读）；
//   - 订阅 appsettings:changed('debugMode')：别的窗口改了开关，这里同步刷新。
export const useDebugLog = (): UseDebugLogResult => {
  const [status, setStatus] = useState<DebugLogStatus | null>(null);
  const [statusLoading, setStatusLoading] = useState<boolean>(true);
  const [toggling, setToggling] = useState<boolean>(false);
  const [page, setPage] = useState<DebugLogPage | null>(null);
  const [pageLoading, setPageLoading] = useState<boolean>(true);
  const [filter, setFilterState] = useState<DebugLogFilter>(EMPTY_FILTER);
  const [clearing, setClearing] = useState<boolean>(false);
  const [error, setError] = useState<AppError | null>(null);
  const aliveRef = useRef<boolean>(true);
  const filterRef = useRef<DebugLogFilter>(EMPTY_FILTER);

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const loadStatus = useCallback(async (): Promise<void> => {
    try {
      const next = await PrivacyService.getDebugLogStatus();
      if (aliveRef.current) setStatus(next);
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    } finally {
      if (aliveRef.current) setStatusLoading(false);
    }
  }, []);

  const loadPage = useCallback(async (f: DebugLogFilter): Promise<void> => {
    setPageLoading(true);
    try {
      const next = await PrivacyService.readDebugLog(f);
      if (aliveRef.current) setPage(next);
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    } finally {
      if (aliveRef.current) setPageLoading(false);
    }
  }, []);

  const refresh = useCallback(async (): Promise<void> => {
    await Promise.all([loadStatus(), loadPage(filterRef.current)]);
  }, [loadStatus, loadPage]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    const cancel = AppSettingsService.onChanged((field) => {
      if (field === 'debugMode' || field === 'privacy') void loadStatus();
    });
    return cancel;
  }, [loadStatus]);

  const setFilter = useCallback(
    (patch: Partial<DebugLogFilter>): void => {
      const next = { ...filterRef.current, ...patch };
      filterRef.current = next;
      setFilterState(next);
      void loadPage(next);
    },
    [loadPage],
  );

  const toggle = useCallback(
    async (enabled: boolean): Promise<void> => {
      setToggling(true);
      setError(null);
      try {
        await AppSettingsService.setDebugMode(enabled);
      } catch (err) {
        if (aliveRef.current) setError(toAppError(err));
      } finally {
        // 不管成功与否都以后端状态为准（开启失败时后端已回落为关）。
        await refresh();
        if (aliveRef.current) setToggling(false);
      }
    },
    [refresh],
  );

  const clear = useCallback(async (): Promise<boolean> => {
    setClearing(true);
    setError(null);
    try {
      const next = await PrivacyService.clearDebugLog();
      if (aliveRef.current) setStatus(next);
      await loadPage(filterRef.current);
      return true;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return false;
    } finally {
      if (aliveRef.current) setClearing(false);
    }
  }, [loadPage]);

  const revealDir = useCallback(async (): Promise<void> => {
    try {
      await PathsService.reveal('logs');
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    }
  }, []);

  const dismissError = useCallback((): void => setError(null), []);

  return {
    status,
    statusLoading,
    toggling,
    envManaged: status?.mode.source === 'env',
    toggle,
    page,
    pageLoading,
    filter,
    setFilter,
    refresh,
    clearing,
    clear,
    revealDir,
    error,
    dismissError,
  };
};
