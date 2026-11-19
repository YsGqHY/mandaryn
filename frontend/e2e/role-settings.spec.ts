import { expect, test } from '@playwright/test';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { navTo, openAppAt } from './helpers';
import { startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

const port = Number(process.env.E2E_ROLE_SETTINGS_PORT ?? 9376);

test('角色设置：保存命名前缀和主题令牌，重启后仍保留', async ({ page }) => {
  test.setTimeout(45_000);
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-role-settings-'));
  const dataDir = path.join(temp, 'data');
  const modelRoot = path.join(temp, 'models');
  const dbFile = path.join(dataDir, 'mandaryn.db');
  let server: ServerHandle | null = null;
  const launch = async (fresh: boolean) => {
    server = startServer({ port, dataDir, fresh, env: { MANDARYN_MODEL_ROOT: modelRoot, MANDARYN_OFFLINE: '1' } });
    await waitForServer(port, 20_000, server);
  };
  const saveLocation = async () => {
    await openAppAt(page, `http://127.0.0.1:${port}`);
    await navTo(page, '设置');
  };
  try {
    await launch(true);
    const seed = new DatabaseSync(dbFile);
    try {
      seed.prepare('INSERT INTO roles (id,name,color_token,is_system,created_at,updated_at) VALUES (?,?,?,0,1,1)')
        .run('rl_e2e_color', '测试现有角色', 'role-1');
    } finally { seed.close(); }
    await saveLocation();
    const prefix = page.getByRole('textbox', { name: '默认角色命名前缀' });
    await expect(prefix).toHaveValue('说话人');
    await prefix.fill('主持人');
    await page.getByRole('button', { name: '保存角色前缀' }).click();
    await page.getByRole('combobox', { name: '已有角色' }).click();
    await page.getByRole('option', { name: '测试现有角色' }).click();
    await page.getByRole('combobox', { name: '角色配色' }).click();
    await page.getByRole('option', { name: '角色颜色 4', exact: true }).click();
    await expect.poll(() => {
      const db = new DatabaseSync(dbFile, { readOnly: true });
      try { return db.prepare('SELECT color_token FROM roles WHERE id = ?').get('rl_e2e_color')?.color_token; }
      finally { db.close(); }
    }).toBe('role-4');
    await stopServer(server);
    await launch(false);
    await saveLocation();
    await expect(prefix).toHaveValue('主持人');
    await page.getByRole('combobox', { name: '已有角色' }).click();
    await page.getByRole('option', { name: '测试现有角色' }).click();
    await expect(page.getByRole('combobox', { name: '角色配色' })).toHaveValue('角色颜色 4');
  } finally {
    await stopServer(server);
    fs.rmSync(temp, { recursive: true, force: true });
  }
});
