// Hard rules for buttons, everywhere the checks look.
//   button-height  buttons that sit side by side are the same height.
//   button-wrap    a button's label is one line.
//
// Buttons that sit side by side are the same height. Widths may
// differ with their labels; a taller or shorter neighbour never passes, however
// it came about (a label that wraps, a different padding, a stray override).
//
// "Side by side" is what the browser laid out: visible buttons that share a
// parent and overlap vertically. Evaluated inside the page, so it reads
// rectangles, not class names. Returns [{rule, message, where}].
export const buttonHeightTolerance = 1;

export function installButtonHeights() {
  return function run(selector, tolerance) {
    const root = document.querySelector(selector);
    if (!root) return [];
    const describe = el => {
      const text = (el.getAttribute("aria-label") || el.textContent || "").trim().replace(/\s+/g, " ").slice(0, 30);
      return `${el.tagName.toLowerCase()}${el.id ? `#${el.id}` : ""} “${text}”`;
    };
    const shown = el => {
      const s = getComputedStyle(el);
      if (s.display === "none" || s.visibility === "hidden" || el.hidden) return false;
      for (let p = el.parentElement; p && p !== root.parentElement; p = p.parentElement) if (p.hidden || getComputedStyle(p).display === "none") return false;
      const r = el.getBoundingClientRect();
      return r.width > 0.5 && r.height > 0.5;
    };
    const buttons = [root, ...root.querySelectorAll("*")].flatMap(el => (el === root ? [] : [el])).filter(el => el.matches("button, [role=button], input[type=button], input[type=submit]") && shown(el));
    const byParent = new Map();
    for (const b of buttons) (byParent.get(b.parentElement) || byParent.set(b.parentElement, []).get(b.parentElement)).push(b);
    const out = [];
    for (const b of buttons) {
      // A row made of a button (a name and a status badge) is a composite, not an
      // action label: it may stack its parts. An action label is one run of text.
      const walker = document.createTreeWalker(b, NodeFilter.SHOW_TEXT);
      const owners = new Set(), tops = new Set();
      for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        if (!n.textContent.trim()) continue;
        owners.add(n.parentElement);
        const range = document.createRange();
        range.selectNodeContents(n);
        for (const r of range.getClientRects()) if (r.width > 0.5 && r.height > 0.5) tops.add(Math.round((r.top + r.height / 2) / 4));
      }
      if (owners.size > 1) continue;
      if (tops.size > 1) out.push({ rule: "button-wrap", message: `label runs over ${tops.size} lines`, where: describe(b) });
    }
    for (const group of byParent.values()) {
      if (group.length < 2) continue;
      for (let i = 0; i < group.length; i++) {
        for (let j = i + 1; j < group.length; j++) {
          const a = group[i].getBoundingClientRect(), c = group[j].getBoundingClientRect();
          const overlap = Math.min(a.bottom, c.bottom) - Math.max(a.top, c.top);
          if (overlap < Math.min(a.height, c.height) * 0.5) continue; // another row
          if (Math.abs(a.height - c.height) > tolerance) out.push({ rule: "button-height", message: `${a.height.toFixed(1)}px beside ${describe(group[j])} at ${c.height.toFixed(1)}px`, where: describe(group[i]) });
        }
      }
    }
    return out;
  };
}
