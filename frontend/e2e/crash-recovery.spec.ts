import { expect, test } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Page } from '@playwright/test';
import {
  expectAudioPlayable,
  navTo,
  openAppAt,
  pickDefaultDevice,
  startRecording,
  stopRecording,
  TEST_TONE_LAUNCH,
} from './helpers';
import { REPO_ROOT, killServer, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

// 崩溃恢复（异常场景 E2E / UC-07）：录制中被强杀 → 重启应用 → 会话不丢、音频仍可回放。
//
// 现场构造方式（这是本用例唯一诚实的方式）：
//   1) 真实录制设备回环 4 秒（页面同时播放小音量测试音，见 helpers.playTestTone）；
//   2) 直接 SIGKILL 后端进程 —— 不给收尾机会，于是库里的 status 停在 recording、
//      WAV 头的 data 长度仍是 0（只在正常 Seal 时回填）；
//   3) 用同一个数据目录重启后端，再从**用户视角**断言结果。
//
// 进程生命周期自己管：Playwright 的 webServer 只负责首次启动，无法中途重启。
// 因此本文件被划进 `isolated` project，端口与数据目录都与共享 server 完全分开。
test.use({ launchOptions: TEST_TONE_LAUNCH });
const PORT = Number(process.env.E2E_CRASH_PORT ?? 9356);
// 放在 .e2e-data/ 之下：该目录整体被 .gitignore 忽略（用例会录进真实设备音频）。
const DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'crash');
const BASE = `http://127.0.0.1:${PORT}`;

test.describe.configure({ mode: 'serial' });

let server: ServerHandle | null = null;
let sessionID = '';
let audioFile = '';

const sessionRow = (): { status: string; duration_ms: number } | null => {
  const db = new DatabaseSync(path.join(DATA_DIR, 'mandaryn.db'));
  try {
    const rows = db.prepare('SELECT status, duration_ms FROM sessions').all() as {
      status: string;
      duration_ms: number;
    }[];
    return rows[0] ?? null;
  } finally {
    db.close();
  }
};

// wavDataSize 读 WAV 头里声明的 data 块长度（崩溃后应为 0，修补后应等于 文件长度-44）。
const wavDataSize = (): number => fs.readFileSync(audioFile).readUInt32LE(40);

// createCrashScene 制造一份「录制中被强杀」的现场，并返回会话 ID。
//
// 两条用例各自调用一次：让它们互相独立（可用 --grep 单独跑），代价只是多录 4 秒。
const createCrashScene = async (page: Page): Promise<string> => {
  server = startServer({ port: PORT, dataDir: DATA_DIR });
  await waitForServer(PORT, 20_000, server ?? undefined);

  await openAppAt(page, BASE);
  await navTo(page, '录制');
  await pickDefaultDevice(page);
  await startRecording(page, 4);

  // 强杀：不给 ServiceShutdown 任何收尾机会
  await killServer(server);
  server = null;

  const sessionsDir = path.join(DATA_DIR, 'sessions');
  const ids = fs.readdirSync(sessionsDir);
  expect(ids.length).toBe(1);
  sessionID = ids[0];
  audioFile = path.join(sessionsDir, sessionID, 'audio.wav');
  return sessionID;
};

test.afterEach(async () => {
  await stopServer(server);
  server = null;
});

test('强杀后端后重启：会话仍在会话库，音频仍可回放', async ({ page }) => {
  await createCrashScene(page);

  // 现场判据：库里状态不是终态，且 WAV 头声明的 data 长度为 0（未 Seal）
  expect(sessionRow()?.status).toBe('recording');
  expect(wavDataSize()).toBe(0);
  // 但音频数据确实已经落盘（崩溃前一直在刷盘）
  expect(fs.statSync(audioFile).size).toBeGreaterThan(44 + 16000);

  // 同一数据目录重启，从用户视角断言
  server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false });
  await waitForServer(PORT, 20_000, server ?? undefined);

  await openAppAt(page, BASE);
  await navTo(page, '会话库');
  // 会话没有被崩溃吞掉：仍在库里，且能打开回放
  await expect(page.locator('main')).toContainText('共 1 条');
  await page.locator('[aria-label="回放"]').first().click();

  // 可回放 = 音频真的能播（duration 有值、play() 成功、currentTime 前进）
  const duration = await expectAudioPlayable(page);
  expect(duration).toBeGreaterThan(2);

  // 音频字节一个没丢：/audio 提供的正是崩溃前落盘的那份文件
  const served = await page.request.get(`${BASE}/audio/${sessionID}`);
  expect(served.ok()).toBe(true);
  const body = await served.body();
  expect(body.length).toBe(fs.statSync(audioFile).size);
  expect(body.subarray(0, 4).toString('ascii')).toBe('RIFF');
});

// 崩溃恢复的第二半：会话必须被**识别为异常结束**并把 WAV 头修好（FR-03 / UC-07）。
//
// 为什么这条用 test.fail()：产品已有 RecoverUnfinished（internal/recording/recover.go）
// 与 RepairWAV，但**装配层从未调用它**（internal/app/app.go 无任何调用点），因此真实
// 行为是：重启后 status 仍是 recording、WAV 头 data 长度仍是 0。上面那条用例之所以还能
// 播放，纯属浏览器容忍「data 长度 0 + 后面有数据」这种残缺头，而不是因为头被修好了。
//
// G-E2E-01 已修复：internal/app 现在在 recSvc 就绪后调用 RecoverUnfinished（与 modelsSvc.Cleanup
// 同位置、失败不阻断启动）。此前这里用 test.fail() 钉住缺陷，修好后 Playwright 报
// 「Expected to fail, but passed」提示去掉标注 —— 本用例已改回普通断言，成为真正的红灯。
test('崩溃恢复：会话被标记为中断且 WAV 头按实际长度回填', async ({ page }) => {

  await createCrashScene(page);

  server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false });
  await waitForServer(PORT, 20_000, server ?? undefined);
  await openAppAt(page, BASE);

  // 期望 1：库里状态被改为 interrupted（可辨识的「异常结束」）
  await expect.poll(() => sessionRow()?.status, { timeout: 15_000 }).toBe('interrupted');
  // 期望 2：WAV 头被按实际文件长度回填，且时长元数据随之更新
  const actual = fs.statSync(audioFile).size - 44;
  expect(wavDataSize()).toBe(actual);
  expect(sessionRow()?.duration_ms ?? 0).toBeGreaterThan(2000);

  // 期望 3（用户可见）：修补后仍可回放
  await navTo(page, '会话库');
  await page.locator('[aria-label="回放"]').first().click();
  await expectAudioPlayable(page);
});
