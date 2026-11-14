import { expect, test } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { REPO_ROOT, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';
import { ensureToneWav } from './fixtures';

// 回放高亮（FR-04）：歌词式列表随播放位置高亮、点句跳转并归位。
//
// 为什么不走「真转写」：E2E 的数据目录是隔离且全新的，没有任何模型包；而设备回环在
// 无人说话的机器上只能录到静音，VAD 分不出句子。因此这里按「预置语料」的方式做：
// 在 server 启动**之前**直接写入一条会话 + 若干片段 + 一份**真实中文语音 WAV**，
// 再断言高亮与跳转的机械行为。
//
// 诚实边界（重要）：片段**文本是测试夹具**，不是 ASR 产物 —— 本用例验证的是
// 「时间轴 → 高亮 → 跳转」的同步逻辑，不是识别准确率。真实转写 + 真实语音的全链路
// 需要模型包，见 docs/DEV-ISSUES-G-e2e.md 的人工验证步骤。
const PORT = Number(process.env.E2E_PLAYBACK_PORT ?? 9358);
const DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'playback');
const BASE = `http://127.0.0.1:${PORT}`;

// 音频夹具：**运行时生成**（16kHz / 单声道 / 16bit 正弦波），不依赖任何本机语音文件。
// 回放高亮只依赖音频**时长**（把 currentTime 映射到片段区间），与音频内容、是否含人声无关，
// 因此生成音即可；原先硬编码个人机器绝对路径 + 缺失即 skip 的做法会让 CI 上这条用例
// 静默不执行（绿灯不代表验证过），已改为生成夹具。
const AUDIO_MS = 6000;

const SESSION_ID = 's_E2EPLAYBACK0000000000001';

// 片段切分：对齐 5.592s 音频，句间留 200ms 间隙，避免边界抖动导致高亮判定不稳。
const SEGMENTS = [
  { seq: 0, startMs: 200, endMs: 1200, text: '（夹具第 1 句）这一行用来验证按句高亮' },
  { seq: 1, startMs: 1400, endMs: 2500, text: '（夹具第 2 句）播放到边界应该切到下一句' },
  { seq: 2, startMs: 2800, endMs: 4000, text: '（夹具第 3 句）点击任意一句应跳到该句开头' },
  { seq: 3, startMs: 4200, endMs: 5300, text: '（夹具第 4 句）用于验证向后跳转仍然精确' },
];

// seed 在 server 已建好 schema、且已停止时写入语料。
//
// 为什么先起一次 server 再停：表结构由 GORM AutoMigrate 生成（以
// internal/storage/models.go 为准），自己手抄一份 schema 必然随代码漂移。
const seed = (source: string): void => {
  const raw = fs.readFileSync(source);
  // 只接受标准 44 字节 PCM 头且 16k 采样率，否则片段毫秒与音频时间轴对不上。
  expect(raw.subarray(0, 4).toString('ascii')).toBe('RIFF');
  expect(raw.readUInt32LE(24)).toBe(16000);

  const audioRel = `sessions/${SESSION_ID}/audio.wav`;
  const audioAbs = path.join(DATA_DIR, audioRel);
  fs.mkdirSync(path.dirname(audioAbs), { recursive: true });
  fs.copyFileSync(source, audioAbs);

  const frames = raw.readUInt32LE(40) / 2; // 16bit 单声道
  const durationMs = Math.round((frames / 16000) * 1000);
  const now = Date.now();

  const db = new DatabaseSync(path.join(DATA_DIR, 'mandaryn.db'));
  try {
    db.prepare(
      `INSERT INTO sessions
        (id, title, source_id, source_name, source_kind, model_source, engine_id, status,
         degraded, degrade_reason, started_at, ended_at, duration_ms, audio_path, audio_format,
         size_bytes, segment_count, speaker_count, char_count,
         transcription_status, transcribed_at, transcribe_engine_id, live_transcribed,
         transcribe_progress_ms, created_at, updated_at)
       VALUES (?,?,?,?,?,?,?,?,0,'',?,?,?,?,?,?,?,0,?,?,?,?,0,?,?,?)`,
    ).run(
      SESSION_ID,
      '回放高亮夹具会话',
      'dev:e2e-playback',
      '音频设备（E2E 夹具）',
      'device',
      'local',
      'e2e-fixture',
      'completed',
      now - durationMs,
      now,
      durationMs,
      audioRel,
      'wav',
      raw.length,
      SEGMENTS.length,
      SEGMENTS.reduce((sum, seg) => sum + seg.text.length, 0),
      'done',
      now,
      'e2e-fixture',
      durationMs,
      now,
      now,
    );

    const insSeg = db.prepare(
      `INSERT INTO segments
        (session_id, seq, start_ms, end_ms, text, speaker_id, confidence, engine_id, is_final, edited, created_at)
       VALUES (?,?,?,?,?,'',0.9,'e2e-fixture',1,0,?)`,
    );
    for (const seg of SEGMENTS) insSeg.run(SESSION_ID, seg.seq, seg.startMs, seg.endMs, seg.text, now);
  } finally {
    db.close();
  }
};

test.describe.configure({ mode: 'serial' });

let server: ServerHandle | null = null;

test.beforeAll(async () => {
  // 生成夹具（不再查找本机文件、不再 test.skip）：生成失败会抛错，属真实失败。
  const source = ensureToneWav(AUDIO_MS);

  // 1) 起一次 server 建 schema，然后停掉（数据目录必须是干净的）
  server = startServer({ port: PORT, dataDir: DATA_DIR });
  await waitForServer(PORT, 20_000, server ?? undefined);
  await stopServer(server);
  server = null;

  // 2) 落语料：会话 + 片段 + 真实 WAV
  seed(source);

  // 3) 再起 server 供断言使用（fresh:false = 保留刚写入的语料）
  server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false });
  await waitForServer(PORT, 20_000, server ?? undefined);
});

test.afterAll(async () => {
  await stopServer(server);
  server = null;
});

test('回放：按句高亮跟随播放位置，点句跳转后归位', async ({ page }) => {
  test.info().annotations.push({
    type: '夹具说明',
    description:
      '片段文本为测试夹具（非 ASR 产物）：隔离数据目录内没有模型包，设备回环也录不到语音，' +
      '因此用真实中文 WAV + 手工时间轴验证「时间轴→高亮→跳转」同步逻辑。',
  });

  await page.goto(`${BASE}/`);
  await expect(page.locator('#startup-skeleton')).toHaveCount(0, { timeout: 10_000 });

  await page.getByRole('button', { name: '会话库' }).first().click();
  await expect(page.locator('main')).toContainText('回放高亮夹具会话');
  await expect(page.locator('main')).toContainText('4 句');

  await page.locator('[aria-label="回放"]').first().click();

  // 抽屉内总句数来自后端时间轴（不是前端猜的）
  const list = page.locator('[data-playback-total]');
  await expect(list).toHaveAttribute('data-playback-total', String(SEGMENTS.length), { timeout: 15_000 });

  const audio = page.locator('audio').first();
  await expect
    .poll(async () => audio.evaluate((el: HTMLAudioElement) => el.readyState), { timeout: 15_000 })
    .toBeGreaterThanOrEqual(1);
  expect(await audio.evaluate((el: HTMLAudioElement) => el.duration)).toBeGreaterThan(5);

  // 1) 点第 3 句 → 音频跳到该句开头，且该句成为当前句
  await page.locator('[data-playback-row="2"]').click();
  await expect(page.locator('[data-playback-row="2"]')).toHaveAttribute('aria-current', 'true');
  const jumpTime = await audio.evaluate((el: HTMLAudioElement) => el.currentTime);
  expect(Math.abs(jumpTime * 1000 - SEGMENTS[2].startMs)).toBeLessThan(250);

  // 2) 句内播放 0.7s：高亮不乱跳、时钟确实在前进
  const clockBefore = await audio.evaluate((el: HTMLAudioElement) => el.currentTime);
  await audio.evaluate(async (el: HTMLAudioElement) => {
    await el.play();
    await new Promise((resolve) => setTimeout(resolve, 700));
    el.pause();
  });
  const clockAfter = await audio.evaluate((el: HTMLAudioElement) => el.currentTime);
  expect(clockAfter).toBeGreaterThan(clockBefore);
  await expect(page.locator('[data-playback-row="2"]')).toHaveAttribute('aria-current', 'true');

  // 3) 跳到第 4 句：高亮跟着换，且**同时只有一句**高亮
  await page.locator('[data-playback-row="3"]').click();
  await expect(page.locator('[data-playback-row="3"]')).toHaveAttribute('aria-current', 'true');
  await expect(page.locator('[data-playback-row][aria-current="true"]')).toHaveCount(1);
  const lastTime = await audio.evaluate((el: HTMLAudioElement) => el.currentTime);
  expect(Math.abs(lastTime * 1000 - SEGMENTS[3].startMs)).toBeLessThan(250);
});
