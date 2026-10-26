// Layout rules for the job detail, evaluated inside the page.
//
// Every rule looks at what the browser actually laid out (rectangles, computed
// styles, what sits under a point), never at class names alone, so a rule keeps
// catching a problem whatever markup produced it. `install()` returns the
// source of one function; the runner evaluates it in the page and calls it
// with the dialog selector and the budgets.
//
// A violation is {rule, message, where}. Rules are named so a failure reads
// as "overlap: Cancel × Show in Files" rather than a pixel diff.

export const budgets = {
  // A control is never smaller than a thumb or larger than a toolbar button.
  minTarget: 28,        // CSS px, the visible box with a mouse
  minTargetTouch: 28,   // with a finger
  minGap: 6,            // between neighbouring controls
  maxButtonHeight: 56,
  // One status block, one action bar: how many controls each may show.
  maxBarControls: 3,
  maxRowControls: 1,    // a collapsed file row shows at most one button
  // Text.
  minFont: 11,
  // Progress: every bar of a kind is exactly this tall.
  barHeights: { hero: 6, mini: 3 },
  // A line that must stay on one line may be this many line-heights tall.
  singleLineSlack: 1.35,
  // Contrast of text against the surface it sits on (WCAG AA).
  contrast: 4.5,
  contrastLarge: 3,
  // Order of the named sections of the dialog. Missing ones are skipped,
  // repeated ones are a violation.
  sections: ["header", "status", "files", "details", "actions"],
  // Sections every state must show.
  requiredSections: ["header", "status"],
  overlapTolerance: 1.5, // px a box may overlap its neighbour by (borders)
};

export function install() {
  return function run(selector, b) {
    const dialog = document.querySelector(selector);
    const out = [];
    const add = (rule, message, el) => out.push({ rule, message, where: describe(el) });
    if (!dialog) return [{ rule: "present", message: `${selector} is not in the page`, where: "" }];

    function describe(el) {
      if (!el || !el.tagName) return "";
      const text = (el.getAttribute?.("aria-label") || el.textContent || "").trim().replace(/\s+/g, " ").slice(0, 40);
      const id = el.id ? `#${el.id}` : "";
      const cls = typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).slice(0, 2).join(".") : "";
      return `${el.tagName.toLowerCase()}${id}${cls}${text ? ` “${text}”` : ""}`;
    }
    const visible = el => {
      if (!(el instanceof Element)) return false;
      const style = getComputedStyle(el);
      if (style.display === "none" || style.visibility === "hidden" || Number(style.opacity) === 0) return false;
      const r = el.getBoundingClientRect();
      if (r.width < 0.5 || r.height < 0.5) return false;
      // Inside a closed <details> only the summary shows.
      for (let p = el.parentElement; p && p !== dialog.parentElement; p = p.parentElement) {
        if (p.tagName === "DETAILS" && !p.open && !(el.tagName === "SUMMARY" && el.parentElement === p)) {
          const summary = [...p.children].find(c => c.tagName === "SUMMARY");
          if (!summary || !summary.contains(el)) return false;
        }
        if (p.hidden) return false;
      }
      return true;
    };
    const all = [...dialog.querySelectorAll("*")].filter(visible);
    // Bars the dialog pins to its edges. Content scrolling under them is how a
    // scrolling dialog works; what is checked is that it can be scrolled clear.
    const pinned = el => { for (let p = el; p && p !== dialog.parentElement; p = p.parentElement) if (/sticky|fixed/.test(getComputedStyle(p).position) && p !== dialog) return p; return null; };
    const interactive = all.filter(el => el.matches("button, a[href], summary, input, select, textarea, [role=button], [tabindex]:not([tabindex='-1'])"));
    const rect = el => el.getBoundingClientRect();
    const dr = rect(dialog);
    const inScroller = el => Boolean(el.closest("pre, [data-scroll-x]"));
    const textOf = el => (el.textContent || "").replace(/\s+/g, " ").trim();

    // 1. Nothing sticks out sideways. The dialog itself never scrolls sideways
    //    and no box leaves its edges, except inside an intended scroller.
    if (dialog.scrollWidth > dialog.clientWidth + 1) add("overflow-x", `dialog scrolls sideways (${dialog.scrollWidth} > ${dialog.clientWidth})`, dialog);
    // A modal dialog owns the screen: the page behind it must not scroll, or
    // its scrollbar moves with nothing visible changing.
    if (dialog.open && dialog.matches(":modal") && !/(hidden|clip)/.test(getComputedStyle(document.documentElement).overflowY)) add("page-scrolls-behind", "the page behind the open dialog can still scroll", document.documentElement);
    if (document.documentElement.scrollWidth > document.documentElement.clientWidth + 1) add("overflow-x", `page scrolls sideways (${document.documentElement.scrollWidth} > ${document.documentElement.clientWidth})`, document.documentElement);
    for (const el of all) {
      if (inScroller(el) || el === dialog || el.closest(".sr-only")) continue;
      const r = rect(el);
      if (r.right > dr.right + 1 || r.left < dr.left - 1) add("overflow-x", `leaves the dialog by ${Math.round(Math.max(r.right - dr.right, dr.left - r.left))}px`, el);
    }

    // 2. A control sits on top of nothing but its own surface: whatever is at
    //    its centre is itself or inside it. Covered controls are unusable.
    const clipRect = el => {
      let r = rect(el);
      for (let p = el.parentElement; p && p !== document.documentElement; p = p.parentElement) {
        const s = getComputedStyle(p);
        if (/(auto|scroll|hidden|clip)/.test(s.overflowX + s.overflowY) && p !== dialog) {
          const pr = rect(p);
          r = { left: Math.max(r.left, pr.left), right: Math.min(r.right, pr.right), top: Math.max(r.top, pr.top), bottom: Math.min(r.bottom, pr.bottom) };
        }
      }
      return r;
    };
    for (const el of interactive) {
      const r = clipRect(el);
      if (r.right - r.left < 1 || r.bottom - r.top < 1) continue;
      // The part inside the viewport and the dialog's own scrollport.
      const x = Math.min(Math.max((r.left + r.right) / 2, 0), innerWidth - 1), y = (r.top + r.bottom) / 2;
      if (y < 0 || y > innerHeight) continue; // scrolled out of view; checked at the other scroll positions
      const top = document.elementFromPoint(x, y);
      if (top && top !== el && !el.contains(top) && !top.contains(el) && !(pinned(top) && !pinned(el))) add("covered", `covered by ${describe(top)}`, el);
    }

    // 2b. A word in a control is never cut in half: each word fits the line it is on.
    const measure = document.createElement("canvas").getContext("2d");
    for (const el of interactive) {
      if (!visible(el) || !el.textContent.trim()) continue;
      const cs = getComputedStyle(el);
      if (cs.whiteSpace === "nowrap") continue;
      // Only plain labels: a control that holds a truncated name or a count is checked by single-line.
      if ([...el.children].some(c => !c.matches(".spinner, .action-label, svg"))) continue;
      measure.font = `${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
      const label = [...el.querySelectorAll(".action-label")][0] || el;
      const box = label.getBoundingClientRect();
      const room = box.width - (label === el ? parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight) : 0)
        - (label === el ? [...el.children].filter(c => c.matches?.(".spinner, .jd-chevron") && getComputedStyle(c).display !== "none").reduce((sum, c) => sum + c.getBoundingClientRect().width + parseFloat(cs.columnGap || 0), 0) : 0);
      for (const word of el.textContent.trim().split(/\s+/)) {
        if (word.length > 3 && measure.measureText(word).width > room + 1 && !/^[\u3000-\u9fff]/.test(word)) { add("word-broken", `“${word}” does not fit the ${Math.round(room)}px its control leaves for it`, el); break; }
      }
    }

    // 3. Controls do not overlap each other.
    for (let i = 0; i < interactive.length; i++) {
      for (let j = i + 1; j < interactive.length; j++) {
        const a = interactive[i], c = interactive[j];
        if (a.contains(c) || c.contains(a)) continue;
        if (Boolean(pinned(a)) !== Boolean(pinned(c))) continue;
        const ra = rect(a), rc = rect(c);
        const w = Math.min(ra.right, rc.right) - Math.max(ra.left, rc.left);
        const h = Math.min(ra.bottom, rc.bottom) - Math.max(ra.top, rc.top);
        if (w > b.overlapTolerance && h > b.overlapTolerance) add("overlap", `overlaps ${describe(c)} by ${Math.round(w)}×${Math.round(h)}px`, a);
      }
    }

    // 4. Control size: no sliver, no slab. A control is as wide as its label
    //    needs, never a full-width wall of outline unless it is the one primary
    //    action of the bar.
    const coarse = matchMedia("(pointer: coarse)").matches;
    const minTarget = coarse ? b.minTargetTouch : b.minTarget;
    for (const el of interactive) {
      if (el.matches("input[type=checkbox], input[type=radio]")) continue;
      const r = rect(el);
      if (Math.min(r.width, r.height) < minTarget - 0.5 && !el.closest("[data-inline-control]")) add("target-small", `${Math.round(r.width)}×${Math.round(r.height)}px is under ${minTarget}px`, el);
      if (el.tagName === "BUTTON" && r.height > b.maxButtonHeight) add("button-tall", `${Math.round(r.height)}px tall (max ${b.maxButtonHeight}); label wraps or padding is too big`, el);
    }

    // 4b. Neighbouring controls keep a gap, so a thumb lands on one of them.
    for (let i = 0; i < interactive.length; i++) {
      for (let j = i + 1; j < interactive.length; j++) {
        const a = interactive[i], c = interactive[j];
        if (a.contains(c) || c.contains(a) || Boolean(pinned(a)) !== Boolean(pinned(c))) continue;
        const ra = rect(a), rc = rect(c);
        // Two generous targets (44px both ways) side by side cannot be mistaken for each other.
        if (Math.min(ra.width, ra.height, rc.width, rc.height) >= 44) continue;
        const overlapX = Math.min(ra.right, rc.right) - Math.max(ra.left, rc.left), overlapY = Math.min(ra.bottom, rc.bottom) - Math.max(ra.top, rc.top);
        const gapX = Math.max(ra.left, rc.left) - Math.min(ra.right, rc.right), gapY = Math.max(ra.top, rc.top) - Math.min(ra.bottom, rc.bottom);
        const side = overlapY > 4 && gapX >= -b.overlapTolerance && gapX < b.minGap, stacked = overlapX > 4 && gapY >= -b.overlapTolerance && gapY < b.minGap;
        if (side || stacked) add("target-gap", `${Math.max(0, Math.round(side ? gapX : gapY))}px from ${describe(c)}`, a);
      }
    }

    // 5. Controls have a name a screen reader can read.
    for (const el of interactive) {
      if (el.matches("input, select, textarea")) continue;
      const name = (el.getAttribute("aria-label") || el.getAttribute("title") || textOf(el)).trim();
      if (!name) add("unnamed-control", "control has no text, aria-label or title", el);
    }

    // 6. Text that must stay on one line does. Marked by the markup
    //    (data-single-line) so a wrap is a decision, not an accident.
    for (const el of all) {
      if (!el.matches("[data-single-line]")) continue;
      const style = getComputedStyle(el);
      const line = parseFloat(style.lineHeight) || parseFloat(style.fontSize) * 1.3;
      const lines = rect(el).height / line;
      if (lines > b.singleLineSlack) add("single-line", `wraps to ${lines.toFixed(1)} lines: “${textOf(el).slice(0, 50)}”`, el);
    }

    // 7. Text is never silently cut. Cutting is allowed only where the markup
    //    chose to (ellipsis or line clamp) and the full text is reachable.
    for (const el of all) {
      const style = getComputedStyle(el);
      const clips = /(hidden|clip)/.test(style.overflowX) || /(hidden|clip)/.test(style.overflowY);
      if (!clips || !el.firstChild) continue;
      const wide = el.scrollWidth > el.clientWidth + 1, tall = el.scrollHeight > el.clientHeight + 1;
      if (!wide && !tall) continue;
      const chosen = style.textOverflow === "ellipsis" || style.webkitLineClamp !== "none" && style.webkitLineClamp !== "" && style.webkitLineClamp !== undefined;
      if (chosen) {
        if (!el.title && !el.getAttribute("aria-label") && !el.closest("[title]")) add("clipped-no-title", "text is truncated and its full text is not reachable", el);
        continue;
      }
      if (el === dialog || el.matches("progress, meter, .bar, .bar *") || !textOf(el)) continue;
      add("clipped", `${wide ? "width" : "height"} cuts the content`, el);
    }

    // 8. Progress bars are one component: same height per kind, full width of
    //    their column, one fill colour. Native <progress>/<meter> draw
    //    differently per browser and are not allowed.
    const bars = all.filter(el => el.matches("progress, meter, [role=progressbar], [data-bar]"));
    for (const el of bars) {
      if (el.matches("progress, meter")) { add("bar-native", "native progress/meter; use the bar component", el); continue; }
      const kind = el.dataset.bar;
      if (!kind || !(kind in b.barHeights)) { add("bar-kind", `bar has no known data-bar (${Object.keys(b.barHeights).join("|")})`, el); continue; }
      const r = rect(el);
      if (Math.abs(r.height - b.barHeights[kind]) > 0.6) add("bar-height", `${kind} bar is ${r.height.toFixed(1)}px, expected ${b.barHeights[kind]}`, el);
      const parent = el.parentElement;
      if (kind === "hero" && parent && Math.abs(r.width - parent.clientWidth + parseFloat(getComputedStyle(parent).paddingLeft) + parseFloat(getComputedStyle(parent).paddingRight)) > 2) add("bar-width", `hero bar is ${Math.round(r.width)}px in a ${parent.clientWidth}px column`, el);
      const fill = el.querySelector("[data-fill]");
      if (fill) {
        const color = getComputedStyle(fill).backgroundColor;
        (run.fills ||= new Set()).add(`${kind}:${color}`);
        const value = Number(el.getAttribute("aria-valuenow")), max = Number(el.getAttribute("aria-valuemax") || 100);
        if (Number.isFinite(value) && Math.abs(rect(fill).width / Math.max(1, r.width) * max - value) > Math.max(1, max * 0.02)) add("bar-fill", `fill ${(rect(fill).width / r.width * 100).toFixed(0)}% does not match aria-valuenow ${value}/${max}`, el);
      }
    }
    const fills = [...(run.fills || [])];
    delete run.fills;
    const colors = new Map();
    for (const f of fills) { const [k, c] = [f.slice(0, f.indexOf(":")), f.slice(f.indexOf(":") + 1)]; (colors.get(k) || colors.set(k, new Set()).get(k)).add(c); }
    for (const [k, set] of colors) if (set.size > 1) add("bar-color", `${k} bars use ${set.size} fill colours (${[...set].join(" ")})`, dialog);

    // 9. Spinners and status dots are round, centred on their text line, and
    //    not clipped by the box they sit in.
    for (const el of all) {
      if (!el.matches("[data-spinner], [data-dot]")) continue;
      const r = rect(el);
      if (Math.abs(r.width - r.height) > 0.6) add("round-shape", `${r.width.toFixed(1)}×${r.height.toFixed(1)} is not square`, el);
      const row = el.parentElement;
      const sibling = [...row.children].find(c => c !== el && visible(c) && textOf(c));
      if (sibling) {
        const range = document.createRange();
        range.selectNodeContents(sibling);
        const lines = [...range.getClientRects()].filter(q => q.width > 0);
        const first = lines[0];
        if (first) {
          const textMid = (first.top + first.bottom) / 2, mark = (r.top + r.bottom) / 2;
          if (Math.abs(textMid - mark) > 2.5) add("misaligned", `centre is ${Math.abs(textMid - mark).toFixed(1)}px off the text line`, el);
        }
      }
      const pr = rect(row);
      if (r.left < pr.left - 0.5 || r.right > pr.right + 0.5 || r.top < pr.top - 0.5 || r.bottom > pr.bottom + 0.5) add("mark-clipped", "mark leaves the box that holds it", el);
    }

    // 10. Readable text: size and contrast.
    function backgroundOf(el) {
      for (let p = el; p; p = p.parentElement) {
        const c = getComputedStyle(p).backgroundColor;
        const m = /rgba?\(([\d.]+)[, ]+([\d.]+)[, ]+([\d.]+)(?:[,/ ]+([\d.]+))?/.exec(c);
        if (m && (m[4] === undefined || Number(m[4]) > 0.95)) return [Number(m[1]), Number(m[2]), Number(m[3])];
      }
      return [255, 255, 255];
    }
    const lum = ([r, g, bl]) => { const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(bl); };
    const parse = c => { const m = /rgba?\(([\d.]+)[, ]+([\d.]+)[, ]+([\d.]+)(?:[,/ ]+([\d.]+))?/.exec(c); return m ? [Number(m[1]), Number(m[2]), Number(m[3]), m[4] === undefined ? 1 : Number(m[4])] : [0, 0, 0, 1]; };
    const seenContrast = new Set();
    for (const el of all) {
      const own = [...el.childNodes].some(n => n.nodeType === 3 && n.textContent.trim());
      if (!own) continue;
      const style = getComputedStyle(el);
      const size = parseFloat(style.fontSize);
      if (size < b.minFont) add("font-small", `${size}px text (min ${b.minFont})`, el);
      const [r, g, bl, a] = parse(style.color);
      const bg = backgroundOf(el);
      const fg = [r, g, bl].map((v, i) => v * a + bg[i] * (1 - a));
      const l1 = lum(fg), l2 = lum(bg), ratio = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
      const large = size >= 24 || size >= 18.66 && Number(style.fontWeight) >= 700;
      const need = large ? b.contrastLarge : b.contrast;
      const key = `${style.color}|${bg}|${size}`;
      if (ratio < need && !seenContrast.has(key) && !el.closest("[disabled], [aria-disabled=true]")) { seenContrast.add(key); add("contrast", `${ratio.toFixed(2)}:1 (needs ${need}:1) for “${textOf(el).slice(0, 30)}”`, el); }
    }

    // 11. Fixed anatomy: the named sections appear in one order, once each.
    const sections = [...dialog.querySelectorAll("[data-section]")].filter(visible);
    const names = sections.map(s => s.dataset.section);
    for (const name of new Set(names)) if (names.filter(n => n === name).length > 1) add("section-repeated", `section “${name}” appears ${names.filter(n => n === name).length} times`, dialog);
    const order = names.map(n => b.sections.indexOf(n)).filter(i => i >= 0);
    if (order.some((v, i) => i && v < order[i - 1])) add("section-order", `sections are ${names.join(" → ")}; expected ${b.sections.join(" → ")}`, dialog);
    for (const required of b.requiredSections) if (!names.includes(required)) add("section-missing", `section “${required}” is missing`, dialog);
    for (const el of sections) { const r = rect(el); if (r.top < b.overlapTolerance * -1 && el.dataset.section !== "header" && !/sticky|fixed/.test(getComputedStyle(el).position)) { /* scrolled; fine */ } }

    // 12. One statement per fact. The same sentence, or one inside another,
    //     is not repeated across the header and the status block.
    const statements = [];
    for (const el of dialog.querySelectorAll("[data-section=header] *, [data-section=status] *")) {
      if (!visible(el) || ![...el.childNodes].some(n => n.nodeType === 3 && n.textContent.trim())) continue;
      if (el.closest("[data-allow-repeat]")) continue;
      const t = textOf(el).toLowerCase().replace(/[·•:,.]/g, " ").replace(/\s+/g, " ").trim();
      if (t.length >= 8) statements.push({ t, el });
    }
    for (let i = 0; i < statements.length; i++) {
      for (let j = i + 1; j < statements.length; j++) {
        const a = statements[i], c = statements[j];
        if (a.el.contains(c.el) || c.el.contains(a.el)) continue;
        if (a.t === c.t || (a.t.length > 14 && c.t.includes(a.t)) || (c.t.length > 14 && a.t.includes(c.t))) add("repeated-text", `“${(a.t.length < c.t.length ? a.t : c.t).slice(0, 40)}” is said twice`, c.el);
      }
    }

    // 13. How many controls each region shows.
    const bar = dialog.querySelector("[data-section=actions]");
    if (bar) {
      // The open More menu lists what is behind its toggle; only the bar itself is counted.
      const n = [...bar.querySelectorAll("button, a[href], summary")].filter(c => visible(c) && !c.closest(".more-menu")).length;
      if (n > b.maxBarControls) add("too-many-controls", `${n} controls in the action bar (max ${b.maxBarControls})`, bar);
      // Two buttons and a More menu, or two buttons: a third plain button is a wall in the making.
      const plain = [...bar.querySelectorAll("button")].filter(c => visible(c) && !c.closest(".more-menu")).length;
      if (plain > 2) add("too-many-controls", `${plain} buttons beside the More menu (max 2)`, bar);
    }
    for (const row of dialog.querySelectorAll("[data-file-row]")) {
      if (!visible(row)) continue;
      const open = row.dataset.open === "true";
      const n = [...row.querySelectorAll("button, a[href]")].filter(c => visible(c) && !c.closest("[data-row-body]")).length;
      if (!open && n > b.maxRowControls) add("too-many-controls", `collapsed row shows ${n} buttons (max ${b.maxRowControls})`, row);
    }

    // 14. The bars the dialog pins (header, action bar) stay inside the screen
    //     and leave the content reachable: the last thing in the dialog can be
    //     scrolled above the action bar.
    const header = dialog.querySelector("[data-section=header]");
    if (header) { const r = rect(header); if (r.height > innerHeight * 0.4) add("header-tall", `header takes ${Math.round(r.height)}px of ${innerHeight}px`, header); }
    if (bar && /sticky|fixed/.test(getComputedStyle(bar).position)) {
      const r = rect(bar);
      if (r.bottom > innerHeight + 1 || r.top < 0) add("bar-offscreen", "pinned action bar is outside the screen", bar);
      if (r.height > innerHeight * 0.3) add("bar-tall", `action bar takes ${Math.round(r.height)}px of ${innerHeight}px`, bar);
    }

    // 14b. Content can be scrolled clear of the pinned bars, at both ends.
    const pinnedBars = [...dialog.querySelectorAll("*")].filter(el => visible(el) && /sticky|fixed/.test(getComputedStyle(el).position) && el !== dialog);
    const content = all.filter(el => !pinned(el) && el.children.length === 0 && textOf(el));
    if (content.length) {
      const atTop = dialog.scrollTop <= 1, atBottom = dialog.scrollTop + dialog.clientHeight >= dialog.scrollHeight - 1;
      const first = content.reduce((m, el) => (rect(el).top < rect(m).top ? el : m)), last = content.reduce((m, el) => (rect(el).bottom > rect(m).bottom ? el : m));
      for (const bar of pinnedBars) {
        const r = rect(bar);
        if (atBottom && r.top > innerHeight * 0.5 && rect(last).bottom > r.top + 1) add("content-under-bar", `the last line is ${Math.round(rect(last).bottom - r.top)}px under the bottom bar at the end of the scroll`, last);
        if (atTop && r.bottom < innerHeight * 0.5 && rect(first).top < r.bottom - 1 && first !== bar && !bar.contains(first)) add("content-under-bar", `the first line is ${Math.round(r.bottom - rect(first).top)}px under the top bar at rest`, first);
      }
    }

    // 15. A state does not need the dialog to be taller than it must be: an
    //     empty region keeps no space.
    for (const el of dialog.querySelectorAll("[data-section]")) {
      if (!visible(el)) continue;
      if (!textOf(el) && !el.querySelector("button, progress, [data-bar], img, svg") && rect(el).height > 4) add("empty-space", "section is empty but takes room", el);
    }

    return out;
  };
}
