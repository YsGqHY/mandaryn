import { expect, test } from '@playwright/test';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { expectAudioPlayable, navTo, openAppAt } from './helpers';
import { startServer, stopServer, waitForServer, type ServerHandle } from './serverctl';

const modelSource = process.env.E2E_REAL_ASR_DIR;
const speechSource = process.env.E2E_REAL_SPEECH_WAV;
const referenceSource = process.env.E2E_REAL_ASR_REFERENCE;
const normalize = (value: string) => Array.from(value.normalize('NFKC').toLowerCase().replace(/[^\p{L}\p{N}]/gu, ''));

function compareTranscript(reference: string, actual: string) {
  const expected = normalize(reference);
  const got = normalize(actual);
  const rows = Array.from({ length: expected.length + 1 }, (_, i) => Array.from({ length: got.length + 1 }, (_, j) => i === 0 ? j : i));
  for (let i = 1; i <= expected.length; i++) {
    for (let j = 1; j <= got.length; j++) {
      rows[i][j] = Math.min(rows[i - 1][j] + 1, rows[i][j - 1] + 1, rows[i - 1][j - 1] + Number(expected[i - 1] !== got[j - 1]));
    }
  }
  let additions = 0, omissions = 0, substitutions = 0;
  let i = expected.length, j = got.length;
  while (i > 0 || j > 0) {
    if (i > 0 && j > 0 && rows[i][j] === rows[i - 1][j - 1] + Number(expected[i - 1] !== got[j - 1])) {
      if (expected[i - 1] !== got[j - 1]) substitutions++;
      i--; j--;
    } else if (i > 0 && rows[i][j] === rows[i - 1][j] + 1) {
      omissions++; i--;
    } else {
      additions++; j--;
    }
  }
  const edits = additions + omissions + substitutions;
  return { referenceCharacters: expected.length, actualCharacters: got.length, additions, omissions, substitutions, edits, cer: edits / expected.length };
}
const port = Number(process.env.E2E_REAL_ASR_PORT ?? 9375);
const sha256 = (bytes: Buffer): string => createHash('sha256').update(bytes).digest('hex');
const sessionId = 's_E2EREALASR0000000000001';

test('真实中文转写：解码失败后重试成功，片段可回放且不改写音频', async ({ page }) => {
  test.skip(process.platform !== 'win32', '当前真实本地引擎要求 Windows CGO');
  test.skip(modelSource === undefined || speechSource === undefined, '设置 E2E_REAL_ASR_DIR 和 E2E_REAL_SPEECH_WAV 后跑真实模型');
  if (modelSource === undefined || speechSource === undefined) throw new Error('missing real ASR fixture');
  test.setTimeout(90_000);
  const speech = fs.readFileSync(speechSource);
  expect(speech.subarray(0, 4).toString()).toBe('RIFF');
  expect(speech.readUInt16LE(22)).toBe(1);
  expect(speech.readUInt32LE(24)).toBe(16000);
  expect(speech.readUInt16LE(34)).toBe(16);
  const durationMs = Math.round((speech.length - 44) / 32);
  expect(durationMs).toBeGreaterThan(1000);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mandaryn-real-asr-e2e-'));
  const dataDir = path.join(root, 'data');
  const modelRoot = path.join(root, 'model-root');
  const bundleDir = path.join(modelRoot, 'models', 'sensevoice-small@1.2.0');
  const audioRel = `sessions/${sessionId}/audio.wav`;
  const audio = path.join(dataDir, audioRel);
  const dbPath = path.join(dataDir, 'mandaryn.db');
  const fixtureError = Buffer.from('deliberately invalid WAV for retry E2E');
  let server: ServerHandle | null = null;
  try {
    fs.mkdirSync(bundleDir, { recursive: true });
    const files = ['model.int8.onnx', 'tokens.txt'].map(name => {
      const file = path.join(modelSource, name);
      fs.copyFileSync(file, path.join(bundleDir, name));
      const bytes = fs.readFileSync(file);
      return { path: name, size: bytes.length, sha256: sha256(bytes) };
    });
    fs.writeFileSync(path.join(bundleDir, 'manifest.json'), JSON.stringify({ bundleId: 'sensevoice-small', name: 'SenseVoice', version: '1.2.0', roles: ['offline'], files }));
    server = startServer({ port, dataDir, env: { MANDARYN_MODEL_ROOT: modelRoot, MANDARYN_OFFLINE: '1' } });
    await waitForServer(port, 20_000, server);
    fs.mkdirSync(path.dirname(audio), { recursive: true });
    fs.writeFileSync(audio, fixtureError);
    let db = new DatabaseSync(dbPath);
    try {
      const now = Date.now();
      db.prepare(`INSERT INTO sessions (id,title,source_id,source_name,source_kind,model_source,engine_id,status,
        degraded,degrade_reason,started_at,ended_at,duration_ms,audio_path,audio_format,size_bytes,segment_count,speaker_count,char_count,
        transcription_status,live_transcribed,transcribe_progress_ms,created_at,updated_at)
        VALUES (?,?,?,?,?,?,?,?,0,'',?,?,?,?,?,?,0,0,0,'none',0,0,?,?)`).run(sessionId, '真实语音转写夹具',
        'dev:real-asr', 'E2E', 'device', 'local', '', 'completed', now - durationMs, now, durationMs, audioRel, 'wav', speech.length, now, now);
    } finally { db.close(); }
    await openAppAt(page, `http://127.0.0.1:${port}`);
    await navTo(page, '会话库');
    await page.getByRole('button', { name: '发起转写' }).first().click();
    const retry = page.getByRole('button', { name: '重试转写' }).first();
    await expect(retry).toBeVisible({ timeout: 30_000 });
    expect(fs.readFileSync(audio)).toEqual(fixtureError);

    // Only the test replaces its deliberately corrupt bytes; the app may not rewrite either file.
    fs.writeFileSync(audio, speech);
    await retry.click();
    await expect(page.getByText('已转写', { exact: true }).first()).toBeVisible({ timeout: 45_000 });
    expect(sha256(fs.readFileSync(audio))).toBe(sha256(speech));
    db = new DatabaseSync(dbPath, { readOnly: true });
    try {
      const result = db.prepare('SELECT status,transcription_status,transcribe_engine_id FROM sessions WHERE id = ?').get(sessionId);
      expect(result?.status).toBe('completed');
      expect(result?.transcription_status).toBe('done');
      expect(String(result?.transcribe_engine_id)).toContain('sensevoice');
      const pieces = db.prepare('SELECT start_ms,end_ms,text,is_final FROM segments WHERE session_id = ? ORDER BY seq').all(sessionId);
      expect(pieces.length).toBeGreaterThan(0);
      for (let i = 0; i < pieces.length; i++) {
        const p = pieces[i];
        expect(Number(p.end_ms)).toBeGreaterThan(Number(p.start_ms));
        expect(Number(p.start_ms)).toBeGreaterThanOrEqual(i === 0 ? 0 : Number(pieces[i - 1].start_ms));
        expect(Number(p.is_final)).toBe(1);
        expect(String(p.text)).toMatch(/\p{Script=Han}/u);
      }
      if (referenceSource !== undefined) {
        const dialogue = fs.readFileSync(referenceSource, 'utf8').split(/^---/m)[0];
        const lines = Array.from(dialogue.matchAll(/^角色[A-Z]\s*[：:]\s*(.+)$/gm), (match) => match[1].trim());
        expect(lines.length, 'reference dialogue must contain speaker lines').toBeGreaterThan(0);
        const actualLines = pieces.map((p) => String(p.text));
        const metrics = compareTranscript(lines.join(''), actualLines.join(''));
        expect(metrics.referenceCharacters).toBeGreaterThan(0);
        // Only numeric data enters CI logs. A local opt-in report can include the text for inspection.
        console.info(`ASR_REFERENCE_METRICS ${JSON.stringify({ ...metrics, referenceLines: lines.length, actualSegments: actualLines.length })}`);
        const reportPath = process.env.E2E_REAL_ASR_REPORT;
        if (reportPath !== undefined) fs.writeFileSync(reportPath, JSON.stringify({ metrics, lines, actualLines }, null, 2), { flag: 'wx' });
      }
    } finally { db.close(); }
    await page.locator('[aria-label="回放"]').first().click();
    expect(await expectAudioPlayable(page, 0.03)).toBeGreaterThan(1);
  } finally {
    await stopServer(server);
    fs.rmSync(root, { recursive: true, force: true });
  }
});
