import { localeRegistry } from '@/i18n';
import { transcriptionEnUS } from './en-US';
import { transcriptionZhCN } from './zh-CN';

export function registerTranscriptionLocales() {
  localeRegistry.extend('en-US', transcriptionEnUS);
  localeRegistry.extend('zh-CN', transcriptionZhCN);
}
