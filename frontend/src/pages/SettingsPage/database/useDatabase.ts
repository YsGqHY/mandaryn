import { useCallback, useEffect, useRef, useState } from 'react';
import {
  StorageService,
  type StorageStats,
  type TableInfo,
  type TableStats,
} from '@/services';
import { PathsService } from '@/services/models/PathsService';

type BusyStatus =
  | { kind: 'choosing' }
  | { kind: 'changing' }
  | { kind: 'resetting' }
  | { kind: 'cancelling' }
  | { kind: 'clearing'; tableName: string };

export type DatabaseStatus =
  | BusyStatus
  | { kind: 'idle' }
  | { kind: 'changed' }
  | { kind: 'reset' }
  | { kind: 'cancelled' }
  | { kind: 'copied' }
  | { kind: 'cleared'; tableName: string }
  | { kind: 'failed'; message: string };

// 数据库统计保持反映当前连接，目录改动仅暂存至下次启动。
export const useDatabase = () => {
  const [stats, setStats] = useState<StorageStats | null>(null);
  const [tableStats, setTableStats] = useState<TableStats | null>(null);
  const [status, setStatus] = useState<DatabaseStatus>({ kind: 'idle' });
  const [targetRoot, setTargetRoot] = useState('');
  const aliveRef = useRef(true);
  const operationRef = useRef(false);
  const busy = status.kind === 'choosing' || status.kind === 'changing' ||
    status.kind === 'resetting' || status.kind === 'cancelling' || status.kind === 'clearing';

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const refresh = useCallback(async (): Promise<void> => {
    try {
      // 并发拉两份；其中一份失败不影响另一份展示
      const [s, ts] = await Promise.allSettled([
        StorageService.getStats(),
        StorageService.getTableStats(),
      ]);
      if (!aliveRef.current) return;
      if (s.status === 'fulfilled') setStats(s.value);
      if (ts.status === 'fulfilled') setTableStats(ts.value);
    } catch {
      // 静默：保留上次成功值
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const run = async (start: BusyStatus, action: () => Promise<DatabaseStatus>): Promise<void> => {
    if (operationRef.current) return;
    operationRef.current = true;
    setStatus(start);
    try {
      const result = await action();
      if (aliveRef.current) setStatus(result);
    } catch (err) {
      if (aliveRef.current) setStatus({ kind: 'failed', message: getErrorMessage(err) });
    } finally {
      operationRef.current = false;
    }
  };

  const stageRoot = async (directory: string): Promise<DatabaseStatus> => {
    await StorageService.setCustomPath(directory);
    await refresh();
    if (aliveRef.current) setTargetRoot('');
    return { kind: 'changed' };
  };

  const submitRoot = () => {
    const directory = targetRoot.trim();
    if (directory) void run({ kind: 'changing' }, () => stageRoot(directory));
  };

  const chooseRoot = (pick: () => Promise<string | null>) =>
    run({ kind: 'choosing' }, async () => {
      const directory = await pick();
      if (!directory || !aliveRef.current) return { kind: 'idle' };
      setStatus({ kind: 'changing' });
      return stageRoot(directory);
    });

  const resetPath = () => run({ kind: 'resetting' }, async () => {
    await StorageService.resetPath();
    await refresh();
    if (aliveRef.current) setTargetRoot('');
    return { kind: 'reset' };
  });

  const cancelRootMigration = () => run({ kind: 'cancelling' }, async () => {
    await StorageService.cancelRootMigration();
    await refresh();
    if (aliveRef.current) setTargetRoot('');
    return { kind: 'cancelled' };
  });

  const clearTable = (table: TableInfo) => {
    if (!table.clearable) return;
    void run({ kind: 'clearing', tableName: table.name }, async () => {
      await StorageService.clearTable(table.name);
      await refresh();
      return { kind: 'cleared', tableName: table.name };
    });
  };

  const copyOverrideHint = async (hint: string) => {
    try {
      await PathsService.copyText(hint);
      if (aliveRef.current && !operationRef.current) setStatus({ kind: 'copied' });
    } catch (err) {
      if (aliveRef.current && !operationRef.current) {
        setStatus({ kind: 'failed', message: getErrorMessage(err) });
      }
    }
  };

  return {
    stats, tableStats, status, busy, targetRoot, setTargetRoot,
    submitRoot, chooseRoot, resetPath, cancelRootMigration, clearTable, copyOverrideHint,
  };
};

const getErrorMessage = (err: unknown): string => {
  if (err instanceof Error) return err.message;
  return String(err);
};
