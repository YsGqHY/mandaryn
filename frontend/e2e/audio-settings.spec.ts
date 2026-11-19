import { expect, test, type Page } from '@playwright/test';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { navTo, openAppAt } from './helpers';
import { REPO_ROOT, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

const PORT = Number(process.env.E2E_AUDIO_PORT ?? 9378);
const DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'audio-settings');
const BASE = `http://127.0.0.1:${PORT}`;
const DB = path.join(DATA_DIR, 'mandaryn.db');
const source = (page: Page) => page.getByRole('combobox', { name: '默认音频源', exact: true });
const limit = (page: Page) => page.getByRole('combobox', { name: '说话人上限', exact: true });
const autoName = (page: Page) => page.getByRole('switch', { name: /^会话自动命名/ });
const openAudio = async (page: Page) => {
  await openAppAt(page, BASE);
  await navTo(page, '设置');
  await page.getByRole('button', { name: /^音频与录制/ }).click();
  await expect(autoName(page)).toBeEnabled();
};
const settingsRow = () => {
  const db = new DatabaseSync(DB, { readOnly: true });
  try {
    return db.prepare('SELECT default_source_id, max_speakers, auto_name, save_format, sensitivity, no_audio_persist FROM app_settings WHERE id=1').get();
  } finally {
    db.close();
  }
};

test('音频设置保存真实值：兼容旧来源、显式清空、跨页同步与重启后恢复', async ({ page, context }) => {
  test.setTimeout(90_000);
  let server: ServerHandle | null = null;
  try {
    server = startServer({ port: PORT, dataDir: DATA_DIR });
    await waitForServer(PORT, 20_000, server);
    await stopServer(server);
    const db = new DatabaseSync(DB);
    try {
      // 旧版本只在 preferences 记来源；隐藏字段不能被新 UI 默认值覆盖。
      db.prepare('INSERT OR REPLACE INTO preferences(key,value) VALUES (?,?)').run('capture.preferredSource', 'dev:audio-settings-unavailable');
      db.prepare("UPDATE app_settings SET default_source_id='', save_format='flac', sensitivity='noisy', no_audio_persist=1 WHERE id=1").run();
    } finally {
      db.close();
    }
    server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false });
    await waitForServer(PORT, 20_000, server);
    await openAudio(page);
    await expect(source(page)).toHaveValue('已保存的音频源（暂不可用）');
    await expect(page.getByText('默认音频源当前不可用，请重新选择；不会自动改录其它来源。')).toBeVisible();
    await expect(page.getByRole('combobox', { name: '录制保存格式' })).toHaveValue('FLAC');
    await expect(page.getByRole('combobox', { name: '分句灵敏度' })).toHaveValue('嘈杂');

    const recordPage = await context.newPage();
    await openAppAt(recordPage, BASE);
    await navTo(recordPage, '录制');
    await expect(recordPage.getByText('上次选择的源已不可用，请重新选择。')).toBeVisible();

    await limit(page).click();
    await page.getByRole('option', { name: '3 人', exact: true }).click();
    await expect(limit(page)).toBeEnabled();
    await expect(limit(page)).toHaveValue('3 人');
    await autoName(page).click();
    await expect(autoName(page)).not.toBeChecked();
    await expect(autoName(page)).toBeEnabled();
    await page.getByRole('button', { name: '清空默认音频源', exact: true }).click();
    await expect(source(page)).toHaveValue('');
    await expect(recordPage.getByText('上次选择的源已不可用，请重新选择。')).toBeHidden();
    await recordPage.close();
    expect(settingsRow()).toMatchObject({ default_source_id: '', max_speakers: 3, auto_name: 0, save_format: 'flac', sensitivity: 'noisy', no_audio_persist: 1 });

    await stopServer(server);
    server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false });
    await waitForServer(PORT, 20_000, server);
    await openAudio(page);
    await expect(source(page)).toHaveValue('');
    await expect(limit(page)).toHaveValue('3 人');
    await expect(autoName(page)).not.toBeChecked();
    const sensitivity = page.getByRole('combobox', { name: '分句灵敏度' });
    await expect(sensitivity).toHaveValue('嘈杂');
    await sensitivity.click();
    await page.getByRole('option', { name: '标准', exact: true }).click();
    await expect.poll(() => settingsRow()?.sensitivity).toBe('standard');
    await expect(page.getByText('默认音频源当前不可用，请重新选择；不会自动改录其它来源。')).toHaveCount(0);
    expect(settingsRow()).toMatchObject({ save_format: 'flac', sensitivity: 'standard', no_audio_persist: 1 });
  } finally {
    await stopServer(server);
  }
});
