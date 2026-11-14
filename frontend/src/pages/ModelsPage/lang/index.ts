import { localeRegistry } from '@/i18n';
import { modelsPageZhCN } from './zh-CN';
import { modelsPageEnUS } from './en-US';

// 把 ModelsPage 的语言包合并到已注册的全局 locale。
// App 启动时调用一次（与 registerHomePageLocales 同位置），幂等。
let registered = false;

export const registerModelsPageLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', modelsPageZhCN);
  localeRegistry.extend('en-US', modelsPageEnUS);
  registered = true;
};
