import { expect, test, type Locator, type Page } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { navTo, openAppAt } from './helpers';
import { REPO_ROOT, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

// 调试模式（设置 → 隐私边界）：
//   1) 默认关闭，数据目录下没有 debug.log；
//   2) 开启后后端日志与前端 console.error 都写进 <dataRoot>/logs/debug.log（JSON Lines），查看器可读、可筛选；
//   3) 开关持久化在 app_settings.log_level，刷新页面仍为开；
//   4) 关闭后立即停写（文件不再增长），已有日志仍可在查看器里读到；
//   5) 环境变量 MANDARYN_LOG_LEVEL=debug 接管：开关禁用并说明，启动阶段就开始记录。
//
// 自管 server（isolated project）：需要干净数据目录，并要用不同环境变量再起一个实例。
const PORT = Number(process.env.E2E_DEBUG_PORT ?? 9364);
const ENV_PORT = Number(process.env.E2E_DEBUG_ENV_PORT ?? 9366);
const DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'debug');
const ENV_DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'debug-env');
const BASE = `http://127.0.0.1:${PORT}`;
const SWITCH_LABEL = '开启调试模式（把调试日志写入数据目录）';

const debugLogPath = (dir: string): string => path.join(dir, 'logs', 'debug.log');
const readDebugLog = (dir: string): string =>
  fs.existsSync(debugLogPath(dir)) ? fs.readFileSync(debugLogPath(dir), 'utf8') : '';
const logLevelInDb = (dir: string): string => {
  const db = new DatabaseSync(path.join(dir, 'mandaryn.db'));
  try {
    const row = db.prepare('SELECT log_level FROM app_settings WHERE id = 1').get() as { log_level: string };
    return row.log_level;
  } finally {
    db.close();
  }
};

test.describe.configure({ mode: 'serial' });

let server: ServerHandle | null = null;

test.beforeAll(async () => {
  server = startServer({ port: PORT, dataDir: DATA_DIR });
  await waitForServer(PORT, 20_000, server);
});

test.afterAll(async () => {
  await stopServer(server);
  server = null;
});

const openDebugCard = async (page: Page, base = BASE): Promise<Locator> => {
  await openAppAt(page, base);
  await navTo(page, '设置');
  await page.getByRole('button', { name: /^隐私边界/ }).first().click();
  const card = page.getByTestId('debug-mode-card');
  await expect(card).toBeVisible();
  return card;
};

const debugSwitch = (card: Locator): Locator => card.getByRole('switch', { name: SWITCH_LABEL });

// 制造一些后端活动：切到录制页（枚举音频源、读引擎状态）再回设置页。
const backendActivity = async (page: Page): Promise<void> => {
  await navTo(page, '录制');
  await page.waitForTimeout(800);
  await navTo(page, '设置');
  await page.getByRole('button', { name: /^隐私边界/ }).first().click();
};

test('默认关闭：开关为关，数据目录下没有调试日志', async ({ page }) => {
  const card = await openDebugCard(page);
  await expect(debugSwitch(card)).not.toBeChecked();
  await expect(card).toContainText('已关闭');
  await expect(card.getByTestId('debug-log-empty')).toContainText('调试模式未开启');
  // 存储位置与保留策略都明确写在界面上
  await expect(card.getByTestId('debug-log-dir')).toContainText(path.join('debug', 'logs'));
  await expect(card).toContainText('最多保留 5 个文件');
  expect(fs.existsSync(debugLogPath(DATA_DIR))).toBe(false);
  expect(logLevelInDb(DATA_DIR)).toBe('info');
});

test('开启后写入后端与前端日志：JSON Lines、带必需字段、敏感值已脱敏、查看器可读', async ({ page }) => {
  const card = await openDebugCard(page);
  await debugSwitch(card).click();
  await expect(debugSwitch(card)).toBeChecked();
  await expect(card).toContainText('记录中');
  expect(logLevelInDb(DATA_DIR)).toBe('debug');

  await backendActivity(page);

  // 前端上报器靠 appsettings:changed 刷新开关状态，存在毫秒级延迟：每轮用新文案重试，直到落盘。
  let n = 0;
  await expect
    .poll(
      async () => {
        n += 1;
        await page.evaluate((i) => console.error(`e2e-debug-probe-${i} token=sk-abcdefghijklmnop`), n);
        await page.waitForTimeout(300);
        return readDebugLog(DATA_DIR).includes('e2e-debug-probe');
      },
      { timeout: 15_000 },
    )
    .toBe(true);

  const raw = readDebugLog(DATA_DIR);
  expect(raw).not.toContain('sk-abcdefghijklmnop');
  const records = raw
    .split('\n')
    .filter((line) => line.trim() !== '')
    .map((line) => JSON.parse(line) as Record<string, unknown>);
  for (const r of records) {
    for (const key of ['time', 'level', 'msg', 'component', 'origin']) {
      expect(r, `缺少 ${key}：${JSON.stringify(r)}`).toHaveProperty(key);
    }
  }
  const backend = records.filter((r) => r.origin === 'backend');
  expect(backend.length).toBeGreaterThan(0);
  expect(backend.some((r) => r.level === 'DEBUG')).toBe(true);
  expect(String(backend[0]?.source ?? '')).toMatch(/^internal\/.+\.go:\d+$/);
  const probe = records.find((r) => r.origin === 'frontend' && String(r.message ?? '').includes('e2e-debug-probe'));
  expect(probe?.level).toBe('ERROR');
  expect(probe?.route).toBe('settings');

  // 查看器：刷新后能看到探针；按「前端」筛选只剩前端记录。
  await card.getByRole('button', { name: '刷新' }).click();
  const list = card.getByTestId('debug-log-list');
  await expect(list).toContainText('e2e-debug-probe');
  await card.getByText('前端', { exact: true }).click();
  await expect(list).toContainText('e2e-debug-probe');
  await expect(list.locator('[data-origin="backend"]')).toHaveCount(0);
});

test('开关持久化：刷新页面后仍为开', async ({ page }) => {
  const card = await openDebugCard(page);
  await expect(debugSwitch(card)).toBeChecked();
  expect(logLevelInDb(DATA_DIR)).toBe('debug');
});

test('关闭后停止写入新日志，已有日志仍可查看', async ({ page }) => {
  const card = await openDebugCard(page);
  await debugSwitch(card).click();
  await expect(debugSwitch(card)).not.toBeChecked();
  await expect(card).toContainText('已关闭');
  expect(logLevelInDb(DATA_DIR)).toBe('info');

  const sizeAfterOff = fs.statSync(debugLogPath(DATA_DIR)).size;
  await backendActivity(page);
  await page.evaluate(() => console.error('e2e-after-off'));
  // 后端 perf 采样每 0.5 秒一条 DEBUG：开着的话 2 秒内文件必然增长。
  await page.waitForTimeout(2_000);
  expect(fs.statSync(debugLogPath(DATA_DIR)).size).toBe(sizeAfterOff);
  expect(readDebugLog(DATA_DIR)).not.toContain('e2e-after-off');

  const reopened = page.getByTestId('debug-mode-card');
  await reopened.getByRole('button', { name: '刷新' }).click();
  await expect(reopened.getByTestId('debug-log-list')).toContainText('e2e-debug-probe');
});

test('环境变量 MANDARYN_LOG_LEVEL=debug 接管：开关禁用并说明，启动阶段即开始记录', async ({ page }) => {
  const envServer = startServer({ port: ENV_PORT, dataDir: ENV_DATA_DIR, env: { MANDARYN_LOG_LEVEL: 'debug' } });
  try {
    await waitForServer(ENV_PORT, 20_000, envServer);
    const card = await openDebugCard(page, `http://127.0.0.1:${ENV_PORT}`);
    const sw = debugSwitch(card);
    await expect(sw).toBeChecked();
    await expect(sw).toBeDisabled();
    await expect(card).toContainText('已由环境变量 MANDARYN_LOG_LEVEL 接管');
    // 用户层没被改写：库里仍是默认的 info
    expect(logLevelInDb(ENV_DATA_DIR)).toBe('info');
    // 启动早期的记录也在：数据库刚打开时的这一条，用户层开关（要先读库）是记不到的
    expect(readDebugLog(ENV_DATA_DIR)).toContain('"msg":"storage opened"');
  } finally {
    await stopServer(envServer);
  }
});
