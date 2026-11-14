// PathsService：路径解析（ADR-02 / API-9.11 ~ API-9.13）的前端封装。
//
// 为什么放在 services/models 下：本页只关心 **modelRoot** 的来源展示与
// 「受管时暂存」这一条路径；数据根（dataRoot）仍归设置页，避免两处重复维护。
//
// 铁律提醒（ADR-02）：
//   - 前端**只按后端返回的 source 渲染**（default / user / env / cli），
//     不得自行推断优先级；
//   - 被 env / cli 接管时提交的目录不会生效，后端写入 pendingValue，
//     界面必须说明「解除接管并重启后生效」。
import { Clipboard } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/pathsvc';
import type { ResolvedPath } from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export，避免前端另造一套重复类型。
export type { ResolvedPath } from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// 路径键（与后端 contract.PathKey* 一致）。
export const PATH_KEY_DATA_ROOT = 'dataRoot';
export const PATH_KEY_MODEL_ROOT = 'modelRoot';

// 路径来源（与后端 contract.PathSource* 一致）。前端只用它决定文案与可用操作。
export const PATH_SOURCE_DEFAULT = 'default';
export const PATH_SOURCE_USER = 'user';
export const PATH_SOURCE_ENV = 'env';
export const PATH_SOURCE_CLI = 'cli';

// isManagedSource 判断某个来源是否「被上层接管」（用户改设置不会生效）。
export const isManagedSource = (source: string): boolean =>
  source === PATH_SOURCE_ENV || source === PATH_SOURCE_CLI;

export const PathsService = {
  // 所有路径的生效值 / 来源 / 用户值 / 暂存值 / 解除提示（API-9.11）。
  async getResolvedPaths(): Promise<ResolvedPath[]> {
    try {
      return await Binding.GetResolvedPaths();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 只取 modelRoot 那一条（找不到时返回 null，而不是抛错）。
  async getModelRoot(): Promise<ResolvedPath | null> {
    const list = await PathsService.getResolvedPaths();
    return list.find((p) => p.key === PATH_KEY_MODEL_ROOT) ?? null;
  },

  // 设置模型目录的用户值（API-9.12）。受管时不报错，改为写入暂存值。
  async setModelRoot(path: string): Promise<ResolvedPath> {
    try {
      return await Binding.SetModelRoot(path);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 清除用户值与暂存值、回落默认（API-9.13）。
  async clearModelRoot(): Promise<ResolvedPath> {
    try {
      return await Binding.ClearModelRoot();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 在系统文件管理器中打开目录（API-9.14）。
  async reveal(key: string): Promise<void> {
    try {
      await Binding.RevealPath(key);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 复制文本到系统剪贴板（解除接管的命令要能一键复制，手抄环境变量极易出错）。
  // 走 Wails 运行时而非浏览器 API：WebView 下 navigator.clipboard 需要安全上下文，
  // 本地 http 资源并不总是满足。
  async copyText(text: string): Promise<void> {
    try {
      await Clipboard.SetText(text);
    } catch (err) {
      throw toAppError(err);
    }
  },
} as const;
