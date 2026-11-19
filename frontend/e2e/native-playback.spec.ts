import { chromium, expect, test, type Page } from '@playwright/test';
import { execFile, spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { DatabaseSync } from 'node:sqlite';
import {
  assertPlaybackMetric, measurePlaybackWindow, openPlaybackMetric, preparePlaybackWindow, seedPlaybackMetric,
  METRIC_AUDIO_MS, METRIC_SEGMENTS, METRIC_STRIDE_MS,
} from './playback-metric.fixture';
import { expectAudioPlayable, navTo } from './helpers';
import { killServer, type ServerHandle } from './serverctl';

const desktop = process.env.E2E_NATIVE_PLAYBACK_EXE;
const fixtureWav = process.env.E2E_REAL_SPEECH_WAV;
const fixtureText = process.env.E2E_REAL_ASR_REFERENCE;
const speechPlayer = process.env.E2E_NATIVE_SPEECH_PLAYER;
const full = process.env.E2E_NATIVE_PLAYBACK_FULL === '1';
const port = Number(process.env.E2E_NATIVE_PLAYBACK_PORT ?? 9391);
const id = 's_E2ENATIVEPLAYBACK000000';
const speechTitle = '原生另存为夹具';
const dialogDriver = fileURLToPath(new URL('./native-save-dialog.py', import.meta.url));
const run = promisify(execFile);
const hash = (file: string) => createHash('sha256').update(fs.readFileSync(file)).digest('hex');

async function assertFreePort(port: number) {
  await new Promise<void>((resolve, reject) => {
    const socket = net.createServer();
    socket.once('error', reject);
    socket.listen(port, '127.0.0.1', () => socket.close((err) => err ? reject(err) : resolve()));
  });
}

async function withDesktop(seed: (data: string) => void, use: (page: Page, pid: number, profile: string) => Promise<void>) {
  await assertFreePort(port);
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-native-playback-'));
  const dataDir = path.join(profile, 'data');
  const env = { ...process.env, AppData: path.join(profile, 'appdata'), MANDARYN_DATA_DIR: dataDir,
    MANDARYN_MODEL_ROOT: path.join(profile, 'models'), MANDARYN_OFFLINE: '1' };
  const handles: ServerHandle[] = [];
  const launch = async (p: number) => {
    const endpoint = `http://127.0.0.1:${p}`;
    const logs: string[] = [];
    const child = spawn(path.resolve(desktop!), [], { env: { ...env, E2E_NATIVE_DESKTOP_PORT: String(p) }, stdio: ['ignore', 'pipe', 'pipe'] });
    const handle = { child, logs, port: p, dataDir };
    handles.push(handle);
    child.stdout!.on('data', buf => logs.push(String(buf)));
    child.stderr!.on('data', buf => logs.push(String(buf)));
    await expect.poll(async () => {
      if (child.exitCode !== null) throw new Error(`Native app exited: ${logs.slice(-8).join('')}`);
      try {
        const response = await fetch(`${endpoint}/json/version`, { signal: AbortSignal.timeout(500) });
        if (!response.ok) return false;
        const result = await response.json();
        return typeof result.webSocketDebuggerUrl === 'string';
      } catch { return false; }
    }, { timeout: 20_000 }).toBe(true);
    const browser = await chromium.connectOverCDP(endpoint);
    const context = browser.contexts()[0];
    const page = context.pages().find(p => !p.url().includes('systray.html')) ?? await context.waitForEvent('page');
    await expect(page.locator('#startup-skeleton')).toHaveCount(0, { timeout: 15_000 });
    return { browser, page, handle };
  };
  try {
    // Seed only after the real app has initialized the SQLite schema. The next
    // startup sees the fixture exactly as it would see an existing session.
    await assertFreePort(port + 1);
    const init = await launch(port + 1);
    await init.browser.close();
    await killServer(init.handle);
    seed(dataDir);
    const { browser, page, handle } = await launch(port);
    try { await use(page, handle.child.pid!, profile); }
    finally { await browser.close().catch(() => {}); }
  } finally {
    for (const handle of handles) await killServer(handle);
    try { await fs.promises.rm(profile, { recursive: true, force: true, maxRetries: 7, retryDelay: 200 }); }
    catch (error) {
      if (!['EPERM', 'EBUSY'].includes((error as NodeJS.ErrnoException).code ?? '')) throw error;
      console.warn(`WebView2 still holds isolated test profile: ${profile}`);
    }
  }
}

function seedSpeech(data: string) {
  const relative = `sessions/${id}/audio.wav`;
  fs.mkdirSync(path.dirname(path.join(data, relative)), { recursive: true });
  fs.copyFileSync(fixtureWav!, path.join(data, relative));
  const lines = [...fs.readFileSync(fixtureText!, 'utf8').split(/^---/m)[0].matchAll(/^(角色[A-Z])\s*[：:]\s*(.+)$/gm)]
    .map(m => ({ role: m[1], text: m[2].trim() }));
  expect(lines.length).toBeGreaterThan(0);
  const db = new DatabaseSync(path.join(data, 'mandaryn.db'));
  try {
    const now = Date.now();
    db.prepare(`INSERT INTO sessions (id,title,source_id,model_source,status,started_at,ended_at,duration_ms,audio_path,audio_format,segment_count,transcription_status,created_at,updated_at)
      VALUES (?,?,'dev:fixture','local','completed',?,?,17120,?,'wav',?,'done',?,?)`).run(id, speechTitle, now - 17120, now, relative, lines.length, now, now);
    const roles = new Map<string, string>();
    for (const { role } of lines) {
      if (roles.has(role)) continue;
      const speaker = `sp_native_${roles.size}`;
      const roleID = `rl_native_${roles.size}`;
      db.prepare('INSERT INTO speakers (id,first_seen_at,last_seen_at) VALUES (?,?,?)').run(speaker, now, now);
      db.prepare('INSERT INTO roles (id,name,color_token) VALUES (?,?,?)').run(roleID, role, 'role-1');
      db.prepare('INSERT INTO session_speakers (session_id,speaker_id,role_id) VALUES (?,?,?)').run(id, speaker, roleID);
      roles.set(role, speaker);
    }
    const insert = db.prepare('INSERT INTO segments (session_id,seq,start_ms,end_ms,text,speaker_id,is_final) VALUES (?,?,?,?,?,?,1)');
    lines.forEach((line, i) => insert.run(id, i, i * 2000, (i + 1) * 2000, line.text, roles.get(line.role)!));
  } finally { db.close(); }
  return lines;
}

const normalize = (value: string) => value.normalize('NFKC').toLowerCase().replace(/[^\p{L}\p{N}]/gu, '');

// This spec is opt-in because it requires an interactive Windows desktop and
// opens real shell dialogs. Only the isolated child PID is driven by Win32.
test.skip(process.platform !== 'win32' || desktop === undefined, 'Requires Windows and E2E_NATIVE_PLAYBACK_EXE');

test('原生 WebView2 回放量化采用同一套 80ms 判据', async () => {
  test.setTimeout(full ? METRIC_AUDIO_MS + 180_000 : 125_000);
  await withDesktop(seedPlaybackMetric, async page => {
    await openPlaybackMetric(page);
    const starts = full ? [0] : [198, 2398, 4990].map(seq => seq * METRIC_STRIDE_MS);
    for (const start of starts) {
      await preparePlaybackWindow(page, start);
      const metric = await measurePlaybackWindow(page, full ? METRIC_AUDIO_MS : start + 6000);
      await test.info().attach(`webview2-${start}.json`, { body: JSON.stringify(metric, null, 2), contentType: 'application/json' });
      console.info(`NATIVE_METRIC ${JSON.stringify({ start, max: metric.maxErrorMs, p95: metric.p95ErrorMs, count: metric.boundaries.length, missing: metric.missing.length, wall: metric.wallMs })}`);
      assertPlaybackMetric(metric);
      if (full) {
        expect(metric.boundaries).toHaveLength(METRIC_SEGMENTS - 1);
        expect(metric.wallMs).toBeGreaterThanOrEqual(METRIC_AUDIO_MS - 1000);
      }
    }
    if (!full) {
      await preparePlaybackWindow(page, starts[0]);
      const negative = await measurePlaybackWindow(page, starts[0] + 6000, 160);
      expect(negative.maxErrorMs).toBeGreaterThanOrEqual(120);
      expect(() => assertPlaybackMetric(negative)).toThrow();
    }
  });
});

test('原生另存为：从 14761 会话导出参考文字并验证取消', async () => {
  test.skip(!fixtureWav || !fixtureText, 'Requires the approved WAV and its reference text');
  test.setTimeout(120_000);
  let reference: ReturnType<typeof seedSpeech> = [];
  const before = hash(fixtureWav!);
  await withDesktop(data => { reference = seedSpeech(data); }, async (page, pid, profile) => {
    await navTo(page, '会话库');
    const row = page.locator(`[data-session-id="${id}"]`);
    await row.getByRole('button', { name: '回放', exact: true }).click();
    expect(await expectAudioPlayable(page, 0.03)).toBeGreaterThan(17);
    await page.keyboard.press('Escape');
    const choose = async (option: string) => {
      await row.getByRole('button', { name: '导出文本', exact: true }).click();
      await page.getByRole('menuitem', { name: option, exact: true }).click();
    };
    const native = async (title: string, action: string, filename = '', currentName = '') => {
      await run('python', [dialogDriver, String(pid), title, action, filename, currentName], { timeout: 20_000, windowsHide: true });
    };
    const transcript = path.join(profile, '验收导出.txt');
    await choose('导出为纯文本 (.txt)');
    await native('导出会话文本', 'cancel', '', `${speechTitle}.txt`);
    expect(fs.existsSync(transcript)).toBe(false);
    await choose('导出为纯文本 (.txt)');
    await native('导出会话文本', 'save', transcript, `${speechTitle}.txt`);
    await native('导出完成', 'ok');
    const actual = normalize(fs.readFileSync(transcript, 'utf8'));
    for (const { role, text } of reference) expect(actual).toContain(normalize(role + text));
    const subtitle = path.join(profile, '验收导出.srt');
    await choose('导出为字幕 (.srt)');
    await native('导出会话文本', 'save', subtitle, `${speechTitle}.srt`);
    await native('导出完成', 'ok');
    const srt = fs.readFileSync(subtitle, 'utf8');
    expect(srt).toMatch(/00:00:00,000 --> 00:00:02,000/);
    for (const { role, text } of reference) expect(normalize(srt)).toContain(normalize(role + text));
    for (const [extension, label] of [['csv', '导出为 CSV (.csv)'], ['xlsx', '导出为 Excel (.xlsx)']]) {
      const output = path.join(profile, `验收导出.${extension}`);
      await choose(label);
      await native('导出会话文本', 'save', output, `${speechTitle}.${extension}`);
      await native('导出完成', 'ok');
      const script = `import csv,json,sys; p=sys.argv[1];\nif p.endswith('.csv'):\n with open(p,encoding='utf-8-sig',newline='') as f: rows=list(csv.reader(f))\nelse:\n from openpyxl import load_workbook\n w=load_workbook(p,read_only=True,data_only=True); rows=list(w['会话记录'].values); w.close()\nprint(json.dumps(rows))`;
      const { stdout } = await run('python', ['-c', script, output], { timeout: 10_000, windowsHide: true });
      const [headers, ...rows] = JSON.parse(stdout) as (string | number)[][];
      const speaker = headers.indexOf('说话人');
      const time = headers.indexOf('起始毫秒');
      expect(speaker, `${extension} headers: ${JSON.stringify(headers)}`).toBeGreaterThanOrEqual(0);
      expect(time, `${extension} headers: ${JSON.stringify(headers)}`).toBeGreaterThanOrEqual(0);
      expect(rows).toHaveLength(reference.length);
      reference.forEach(({ role, text }, i) => {
        expect(rows[i][speaker]).toBe(role);
        expect(Number(rows[i][time])).toBe(i * 2000);
        expect(rows[i]).toContain(text);
      });
    }
    expect(hash(path.join(profile, 'data', 'sessions', id, 'audio.wav'))).toBe(before);
    expect(hash(fixtureWav!)).toBe(before);
  });
});


function attenuatedWav(source: string): Buffer {
  const wav = fs.readFileSync(source);
  if (wav.toString('ascii', 0, 4) !== 'RIFF' || wav.toString('ascii', 8, 12) !== 'WAVE') throw new Error('expected a PCM WAV fixture');
  let pcm16 = false;
  for (let offset = 12; offset + 8 <= wav.length;) {
    const size = wav.readUInt32LE(offset + 4);
    const start = offset + 8;
    if (size > wav.length - start) throw new Error('truncated WAV fixture');
    const kind = wav.toString('ascii', offset, offset + 4);
    if (kind === 'fmt ' && size >= 16) pcm16 = wav.readUInt16LE(start) === 1 && wav.readUInt16LE(start + 14) === 16;
    if (kind === 'data' && pcm16) {
      for (let i = start; i + 1 < start + size; i += 2) wav.writeInt16LE(Math.round(wav.readInt16LE(i) * 0.03), i);
      return wav;
    }
    offset = start + size + (size % 2);
  }
  throw new Error('expected 16-bit PCM samples in WAV fixture');
}

test('原生录音另存为：专用播放进程录制后保留 WAV 和 FLAC 原格式', async () => {
  test.skip(!fixtureWav || !speechPlayer, 'Requires E2E_REAL_SPEECH_WAV and the uniquely named E2E_NATIVE_SPEECH_PLAYER');
  test.setTimeout(120_000);
  const original = hash(fixtureWav!);
  await withDesktop(() => {}, async (page, pid, profile) => {
    const voice = path.join(profile, 'quiet-source.wav');
    fs.writeFileSync(voice, attenuatedWav(fixtureWav!), { flag: 'wx' });
    const exe = path.basename(speechPlayer!).toLowerCase();
    expect(exe).toBe('mandaryn-native-speech-player.exe');
    const player = spawn(path.resolve(speechPlayer!), [voice], { windowsHide: true, stdio: 'ignore' });
    const playerHandle = { child: player, dataDir: profile, port: 0, logs: [] };
    try {
      for (const format of ['wav', 'flac']) {
        if (format === 'flac') {
          const db = new DatabaseSync(path.join(profile, 'data', 'mandaryn.db'));
          try { db.prepare('UPDATE app_settings SET save_format = ? WHERE id = 1').run('flac'); }
          finally { db.close(); }
        }
        await navTo(page, '录制');
        const source = page.locator('[aria-label^="选择音频源"]').filter({ has: page.getByText(exe, { exact: true }) });
        await expect(source).toHaveCount(1, { timeout: 12_000 });
        await source.click();
        await page.getByRole('button', { name: '开始录制', exact: true }).click();
        const stop = page.getByRole('button', { name: '停止', exact: true });
        await expect(stop).toBeEnabled();
        await page.waitForTimeout(3500); // capture a bounded sample of live speech from the test player
        await stop.click();
        await expect(page.locator('main')).toContainText('已完成');
        const db = new DatabaseSync(path.join(profile, 'data', 'mandaryn.db'));
        const readSession = () => db.prepare('SELECT * FROM sessions ORDER BY started_at DESC LIMIT 1').get()!;
        try {
          const before = readSession();
          expect(before.source_id).toBe(`app:${exe}`);
          expect(before.audio_format).toBe(format);
          const sourcePath = path.join(profile, 'data', String(before.audio_path));
          const copied = path.join(profile, `录音副本.${format}`);
          const audioHash = hash(sourcePath);
          await page.getByRole('button', { name: '另存录音文件', exact: true }).click();
          await run('python', [dialogDriver, String(pid), '另存录音文件', 'save', copied, `recording.${format}`], { timeout: 20_000, windowsHide: true });
          await run('python', [dialogDriver, String(pid), '已另存录音文件', 'ok', '', ''], { timeout: 20_000, windowsHide: true });
          expect(fs.readFileSync(copied).toString('ascii', 0, 4)).toBe(format === 'flac' ? 'fLaC' : 'RIFF');
          expect(hash(copied)).toBe(audioHash);
          expect(hash(sourcePath)).toBe(audioHash);
          expect(readSession()).toEqual(before);
          await navTo(page, '会话库');
          await page.locator(`[data-session-id="${before.id}"]`).getByRole('button', { name: '回放', exact: true }).click();
          expect(await expectAudioPlayable(page, 0.03)).toBeGreaterThan(3);
          await page.keyboard.press('Escape');
        } finally { db.close(); }
      }
    } finally { await killServer(playerHandle); }
  });
  expect(hash(fixtureWav!)).toBe(original);
});
