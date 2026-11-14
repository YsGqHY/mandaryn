import { localeRegistry } from '@/i18n';
import { recordPageZhCN } from './zh-CN';
import { recordPageEnUS } from './en-US';

// 把 RecordPage 的语言包合并到已注册的全局 locale。
// App 启动时调用一次（与 registerHomePageLocales 同位置），幂等。
let registered = false;

export const registerRecordPageLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', recordPageZhCN);
  localeRegistry.extend('en-US', recordPageEnUS);
  registered = true;
};
