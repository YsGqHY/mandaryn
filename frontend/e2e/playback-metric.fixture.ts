import { expect, type Page } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';

// 专用夹具，不是 ASR 准确率测试。真实 PCM 媒体时钟 + 5000 条 SQLite 时间轴。
export const METRIC_AUDIO_MS = 3_600_000;
export const METRIC_SEGMENTS = 5000;
export const METRIC_STRIDE_MS = METRIC_AUDIO_MS / METRIC_SEGMENTS; // 720ms，连续无间隙
export const METRIC_TITLE = '一小时五千句回放量化夹具';
const SESSION_ID = 's_E2EMETRIC00000000000001';
const LIMIT_MS = 80;

export const seedPlaybackMetric = (dataDir: string): void => {
  const audioRel = `sessions/${SESSION_ID}/audio.wav`;
  const audioAbs = path.join(dataDir, audioRel);
  fs.mkdirSync(path.dirname(audioAbs), { recursive: true });
  const dataBytes = (METRIC_AUDIO_MS / 1000) * 16000 * 2;
  const header = Buffer.alloc(44);
  header.write('RIFF', 0);
  header.writeUInt32LE(36 + dataBytes, 4);
  header.write('WAVEfmt ', 8);
  header.writeUInt32LE(16, 16);
  header.writeUInt16LE(1, 20);
  header.writeUInt16LE(1, 22);
  header.writeUInt32LE(16000, 24);
  header.writeUInt32LE(32000, 28);
  header.writeUInt16LE(2, 32);
  header.writeUInt16LE(16, 34);
  header.write('data', 36);
  header.writeUInt32LE(dataBytes, 40);
  const fd = fs.openSync(audioAbs, 'w');
  try {
    fs.writeSync(fd, header);
    // 文件余下字节按 PCM 静音补零；不写共享音频缓存，不生成 5760 万次正弦。
    fs.ftruncateSync(fd, 44 + dataBytes);
  } finally {
    fs.closeSync(fd);
  }
  const now = Date.now();
  const db = new DatabaseSync(path.join(dataDir, 'mandaryn.db'));
  try {
    db.exec('BEGIN');
    db.prepare(`INSERT INTO sessions
      (id,title,source_id,source_name,source_kind,model_source,engine_id,status,
       degraded,degrade_reason,started_at,ended_at,duration_ms,audio_path,audio_format,
       size_bytes,segment_count,speaker_count,char_count,transcription_status,
       transcribed_at,transcribe_engine_id,live_transcribed,transcribe_progress_ms,created_at,updated_at)
      VALUES (?,?,?,?,?,?,?,?,0,'',?,?,?,?,?,?,?,0,0,'done',?,'e2e-fixture',0,?,?,?)`)
      .run(SESSION_ID, METRIC_TITLE, 'dev:e2e-metric', 'E2E PCM', 'device', 'local',
        'e2e-fixture', 'completed', now - METRIC_AUDIO_MS, now, METRIC_AUDIO_MS,
        audioRel, 'wav', 44 + dataBytes, METRIC_SEGMENTS, now, METRIC_AUDIO_MS, now, now);
    const insert = db.prepare(`INSERT INTO segments
      (session_id,seq,start_ms,end_ms,text,speaker_id,confidence,engine_id,is_final,edited,created_at)
      VALUES (?,?,?,?,?,'',0.9,'e2e-fixture',1,0,?)`);
    for (let seq = 0; seq < METRIC_SEGMENTS; seq += 1) {
      insert.run(SESSION_ID, seq, seq * METRIC_STRIDE_MS, (seq + 1) * METRIC_STRIDE_MS,
        `回放量化片段 ${seq + 1}`, now);
    }
    db.exec('COMMIT');
  } catch (error) {
    db.exec('ROLLBACK');
    throw error;
  } finally {
    db.close();
  }
};

export interface PlaybackMetric {
  startMs: number;
  endMs: number;
  wallMs: number;
  boundaries: Array<{ seq: number; errorMs: number }>;
  missing: number[];
  failures: string[];
  maxErrorMs: number;
  p95ErrorMs: number;
  maxFrameGapMs: number;
  seekingEvents: number;
  delayMs: number;
}

// 同一个判据用于正测和负对照；绝不以 p95 代替 PRD 的单次 80ms 上限。
export const assertPlaybackMetric = (metric: PlaybackMetric): void => {
  expect(metric.boundaries.length, '必须真的观察到跨句提交，不能拿零样本通过').toBeGreaterThanOrEqual(5);
  expect(metric.missing, '每个完整播放的边界都必须有对应 DOM 高亮提交').toEqual([]);
  expect(metric.maxErrorMs, 'FR-04-2：每次 DOM 高亮提交与真实媒体边界的误差 ≤80ms').toBeLessThanOrEqual(LIMIT_MS);
  expect(metric.failures, '播放不能 seek、倍速、停滞、隐藏或丢失当前高亮').toEqual([]);
  expect(metric.seekingEvents, '测量窗内不能用 seek 代替真实播放').toBe(0);
  expect(metric.endMs - metric.startMs, '真实媒体时钟必须推进').toBeGreaterThanOrEqual(4000);
  expect(metric.wallMs, '1x 播放不能由加速媒体时钟冒充').toBeGreaterThanOrEqual(metric.endMs - metric.startMs - 250);
};

export const openPlaybackMetric = async (page: Page, base?: string): Promise<void> => {
  if (base !== undefined) await page.goto(`${base}/`);
  await expect(page.locator('#startup-skeleton')).toHaveCount(0, { timeout: 10_000 });
  await page.getByRole('button', { name: '会话库' }).first().click();
  const row = page.getByText(METRIC_TITLE, { exact: true })
    .locator('xpath=ancestor::div[.//button[@aria-label="回放"]][1]');
  await expect(row).toHaveCount(1);
  await row.locator('[aria-label="回放"]').click();
  await expect(page.locator('[data-playback-total]')).toHaveAttribute('data-playback-total', String(METRIC_SEGMENTS));
  await expect.poll(() => page.locator('audio').first().evaluate((a: HTMLAudioElement) => a.readyState),
    { timeout: 20_000 }).toBeGreaterThanOrEqual(1);
  expect(await page.locator('audio').first().evaluate((a: HTMLAudioElement) => a.duration)).toBe(3600);
};

// seek 只用于短窗口的准备，且必须在安装 meter 之前完成。full 模式仅从 0 开始，
// 中途禁止 seek；既不预加载全部页，也不关闭虚拟列表/自动居中。
export const preparePlaybackWindow = async (page: Page, startMs: number): Promise<void> => {
  await page.locator('audio').first().evaluate(async (audio: HTMLAudioElement, target: number) => {
    audio.pause();
    audio.muted = true;
    audio.playbackRate = 1;
    if (Math.abs(audio.currentTime * 1000 - target) < 1) return;
    await new Promise<void>((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('量化窗口准备 seek 超时')), 15_000);
      audio.addEventListener('seeked', () => { clearTimeout(timeout); resolve(); }, { once: true });
      audio.currentTime = target / 1000;
    });
  }, startMs);
  await expect(page.locator(`[data-playback-row="${Math.floor(startMs / METRIC_STRIDE_MS)}"]`))
    .toHaveAttribute('aria-current', 'true', { timeout: 20_000 });
};

export const measurePlaybackWindow = async (
  page: Page, stopMs: number, delayMs = 0,
): Promise<PlaybackMetric> => {
  // 页面内 MutationObserver 记录 React 真正提交 aria-current 的时刻，避免 Playwright
  // 轮询/跨进程往返被误算成产品延迟。rAF 另行检查冻结/缺失/重复高亮和采样盲区。
  const pending = page.evaluate(async ({ stop, stride, count, injectedDelay }): Promise<PlaybackMetric> => {
    const audio = document.querySelector('audio') as HTMLAudioElement | null;
    const list = document.querySelector('[data-playback-total]');
    if (audio === null || list === null) throw new Error('量化前必须先打开回放抽屉');
    const descriptor = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'currentTime');
    if (descriptor?.get === undefined || descriptor.set === undefined) throw new Error('缺少原生媒体时钟');
    const nativeGet = descriptor.get;
    const nativeSet = descriptor.set;
    const readMs = (): number => nativeGet.call(audio) * 1000;
    const priorDescriptor = Object.getOwnPropertyDescriptor(audio, 'currentTime');
    // 负对照：只让产品读到落后 160ms 的媒体位置；meter 始终读取原生 currentTime。
    // 不修改夹具边界、测量值或阈值，因此同一条 ≤80ms 断言必须真正报红。
    if (injectedDelay > 0) {
      Object.defineProperty(audio, 'currentTime', {
        configurable: true,
        get: () => Math.max(0, nativeGet.call(audio) - injectedDelay / 1000),
        set: (value: number) => nativeSet.call(audio, value),
      });
    }
    return await new Promise<PlaybackMetric>((resolve) => {
      const start = readMs();
      const transitions = new Map<number, number>();
      const failures: string[] = [];
      let seekingEvents = 0;
      let frame = 0;
      let lastFrame = 0;
      let maxFrameGapMs = 0;
      let startedAt = 0;
      let finished = false;
      const fail = (message: string): void => { if (failures.length < 20) failures.push(message); };
      const highlighted = (): number[] => Array.from(list.querySelectorAll('[data-playback-row][aria-current="true"]'))
        .map((el) => Number(el.getAttribute('data-playback-row')));
      const record = (): void => {
        if (startedAt === 0 || finished) return;
        const now = readMs();
        const active = highlighted();
        if (active.length > 1) fail(`同时高亮 ${active.join(',')} @${now.toFixed(2)}`);
        for (const seq of active) {
          const boundary = seq * stride;
          if (boundary > start + 1 && boundary <= stop && !transitions.has(seq)) {
            transitions.set(seq, now - boundary);
          }
        }
      };
      const observer = new MutationObserver(record);
      observer.observe(list, { attributes: true, attributeFilter: ['aria-current'], childList: true, subtree: true });
      const onSeeking = (): void => { seekingEvents += 1; };
      const onRateChange = (): void => { if (audio.playbackRate !== 1) fail('播放倍速不是 1x'); };
      const onVisibility = (): void => { if (document.hidden) fail('页面进入后台，不能作为前台 80ms 证据'); };
      const onError = (): void => { fail('媒体加载/解码失败'); finish(); };
      const onEnded = (): void => {
        if (readMs() < stop - 1) fail('媒体提前结束');
        finish();
      };
      const finish = (): void => {
        if (finished) return;
        record();
        finished = true;
        clearTimeout(watchdog);
        cancelAnimationFrame(frame);
        observer.disconnect();
        audio.removeEventListener('playing', onPlaying);
        audio.removeEventListener('seeking', onSeeking);
        audio.removeEventListener('ratechange', onRateChange);
        audio.removeEventListener('ended', onEnded);
        audio.removeEventListener('error', onError);
        document.removeEventListener('visibilitychange', onVisibility);
        const end = readMs();
        const wallMs = startedAt === 0 ? 0 : performance.now() - startedAt;
        audio.pause();
        if (priorDescriptor === undefined) Reflect.deleteProperty(audio, 'currentTime');
        else Object.defineProperty(audio, 'currentTime', priorDescriptor);
        // 末尾不足 80ms 的边界没有完整验收窗口，不算 missing；完整长测仍涵盖 1..4999。
        const lastSeq = Math.min(count - 1, Math.floor((Math.min(end, stop) - 80) / stride));
        const missing: number[] = [];
        for (let seq = Math.floor(start / stride) + 1; seq <= lastSeq; seq += 1) {
          if (!transitions.has(seq)) missing.push(seq);
        }
        const boundaries = Array.from(transitions, ([seq, errorMs]) => ({ seq, errorMs }));
        const errors = boundaries.map((b) => Math.abs(b.errorMs)).sort((a, b) => a - b);
        resolve({ startMs: start, endMs: end, wallMs, boundaries, missing, failures,
          maxErrorMs: errors.at(-1) ?? Infinity,
          p95ErrorMs: errors[Math.max(0, Math.ceil(errors.length * 0.95) - 1)] ?? Infinity,
          maxFrameGapMs, seekingEvents, delayMs: injectedDelay });
      };
      const tick = (stamp: number): void => {
        if (finished) return;
        if (lastFrame > 0) maxFrameGapMs = Math.max(maxFrameGapMs, stamp - lastFrame);
        lastFrame = stamp;
        const now = readMs();
        const expected = Math.min(count - 1, Math.floor(now / stride));
        const active = highlighted();
        // 首边界不纳入切换误差，之后不允许任何旧句/空白持续超过 80ms。
        if (expected * stride > start + 1 && now - expected * stride > 80 &&
          (active.length !== 1 || active[0] !== expected)) {
          fail(`媒体 ${now.toFixed(2)}ms 应高亮 ${expected}，实际 ${active.join(',') || '空白'}`);
        }
        if (now >= stop) { finish(); return; }
        frame = requestAnimationFrame(tick);
      };
      const onPlaying = (): void => {
        if (startedAt !== 0) return;
        startedAt = performance.now();
        if (document.hidden) fail('开始播放时页面不可见');
        onRateChange();
        frame = requestAnimationFrame(tick);
      };
      const watchdog = setTimeout(() => { fail('真实播放未在音频时长 +30s 内完成'); finish(); }, stop - start + 30_000);
      audio.addEventListener('playing', onPlaying);
      audio.addEventListener('seeking', onSeeking);
      audio.addEventListener('ratechange', onRateChange);
      audio.addEventListener('ended', onEnded);
      audio.addEventListener('error', onError);
      document.addEventListener('visibilitychange', onVisibility);
      // meter 就位后才经真实 UI 触发播放（不是直接 audio.play()）。
      const button = document.querySelector('button[aria-label="播放"]') as HTMLButtonElement | null;
      if (button === null) { fail('缺少真实播放按钮'); finish(); return; }
      button.click();
    });
  }, { stop: stopMs, stride: METRIC_STRIDE_MS, count: METRIC_SEGMENTS, injectedDelay: delayMs });
  return await pending;
};
