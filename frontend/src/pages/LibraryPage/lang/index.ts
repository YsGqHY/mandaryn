import { localeRegistry } from '@/i18n';
import { libraryPageZhCN } from './zh-CN';
import { libraryPageEnUS } from './en-US';

// 把 LibraryPage 的语言包合并到已注册的全局 locale。
// App 启动时调用一次（与 registerRecordPageLocales 同位置），幂等。
let registered = false;

export const registerLibraryPageLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', libraryPageZhCN);
  localeRegistry.extend('en-US', libraryPageEnUS);
  registered = true;
};
