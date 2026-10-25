// npm run verify            quick matrix, prints findings, exits 1 on any
// npm run verify:full       the whole matrix (several minutes)
import { runMatrix, out } from "./runner.mjs";
import { runTransitions } from "./transitions.mjs";
const started = Date.now();
let done = 0;
const report = await runMatrix({ onCell: () => { if (++done % 25 === 0) process.stdout.write(`\r${done} cells`); } });
process.stdout.write(`\r${report.cells} cells in ${Math.round((Date.now() - started) / 1000)}s\n`);
// A job that changes state under an open dialog: nothing jumps, nothing goes stale.
const moves = await runTransitions();
if (moves.length) {
  console.log(`\n${moves.length} findings while a job changes state under the open dialog:`);
  const seen = new Set();
  for (const v of moves) {
    const key = `${v.journey}|${v.step}|${v.rule}|${v.message}`;
    if (seen.has(key)) continue;
    seen.add(key);
    console.log(`  ${v.rule}  ${v.step}  (${v.screen})  ${v.message}`);
  }
}
if (report.failed) {
  console.log(`\n${report.failed} cells have findings. Summary: ${out}report.md · pictures: ${out}index.html\n`);
  const byRule = new Map();
  for (const v of report.violations) byRule.set(v.rule, (byRule.get(v.rule) || 0) + 1);
  for (const [rule, n] of [...byRule].sort((a, b) => b[1] - a[1])) console.log(`  ${String(n).padStart(5)}  ${rule}`);
}
if (report.failed || moves.length) process.exit(1);
console.log("All cells pass.");
