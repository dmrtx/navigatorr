// A job changes state while its dialog is open. The detail has to redraw the
// same blocks in place: the header and the action bar stay where they were, the
// sections stay in order, and every rule in invariants.mjs holds after each
// step. Each journey is the states one job passes through, in order.
import { chromium } from "playwright";
import { startServer } from "./server.mjs";
import { scenarios, world } from "./fixtures.mjs";
import { screens } from "./runner.mjs";
import { budgets, install } from "./invariants.mjs";

export const journeys = [
  ["file-queued", "file-analysing", "file-encoding", "file-validating", "file-candidate", "file-replacing", "file-replaced"],
  ["file-encoding", "file-failed", "file-queued", "file-encoding", "file-cancelled"],
  ["file-decision", "file-candidate-larger", "file-not-converted"],
  ["batch-queued", "batch-running", "batch-running-decision", "batch-running-failures", "batch-candidates", "batch-mixed-candidates"],
  ["batch-candidates", "batch-replacing-start", "batch-replacing-mid", "batch-replacing-failure", "batch-replacing-done"],
  ["batch-replacing-mid", "batch-replacing-stopped", "batch-partially-replaced"],
];

// Phones are a full-screen sheet, so the header and bar have one fixed place.
const phones = ["phone-small", "phone", "phone-large"];
const said = () => (document.querySelector("#detail-status")?.innerText || "") + "|" + (document.querySelector("#detail-summary")?.innerText || "") + "|" + (document.querySelector("#detail-controls")?.innerText || "");
const edges = () => {
  const box = selector => { const e = document.querySelector(selector); if (!e || e.hidden) return null; const r = e.getBoundingClientRect(); return { top: Math.round(r.top), bottom: Math.round(r.bottom), height: Math.round(r.height) }; };
  return { dialog: box("#job-detail"), header: box("#job-detail [data-section=header]"), bar: box("#job-detail [data-section=actions]") };
};

export async function runTransitions({ browserChannel = process.env.LAYOUT_BROWSER, onStep } = {}) {
  const byId = new Map(scenarios.map(s => [s.id, s]));
  const rules = install();
  const found = [];
  const browser = await chromium.launch(browserChannel ? { channel: browserChannel } : {});
  for (const screenName of phones) {
    const screen = screens[screenName];
    const context = await browser.newContext({ viewport: { width: screen.width, height: screen.height }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, serviceWorkers: "block", reducedMotion: "reduce" });
    await context.clock.setFixedTime(new Date(Date.UTC(2026, 9, 25, 12, 0, 0)));
    const state = { jobs: [], items: {} };
    const server = await startServer(state);
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", e => errors.push(`pageerror: ${e.message}`));
    await page.goto(`${server.origin}/?view=queue`);
    await page.waitForSelector("#workspace:not([hidden])", { timeout: 10000 });
    for (const journey of journeys) {
      let before = null, id = "";
      for (const [index, step] of journey.entries()) {
        const scenario = byId.get(step);
        if (!scenario) { found.push({ journey: journey.join(" > "), step, screen: screenName, rule: "journey", message: "unknown scenario" }); continue; }
        const w = world(scenario, "long", "normal");
        // The same job changes state: keep one id and one name through the journey.
        id ||= `journey-${journeys.indexOf(journey)}`;
        for (const job of w.jobs) { job.id = id; job.workflow_id = id; }
        state.jobs = w.jobs;
        state.items = Object.fromEntries(Object.values(w.items).map(items => [id, items]));
        if (index === 0) {
          await page.evaluate(i => { history.pushState({}, "", `/?view=queue&job=${i}`); dispatchEvent(new PopStateEvent("popstate")); }, id);
          await page.waitForFunction(i => document.querySelector("#detail-summary")?.dataset.operationId === i && !/Loading job/.test(document.querySelector("#detail-summary").textContent), id, { timeout: 5000 });
        } else {
          // What a poll does: the dialog is open, the server's answer has changed.
          const stamp = await page.evaluate(said);
          await page.evaluate(() => dispatchEvent(new Event("online")));
          // Wait for the dialog to change; if it never does, the check below says it is stale.
          await page.waitForFunction(`(${said.toString()})() !== ${JSON.stringify(stamp)}`, null, { timeout: 3000 }).catch(() => {});
          await page.waitForTimeout(150);
        }
        await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
        const here = await page.evaluate(edges);
        const problems = await page.evaluate(`(${rules.toString()})("#job-detail", ${JSON.stringify(budgets)})`);
        const where = { journey: journey.join(" > "), step: `${index + 1}. ${step}`, screen: screenName };
        for (const v of problems) found.push({ ...where, rule: v.rule, message: `${v.message}` });
        if (before) {
          // The title is the same, so the header does not move; the bar sits at the bottom of the same screen.
          if (before.header && here.header && (before.header.top !== here.header.top || before.header.height !== here.header.height))
            found.push({ ...where, rule: "header-jump", message: `header moved from ${before.header.top}+${before.header.height} to ${here.header.top}+${here.header.height}` });
          if (before.bar && here.bar && before.bar.bottom !== here.bar.bottom)
            found.push({ ...where, rule: "bar-jump", message: `action bar moved from ${before.bar.bottom} to ${here.bar.bottom}` });
          if (before.dialog && here.dialog && (before.dialog.top !== here.dialog.top || before.dialog.bottom !== here.dialog.bottom))
            found.push({ ...where, rule: "dialog-jump", message: `dialog resized from ${before.dialog.top}-${before.dialog.bottom} to ${here.dialog.top}-${here.dialog.bottom}` });
        }
        const text = await page.evaluate(said);
        // What the dialog says has to follow the job. If it says what it said
        // before, the refresh did not land and the screen is stale.
        if (before && text === before.text) found.push({ ...where, rule: "stale-detail", message: "the dialog still says what the previous state said" });
        before = { ...here, text };
        if (process.env.LAYOUT_TRACE) console.log(screenName, step, JSON.stringify(text.slice(0, 90)));
        onStep?.(where);
      }
      await page.evaluate(() => document.querySelector("#job-detail")?.close());
      await page.waitForTimeout(100);
    }
    for (const e of errors) found.push({ journey: "", step: "", screen: screenName, rule: "js-error", message: e });
    await context.close();
    await server.close();
  }
  await browser.close();
  return found;
}
