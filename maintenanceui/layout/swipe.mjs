// The swipe-to-reveal actions on the queue, driven with real touch events
// (Chrome DevTools Protocol), on every touch screen in both themes. Hard rules:
//   - at rest the actions are hidden and the row sits where it always did
//   - a swipe to the left opens a tray of actions at the row's right edge, on screen,
//     clear of the row's own content, each tile one line and equal in height
//   - a swipe to the right, a tap on the row, or a swipe on another row shuts it
//   - a long swipe archives, a short one never does, and neither opens the job
//   - the page never scrolls sideways, and the corner buttons are gone only
//     where the tray replaces them (desktop keeps them)
import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";
import { startServer } from "./server.mjs";
import { screens, out } from "./runner.mjs";
import { scenarios, world as makeWorld } from "./fixtures.mjs";
import { buttonHeightTolerance, installButtonHeights } from "./button_rules.mjs";

const rows = ["file-encoding", "file-candidate", "file-failed", "batch-running", "batch-candidates", "file-decision"];

const touch = async (cdp, type, points) => cdp.send("Input.dispatchTouchEvent", { type, touchPoints: points.map(([x, y]) => ({ x, y, id: 1 })) });
// A finger that goes from (x, y) by dx, in steps, and lifts (or does not).
async function drag(cdp, x, y, dx, { lift = true, steps = 8 } = {}) {
  await touch(cdp, "touchStart", [[x, y]]);
  for (let i = 1; i <= steps; i++) await touch(cdp, "touchMove", [[x + (dx * i) / steps, y]]);
  if (lift) await touch(cdp, "touchEnd", []);
}

export async function runSwipe({ screenNames = Object.keys(screens).filter(n => screens[n].mobile), themes = ["light", "dark"], only = process.env.LAYOUT_ONLY || "", gallery = process.env.LAYOUT_GALLERY === "1" } = {}) {
  await mkdir(out, { recursive: true });
  const sameHeight = installButtonHeights();
  const worlds = rows.map(id => makeWorld(scenarios.find(s => s.id === id), "short", "normal"));
  const calls = [];
  const state = { jobs: worlds.flatMap(w => w.jobs), items: Object.assign({}, ...worlds.map(w => w.items)) };
  state.api = (name, params) => { if (name === "archive") { calls.push(name); return {}; } };
  const server = await startServer(state);
  const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
  const found = [];
  try {
    for (const screenName of screenNames) {
      for (const theme of themes) {
        if (only && !`swipe-${screenName}`.includes(only)) continue;
        const screen = screens[screenName];
        const cell = `swipe__${screenName}__${theme}`;
        const mark = (rule, message, where = "") => { if (!found.some(f => f.cell === cell && f.rule === rule && f.message === message)) found.push({ cell, scenario: "swipe", screen: screenName, theme, zoom: 1, rule, message, where }); };
        const context = await browser.newContext({ viewport: { width: screen.width, height: screen.height }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, colorScheme: theme, serviceWorkers: "block", reducedMotion: "reduce" });
        await context.clock.setFixedTime(new Date(Date.UTC(2026, 9, 25, 12, 0, 0)));
        const page = await context.newPage();
        const errors = [];
        page.on("pageerror", e => errors.push(`pageerror: ${e.message}`));
        page.on("console", m => m.type() === "error" && !/favicon|Failed to load resource/.test(m.text()) && errors.push(`console: ${m.text()}`));
        try {
          const cdp = await context.newCDPSession(page);
          await page.goto(`${server.origin}/?view=queue`);
          await page.waitForSelector(".job-row.has-menu", { timeout: 10000 });
          // The page loads the queue more than once at start; a row redrawn under a finger takes the click with it.
          await page.waitForLoadState("networkidle").catch(() => {});
          await page.waitForTimeout(400);
          const state0 = () => page.evaluate(() => {
            const r = el => el.getBoundingClientRect();
            return [...document.querySelectorAll(".job-row.has-menu")].map(row => {
              const tray = row.querySelector(".job-actions"), content = row.querySelector(".job-heading");
              const ts = getComputedStyle(tray);
              return { id: row.dataset.operationId, open: row.classList.contains("swipe-open"), trayVisible: ts.visibility !== "hidden", trayLeft: r(tray).left, contentRight: r(content).right, scrollX: document.documentElement.scrollWidth - document.documentElement.clientWidth, tiles: tray.children.length };
            });
          });
          const hint = () => page.evaluate(() => { const h = document.getElementById("swipe-hint"); return h && !h.hidden && getComputedStyle(h).display !== "none"; });
          if (!await hint()) mark("hint", "the swipe hint is not shown before the first swipe", "#swipe-hint");
          for (const s of await state0()) {
            if (s.trayVisible) mark("tray-at-rest", "the actions tray is visible while the row is shut", s.id);
            if (s.scrollX > 1) mark("overflow-x", `the page scrolls sideways by ${s.scrollX}px`, s.id);
          }
          const pick = async n => { const found = await page.evaluate(n => {
            const row = [...document.querySelectorAll(".job-row.has-menu")][n];
            if (!row) return null;
            row.scrollIntoView({ block: "center", behavior: "instant" });
            const r = row.getBoundingClientRect(), h = row.querySelector(".job-heading").getBoundingClientRect();
            return { id: row.dataset.operationId, x: innerWidth - 40, y: Math.round(Math.min(Math.max(h.top + h.height / 2, 8), innerHeight - 8)), width: r.width };
          }, n); await page.waitForTimeout(60); return found; };
          const count = (await state0()).length;
          for (let n = 0; n < count; n++) {
            const row = await pick(n);
            if (!row) continue;
            const where = `row ${n + 1}`;
            // 1. A short swipe, below half the tray, springs back.
            await drag(cdp, row.x, row.y, -20);
            await page.waitForTimeout(60);
            if ((await state0())[n].open) mark("short-swipe", "a 20px swipe left the tray open", where);
            // 2. A swipe to the left opens it.
            await drag(cdp, row.x, row.y, -Math.min(row.width * 0.45, 190));
            await page.waitForTimeout(120);
            const open = (await state0())[n];
            if (!open.open) { mark("swipe-open", "a swipe to the left did not open the tray", where); continue; }
            if (!open.trayVisible) mark("tray-hidden", "the tray is open but not visible", where);
            if (open.trayLeft < 16) mark("tray-wide", `the tray starts at ${Math.round(open.trayLeft)}px of ${screen.width}; the row should still peek`, where);
            if (open.contentRight > open.trayLeft + 0.5) mark("tray-overlap", `the row content ends at ${Math.round(open.contentRight)}px, under the tray that starts at ${Math.round(open.trayLeft)}`, where);
            if (open.scrollX > 1) mark("overflow-x", `the page scrolls sideways by ${open.scrollX}px`, where);
            for (const v of await page.evaluate(`(${sameHeight.toString()})(".job-row.swipe-open .job-actions", ${buttonHeightTolerance})`)) mark(v.rule, v.message, where);
            const tiles = await page.evaluate(() => [...document.querySelectorAll(".job-row.swipe-open .job-actions > button")].map(b => { const r = b.getBoundingClientRect(); return { label: b.getAttribute("aria-label"), w: r.width, h: r.height, left: r.left, right: r.right, shown: getComputedStyle(b).display !== "none" }; }));
            for (const t of tiles) {
              if (!t.shown) mark("tile-missing", `“${t.label}” is not shown in the tray`, where);
              if (t.w < 44 || t.h < 44) mark("target-small", `“${t.label}” tile is ${Math.round(t.w)}×${Math.round(t.h)}px (min 44)`, where);
              if (t.right > screen.width + 0.5 || t.left < -0.5) mark("tile-offscreen", `“${t.label}” tile spans ${Math.round(t.left)}–${Math.round(t.right)} of ${screen.width}`, where);
            }
            if (!tiles.length) mark("tray-empty", "the open tray holds no actions", where);
            if (gallery && n === 0) await page.screenshot({ path: `${out}${cell}__open.png` }).catch(() => {});
            if (await hint()) mark("hint", "the swipe hint stays after the first swipe", "#swipe-hint");
            // 3. A tap on the open row shuts it and does not open the job.
            const before = page.url();
            const tapX = Math.max(Math.round(open.trayLeft) - 24, 8);
            // The first swipe hides the hint above the list, so the row has moved since it was picked.
            const tapY = await page.evaluate(id => { const h = document.querySelector(`.job-row[data-operation-id="${id}"] .job-heading`).getBoundingClientRect(); return Math.round(Math.min(Math.max(h.top + h.height / 2, 8), innerHeight - 8)); }, row.id);
            // The tap is sent as a touch. Some headless browsers never turn a synthetic touch into a click;
            // the app answers the click, so one is made with the mouse when the touch produced none.
            await page.evaluate(() => { window.__clicks = 0; if (!window.__counting) { window.__counting = true; document.addEventListener("click", () => window.__clicks++, true); } });
            await page.touchscreen.tap(tapX, tapY);
            const touched = await page.waitForFunction(() => window.__clicks > 0, null, { timeout: 400 }).then(() => true, () => false);
            if (!touched) await page.mouse.click(tapX, tapY);
            // A cold runner can take longer than a fixed beat to deliver the click.
            await page.waitForFunction(id => !document.querySelector(`.job-row[data-operation-id="${id}"]`)?.classList.contains("swipe-open"), row.id, { timeout: 700 }).catch(() => {});
            if ((await state0())[n].open) mark("tap-closes", "a tap on the open row left the tray open", where);
            if (page.url() !== before) mark("tap-navigates", "the tap that shuts the tray opened the job", where);
            // 4. Swipe open, then swipe right to shut.
            const rowY = await page.evaluate(id => { const h = document.querySelector(`.job-row[data-operation-id="${id}"] .job-heading`).getBoundingClientRect(); return Math.round(Math.min(Math.max(h.top + h.height / 2, 8), innerHeight - 8)); }, row.id);
            await drag(cdp, row.x, rowY, -Math.min(row.width * 0.45, 190));
            await page.waitForTimeout(120);
            const trayPx = (await state0())[n].trayLeft;
            await drag(cdp, Math.min(trayPx + 20, screen.width - 40), rowY, 140);
            await page.waitForTimeout(120);
            if ((await state0())[n].open) mark("swipe-left", "a swipe to the right did not shut the tray", where);
          }
          // 5. A long swipe on a row that can be archived asks to archive it; the job does not open.
          const archivable = await page.evaluate(() => [...document.querySelectorAll(".job-row.has-menu")].findIndex(r => r.querySelector('.job-actions [aria-label="Archive"]')));
          if (archivable < 0) mark("archive-row", "no row offers Archive to test the long swipe with", "");
          else {
            const row = await pick(archivable);
            const url = page.url();
            await drag(cdp, row.x, row.y, -row.width * 0.8);
            await page.waitForFunction(() => [...document.querySelectorAll("dialog[open]")].some(d => /Archive/.test(d.textContent)), null, { timeout: 2500 }).catch(() => {});
            const asked = await page.evaluate(() => [...document.querySelectorAll("dialog[open]")].some(d => /Archive/.test(d.textContent)));
            if (!asked && !calls.length) mark("long-swipe", "a long swipe did not archive the job", `row ${archivable + 1}`);
            if (page.url() !== url) mark("long-swipe-navigates", "the long swipe opened the job", `row ${archivable + 1}`);
            if ((await state0())[archivable].open) mark("long-swipe-open", "the tray stays open after a long swipe", `row ${archivable + 1}`);
            await page.keyboard.press("Escape").catch(() => {});
          }
          // 6. A scroll along the list never opens a tray.
          await page.evaluate(() => scrollTo(0, 0));
          const row = await pick(0);
          await touch(cdp, "touchStart", [[row.x, row.y]]);
          for (let i = 1; i <= 6; i++) await touch(cdp, "touchMove", [[row.x - i, row.y + i * 8]]);
          await touch(cdp, "touchEnd", []);
          await page.waitForTimeout(80);
          if ((await state0()).some(s => s.open)) mark("scroll-opens", "a vertical scroll opened a tray", "");
          for (const e of errors) mark("js-error", e);
        } catch (error) {
          mark("render", error.message.split("\n")[0]);
        }
        if (gallery || found.some(f => f.cell === cell)) await page.screenshot({ path: `${out}${cell}.png` }).catch(() => {});
        await context.close();
      }
    }
  } finally {
    await browser.close();
    await server.close();
  }
  return found;
}

const innerWidthOf = (screen, zoom) => screen.width / zoom;
