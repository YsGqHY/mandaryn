// ModelsService：模型仓库（清单 / 已装 / 下载 / 校验 / 删除 / 目录迁移）的前端封装。
//
// 后端 internal/services/modelssvc（API-6.1~6.13）：
//   ListAvailable / ListInstalled / Download / PauseDownload / CancelDownload /
//   Verify / Remove / GetStoreStats / SetModelRoot / MigrateModelRoot /
//   CancelMigration / GetMigrationState / ImportOffline
//
// 语义提醒（与后端契约一致）：
//   - SetModelRoot 只做预检并返回 MigratePlan，**不搬迁数据**；
//   - 真正搬迁走 MigrateModelRoot(MigrateRequest)，进度经 models:migrate 事件推送；
//   - 下载进度经 models:download（4Hz 节流）；仓库变化经 models:changed。
import { Events } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/modelssvc';
import type {
  BundleCatalog,
  DownloadState,
  InstalledBundle,
  MigratePlan,
  MigrateRequest,
  MigrateState,
  ModelChanged,
  ModelMirror,
  StoreStats,
  VerifyResult,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export，避免前端另造一套重复类型。
export type {
  BundleInfo,
  DownloadState,
  InstalledBundle,
  MigratePlan,
  MigrateRequest,
  MigrateState,
  ModelChanged,
  ModelMirror,
  StoreStats,
  VerifyResult,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';

export const ModelsService = {
  async getModelMirror(): Promise<ModelMirror> {
    try { return await Binding.GetModelMirror(); }
    catch (err) { throw toAppError(err); }
  },
  async setModelMirror(url: string): Promise<ModelMirror> {
    try { return await Binding.SetModelMirror(url); }
    catch (err) { throw toAppError(err); }
  },
  // 可下载清单（远端缓存 + 内置兜底，API-6.1）。
  async listAvailable(refresh = false): Promise<BundleCatalog> {
    try {
      return await Binding.ListAvailable(refresh);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 已装包列表（含 state / 占用，API-6.2）。
  async listInstalled(): Promise<InstalledBundle[]> {
    try {
      return await Binding.ListInstalled();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 开始 / 继续下载（API-6.3），返回首个进度快照；后续进度走事件。
  async download(bundleId: string): Promise<DownloadState> {
    try {
      return await Binding.Download(bundleId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 暂停下载并保留分片（API-6.4）。
  async pauseDownload(bundleId: string): Promise<void> {
    try {
      await Binding.PauseDownload(bundleId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 取消下载（API-6.5）。removeParts=true 时同时删掉已下分片。
  async cancelDownload(bundleId: string, removeParts: boolean): Promise<void> {
    try {
      await Binding.CancelDownload(bundleId, removeParts);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 重新哈希校验（API-6.6）。
  async verify(bundleId: string): Promise<VerifyResult> {
    try {
      return await Binding.Verify(bundleId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 删除已装包（API-6.7）。
  async remove(bundleId: string): Promise<void> {
    try {
      await Binding.Remove(bundleId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 目录与占用（API-6.8）。
  async getStoreStats(): Promise<StoreStats> {
    try {
      return await Binding.GetStoreStats();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 迁移预检（API-6.9）：只返回计划，不动数据。
  async setModelRoot(target: string): Promise<MigratePlan> {
    try {
      return await Binding.SetModelRoot(target);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 执行迁移（API-6.10），返回初始进度快照；后续进度走 models:migrate 事件。
  async migrateModelRoot(request: MigrateRequest): Promise<MigrateState> {
    try {
      return await Binding.MigrateModelRoot(request);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 取消迁移并回滚（API-6.11）。
  async cancelMigration(): Promise<void> {
    try {
      await Binding.CancelMigration();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 迁移进度轮询兜底（API-6.12，事件丢失时）。
  async getMigrationState(): Promise<MigrateState> {
    try {
      return await Binding.GetMigrationState();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 导入离线包（API-6.13，内网部署）。
  async importOffline(archivePath: string): Promise<InstalledBundle> {
    try {
      return await Binding.ImportOffline(archivePath);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅下载进度（EV-09 models:download，4Hz）。返回取消订阅函数。
  onDownload(callback: (state: DownloadState) => void): () => void {
    const cancel = Events.On(AppEvents.ModelsDownload, (event) => {
      const payload = event.data as DownloadState | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },

  // 订阅仓库变化（EV-10 models:changed：安装 / 删除 / 校验完成）。返回取消订阅函数。
  onChanged(callback: (payload: ModelChanged) => void): () => void {
    const cancel = Events.On(AppEvents.ModelsChanged, (event) => {
      const payload = event.data as ModelChanged | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },

  // 订阅迁移进度（EV-11 models:migrate）。返回取消订阅函数。
  onMigrate(callback: (state: MigrateState) => void): () => void {
    const cancel = Events.On(AppEvents.ModelsMigrate, (event) => {
      const payload = event.data as MigrateState | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
