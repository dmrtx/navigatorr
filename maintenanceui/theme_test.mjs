import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const read = (name) => readFile(new URL(`./assets/${name}`, import.meta.url), "utf8");
const css = await read("app.css");
const markup = await read("index.html");
const script = await read("theme.js");

function block(selector) {
  const start = css.indexOf(`${selector} {`);
  assert.notEqual(start, -1, `missing ${selector}`);
  const open = css.indexOf("{", start);
  return css.slice(open + 1, css.indexOf("}", open));
}
function tokens(body) {
  return Object.fromEntries([...body.matchAll(/--([\w-]+):\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]));
}
const light = tokens(block(":root"));
const systemDark = tokens(block(':root:not([data-theme="light"])'));
const pinnedDark = tokens(block(':root[data-theme="dark"]'));
const dark = { ...light, ...systemDark };

test("colors are tokens with a complete dark palette for system and pinned themes", () => {
  assert.deepEqual(systemDark, pinnedDark, "system and pinned dark palettes must match");
  const colors = Object.keys(light).filter((k) => /^#/.test(light[k]));
  const fixed = ["media-bg", "media-divider"];
  for (const name of colors.filter((k) => !fixed.includes(k)))
    assert.ok(name in systemDark, `dark palette lacks --${name}`);
  assert.match(css, /@media \(prefers-color-scheme: dark\) \{\s*:root:not\(\[data-theme="light"\]\)/);
  const used = new Set([...css.matchAll(/var\(--([\w-]+)/g)].map((m) => m[1]));
  const defined = new Set([...css.matchAll(/--([\w-]+):/g)].map((m) => m[1]).concat("frame-width")); // set by app.js
  for (const name of used) assert.ok(defined.has(name), `--${name} is used but never defined`);
  const firstRule = css.indexOf("* {");
  const literals = css.slice(firstRule).match(/:\s*[^;{}]*#[0-9a-fA-F]{3,8}\b[^;{}]*;/g) || [];
  assert.deepEqual(literals, [], "color literals belong in the token blocks");
});

// WCAG 2.x relative luminance and contrast; translucent tokens are composited.
function rgba(hex) {
  let h = hex.slice(1);
  if (h.length <= 4) h = [...h].map((c) => c + c).join("");
  return [0, 2, 4, 6].map((i) => (i < h.length ? parseInt(h.slice(i, i + 2), 16) : 255));
}
const over = (fg, bg) => fg.slice(0, 3).map((v, i) => v * (fg[3] / 255) + bg[i] * (1 - fg[3] / 255));
function luminance(rgb) {
  const [r, g, b] = rgb.map((v) => ((v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}
function contrast(palette, fg, bg, base = "surface") {
  const backdrop = over(rgba(palette[bg]), rgba(palette[base]));
  const [a, b] = [luminance(over(rgba(palette[fg]), backdrop)), luminance(backdrop)].sort((x, y) => y - x);
  return (a + 0.05) / (b + 0.05);
}

test("both palettes meet WCAG AA for text, badges, controls and focus", () => {
  const text = [
    ["ink", "paper"], ["ink", "surface"], ["ink", "field"], ["ink", "code-bg"],
    ["muted", "paper"], ["muted", "surface"], ["muted", "field"], ["muted", "code-bg"], ["muted", "neutral-wash"],
    ["accent", "paper"], ["accent", "surface"], ["accent", "recovery-wash"],
    ["forest", "paper"], ["forest", "surface"], ["forest", "success-wash"], ["forest", "subtle"], ["forest", "forest-wash"], ["forest", "hover"],
    ["on-forest", "forest"], ["on-forest", "forest-strong"], ["paper", "ink"],
    ["danger", "surface"], ["danger", "danger-wash"], ["warning", "surface"], ["warning-strong", "warning-wash"],
    ["decision", "decision-wash"], ["info", "info-wash"], ["preview", "preview-wash"], ["neutral", "neutral-wash"],
  ];
  const nonText = [["control-line", "surface"], ["control-line", "field"], ["accent", "paper"], ["accent", "surface"]];
  for (const [name, palette] of [["light", light], ["dark", dark]]) {
    for (const [fg, bg] of text)
      assert.ok(contrast(palette, fg, bg) >= 4.5, `${name}: --${fg} on --${bg} is ${contrast(palette, fg, bg).toFixed(2)}:1`);
    for (const [fg, bg] of nonText)
      assert.ok(contrast(palette, fg, bg) >= 3, `${name}: --${fg} on --${bg} is ${contrast(palette, fg, bg).toFixed(2)}:1`);
  }
});

test("markup loads the theme before styles and offers System, Light and Dark", () => {
  const theme = markup.indexOf('<script src="/theme.js"></script>');
  assert.ok(theme > 0 && theme < markup.indexOf('href="/app.css"'), "theme.js must be a classic script before app.css");
  assert.match(markup, /name="theme-color"\s+media="\(prefers-color-scheme: light\)"/);
  assert.match(markup, /name="theme-color"\s+media="\(prefers-color-scheme: dark\)"/);
  const choices = [...markup.matchAll(/data-theme-choice="(\w+)" aria-pressed="(\w+)"/g)].map((m) => [m[1], m[2]]);
  assert.deepEqual(choices, [["system", "true"], ["light", "false"], ["dark", "false"]]);
});

function page({ stored = null, storage = true } = {}) {
  const listeners = {};
  const attrs = (initial) => ({
    values: { ...initial },
    getAttribute(name) { return this.values[name] ?? null; },
    setAttribute(name, value) { this.values[name] = String(value); },
  });
  const metas = [
    attrs({ name: "theme-color", media: "(prefers-color-scheme: light)", content: "#244d40" }),
    attrs({ name: "theme-color", media: "(prefers-color-scheme: dark)", content: "#121715" }),
  ];
  const buttons = ["system", "light", "dark"].map((choice) => ({
    ...attrs({ "aria-pressed": "false" }),
    dataset: { themeChoice: choice },
    closest() { return this; },
  }));
  const data = new Map(stored ? [["navigatorr-theme", stored]] : []);
  const localStorage = storage
    ? { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => data.set(k, v), removeItem: (k) => data.delete(k) }
    : { getItem() { throw new Error("blocked"); }, setItem() { throw new Error("blocked"); }, removeItem() { throw new Error("blocked"); } };
  const document = {
    documentElement: { dataset: {} },
    querySelectorAll: (selector) => (selector.includes("theme-color") ? metas : buttons),
    addEventListener: (name, fn) => (listeners[name] = fn),
  };
  const window = { localStorage, addEventListener: (name, fn) => (listeners["window:" + name] = fn) };
  vm.runInNewContext(script, { document, window });
  const pressed = () => buttons.filter((b) => b.getAttribute("aria-pressed") === "true").map((b) => b.dataset.themeChoice);
  const click = (i) => listeners.click({ target: buttons[i] });
  return { root: document.documentElement, metas, pressed, click, data, listeners };
}

test("a saved choice applies before first paint and pins the browser color", () => {
  const p = page({ stored: "dark" });
  assert.equal(p.root.dataset.theme, "dark");
  assert.deepEqual(p.pressed(), ["dark"]);
  assert.deepEqual(p.metas.map((m) => m.getAttribute("content")), ["#121715", "#121715"]);
  assert.equal(page({ stored: "purple" }).root.dataset.theme, undefined);
});

test("the switch persists per browser and System restores the media-specific colors", () => {
  const p = page();
  assert.equal(p.root.dataset.theme, undefined);
  assert.deepEqual(p.pressed(), ["system"]);
  p.click(1);
  assert.equal(p.root.dataset.theme, "light");
  assert.equal(p.data.get("navigatorr-theme"), "light");
  assert.deepEqual(p.metas.map((m) => m.getAttribute("content")), ["#244d40", "#244d40"]);
  p.click(0);
  assert.equal(p.root.dataset.theme, undefined);
  assert.equal(p.data.has("navigatorr-theme"), false);
  assert.deepEqual(p.metas.map((m) => m.getAttribute("content")), ["#244d40", "#121715"]);
  p.data.set("navigatorr-theme", "dark");
  p.listeners["window:storage"]({ key: "navigatorr-theme" });
  assert.equal(p.root.dataset.theme, "dark");
});

test("blocked storage still follows the system and applies a choice for this page", () => {
  const p = page({ storage: false });
  assert.equal(p.root.dataset.theme, undefined);
  p.click(2);
  assert.equal(p.root.dataset.theme, "dark");
  assert.deepEqual(p.pressed(), ["dark"]);
});
