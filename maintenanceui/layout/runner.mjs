// Runs the job detail through a matrix of states × screens × themes × zoom ×
// text shapes and applies every rule in invariants.mjs to each cell.
//
//   LAYOUT_PROFILE=quick|full    how much of the matrix to run (default quick)
//   LAYOUT_BROWSER=chrome        use the installed Chrome instead of Playwright's
//   LAYOUT_ONLY=batch-replacing  only scenarios whose id contains this
//   LAYOUT_GALLERY=1             also keep a screenshot of every scenario
import { chromium } from "playwright";
import { mkdir, writeFile, rm } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { startServer } from "./server.mjs";
import { scenarios, world, nameVariants, sizeVariants } from "./fixtures.mjs";
import { budgets, install } from "./invariants.mjs";

export const out = fileURLToPath(new URL("./out/", import.meta.url));

export const screens = {
  "phone-small": { width: 320, height: 568, mobile: true },
  "phone-android": { width: 360, height: 740, mobile: true },
  "phone": { width: 390, height: 844, mobile: true },
  "phone-large": { width: 430, height: 932, mobile: true },
  "phone-landscape": { width: 844, height: 390, mobile: true },
  "tablet": { width: 768, height: 1024, mobile: true },
  "desktop": { width: 1280, height: 800, mobile: false },
};

// A cell is one combination. `quick` samples the corners of the matrix so every
// screen, theme, zoom and text shape appears, without the full product.
export function cells(profile = "quick", only = "") {
  const list = scenarios.filter(s => !only || s.id.includes(only));
  const result = [];
  if (profile === "full") {
    for (const scenario of list)
      for (const name of Object.keys(nameVariants))
        for (const screen of Object.keys(screens))
          for (const [theme, zoom] of [["light", 1], ["dark", 1], ["light", 1.5], ["dark", 1.5]])
            result.push({ scenario, name, size: "normal", screen, theme, zoom, act: "" });
    for (const scenario of list)
      for (const act of ["more", "expand", "pending"])
        for (const screen of ["phone-small", "phone", "tablet"])
          result.push({ scenario, name: "long", size: "normal", screen, theme: "light", zoom: 1, act });
    for (const scenario of list.filter(s => ["file-encoding", "file-candidate", "batch-candidates", "batch-replacing-mid"].includes(s.id)))
      for (const size of ["huge", "tiny", "larger"])
        for (const screen of ["phone-small", "phone", "desktop"])
          result.push({ scenario, name: "short", size, screen, theme: "light", zoom: 1, act: "" });
    return result;
  }
  const picks = [
    ["phone", "short", "light", 1], ["phone", "long", "dark", 1], ["phone-small", "unbroken", "light", 1.5],
    ["phone-landscape", "short", "dark", 1], ["phone-android", "cjk", "light", 1], ["tablet", "long", "light", 1], ["desktop", "unbroken", "dark", 1],
  ];
  for (const scenario of list)
    for (const [screen, name, theme, zoom] of picks)
      result.push({ scenario, name, size: "normal", screen, theme, zoom, act: "" });
  // The states a person reaches by touching: the More menu open, files open, a
  // button waiting on the server.
  const acts = [["phone", "long", "light", 1, "more"], ["phone-small", "unbroken", "dark", 1.5, "expand"], ["phone", "short", "light", 1, "pending"]];
  for (const scenario of list)
    for (const [screen, name, theme, zoom, act] of acts)
      result.push({ scenario, name, size: "normal", screen, theme, zoom, act });
  return result;
}

export const cellName = c => `${c.scenario.id}__${c.screen}__${c.name}__${c.size}__${c.theme}__x${c.zoom}${c.act ? `__${c.act}` : ""}`;

// Puts the dialog in a state only a touch reaches. Returns false when the
// scenario has nothing to touch, so that cell is just the plain one.
async function act(page, what) {
  if (what === "more") {
    const toggle = page.locator("#detail-controls .more-actions > summary").first();
    if (!await toggle.count()) return false;
    await toggle.click();
  } else if (what === "expand") {
    const heads = page.locator("#job-detail [data-file-toggle]");
    const n = Math.min(await heads.count(), 3);
    if (!n) return false;
    for (let i = 0; i < n; i++) await heads.nth(i).click();
  } else if (what === "pending") {
    // What button() does while its request is out: busy, disabled, a spinner.
    const applied = await page.evaluate(() => {
      const target = [...document.querySelectorAll("#detail-controls button")].find(b => b.offsetParent);
      if (!target) return false;
      const spinner = document.createElement("span");
      spinner.className = "spinner";
      spinner.dataset.spinner = "";
      spinner.setAttribute("aria-hidden", "true");
      target.disabled = true;
      target.setAttribute("aria-busy", "true");
      target.classList.add("button-pending");
      target.prepend(spinner);
      return true;
    });
    if (!applied) return false;
  }
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
  return true;
}

async function open(page, id) {
  // The console routes through history. A navigation that lands while the
  // previous one is still applying is dropped, so ask again until it opens.
  for (let attempt = 1; ; attempt++) {
    await page.evaluate(i => { history.pushState({}, "", `/?view=queue&job=${encodeURIComponent(i)}`); dispatchEvent(new PopStateEvent("popstate")); }, id);
    try {
      await page.waitForFunction(i => document.querySelector("#job-detail")?.open && document.querySelector("#detail-summary")?.dataset.operationId === i && !/Loading job|no longer available/.test(document.querySelector("#detail-summary")?.textContent || ""), id, { timeout: 4000 });
      break;
    } catch (error) {
      if (attempt === 3) throw error;
    }
  }
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
}

export async function runMatrix({ profile = process.env.LAYOUT_PROFILE || "quick", only = process.env.LAYOUT_ONLY || "", gallery = process.env.LAYOUT_GALLERY === "1", workers = Number(process.env.LAYOUT_WORKERS || 3), onCell } = {}) {
  const all = cells(profile, only);
  await rm(out, { recursive: true, force: true });
  await mkdir(out, { recursive: true });
  const live = { jobs: [], items: {} };
  const server = await startServer(live);
  const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
  const rules = install();
  const report = { profile, cells: all.length, failed: 0, retried: 0, acts: {}, violations: [], errors: [] };
  // One context per screen shape: the viewport, touch and theme are fixed for
  // the context, the scenarios swap inside it.
  const groups = new Map();
  for (const cell of all) {
    const key = `${cell.screen}|${cell.theme}`;
    (groups.get(key) || groups.set(key, []).get(key)).push(cell);
  }
  const queue = [...groups.entries()];
  async function work() {
    for (let entry = queue.shift(); entry; entry = queue.shift()) {
      const [key, group] = entry;
      const [screenName, theme] = key.split("|");
      const screen = screens[screenName];
      const context = await browser.newContext({
        viewport: { width: screen.width, height: screen.height }, deviceScaleFactor: 2, isMobile: screen.mobile, hasTouch: screen.mobile,
        colorScheme: theme, serviceWorkers: "block", reducedMotion: "reduce",
      });
      // Each context has its own copy of the world, so contexts can run side by side.
      const state = { jobs: [], items: {} };
      const own = await startServer(state);
      // The scenarios are dated against one fixed moment, so "2 s ago" stays 2 s ago.
      await context.clock.setFixedTime(new Date(Date.UTC(2026, 9, 25, 12, 0, 0)));
      const page = await context.newPage();
      let errors = [];
      page.on("pageerror", e => errors.push(`pageerror: ${e.message}`));
      page.on("console", m => m.type() === "error" && !/favicon|Failed to load resource/.test(m.text()) && errors.push(`console: ${m.text()}`));
      await page.goto(`${own.origin}/?view=queue`);
      await page.waitForSelector("#workspace:not([hidden])", { timeout: 10000 });
      for (const cell of group) {
        const w = world(cell.scenario, cell.name, cell.size);
        state.jobs = w.jobs; state.items = w.items;
        errors = [];
        const found = [];
        // A cell starts from the closed queue, so a poll of the previous job
        // never answers for this one. If the page still fails to load the job,
        // one fresh page gets a second go; the retries are counted in the report.
        for (let attempt = 1; attempt <= 2; attempt++) {
          found.length = 0;
          errors = [];
          try {
            await page.evaluate(() => { document.querySelector("#job-detail")?.open && document.querySelector("#job-detail").close(); history.pushState({}, "", "/?view=queue"); dispatchEvent(new PopStateEvent("popstate")); });
            // Browser zoom shrinks the viewport in CSS pixels: 150% on a 320px phone
            // lays the page out at 213px. Emulate that, not a scaled page.
            await page.setViewportSize({ width: Math.round(screen.width / cell.zoom), height: Math.round(screen.height / cell.zoom) });
            await open(page, w.id);
            if (cell.act) { const applied = await act(page, cell.act); report.acts[cell.act] = (report.acts[cell.act] || 0) + (applied ? 1 : 0); }
            for (const where of ["top", "bottom"]) {
              await page.evaluate(position => { const d = document.querySelector("#job-detail"); d.scrollTop = position === "top" ? 0 : d.scrollHeight; }, where);
              await page.evaluate(() => new Promise(r => requestAnimationFrame(r)));
              const found_ = await page.evaluate(`(${rules.toString()})("#job-detail", ${JSON.stringify(budgets)})`);
              for (const v of found_) if (!found.some(f => f.rule === v.rule && f.where === v.where && f.message === v.message)) found.push({ ...v, scroll: where });
            }
          } catch (error) {
            found.push({ rule: "render", message: error.message.split("\n")[0], where: "" });
          }
          if (!found.some(f => f.rule === "render") || attempt === 2) break;
          report.retried++;
          await page.goto(`${own.origin}/?view=queue`);
          await page.waitForSelector("#workspace:not([hidden])", { timeout: 10000 });
        }
        for (const e of errors) found.push({ rule: "js-error", message: e, where: "" });
        const name = cellName(cell);
        if (found.length || gallery) {
          await page.evaluate(() => { document.querySelector("#job-detail").scrollTop = 0; });
          await page.screenshot({ path: `${out}${name}.png` }).catch(() => {});
        }
        if (found.length) {
          report.failed++;
          for (const v of found) report.violations.push({ cell: name, scenario: cell.scenario.id, screen: cell.screen, name: cell.name, size: cell.size, theme: cell.theme, zoom: cell.zoom, ...v });
        }
        onCell?.(cell, found);
      }
      await context.close();
      await own.close();
    }
  }
  await Promise.all(Array.from({ length: Math.max(1, Math.min(workers, queue.length)) }, work));
  // A touch state that applied to no cell at all tested nothing: the menu or the
  // toggles it needs have disappeared.
  for (const what of new Set(all.map(c => c.act).filter(Boolean)))
    if (!report.acts[what]) {
      report.failed++;
      report.violations.push({ cell: `(coverage) ${what}`, scenario: "", screen: "", name: "", size: "", theme: "", zoom: 1, rule: "coverage", message: `no scenario had anything to "${what}"`, where: "" });
    }
  await browser.close();
  await server.close();
  await writeReport(report);
  return report;
}

function tally(violations, by) {
  const map = new Map();
  for (const v of violations) map.set(by(v), (map.get(by(v)) || 0) + 1);
  return [...map.entries()].sort((a, b) => b[1] - a[1]);
}

async function writeReport(report) {
  await writeFile(`${out}report.json`, JSON.stringify(report, null, 1));
  const lines = [`# Layout report (${report.profile})`, "", `${report.failed} of ${report.cells} cells have violations (${report.violations.length} findings); ${report.retried} cells needed a second load. Touch states applied: ${Object.entries(report.acts).map(([k, n]) => `${k} ${n}`).join(", ") || "none"}.`, ""];
  lines.push("## By rule", ...tally(report.violations, v => v.rule).map(([k, n]) => `- ${k}: ${n}`), "");
  lines.push("## By scenario", ...tally(report.violations, v => v.scenario).map(([k, n]) => `- ${k}: ${n}`), "");
  lines.push("## By screen", ...tally(report.violations, v => v.screen).map(([k, n]) => `- ${k}: ${n}`), "");
  const seen = new Set();
  lines.push("## Distinct findings", "");
  for (const v of report.violations) {
    const key = `${v.rule}|${v.where}|${v.message}`.replace(/\d+(\.\d+)?/g, "#");
    if (seen.has(key)) continue;
    seen.add(key);
    lines.push(`- **${v.rule}** ${v.where} — ${v.message} _(e.g. ${v.cell})_`);
  }
  await writeFile(`${out}report.md`, lines.join("\n"));
  const rows = report.violations.reduce((acc, v) => ((acc[v.cell] ||= []).push(v), acc), {});
  const html = ["<meta charset=utf-8><style>body{font:13px system-ui;margin:16px}figure{display:inline-block;vertical-align:top;margin:8px;width:300px}img{width:300px;border:1px solid #888}li{margin:2px 0}</style><h1>Layout findings</h1>"];
  for (const [cell, list] of Object.entries(rows).slice(0, 400)) html.push(`<figure><img src="${cell}.png" loading=lazy><figcaption><b>${cell}</b><ul>${list.slice(0, 6).map(v => `<li>${v.rule}: ${v.message.replace(/</g, "&lt;")}</li>`).join("")}</ul></figcaption></figure>`);
  await writeFile(`${out}index.html`, html.join("\n"));
}
