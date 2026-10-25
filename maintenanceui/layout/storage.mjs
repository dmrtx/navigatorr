// The Stats tab's storage section, through the same real-browser rules as the
// Activity list: nothing leaves the card sideways, a measurement never wraps, the
// bar has a line of its own, nothing overlaps, text stays readable. It runs on
// every screen, in both themes, at 100% and 150% zoom, against names and sizes
// that stretch the layout.
import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";
import { startServer } from "./server.mjs";
import { screens, out } from "./runner.mjs";
import { listBudgets, installList } from "./list_invariants.mjs";

const TB = 1e12, GB = 1e9, MB = 1e6;
const day = (n, bytes) => ({ t: new Date(Date.UTC(2026, 9, 25 - n, 12)).toISOString(), bytes });
const series = (from, to, days = 30) => Array.from({ length: days }, (_, i) => day(days - 1 - i, from + ((to - from) * i) / (days - 1)));
const folder = (volume, name, bytes, extra = {}) => ({
  path: `/volume1/Media/${name}`, name, volume, bytes, source: "helper", measured_at: "2026-10-25T12:00:00Z",
  trend: { points: series(bytes * 1.08, bytes), change_7d: -bytes * 0.02, change_30d: -bytes * 0.08 }, ...extra,
});
function volume(id, total, used, folders, extra = {}) {
  const accounted = folders.filter(f => !f.inside).reduce((n, f) => n + f.bytes, 0);
  return {
    id, roots: folders.map(f => f.path), total_bytes: total, used_bytes: used, free_bytes: total - used - total * 0.01,
    accounted_bytes: accounted, other_bytes: Math.max(0, used - accounted), source: "helper", measured_at: "2026-10-25T12:00:00Z",
    trend: { points: series(used * 1.06, used), change_7d: -used * 0.01, change_30d: -used * 0.06, change_all: -used * 0.06 }, ...extra,
  };
}
const base = (volumes, folders, extra = {}) => ({ helper: true, measuring: false, measured_at: "2026-10-25T12:00:00Z", since: "2026-09-25T12:00:00Z", volumes, folders, ...extra });
const v1 = "/volume1/Media/Anime";

export const storageVariants = {
  typical: () => {
    const f = [folder(v1, "Anime", 7 * TB), folder(v1, "Movies", 4.1 * TB)];
    return base([volume(v1, 20 * TB, 12 * TB, f)], f);
  },
  many: () => {
    const f = [folder(v1, "Anime", 5.2 * TB), folder(v1, "Movies", 3.1 * TB), folder(v1, "TvSeries", 2.2 * TB), folder(v1, "Downloads", 800 * GB), folder(v1, "Documentaries", 400 * GB), folder(v1, "Music", 90 * GB),
      folder(v1, "Podcasts", 40 * GB, { inside: "/volume1/Media/Downloads", path: "/volume1/Media/Downloads/Podcasts" })];
    return base([volume(v1, 20 * TB, 13 * TB, f)], f);
  },
  names: () => {
    const f = [
      folder(v1, "The Extraordinarily Long Running Documentary Series About Everything", 3 * TB),
      folder(v1, `Monogatari.S03.Complete.BluRay.1080p.${"x".repeat(60)}`, 2 * TB),
      folder(v1, "物語シリーズ・第三期のすべてのエピソード", 1 * TB),
    ];
    return base([volume(v1, 10 * TB, 6.5 * TB, f)], f);
  },
  tiny: () => {
    const f = [folder(v1, "Anime", 3 * MB), folder(v1, "Movies", 1 * MB)];
    return base([volume(v1, 20 * TB, 9 * MB, f, { trend: { points: series(9 * MB, 9 * MB, 3) } })], f);
  },
  full: () => {
    const f = [folder(v1, "Anime", 12 * TB), folder(v1, "Movies", 7.9 * TB)];
    return base([volume(v1, 20 * TB, 19.95 * TB, f, { free_bytes: 0.05 * TB })], f);
  },
  young: () => {
    const f = [folder(v1, "Anime", 7 * TB, { trend: { points: [day(0, 7 * TB)] } })];
    return base([volume(v1, 20 * TB, 8 * TB, f, { trend: { points: [day(0, 8 * TB)] } })], f);
  },
  growing: () => {
    const f = [folder(v1, "Anime", 7 * TB, { trend: { points: series(6 * TB, 7 * TB), change_7d: 0.3 * TB, change_30d: 1 * TB } })];
    return base([volume(v1, 20 * TB, 8 * TB, f, { trend: { points: series(7 * TB, 8 * TB), change_7d: 0.3 * TB, change_30d: 1 * TB } })], f);
  },
  mount: () => {
    const f = [folder(v1, "Anime", 7 * TB, { source: "mount" })];
    return base([volume(v1, 20 * TB, 12 * TB, f, { source: "mount" })], f, { helper: false, notes: ["The NAS helper could not report volume space; the share was asked instead.", "2 of 5 folders could not be sized completely. Folder sizes need the NAS helper for libraries this large."] });
  },
  multi: () => {
    const a = [folder(v1, "Anime", 7 * TB)], b = [folder("/volume2/Backup", "Backup", 2 * TB, { path: "/volume2/Backup" })];
    return base([volume(v1, 20 * TB, 12 * TB, a), volume("/volume2/Backup", 8 * TB, 2.4 * TB, b)], [...a, ...b]);
  },
  measuring: () => {
    const f = [folder(v1, "Anime", 7 * TB)];
    return base([volume(v1, 20 * TB, 12 * TB, f)], f, { measuring: true });
  },
  failed: () => {
    const f = [folder(v1, "Anime", 7 * TB)];
    return base([volume(v1, 20 * TB, 12 * TB, f)], f, { error: `the store is busy: ${"x".repeat(80)}` });
  },
  empty: () => base([], []),
  firstRun: () => base([], [], { measured_at: "", measuring: true }),
};

// The storage section is a card in a page, not a list row, so it may be as tall
// as its folders need.
const budgets = { ...listBudgets, maxRowHeight: 1400 };

export async function runStorage({ screenNames = Object.keys(screens), themes = ["light", "dark"], zooms = [1, 1.5], only = process.env.LAYOUT_ONLY || "" } = {}) {
  await mkdir(out, { recursive: true });
  const rules = installList();
  const found = [];
  const world = { jobs: [], items: {}, api: (name) => (name === "storage" ? world.reply : undefined), reply: {} };
  const server = await startServer(world);
  const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
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
        for (const [name, make] of Object.entries(storageVariants)) {
          if (only && !`storage-${name}`.includes(only)) continue;
          world.reply = make();
          await page.setViewportSize({ width: screen.width, height: screen.height });
          await page.goto(`${server.origin}/?view=stats`);
          await page.waitForSelector("#workspace:not([hidden])", { timeout: 10000 });
          await page.waitForFunction(() => document.querySelector("#storage-status")?.textContent || document.querySelector("#storage-volumes")?.children.length, null, { timeout: 5000 }).catch(() => {});
          for (const zoom of zooms) {
            await page.setViewportSize({ width: Math.round(screen.width / zoom), height: Math.round(screen.height / zoom) });
            const cell = `storage-${name}__${screenName}__${theme}__x${zoom}`;
            errors.length = 0;
            const mark = (rule, message, where = "") => found.push({ cell, scenario: `storage-${name}`, screen: screenName, theme, zoom, rule, message, where });
            try {
              await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
              const cards = await page.locator(".storage-volume").count();
              const expected = world.reply.volumes.length;
              if (cards !== expected) mark("storage-cards", `${cards} volume cards for ${expected} volumes`);
              for (let i = 0; i < cards; i++) {
                await page.evaluate(n => { const c = document.querySelectorAll(".storage-volume")[n]; c.dataset.layoutCheck = String(n); c.scrollIntoView({ block: "start" }); }, i);
                const selector = `.storage-volume[data-layout-check="${i}"]`;
                const violations = await page.evaluate(`(${rules.toString()})(${JSON.stringify(selector)}, ${JSON.stringify(budgets)})`);
                // A folder's name and the headline are free text that may wrap; the
                // generic short-label rule does not fit them (checked below instead).
                for (const v of violations.filter(v => !(v.rule === "list-short-wrap" && /storage-(name|volume-used)/.test(v.where)))) if (!found.some(f => f.cell === cell && f.rule === v.rule && f.where === v.where && f.message === v.message)) mark(v.rule, v.message, v.where);
              }
              // The status line and the button sit outside the cards.
              const header = await page.evaluate(() => {
                const bad = [];
                const status = document.querySelector("#storage-status"), button = document.querySelector("#storage-refresh"), section = document.querySelector(".storage");
                const s = section.getBoundingClientRect();
                for (const el of [status, button]) {
                  const r = el.getBoundingClientRect();
                  if (r.right > s.right + 1 || r.left < s.left - 1) bad.push(`${el.id} leaves the section`);
                }
                if (document.documentElement.scrollWidth > document.documentElement.clientWidth + 1) bad.push("the page scrolls sideways");
                const r = button.getBoundingClientRect();
                if (Math.min(r.width, r.height) < 28) bad.push(`refresh button is ${Math.round(r.width)}×${Math.round(r.height)}`);
                return bad;
              });
              for (const message of header) mark("storage-header", message);
              // A short folder name keeps one line: the size drops below it on a narrow screen.
              const names = await page.evaluate(() => [...document.querySelectorAll(".storage-name")].filter(n => n.textContent.trim().length <= 16).map(n => ({ text: n.textContent.trim(), lines: Math.round(n.getBoundingClientRect().height / parseFloat(getComputedStyle(n).lineHeight || "16")) })));
              for (const n of names) if (n.lines > 1) mark("storage-name-wrap", `“${n.text}” wraps to ${n.lines} lines`);
              const bars = await page.evaluate(() => [...document.querySelectorAll(".storage-bar")].map(bar => ({ total: [...bar.children].reduce((n, i) => n + i.getBoundingClientRect().width, 0), width: bar.getBoundingClientRect().width, height: bar.getBoundingClientRect().height })));
              for (const b of bars) {
                if (b.total > b.width + 3) mark("storage-bar-overflow", `segments add up to ${Math.round(b.total)}px in a ${Math.round(b.width)}px bar`);
                if (b.height < 6) mark("storage-bar-flat", `bar is ${b.height.toFixed(1)}px tall`);
              }
              const charts = await page.evaluate(() => [...document.querySelectorAll(".storage-trend svg")].map(s => { const r = s.getBoundingClientRect(); return { w: r.width, h: r.height }; }));
              for (const c of charts) if (c.w < 40 || c.h < 20) mark("storage-chart", `trend chart is ${Math.round(c.w)}×${Math.round(c.h)}px`);
              for (const e of errors) mark("js-error", e);
              if (found.some(f => f.cell === cell)) await page.screenshot({ path: `${out}${cell}.png`, fullPage: true }).catch(() => {});
            } catch (error) {
              mark("render", error.message.split("\n")[0]);
            }
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
