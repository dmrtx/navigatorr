// One look for every control, in every view and dialog of the console. Hard rules:
//   control-height   buttons and fields are --control-size tall (30px)
//   control-radius   buttons and fields share one corner (5px)
//   control-font     a button's text is 13px; a field's is 13px, or 16px on a
//                    touch screen so iOS does not zoom (the line box is fixed, so
//                    the height does not change with it)
//   field-edge       a field has its own border, or sits in a ruled row; none is
//                    bare, and none carries a different border from its neighbours
//   icon-name        a button with only an icon has an accessible name
//   icon-place       an icon beside a label comes first, and is square and 14-20px
// Exempt by name, each for a stated reason: see `apart`.
import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";
import { startServer } from "./server.mjs";
import { screens, out } from "./runner.mjs";
import { scenarios, world as makeWorld } from "./fixtures.mjs";

// Controls that are a different kind of thing on purpose.
const apart = {
  ".tabs button": "the navigation bar is a bar of tabs, not buttons",
  "#worker-status": "a status line that happens to be pressable",
  ".column-sort": "a table header's sort toggle",
  ".chip-remove": "the × inside a chip",
  "#batch-review .dialog-heading button, #batch-review .submit-actions button": "the replacement sheet's thumb-sized footer",
  ".job-row .job-actions .action-icon": "a row's icon buttons, labelled by their tooltip",
  ".job-row .more-menu button": "menu items",
  "summary": "disclosure rows",
  ".jd-actions .more-actions > summary": "the More toggle",
  ".action-main": "the one link-styled action at the end of a sentence",
  "[data-file-toggle]": "a file's expandable row in a batch",
};

const sizeLimit = 1;
const collect = (apartSelectors) => {
  const found = [];
  const shown = el => { const s = getComputedStyle(el); if (s.display === "none" || s.visibility === "hidden") return false; for (let p = el; p; p = p.parentElement) if (p.hidden || getComputedStyle(p).display === "none") return false; const r = el.getBoundingClientRect(); return r.width > 0.5 && r.height > 0.5; };
  const coarse = matchMedia("(pointer: coarse)").matches;
  const name = el => `${el.tagName.toLowerCase()}${el.id ? `#${el.id}` : ""} “${(el.getAttribute("aria-label") || el.textContent || el.placeholder || "").trim().replace(/\s+/g, " ").slice(0, 26)}”`;
  const mark = (rule, message, el) => found.push({ rule, message, where: name(el) });
  for (const el of document.querySelectorAll("button, input, select, textarea")) {
    if (!shown(el)) continue;
    if (el.tagName === "INPUT" && ["checkbox", "radio", "hidden", "range", "file"].includes(el.type)) continue;
    if (apartSelectors.some(sel => el.matches(sel))) continue;
    const s = getComputedStyle(el), r = el.getBoundingClientRect();
    const field = el.tagName !== "BUTTON";
    const tall = el.tagName === "TEXTAREA";
    if (!tall && Math.abs(r.height - 30) > 1) mark("control-height", `${field ? "field" : "button"} is ${Math.round(r.height * 10) / 10}px tall (30)`, el);
    if (s.borderTopLeftRadius !== "5px") mark("control-radius", `corner is ${s.borderTopLeftRadius} (5px)`, el);
    // The × that closes a dialog is a glyph, one size wherever it is; an icon button's text size is moot.
    const glyph = !field && (el.textContent || "").trim() === "×";
    const iconOnly = !field && el.querySelector("svg") && ![...el.childNodes].some(n => n.nodeType === 3 && n.textContent.trim());
    const want = glyph ? "22px" : field && coarse ? "16px" : "13px";
    if (s.fontSize !== want && !iconOnly) mark("control-font", `text is ${s.fontSize} (${want})`, el);
    if (field) {
      let ruled = false;
      for (let p = el.parentElement, i = 0; p && i < 3; p = p.parentElement, i++) if (p.tagName === "LABEL" && parseFloat(getComputedStyle(p).borderBottomWidth) > 0) ruled = true;
      if (parseFloat(s.borderTopWidth) === 0 && !ruled) mark("field-edge", "field has no border and no ruled row", el);
    } else {
      const label = (el.textContent || "").trim();
      const icon = el.querySelector("svg");
      if (!label && !icon && !el.getAttribute("aria-label") && !el.title) mark("icon-name", "button has no label and no icon", el);
      const visibleText = [...el.childNodes].filter(n => n.nodeType === 3 && n.textContent.trim()).length > 0 || [...el.querySelectorAll("span")].some(sp => shown(sp) && sp.textContent.trim());
      if (icon && !visibleText && !(el.getAttribute("aria-label") || el.title)) mark("icon-name", "icon-only button has no accessible name", el);
      if (icon && shown(icon)) {
        const ir = icon.getBoundingClientRect();
        if (Math.abs(ir.width - ir.height) > 0.5 || ir.width < 14 || ir.width > 20.5) mark("icon-place", `icon is ${Math.round(ir.width)}×${Math.round(ir.height)}px (square, 14-20)`, el);
        if (visibleText) {
          const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT, { acceptNode: n => n.textContent.trim() && !n.parentElement.closest("svg") && shown(n.parentElement) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT });
          const text = walker.nextNode();
          if (text) { const range = document.createRange(); range.selectNodeContents(text); const tr = range.getBoundingClientRect(); if (ir.left > tr.left + 0.5 && Math.abs(ir.top - tr.top) < r.height) mark("icon-place", "icon comes after its label", el); }
        }
      }
    }
  }
  return found;
};

export async function runControls({ screenNames = ["desktop", "phone", "tablet"], themes = ["light", "dark"], only = process.env.LAYOUT_ONLY || "", gallery = false } = {}) {
  await mkdir(out, { recursive: true });
  const ids = ["file-encoding", "file-candidate", "file-failed", "batch-running", "batch-candidates", "file-decision", "podcast-classifying"];
  const worlds = ids.map(id => makeWorld(scenarios.find(s => s.id === id), "short", "normal"));
  const state = { jobs: worlds.flatMap(w => w.jobs), items: Object.assign({}, ...worlds.map(w => w.items)) };
  const server = await startServer(state);
  const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
  const found = [];
  const apartSelectors = Object.keys(apart);
  try {
    for (const screenName of screenNames) {
      for (const theme of themes) {
        if (only && !`controls-${screenName}`.includes(only)) continue;
        const screen = screens[screenName];
        const context = await browser.newContext({ viewport: { width: screen.width, height: screen.height }, deviceScaleFactor: 1, isMobile: screen.mobile, hasTouch: screen.mobile, colorScheme: theme, serviceWorkers: "block", reducedMotion: "reduce" });
        await context.clock.setFixedTime(new Date(Date.UTC(2026, 9, 25, 12, 0, 0)));
        const page = await context.newPage();
        const errors = [];
        page.on("pageerror", e => errors.push(`pageerror: ${e.message}`));
        const grab = async view => {
          for (const v of await page.evaluate(collect, apartSelectors)) {
            if (!found.some(f => f.screen === screenName && f.theme === theme && f.rule === v.rule && f.where === v.where && f.message === v.message)) found.push({ cell: `controls__${screenName}__${theme}`, scenario: "controls", screen: screenName, theme, zoom: 1, view, ...v });
          }
        };
        try {
          await page.goto(`${server.origin}/?view=queue`);
          await page.waitForSelector(".job-row", { timeout: 10000 });
          await page.waitForTimeout(300);
          await grab("queue");
          for (const tab of ["library", "recipes", "stats", "advanced"]) {
            await page.evaluate(t => document.querySelector(`[data-tab="${t}"]`)?.click(), tab);
            await page.waitForTimeout(400);
            await grab(tab);
          }
          await page.evaluate(() => document.querySelector('[data-tab="jobs"]')?.click());
          await page.waitForTimeout(300);
          // Every dialog, opened as it is; the ones a job fills are also opened on real jobs below.
          for (const id of await page.evaluate(() => [...document.querySelectorAll("dialog")].map(d => d.id))) {
            await page.evaluate(i => { const d = document.getElementById(i); if (!d.open) d.showModal(); }, id);
            await page.waitForTimeout(100);
            await grab(`dialog ${id}`);
            await page.evaluate(i => { const d = document.getElementById(i); if (d.open) d.close(); }, id);
          }
          for (const job of state.jobs.slice(0, 12)) {
            await page.evaluate(i => { history.pushState({}, "", `/?view=queue&job=${encodeURIComponent(i)}`); dispatchEvent(new PopStateEvent("popstate")); }, job.id);
            await page.waitForTimeout(450);
            await grab(`job ${job.id}`);
            await page.evaluate(() => { const d = document.getElementById("job-detail"); if (d?.open) d.close(); });
          }
          for (const e of errors) found.push({ cell: `controls__${screenName}__${theme}`, scenario: "controls", screen: screenName, theme, zoom: 1, view: "page", rule: "js-error", message: e, where: "" });
        } catch (error) {
          found.push({ cell: `controls__${screenName}__${theme}`, scenario: "controls", screen: screenName, theme, zoom: 1, view: "page", rule: "render", message: error.message.split("\n")[0], where: "" });
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
