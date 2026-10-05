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
  value = "";
  hidden = false;
  disabled = false;
  open = false;
  textContent = "";
  addEventListener(name, fn) {
    this.listeners.set(name, fn);
  }
  setAttribute() {}
  append(...children) {
    this.children.push(...children);
  }
  replaceChildren(...children) {
    this.children = children;
    this.value = "";
  }
  showModal() {
    this.open = true;
  }
  close() {
    this.open = false;
    this.listeners.get("close")?.();
  }
  contains() {
    return false;
  }
  querySelectorAll() {
    return [];
  }
}
function harness({ integrations = [], latest, legacy = null } = {}) {
  const elements = new Map(),
    calls = [],
    opened = [];
  const context = vm.createContext({
    document: {
      hidden: false,
      getElementById(id) {
        if (!elements.has(id)) elements.set(id, new Element());
        return elements.get(id);
      },
      createElement: () => new Element(),
      querySelectorAll: () => [],
    },
    URLSearchParams,
    setInterval() {},
    setTimeout() {},
    clearTimeout() {},
    console,
    localStorage: { getItem: () => JSON.stringify(legacy) },
    configured: integrations,
    record: (name, args) => calls.push({ name, args }),
    opened: (id) => opened.push(id),
    latest,
  });
  vm.runInContext(source.replace(/safe\(initialize\);\s*$/, ""), context);
  vm.runInContext(
    `state.info={allow_destructive:true,services:configured}; controls=()=>{}; submissionID=()=>"test-receipt"; api=async()=>({jobs:[latest]}); tool=async(name,args)=>{record(name,args);return {id:"planned"};}; openJob=async(id)=>opened(id); loadJobs=async()=>{};`,
    context,
  );
  return {
    context,
    elements,
    calls,
    opened,
    run: (code) => vm.runInContext(code, context),
  };
}

test("an agent's durable Sonarr child uses its parent integration without browser storage", async () => {
  const job = {
    id: "agent",
    candidate_ready: true,
    replacement_context: {
      service: "sonarr",
      series_id: 77,
      source: "parent_batch",
    },
  };
  const h = harness({ integrations: [{ name: "sonarr" }], latest: job });
  h.context.job = job;
  await h.run("prepareReplacement(job)");
  assert.equal(h.calls.length, 1);
  assert.deepEqual(JSON.parse(h.calls[0].args.inputs), {
    transcode_action_id: "agent",
    service: "sonarr",
    series_id: 77,
  });
  assert.deepEqual(h.opened, ["planned"]);
  assert.equal(h.elements.get("replacement-choice").open, false);
});

test("unknown history offers an explicit local or library choice before creating a plan", async () => {
  const job = {
    id: "history",
    candidate_ready: true,
    source_path: "/media/file.mp4",
  };
  const h = harness({
    integrations: [{ name: "sonarr" }, { name: "radarr" }],
    latest: job,
  });
  h.context.job = job;
  await h.run("prepareReplacement(job)");
  assert.equal(h.calls.length, 0);
  assert.equal(h.elements.get("replacement-choice").open, true);
  assert.deepEqual(
    h.elements.get("replacement-method").children.map((n) => n.value),
    ["filesystem", "sonarr", "radarr"],
  );
  h.elements.get("replacement-method").value = "radarr";
  h.elements.get("replacement-method").listeners.get("change")();
  h.elements.get("replacement-library-id").value = "42";
  await h.elements.get("prepare-replacement-choice").listeners.get("click")();
  assert.deepEqual(JSON.parse(h.calls[0].args.inputs), {
    transcode_action_id: "history",
    service: "radarr",
    movie_id: 42,
  });
  assert.deepEqual(h.opened, ["planned"]);
  assert.equal(
    h.calls.some((call) => call.name === "action_resume"),
    false,
  );
});

test("legacy browser hints preselect an import but cannot silently choose its route", async () => {
  const job = { id: "old", candidate_ready: true };
  const h = harness({
    integrations: [{ name: "sonarr" }],
    latest: job,
    legacy: { service: "sonarr", id: 12 },
  });
  h.context.job = job;
  await h.run("prepareReplacement(job)");
  assert.equal(h.calls.length, 0);
  assert.equal(h.elements.get("replacement-method").value, "sonarr");
  assert.equal(h.elements.get("replacement-library-id").value, "12");
  h.elements.get("replacement-method").value = "filesystem";
  h.elements.get("replacement-method").listeners.get("change")();
  await h.elements.get("prepare-replacement-choice").listeners.get("click")();
  assert.deepEqual(JSON.parse(h.calls[0].args.inputs), {
    transcode_action_id: "old",
    service: "filesystem",
  });
});

test("an existing promotion is opened without changing its route or creating another plan", async () => {
  const h = harness();
  await h.run(
    'prepareReplacement({id:"file",replacement_action_id:"prior-review"})',
  );
  assert.equal(h.calls.length, 0);
  assert.deepEqual(h.opened, ["prior-review"]);
});

test("a candidate consumed while the selection dialog was open cannot create another plan", async () => {
  const job = { id: "file", candidate_ready: true };
  const h = harness({
    integrations: [{ name: "sonarr" }],
    latest: { ...job, candidate_ready: false },
  });
  h.context.job = job;
  await h.run("prepareReplacement(job)");
  await assert.rejects(
    h.run('enqueueReplacement(job,{service:"filesystem"})'),
    /no longer ready/,
  );
  assert.equal(h.calls.length, 0);
});
