// Sheets close with a drag down, driven with real touch events (Chrome DevTools
// Protocol) on every touch screen in both themes. Hard rules:
//   - a drag down on the heading moves the sheet with the finger and, past a
//     quarter of it, closes it through its own close button
//   - a short drag springs back and leaves the sheet open
//   - a drag up or sideways never moves it
//   - on the body, a drag down closes only when the body is scrolled to the top;
//     a scrolled body scrolls instead
import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";
import { startServer } from "./server.mjs";
import { screens, out } from "./runner.mjs";
import { scenarios, world as makeWorld } from "./fixtures.mjs";

const touch = async (cdp, type, points) => cdp.send("Input.dispatchTouchEvent", { type, touchPoints: points.map(([x, y]) => ({ x, y, id: 1 })) });
async function drag(cdp, x, y, dx, dy, { lift = true, steps = 10 } = {}) {
  await touch(cdp, "touchStart", [[x, y]]);
  for (let i = 1; i <= steps; i++) await touch(cdp, "touchMove", [[x + (dx * i) / steps, y + (dy * i) / steps]]);
  if (lift) await touch(cdp, "touchEnd", []);
}

export async function runSheet({ screenNames = Object.keys(screens).filter(n => screens[n].mobile), themes = ["light", "dark"], only = process.env.LAYOUT_ONLY || "", gallery = process.env.LAYOUT_GALLERY === "1" } = {}) {
  await mkdir(out, { recursive: true });
  const world = makeWorld(scenarios.find(s => s.id === "batch-running"), "short", "normal");
  const server = await startServer({ jobs: world.jobs, items: world.items });
  const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
  const found = [];
  try {
    for (const screenName of screenNames) {
      for (const theme of themes) {
        if (only && !`sheet-${screenName}`.includes(only)) continue;
        const screen = screens[screenName];
        const cell = `sheet__${screenName}__${theme}`;
        const mark = (rule, message, where = "") => { if (!found.some(f => f.cell === cell && f.rule === rule && f.message === message)) found.push({ cell, scenario: "sheet", screen: screenName, theme, zoom: 1, rule, message, where }); };
        const context = await browser.newContext({ viewport: { width: screen.width, height: screen.height }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, colorScheme: theme, serviceWorkers: "block", reducedMotion: "reduce" });
        await context.clock.setFixedTime(new Date(Date.UTC(2026, 9, 25, 12, 0, 0)));
        const page = await context.newPage();
        const errors = [];
        page.on("pageerror", e => errors.push(`pageerror: ${e.message}`));
        page.on("console", m => m.type() === "error" && !/favicon|Failed to load resource/.test(m.text()) && errors.push(`console: ${m.text()}`));
        try {
          const cdp = await context.newCDPSession(page);
          await page.goto(`${server.origin}/?view=queue`);
          await page.waitForSelector(".job-row .job-heading a, .job-row .job-heading button", { timeout: 10000 });
          const open = async () => {
            await page.evaluate(() => { const l = document.querySelector(".job-row .job-heading a"); l.click(); });
            await page.waitForSelector("#job-detail[open]", { timeout: 5000 });
            await page.waitForTimeout(150);
          };
          const sheet = () => page.evaluate(() => {
            const d = document.getElementById("job-detail"), r = d.getBoundingClientRect(), h = d.querySelector(".jd-header").getBoundingClientRect();
            return { open: d.open, top: r.top, bottom: r.bottom, height: r.height, transform: d.style.transform, headerY: h.top + h.height / 2, headerX: Math.min(h.left + 40, innerWidth - 80), scroller: Math.max(...[d, ...d.querySelectorAll("*")].map(e => e.scrollHeight > e.clientHeight + 1 && /auto|scroll/.test(getComputedStyle(e).overflowY) ? e.scrollHeight - e.clientHeight : 0)) };
          });
          await open();
          const start = await sheet();
          const x = start.headerX, y = start.headerY;
          // 1. A drag down on the heading follows the finger.
          await drag(cdp, x, y, 0, 90, { lift: false });
          await page.waitForTimeout(50);
          const moving = await sheet();
          const moved = moving.top - start.top;
          if (moved < 60 || moved > 90) mark("sheet-follow", `a 90px drag moved the sheet ${Math.round(moved)}px`, "job-detail");
          if (gallery) await page.screenshot({ path: `${out}${cell}__dragging.png` }).catch(() => {});
          // 2. Lifting short of a quarter springs back and leaves it open.
          await touch(cdp, "touchEnd", []);
          await page.waitForTimeout(400);
          const back = await sheet();
          if (!back.open) mark("sheet-short", "a short drag closed the sheet", "job-detail");
          else if (Math.abs(back.top - start.top) > 1 || back.transform) mark("sheet-spring", `the sheet did not come back (${Math.round(back.top - start.top)}px off)`, "job-detail");
          // 3. A drag up or sideways never moves it.
          if (back.open) {
            await drag(cdp, x, y + 10, 0, -80);
            await drag(cdp, x, y, 120, 20);
            await page.waitForTimeout(300);
            const still = await sheet();
            if (!still.open || Math.abs(still.top - start.top) > 1) mark("sheet-stray", "a drag up or sideways moved or closed the sheet", "job-detail");
          }
          // 4. A long drag down closes it, through the close button.
          if ((await sheet()).open) {
            await drag(cdp, x, y, 0, Math.round(start.height * 0.4));
            await page.waitForFunction(() => !document.getElementById("job-detail").open, null, { timeout: 2500 }).catch(() => {});
            if ((await sheet()).open) mark("sheet-close", "a long drag down did not close the sheet", "job-detail");
          }
          // Reopened, it is whole again: nothing of the drag is left on it.
          await open();
          const reopened = await sheet();
          if (Math.abs(reopened.top - start.top) > 1 || reopened.transform) mark("sheet-leftover", `the reopened sheet is ${Math.round(reopened.top - start.top)}px off, transform “${reopened.transform}”`, "job-detail");
          // 5. The body, scrolled to its top, closes the sheet like the heading does.
          {
            await page.evaluate(() => { const d = document.getElementById("job-detail"); for (const e of [d, ...d.querySelectorAll("*")]) e.scrollTop = 0; });
            await drag(cdp, Math.round(screen.width / 2), Math.round(start.top + start.height * 0.5), 0, Math.round(start.height * 0.4));
            await page.waitForFunction(() => !document.getElementById("job-detail").open, null, { timeout: 2500 }).catch(() => {});
            if ((await sheet()).open) mark("sheet-body-close", "a drag down on a body at its top did not close the sheet", "job-detail");
            else await open();
          }
          // 6. A body scrolled down scrolls; it does not close.
          if (reopened.scroller > 0) {
            await page.evaluate(() => { const d = document.getElementById("job-detail"); const e = [d, ...d.querySelectorAll("*")].find(e => e.scrollHeight > e.clientHeight + 1 && /auto|scroll/.test(getComputedStyle(e).overflowY)); e.scrollTop = 40; window.__scrolled = e; });
            const body = await page.evaluate(() => { const r = window.__scrolled.getBoundingClientRect(); return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + Math.min(r.height / 2, 120)) }; });
            await drag(cdp, body.x, body.y, 0, 60);
            await page.waitForTimeout(300);
            const scrolled = await sheet();
            if (!scrolled.open || Math.abs(scrolled.top - start.top) > 1) mark("sheet-scrolled-body", "a drag down on a scrolled body moved or closed the sheet", "job-detail");
          }
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
