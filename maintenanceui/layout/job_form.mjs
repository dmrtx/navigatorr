// The Files tab's job form for one file, opened from its link on every screen
// and theme. Hard rules:
//   - the audio and subtitle lists start folded
//   - the Process button is wholly on screen when the form opens, and still is
//     with both lists unfolded
//   - beside the file list (wide screens) the form is never taller than the screen
import { chromium } from "playwright";
import { startServer } from "./server.mjs";
import { screens } from "./runner.mjs";

const GB = 1024 ** 3;
const folder = "/media/Movies/Example (2020)", file = `${folder}/Example (2020).mkv`;
const languages = ["jpn", "eng", "spa", "fre", "ger", "ita", "por"];
const tracks = {
  languages: [],
  audio: languages.slice(0, 4).map((language, i) => ({ index: i + 1, language, codec: i ? "ac3" : "truehd", channels: 6, keep: true, bytes: 0.4 * GB, kbps: 640, outputs: { size: { output: "aac", kbps: 256, bytes: 0.15 * GB } } })),
  subtitles: Array.from({ length: 14 }, (_, i) => ({ index: i + 5, language: languages[i % languages.length], codec: "subrip", keep: true, bytes: 50000 })),
  size_bytes: 20 * GB, duration_sec: 7200, video_bytes: 18 * GB,
};

export async function runJobForm({ screenNames = Object.keys(screens), themes = ["light", "dark"] } = {}) {
  const found = [];
  const state = { jobs: [], items: {}, api: (name, params) => {
    if (name === "folder") return { items: [{ path: file, name: file.split("/").pop(), is_dir: false, size: 20 * GB }], total: 1, has_more: false, path: params.get("path") || "" };
    if (name === "source-tracks") return tracks;
  } };
  const server = await startServer(state);
  const browser = await chromium.launch(process.env.LAYOUT_BROWSER ? { channel: process.env.LAYOUT_BROWSER } : {});
  try {
    for (const screenName of screenNames) {
      for (const theme of themes) {
        const screen = screens[screenName];
        const context = await browser.newContext({ viewport: { width: screen.width, height: screen.height }, isMobile: screen.mobile, hasTouch: screen.mobile, colorScheme: theme, serviceWorkers: "block", reducedMotion: "reduce" });
        const page = await context.newPage();
        const add = (rule, message) => found.push({ rule, message, screen: screenName, theme });
        page.on("pageerror", e => add("js-error", e.message));
        try {
          await page.goto(`${server.origin}/?${new URLSearchParams({ view: "files", source: `folder:${folder.slice(0, folder.lastIndexOf("/"))}`, folder, file })}`);
          await page.waitForSelector("#keep-tracks-file:not([hidden])", { timeout: 8000 });
          await page.waitForFunction(() => document.querySelector("#keep-subtitle-count")?.textContent);
          const look = () => page.evaluate(() => {
            const rect = el => el?.getBoundingClientRect();
            const submit = rect(document.querySelector("#job-form .submit-actions .primary"));
            const editor = document.querySelector(".target-editor");
            const beside = getComputedStyle(editor).position === "sticky";
            return {
              folded: ["keep-audio", "keep-subtitles"].filter(id => document.getElementById(id).open),
              submit: submit && submit.height > 0 && submit.top >= 0 && submit.bottom <= innerHeight + 0.5,
              editorBottom: beside ? Math.round(rect(editor).bottom) : null, height: innerHeight,
            };
          });
          const opened = await look();
          if (opened.folded.length) add("folded", `${opened.folded.join(", ")} open when the form opens`);
          if (!opened.submit) add("submit", "the Process button is not wholly on screen when the form opens");
          if (opened.editorBottom !== null && opened.editorBottom > opened.height) add("height", `the form ends at ${opened.editorBottom}px on a ${opened.height}px screen`);
          for (const id of ["keep-audio", "keep-subtitles"]) await page.click(`#${id} > summary`);
          const unfolded = await look();
          if (!unfolded.submit) add("submit", "the Process button is not wholly on screen with both lists unfolded");
          if (unfolded.editorBottom !== null && unfolded.editorBottom > unfolded.height) add("height", `with both lists unfolded the form ends at ${unfolded.editorBottom}px on a ${unfolded.height}px screen`);
        } catch (e) {
          add("run", e.message.split("\n")[0]);
        } finally {
          await context.close();
        }
      }
    }
  } finally {
    await browser.close();
    await server.close();
  }
  return found;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const found = await runJobForm();
  for (const v of found) console.log(`  ${v.rule}  (${v.screen}, ${v.theme})  ${v.message}`);
  console.log(found.length ? `${found.length} findings` : "The job form fits every screen.");
  process.exit(found.length ? 1 : 0);
}
