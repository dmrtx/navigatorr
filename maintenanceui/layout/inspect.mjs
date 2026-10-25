// Prints what the job detail says for one scenario, section by section, and
// optionally saves a screenshot:  node inspect.mjs file-candidate [phone] [shot.png]
import { chromium } from "playwright";
import { startServer } from "./server.mjs";
import { scenarios, world } from "./fixtures.mjs";
import { screens } from "./runner.mjs";

const [id, screenName = "phone", shot] = process.argv.slice(2);
const scenario = scenarios.find(s => s.id === id);
if (!scenario) { console.error(`unknown scenario ${id}; try: ${scenarios.map(s => s.id).join(" ")}`); process.exit(2); }
const screen = screens[screenName];
const state = { jobs: [], items: {} };
const server = await startServer(state);
const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
const context = await browser.newContext({ viewport: { width: screen.width, height: screen.height }, deviceScaleFactor: 2, isMobile: screen.mobile, hasTouch: screen.mobile, serviceWorkers: "block", reducedMotion: "reduce" });
// The scenarios are dated against one fixed moment, so "2 s ago" stays 2 s ago.
await context.clock.setFixedTime(new Date(Date.UTC(2026, 9, 25, 12, 0, 0)));
const page = await context.newPage();
await page.goto(`${server.origin}/?view=queue`);
await page.waitForSelector("#workspace:not([hidden])");
const w = world(scenario, "short", "normal");
state.jobs = w.jobs; state.items = w.items;
await page.evaluate(i => { history.pushState({}, "", `/?view=queue&job=${encodeURIComponent(i)}`); dispatchEvent(new PopStateEvent("popstate")); }, w.id);
await page.waitForFunction(i => document.querySelector("#detail-summary")?.dataset.operationId === i, w.id);
await page.waitForTimeout(300);
const text = await page.evaluate(() => [...document.querySelectorAll("#job-detail [data-section]")].filter(s => s.offsetParent !== null).map(s => `## ${s.dataset.section}\n${s.innerText}`).join("\n\n"));
console.log(text);
if (shot) await page.screenshot({ path: shot });
await browser.close();
await server.close();
