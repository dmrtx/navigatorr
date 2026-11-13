import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("./assets/app.js", import.meta.url), "utf8");
const start = source.indexOf("// builds:start");
const end = source.indexOf("// builds:end");
assert.ok(start >= 0 && end > start, "builds block found");

class El {
  constructor(tag, text = "", className = "") {
    this.tag = tag;
    this.textContent = text;
    this.className = className;
    this.dataset = {};
    this.children = [];
  }
  append(...items) { this.children.push(...items); }
  replaceChildren() { this.children = []; }
}
const context = vm.createContext({ node: (tag, text = "", className = "") => new El(tag, text, className) });
vm.runInContext(source.slice(start, end) + "\nthis.api={formatBuildDuration,buildDuration,buildHeartbeatAge,sortBuilds,nodeBuildLines,renderBuilds,buildColumns};", context);
const ui = context.api;

const t0 = Date.parse("2026-11-13T10:00:00Z");
const build = (over) => ({ id: "b", target: "m1", source: "mdm-stack", holder: "itp1", kind: "gradle", title: "aosp-enrollment", priority: "normal", status: "running", created_at: "2026-11-13T10:00:00Z", started_at: "2026-11-13T10:00:00Z", last_heartbeat_at: "2026-11-13T10:11:30Z", ...over });

test("durations read as minutes and hours", () => {
  assert.equal(ui.formatBuildDuration(20_000), "<1 min");
  assert.equal(ui.formatBuildDuration(12 * 60_000), "12 min");
  assert.equal(ui.formatBuildDuration(60 * 60_000), "1 h");
  assert.equal(ui.formatBuildDuration(125 * 60_000), "2 h 5 min");
  assert.equal(ui.buildDuration(build(), t0 + 12 * 60_000), "12 min");
  assert.equal(ui.buildDuration(build({ status: "succeeded", ended_at: "2026-11-13T10:30:00Z" }), t0 + 99 * 60_000), "30 min");
  // A waiting build has no start yet: its age counts from creation.
  assert.equal(ui.buildDuration(build({ status: "waiting", started_at: "" }), t0 + 3 * 60_000), "3 min");
  assert.equal(ui.buildHeartbeatAge(build(), t0 + 11 * 60_000 + 45_000), "15 s ago");
});

test("the node card line names the build and who holds it, for that node's builds only", () => {
  const lines = ui.nodeBuildLines("m1", [build()], t0 + 12 * 60_000);
  assert.deepEqual(Array.from(lines), ["m1 busy: build aosp-enrollment (mdm-stack:itp1), 12 min"]);
  assert.deepEqual(Array.from(ui.nodeBuildLines("m1", [build({ holder: "" })], t0)), ["m1 busy: build aosp-enrollment (mdm-stack), <1 min"]);
  assert.match(ui.nodeBuildLines("m1", [build({ status: "waiting" })], t0)[0], /waiting for an idle node/);
  assert.equal(ui.nodeBuildLines("m1", undefined).length, 0);
});

test("running builds come first, then waiting, then history in server order", () => {
  const rows = [
    build({ id: "old", status: "succeeded", created_at: "2026-11-13T09:00:00Z" }),
    build({ id: "wait", status: "waiting", created_at: "2026-11-13T09:30:00Z" }),
    build({ id: "run2", created_at: "2026-11-13T09:40:00Z" }),
    build({ id: "older", status: "failed", created_at: "2026-11-13T08:00:00Z" }),
    build({ id: "run1", created_at: "2026-11-13T09:10:00Z" }),
  ];
  assert.deepEqual(Array.from(ui.sortBuilds(rows), (b) => b.id), ["run1", "run2", "wait", "old", "older"]);
});

test("the table shows every target with all ten columns and a status per row", () => {
  const host = new El("div");
  ui.renderBuilds(host, [build({ target: "laptop" }), build({ id: "x", status: "failed", ended_at: "2026-11-13T10:05:00Z" })], t0 + 60_000);
  const table = host.children[0];
  const [thead, tbody] = table.children;
  assert.deepEqual(Array.from(thead.children[0].children, (c) => c.textContent), Array.from(ui.buildColumns));
  assert.equal(tbody.children.length, 2);
  const first = tbody.children[0].children;
  assert.equal(first.length, 10);
  assert.equal(first[0].textContent, "laptop");
  assert.equal(first[6].dataset.status, "running");
  assert.equal(tbody.children[1].children[8].textContent, "", "history shows no live heartbeat age");
  ui.renderBuilds(host, []);
  assert.equal(host.children[0].textContent, "No builds yet.");
});
