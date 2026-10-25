// Layout rules for one row of the Activity list, evaluated inside the page.
//
// The detail dialog has its own rules (invariants.mjs). A row is a smaller
// surface with its own failure: a progress bar squeezed between two blocks of
// text that wrap and resize on every update. Every rule here reads what the
// browser laid out, never class names, so it keeps catching the problem
// whatever markup draws the row.
//
// A violation is {rule, message, where}.

export const listBudgets = {
  // A short label ("1.48 GB / 3.37 GB", "43%", "Replacing") never wraps.
  shortText: 24,
  // A bar spans its whole progress block, on a line of its own.
  barMinShare: 0.9,
  // Tallest the row may be per file shown (a runaway wrap, not a design limit).
  maxRowHeight: 340,
  // The list is the compact surface: its icon buttons and badges are smaller
  // than the dialog's. These are the floors, not the dialog's budgets.
  minFont: 9,
  overlapTolerance: 1.5,
  // The title keeps room for a few letters however many buttons share its line.
  minTitleEm: 7,
  minTargetTouch: 28,
  minTarget: 28,
};

export function installList() {
  return function run(selector, b) {
    const row = document.querySelector(selector);
    const out = [];
    const add = (rule, message, el) => out.push({ rule, message, where: describe(el) });
    if (!row) return [{ rule: "present", message: `${selector} is not in the page`, where: "" }];
    function describe(el) {
      if (!el || !el.tagName) return "";
      const text = (el.getAttribute?.("aria-label") || el.textContent || "").trim().replace(/\s+/g, " ").slice(0, 40);
      const cls = typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).slice(0, 2).join(".") : "";
      return `${el.tagName.toLowerCase()}${cls}${text ? ` “${text}”` : ""}`;
    }
    const visible = el => {
      if (!(el instanceof Element)) return false;
      const style = getComputedStyle(el);
      if (style.display === "none" || style.visibility === "hidden" || Number(style.opacity) === 0) return false;
      const r = el.getBoundingClientRect();
      if (r.width < 0.5 || r.height < 0.5) return false;
      for (let p = el.parentElement; p && p !== row.parentElement; p = p.parentElement) {
        if (p.hidden) return false;
        if (p.tagName === "DETAILS" && !p.open && !(el.tagName === "SUMMARY" && el.parentElement === p)) {
          const summary = [...p.children].find(c => c.tagName === "SUMMARY");
          if (!summary || !summary.contains(el)) return false;
        }
      }
      return true;
    };
    const rect = el => el.getBoundingClientRect();
    const textOf = el => (el.textContent || "").replace(/\s+/g, " ").trim();
    // The part of a text box a clipping ancestor lets through: a line clamped
    // away is not drawn, so it cannot overlap anything.
    const shown = (q, el) => {
      let left = q.left, right = q.right, top = q.top, bottom = q.bottom;
      for (let p = el; p && p !== row.parentElement; p = p.parentElement) {
        const s = getComputedStyle(p);
        if (!/(hidden|clip)/.test(s.overflowX + s.overflowY)) continue;
        const c = p.getBoundingClientRect();
        left = Math.max(left, c.left); right = Math.min(right, c.right); top = Math.max(top, c.top); bottom = Math.min(bottom, c.bottom);
      }
      return { left, right, top, bottom, width: Math.max(0, right - left), height: Math.max(0, bottom - top) };
    };
    // The box the text sits in: padding is room the box keeps free on purpose.
    const inner = el => {
      const r = el.getBoundingClientRect(), s = getComputedStyle(el);
      return { left: r.left + parseFloat(s.paddingLeft), right: r.right - parseFloat(s.paddingRight), top: r.top + parseFloat(s.paddingTop), bottom: r.bottom - parseFloat(s.paddingBottom) };
    };
    const all = [...row.querySelectorAll("*")].filter(visible);
    const rr = rect(row);
    const isBar = el => el.matches("progress, meter, [role=progressbar], [data-bar], .batch-segments, .segment-bar");
    const interactive = all.filter(el => el.matches("button, a[href], summary, input, select, textarea, [role=button]"));

    // 1. Nothing leaves the row sideways, and the page never scrolls sideways.
    // The row's own scrollWidth is not used: the invisible touch area around an
    // icon button reaches past the row without drawing or scrolling anything.
    if (document.documentElement.scrollWidth > document.documentElement.clientWidth + 1) add("list-overflow-x", `page scrolls sideways (${document.documentElement.scrollWidth} > ${document.documentElement.clientWidth})`, document.documentElement);
    for (const el of all) {
      if (el.closest(".sr-only")) continue;
      const r = rect(el);
      if (r.right > rr.right + 1 || r.left < rr.left - 1) add("list-overflow-x", `leaves the row by ${Math.round(Math.max(r.right - rr.right, rr.left - r.left))}px`, el);
    }

    // 2. A short label is one line. A measurement or a count that wraps
    //    ("1.48 / GB / 3.37 / GB" over four lines) is unreadable, and its box
    //    changes size with every update.
    for (const el of all) {
      if (el.children.length || !textOf(el) || isBar(el)) continue;
      if (el.closest("[data-allow-wrap]")) continue;
      // A title is clamped to two lines on purpose; a name with no spaces breaks anywhere.
      if (el.matches("a.job-title")) continue;
      const text = textOf(el);
      if (text.length > b.shortText) continue;
      // A sentence ("Measuring final quality") may take a second line on a
      // very narrow screen; a measurement or a count ("1.48 GB / 3.37 GB") may not.
      if ((text.match(/[A-Za-z\u00C0-\u024F]{4,}/g) || []).length >= 2) continue;
      const style = getComputedStyle(el);
      const line = parseFloat(style.lineHeight) || parseFloat(style.fontSize) * 1.3;
      if (rect(el).height / line > 1.45) add("list-short-wrap", `“${text}” wraps to ${(rect(el).height / line).toFixed(1)} lines`, el);
    }

    // 3. A bar has a line of its own and most of the row's width. Text beside
    //    it squeezes the bar and the text, and the arrangement changes as the
    //    numbers change.
    // A bar that has collapsed to nothing says nothing about progress. It is
    // not in `all`, which only holds what has a size.
    for (const bar of row.querySelectorAll("progress, meter, [role=progressbar], .batch-segments")) {
      if (getComputedStyle(bar).display === "none" || bar.closest("[hidden]") || bar.closest(".sr-only")) continue;
      if (rect(bar).height < 3) add("list-bar-invisible", `bar is ${Math.round(rect(bar).width)}×${rect(bar).height.toFixed(1)}px`, bar);
    }
    for (const bar of all.filter(isBar)) {
      const r = rect(bar);
      const block = bar.parentElement, bs = getComputedStyle(block);
      const room = block.clientWidth - parseFloat(bs.paddingLeft) - parseFloat(bs.paddingRight);
      if (r.width < room * b.barMinShare) add("list-bar-narrow", `bar is ${Math.round(r.width)}px in a ${Math.round(room)}px block`, bar);
      for (const el of all) {
        if (el === bar || bar.contains(el) || el.contains(bar) || el.children.length || !textOf(el)) continue;
        const range = document.createRange();
        range.selectNodeContents(el);
        for (const raw of range.getClientRects()) {
          const q = shown(raw, el);
          if (q.width < 1 || q.height < 1) continue;
          const shared = Math.min(q.bottom, r.bottom) - Math.max(q.top, r.top);
          const apart = Math.max(q.left, r.left) - Math.min(q.right, r.right);
          if (shared > 2 && apart > -b.overlapTolerance) { add("list-bar-shares-line", `text “${textOf(el).slice(0, 30)}” sits on the bar's line`, bar); break; }
          if (shared > b.overlapTolerance && apart <= -b.overlapTolerance) { add("list-overlap", `text “${textOf(el).slice(0, 30)}” overlaps the bar`, bar); break; }
        }
      }
    }

    // 4. Visible boxes with text do not overlap their neighbours.
    const leaves = all.filter(el => !el.children.length && textOf(el) && !isBar(el));
    for (let i = 0; i < leaves.length; i++) {
      for (let j = i + 1; j < leaves.length; j++) {
        const a = leaves[i], c = leaves[j];
        if (a.contains(c) || c.contains(a)) continue;
        const ra = inner(a), rc = inner(c);
        const w = Math.min(ra.right, rc.right) - Math.max(ra.left, rc.left);
        const h = Math.min(ra.bottom, rc.bottom) - Math.max(ra.top, rc.top);
        if (w > b.overlapTolerance && h > b.overlapTolerance) add("list-overlap", `overlaps ${describe(c)} by ${Math.round(w)}×${Math.round(h)}px`, a);
      }
    }

    // 5. Controls: covered, touchable, and apart.
    const coarse = matchMedia("(pointer: coarse)").matches;
    const minTarget = coarse ? b.minTargetTouch : b.minTarget;
    for (const el of interactive) {
      const r = rect(el);
      // A title or a button inside a sentence is a link in running text, not a
      // free-standing button; WCAG exempts those from the target size.
      if (Math.min(r.width, r.height) < minTarget - 0.5 && !el.matches("a.job-title, p button, p a")) add("list-target-small", `${Math.round(r.width)}×${Math.round(r.height)}px is under ${minTarget}px`, el);
      const x = Math.min(Math.max((r.left + r.right) / 2, 0), innerWidth - 1), y = (r.top + r.bottom) / 2;
      if (y < 0 || y > innerHeight) continue;
      const top = document.elementFromPoint(x, y);
      if (top && top !== el && !el.contains(top) && !top.contains(el)) add("list-covered", `covered by ${describe(top)}`, el);
    }
    for (let i = 0; i < interactive.length; i++) {
      for (let j = i + 1; j < interactive.length; j++) {
        const a = interactive[i], c = interactive[j];
        if (a.contains(c) || c.contains(a)) continue;
        const ra = rect(a), rc = rect(c);
        const w = Math.min(ra.right, rc.right) - Math.max(ra.left, rc.left);
        const h = Math.min(ra.bottom, rc.bottom) - Math.max(ra.top, rc.top);
        if (w > b.overlapTolerance && h > b.overlapTolerance) add("list-overlap", `control overlaps ${describe(c)} by ${Math.round(w)}×${Math.round(h)}px`, a);
      }
    }

    // 6. Text is never silently cut: a clipping box either shows an ellipsis
    //    with the full text reachable, or it does not cut.
    for (const el of all) {
      const style = getComputedStyle(el);
      const clips = /(hidden|clip)/.test(style.overflowX) || /(hidden|clip)/.test(style.overflowY);
      if (!clips || !el.firstChild || isBar(el)) continue;
      const wide = el.scrollWidth > el.clientWidth + 1, tall = el.scrollHeight > el.clientHeight + 1;
      if (!wide && !tall) continue;
      const chosen = style.textOverflow === "ellipsis" || (style.webkitLineClamp && style.webkitLineClamp !== "none");
      if (chosen) { if (!el.title && !el.getAttribute("aria-label") && !el.closest("[title]")) add("list-clipped-no-title", "text is truncated and its full text is not reachable", el); continue; }
      if (!textOf(el)) continue;
      add("list-clipped", `${wide ? "width" : "height"} cuts the content`, el);
    }

    // A title squeezed by the buttons beside it shows one letter and an ellipsis.
    for (const el of all) {
      if (!el.matches("a.job-title")) continue;
      const em = parseFloat(getComputedStyle(el).fontSize);
      if (rect(el).width < b.minTitleEm * em) add("list-title-squeezed", `title is ${Math.round(rect(el).width)}px wide (under ${b.minTitleEm}em)`, el);
    }

    // 7. Text is readable in size, and a row is not a wall.
    for (const el of all) {
      if (![...el.childNodes].some(n => n.nodeType === 3 && n.textContent.trim())) continue;
      const size = parseFloat(getComputedStyle(el).fontSize);
      if (size < b.minFont) add("list-font-small", `${size}px text (min ${b.minFont})`, el);
    }
    if (rr.height > b.maxRowHeight) add("list-row-tall", `row is ${Math.round(rr.height)}px tall (max ${b.maxRowHeight})`, row);
    return out;
  };
}
