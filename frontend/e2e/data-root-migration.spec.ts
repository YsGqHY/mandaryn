import { chromium, expect, test, type Browser } from '@playwright/test';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { expectAudioPlayable, navTo, openAppAt } from './helpers';
import { startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

const port = Number(process.env.E2E_DATA_ROOT_PORT ?? 9388);
const base = `http://127.0.0.1:${port}`;
const id = 's_E2EDATAROOT000000000000';
const relativeAudio = `sessions/${id}/audio.wav`;
// Opt-in: point this at a Windows binary built with production,desktop_smoke.
const desktop = process.env.E2E_DATA_ROOT_DESKTOP;
let desktopLaunches = 0;

function migrationAudio(): Buffer {
  const fixture = process.env.E2E_DATA_ROOT_WAV;
  if (fixture) return fs.readFileSync(fixture);
  // A silent PCM fixture keeps the regression runnable without private audio.
  const wav = Buffer.alloc(44 + 64000);
  wav.write('RIFF'); wav.writeUInt32LE(wav.length - 8, 4); wav.write('WAVEfmt ', 8);
  wav.writeUInt32LE(16, 16); wav.writeUInt16LE(1, 20); wav.writeUInt16LE(1, 22);
  wav.writeUInt32LE(16000, 24); wav.writeUInt32LE(32000, 28); wav.writeUInt16LE(2, 32);
  wav.writeUInt16LE(16, 34); wav.write('data', 36); wav.writeUInt32LE(64000, 40);
  return wav;
}

for (const fail of [false, true]) {
  test(fail ? '数据根迁移遇到冲突后保留旧根和音频' : '受管暂存解除后重启迁移整个数据根', async ({ page: browserPage }) => {
    test.setTimeout(75_000);
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-root-e2e-'));
    const source = path.join(root, 'managed');
    const target = path.join(root, 'destination');
    const env = { AppData: path.join(root, 'profile'), XDG_DATA_HOME: path.join(root, 'profile'), HOME: path.join(root, 'profile'), MANDARYN_MODEL_ROOT: path.join(root, 'models'), MANDARYN_OFFLINE: '1' };
    let server: ServerHandle | null = null;
    let webview: Browser | undefined;
    let page = browserPage;
    async function launch(managed: boolean, fresh: boolean) {
      if (desktop) {
        const debugPort = port + desktopLaunches++;
        const endpoint = `http://127.0.0.1:${debugPort}`;
        const child = spawn(path.resolve(desktop), [], { stdio: 'ignore', env: {
          ...process.env, ...env, E2E_NATIVE_DESKTOP_PORT: String(debugPort), MANDARYN_DATA_DIR: managed ? source : '',
        } });
        server = { child, port: debugPort, dataDir: source, logs: [] };
        await expect.poll(async () => {
          try { return (await fetch(`${endpoint}/json/version`, { signal: AbortSignal.timeout(600) })).ok; }
          catch { return false; }
        }, { timeout: 20_000 }).toBe(true);
        webview = await chromium.connectOverCDP(endpoint);
        const context = webview.contexts()[0];
        page = context.pages().find(p => !p.url().includes('systray.html')) ?? await context.waitForEvent('page');
      } else {
        server = startServer({ port, dataDir: source, managedDataRoot: managed, fresh, env });
        await waitForServer(port, 20_000, server);
        await openAppAt(page, base);
      }
      await navTo(page, '设置');
      await page.getByRole('button', { name: /^数据存储/ }).click();
    }
    async function stop() {
      await webview?.close().catch(() => {});
      webview = undefined;
      await stopServer(server);
      server = null;
    }
    try {
      await launch(true, true);
      const db = new DatabaseSync(path.join(source, 'mandaryn.db'));
      try {
        const now = Date.now();
        db.prepare(`INSERT INTO sessions (id,title,source_id,model_source,status,started_at,ended_at,duration_ms,audio_path,audio_format,created_at,updated_at)
          VALUES (?,?,'dev:test','local','completed',?,?,2000,?,'wav',?,?)`).run(id, '根目录迁移夹具', now - 2000, now, relativeAudio, now, now);
      } finally { db.close(); }
      const wav = migrationAudio();
      fs.mkdirSync(path.join(source, 'sessions', id), { recursive: true });
      fs.writeFileSync(path.join(source, relativeAudio), wav);
      fs.writeFileSync(path.join(source, 'migration-fixture.txt'), 'durable user data');
      // The application creates its key lazily; seed a test-only key if the fixture has none.
      const keyPath = path.join(source, '.master.key');
      if (!fs.existsSync(keyPath)) fs.writeFileSync(keyPath, Buffer.alloc(32, 0xa5), { mode: 0o600 });
      const secret = fs.readFileSync(keyPath);
      await expect(page.getByTestId('data-root-current')).toHaveValue(source);
      await expect(page.getByTestId('data-root-managed')).toBeVisible();
      const input = page.getByTestId('data-root-target');
      await input.fill(target);
      await input.press('Enter');
      await expect(page.getByTestId('data-root-pending')).toHaveValue(target);
      expect(fs.existsSync(target)).toBe(false);
      await stop();
      await launch(true, false);
      await expect(page.getByTestId('data-root-current')).toHaveValue(source);
      await expect(page.getByTestId('data-root-pending')).toHaveValue(target);
      expect(fs.existsSync(target)).toBe(false);
      await stop();
      if (fail) {
        fs.mkdirSync(target);
        fs.writeFileSync(path.join(target, 'conflict.txt'), 'must not be overwritten');
      }
      await launch(false, false);
      await expect(page.getByTestId('data-root-current')).toHaveValue(fail ? source : target);
      if (fail) {
        await expect(page.getByTestId('data-root-migration-error')).toBeVisible();
        await expect(page.getByTestId('data-root-pending')).toHaveValue(target);
        expect(fs.readFileSync(path.join(target, 'conflict.txt'), 'utf8')).toBe('must not be overwritten');
        expect(fs.existsSync(path.join(target, 'mandaryn.db'))).toBe(false);
      } else {
        await expect(page.getByTestId('data-root-pending')).toHaveCount(0);
        expect(fs.readFileSync(path.join(target, 'migration-fixture.txt'), 'utf8')).toBe('durable user data');
        expect(fs.readFileSync(path.join(target, '.master.key'))).toEqual(secret);
        expect(fs.readFileSync(path.join(target, relativeAudio))).toEqual(wav);
      }
      expect(fs.readFileSync(path.join(source, relativeAudio))).toEqual(wav);
      await navTo(page, '会话库');
      await expect(page.getByText('根目录迁移夹具', { exact: true })).toBeVisible();
      const response = await page.evaluate(async (session) => {
        const audio = await fetch(`/audio/${session}`, { headers: { Range: 'bytes=0-43' } });
        return { status: audio.status, body: Array.from(new Uint8Array(await audio.arrayBuffer())) };
      }, id);
      expect(response.status).toBe(206);
      expect(Buffer.from(response.body)).toEqual(wav.subarray(0, 44));
      await page.getByRole('button', { name: '回放', exact: true }).first().click();
      expect(await expectAudioPlayable(page, 0.03)).toBeGreaterThan(1);
    } finally {
      await stop();
      try {
        await fs.promises.rm(root, { recursive: true, force: true, maxRetries: 7, retryDelay: 250 });
      } catch (error) {
        // The external WebView2 process can outlive the test app and keep its profile open.
        if (!desktop || !['EPERM', 'EBUSY'].includes((error as NodeJS.ErrnoException).code ?? '')) throw error;
        console.warn(`WebView2 still holds the isolated test profile: ${root}`);
      }
    }
  });
}
