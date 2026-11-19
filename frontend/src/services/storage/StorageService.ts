// StorageService：数据根目录与待迁移设置 / 数据库占用 / 表级统计与清空。
// 设置目录仅暂存迁移计划；应用下次启动时才迁移，失败则保留原目录。
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/storagesvc';

export interface StorageStats {
  path: string;
  isCustom: boolean;
  defaultPath: string;
  sizeBytes: number;
  dataRoot: string;
  pendingRoot: string;
  managedBy: string;
  overrideHint: string;
  migrationError: string;
}

// 单张表的展示元数据 + 占用。
// labelKey 是 i18n 键的"后缀"——前端拼成 settings.database.tables.<key>.label。
// estimated=true 表示后端无法用 dbstat 取精确字节数，UI 应注明"估算"。
export interface TableInfo {
  name: string;
  labelKey: string;
  clearable: boolean;
  rowCount: number;
  sizeBytes: number;
  estimated: boolean;
}

export interface TableStats {
  totalBytes: number;
  tables: TableInfo[];
}

export const StorageService = {
  async getStats(): Promise<StorageStats> {
    const s = await Binding.GetStats();
    return {
      path: s.path ?? '',
      isCustom: Boolean(s.isCustom),
      defaultPath: s.defaultPath ?? '',
      sizeBytes: Number(s.sizeBytes ?? 0),
      dataRoot: s.dataRoot ?? '',
      pendingRoot: s.pendingRoot ?? '',
      managedBy: s.managedBy ?? '',
      overrideHint: s.overrideHint ?? '',
      migrationError: s.migrationError ?? '',
    };
  },

  async getTableStats(): Promise<TableStats> {
    const s = await Binding.GetTableStats();
    return {
      totalBytes: Number(s.totalBytes ?? 0),
      tables: (s.tables ?? []).map<TableInfo>((t) => ({
        name: t.name ?? '',
        labelKey: t.labelKey ?? '',
        clearable: Boolean(t.clearable),
        rowCount: Number(t.rowCount ?? 0),
        sizeBytes: Number(t.sizeBytes ?? 0),
        estimated: Boolean(t.estimated),
      })),
    };
  },

  // 目标必须是绝对目录，不能是数据库文件。
  async setCustomPath(directory: string): Promise<void> {
    await Binding.SetCustomPath(directory);
  },

  async resetPath(): Promise<void> {
    await Binding.ResetPath();
  },

  async cancelRootMigration(): Promise<void> {
    await Binding.CancelRootMigration();
  },

  async clearTable(name: string): Promise<void> {
    await Binding.ClearTable(name);
  },
} as const;
