import { localeRegistry } from '@/i18n';
import { audioZhCN } from './zh-CN';
import { audioEnUS } from './en-US';

let registered = false;

export const registerAudioLocales = (): void => {
  if (registered) return;
  localeRegistry.extend('zh-CN', audioZhCN);
  localeRegistry.extend('en-US', audioEnUS);
  registered = true;
};
