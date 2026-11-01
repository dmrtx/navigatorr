// The Review tab, driven in a real browser on every screen and theme. Hard rules:
//   - every entry is a row, each with a title, its folder and a size
//   - an entry inside the media folders is a link that opens its folder in Files
//   - an entry outside them is not a link
//   - Processed and Drop send one request each, take the row away and offer Undo
//   - the page never scrolls sideways and no row's content overflows its row
import { chromium } from "playwright";
import { startServer, reviewRows } from "./server.mjs";
import { screens } from "./runner.mjs";
import { scenarios, world as makeWorld } from "./fixtures.mjs";

export async function runReviewTab({ screenNames = Object.keys(screens), themes = ["light", "dark"] } = {}) {
  const found = [];
  const w = makeWorld(scenarios.find(s => s.id === "file-encoding"), "short", "normal");
  const state = { jobs: w.jobs, items: w.items, review: structuredClone(reviewRows), api: (name, params) => (name === "folder" ? { items: [], total: 0, has_more: false, path: params.get("path") || "" } : undefined) };
  const server = await startServer(state);
  const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
  try {
    for (const screenName of screenNames) {
      for (const theme of themes) {
        const screen = screens[screenName];
        const context = await browser.newContext({ viewport: { width: screen.width, height: screen.height }, isMobile: screen.mobile, hasTouch: screen.mobile, colorScheme: theme, serviceWorkers: "block", reducedMotion: "reduce" });
        const page = await context.newPage();
        const add = (rule, message) => found.push({ rule, message, screen: screenName, theme });
        const posts = [];
        page.on("pageerror", e => add("js-error", e.message));
        page.on("request", r => { if (r.method() === "POST" && r.url().includes("maintenance-queue")) posts.push(JSON.parse(r.postData())); });
        try {
          await page.goto(`${server.origin}/?view=review`);
          await page.waitForSelector(".review-row", { timeout: 10000 });
          const rows = await page.$$eval(".review-row", els => els.map(el => ({
            title: el.querySelector(".review-title")?.textContent || "",
            path: el.querySelector(".review-path")?.textContent || "",
            size: el.querySelector(".review-size")?.textContent || "",
            link: el.querySelector("a.review-main")?.getAttribute("href") || "",
            overflow: el.scrollWidth > el.clientWidth + 1,
          })));
          if (rows.length !== reviewRows.length) add("rows", `${rows.length} rows for ${reviewRows.length} entries`);
          for (const r of rows) {
            if (!r.title || !r.path) add("identify", `a row has no title or no folder: “${r.title}”`);
            if (r.overflow) add("overflow", `“${r.title}” overflows its row`);
          }
          const slam = rows.find(r => r.title.startsWith("The First Slam Dunk"));
          const params = new URLSearchParams((slam?.link || "").replace(/^\?/, ""));
          if (params.get("view") !== "files" || params.get("source") !== "folder:/media/Movies" || params.get("folder") !== "/media/Movies/The First Slam Dunk (2022)") add("link", `Files link is “${slam?.link}”`);
          const outside = rows.find(r => r.title === "Elsewhere");
          if (outside?.link) add("link", "an entry outside the media folders is a link");
          if (await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth + 1)) add("scroll-x", "the page scrolls sideways");

          // Tap a row: Files opens that folder.
          await page.click("a.review-main");
          await page.waitForFunction(() => location.search.includes("view=files"), null, { timeout: 5000 }).catch(() => add("open", "tapping a row did not open Files"));
          await page.goBack();
          await page.waitForSelector(".review-row", { timeout: 5000 }).catch(() => add("back", "Back from Files did not return to Review"));

          // Mark one processed.
          await page.click('.review-row[data-review-id="55"] [aria-label^="Mark"]');
          await page.waitForSelector('.review-row[data-review-id="55"]', { state: "detached", timeout: 5000 }).catch(() => add("processed", "the row stayed after Processed"));
          if (posts.length !== 1 || posts[0].id !== 55 || posts[0].action !== "processed") add("processed", `request was ${JSON.stringify(posts)}`);
          if (!(await page.textContent("#notice")).includes("Undo")) add("undo", "no Undo after Processed");
          // Drop another.
          await page.click('.review-row[data-review-id="70"] [aria-label^="Drop"]');
          await page.waitForSelector('.review-row[data-review-id="70"]', { state: "detached", timeout: 5000 }).catch(() => add("drop", "the row stayed after Drop"));
          if (posts.length !== 2 || posts[1].id !== 70 || posts[1].action !== "dismiss") add("drop", `request was ${JSON.stringify(posts)}`);
        } catch (error) {
          add("render", error.message.split("\n")[0]);
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

if (import.meta.url === `file://${process.argv[1]}`) {
  const found = await runReviewTab();
  for (const f of found) console.log(`  ${f.rule}  (${f.screen}, ${f.theme})  ${f.message}`);
  console.log(found.length ? `${found.length} findings` : "Review tab passes.");
  process.exit(found.length ? 1 : 0);
}
