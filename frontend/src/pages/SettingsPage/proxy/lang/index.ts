import { localeRegistry } from '@/i18n';
import { proxyZhCN } from './zh-CN';
import { proxyEnUS } from './en-US';

let registered = false;

export const registerProxyLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', proxyZhCN);
  localeRegistry.extend('en-US', proxyEnUS);
  registered = true;
};
