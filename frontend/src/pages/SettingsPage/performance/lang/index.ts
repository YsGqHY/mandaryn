import { localeRegistry } from '@/i18n';
import { performanceZhCN } from './zh-CN';
import { performanceEnUS } from './en-US';

let registered = false;

export const registerPerformanceLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', performanceZhCN);
  localeRegistry.extend('en-US', performanceEnUS);
  registered = true;
};
