import { localeRegistry } from '@/i18n';
import { hotkeysZhCN } from './zh-CN';
import { hotkeysEnUS } from './en-US';

let registered = false;

export const registerHotkeysLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', hotkeysZhCN);
  localeRegistry.extend('en-US', hotkeysEnUS);
  registered = true;
};
