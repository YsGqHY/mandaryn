// UpdatesService：检查更新（API-11.1 / 11.2）的前端封装。
//
// 后端 internal/services/updatesvc：
//   Version()  当前版本（不联网；离线也能用）
//   Check()    查最新版本（经 outbound 留痕 + 离线闸门；不做静默自更新）
//
// 铁律（FR-08）：后端**不下载、不安装**任何东西，只回答"有没有新版本"。
// 打开下载页由前端用系统浏览器完成（Browser.OpenURL），因此这一路也不产生后端的出站请求。
import { Browser } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/updatesvc';
import type { UpdateInfo } from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type { UpdateInfo } from '@bindings/vip.oberon.mandaryn/internal/contract/models';

export const UpdatesService = {
  // 当前版本号。不联网，因此离线时也能显示。
  async version(): Promise<string> {
    try {
      return await Binding.Version();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 检查更新。离线模式下后端直接返回 ERR_OFFLINE_ENFORCED（不发请求）。
  async check(): Promise<UpdateInfo> {
    try {
      return await Binding.Check();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 用系统默认浏览器打开 URL。
  //
  // 为什么要前端来开：让用户看到真实地址、由浏览器负责后续（下载、警告页），
  // 应用本身不参与下载，也不接触安装包 —— 与"不做静默自更新"一致。
  async openExternal(url: string): Promise<void> {
    // 只允许 http(s)：防止后端返回异常 scheme（如 file://）被当作可执行动作。
    if (!/^https?:\/\//i.test(url)) {
      throw toAppError(new Error(`拒绝打开非 http(s) 地址：${url}`));
    }
    await Browser.OpenURL(url);
  },
} as const;
