import { localeRegistry } from '@/i18n';
import { aboutZhCN } from './zh-CN';
import { aboutEnUS } from './en-US';

let registered = false;

export const registerAboutLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', aboutZhCN);
  localeRegistry.extend('en-US', aboutEnUS);
  registered = true;
};
