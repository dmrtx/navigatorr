import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(
  new URL("./assets/app.js", import.meta.url),
  "utf8",
);
class Element {
  children = [];
  listeners = new Map();
  dataset = {};
  hidden = false;
  disabled = false;
  value = "";
  textContent = "";
  open = false;
  addEventListener(name, fn) {
    this.listeners.set(name, fn);
  }
  setAttribute() {}
  append(...children) {
    this.children.push(...children);
  }
  replaceChildren(...children) {
    this.children = children;
  }
  contains(element) {
    return this.children.includes(element);
  }
  querySelectorAll() {
    return this.children
      .flatMap((c) => [c, ...c.querySelectorAll()])
      .filter((c) => c.dataset.jobControl);
  }
  showModal() {
    this.open = true;
  }
  close() {
    this.open = false;
  }
  focus() {}
}
function harness() {
  const elements = new Map();
  let interval;
  const document = {
    hidden: false,
    activeElement: null,
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, new Element());
      return elements.get(id);
    },
    createElement: () => new Element(),
    querySelectorAll: () => [],
  };
  const context = vm.createContext({
    document,
    console,
    URLSearchParams,
    confirm: () => true,
    setTimeout: () => 0,
    clearTimeout() {},
    setInterval(fn) {
      interval = fn;
    },
  });
  vm.runInContext(source.replace(/safe\(initialize\);\s*$/, ""), context);
  vm.runInContext("state.info = {allow_destructive:false};", context);
  return {
    context,
    elements,
    document,
    interval,
    run: (code) => vm.runInContext(code, context),
  };
}
function deferred() {
  let resolve;
  const promise = new Promise((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

test("late detail response cannot overwrite the newly opened job", async () => {
  const h = harness(),
    a = deferred(),
    b = deferred();
  h.context.statusRequests = { a: a.promise, b: b.promise };
  h.run(
    'api = async (path) => ({jobs:[await statusRequests[path.split("=")[1]]]}); tool = async () => ({data:{}});',
  );
  const first = h.run('openJob("a")');
  const second = h.run('openJob("b")');
  b.resolve({
    id: "b",
    status: "completed",
    action_name: "benchmark_transcode",
  });
  await second;
  a.resolve({ id: "a", status: "failed", action_name: "transcode_media" });
  await first;
  assert.equal(h.elements.get("detail-summary").children[0].textContent, "b");
  assert.equal(h.run("state.detailJob.id"), "b");
});

test("inline retry controls retain the rendered job id", async () => {
  const h = harness(),
    calls = [];
  h.context.record = (name, args) => calls.push({ name, args });
  h.run(
    "tool = async (name,args) => {record(name,args); return {};}; loadJobs = async () => {};",
  );
  const controls = h.run(
    'jobControls({id:"failed-a",status:"failed",action_name:"transcode_media"})',
  );
  h.run('state.detail = "other-b";');
  await controls.children
    .find((b) => b.textContent === "Reintentar")
    .listeners.get("click")();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].name, "action_retry");
  assert.equal(calls[0].args.id, "failed-a");
});

test("batch approval first shows the concrete plan without resuming", async () => {
  const h = harness(),
    calls = [];
  h.context.plan = {
    batch_id: "batch-a",
    digest: "sha256:plan",
    members: [
      {
        item_key: "episode-1",
        original_path: "/source.mkv",
        candidate_path: "/candidate.mkv",
      },
    ],
  };
  h.context.record = (name) => calls.push(name);
  h.run("tool = async (name) => {record(name); return {data:plan};};");
  const controls = h.run(
    'jobControls({id:"batch-a",status:"waiting_decision",action_name:"transcode_batch",batch:{promotion_plan_ready:true},waiting_options:[{decision:"approve",description:"Aprobar"}]})',
  );
  await controls.children[0].listeners.get("click")();
  assert.deepEqual(calls, ["action_detail"]);
  assert.equal(h.elements.get("batch-review").open, true);
  assert.match(h.elements.get("batch-review-data").textContent, /\/source.mkv/);
  assert.match(
    h.elements.get("batch-review-data").textContent,
    /\/candidate.mkv/,
  );
});

test("timer does not overlap operations polls", async () => {
  const h = harness(),
    pending = deferred();
  let calls = 0;
  h.context.wait = () => {
    calls++;
    return pending.promise;
  };
  h.run("loadJobs = async () => wait();");
  const first = h.interval();
  await h.interval();
  assert.equal(calls, 1);
  pending.resolve();
  await first;
  await h.interval();
  assert.equal(calls, 2);
});

test("late folder response cannot replace a newly selected root", async () => {
  const h = harness(),
    a = deferred(),
    b = deferred();
  h.context.responses = { a: a.promise, b: b.promise };
  h.run(
    'controls = () => {}; api = async (path) => path.includes("%2Fa") ? responses.a : responses.b;',
  );
  h.document.getElementById("service").value = "folder:/a";
  const first = h.run("loadLibrary()");
  h.document.getElementById("service").value = "folder:/b";
  const second = h.run("loadLibrary()");
  b.resolve({ path: "/b", total: 0, items: [], has_more: false });
  await second;
  a.resolve({ path: "/a", total: 0, items: [], has_more: false });
  await first;
  assert.equal(h.elements.get("selection-title").textContent, "/b");
});

test("changing folders clears an old file selection", async () => {
  const h = harness();
  h.run(
    'controls = () => {}; api = async (path) => path.includes("%2Froot%2Fchild") ? {path:"/root/child",total:0,items:[],has_more:false} : {path:"/root",total:1,items:[{path:"/root/child",is_dir:true}],has_more:false};',
  );
  h.document.getElementById("service").value = "folder:/root";
  h.document.getElementById("path").value = "/root/old.mkv";
  await h.run("loadLibrary()");
  const button = h.elements.get("library-items").children[0].children[1];
  await button.listeners.get("click")();
  assert.equal(h.elements.get("path").value, "");
  assert.equal(h.run("state.folder"), "/root/child");
});

test("submission is single-flight while gathering folder files", async () => {
  const h = harness(),
    pending = deferred();
  let builds = 0;
  h.context.wait = () => {
    builds++;
    return pending.promise;
  };
  h.run("controls = () => {}; buildAndSubmitJob = async () => wait();");
  const first = h.run("submitJob()");
  await h.run("submitJob()");
  assert.equal(builds, 1);
  pending.resolve();
  await first;
  assert.equal(h.run("state.submitting"), false);
});
