import { expect, test, type Page } from '@playwright/test';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { expectAudioPlayable, navTo, openAppAt, pickDefaultDevice, startRecording, stopRecording, TEST_TONE_LAUNCH } from './helpers';
import { killServer, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

const port = Number(process.env.E2E_FLAC_PORT ?? 9379);
const base = `http://127.0.0.1:${port}`;
test.use({ launchOptions: TEST_TONE_LAUNCH });

function streamSamples(file: Buffer): number {
  expect(file.toString('ascii', 0, 4)).toBe('fLaC');
  expect(file[4] & 0x7f).toBe(0);
  return Number(file.readBigUInt64BE(18) & 0xFFFFFFFFFn);
}

async function setFLAC(page: Page) {
  await navTo(page, '设置');
  await page.getByRole('button', { name: /^音频与录制/ }).click();
  const format = page.getByRole('combobox', { name: '录制保存格式' });
  await expect(format).toBeEnabled();
  await expect(format).toHaveValue('WAV');
  await format.click();
  await page.getByRole('option', { name: 'FLAC', exact: true }).click();
  await expect(format).toHaveValue('FLAC');
  await expect(format).toBeEnabled();
}

for (const crash of [false, true]) {
  test(crash ? 'FLAC 崩溃后恢复 STREAMINFO 并可回放' : '选择 FLAC 后录制、封存及回放同一文件', async ({ page }) => {
    test.setTimeout(65_000);
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-flac-e2e-'));
    const dataDir = path.join(root, 'data');
    const env = { MANDARYN_MODEL_ROOT: path.join(root, 'model-root'), MANDARYN_OFFLINE: '1' };
    let server: ServerHandle | null = null;
    let sessionId = '';
    const session = () => {
      const db = new DatabaseSync(path.join(dataDir, 'mandaryn.db'), { readOnly: true });
      try { return db.prepare('SELECT id,status,audio_path,audio_format,duration_ms,size_bytes FROM sessions WHERE id=?').get(sessionId); }
      finally { db.close(); }
    };
    try {
      server = startServer({ port, dataDir, env });
      await waitForServer(port, 20_000, server);
      await openAppAt(page, base);
      await setFLAC(page);
      await navTo(page, '录制');
      await pickDefaultDevice(page);
      await startRecording(page, 3);
      const ids = fs.readdirSync(path.join(dataDir, 'sessions'));
      expect(ids).toHaveLength(1);
      sessionId = ids[0];
      const file = path.join(dataDir, 'sessions', sessionId, 'audio.flac');
      expect(session()).toMatchObject({ audio_path: `sessions/${sessionId}/audio.flac`, audio_format: 'flac', status: 'recording' });
      if (crash) {
        expect(streamSamples(fs.readFileSync(file))).toBe(0);
        expect(fs.statSync(file).size).toBeGreaterThan(2048);
        await killServer(server);
        server = startServer({ port, dataDir, fresh: false, env });
        await waitForServer(port, 20_000, server);
        await openAppAt(page, base);
      } else {
        await stopRecording(page);
      }
      await expect.poll(() => session()?.status).toBe(crash ? 'interrupted' : 'completed');
      const bytes = fs.readFileSync(file);
      const samples = streamSamples(bytes);
      expect(samples).toBeGreaterThan(32_000);
      expect(session()?.duration_ms).toBe(Math.floor(samples / 16));
      expect(session()?.size_bytes).toBe(bytes.length);
      expect(bytes.length).toBeLessThan(samples * 2);
      const response = await page.request.get(`${base}/audio/${sessionId}`, { headers: { Range: 'bytes=0-3' } });
      expect(response.status()).toBe(206);
      expect(response.headers()['content-type']).toContain('audio/flac');
      expect((await response.body()).toString('ascii')).toBe('fLaC');
      await navTo(page, '会话库');
      await page.locator(`[data-session-id="${sessionId}"]`).getByRole('button', { name: '回放', exact: true }).click();
      expect(await expectAudioPlayable(page, 0.03)).toBeGreaterThan(2);
    } finally {
      await stopServer(server);
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
}
