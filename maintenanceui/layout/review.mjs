// The replacement review dialog ("Replace files"), opened the way a person
// opens it: the batch in the queue, then Review candidates. It runs on every
// screen, in both themes, at 100% and 150% zoom, for one file and for many,
// for the choosing step and for the prepared plan. Hard rules:
//   - the dialog never scrolls as a whole; only the file list does
//   - the decision buttons are always on screen, below the list
//   - buttons are one line each, and the same height side by side
//   - nothing leaves the dialog sideways, and a row never runs away in height
import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";
import { startServer } from "./server.mjs";
import { screens, out } from "./runner.mjs";
import { scenarios, world as makeWorld } from "./fixtures.mjs";
import { buttonHeightTolerance, installButtonHeights } from "./button_rules.mjs";

const GB = 1e9;
const members = (n, long = false) => Array.from({ length: n }, (_, i) => ({
  item_key: `item-${i + 1}`, transcode_action_id: `act-${i + 1}`, episode: long ? `Trailer.Park.Boys.S11E${String(i + 1).padStart(2, "0")}.${"1080p.WEBRip.x264-".repeat(4)}MOROSE` : `Trailer.Park.Boys.S11E${String(i + 1).padStart(2, "0")}.1080p.WEBRip.x264-MOROSE`,
  original_path: `/volume1/Media/TvSeries/Trailer Park Boys/Season 11/Trailer.Park.Boys.S11E${String(i + 1).padStart(2, "0")}.mkv`, candidate_path: `/volume1/Media/.navigatorr/c${i + 1}.mkv`,
}));
const facts = (list) => Object.fromEntries(list.map((m, i) => [m.transcode_action_id, { source_bytes: (2.1 + (i % 5) * 0.17) * GB, candidate_bytes: (1.0 + (i % 4) * 0.08) * GB, vmaf_mean: 98 + (i % 2), vmaf_worst_window: 95 + (i % 3), tracks_message: i % 3 === 0 ? "Kept 2 audio tracks and 2 of 3 subtitles" : undefined }]));

export const reviewVariants = {
  "choose-one": { mode: "choose", count: 1 },
  "choose-ten": { mode: "choose", count: 10 },
  "choose-forty": { mode: "choose", count: 40 },
  "choose-long-names": { mode: "choose", count: 6, long: true },
  "plan-ten": { mode: "plan", count: 10 },
  "plan-long-names": { mode: "plan", count: 4, long: true },
};

const check = () => {
  const out = [];
  const dialog = document.getElementById("batch-review");
  const mark = (rule, message, where = "") => out.push({ rule, message, where });
  if (!dialog?.open) return [{ rule: "review-open", message: "the review dialog did not open", where: "" }];
  const minTarget = matchMedia("(pointer: coarse)").matches ? 40 : 28;
  const r = el => el.getBoundingClientRect();
  const shown = el => { const s = getComputedStyle(el); return s.display !== "none" && s.visibility !== "hidden" && !el.hidden && r(el).width > 0.5; };
  const dr = r(dialog), list = document.getElementById("batch-review-data");
  if (dialog.scrollHeight > dialog.clientHeight + 1) mark("dialog-scrolls", `the dialog scrolls as a whole (${dialog.scrollHeight} > ${dialog.clientHeight}); only the list may`, "dialog#batch-review");
  if (dialog.scrollWidth > dialog.clientWidth + 1) mark("overflow-x", `the dialog scrolls sideways (${dialog.scrollWidth} > ${dialog.clientWidth})`, "dialog#batch-review");
  if (document.documentElement.scrollWidth > document.documentElement.clientWidth + 1) mark("overflow-x", "the page scrolls sideways", "html");
  if (dr.top < -0.5 || dr.bottom > innerHeight + 0.5 || dr.left < -0.5 || dr.right > innerWidth + 0.5) mark("dialog-offscreen", `dialog box ${Math.round(dr.left)},${Math.round(dr.top)} → ${Math.round(dr.right)},${Math.round(dr.bottom)} leaves the ${innerWidth}×${innerHeight} screen`, "dialog#batch-review");
  const lr = r(list);
  const decisions = [...dialog.querySelectorAll(".submit-actions button")].filter(shown);
  if (!decisions.length) mark("footer-missing", "no decision buttons are shown", "");
  for (const b of decisions) {
    const br = r(b), label = (b.textContent || "").trim();
    if (br.bottom > innerHeight + 0.5 || br.top < 0) mark("footer-offscreen", `“${label}” is outside the screen (${Math.round(br.top)}–${Math.round(br.bottom)} of ${innerHeight})`, label);
    if (br.top < lr.bottom - 0.5) mark("footer-over-list", `“${label}” sits on top of the file list`, label);
    if (br.right > dr.right + 0.5 || br.left < dr.left - 0.5) mark("overflow-x", `“${label}” leaves the dialog`, label);
    if (Math.min(br.width, br.height) < minTarget) mark("target-small", `“${label}” is ${Math.round(br.width)}×${Math.round(br.height)}px (min ${minTarget})`, label);
  }
  const close = document.getElementById("close-batch-review");
  if (close) { const cr = r(close); if (cr.width < minTarget || cr.height < minTarget) mark("target-small", `close button is ${Math.round(cr.width)}×${Math.round(cr.height)}px (min ${minTarget})`, "close"); if (cr.right > dr.right + 0.5) mark("overflow-x", "close button leaves the dialog", "close"); }
  const rows = [...list.querySelectorAll(".replacement-row")];
  for (const row of rows) { const h = r(row).height; if (h > 220) mark("row-tall", `a file row is ${Math.round(h)}px tall`, row.textContent.trim().slice(0, 40)); }
  // A long list scrolls inside itself, and the first file is reachable.
  if (list.scrollHeight > list.clientHeight + 1 && getComputedStyle(list).overflowY === "visible") mark("list-not-scrolling", "the list is taller than its box and does not scroll", "list");
  const heading = dialog.querySelector(".dialog-heading");
  if (heading && r(heading).top < dr.top - 0.5) mark("heading-offscreen", "heading is above the dialog", "heading");
  return out;
};

export async function runReview({ screenNames = Object.keys(screens), themes = ["light", "dark"], zooms = [1, 1.5], only = process.env.LAYOUT_ONLY || "", gallery = process.env.LAYOUT_GALLERY === "1" } = {}) {
  await mkdir(out, { recursive: true });
  const sameHeight = installButtonHeights();
  const scenario = scenarios.find(s => s.id === "batch-candidates");
  const state = { jobs: [], items: {}, reply: {} };
  state.api = (name, params) => (name === "batch-candidates" ? state.reply(params) : undefined);
  const server = await startServer(state);
  const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
  const found = [];
  try {
    for (const screenName of screenNames) {
      for (const theme of themes) {
        const screen = screens[screenName];
        const context = await browser.newContext({ viewport: { width: screen.width, height: screen.height }, deviceScaleFactor: 2, isMobile: screen.mobile, hasTouch: screen.mobile, colorScheme: theme, serviceWorkers: "block", reducedMotion: "reduce" });
        await context.clock.setFixedTime(new Date(Date.UTC(2026, 9, 25, 12, 0, 0)));
        const page = await context.newPage();
        const errors = [];
        page.on("pageerror", e => errors.push(`pageerror: ${e.message}`));
        page.on("console", m => m.type() === "error" && !/favicon|Failed to load resource/.test(m.text()) && errors.push(`console: ${m.text()}`));
        await page.goto(`${server.origin}/?view=queue`);
        await page.waitForSelector("#workspace:not([hidden])", { timeout: 10000 });
        for (const [name, variant] of Object.entries(reviewVariants)) {
          if (only && !`review-${name}`.includes(only)) continue;
          const w = makeWorld(scenario, "short", "normal");
          const list = members(variant.count, variant.long);
          state.jobs = w.jobs; state.items = w.items;
          state.reply = (params) => params.get("plan")
            ? (variant.mode === "plan" ? { prepared: true, id: w.id, digest: "digest-1", waiting: true, approved: false, members: list, member_facts: facts(list) } : { prepared: false })
            : { members: list, member_facts: facts(list), version: "v1", excluded: variant.count > 5 ? 2 : 0 };
          for (const zoom of zooms) {
            const cell = `review-${name}__${screenName}__${theme}__x${zoom}`;
            const mark = (rule, message, where = "") => { if (!found.some(f => f.cell === cell && f.rule === rule && f.where === where && f.message === message)) found.push({ cell, scenario: `review-${name}`, screen: screenName, theme, zoom, rule, message, where }); };
            errors.length = 0;
            try {
              await page.evaluate(() => { document.querySelector("#batch-review")?.open && document.querySelector("#batch-review").close(); document.querySelector("#job-detail")?.open && document.querySelector("#job-detail").close(); history.pushState({}, "", "/?view=queue"); dispatchEvent(new PopStateEvent("popstate")); });
              await page.setViewportSize({ width: Math.round(screen.width / zoom), height: Math.round(screen.height / zoom) });
              await page.waitForSelector(`.job-row[data-operation-id="${w.id}"]`, { timeout: 5000 });
              await page.evaluate(i => { history.pushState({}, "", `/?view=queue&job=${encodeURIComponent(i)}`); dispatchEvent(new PopStateEvent("popstate")); }, w.id);
              await page.waitForFunction(i => document.querySelector("#job-detail")?.open && document.querySelector("#detail-summary")?.dataset.operationId === i, w.id, { timeout: 8000 });
              await page.locator("#detail-controls").getByRole("button", { name: /^Review/ }).first().click({ timeout: 5000 });
              await page.waitForFunction(() => document.querySelector("#batch-review")?.open && document.querySelector("#batch-review-data")?.children.length, null, { timeout: 5000 });
              await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
              for (const v of await page.evaluate(check)) mark(v.rule, v.message, v.where);
              for (const v of await page.evaluate(`(${sameHeight.toString()})("#batch-review", ${buttonHeightTolerance})`)) mark(v.rule, v.message, v.where);
              // The list scrolled to its end: the last file is reachable and the footer has not moved.
              await page.evaluate(() => { const l = document.getElementById("batch-review-data"); l.scrollTop = l.scrollHeight; });
              for (const v of await page.evaluate(check)) mark(v.rule, v.message, v.where);
              for (const e of errors) mark("js-error", e);
            } catch (error) {
              mark("render", error.message.split("\n")[0]);
            }
            if (gallery || found.some(f => f.cell === cell)) await page.screenshot({ path: `${out}${cell}.png` }).catch(() => {});
          }
        }
        await context.close();
      }
    }
  } finally {
    await browser.close();
    await server.close();
  }
  return found;
}
