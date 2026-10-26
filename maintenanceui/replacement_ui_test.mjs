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
  prepend(...children) {
    this.children.unshift(...children);
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
    planned: {
      id: "planned",
      status: "waiting_decision",
      waiting_options: [{ decision: "approve" }],
      promotion: {
        original_path: "/media/one.mkv",
        candidate_path: "/media/.candidates/one.mkv",
        original_sha256: "original",
        candidate_sha256: "candidate",
        original_bytes: 1000,
        candidate_bytes: 400,
        plan_digest: "sha256:plan",
      },
    },
  });
  vm.runInContext(source.replace(/safe\(initialize\);\s*$/, ""), context);
  vm.runInContext(
    `state.info={allow_destructive:true,services:configured}; controls=()=>{}; submissionID=()=>"test-receipt"; api=async(path)=>({jobs:[String(path).includes("id=planned")?planned:latest]}); tool=async(name,args)=>{record(name,args);return {id:"planned"};}; openJob=async(id)=>opened(id); loadJobs=async()=>{};`,
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

// Lets the unawaited replaceWhenPlanned loop run to its next stop.
const settle = async () => { for (let i = 0; i < 20; i++) await new Promise((resolve) => setImmediate(resolve)); };
// Records what the replacement flow does instead of doing it: approvals, notices, opened jobs.
function watch(h, { jobs } = {}) {
  h.context.resumes = [];
  h.context.notes = [];
  h.run("pollDelay=()=>Promise.resolve(); notify=(message)=>notes.push(message); jobControl=async(...args)=>resumes.push(args);");
  if (jobs) {
    h.context.answers = [...jobs];
    h.run('api=async(path)=>({jobs:[String(path).includes("id=planned")?(answers.length>1?answers.shift():answers[0]):latest]})');
  }
  return {
    resumes: () => JSON.parse(JSON.stringify(h.context.resumes)),
    notes: () => [...h.context.notes],
  };
}
const approveDecision = { decision: { label: "Replace file", verb: "Replacing file", failure: "Replacement failed" } };

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
  const seen = watch(h);
  h.context.job = job;
  await h.run("prepareReplacement(job)");
  await settle();
  assert.equal(h.calls.length, 1);
  assert.deepEqual(JSON.parse(h.calls[0].args.inputs), {
    transcode_action_id: "agent",
    service: "sonarr",
    series_id: 77,
  });
  // The plan is ready, so it is approved as it is read: no second dialog, no second click.
  assert.deepEqual(h.opened, []);
  assert.equal(h.elements.get("file-review").open, false);
  assert.equal(h.elements.get("replacement-choice").open, false);
  assert.equal(h.calls.some((call) => call.name === "action_resume"), false);
  assert.deepEqual(seen.resumes(), [["planned", "action_resume", { decision: "approve", plan_digest: "sha256:plan" }, approveDecision]]);
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
  const seen = watch(h);
  await h.elements.get("prepare-replacement-choice").listeners.get("click")();
  await settle();
  assert.deepEqual(JSON.parse(h.calls[0].args.inputs), {
    transcode_action_id: "history",
    service: "radarr",
    movie_id: 42,
  });
  assert.equal(h.elements.get("file-review").open, false);
  assert.equal(h.elements.get("replacement-choice").open, false);
  assert.equal(seen.resumes().length, 1);
  assert.equal(seen.resumes()[0][2].plan_digest, "sha256:plan");
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

test("one tap replaces the file: the plan is approved once it is ready, not before", async () => {
  const job = { id: "file", candidate_ready: true };
  const h = harness({ integrations: [], latest: job });
  const running = { id: "planned", status: "running" };
  const seen = watch(h, { jobs: [running, running, h.context.planned] });
  h.context.job = job;
  h.context.refreshed = 0;
  h.run('$("job-detail").open = true; refreshDetail = async () => { refreshed++; };');
  await h.run('enqueueReplacement(job,{service:"filesystem"})');
  await settle();
  // The open batch is read again at once, so the file shows as replacing.
  assert.equal(h.context.refreshed, 1);
  // The dialog is gone and the plan was read until it was ready.
  assert.equal(h.elements.get("replacement-choice").open, false);
  assert.equal(h.elements.get("file-review").open, false);
  assert.deepEqual(seen.resumes(), [["planned", "action_resume", { decision: "approve", plan_digest: "sha256:plan" }, approveDecision]]);
  assert.equal(seen.notes()[0], "Preparing the replacement. It starts by itself.");
});

test("a replacement whose library may re-download the old file is shown for review, not approved", async () => {
  const job = { id: "file", candidate_ready: true };
  const h = harness({ integrations: [{ name: "sonarr" }], latest: job });
  const warned = { ...h.context.planned, promotion: { ...h.context.planned.promotion, regrab_warning: { service: "sonarr", message: "Sonarr may download this episode again." } } };
  const seen = watch(h, { jobs: [warned] });
  await h.run('replaceWhenPlanned("planned")');
  assert.deepEqual(seen.resumes(), []);
  assert.equal(h.elements.get("file-review").open, true);
});

test("a plan that fails or is withdrawn is reported and approves nothing", async () => {
  const h = harness({ integrations: [] });
  for (const gone of [{ id: "planned", status: "failed", error: "Source file is missing" }, { id: "planned", status: "cancelled" }, { id: "planned", status: "completed" }]) {
    const seen = watch(h, { jobs: [gone] });
    await h.run('replaceWhenPlanned("planned")');
    assert.deepEqual(seen.resumes(), []);
    assert.equal(seen.notes().length, 2);
    assert.match(seen.notes()[1], gone.status === "failed" ? /Source file is missing/ : /no longer be approved\. Nothing was changed/);
  }
});

test("a plan waiting on something other than approval opens its job", async () => {
  const h = harness({ integrations: [] });
  const seen = watch(h, { jobs: [{ id: "planned", status: "waiting_decision", waiting_options: [{ decision: "reject" }] }] });
  await h.run('replaceWhenPlanned("planned")');
  assert.deepEqual(seen.resumes(), []);
  assert.deepEqual(h.opened, ["planned"]);
});

test("a plan that never finishes preparing stops waiting and says where to look", async () => {
  const h = harness({ integrations: [] });
  const seen = watch(h, { jobs: [{ id: "planned", status: "running" }] });
  await h.run('replaceWhenPlanned("planned")');
  assert.deepEqual(seen.resumes(), []);
  assert.equal(seen.notes().at(-1), "The replacement is still being prepared. Open the job to follow it.");
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
