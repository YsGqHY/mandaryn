import { useCallback, useEffect, useRef, useState } from 'react';
import {
  AppSettingsService,
  OUTBOUND_PAGE_SIZE,
  PrivacyService,
  isOfflineEnvForced,
  toAppError,
  type AuditReport,
  type ClearResult,
  type DiagnosticsArtifact,
  type OutboundPage,
  type PrivacyStatus,
  type SaveArtifactResult,
} from '@/services';

// 反馈作用区域（视图按 area 决定把提示放在哪一块）。
export type PrivacyArea = 'offline' | 'audioRetention' | 'records' | 'audit' | 'export';

// 操作反馈：只描述「发生了什么」，文案由视图查 i18n。
export type PrivacyNotice =
  | { kind: 'idle' }
  | { kind: 'error'; area: PrivacyArea; code: string; message: string }
  | { kind: 'cleared'; deleted: number }
  | { kind: 'exported' }
  | { kind: 'saved' }
  | { kind: 'saveUnsupported' };

export interface UsePrivacyResult {
  // —— 离线开关 ——
  status: PrivacyStatus | null;
  statusLoading: boolean;
  offlineBusy: boolean;
  // envForced：离线由环境变量接管 —— 界面必须禁用开关，不得假装能改。
  envForced: boolean;
  toggleOffline: (enabled: boolean) => Promise<void>;
  noAudioPersist: boolean | null;
  audioRetentionBusy: boolean;
  toggleNoAudioPersist: (enabled: boolean) => Promise<void>;

  // —— 出站记录 ——
  page: OutboundPage | null;
  pageIndex: number;
  pageCount: number;
  recordsBusy: boolean;
  goToPage: (index: number) => void;
  clearRecords: () => Promise<ClearResult | null>;

  // —— 零出站自检 ——
  audit: AuditReport | null;
  auditBusy: boolean;
  runAudit: () => Promise<void>;

  // —— 诊断包 ——
  artifact: DiagnosticsArtifact | null;
  artifactBytes: number;
  exportBusy: boolean;
  buildDiagnostics: () => Promise<void>;
  saveArtifact: (path: string) => Promise<SaveArtifactResult | null>;

  notice: PrivacyNotice;
  dismissNotice: () => void;
}

// 隐私边界子页面 ViewModel（FR-08）：
//   - mount 拉一次 GetStatus + 第一页出站记录
//   - 离线开关写 SetOffline；env 接管时后端仍返回 offline=true，故以返回值为准
//   - 清空留痕 / 诊断包导出都是显式用户动作（确认框由视图用 NativeDialogs 弹）
export const usePrivacy = (): UsePrivacyResult => {
  const [status, setStatus] = useState<PrivacyStatus | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [offlineBusy, setOfflineBusy] = useState(false);
  const [noAudioPersist, setNoAudioPersist] = useState<boolean | null>(null);
  const [audioRetentionBusy, setAudioRetentionBusy] = useState(false);

  const [page, setPage] = useState<OutboundPage | null>(null);
  const [pageIndex, setPageIndex] = useState(0);
  const [recordsBusy, setRecordsBusy] = useState(false);

  const [audit, setAudit] = useState<AuditReport | null>(null);
  const [auditBusy, setAuditBusy] = useState(false);

  const [artifact, setArtifact] = useState<DiagnosticsArtifact | null>(null);
  const [artifactBytes, setArtifactBytes] = useState(0);
  const [exportBusy, setExportBusy] = useState(false);

  const [notice, setNotice] = useState<PrivacyNotice>({ kind: 'idle' });

  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const dismissNotice = useCallback(() => setNotice({ kind: 'idle' }), []);

  // —— 状态与记录加载 ——
  const loadStatus = useCallback(async (): Promise<void> => {
    setStatusLoading(true);
    try {
      const next = await PrivacyService.getStatus();
      if (!aliveRef.current) return;
      setStatus(next);
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setNotice({ kind: 'error', area: 'offline', code: appErr.code, message: appErr.message });
    } finally {
      if (aliveRef.current) setStatusLoading(false);
    }
  }, []);

  const loadPage = useCallback(async (index: number): Promise<void> => {
    setRecordsBusy(true);
    try {
      const limit = OUTBOUND_PAGE_SIZE;
      const next = await PrivacyService.listOutbound(limit, Math.max(0, index) * limit);
      if (!aliveRef.current) return;
      setPage(next);
      // 后端回显 offset/limit：以它校正页码（例如越界后自动回落）
      const safeLimit = next.limit > 0 ? next.limit : limit;
      setPageIndex(Math.floor(next.offset / safeLimit));
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setNotice({ kind: 'error', area: 'records', code: appErr.code, message: appErr.message });
    } finally {
      if (aliveRef.current) setRecordsBusy(false);
    }
  }, []);

  useEffect(() => {
    void loadStatus();
    void loadPage(0);
  }, [loadStatus, loadPage]);

  useEffect(() => {
    let cancelled = false;
    void AppSettingsService.getNoAudioPersist().then((enabled) => {
      if (!cancelled) setNoAudioPersist(enabled);
    }).catch((err: unknown) => {
      if (cancelled) return;
      const { code, message } = toAppError(err);
      setNotice({ kind: 'error', area: 'audioRetention', code, message });
    });
    return () => { cancelled = true; };
  }, []);

  const toggleNoAudioPersist = useCallback(async (enabled: boolean): Promise<void> => {
    if (noAudioPersist === null || audioRetentionBusy) return;
    setAudioRetentionBusy(true);
    dismissNotice();
    try {
      await AppSettingsService.setNoAudioPersist(enabled);
      if (aliveRef.current) setNoAudioPersist(enabled);
    } catch (err) {
      if (!aliveRef.current) return;
      const { code, message } = toAppError(err);
      setNotice({ kind: 'error', area: 'audioRetention', code, message });
    } finally {
      if (aliveRef.current) setAudioRetentionBusy(false);
    }
  }, [audioRetentionBusy, dismissNotice, noAudioPersist]);

  const envForced = isOfflineEnvForced(status);

  const toggleOffline = useCallback(async (enabled: boolean): Promise<void> => {
    setOfflineBusy(true);
    dismissNotice();
    try {
      const next = await PrivacyService.setOffline(enabled);
      if (!aliveRef.current) return;
      // 以返回值为准：env 接管时即使关掉用户层，offline 仍是 true。
      setStatus(next);
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setNotice({ kind: 'error', area: 'offline', code: appErr.code, message: appErr.message });
    } finally {
      if (aliveRef.current) setOfflineBusy(false);
    }
  }, [dismissNotice]);

  const goToPage = useCallback(
    (index: number): void => {
      void loadPage(index);
    },
    [loadPage],
  );

  const clearRecords = useCallback(async (): Promise<ClearResult | null> => {
    setRecordsBusy(true);
    dismissNotice();
    try {
      const result = await PrivacyService.clearOutbound();
      if (!aliveRef.current) return null;
      // 清空后回到第一页并刷新汇总（blockedCount / recentCount 会变）。
      setPageIndex(0);
      await loadPage(0);
      await loadStatus();
      if (!aliveRef.current) return result;
      setNotice({ kind: 'cleared', deleted: result.deleted });
      return result;
    } catch (err) {
      if (!aliveRef.current) return null;
      const appErr = toAppError(err);
      setNotice({ kind: 'error', area: 'records', code: appErr.code, message: appErr.message });
      return null;
    } finally {
      if (aliveRef.current) setRecordsBusy(false);
    }
  }, [dismissNotice, loadPage, loadStatus]);

  const runAudit = useCallback(async (): Promise<void> => {
    setAuditBusy(true);
    dismissNotice();
    try {
      const report = await PrivacyService.runAudit();
      if (!aliveRef.current) return;
      setAudit(report);
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setNotice({ kind: 'error', area: 'audit', code: appErr.code, message: appErr.message });
    } finally {
      if (aliveRef.current) setAuditBusy(false);
    }
  }, [dismissNotice]);

  const buildDiagnostics = useCallback(async (): Promise<void> => {
    setExportBusy(true);
    dismissNotice();
    try {
      const next = await PrivacyService.exportDiagnostics();
      if (!aliveRef.current) return;
      setArtifact(next);
      setArtifactBytes(PrivacyService.artifactByteLength(next));
      setNotice({ kind: 'exported' });
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setNotice({ kind: 'error', area: 'export', code: appErr.code, message: appErr.message });
    } finally {
      if (aliveRef.current) setExportBusy(false);
    }
  }, [dismissNotice]);

  // path 必须来自视图的 NativeDialogs 原生「另存为」。
  const saveArtifact = useCallback(
    async (path: string): Promise<SaveArtifactResult | null> => {
      if (!artifact) return null;
      setExportBusy(true);
      try {
        const result = await PrivacyService.saveDiagnostics(path, artifact);
        if (!aliveRef.current) return result;
        setNotice(result.ok ? { kind: 'saved' } : { kind: 'saveUnsupported' });
        return result;
      } finally {
        if (aliveRef.current) setExportBusy(false);
      }
    },
    [artifact],
  );

  const pageCount = page && page.limit > 0 ? Math.max(1, Math.ceil(page.total / page.limit)) : 1;

  return {
    status,
    statusLoading,
    offlineBusy,
    envForced,
    toggleOffline,
    noAudioPersist,
    audioRetentionBusy,
    toggleNoAudioPersist,
    page,
    pageIndex,
    pageCount,
    recordsBusy,
    goToPage,
    clearRecords,
    audit,
    auditBusy,
    runAudit,
    artifact,
    artifactBytes,
    exportBusy,
    buildDiagnostics,
    saveArtifact,
    notice,
    dismissNotice,
  };
};
