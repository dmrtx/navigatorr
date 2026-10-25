// npm run verify            quick matrix, prints findings, exits 1 on any
// npm run verify:full       the whole matrix (several minutes)
import { runMatrix, out } from "./runner.mjs";
const started = Date.now();
let done = 0;
const report = await runMatrix({ onCell: () => { if (++done % 25 === 0) process.stdout.write(`\r${done} cells`); } });
process.stdout.write(`\r${report.cells} cells in ${Math.round((Date.now() - started) / 1000)}s\n`);
if (report.failed) {
  console.log(`\n${report.failed} cells have findings. Summary: ${out}report.md · pictures: ${out}index.html\n`);
  const byRule = new Map();
  for (const v of report.violations) byRule.set(v.rule, (byRule.get(v.rule) || 0) + 1);
  for (const [rule, n] of [...byRule].sort((a, b) => b[1] - a[1])) console.log(`  ${String(n).padStart(5)}  ${rule}`);
  process.exit(1);
}
console.log("All cells pass.");
