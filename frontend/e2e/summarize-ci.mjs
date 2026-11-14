import fs from 'node:fs';

// CI 用：把 Playwright JSON 报告的通过/失败/跳过汇总写入 GitHub Job Summary，
// 并在「跳过数超出预期」或「存在失败」时以非零退出码告警。
//
// 为什么需要它：本项目的 E2E 里有**设计性 skip**（需要模型包 / 原生对话框 / 真实进程），
// 这些是合理的、永久不会执行的用例。若只把「进程退出码」当作判据，一旦某个真实用例
// 因环境问题被 skip（例如以前的「缺语音素材就 skip」），流水线依然全绿 ——
// 那就等于把「没测」当成「通过」。所以这里把 skip 数显式对账。
const EXPECTED_SKIPPED = Number(process.env.E2E_EXPECTED_SKIPPED ?? '5');
// Playwright 的 json reporter 由 PLAYWRIGHT_JSON_OUTPUT_NAME 决定输出文件；
// 这里默认值与 CI 里设置的一致，本地手动跑也能直接用。
const REPORT = process.env.PLAYWRIGHT_JSON_OUTPUT_NAME ?? 'e2e-report.json';

const readReport = () => {
  if (!fs.existsSync(REPORT)) {
    throw new Error(`找不到 Playwright JSON 报告：${REPORT}`);
  }
  return JSON.parse(fs.readFileSync(REPORT, 'utf8'));
};

// 递归遍历 spec 树，收集每个用例的最终状态。
const collect = (suite, out) => {
  for (const spec of suite.specs ?? []) {
    for (const t of spec.tests ?? []) {
      const results = t.results ?? [];
      const last = results[results.length - 1];
      out.push({
        title: spec.title,
        file: spec.file,
        status: last?.status ?? 'unknown',
        // skip 的原因写在用例的 annotations 里（Playwright 的 skip 说明也走这里）
        annotations: (t.annotations ?? []).map((a) => a.description ?? a.type).filter(Boolean),
      });
    }
  }
  for (const child of suite.suites ?? []) collect(child, out);
};

const main = () => {
  const report = readReport();
  const cases = [];
  for (const suite of report.suites ?? []) collect(suite, cases);

  const passed = cases.filter((c) => c.status === 'passed');
  const failed = cases.filter((c) => c.status === 'failed' || c.status === 'timedOut');
  const skipped = cases.filter((c) => c.status === 'skipped');

  const lines = [];
  lines.push('## E2E 结果（Wails server 模式 + Playwright）');
  lines.push('');
  lines.push('| 状态 | 数量 |');
  lines.push('|------|------|');
  lines.push(`| 通过 | ${passed.length} |`);
  lines.push(`| 失败 | ${failed.length} |`);
  lines.push(`| 跳过 | ${skipped.length}（预期 ${EXPECTED_SKIPPED}） |`);
  lines.push('');

  if (skipped.length > 0) {
    lines.push('### 跳过的用例（确认均为设计性跳过）');
    lines.push('');
    for (const s of skipped) {
      const why = s.annotations.length > 0 ? s.annotations.join('；') : '（用例未标注原因）';
      lines.push(`- \`${s.file}\` — ${s.title} — ${why}`);
    }
    lines.push('');
  }

  if (failed.length > 0) {
    lines.push('### 失败用例');
    lines.push('');
    for (const f of failed) lines.push(`- \`${f.file}\` — ${f.title}（${f.status}）`);
    lines.push('');
  }

  const summary = lines.join('\n');
  console.log(summary);

  const summaryFile = process.env.GITHUB_STEP_SUMMARY;
  if (summaryFile) fs.appendFileSync(summaryFile, summary + '\n');

  const problems = [];
  if (failed.length > 0) problems.push(`存在 ${failed.length} 个失败用例`);
  if (skipped.length > EXPECTED_SKIPPED) {
    problems.push(
      `跳过数 ${skipped.length} 超过预期 ${EXPECTED_SKIPPED} —— 可能有真实用例因环境问题被静默跳过`,
    );
  }
  if (skipped.length < EXPECTED_SKIPPED) {
    problems.push(`跳过数 ${skipped.length} 少于预期 ${EXPECTED_SKIPPED} —— 预期跳过的用例消失，请核对`);
  }

  if (problems.length > 0) {
    for (const p of problems) console.error(`::error::${p}`);
    process.exitCode = 1;
  }
};

try {
  main();
} catch (err) {
  console.error(`::error::E2E 汇总失败：${err instanceof Error ? err.message : String(err)}`);
  process.exitCode = 1;
}
