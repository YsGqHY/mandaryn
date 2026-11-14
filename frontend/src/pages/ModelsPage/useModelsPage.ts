import { useCallback, useEffect, useRef, useState } from 'react';
import {
  ModelsService,
  toAppError,
  type AppError,
  type BundleInfo,
  type DownloadState,
  type InstalledBundle,
  type MigratePlan,
  type MigrateState,
  type StoreStats,
  type VerifyResult,
} from '@/services';
import { PathsService, type ResolvedPath } from '@/services/models/PathsService';

export interface UseModelsPageResult {
  stats: StoreStats | null;
  installed: InstalledBundle[];
  available: BundleInfo[];
  // 下载进度：bundleId → 最新快照（事件驱动，4Hz）
  downloads: Record<string, DownloadState>;
  // 迁移进度：null = 没有进行过迁移
  migrate: MigrateState | null;
  migrating: boolean;
  // 生效的 modelRoot 及其来源（source 由后端给，前端只按它渲染）
  modelRoot: ResolvedPath | null;
  // 待确认的迁移计划：非 null 时页面显示「计划卡片」，等用户二次确认
  plan: MigratePlan | null;
  // 已尝试过的计划：失败 / 取消后用于「重试」
  attemptedPlan: MigratePlan | null;
  loading: boolean;
  error: AppError | null;
  // 拉取「可下载模型」清单失败的原因：单独展示在该卡片里，不进页头错误条 ——
  // 清单失败与「下载失败」共用 ERR_DOWNLOAD_* 错误码，混在页头会被读成「下载时网络中断」。
  catalogError: AppError | null;
  // 正在做耗时操作（校验 / 删除 / 开始下载）的包 ID
  busyBundleId: string | null;
  reload: () => Promise<void>;
  verifyBundle: (bundleId: string) => Promise<VerifyResult | null>;
  removeBundle: (bundleId: string) => Promise<boolean>;
  startDownload: (bundleId: string) => Promise<void>;
  pauseDownload: (bundleId: string) => Promise<void>;
  cancelDownload: (bundleId: string) => Promise<void>;
  // 预检：返回迁移计划（不执行）；null 表示预检失败（错误已进 error）
  planMigrate: (target: string) => Promise<MigratePlan | null>;
  // 放弃当前待确认的计划
  discardPlan: () => void;
  startMigrate: (plan: MigratePlan) => Promise<void>;
  // 重试用上一次的计划再跑一遍
  retryMigrate: () => Promise<void>;
  cancelMigrate: () => Promise<void>;
  // 受管（env / cli）状态下改目录：写入暂存值，不搬迁数据
  stageModelRoot: (target: string) => Promise<ResolvedPath | null>;
  revealModelRoot: () => Promise<void>;
  copyText: (text: string) => Promise<void>;
  dismissError: () => void;
}

// 迁移进行中 = 处于这些阶段之外。
const TERMINAL_PHASES = new Set(['', 'idle', 'done', 'failed', 'rollback']);

// ModelsPage ViewModel：
//   - 列表与统计全部以后端为准（没有本地镜像）
//   - 下载 / 迁移进度靠事件推送，事件里带 bundleId / phase
//   - 原生对话框（确认 / 选目录 / 提示）由 View 负责，VM 只做数据与调用
export const useModelsPage = (): UseModelsPageResult => {
  const [stats, setStats] = useState<StoreStats | null>(null);
  const [installed, setInstalled] = useState<InstalledBundle[]>([]);
  const [available, setAvailable] = useState<BundleInfo[]>([]);
  const [downloads, setDownloads] = useState<Record<string, DownloadState>>({});
  const [migrate, setMigrate] = useState<MigrateState | null>(null);
  const [modelRoot, setModelRoot] = useState<ResolvedPath | null>(null);
  const [plan, setPlan] = useState<MigratePlan | null>(null);
  const [attemptedPlan, setAttemptedPlan] = useState<MigratePlan | null>(null);
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<AppError | null>(null);
  const [catalogError, setCatalogError] = useState<AppError | null>(null);
  const [busyBundleId, setBusyBundleId] = useState<string | null>(null);
  const aliveRef = useRef<boolean>(true);

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const reload = useCallback(async (): Promise<void> => {
    const [statsResult, installedResult, availableResult] = await Promise.allSettled([
      ModelsService.getStoreStats(),
      ModelsService.listInstalled(),
      ModelsService.listAvailable(),
    ]);
    if (!aliveRef.current) return;

    if (statsResult.status === 'fulfilled') setStats(statsResult.value);
    if (installedResult.status === 'fulfilled') setInstalled(installedResult.value);
    if (availableResult.status === 'fulfilled') setAvailable(availableResult.value);
    setCatalogError(availableResult.status === 'rejected' ? toAppError(availableResult.reason) : null);

    // 只剩「统计读不到」不报错：那通常意味着目录还没建；已安装列表失败才进页头错误条。
    // 清单失败由 catalogError 在「可下载模型」卡片里说明。
    if (installedResult.status === 'rejected') setError(toAppError(installedResult.reason));
  }, []);

  // reloadPaths 单独一条：路径来源由 pathsvc 提供，与仓库统计是两次读，
  // 迁移后要先失效解析器缓存（后端 OnModelRootChanged 已做）再读才拿得对。
  const reloadPaths = useCallback(async (): Promise<void> => {
    try {
      const resolved = await PathsService.getModelRoot();
      if (aliveRef.current) setModelRoot(resolved);
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    }
  }, []);

  // 首次加载：列表 + 统计 + 路径来源 + 迁移状态（事件丢失时的兜底）。
  useEffect(() => {
    let cancelled = false;
    const load = async (): Promise<void> => {
      await reload();
      await reloadPaths();
      // 迁移可能在上次会话里被打断，这里读一次当前阶段用于展示。
      try {
        const state = await ModelsService.getMigrationState();
        if (!cancelled && aliveRef.current && state.phase !== '' && state.phase !== 'idle') {
          setMigrate(state);
        }
      } catch {
        // 兜底读失败不影响主流程
      }
      if (!cancelled && aliveRef.current) setLoading(false);
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [reload, reloadPaths]);

  // 事件订阅：下载进度 / 仓库变化 / 迁移进度。
  useEffect(() => {
    const offDownload = ModelsService.onDownload((state) => {
      if (!aliveRef.current) return;
      setDownloads((prev) => ({ ...prev, [state.bundleId]: state }));
    });
    const offChanged = ModelsService.onChanged((payload) => {
      if (!aliveRef.current) return;
      // 安装 / 删除 / 校验完成：仓库里的东西变了，整表刷新最省心。
      void reload();
      if (payload.installed) {
        // 装完把该包的下载进度项清掉，避免残留"已完成"进度条
        setDownloads((prev) => {
          if (prev[payload.bundleId] === undefined) return prev;
          const next = { ...prev };
          delete next[payload.bundleId];
          return next;
        });
      }
    });
    const offMigrate = ModelsService.onMigrate((state) => {
      if (!aliveRef.current) return;
      setMigrate(state);
      // 终态才刷新：进行中的 4Hz 进度事件不该带着 IO 一起打满。
      if (TERMINAL_PHASES.has(state.phase)) {
        // 成功要看到新根的体积 / 包列表；失败与回滚也要看到「旧目录还在」的实况。
        void reload();
        void reloadPaths();
      }
    });
    return () => {
      offDownload();
      offChanged();
      offMigrate();
    };
  }, [reload, reloadPaths]);

  const verifyBundle = useCallback(async (bundleId: string): Promise<VerifyResult | null> => {
    setBusyBundleId(bundleId);
    setError(null);
    try {
      const result = await ModelsService.verify(bundleId);
      if (!aliveRef.current) return null;
      // 校验会改 state（corrupt ↔ installed），刷一次列表
      void reload();
      return result;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return null;
    } finally {
      if (aliveRef.current) setBusyBundleId(null);
    }
  }, [reload]);

  const removeBundle = useCallback(async (bundleId: string): Promise<boolean> => {
    setBusyBundleId(bundleId);
    setError(null);
    try {
      await ModelsService.remove(bundleId);
      if (!aliveRef.current) return false;
      await reload();
      return true;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return false;
    } finally {
      if (aliveRef.current) setBusyBundleId(null);
    }
  }, [reload]);

  const startDownload = useCallback(async (bundleId: string): Promise<void> => {
    setBusyBundleId(bundleId);
    setError(null);
    try {
      const state = await ModelsService.download(bundleId);
      if (!aliveRef.current) return;
      setDownloads((prev) => ({ ...prev, [state.bundleId]: state }));
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    } finally {
      if (aliveRef.current) setBusyBundleId(null);
    }
  }, []);

  const pauseDownload = useCallback(async (bundleId: string): Promise<void> => {
    setError(null);
    try {
      await ModelsService.pauseDownload(bundleId);
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    }
  }, []);

  const cancelDownload = useCallback(async (bundleId: string): Promise<void> => {
    setError(null);
    try {
      // removeParts=true：取消即丢弃已下分片（UI 上「取消下载」的语义）。
      await ModelsService.cancelDownload(bundleId, true);
      if (!aliveRef.current) return;
      setDownloads((prev) => {
        if (prev[bundleId] === undefined) return prev;
        const next = { ...prev };
        delete next[bundleId];
        return next;
      });
      void reload();
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    }
  }, [reload]);

  const planMigrate = useCallback(async (target: string): Promise<MigratePlan | null> => {
    setError(null);
    try {
      // 注意：SetModelRoot（API-6.9）只做预检并返回计划，**不搬迁数据**，
      // 也**不写**用户设置；用户二次确认后才会写库（preflight → switching 落库）。
      const next = await ModelsService.setModelRoot(target);
      if (!aliveRef.current) return null;
      setPlan(next);
      return next;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return null;
    }
  }, []);

  const discardPlan = useCallback((): void => setPlan(null), []);

  const runMigrate = useCallback(
    async (target: MigratePlan): Promise<void> => {
      setError(null);
      setAttemptedPlan(target);
      setPlan(null);
      try {
        // 网络路径（warnings 含 'network-path'）走「复制模式」：保留源，双份共存。
        const copyOnly = target.warnings.includes('network-path');
        const state = await ModelsService.migrateModelRoot({ target: target.to, copyOnly });
        if (!aliveRef.current) return;
        setMigrate(state);
      } catch (err) {
        if (aliveRef.current) setError(toAppError(err));
      }
    },
    [],
  );

  // startMigrate / retryMigrate 都只是 runMigrate 的入口；分开命名是为了让
  // View 层语义清楚（计划卡片上的「开始迁移」vs 失败后的「重试」）。
  const startMigrate = runMigrate;

  const retryMigrate = useCallback(async (): Promise<void> => {
    if (attemptedPlan === null) return;
    await runMigrate(attemptedPlan);
  }, [attemptedPlan, runMigrate]);

  const cancelMigrate = useCallback(async (): Promise<void> => {
    setError(null);
    try {
      await ModelsService.cancelMigration();
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    }
  }, []);

  // 受管（env / cli 接管）状态下改目录：只写暂存值，不搬数据。
  // 为什么不能顺手迁移：生效路径由 env / cli 决定，搬到一个「不生效的目录」
  // 等于把用户的模型文件挪进死角。见 ADR-02 / FR-07-8。
  const stageModelRoot = useCallback(async (target: string): Promise<ResolvedPath | null> => {
    setError(null);
    try {
      const resolved = await PathsService.setModelRoot(target);
      if (!aliveRef.current) return null;
      setModelRoot(resolved);
      return resolved;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return null;
    }
  }, []);

  const revealModelRoot = useCallback(async (): Promise<void> => {
    try {
      await PathsService.reveal('modelRoot');
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    }
  }, []);

  const copyText = useCallback(async (text: string): Promise<void> => {
    try {
      await PathsService.copyText(text);
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
    }
  }, []);

  const dismissError = useCallback((): void => setError(null), []);

  const migrating = migrate !== null && !TERMINAL_PHASES.has(migrate.phase);

  return {
    stats,
    installed,
    available,
    downloads,
    migrate,
    migrating,
    modelRoot,
    plan,
    attemptedPlan,
    loading,
    error,
    catalogError,
    busyBundleId,
    reload,
    verifyBundle,
    removeBundle,
    startDownload,
    pauseDownload,
    cancelDownload,
    planMigrate,
    discardPlan,
    startMigrate,
    retryMigrate,
    cancelMigrate,
    stageModelRoot,
    revealModelRoot,
    copyText,
    dismissError,
  };
};
