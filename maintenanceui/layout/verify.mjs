// npm run verify            quick matrix, prints findings, exits 1 on any
// npm run verify:full       the whole matrix (several minutes)
import { runMatrix, out } from "./runner.mjs";
import { runTransitions } from "./transitions.mjs";
import { runStorage } from "./storage.mjs";
import { runReview } from "./review.mjs";
import { runSwipe } from "./swipe.mjs";
import { runSheet } from "./sheet.mjs";
import { runControls } from "./controls.mjs";
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
// The Stats tab's storage section, on every screen and theme.
const stats = await runStorage();
if (stats.length) {
  console.log(`\n${stats.length} findings in the Stats storage section:`);
  const seen = new Set();
  for (const v of stats) {
    const key = `${v.scenario}|${v.rule}|${v.message}|${v.where}`;
    if (seen.has(key)) continue;
    seen.add(key);
    console.log(`  ${v.rule}  ${v.scenario}  (${v.screen}, ${v.theme}, x${v.zoom})  ${v.message}  ${v.where}`);
  }
}
if (report.failed) {
  console.log(`\n${report.failed} cells have findings. Summary: ${out}report.md · pictures: ${out}index.html\n`);
  const byRule = new Map();
  for (const v of report.violations) byRule.set(v.rule, (byRule.get(v.rule) || 0) + 1);
  for (const [rule, n] of [...byRule].sort((a, b) => b[1] - a[1])) console.log(`  ${String(n).padStart(5)}  ${rule}`);
}
// The replacement review dialog, opened the way a person opens it.
const reviews = await runReview();
if (reviews.length) {
  console.log(`\n${reviews.length} findings in the replacement review dialog:`);
  const seen = new Set();
  for (const v of reviews) {
    const key = `${v.scenario}|${v.rule}|${v.message}|${v.where}`;
    if (seen.has(key)) continue;
    seen.add(key);
    console.log(`  ${v.rule}  ${v.scenario}  (${v.screen}, ${v.theme}, x${v.zoom})  ${v.message}  ${v.where}`);
  }
}
// The queue's swipe-to-reveal actions, driven with real touches on every touch screen.
const swipes = await runSwipe();
if (swipes.length) {
  console.log(`\n${swipes.length} findings in the queue's swipe actions:`);
  const seen = new Set();
  for (const v of swipes) {
    const key = `${v.rule}|${v.message}|${v.where}|${v.screen}`;
    if (seen.has(key)) continue;
    seen.add(key);
    console.log(`  ${v.rule}  (${v.screen}, ${v.theme})  ${v.message}  ${v.where}`);
  }
}
// Sheets close with a drag down, driven with real touches on every touch screen.
const sheets = await runSheet();
if (sheets.length) {
  console.log(`\n${sheets.length} findings in the sheets' drag to close:`);
  const seen = new Set();
  for (const v of sheets) {
    const key = `${v.rule}|${v.message}|${v.where}|${v.screen}`;
    if (seen.has(key)) continue;
    seen.add(key);
    console.log(`  ${v.rule}  (${v.screen}, ${v.theme})  ${v.message}  ${v.where}`);
  }
}
// One look for every control: height, corner, type, field edges and icons, in every view and dialog.
const controls = await runControls();
if (controls.length) {
  console.log(`\n${controls.length} findings in the controls' shared look:`);
  const seen = new Set();
  for (const v of controls) {
    const key = `${v.rule}|${v.message}|${v.where}|${v.screen}`;
    if (seen.has(key)) continue;
    seen.add(key);
    console.log(`  ${v.rule}  (${v.screen}, ${v.theme}, ${v.view})  ${v.message}  ${v.where}`);
  }
}
if (report.failed || moves.length || stats.length || reviews.length || swipes.length || sheets.length || controls.length) process.exit(1);
console.log("All cells pass.");
