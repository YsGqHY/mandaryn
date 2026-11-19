import { chromium, expect, test, type Browser, type Page } from '@playwright/test';
import { spawn } from 'node:child_process';
import { DatabaseSync } from 'node:sqlite';
import { createHash } from 'node:crypto';
import { createServer } from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

const hash = (bytes: Buffer): string => createHash('sha256').update(bytes).digest('hex');
const modelName = 'silero_vad.onnx';
const upstreamSHA = '9e2449e1087496d8d4caba907f23e0bd3f78d91fa552479bb9c23ac09cbb1fd6';

test('模型清单：冷启动兜底，立即刷新并下载校验后的模型文件', async ({ page }) => {
  test.setTimeout(60_000);
  const asset = process.env.E2E_REAL_VAD_FILE;
  const file = asset === undefined ? Buffer.from('deterministic download fixture') : fs.readFileSync(asset);
  const digest = asset === undefined ? hash(file) : upstreamSHA;
  expect(hash(file)).toBe(digest);
  test.info().annotations.push({ type: 'asset', description: asset === undefined ? 'synthetic transport fixture' : 'verified upstream Silero VAD' });
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-e2e-catalog-'));
  const dataDir = path.join(root, 'data');
  const modelRoot = path.join(root, 'models-root');
  const bundleName = 'Catalog verification fixture';
  const bundleId = 'catalog-verification-vad';
  let online = false;
  let requests = 0;
  let ranges = 0;
  const mock = createServer((req, res) => {
    if (req.url === '/test/models.json') {
      requests++;
      if (!online) { res.writeHead(503).end(); return; }
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ schemaVersion: 1, bundles: [{
        bundleId, name: bundleName, version: '1.0.0', roles: ['vad'],
        files: [{ path: modelName, url: modelName, size: file.length, sha256: digest }],
      }] }));
      return;
    }
    const match = /^bytes=(\d+)-(\d+)$/.exec(req.headers.range ?? '');
    if (req.url !== `/test/${modelName}` || match === null) { res.writeHead(404).end(); return; }
    const start = Number(match[1]);
    const end = Math.min(Number(match[2]), file.length - 1);
    if (start > end) { res.writeHead(416).end(); return; }
    ranges++;
    const chunk = file.subarray(start, end + 1);
    res.writeHead(206, { 'Content-Range': `bytes ${start}-${end}/${file.length}`, 'Content-Length': chunk.length, ETag: '"verified-vad"' });
    res.end(chunk);
  });
  let server: ServerHandle | null = null;
  try {
    await new Promise<void>((resolve, reject) => {
      mock.once('error', reject);
      mock.listen(0, '127.0.0.1', resolve);
    });
    const source = mock.address();
    if (source === null || typeof source === 'string') throw new Error('fixture did not bind a TCP port');
    const port = Number(process.env.E2E_CATALOG_PORT ?? 9371);
    const catalogURL = `http://127.0.0.1:${source.port}/test/models.json`;
    server = startServer({ port, dataDir, env: {
      MANDARYN_MODEL_ROOT: modelRoot, MANDARYN_MODEL_CATALOG: catalogURL,
      MANDARYN_MODEL_MIRRORS: catalogURL, MANDARYN_OFFLINE: '0', MANDARYN_HTTP_PROXY: '',
    } });
    await waitForServer(port, 20_000, server);
    await page.goto(`http://127.0.0.1:${port}`);
    await expect(page.locator('#startup-skeleton')).toHaveCount(0);
    await page.getByRole('button', { name: '模型', exact: true }).click();
    await expect(page.getByText('此条目没有完整的下载信息。请联网刷新清单后再试。').first()).toBeVisible();
    await expect(page.getByRole('button', { name: '下载', exact: true }).first()).toBeDisabled();
    await expect(page.getByText(/无法获取模型清单|获取模型清单失败|未能获取模型清单/).first()).toBeVisible();
    const before = requests;
    online = true;
    await page.getByRole('button', { name: '刷新模型清单', exact: true }).click();
    await expect(page.getByText(bundleName, { exact: true })).toBeVisible();
    expect(requests).toBeGreaterThan(before);
    await page.getByRole('button', { name: '下载', exact: true }).click();
    const installed = path.join(modelRoot, 'models', `${bundleId}@1.0.0`);
    await expect.poll(() => fs.existsSync(path.join(installed, 'manifest.json')), { timeout: 25_000 }).toBe(true);
    expect(ranges).toBeGreaterThan(0);
    expect(hash(fs.readFileSync(path.join(installed, modelName)))).toBe(digest);
    await expect(page.getByText(bundleName, { exact: true })).toHaveCount(2);
  } finally {
    await stopServer(server);
    await new Promise<void>((resolve) => mock.close(() => resolve()));
    fs.rmSync(root, { recursive: true, force: true });
  }
});

// Network validation is explicit; the default CI run above remains deterministic.
test('无清单配置时从官方资产下载并校验 Silero VAD', async ({ page }) => {
  test.skip(process.env.E2E_OFFICIAL_MODEL_DOWNLOAD !== '1', '显式开启官方资产网络验收');
  test.setTimeout(90_000);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-e2e-official-model-'));
  const modelRoot = path.join(root, 'model-root');
  const port = Number(process.env.E2E_CATALOG_PORT ?? 9371);
  let server: ServerHandle | null = null;
  try {
    server = startServer({ port, dataDir: path.join(root, 'data'), env: {
      MANDARYN_MODEL_ROOT: modelRoot, MANDARYN_MODEL_CATALOG: '', MANDARYN_MODEL_MIRRORS: '', MANDARYN_OFFLINE: '0',
    } });
    await waitForServer(port, 20_000, server);
    await page.goto(`http://127.0.0.1:${port}`);
    await expect(page.locator('#startup-skeleton')).toHaveCount(0);
    await page.getByRole('button', { name: '模型', exact: true }).click();
    const vad = page.locator('[data-bundle-id="silero-vad"]');
    const download = vad.getByRole('button', { name: '下载', exact: true });
    await expect(download).toBeEnabled();
    await download.click();
    const install = path.join(modelRoot, 'models', 'silero-vad@2.0.0');
    await expect.poll(() => fs.existsSync(path.join(install, 'manifest.json')), { timeout: 60_000 }).toBe(true);
    expect(hash(fs.readFileSync(path.join(install, modelName)))).toBe(upstreamSHA);
  } finally {
    await stopServer(server);
    fs.rmSync(root, { recursive: true, force: true });
  }
});


test('模型镜像保存后不联网，重启保留，部署覆盖与下载生效', async ({ page }) => {
  test.setTimeout(70_000);
  const asset = process.env.E2E_REAL_VAD_FILE;
  const bytes = asset === undefined ? Buffer.from('mirror download transport fixture') : fs.readFileSync(asset);
  const digest = asset === undefined ? hash(bytes) : upstreamSHA;
  expect(hash(bytes)).toBe(digest);
  const catalogHits = { personal: 0, deployed: 0 };
  const rangeHits = { personal: 0, deployed: 0 };
  const mock = createServer((req, res) => {
    const kind = req.url?.startsWith('/personal/') ? 'personal' : req.url?.startsWith('/deployed/') ? 'deployed' : null;
    if (kind === null) { res.writeHead(404).end(); return; }
    if (req.url === `/${kind}/models.json`) {
      catalogHits[kind]++;
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ schemaVersion: 1, bundles: [{
        bundleId: `mirror-${kind}-vad`, name: `Verified ${kind} VAD`, version: '1.0.0', roles: ['vad'],
        files: [{ path: modelName, url: modelName, size: bytes.length, sha256: digest }],
      }] }));
      return;
    }
    const match = /^bytes=(\d+)-(\d+)$/.exec(req.headers.range ?? '');
    if (req.url !== `/${kind}/${modelName}` || match === null) { res.writeHead(404).end(); return; }
    const start = Number(match[1]);
    const end = Math.min(Number(match[2]), bytes.length - 1);
    if (start > end) { res.writeHead(416).end(); return; }
    rangeHits[kind]++;
    const chunk = bytes.subarray(start, end + 1);
    res.writeHead(206, { 'Content-Range': `bytes ${start}-${end}/${bytes.length}`, 'Content-Length': chunk.length, ETag: '"mirror-file"' });
    res.end(chunk);
  });
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-e2e-mirror-'));
  const dataDir = path.join(root, 'data');
  const modelRoot = path.join(root, 'models');
  const port = Number(process.env.E2E_CATALOG_PORT ?? 9371);
  let server: ServerHandle | null = null;
  const mirror = page.getByRole('textbox', { name: '模型镜像地址' });
  async function launch(catalog: string, mirrors: string, fresh: boolean) {
    server = startServer({ port, dataDir, fresh, env: {
      MANDARYN_MODEL_ROOT: modelRoot, MANDARYN_MODEL_CATALOG: catalog, MANDARYN_MODEL_MIRRORS: mirrors,
      MANDARYN_OFFLINE: '0', MANDARYN_HTTP_PROXY: '',
    } });
    await waitForServer(port, 20_000, server);
    await page.goto(`http://127.0.0.1:${port}`);
    await expect(page.locator('#startup-skeleton')).toHaveCount(0);
    await page.getByRole('button', { name: '模型', exact: true }).click();
  }
  try {
    await new Promise<void>((resolve, reject) => { mock.once('error', reject); mock.listen(0, '127.0.0.1', resolve); });
    const address = mock.address();
    if (address === null || typeof address === 'string') throw new Error('fixture did not bind a TCP port');
    const base = `http://127.0.0.1:${address.port}`;
    const personal = `${base}/personal/models.json`;
    await launch('', '', true);
    await expect(mirror).toBeEnabled();
    await mirror.fill('file:///invalid');
    await page.getByRole('button', { name: '保存模型镜像' }).click();
    await expect(page.getByText(/请输入不含凭据/)).toBeVisible();
    await mirror.fill(personal);
    await page.getByRole('button', { name: '保存模型镜像' }).click();
    await expect(page.getByText('已保存，刷新清单后可下载。')).toBeVisible();
    expect(catalogHits.personal).toBe(0);
    await page.getByRole('button', { name: '刷新模型清单' }).click();
    await expect(page.getByText('Verified personal VAD', { exact: true })).toBeVisible();
    expect(catalogHits.personal).toBeGreaterThan(0);
    await stopServer(server);

    // Deployment mirrors override the saved UI value on the same database.
    const beforeDeploy = catalogHits.personal;
    await launch('', `${base}/deployed`, false);
    await expect(mirror).toHaveValue(personal);
    await expect(mirror).toBeDisabled();
    await expect(page.getByText(/MANDARYN_MODEL_MIRRORS 已接管/)).toBeVisible();
    await expect(page.getByText('Verified deployed VAD', { exact: true })).toBeVisible();
    expect(catalogHits.personal).toBe(beforeDeploy);
    expect(catalogHits.deployed).toBeGreaterThan(0);
    await stopServer(server);

    // Removing the deployment override restores the still-saved personal mirror.
    await launch('', '', false);
    await expect(mirror).toBeEnabled();
    await expect(mirror).toHaveValue(personal);
    await expect(page.getByText('Verified personal VAD', { exact: true })).toBeVisible();
    await page.locator('[data-bundle-id="mirror-personal-vad"]').getByRole('button', { name: '下载', exact: true }).click();
    const installed = path.join(modelRoot, 'models', 'mirror-personal-vad@1.0.0');
    await expect.poll(() => fs.existsSync(path.join(installed, 'manifest.json')), { timeout: 25_000 }).toBe(true);
    expect(rangeHits.personal).toBeGreaterThan(0);
    expect(rangeHits.deployed).toBe(0);
    expect(hash(fs.readFileSync(path.join(installed, modelName)))).toBe(digest);
  } finally {
    await stopServer(server);
    await new Promise<void>((resolve) => mock.close(() => resolve()));
    fs.rmSync(root, { recursive: true, force: true });
  }
});


test('原生模型页：默认源下载并安装完整官方 SenseVoice 归档', async ({}, testInfo) => {
  const exe = process.env.E2E_NATIVE_MODEL_EXE;
  test.skip(process.platform !== 'win32' || !exe || process.env.E2E_OFFICIAL_MODEL_DOWNLOAD !== '1',
    '需要带 desktop_smoke 的 Windows 构建及显式官方资产网络验收');
  test.setTimeout(12 * 60_000);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-e2e-native-model-'));
  const modelRoot = path.join(root, 'models');
  const dataDir = path.join(root, 'data');
  const manifestPath = path.join(modelRoot, 'models', 'sensevoice-small@1.2.0', 'manifest.json');
  const assetBytes = 163002883;
  let browser: Browser | undefined;
  let server: ServerHandle | null = null;
  async function launch(port: number, offline: boolean) {
    const endpoint = `http://127.0.0.1:${port}`;
    await expect(fetch(`${endpoint}/json/version`, { signal: AbortSignal.timeout(800) })).rejects.toThrow();
    const logs: string[] = [];
    const child = spawn(path.resolve(exe!), [], { env: {
      ...process.env, AppData: path.join(root, 'appdata'), MANDARYN_DATA_DIR: dataDir,
      MANDARYN_MODEL_ROOT: modelRoot, MANDARYN_MODEL_CATALOG: '', MANDARYN_MODEL_MIRRORS: '',
      MANDARYN_HTTP_PROXY: '', MANDARYN_OFFLINE: offline ? '1' : '0', E2E_NATIVE_DESKTOP_PORT: String(port),
    }, stdio: ['ignore', 'pipe', 'pipe'] });
    child.stdout?.on('data', buf => logs.push(String(buf)));
    child.stderr?.on('data', buf => logs.push(String(buf)));
    server = { child, port, dataDir, logs };
    await expect.poll(async () => {
      try { return (await fetch(`${endpoint}/json/version`, { signal: AbortSignal.timeout(600) })).ok; }
      catch { return false; }
    }, { timeout: 20_000 }).toBe(true);
    browser = await chromium.connectOverCDP(endpoint);
    const context = browser.contexts()[0];
    const page = context.pages().find(p => !p.url().includes('systray.html')) ?? await context.waitForEvent('page');
    await expect(page.locator('#startup-skeleton')).toHaveCount(0, { timeout: 15_000 });
    await page.getByRole('button', { name: '模型', exact: true }).click();
    return page;
  }
  async function stop() {
    await browser?.close().catch(() => {});
    browser = undefined;
    await stopServer(server);
  }
  const readDb = () => new DatabaseSync(path.join(dataDir, 'mandaryn.db'), { readOnly: true });
  try {
    const port = Number(process.env.E2E_NATIVE_MODEL_PORT ?? 9395);
    const page = await launch(port, false);
    expect(fs.existsSync(manifestPath)).toBe(false);
    const download = page.locator('[data-bundle-id="sensevoice-small"]').getByRole('button', { name: '下载', exact: true });
    await expect(download).toBeEnabled();
    await download.click();
    await expect.poll(() => {
      if (fs.existsSync(manifestPath)) return true;
      const db = readDb();
      try {
        const row = db.prepare('SELECT state, last_error, bytes_done FROM downloads WHERE id = ?').get('sensevoice-small@1.2.0');
        if (row?.state === 'failed') throw new Error(`Official download failed: ${JSON.stringify(row)}`);
        return false;
      } finally { db.close(); }
    }, { timeout: 10 * 60_000, intervals: [300, 1000, 3000] }).toBe(true);
    const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8')) as {
      bundleId: string; version: string; sizeBytes: number; files: {path: string; sha256: string; size: number}[];
    };
    expect(manifest.bundleId).toBe('sensevoice-small');
    expect(manifest.version).toBe('1.2.0');
    expect(manifest.files.map(f => f.path)).toEqual(['model.int8.onnx', 'tokens.txt', 'LICENSE']);
    expect(manifest.sizeBytes).toBe(manifest.files.reduce((total, f) => total + f.size, 0));
    for (const file of manifest.files) {
      const filename = path.join(path.dirname(manifestPath), file.path);
      expect(fs.statSync(filename).size).toBe(file.size);
      const digest = createHash('sha256');
      for await (const chunk of fs.createReadStream(filename)) digest.update(chunk);
      expect(digest.digest('hex')).toBe(file.sha256);
    }
    const installedModel = (page: Page) => page.getByRole('button', { name: '校验', exact: true })
      .locator('xpath=ancestor::div[.//span[normalize-space()="SenseVoice Small（中英日韩粤）"]][1]');
    await expect(installedModel(page).getByText('已安装', { exact: true })).toBeVisible();
    const db = readDb();
    let verifiedBefore: number;
    try {
      const bundle = db.prepare('SELECT state,size_bytes,verified_at FROM bundles WHERE id=?').get('sensevoice-small@1.2.0');
      verifiedBefore = Number(bundle?.verified_at ?? 0);
      expect(bundle)
        .toMatchObject({ state: 'installed', size_bytes: manifest.sizeBytes });
      const audit = db.prepare("SELECT host, audio, SUM(bytes) AS bytes, COUNT(*) AS requests FROM outbound_log WHERE kind='model-download' AND result='ok' GROUP BY host,audio").all();
      expect(audit).toHaveLength(1);
      // Outbound entries are throttled to one per second, not a byte ledger.
      expect(audit[0]).toMatchObject({ host: 'github.com', audio: 0 });
      expect(Number(audit[0].bytes)).toBeGreaterThan(0);
      await expect.poll(() => server?.logs.join('') ?? '').toMatch(new RegExp(`model bundle installed[^\\n]*bundle=sensevoice-small[^\\n]*bytes=${assetBytes}(?:\\s|$)`));
      console.info(`SENSEVOICE_NATIVE_INSTALL ${JSON.stringify({ archiveBytes: assetBytes, audit, files: manifest.files })}`);
    } finally { db.close(); }
    expect(fs.readdirSync(path.dirname(manifestPath)).sort()).toEqual(['LICENSE', 'manifest.json', 'model.int8.onnx', 'tokens.txt']);
    await stop();
    const restarted = await launch(port + 1, true);
    await expect(restarted.getByText('SenseVoice Small（中英日韩粤）', { exact: true }).first()).toBeVisible();
    const verify = restarted.getByRole('button', { name: '校验', exact: true });
    await expect(verify).toHaveCount(1);
    await verify.click();
    await expect.poll(() => {
      const db = readDb();
      try { return Number(db.prepare('SELECT verified_at FROM bundles WHERE id=?').get('sensevoice-small@1.2.0')?.verified_at ?? 0); }
      finally { db.close(); }
    }, { timeout: 20_000 }).toBeGreaterThan(verifiedBefore);
    await expect(installedModel(restarted).getByText('已安装', { exact: true })).toBeVisible();
  } catch (error) {
    await testInfo.attach('native-model-log.txt', { body: server?.logs.join('') ?? '', contentType: 'text/plain' });
    throw error;
  } finally {
    await stop();
    try { await fs.promises.rm(root, { recursive: true, force: true, maxRetries: 7, retryDelay: 200 }); }
    catch (error) {
      if (!['EPERM', 'EBUSY'].includes((error as NodeJS.ErrnoException).code ?? '')) throw error;
      console.warn(`WebView2 still holds the isolated test profile: ${root}`);
    }
  }
});
