// Opt-in Windows desktop acceptance. Run from the repository root with the path
// to a native executable built with -tags production,desktop_smoke (never ship that build).
import { spawn, execFileSync } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { chromium, expect } from '@playwright/test';

if (process.platform !== 'win32' || !process.argv[2]) throw new Error('Provide a native Windows test executable');
const executable = path.resolve(process.argv[2]);
const port = Number(process.env.E2E_NATIVE_DESKTOP_PORT ?? 9384);
if (!Number.isInteger(port) || port < 1024 || port >= 65535) throw new Error('Choose a test port between 1024 and 65534');
let launchCount = 0;
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-native-settings-'));
let child, browser;
const debugLog = path.join(root, 'data', 'logs', 'debug.log');
function playbackCount(includeFailed = false) {
  if (!fs.existsSync(debugLog)) return 0;
  return fs.readFileSync(debugLog, 'utf8').split('\n')
    .filter((line) => {
      try {
        const row = JSON.parse(line);
        return row.msg === 'recording cue playback' && (includeFailed || row.success === true);
      } catch { return false; }
    }).length;
}

async function finishRecording(page, withTone) {
  await page.getByRole('button', { name: '录制', exact: true }).click();
  const device = page.locator('[aria-label^="选择音频源"]').filter({ hasText: '默认设备' }).first();
  await device.click();
  await page.getByRole('button', { name: '开始录制' }).click();
  const stop = page.getByRole('button', { name: '停止', exact: true });
  await expect(stop).toBeEnabled();
  if (withTone) {
    await page.evaluate(async () => {
      const context = new AudioContext();
      await context.resume();
      const tone = context.createOscillator();
      const gain = context.createGain();
      gain.gain.value = 0.03;
      tone.connect(gain).connect(context.destination);
      tone.start();
      await new Promise((resolve) => setTimeout(resolve, 2300));
      tone.stop();
      await context.close();
    });
  }
  await stop.click();
  await expect(page.locator('main')).toContainText(withTone ? '已完成' : /已完成|失败/);
  await page.getByRole('button', { name: '设置', exact: true }).click();
}

// Exercise the native Wails tray callback on a message-only window owned by our
// isolated app process, without moving the user's cursor or touching other apps.
const win32 = String.raw`
import ctypes as c, sys, json
from ctypes import wintypes as w
pid, action = int(sys.argv[1]), sys.argv[2]
u = c.WinDLL('user32', use_last_error=True)
u.GetWindowThreadProcessId.argtypes = (w.HWND, c.POINTER(w.DWORD))
u.FindWindowExW.argtypes = (w.HWND, w.HWND, w.LPCWSTR, w.LPCWSTR)
u.FindWindowExW.restype = w.HWND
u.IsWindowVisible.argtypes = (w.HWND,)
u.IsIconic.argtypes = (w.HWND,)
u.PostMessageW.argtypes = (w.HWND,w.UINT,w.WPARAM,w.LPARAM)
def owned(hwnd):
    p = w.DWORD()
    u.GetWindowThreadProcessId(hwnd, c.byref(p))
    return p.value == pid
if action == 'click':
    parent = w.HWND(-3)
    hwnd = None
    while True:
        hwnd = u.FindWindowExW(parent, hwnd, 'WailsWebviewWindow', None)
        if not hwnd: raise RuntimeError('No owned native tray callback window')
        if owned(hwnd): break
    if not u.PostMessageW(hwnd, 0x0401, 0, 0x0202): raise c.WinError(c.get_last_error())
else:
    result = []
    CB = c.WINFUNCTYPE(w.BOOL,w.HWND,w.LPARAM)
    @CB
    def visit(hwnd, _):
        if owned(hwnd) and u.IsWindowVisible(hwnd): result.append([hwnd, bool(u.IsIconic(hwnd))])
        return True
    u.EnumWindows.argtypes = (CB,w.LPARAM)
    u.EnumWindows(visit, 0)
    print(json.dumps(result))
`;
const native = (action) => execFileSync('python', ['-c', win32, `${child.pid}`, action], { encoding: 'utf8', timeout: 5000, windowsHide: true });
const windows = () => JSON.parse(native('windows'));
async function launch() {
  const testPort = port + launchCount++;
  const endpoint = `http://127.0.0.1:${testPort}`;
  child = spawn(executable, [], { env: { ...process.env,
    MANDARYN_DATA_DIR: path.join(root, 'data'), MANDARYN_MODEL_ROOT: path.join(root, 'models'), MANDARYN_OFFLINE: '1',
    E2E_NATIVE_DESKTOP_PORT: String(testPort), MANDARYN_LOG_LEVEL: 'debug',
  }, stdio: ['ignore', 'pipe', 'pipe'] });
  child.stdout.resume();
  child.stderr.resume();
  await expect.poll(async () => {
    try { return (await fetch(`${endpoint}/json/version`, { signal: AbortSignal.timeout(600) })).ok; }
    catch { return false; }
  }, { timeout: 20_000 }).toBe(true);
  browser = await chromium.connectOverCDP(endpoint);
  const context = browser.contexts()[0];
  const page = context.pages().find((p) => !p.url().includes('systray.html')) ?? await context.waitForEvent('page');
  await page.getByRole('button', { name: '设置', exact: true }).click({ timeout: 15_000 });
  return page;
}
async function stop() {
  if (browser) { await browser.close().catch(() => {}); browser = undefined; }
  if (child && child.exitCode === null && child.signalCode === null) {
    const exited = once(child, 'exit');
    child.kill();
    let timer;
    try { await Promise.race([exited, new Promise((resolve) => { timer = setTimeout(resolve, 5000); })]); }
    finally { clearTimeout(timer); }
  }
}
try {
  let page = await launch();
  const tray = () => page.getByRole('switch', { name: '启用系统托盘', exact: true });
  const action = () => page.getByRole('combobox', { name: '托盘左键行为', exact: true });
  const cues = () => page.getByRole('switch', { name: '录制提示音', exact: true });
  await expect(action()).toHaveValue('显示托盘面板');
  await expect(cues()).toBeChecked();
  await expect(tray()).not.toBeChecked();
  await tray().click();
  await expect(tray()).toBeChecked();
  const main = windows().find(([, iconic]) => !iconic)?.[0];
  if (!main) throw new Error('No visible test main window');
  native('click');
  await expect.poll(() => windows().some(([handle, iconic]) => handle !== main && !iconic)).toBe(true);
  console.log('Native tray callback opens the attached panel');
  await page.getByRole('combobox', { name: '托盘左键行为' }).click();
  await page.getByRole('option', { name: '显示主窗口', exact: true }).click();
  await expect(action()).toHaveValue('显示主窗口');
  await finishRecording(page, true);
  await expect.poll(playbackCount).toBe(1);
  await cues().click();
  await expect(cues()).not.toBeChecked();
  await page.getByRole('button', { name: '最小化', exact: true }).click();
  await expect.poll(() => windows().find(([handle]) => handle === main)?.[1]).toBe(true);
  native('click');
  await expect.poll(() => windows().find(([handle]) => handle === main)?.[1]).toBe(false);
  console.log('Native tray callback restores the minimized main window');
  await stop();
  page = await launch();
  await expect(action()).toHaveValue('显示主窗口');
  await expect(cues()).not.toBeChecked();
  const before = playbackCount(true);
  await finishRecording(page, true);
  // The native call is synchronous on the cue worker and clips last <150 ms.
  await new Promise((resolve) => setTimeout(resolve, 650));
  expect(playbackCount(true)).toBe(before);
  console.log('Native settings persist after restart; disabling cues suppresses playback');
} finally {
  await stop();
  try { fs.rmSync(root, { recursive: true, force: true, maxRetries: 4, retryDelay: 200 }); }
  catch { console.warn(`Test profile retained because WebView2 still holds it: ${root}`); }
}
