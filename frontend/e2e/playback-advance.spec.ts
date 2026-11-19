import { expect, test } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { REPO_ROOT, startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';
import { ensureToneWav } from './fixtures';
import {
  METRIC_AUDIO_MS, METRIC_SEGMENTS, METRIC_STRIDE_MS,
  assertPlaybackMetric, measurePlaybackWindow, openPlaybackMetric,
  preparePlaybackWindow, seedPlaybackMetric,
} from './playback-metric.fixture';

// 显式选择 short/full/negative。未选择时不生成 115MB 的一小时夹具；skip 不代表验收通过。
// 同一个现有 isolated spec 承载量化，不改共享 launcher / 项目匹配规则。
const METRIC_MODE = process.env.E2E_PLAYBACK_METRIC;
if (METRIC_MODE !== undefined && !['short', 'full', 'negative'].includes(METRIC_MODE)) {
  throw new Error('E2E_PLAYBACK_METRIC 只能是 short、full 或 negative');
}

// 回放高亮**随播放推进**（FR-04-2 / FR-04-3，歌词式同步）。
//
// 为什么单独立一个用例：playback-highlight.spec.ts 验证的是「点句跳转 + 句内高亮不乱跳」，
// 它的播放断言是「0.7s 后高亮**仍停在同一句**」—— 这个断言在「高亮完全不跟随播放」时
// 同样会通过。于是下面这个真实缺陷曾长期逃过检测：
//
//   监听 effect 只依赖 [audioKey, syncFromAudio]，而 <audio> 元素是在抽屉打开后才挂载的。
//   首次运行该 effect 时 audioRef.current 还是 null，直接 return；元素迟到挂载后依赖不变，
//   effect 不会重跑 → play / timeupdate 监听永远绑不上 → 音频在播，但时钟与高亮全部冻住。
//
// 本用例用**真实播放按钮**（不是 evaluate 里直接 audio.play()）播放 3 秒，断言高亮确实
// 跨越多个句子推进，且位置标签同步前进。修复方式见 usePlayback 的 audioRef 注释。
const PORT = Number(process.env.E2E_ADVANCE_PORT ?? 9362);
const DATA_DIR = path.join(REPO_ROOT, '.e2e-data', 'playback-advance');
const BASE = `http://127.0.0.1:${PORT}`;
const SESSION_ID = 's_E2EADVANCE00000000000001';


// 8 句 × 560ms 步进，覆盖 5.6s 音频；播放 3s 应跨过 5 个边界。
// 音频由 fixtures.ensureToneWav 在运行时生成（只需时长，不涉及 ASR）。
const SEG_COUNT = 8;
const SEG_STRIDE_MS = 560;
const AUDIO_MS = 6000;
const SEGMENTS = Array.from({ length: SEG_COUNT }, (_, i) => ({
  seq: i,
  startMs: i * SEG_STRIDE_MS,
  endMs: i * SEG_STRIDE_MS + 500,
  text: `（夹具第 ${i + 1} 句）用于验证高亮随播放推进`,
}));

const seed = (source: string): void => {
  const raw = fs.readFileSync(source);
  expect(raw.subarray(0, 4).toString('ascii')).toBe('RIFF');
  expect(raw.readUInt32LE(24)).toBe(16000);

  const audioRel = `sessions/${SESSION_ID}/audio.wav`;
  const audioAbs = path.join(DATA_DIR, audioRel);
  fs.mkdirSync(path.dirname(audioAbs), { recursive: true });
  fs.copyFileSync(source, audioAbs);

  const frames = raw.readUInt32LE(40) / 2;
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
      '高亮推进夹具会话',
      'dev:e2e-advance',
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
      SEGMENTS.reduce((sum, s) => sum + s.text.length, 0),
      'done',
      now,
      'e2e-fixture',
      durationMs,
      now,
      now,
    );
    const ins = db.prepare(
      `INSERT INTO segments
        (session_id, seq, start_ms, end_ms, text, speaker_id, confidence, engine_id, is_final, edited, created_at)
       VALUES (?,?,?,?,?,'',0.9,'e2e-fixture',1,0,?)`,
    );
    for (const s of SEGMENTS) ins.run(SESSION_ID, s.seq, s.startMs, s.endMs, s.text, now);
  } finally {
    db.close();
  }
};

let server: ServerHandle | null = null;

test.beforeAll(async () => {
  // 夹具**生成**而非查找：不依赖任何本机/仓库内的语音文件，CI 与本地行为一致。
  // 缺素材时不再 test.skip —— 生成失败会直接抛错，属于真实失败而不是「悄悄跳过」。
  const source = ensureToneWav(AUDIO_MS);

  fs.rmSync(DATA_DIR, { recursive: true, force: true });

  server = startServer({ port: PORT, dataDir: DATA_DIR });
  await waitForServer(PORT, 20_000, server);
  await stopServer(server);
  server = null;

  seed(source);
  if (METRIC_MODE !== undefined) seedPlaybackMetric(DATA_DIR);

  server = startServer({ port: PORT, dataDir: DATA_DIR, fresh: false });
  await waitForServer(PORT, 20_000, server);
});

test.afterAll(async () => {
  if (server !== null) await stopServer(server);
  server = null;
});

test('回放：点播放后高亮随播放位置推进（跨越多个句子）', async ({ page }) => {
  test.info().annotations.push({
    type: '回归说明',
    description:
      '守护「audio 事件监听因元素迟到挂载而从未绑定」这一缺陷：' +
      '症状是音频在播，但位置标签与高亮全部冻住。playback-highlight.spec 的句内断言覆盖不到它。',
  });

  await page.goto(`${BASE}/`);
  await expect(page.locator('#startup-skeleton')).toHaveCount(0, { timeout: 10_000 });
  await page.getByRole('button', { name: '会话库' }).first().click();
  await expect(page.locator('main')).toContainText('高亮推进夹具会话');
  await page.locator('[aria-label="回放"]').first().click();
  await expect(page.locator('[data-playback-total]')).toHaveAttribute(
    'data-playback-total',
    String(SEG_COUNT),
    { timeout: 20_000 },
  );

  const audio = page.locator('audio').first();
  await expect
    .poll(async () => audio.evaluate((el: HTMLAudioElement) => el.readyState), { timeout: 20_000 })
    .toBeGreaterThanOrEqual(1);

  // 走真实 UI：点播放按钮（必须经由 React 的播放路径，而不是直接调 audio.play()）
  const labels = await page.evaluate(() =>
    Array.from(document.querySelectorAll('button[aria-label]')).map((b) => b.getAttribute('aria-label') ?? ''),
  );
  const playLabel = labels.find((l) => /播\s*放|play/i.test(l));
  expect(playLabel, '界面里应存在「播放」按钮').toBeTruthy();
  await page.locator(`button[aria-label="${playLabel}"]`).first().click();
  await expect.poll(async () => audio.evaluate((el: HTMLAudioElement) => el.paused), { timeout: 5_000 }).toBe(false);

  // 播放中逐帧记录「音频位置 + 当前高亮句」
  const samples = await page.evaluate(async (): Promise<Array<{ tMs: number; seqs: number[] }>> => {
    const a = document.querySelector('audio') as HTMLAudioElement;
    const out: Array<{ tMs: number; seqs: number[] }> = [];
    const started = performance.now();
    await new Promise<void>((resolve) => {
      const tick = (): void => {
        const rows = document.querySelectorAll('[data-playback-row][aria-current="true"]');
        const seqs: number[] = [];
        rows.forEach((r) => seqs.push(Number(r.getAttribute('data-playback-row'))));
        out.push({ tMs: Math.round(a.currentTime * 1000), seqs });
        if (performance.now() - started >= 3000) {
          resolve();
          return;
        }
        requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    });
    return out;
  });
  await audio.evaluate((el: HTMLAudioElement) => el.pause());

  const last = samples[samples.length - 1]!;
  expect(last.tMs).toBeGreaterThan(2500); // 确实播了 ~3s

  // 1) 高亮必须跨越多个句子（缺陷存在时恒为第 0 句，此断言会红）
  const distinct = Array.from(new Set(samples.flatMap((s) => s.seqs))).sort((a, b) => a - b);
  expect(distinct.length, `播放 3s 应跨越多个句子，实际只出现过 ${JSON.stringify(distinct)}`).toBeGreaterThan(2);

  // 2) 任何一帧都只能有一句高亮
  const multi = samples.filter((s) => s.seqs.length > 1);
  expect(multi.slice(0, 3), `存在同时多句高亮的帧（共 ${multi.length} 帧）`).toHaveLength(0);

  // 3) 高亮与播放位置一致：末帧的高亮句应为包含该时刻的句子
  const expectedAtEnd = Math.min(Math.floor(last.tMs / SEG_STRIDE_MS), SEG_COUNT - 1);
  expect(last.seqs[0]).toBe(expectedAtEnd);

  // 4) 位置标签也必须在推进（时钟与高亮同源，一起守住「监听未绑定」这个症状）
  const posLabel = await page.evaluate(
    () =>
      Array.from(document.querySelectorAll('[aria-label]'))
        .map((e) => e.getAttribute('aria-label') ?? '')
        .find((l) => l.includes('播放位置')) ?? '',
  );
  expect(posLabel, '位置标签应已推进，而不是停在 00:00').not.toContain('00:00 /');
});

// 运行示例（仓库根目录，主线先重建 server，避免旧嵌入资源造成假证据）：
// E2E_PLAYBACK_METRIC=short pnpm --dir frontend --filter mandaryn-frontend run e2e e2e/playback-advance.spec.ts --project=isolated --grep '回放量化'
// full：约 1 小时连续 1x，无中途 seek；short：三段 6s 预查，不能替代完整 PRD 验收。
// negative：同一条 ≤80ms 断言预期失败（160ms 故障注入）；退出码必须非零。
test('回放量化：一小时五千句的 DOM 提交同步误差 ≤80ms', async ({ page }) => {
  test.skip(METRIC_MODE === undefined, '显式 E2E_PLAYBACK_METRIC=short/full/negative 才生成长会话夹具；未执行不算通过');
  test.setTimeout(METRIC_MODE === 'full' ? METRIC_AUDIO_MS + 180_000 : 120_000);
  await openPlaybackMetric(page, BASE);
  test.info().annotations.push({ type: '量化范围', description: METRIC_MODE === 'full'
    ? '真实一小时 PCM，5000 片段，连续 1x，测量中禁止 seek，每次切换 ≤80ms（不是 p95）'
    : METRIC_MODE === 'negative'
      ? '负对照：产品读时钟落后160ms，meter读原生时钟，同一80ms判据必须报红'
      : '仅短窗预查：首个分页边界、中段分页边界、末尾；不是完整1小时验收' });

  const starts = METRIC_MODE === 'full' || METRIC_MODE === 'negative'
    ? [0]
    : [198 * METRIC_STRIDE_MS, 2398 * METRIC_STRIDE_MS, 4990 * METRIC_STRIDE_MS];
  for (const start of starts) {
    await preparePlaybackWindow(page, start);
    const stop = METRIC_MODE === 'full' ? METRIC_AUDIO_MS : start + 6000;
    const report = await measurePlaybackWindow(page, stop, METRIC_MODE === 'negative' ? 160 : 0);
    await test.info().attach(`playback-metric-${METRIC_MODE}-${start}.json`, {
      body: JSON.stringify({ mode: METRIC_MODE, fixtureDurationMs: METRIC_AUDIO_MS,
        fixtureSegments: METRIC_SEGMENTS, ...report }, null, 2),
      contentType: 'application/json',
    });
    // 先证明负对照确实造出了 ≥120ms，而不是用 unrelated 启动错误冒充红灯。
    if (METRIC_MODE === 'negative') expect(report.maxErrorMs, '负对照故障必须被真实DOM测量捕获').toBeGreaterThanOrEqual(120);
    assertPlaybackMetric(report);
    if (METRIC_MODE === 'full') {
      expect(report.boundaries).toHaveLength(METRIC_SEGMENTS - 1);
      expect(report.endMs).toBeGreaterThanOrEqual(METRIC_AUDIO_MS - 1);
      expect(report.wallMs).toBeGreaterThanOrEqual(METRIC_AUDIO_MS - 1000);
    }
  }
});

test('回放量化负对照：≥120ms 的真实高亮滞后必须被80ms判据拒绝', async ({ page }) => {
  test.skip(METRIC_MODE !== 'short', '内置负对照只随 short 预查执行；negative 模式用于检查真实非零退出码');
  test.setTimeout(60_000);
  await openPlaybackMetric(page, BASE);
  await preparePlaybackWindow(page, 0);
  const report = await measurePlaybackWindow(page, 6000, 160);
  await test.info().attach('playback-metric-negative-control.json', {
    body: JSON.stringify(report, null, 2), contentType: 'application/json',
  });
  expect(report.boundaries.length).toBeGreaterThanOrEqual(5);
  expect(report.missing).toEqual([]);
  expect(report.seekingEvents).toBe(0);
  expect(report.maxErrorMs).toBeGreaterThanOrEqual(120);
  // 确认同一生产判据拒绝这份真实观察结果，而非伪造一个数字的纯函数自测。
  expect(() => assertPlaybackMetric(report)).toThrow();
});
