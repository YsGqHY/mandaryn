import { expect, test, type Page } from '@playwright/test';
import path from 'node:path';
import { navTo, openAppAt } from './helpers';
import { REPO_ROOT, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

const PORT = Number(process.env.E2E_LEAN_PORT ?? 9374);
const DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'lean-settings');
const BASE = `http://127.0.0.1:${PORT}`;

const section = (page: Page, label: string) => page.getByRole('button', { name: new RegExp(`^${label}`) });
const modeSwitch = (page: Page) => page.getByRole('switch', { name: '开启精简模式' });
const offlineSwitch = (page: Page) => page.getByRole('switch', { name: /^强制离线/ });
const budgetSlider = (page: Page) => page.getByRole('slider', { name: '整机性能预算' });
const openSettings = async (page: Page) => {
  await openAppAt(page, BASE);
  await navTo(page, '设置');
};

test('精简模式可切换、同步到正在查看的设置分类，并在重启后恢复', async ({ page, context }) => {
  test.setTimeout(60_000);
  let server: ServerHandle | null = null;
  try {
    server = startServer({ port: PORT, dataDir: DATA_DIR });
    await waitForServer(PORT, 20_000, server);
    await openSettings(page);
    await section(page, '快捷键').click();
    await expect(modeSwitch(page)).toBeEnabled();
    await expect(modeSwitch(page)).not.toBeChecked();

    // 第二页保持在当前分类，不依赖重新导航来接收变更。
    const other = await context.newPage();
    await openSettings(other);
    await section(other, '隐私边界').click();
    await expect(other.getByTestId('debug-mode-card')).toBeVisible();
    await expect(other.getByText('导出诊断包', { exact: true })).toBeVisible();

    await modeSwitch(page).click();
    await expect(modeSwitch(page)).toBeChecked();
    await expect(other.getByTestId('debug-mode-card')).toHaveCount(0);
    await expect(other.getByText('导出诊断包', { exact: true })).toBeHidden();
    await expect(section(other, '隐私边界')).toHaveAttribute('aria-pressed', 'true');
    await expect(offlineSwitch(other)).toBeVisible();
    await expect(other.getByText('出站记录', { exact: true })).toBeVisible();

    await section(other, '网络代理').click();
    await expect(other.getByText('代理配置', { exact: true })).toBeVisible();
    await section(other, '数据存储').click();
    await expect(other.getByRole('button', { name: '更改位置…' })).toBeVisible();
    await expect(other.getByText('字节用量', { exact: true })).toHaveCount(0);
    await section(other, '性能预算').click();
    await expect(budgetSlider(other)).toBeVisible();
    await expect(other.getByText('实时读数', { exact: true })).toHaveCount(0);
    await other.close();

    // 重启真实后端，并打开一个全新的前端会话；不沿用旧内存状态。
    await stopServer(server);
    server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false });
    await waitForServer(PORT, 20_000, server);
    await openSettings(page);
    await section(page, '快捷键').click();
    await expect(modeSwitch(page)).toBeChecked();
    await section(page, '隐私边界').click();
    await expect(offlineSwitch(page)).toBeVisible();
    await expect(page.getByTestId('debug-mode-card')).toHaveCount(0);
    await section(page, '快捷键').click();
    await modeSwitch(page).click();
    await expect(modeSwitch(page)).not.toBeChecked();
    await section(page, '隐私边界').click();
    await expect(page.getByTestId('debug-mode-card')).toBeVisible();
    await expect(page.getByText('导出诊断包', { exact: true })).toBeVisible();
    await section(page, '数据存储').click();
    await expect(page.getByText('字节用量', { exact: true })).toBeVisible();
    await section(page, '性能预算').click();
    await expect(budgetSlider(page)).toBeVisible();
    await expect(page.getByText('实时读数', { exact: true })).toBeVisible();
  } finally {
    await stopServer(server);
  }
});
