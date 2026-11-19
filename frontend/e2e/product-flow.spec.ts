import { expect, test } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import {
  expectAudioPlayable,
  navTo,
  openAppAt,
  pickDefaultDevice,
  startRecording,
  stopRecording,
  TEST_TONE_LAUNCH,
} from './helpers';
import { REPO_ROOT, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';
import { matchingToneWindows, startWindowsRecordingPlayer } from './windows-recording-player';

// 产品主流程里「能真跑通」的那一半 + 转写失败路径。
//
// 为什么成功转写不在本文件：转写需要已安装的本地模型包或在线端点，而 E2E 的数据目录
// 是隔离且全新的（没有任何模型）。因此这里断言的是**真实的失败路径**：
// 无模型时点「发起转写」必须给出可读错误，且**不得**损伤音频与元数据（FR-03-6）。
//
// 录制期间页面自己播放小音量测试音（helpers.playTestTone）：全程静音的会话会被空录音策略
// 判为 failed（FR-03-7），用例不能依赖「本机当时恰好在放音乐」。
//
// 自管 server：本文件需要「干净数据目录 + 自己掌控生命周期」，不复用 webServer。
test.use({ launchOptions: TEST_TONE_LAUNCH });
const PORT = Number(process.env.E2E_PRODUCT_PORT ?? 9357);
// 数据目录刻意放在 .e2e-data/ 之下：该目录整体已被 .gitignore 忽略，
// 用例会录进 3–5 秒真实设备回环音频，不能留在仓库里当未跟踪文件。
const DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'product');
const BASE = `http://127.0.0.1:${PORT}`;

// 两个用例共享同一份录制结果（先录后转写），串行以免互相覆盖。
test.describe.configure({ mode: 'serial' });

let server: ServerHandle | null = null;
let audioFile = '';

test.beforeAll(async () => {
  server = startServer({ port: PORT, dataDir: DATA_DIR });
  await waitForServer(PORT, 20_000, server ?? undefined);
});

test.afterAll(async () => {
  await stopServer(server);
  server = null;
});

test('录制设备回环：落出可回放的会话实例与真实 WAV 文件', async ({ page }) => {
  await openAppAt(page, BASE);
  await navTo(page, '录制');
  await pickDefaultDevice(page);
  await startRecording(page, 4);
  await stopRecording(page);

  // 1) 音频文件真的落盘了：sessions/<id>/audio.wav，且远大于 44 字节头
  const sessionsDir = path.join(DATA_DIR, 'sessions');
  const ids = fs.existsSync(sessionsDir) ? fs.readdirSync(sessionsDir) : [];
  expect(ids.length).toBe(1);
  audioFile = path.join(sessionsDir, ids[0], 'audio.wav');
  expect(fs.existsSync(audioFile)).toBe(true);
  // 录 4 秒 = 4s × 16000Hz × 2B ≈ 128KB；只断言「明显大于头」以避开机器差异
  expect(fs.statSync(audioFile).size).toBeGreaterThan(44 + 16000 * 2 * 2);

  // 2) 会话出现在会话库（不是只写了个文件）
  await navTo(page, '会话库');
  await expect(page.locator('main')).toContainText('共 1 条');
  // 测试音不是语音、也没有模型：句数为 0 是正确结果，不能断言有片段
  await expect(page.locator('main')).toContainText('0 句');
  // 自动命名用设备友好名，不能是截断的端点 ID 碎片（G-E2E-02：曾是「音频设备 e5854bb}」）
  await expect(page.locator('main')).not.toContainText(/音频设备 [0-9a-f]*\}/);
});

test('无模型时发起转写：给出可读错误，音频与元数据不受损、仍可回放', async ({ page }) => {
  await openAppAt(page, BASE);
  await navTo(page, '会话库');
  const sizeBefore = fs.statSync(audioFile).size;

  await page.getByRole('button', { name: '发起转写' }).first().click();

  // 1) 错误是**可读、可行动的中文提示**，不是静默失败、也不是裸错误码
  //
  // 无模型包时后端透出 ERR_MODEL_NOT_FOUND（transcriptsvc.requireEngine），界面引导去「模型」页。
  // 此前外层统一是 ERR_ENGINE_NOT_READY「请稍后重试」，会把用户导向无效重试（G-E2E-03，已修复）。
  // 换成模型包存在时它应变为成功路径，那时本用例的断言应随行为更新。
  const failure = page.getByText(/当前模型不可用/).first();
  await expect(failure).toBeVisible({ timeout: 20_000 });
  await expect(failure).toContainText('「模型」页');
  // 可读性判据：是一句带标点的中文说明，而不是裸 code
  await expect(failure).toContainText(/。$/);

  // 2) 音频没被删、大小没变（FR-03-6：转写失败不得改写音频）
  expect(fs.existsSync(audioFile)).toBe(true);
  expect(fs.statSync(audioFile).size).toBe(sizeBefore);

  // 3) 会话仍在库里，且仍可回放
  await expect(page.locator('main')).toContainText('共 1 条');
  await page.locator('[aria-label="回放"]').first().click();
  const duration = await expectAudioPlayable(page);
  expect(duration).toBeGreaterThan(2);

  // 4) 再点一次同一入口：仍然同一条清晰失败路径（不是「第二次就静默成功」）
  //
  // 说明：本 build 里发起时就同步失败，会话没有进入 failed 终态，因此列表上是**没有**
  // 「重试转写」按钮的（只有 failed 才出现）。所以这里用「重复点击发起转写」验证
  // 同一失败可复现；「重试转写」按钮本身属于 needs-model 的人工验证项。
  await page.getByRole('button', { name: '关闭回放' }).click();
  await page.getByRole('button', { name: '发起转写' }).first().click();
  await expect(page.getByText(/当前模型不可用/).first()).toBeVisible({
    timeout: 20_000,
  });
  expect(fs.statSync(audioFile).size).toBe(sizeBefore);
});

test('Windows 应用音源退出：同一会话自动暂停、留下 source-lost 并可回放', async ({ page }, testInfo) => {
  test.skip(process.platform !== 'win32', '此用例验证 Windows 按应用捕获的退出行为。');
  test.setTimeout(75_000);
  await openAppAt(page, BASE);
  await navTo(page, '录制');
  await expect(
    page.getByText('支持按应用分路捕获：只录制选中应用的声音。', { exact: true }),
    '此用例需要 Windows 进程回环探测通过，不能退回设备混音',
  ).toBeVisible({ timeout: 15_000 });

  const db = new DatabaseSync(path.join(DATA_DIR, 'mandaryn.db'), { readOnly: true });
  try {
    const sessions = db.prepare('SELECT id, source_id, source_kind, status, duration_ms FROM sessions');
    const existing = new Set(sessions.all().map(({ id }) => id));
    const newSessions = () => sessions.all().filter(({ id }) => !existing.has(id));
    const player = startWindowsRecordingPlayer(testInfo.outputPath('source-player'));
    try {
      const source = page.getByRole('button', { name: `选择音频源：${player.name}`, exact: true });
      const refresh = page.getByRole('button', { name: '刷新源列表', exact: true });
      const producing = source.getByText('正在发声', { exact: true });
      await expect.poll(async () => {
        player.assertRunning();
        if (await producing.isVisible()) return true;
        await refresh.click({ timeout: 3_000 });
        return producing.isVisible();
      }, { timeout: 12_000, message: '专用播放器必须作为应用源出现并发声' }).toBe(true);
      await source.click();
      await expect(source).toHaveAttribute('aria-pressed', 'true');

      const main = page.locator('main');
      const recording = main.getByText('录制中', { exact: true });
      const pause = page.getByRole('button', { name: '暂停', exact: true });
      const stop = page.getByRole('button', { name: '停止', exact: true });
      await page.getByRole('button', { name: '开始录制', exact: true }).click();
      await expect(recording).toBeVisible();
      await expect(pause).toBeEnabled();
      const started = newSessions();
      expect(started).toHaveLength(1);
      expect(started[0]).toMatchObject({
        source_id: `app:${player.name}.exe`, source_kind: 'application', status: 'recording',
      });
      const sessionId = String(started[0].id);
      const wavPath = path.join(DATA_DIR, 'sessions', sessionId, 'audio.wav');
      const lossEvents = db.prepare(
        "SELECT at_ms FROM session_events WHERE session_id = ? AND kind = 'source-lost'",
      );

      // 不调用 startRecording：它会额外在浏览器中播放测试音，掩盖捕获错源。
      let captured: Buffer = Buffer.alloc(0);
      await expect.poll(() => {
        player.assertRunning();
        captured = fs.readFileSync(wavPath);
        return matchingToneWindows(captured, player.tonesHz);
      }, { timeout: 10_000, message: '播放器退出前，原会话的 PCM 必须已写入至少 1.2 秒专用双音' })
        .toBeGreaterThanOrEqual(12);
      await expect(recording).toBeVisible();
      await expect(pause).toBeEnabled();
      expect(lossEvents.all(sessionId)).toHaveLength(0);

      await player.exit();
      // 没有点击暂停。录制页自动切到暂停、原会话时间轴写标记、且没有开启新会话。
      await expect(main.getByText('已暂停', { exact: true })).toBeVisible({ timeout: 15_000 });
      await expect(page.getByRole('button', { name: '继续', exact: true })).toBeVisible();
      await expect(stop).toBeEnabled();
      await expect.poll(() => lossEvents.all(sessionId), { timeout: 5_000 }).toHaveLength(1);
      const lostAt = Number(lossEvents.get(sessionId)?.at_ms);
      expect(lostAt).toBeGreaterThanOrEqual(1_200);
      expect(newSessions().map(({ id }) => id)).toEqual([sessionId]);

      await stopRecording(page);
      const completed = newSessions();
      expect(completed).toHaveLength(1);
      expect(completed[0]).toMatchObject({ id: sessionId, status: 'completed' });
      expect(completed[0].duration_ms).toBeGreaterThanOrEqual(lostAt);
      expect(lossEvents.all(sessionId)).toEqual([{ at_ms: lostAt }]);
      const sealed = fs.readFileSync(wavPath);
      expect(sealed.toString('ascii', 0, 4)).toBe('RIFF');
      expect(sealed.readUInt32LE(4)).toBe(sealed.length - 8);
      expect(sealed.readUInt32LE(40)).toBe(sealed.length - 44);
      expect(sealed.subarray(44, captured.length).equals(captured.subarray(44))).toBe(true);

      await navTo(page, '会话库');
      const row = page.locator(`[data-session-id="${sessionId}"]`);
      await row.getByRole('button', { name: '回放', exact: true }).click();
      await expect(page.locator('audio')).toHaveAttribute('src', new RegExp(`/audio/${sessionId}(?:\\?|$)`));
      expect(await expectAudioPlayable(page)).toBeGreaterThanOrEqual(1.2);
    } finally {
      await player.dispose();
    }
  } finally {
    db.close();
  }
});
