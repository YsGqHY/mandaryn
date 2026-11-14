import { localeRegistry } from '@/i18n';
import { privacyZhCN } from './zh-CN';
import { privacyEnUS } from './en-US';

let registered = false;

export const registerPrivacyLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', privacyZhCN);
  localeRegistry.extend('en-US', privacyEnUS);
  registered = true;
};
