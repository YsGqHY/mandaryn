import { useCallback, useEffect, useRef, useState } from 'react';
import { UpdatesService, toAppError, type AppError, type UpdateInfo } from '@/services';

// 检查更新的四种结果：未检查 / 检查中 / 已是最新 / 有新版本 / 失败。
// 明确区分「已最新」与「检查失败」—— 把失败显示成"已是最新"会让用户永远发现不了更新。
export type UpdateStatus =
  | { kind: 'idle' }
  | { kind: 'checking' }
  | { kind: 'latest' }
  | { kind: 'available'; info: UpdateInfo }
  | { kind: 'failed'; code: string; message: string };

export interface UseAboutResult {
  // 当前版本号（来自构建注入，不联网）。
  version: string;
  versionLoading: boolean;
  status: UpdateStatus;
  check: () => Promise<void>;
  openDownload: () => Promise<void>;
}

// 「关于与更新」子页的 ViewModel。
//
// 设计要点：
//   - 版本号与检查更新是**两件事**：版本号必须离线可见，检查才需要网络；
//   - 检查失败时保留具体错误码，由 View 映射文案（离线 / 网络 / 私有仓库取不到清单）；
//   - 不自动检查（不在挂载时发请求）：出站必须是用户显式动作，符合本项目"默认不联网"。
export const useAbout = (): UseAboutResult => {
  const [version, setVersion] = useState<string>('');
  const [versionLoading, setVersionLoading] = useState<boolean>(true);
  const [status, setStatus] = useState<UpdateStatus>({ kind: 'idle' });
  const aliveRef = useRef<boolean>(true);

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const v = await UpdatesService.version();
        if (!cancelled) setVersion(v);
      } catch {
        // 版本号读不到不是致命问题：界面显示占位，不打扰用户。
        if (!cancelled) setVersion('');
      } finally {
        if (!cancelled) setVersionLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const check = useCallback(async (): Promise<void> => {
    setStatus({ kind: 'checking' });
    try {
      const info = await UpdatesService.check();
      if (!aliveRef.current) return;
      setStatus(info.hasUpdate ? { kind: 'available', info } : { kind: 'latest' });
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr: AppError = toAppError(err);
      setStatus({ kind: 'failed', code: appErr.code, message: appErr.message });
    }
  }, []);

  const openDownload = useCallback(async (): Promise<void> => {
    if (status.kind !== 'available') return;
    const url = status.info.downloadUrl || status.info.notesUrl;
    if (url === '') {
      setStatus({ kind: 'failed', code: '', message: '发布页地址为空' });
      return;
    }
    try {
      await UpdatesService.openExternal(url);
    } catch (err) {
      const appErr = toAppError(err);
      if (aliveRef.current) setStatus({ kind: 'failed', code: appErr.code, message: appErr.message });
    }
  }, [status]);

  return { version, versionLoading, status, check, openDownload };
};

// 是否属于"离线模式拦截"——View 需要给出与网络失败不同的说明。
export const isOfflineBlocked = (status: UpdateStatus): boolean =>
  status.kind === 'failed' && status.code === 'ERR_OFFLINE_ENFORCED';
