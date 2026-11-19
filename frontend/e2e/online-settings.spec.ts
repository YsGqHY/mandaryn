import { expect, test, type Page } from '@playwright/test';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { navTo, openAppAt } from './helpers';
import { REPO_ROOT, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

const PORT = Number(process.env.E2E_ONLINE_SETTINGS_PORT ?? 9377);
const DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'online-settings');
const BASE = `http://127.0.0.1:${PORT}`;
const first = 'https://asr.example.invalid/one';
const second = 'https://asr.example.invalid/two';
const fixtureKey = 'sk-fixture-transcription-K7R9';
const mask = 'sk-***K7R9';

const openTranscription = async (page: Page) => {
  await navTo(page, '设置');
  await page.getByRole('button', { name: /^转写/ }).click();
  await expect(page.getByLabel('在线 HTTPS 端点')).toBeEnabled();
};

test('在线设置按端点征求同意、保留密钥掩码且不切换来源', async ({ page }) => {
  test.setTimeout(60_000);
  let server: ServerHandle | null = null;
  try {
    server = startServer({ port: PORT, dataDir: DATA_DIR, env: { MANDARYN_OFFLINE: '1' } });
    await waitForServer(PORT, 20_000, server);
    await openAppAt(page, BASE);
    await openTranscription(page);

    const endpoint = page.getByLabel('在线 HTTPS 端点');
    const model = page.getByLabel('在线模型名');
    const language = page.getByLabel('在线转写语种');
    const key = page.getByLabel('API 密钥', { exact: true });
    const consentPanel = page.getByRole('group', { name: '确认此端点的隐私风险' });
    const consent = consentPanel.getByRole('checkbox');
    const save = page.getByRole('button', { name: '保存在线设置' });
    const fallback = page.getByRole('switch', { name: '在线失败时降级到本地' });
    const switchBack = page.getByRole('switch', { name: '降级后自动尝试切回在线' });

    await expect(endpoint).toHaveValue('');
    await expect(language).toHaveValue('auto');
    await expect(fallback).toBeChecked();
    await expect(switchBack).not.toBeChecked();
    await endpoint.fill('http://asr.example.invalid');
    await expect(page.getByText('请输入有效的 HTTPS 基础地址，不要在 URL 中填写密钥。')).toBeVisible();
    await expect(consentPanel).toHaveCount(0);

    await endpoint.fill(first);
    await expect(consentPanel.getByText(first, { exact: true })).toBeVisible();
    await expect(consent).not.toBeChecked();
    await expect(save).toBeDisabled();
    await page.getByRole('button', { name: /^网络代理/ }).click();
    await openTranscription(page);
    await expect(endpoint).toHaveValue('');

    await endpoint.fill(first);
    await model.fill('model-initial');
    await language.selectOption('other');
    await page.getByLabel('语种代码').fill('ja');
    await expect(key).toHaveAttribute('type', 'password');
    await key.fill(fixtureKey);
    await consent.check();
    await save.click();
    await expect(page.getByText('在线设置已保存。')).toBeVisible();
    await expect(key).toHaveValue('');
    await expect(key).toHaveAttribute('placeholder', mask);
    await expect(consentPanel).toHaveCount(0);

    await model.fill('model-updated');
    await save.click();
    await expect(save).toBeDisabled();
    await expect(key).toHaveAttribute('placeholder', mask);

    // A new path on the same host still requires a fresh, unchecked agreement.
    await endpoint.fill(second);
    await expect(consentPanel.getByText(second, { exact: true })).toBeVisible();
    await expect(consent).not.toBeChecked();
    await expect(save).toBeDisabled();
    await consent.check();
    await endpoint.fill('https://other.example.invalid');
    await expect(consent).not.toBeChecked();
    await expect(save).toBeDisabled();
    await page.getByRole('button', { name: /^网络代理/ }).click();
    await openTranscription(page);
    await expect(endpoint).toHaveValue(first);
    await endpoint.fill(second);
    await consent.check();
    await save.click();
    await expect(save).toBeDisabled();
    await expect(consentPanel).toHaveCount(0);
    await expect(key).toHaveAttribute('placeholder', mask);

    await fallback.click();
    await expect(fallback).not.toBeChecked();
    await switchBack.click();
    await expect(switchBack).toBeChecked();
    await navTo(page, '录制');
    await expect(page.getByRole('radio', { name: '本地', exact: true })).toBeChecked();
    await navTo(page, '设置');
    await page.getByRole('button', { name: /^隐私边界/ }).click();
    await expect(page.getByText('最近没有出站记录。')).toBeVisible();

    await stopServer(server);
    server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false, env: { MANDARYN_OFFLINE: '1' } });
    await waitForServer(PORT, 20_000, server);
    await openAppAt(page, BASE);
    await openTranscription(page);
    await expect(endpoint).toHaveValue(second);
    await expect(model).toHaveValue('model-updated');
    await expect(language).toHaveValue('other');
    await expect(page.getByLabel('语种代码')).toHaveValue('ja');
    await expect(key).toHaveValue('');
    await expect(key).toHaveAttribute('placeholder', mask);
    await expect(fallback).not.toBeChecked();
    await expect(switchBack).toBeChecked();
  } finally {
    await stopServer(server);
  }
});

test('在线不落盘设置跨重启保留，无音频会话可查看定稿', async ({ page }) => {
  const env = { MANDARYN_OFFLINE: '1' };
  let server: ServerHandle | null = null;
  try {
    server = startServer({ port: PORT, dataDir: DATA_DIR, env });
    await waitForServer(PORT, 20_000, server);
    await openAppAt(page, BASE);
    const openPrivacy = async () => {
      await navTo(page, '设置');
      await page.getByRole('button', { name: /^隐私边界/ }).click();
    };
    await openPrivacy();
    const retain = page.getByRole('switch', { name: '不保存在线会话的音频' });
    await expect(retain).toBeEnabled();
    await expect(retain).not.toBeChecked();
    await retain.click();
    await expect(retain).toBeChecked();
    await stopServer(server);
    const id = 's_E2ETEXTONLY000000000000';
    const db = new DatabaseSync(path.join(DATA_DIR, 'mandaryn.db'));
    try {
      const now = Date.now();
      db.prepare(`INSERT INTO sessions (id,title,model_source,status,started_at,ended_at,duration_ms,audio_path,audio_format,transcription_status,live_transcribed,segment_count,created_at,updated_at)
        VALUES (?,?,'online','completed',?,?,2000,'','','done',1,1,?,?)`).run(id, '在线定稿夹具', now - 2000, now, now, now);
      db.prepare('INSERT INTO segments (session_id,seq,start_ms,end_ms,text,is_final) VALUES (?,0,0,1500,?,1)').run(id, '保留的定稿文本');
    } finally { db.close(); }
    server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false, env });
    await waitForServer(PORT, 20_000, server);
    await openAppAt(page, BASE);
    await openPrivacy();
    await expect(retain).toBeChecked();
    await navTo(page, '会话库');
    const row = page.locator(`[data-session-id="${id}"]`);
    await expect(row.getByText('未保存音频', { exact: true })).toBeVisible();
    await expect(row.getByRole('button', { name: '未保存音频，无法转写或重试' })).toBeDisabled();
    await row.getByRole('button', { name: '查看转写文本' }).click();
    await expect(page.getByText('保留的定稿文本', { exact: true })).toBeVisible();
    await expect(page.locator('audio')).toHaveCount(0);
    expect((await page.request.get(`${BASE}/audio/${id}`)).status()).toBe(404);
  } finally {
    await stopServer(server);
  }
});
