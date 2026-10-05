import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(
  new URL("./assets/app.js", import.meta.url),
  "utf8",
);
const markup = await readFile(
  new URL("./assets/index.html", import.meta.url),
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

test("season choices use the complete available file summary, never declared empty seasons", async () => {
  const h=harness();
  h.run('controls=()=>{}; $("service").value="sonarr"; state.media={id:1,seasons:[{seasonNumber:0},{seasonNumber:1},{seasonNumber:9}]}; $("season").value="0"; api=async()=>({total:105,items:[],seasons:[{seasonNumber:1,fileCount:100},{seasonNumber:2,fileCount:5}]});');
  await h.run('loadLibrary()');
  assert.equal(h.elements.get("season").value,"");
  assert.deepEqual(h.elements.get("season").children.map(c=>c.textContent),["All seasons","Season 1 · 100 files","Season 2 · 5 files"]);
  h.run('$("season").value="2";');
  await h.run('loadLibrary()');
  assert.equal(h.elements.get("season").value,"2");
});

test("maintenance actions never use native browser confirmations", () => {
  assert.doesNotMatch(source, /\bconfirm\s*\(/);
});

test("candidate review reads only, shows measured sizes and rechecks before approval", async () => {
  const h = harness(), calls = [];
  h.context.fixture = {id:"candidate",status:"waiting_decision",waiting_reason:"Candidate file size (1910766036 bytes) exceeds original (1599600732 bytes) by 19.5%, which is greater than max_size_increase_percent (0.0%)",waiting_options:[{decision:"accept_loss"}],source_path:"/media/Uzumaki.mkv"};
  h.context.record = (...args) => calls.push(args);
  h.run('api=async()=>({jobs:[fixture]}); jobControl=async(...args)=>record(...args);');
  const review = h.run('reviewJobDecision("candidate","accept_loss")');
  for (let i=0;i<6;i++) await Promise.resolve();
  assert.equal(h.elements.get("action-review").open, true);
  assert.equal(calls.length, 0);
  const content = h.elements.get("action-review-content").children;
  assert.match(content[1].children[1].textContent, /1\.60 GB/);
  assert.match(content[2].children[1].textContent, /1\.91 GB/);
  assert.match(content[3].textContent, /19\.5% larger/);
  assert.doesNotMatch(content.map(c=>c.textContent).join(" "), /max_size_increase_percent|1910766036/);
  h.elements.get("confirm-action-review").listeners.get("click")();
  await review;
  assert.deepEqual(JSON.parse(JSON.stringify(calls)), [["candidate","action_resume",{decision:"accept_loss"}]]);
});

test("changed, expired, dismissed or offline candidate reviews never resume a job", async () => {
  for (const scenario of ["changed","expired","dismissed","offline"]) {
    const h=harness(); let writes=0;
    h.context.fixture={id:"candidate",status:"waiting_decision",waiting_reason:"Review quality",waiting_options:[{decision:"accept_loss"}]};
    h.context.write=()=>writes++;
    h.run('controls=()=>{}; api=async()=>({jobs:[fixture]}); jobControl=async()=>write();');
    const review=h.run('reviewJobDecision("candidate","accept_loss")');
    for(let i=0;i<6;i++) await Promise.resolve();
    if(scenario==="changed") h.context.fixture.waiting_reason="Candidate changed";
    if(scenario==="expired") h.run('invalidateAuthentication()');
    if(scenario==="offline") {
      h.run('setConnection(false)');
      h.elements.get("confirm-action-review").listeners.get("click")();
      assert.equal(h.elements.get("action-review").open,true);
    }
    if(["dismissed","offline"].includes(scenario)) h.elements.get("dismiss-action-review").listeners.get("click")();
    else h.elements.get("confirm-action-review").listeners.get("click")();
    if(scenario==="changed") await assert.rejects(review,/changed/);
    else await review;
    assert.equal(writes,0,scenario);
  }
});

test("opening another app review cancels the previous one and Escape cancels the current one", async () => {
  const h=harness();
  const first=h.run('reviewAction({title:"First",message:"First",confirmLabel:"Approve"})');
  const second=h.run('reviewAction({title:"Second",message:"Second",confirmLabel:"Delete"})');
  assert.equal(await first,false);
  let prevented=false;
  h.elements.get("action-review").listeners.get("cancel")({preventDefault(){prevented=true;}});
  assert.equal(await second,false);
  assert.equal(prevented,true);
});

test("stale or disconnected workers do not display a live progress meter or old speed", () => {
  const h=harness();
  for(const condition of [{waiting_condition:"worker_unreachable"},{worker:{progress:2.1,speed:14.7,fps:238,progress_is_stale:true}}]) {
    h.context.condition=condition;
    const result=h.run('telemetry({status:"waiting_external",...condition})');
    assert.equal(result.children.length,1);
    assert.doesNotMatch(result.children[0].textContent,/14\.70|238|Step/);
    assert.match(result.children[0].textContent,/unreachable|No recent progress/);
    assert.match(h.run('queuePresentation({status:"waiting_external",...condition}).status'),/Worker offline|No updates/);
  }
});

test("new submissions require a fresh ready worker and do not reach submission when offline", async () => {
  const h=harness(); let builds=0, checks=0;
  h.context.check=()=>checks++;
  h.context.build=()=>builds++;
  h.run('controls=()=>{}; refreshWorkers=async()=>{check();state.workerInfo={ready:false};}; buildAndSubmitJob=async()=>build(); state.workerInfo={ready:true};');
  await assert.rejects(h.run('submitJob()'),/No job was submitted/);
  assert.equal(builds,0); assert.equal(checks,1);
  h.run('refreshWorkers=async()=>{check();state.workerInfo={ready:true};};');
  await h.run('submitJob()');
  assert.equal(builds,1); assert.equal(checks,2);
});

test("folder sizes distinguish empty, partial and unavailable measurements; late results cannot alter another folder", async () => {
  const h=harness(), pending=deferred();
  h.context.target=new Element(); h.context.pending=pending.promise;
  h.run('showFolderSize(target,{status:"ready",bytes:0})');
  assert.equal(h.context.target.textContent,"0 B");
  h.run('showFolderSize(target,{status:"partial",bytes:5000,note:"Incomplete"})');
  assert.match(h.context.target.textContent,/^≥ /);
  h.run('showFolderSize(target,{status:"unavailable"})');
  assert.equal(h.context.target.textContent,"Unavailable");
  h.run('target.textContent="Calculating…"; state.folder="/old";state.libraryRevision=1;state.folderSizeTargets=new Map([["/old/child",target]]);api=async()=>pending;');
  const load=h.run('updateFolderSizes(1,"/old","",1)');
  h.run('state.libraryRevision=2;state.folder="/new";');
  pending.resolve({items:[{path:"/old/child",folder_size:{status:"ready",bytes:100}}]});
  await load;
  assert.equal(h.context.target.textContent,"Calculating…");
});

test("every startup control exists in the shipped HTML", () => {
  const ids = [...markup.matchAll(/\bid="([^"]+)"/g)].map((match) => match[1]);
  assert.equal(new Set(ids).size, ids.length, "duplicate HTML ids");
  const elements = new Map(ids.map((id) => [id, new Element()]));
  const context = vm.createContext({
    document: {
      getElementById: (id) => elements.get(id) || null,
      createElement: () => new Element(),
      querySelectorAll: () => [],
    },
    URLSearchParams,
    setTimeout() {},
    clearTimeout() {},
    setInterval() {},
  });
  assert.doesNotThrow(() =>
    vm.runInContext(source.replace(/safe\(initialize\);\s*$/, ""), context),
  );
});

test("mobile file selection advances and back preserves the prepared conversion", () => {
  const h = harness();
  h.run(
    'controls = () => {}; $("service").value = "folder:/media"; state.folderSelected = new Set(["/media/one.mp4"]); $("profile").value = "general-hevc"; $("min-savings").value = "8"; configureSelection();',
  );
  assert.equal(h.run("state.fileStep"), "configure");
  assert.equal(h.elements.get("library").dataset.fileStep, "configure");
  assert.equal(h.elements.get("path").value, "/media/one.mp4");
  assert.equal(h.elements.get("scope").value, "file");
  h.elements.get("back-to-files").listeners.get("click")();
  assert.equal(h.run("state.fileStep"), "browse");
  assert.equal(h.elements.get("path").value, "/media/one.mp4");
  assert.equal(h.elements.get("profile").value, "general-hevc");
  assert.equal(h.elements.get("min-savings").value, "8");
});

test("single-column step changes move keyboard focus to a visible control", () => {
  const h = harness();
  h.context.matchMedia = (query) => ({
    matches: query === "(max-width: 900px)",
  });
  let focused;
  h.run('$("configure-selection").hidden = false;');
  for (const id of ["back-to-files", "configure-selection", "service"])
    h.run(`$("${id}")`).focus = () => {
      focused = id;
    };
  h.run('setFileStep("configure");');
  assert.equal(focused, "back-to-files");
  h.run('setFileStep("browse");');
  assert.equal(focused, "configure-selection");
  h.run(
    'setFileStep("configure"); $("configure-selection").hidden = true; setFileStep("browse");',
  );
  assert.equal(focused, "service");
});

test("multiple selected files configure an explicit batch and using a folder clears selection-only scope", () => {
  const h = harness();
  h.run(
    'controls = () => {}; $("service").value = "folder:/media"; state.folder = "/media/season"; state.folderSelected = new Set(["/media/season/one.mp4", "/media/season/two.mp4"]); configureSelection();',
  );
  assert.equal(h.elements.get("scope").value, "batch");
  assert.equal(h.elements.get("selected-only").checked, true);
  assert.equal(h.run("state.folderSelected.size"), 2);
  h.elements.get("back-to-files").listeners.get("click")();
  h.elements.get("use-container").listeners.get("click")();
  assert.equal(h.elements.get("scope").value, "batch");
  assert.equal(h.elements.get("selected-only").checked, false);
  assert.equal(h.elements.get("recursive").checked, true);
  assert.equal(h.run("state.fileStep"), "configure");
});

test("catalog selection retains its import context and an empty selection cannot advance", () => {
  const h = harness();
  h.run('controls = () => {}; $("service").value = "sonarr";');
  assert.throws(() => h.run("configureSelection()"), /Choose a file first/);
  assert.equal(h.run("state.fileStep"), "browse");
  h.run(
    'state.media = {id:12}; state.selected = new Set([34]); state.files.set(34, {id:34,path:"/series/episode.mp4"}); configureSelection();',
  );
  assert.equal(h.elements.get("path").value, "/series/episode.mp4");
  assert.equal(h.run("state.fileMedia.id"), 12);
  assert.equal(h.run("state.fileService"), "sonarr");
});

test("desktop checkbox selection prepares single and batch jobs without changing the step", () => {
  const h = harness();
  h.run(
    'controls = () => {}; $("service").value = "folder:/media"; state.folderSelected = new Set(["/media/one.mp4"]); fileSelectionChanged();',
  );
  assert.equal(h.elements.get("path").value, "/media/one.mp4");
  assert.equal(h.elements.get("scope").value, "file");
  assert.equal(h.run("state.fileStep"), "browse");
  h.run('state.folderSelected.add("/media/two.mp4"); fileSelectionChanged();');
  assert.equal(h.elements.get("scope").value, "batch");
  assert.equal(h.elements.get("selected-only").checked, true);
  h.run("state.folderSelected.clear(); fileSelectionChanged();");
  assert.equal(h.elements.get("path").value, "");
  assert.equal(h.elements.get("scope").value, "file");
  assert.equal(h.elements.get("selected-only").checked, false);
});

test("mobile checkbox selection waits for Continue and batch summaries show the explicit count", () => {
  const h = harness();
  h.context.matchMedia = () => ({ matches: true });
  h.run(
    'controls = () => {}; $("service").value = "folder:/media"; state.folder = "/media/season"; state.folderSelected = new Set(["/media/season/one.mp4", "/media/season/two.mp4"]); fileSelectionChanged();',
  );
  assert.equal(h.run("state.fileStep"), "browse");
  assert.equal(h.elements.get("path").value, "");
  h.run("configureSelection();");
  assert.equal(h.run("sourceSummary()"), "2 selected files · season");
  h.run('$("selected-only").checked = false;');
  assert.equal(h.run("sourceSummary()"), "Batch: season");
  h.run(
    '$("path").value = "/media/season/one.mp4"; state.folderSelected.clear(); fileSelectionChanged();',
  );
  assert.equal(h.elements.get("path").value, "");
  assert.equal(h.elements.get("scope").value, "file");
});

function submissionHarness() {
  const h = harness(),
    receipts = new Map();
  h.context.sessionStorage = {
    getItem: (key) => receipts.get(key),
    setItem: (key, value) => receipts.set(key, value),
    removeItem: (key) => receipts.delete(key),
  };
  h.run(
    'controls = () => {}; loadJobs = async () => {}; selectTab = (tab) => state.tab = tab; notify = () => {}; submissionID = () => "receipt"; tool = async () => ({id:"submitted"}); $("service").value = "folder:/media"; $("scope").value = "file"; $("path").value = "/media/one.mp4"; $("profile").value = "general-hevc"; $("min-savings").value = "10"; $("max-growth").value = "0"; state.fileStep = "configure"; state.folderSelected = new Set(["/media/one.mp4"]); state.selected.add(12);',
  );
  return { h, receipts };
}

test("opening a file makes its single selection explicit and clears an old batch", () => {
  const h = harness();
  h.run(
    '$("service").value = "folder:/media"; state.folderSelected = new Set(["/media/one.mp4", "/media/two.mp4"]);',
  );
  const first = { dataset: { selection: "/media/one.mp4" }, checked: true };
  const second = { dataset: { selection: "/media/two.mp4" }, checked: true };
  h.run('$("library-items")').querySelectorAll = () => [first, second];
  h.run('selectOneFile("/media/two.mp4");');
  assert.equal(h.run("state.folderSelected.size"), 1);
  assert.equal(first.checked, false);
  assert.equal(second.checked, true);
  h.run(
    '$("service").value = "sonarr"; state.selected = new Set([1, 2]); selectOneFile(2);',
  );
  assert.equal(h.run("state.selected.size"), 1);
  assert.equal(h.run("state.selected.has(2)"), true);
});

test("successful encoding clears submitted sources, retains preferences and returns to browse", async () => {
  const { h, receipts } = submissionHarness();
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(h.run("state.fileStep"), "browse");
  assert.equal(h.run("state.tab"), "jobs");
  assert.equal(h.run("state.folderSelected.size + state.selected.size"), 0);
  assert.equal(h.elements.get("path").value, "");
  assert.equal(h.elements.get("profile").value, "general-hevc");
  assert.equal(h.elements.get("min-savings").value, "10");
  assert.equal(receipts.size, 0);
});

test("benchmark retains its source for encoding while failed submissions retain source and receipt", async () => {
  const benchmark = submissionHarness();
  await benchmark.h.run('buildAndSubmitJob("benchmark")');
  assert.equal(benchmark.h.run("state.fileStep"), "browse");
  assert.equal(benchmark.h.elements.get("path").value, "/media/one.mp4");
  const failed = submissionHarness();
  failed.h.run('tool = async () => { throw new Error("Connection lost"); };');
  await assert.rejects(
    failed.h.run('buildAndSubmitJob("encode")'),
    /Connection lost/,
  );
  assert.equal(failed.h.run("state.fileStep"), "configure");
  assert.equal(failed.h.elements.get("path").value, "/media/one.mp4");
  assert.equal(failed.receipts.size, 1);
});

test("benchmark submission obeys its strict input schema even for an anime library selection", async () => {
  for(const library of [false,true]) {
    const {h}=submissionHarness(); let submission;
    h.context.capture=(_name,args)=>{submission=JSON.parse(args.inputs); return {id:"submitted"};};
    if(library) h.run('state.fileMedia={id:12,seriesType:"anime"}; state.fileService="sonarr";');
    h.run('$("media-kind").value="tv"; $("preserve-depth").checked=true; tool=async(...args)=>capture(...args);');
    await h.run('buildAndSubmitJob("benchmark")');
    assert.deepEqual(Object.keys(submission).sort(),["path","preserve_source_bit_depth","profile"]);
  }
});

test("queue results distinguish candidates, replacements and failures without inventing measurements", () => {
  const h = harness();
  const candidate = h.run(
    'queuePresentation({action_name:"transcode_media",status:"completed",candidate_ready:true,source_path:"/media/one.mp4",savings:{source_bytes:1000000000,candidate_bytes:400000000,candidate_saved_bytes:600000000}})',
  );
  assert.equal(candidate.status, "Candidate ready");
  assert.match(candidate.summary, /1.00 GB → 400.0 MB/);
  assert.match(candidate.summary, /potential savings/);
  assert.doesNotMatch(candidate.summary, /freed/);
  const replaced = h.run(
    'queuePresentation({action_name:"promote_transcode_candidate",status:"completed",promotion:{original_path:"/media/one.mp4"},savings:{realized_saved_bytes:600000000}})',
  );
  assert.equal(replaced.title, "one.mp4");
  assert.match(replaced.summary, /Replacement.*600.0 MB freed/);
  assert.match(
    h.run(
      'queuePresentation({status:"failed",error:"stat /private/path/one.mp4: no such file or directory"}).summary',
    ),
    /File not found/,
  );
  assert.match(
    h.run('queuePresentation({status:"failed"}).summary'),
    /Error details unavailable/,
  );
});

test("decision rows show the next step instead of stale worker telemetry or contradictory zero-file counts", () => {
  const h = harness();
  const waiting = h.run(
    'queuePresentation({status:"waiting_decision",worker:{progress:100,speed:2},waiting_reason:"Review quality result"})',
  );
  assert.equal(waiting.showTelemetry, false);
  assert.match(waiting.summary, /Review quality result/);
  assert.equal(
    h.run(
      'queuePresentation({status:"running",worker:{progress:30}}).showTelemetry',
    ),
    true,
  );
  const batch = h.run(
    'queuePresentation({status:"waiting_decision",batch:{total:0,promotion_plan_ready:true}})',
  );
  assert.match(batch.summary, /Replacement approval required/);
  assert.doesNotMatch(batch.summary, /0 files/);
  const replacedSource = h.run('queuePresentation({status:"waiting_decision",replaced:true,waiting_reason:"Review quality result"})');
  assert.equal(replacedSource.status, "Needs decision");
  assert.match(replacedSource.summary, /Review quality result/);
  assert.doesNotMatch(replacedSource.summary, /Original replaced/);
  assert.equal(h.run('shortJobReason("already_hevc")'), "Already HEVC; original kept");
});

test("loading another page cannot duplicate a conversion whose replacement has started", async () => {
  const h = harness();
  const existing = [{id:"convert",workflow_id:"same",status:"completed"}, ...Array.from({length:24},(_,i)=>({id:`other-${i}`,status:"completed"}))];
  h.context.responses = [{jobs:existing,has_more:true,total:25},{jobs:[{id:"replace",workflow_id:"same",status:"running"}],has_more:false,total:25}];
  h.run('api=async()=>responses.shift(); renderSavings=()=>{};');
  await h.run('loadJobs()');
  await h.run('loadJobs(true)');
  assert.equal(h.run('state.operationJobs.size'), 25);
  assert.equal(h.run('state.operationJobs.get("same").id'), "replace");
});

test("replacement progress exposes real stages without inventing a byte percentage", () => {
  const h = harness();
  const result = h.run('telemetry({status:"running",current_step:2,stages:[{name:"plan_promotion"},{name:"approve_promotion"},{name:"preserve_original"},{name:"import_candidate"}]})');
  assert.match(result.children[0].textContent, /Step 3\/4.*Save and verify recovery copy/);
  assert.equal(result.children.length, 1, "workflow stages are not measured byte progress");
  assert.equal(h.run('queuePresentation({status:"running",promotion:{}}).showTelemetry'), true);
  assert.equal(h.run('queuePresentation({status:"waiting_external",worker:{transcode_phase:"encoding",progress:34}}).status'), "Encoding");
  assert.equal(h.run('queuePresentation({status:"waiting_external",promotion:{}}).status'), "Replacing");
});

test("preview eligibility never claims files are queued on the worker", () => {
  const h = harness();
  const preview = h.run('queuePresentation({status:"completed",batch:{dry_run:true,outcome:"preview",total:4,queued:2,skip:2},batch_files:{context:"Series · Season 2"}})');
  assert.equal(preview.title, "Series · Season 2");
  assert.match(preview.summary, /2 eligible.*Originals unchanged/);
  assert.doesNotMatch(preview.summary, /queued/);
  assert.equal(preview.showTelemetry, false);
});

test("replacement review is read-only and a changed plan blocks approval", async () => {
  const h = harness(), mutations = [];
  h.context.current = {id:"replace",status:"waiting_decision",waiting_options:[{decision:"approve"}],promotion:{original_path:"/media/one.mkv",candidate_path:"/media/.candidates/one.mkv",original_sha256:"original",candidate_sha256:"candidate",original_bytes:1000,candidate_bytes:400}};
  h.context.recordMutation = (...args) => mutations.push(args);
  h.run('api = async () => ({jobs:[current]}); jobControl = async (...args) => recordMutation(...args);');
  await h.run('reviewFilePromotion("replace")');
  assert.equal(h.elements.get("file-review").open, true);
  assert.equal(mutations.length, 0);
  h.context.current.promotion.candidate_sha256 = "changed";
  await h.elements.get("approve-file-review").listeners.get("click")();
  assert.equal(mutations.length, 0);
  assert.match(h.elements.get("notice").textContent, /plan changed/);
  h.run('invalidateAuthentication()');
  assert.equal(h.elements.get("file-review").open, false);
  assert.equal(h.run('state.fileApproval'), null);
});

test("search can close, clear its filter and restore the browse view", () => {
  const h = harness(),
    attrs = {};
  const toggle = h.elements.get("toggle-library-search");
  toggle.setAttribute = (key, value) => (attrs[key] = value);
  h.document.getElementById("library-search-label").hidden = true;
  h.run(
    'loadLibrary = async () => { state.restoredSearch = $("search").value; };',
  );
  toggle.listeners.get("click")();
  assert.equal(attrs["aria-expanded"], "true");
  assert.equal(attrs["aria-label"], "Close search");
  h.elements.get("search").value = "no matching file";
  toggle.listeners.get("click")();
  assert.equal(h.elements.get("library-search-label").hidden, true);
  assert.equal(h.elements.get("search-button").hidden, true);
  assert.equal(h.elements.get("search").value, "");
  assert.equal(attrs["aria-expanded"], "false");
  assert.equal(attrs["aria-label"], "Show search");
  assert.equal(h.run("state.restoredSearch"), "");
});

test("a slow folder change removes old actionable rows before it returns", async () => {
  const h = harness(),
    pending = deferred();
  h.context.response = pending.promise;
  h.run(
    'controls = () => {}; api = async () => response; state.folder = "/media/A";',
  );
  h.document.getElementById("service").value = "folder:/media";
  h.document.getElementById("path").value = "/media/A/old.mp4";
  const host = h.document.getElementById("library-items"),
    old = new Element();
  old.textContent = "old selectable file";
  host.append(old);
  const loading = h.run('navigateFolder("/media/B")');
  assert.equal(host.children.includes(old), false);
  assert.equal(h.elements.get("path").value, "");
  assert.equal(h.run("state.libraryLoading"), true);
  pending.resolve({ path: "/media/B", total: 0, items: [], has_more: false });
  await loading;
  assert.equal(h.run("state.libraryLoading"), false);
});

test("file read failure replaces loading with an error and a usable retry", async () => {
  const h = harness();
  h.run(
    'controls = () => {}; api = async () => { throw new Error("Folder unavailable"); };',
  );
  h.document.getElementById("service").value = "folder:/media";
  await assert.rejects(h.run("loadLibrary()"), /Folder unavailable/);
  const labels = h.elements
    .get("library-items")
    .children.map((n) => n.textContent);
  assert.deepEqual(labels, ["Folder unavailable", "Retry"]);
  assert.equal(h.run("state.libraryLoading"), false);
  assert.equal(h.run("state.libraryError"), true);
});

test("notifications appear inside the active modal instead of behind it", () => {
  const h = harness(),
    dialog = new Element(),
    main = new Element();
  h.document.querySelector = (selector) =>
    selector === "dialog[open]" ? dialog : main;
  h.run('notify("Inspection failed");');
  assert.ok(dialog.children.includes(h.elements.get("notice")));
  assert.equal(h.elements.get("notice").className, "dialog-notice");
  assert.equal(h.elements.get("notice").hidden, false);
});

test("failed job reads replace stale controls with an inline retry", async () => {
  const h = harness();
  h.run('api = async () => { throw new Error("Job unavailable"); };');
  await h.run('openJob("gone")');
  assert.deepEqual(
    h.elements.get("detail-summary").children.map((n) => n.textContent),
    ["Job unavailable", "Retry"],
  );
  assert.equal(h.elements.get("detail-controls").children.length, 0);
  assert.equal(h.elements.get("batch-items-panel").hidden, true);
});

test("repeated batch next taps cannot skip a page while its request is pending", async () => {
  const h = harness(),
    pending = deferred();
  let requests = 0;
  h.context.readPage = () => {
    requests++;
    return pending.promise;
  };
  h.run('state.detail="batch"; state.batchItemsHasMore=true; api=readPage;');
  h.document.getElementById("job-detail").open = true;
  const first = h.run("changeBatchPage(25)");
  await h.run("changeBatchPage(25)");
  assert.equal(requests, 1);
  assert.equal(h.run("state.batchItemsOffset"), 25);
  pending.resolve({
    items: [{ display_label: "File 26", status: "completed" }],
    total: 26,
    has_more: false,
  });
  await first;
  assert.equal(h.run("state.batchItemsOffset"), 25);
  assert.equal(h.elements.get("batch-items-next").disabled, true);
  assert.equal(h.run("state.batchItemsPaging"), null);
});

test("failed batch paging restores the previous page instead of advancing it", async () => {
  const h = harness();
  h.run(
    'state.detail="batch"; state.batchItemsHasMore=true; api=async()=>{throw new Error("Unavailable");};',
  );
  h.document.getElementById("job-detail").open = true;
  await assert.rejects(h.run("changeBatchPage(25)"), /Unavailable/);
  assert.equal(h.run("state.batchItemsOffset"), 0);
  assert.equal(h.elements.get("batch-items-next").disabled, false);
});

test("replaced sources suppress pending savings and link their existing replacement", () => {
  const h = harness();
  h.run("state.info.allow_destructive=true;");
  const controls = h.run(
    'jobControls({id:"source",action_name:"transcode_media",status:"completed",candidate_ready:false,replaced:true,replacement_action_id:"replacement"})',
  );
  assert.deepEqual(
    controls.children.map((n) => n.textContent),
    ["View replacement"],
  );
  const line = h.run(
    "savingsLine({source_bytes:1000,candidate_saved_bytes:700,estimated_saved_bytes:600},true,true)",
  );
  assert.doesNotMatch(line.textContent, /Potential|Estimate/);
});

test("network loss blocks mutations and automatically retries a server read", async () => {
  const h = harness();
  let calls = 0;
  h.run("controls = () => {}; state.tab = 'jobs';");
  h.document.getElementById("workspace").hidden = false;
  h.context.fetch = async () => {
    calls++;
    throw new Error("offline");
  };
  await assert.rejects(h.run('api("operations")'), /Cannot connect/);
  assert.equal(h.elements.get("connection-status").hidden, false);
  await assert.rejects(h.run('api("tool", {name:"action_retry"})'), /Offline/);
  await h.interval();
  assert.equal(calls, 2);
  h.context.fetch = async () => ({
    ok: true,
    status: 200,
    json: async () => ({}),
  });
  h.run('reconnect = async () => api("bootstrap");');
  await h.interval();
  assert.equal(h.elements.get("connection-status").hidden, true);
  assert.equal(h.run("serverReachable"), true);
});

test("reconnection refreshes monitoring without discarding a prepared transcode", async () => {
  const h = harness();
  h.run(
    "controls = () => {}; state.paths = ['prepared.mp4']; state.tab = 'library'; api = async () => ({}); loadJobs = async () => {}; initialize = async () => {throw new Error('discarded draft');};",
  );
  h.document.getElementById("workspace").hidden = false;
  await h.run("reconnect()");
  assert.equal(h.run("state.paths[0]"), "prepared.mp4");
  assert.equal(h.run("state.tab"), "library");
});

test("expired session closes stale approval dialogs and exposes sign-in", async () => {
  const h = harness();
  h.run(
    "controls = () => {}; state.detail = 'old'; state.batchApproval = 'old';",
  );
  h.document.getElementById("job-detail").open = true;
  h.document.getElementById("batch-review").open = true;
  h.context.fetch = async () => ({
    ok: false,
    status: 401,
    json: async () => ({ error: "sign in to Navigatorr" }),
  });
  await assert.rejects(h.run('api("bootstrap")'), /sign in/);
  assert.equal(h.elements.get("job-detail").open, false);
  assert.equal(h.elements.get("batch-review").open, false);
  assert.equal(h.elements.get("login").hidden, false);
  assert.equal(h.run("state.batchApproval"), null);
  assert.equal(h.run("state.authRevision"), 1);
});

test("proxy unavailability blocks changes without parsing an HTML error as JSON", async () => {
  const h = harness();
  h.run("controls = () => {};");
  h.context.fetch = async () => ({
    status: 503,
    json: () => assert.fail("HTML proxy body must not be parsed"),
  });
  await assert.rejects(h.run('api("bootstrap")'), /is unavailable/);
  assert.equal(h.run("serverReachable"), false);
  assert.equal(h.elements.get("approve-batch-review").disabled, true);
});

test("session invalidation prevents a delayed batch approval from reopening", async () => {
  const h = harness(),
    plan = deferred();
  h.context.plan = plan.promise;
  h.run("tool = async () => plan; controls = () => {};");
  const pending = h.run('reviewBatchPromotion("batch-a")');
  h.context.fetch = async () => ({
    ok: false,
    status: 401,
    json: async () => ({ error: "sign in to Navigatorr" }),
  });
  await assert.rejects(h.run('api("bootstrap")'), /sign in/);
  plan.resolve({
    data: {
      batch_id: "batch-a",
      digest: "verified",
      members: [{ item_key: "file" }],
    },
  });
  await pending;
  assert.equal(h.elements.get("batch-review").open, false);
});

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
    .find((b) => b.textContent === "Retry")
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
  h.run("refreshWorkers = async () => {}; loadJobs = async () => wait();");
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
  assert.equal(h.elements.get("selection-title").textContent, "b");
  assert.equal(h.elements.get("selection-title").title, "/b");
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
  h.run("controls = () => {}; refreshWorkers = async () => {state.workerInfo={ready:true};}; buildAndSubmitJob = async () => wait();");
  const first = h.run("submitJob()");
  await h.run("submitJob()");
  assert.equal(builds, 1);
  pending.resolve();
  await first;
  assert.equal(h.run("state.submitting"), false);
});

test("profile reads discard late selections and retain the current loading state", async () => {
  const h = harness(),
    a = deferred(),
    b = deferred();
  h.context.reads = { a: a.promise, b: b.promise };
  h.run(
    "tool = async (_name,args) => reads[args.name]; renderAdvancedProfile = () => {}; renderRecipeList = () => {};",
  );
  const first = h.run('readRecipe("a")');
  const second = h.run('readRecipe("b")');
  a.resolve({
    name: "a",
    profile: { container: "mkv", video: { codec: "libx265", quality: 23 } },
    source: "managed",
  });
  await first;
  assert.equal(h.elements.get("recipe-form").inert, true);
  b.resolve({
    name: "b",
    profile: { container: "mkv", video: { codec: "libx265", quality: 25 } },
    source: "managed",
  });
  await second;
  assert.equal(h.elements.get("recipe-name").value, "b");
  assert.equal(h.elements.get("recipe-form").inert, false);
});

test("failed profile reads keep the previous draft inactive until a successful retry", async () => {
  const h = harness();
  h.run(
    'state.recipe = {name:"old"}; $("recipe-name").value = "old"; tool = async () => { throw new Error("read failed"); };',
  );
  await assert.rejects(h.run('readRecipe("new")'), /read failed/);
  assert.equal(h.elements.get("recipe-form").inert, true);
  assert.equal(h.elements.get("recipe-name").value, "old");
  assert.match(
    h.elements.get("recipe-source").textContent,
    /Could not load new/,
  );
  h.run(
    'tool = async () => ({name:"new",profile:{container:"mkv",video:{codec:"libx265",quality:24}}}); renderAdvancedProfile = () => {}; renderRecipeList = () => {};',
  );
  await h.run('readRecipe("new")');
  assert.equal(h.elements.get("recipe-name").value, "new");
  assert.equal(h.elements.get("recipe-form").inert, false);
});

test("creating a new profile supersedes an in-flight read", async () => {
  const h = harness(),
    pending = deferred();
  h.context.read = pending.promise;
  h.run(
    "tool = async () => read; renderAdvancedProfile = () => {}; renderRecipeList = () => {};",
  );
  const first = h.run('readRecipe("old")');
  h.run(
    'setRecipeControls({container:"mkv",video:{codec:"libx265",quality:24}}); $("recipe-name").value = "new";',
  );
  pending.resolve({
    name: "old",
    profile: { container: "mkv", video: { codec: "libx265", quality: 23 } },
    source: "managed",
  });
  await first;
  assert.equal(h.elements.get("recipe-name").value, "new");
  assert.equal(h.elements.get("recipe-quality").value, "24");
});

test("profile read after session invalidation does not restore authenticated data", async () => {
  const h = harness(),
    pending = deferred();
  h.context.read = pending.promise;
  h.run(
    "tool = async () => read; renderAdvancedProfile = () => {}; renderRecipeList = () => {};",
  );
  const first = h.run('readRecipe("private")');
  h.run("state.authRevision++;");
  pending.resolve({
    name: "private",
    profile: { container: "mkv", video: { codec: "libx265", quality: 23 } },
    source: "managed",
  });
  await first;
  assert.equal(h.run("state.recipe"), null);
  assert.equal(h.elements.get("recipe-name")?.value || "", "");
});

test("saving an unchanged bitrate profile preserves rate control and advanced fields", () => {
  const h = harness();
  const profile = {
    container: "mkv",
    video: {
      codec: "hevc_videotoolbox",
      quality: 0,
      average_bitrate_kbps: 3500,
      max_bitrate_kbps: 5000,
      spatial_aq: true,
      realtime: false,
      qmin: 10,
      closed_gop: true,
    },
    audio: { mode: "copy" },
    subtitles: { mode: "preserve", convert_incompatible: true },
    preserve: { metadata: true, chapters: true, attachments: true },
    optimization: {
      enabled: true,
      search: { bitrate_values: [2500, 3500, 4500], max_candidates: 3 },
    },
    custom_future: { flag: true },
  };
  h.context.inputProfile = profile;
  h.run("renderAdvancedProfile = () => {}; setRecipeControls(inputProfile);");
  const saved = JSON.parse(h.run("JSON.stringify(recipeFromControls())"));
  assert.deepEqual(saved, profile);
  assert.equal(h.elements.get("recipe-quality").disabled, true);
});

test("encoder change clears incompatible knobs and re-calibrates sample search", () => {
  const h = harness();
  h.run(
    'renderAdvancedProfile = () => {}; setRecipeControls({container:"mkv",video:{codec:"hevc_videotoolbox",quality:0,average_bitrate_kbps:3500,spatial_aq:true,realtime:false,qmin:5,gop_size:60,power_efficient:true,profile:"main",pixel_format:"yuv420p"},audio:{mode:"copy"},preserve:{metadata:true,chapters:true,attachments:true},optimization:{enabled:true,search:{bitrate_values:[3000,4000],max_candidates:2}}});',
  );
  h.elements.get("recipe-encoder").value = "libx265";
  h.elements.get("recipe-encoder").listeners.get("change")();
  const p = JSON.parse(h.run("JSON.stringify(recipeFromControls())"));
  assert.equal(p.video.codec, "libx265");
  assert.equal(p.video.quality, 23);
  for (const key of [
    "average_bitrate_kbps",
    "spatial_aq",
    "realtime",
    "qmin",
    "gop_size",
    "power_efficient",
  ])
    assert.equal(p.video[key], undefined);
  assert.equal(p.video.profile, "main");
  assert.deepEqual(p.optimization.search, {
    quality_values: [20, 23, 26],
    max_candidates: 3,
  });
  assert.match(h.elements.get("notice").textContent, /reset/);
  h.elements.get("recipe-encoder").value = "hevc_videotoolbox";
  h.elements.get("recipe-encoder").listeners.get("change")();
  const vt = JSON.parse(h.run("JSON.stringify(recipeFromControls())"));
  assert.equal(vt.video.quality, 65);
  assert.equal(vt.video.preset, undefined);
  assert.equal(vt.video.tune, undefined);
  assert.deepEqual(vt.optimization.search, {
    quality_values: [55, 65, 75],
    max_candidates: 3,
  });
});

test("continuous jobs loading appends a page and refreshes the entire loaded window", async () => {
  const h = harness(),
    requests = [];
  h.context.request = (path) => {
    if (path === "worker-activity") return {available:false};
    const q = new URLSearchParams(path.split("?")[1]);
    const offset = Number(q.get("offset")),
      limit = Number(q.get("limit"));
    requests.push({ offset, limit });
    return {
      jobs: Array.from({ length: Math.min(limit, 60 - offset) }, (_, i) => ({
        id: `job-${offset + i}`,
        action_name: "transcode_media",
        status: "completed",
      })),
      total: 60,
      has_more: offset + limit < 60,
    };
  };
  h.run("api = async (path) => request(path); state.tab = 'jobs';");
  await h.run("loadJobs()");
  await h.run("loadJobs(true)");
  assert.equal(h.run("state.operationJobs.size"), 50);
  assert.deepEqual(requests.slice(0, 2), [
    { offset: 0, limit: 25 },
    { offset: 25, limit: 25 },
  ]);
  await h.run("loadJobs()");
  assert.equal(h.run("state.operationJobs.size"), 50);
  assert.deepEqual(requests.at(-1), { offset: 0, limit: 50 });
  await h.run("loadJobs(true)");
  assert.equal(h.run("state.operationJobs.size"), 60);
  assert.equal(h.elements.get("jobs-more").hidden, true);
});

test("worker activity shows occupied slots, sample work, elapsed time and unknown connection honestly", () => {
  const h=harness();
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:2,worker_slots_used:2,jobs:[
    {id:"first",file:"sample-one.mkv",kind:"benchmark",phase:"evaluating_metrics",started_at:new Date(Date.now()-120000).toISOString(),heartbeat_at:new Date().toISOString(),last_progress_at:new Date(Date.now()-10000).toISOString(),progress:84.4,details:{total_units:20,completed_units:17,sample_number:2,candidate_number:3,metric:"vmaf"}},
    {id:"second",file:"sample-two.mkv",kind:"transcode",phase:"encoding",heartbeat_at:new Date().toISOString(),progress:30},
  ]}};
  h.run('renderWorkerActivity(activity)');
  assert.match(h.elements.get("worker-capacity").textContent,/2\/2 occupied/);
  const first=h.elements.get("worker-slots").children[0];
  assert.match(first.children[0].textContent,/sample-one/);
  assert.match(first.children[1].textContent,/Measuring sample quality.*elapsed.*of sample work/);
  assert.match(first.children[2].textContent,/17\/20.*VMAF/);
  h.run('activity.activity.jobs.shift(); activity.activity.worker_slots_used=1;renderWorkerActivity(activity)');
  assert.match(h.elements.get("worker-slots").children[0].children[0].textContent,/Slot 1 · Available/);
  assert.match(h.elements.get("worker-slots").children[1].children[0].textContent,/Slot 2 · sample-two/);
  h.run('renderWorkerActivity({available:false})');
  assert.match(h.elements.get("worker-capacity").textContent,/Unknown/);
  assert.doesNotMatch(h.elements.get("worker-capacity").textContent,/0\/2/);
});

test("a late comparison response cannot reopen a closed dialog or expired session", async () => {
  for (const expired of [false,true]) {
    const h=harness(),pending=deferred();h.context.pending=pending.promise;
    h.run('api=async()=>pending;');
    const open=h.run('openComparison({id:"action",comparison_action_id:"action",action_name:"benchmark_transcode",status:"completed",source_path:"/media/sample.mkv"})');
    if (expired) h.run('invalidateAuthentication()');
    else { h.elements.get("benchmark-comparison").close();h.elements.get("benchmark-comparison").listeners.get("close")(); }
    pending.resolve({frames:[{sample_index:0,width:128,height:72,frame_index:5,source_seconds:5}]});
    await open;
    assert.equal(h.elements.get("comparison-frame").children.length,0);
    assert.equal(h.elements.get("benchmark-comparison").open,false);
  }
});

test("comparison waits for both paired images and never shows one as a successful comparison", async () => {
  const h=harness();
  h.run('api=async()=>({frames:[{sample_index:0,width:128,height:72,frame_index:5,source_seconds:5}]});');
  await h.run('openComparison({id:"action",comparison_action_id:"action",action_name:"benchmark_transcode",status:"completed",source_path:"/media/sample.mkv"})');
  const imgs=h.elements.get("comparison-frame").children;
  assert.match(imgs[0].src,/side=original/);assert.match(imgs[1].src,/side=candidate/);
  imgs[0].listeners.get("load")();
  assert.equal(h.elements.get("comparison-view").hidden,true);
  imgs[1].listeners.get("load")();
  assert.equal(h.elements.get("comparison-view").hidden,false);
  imgs[1].listeners.get("error")();
  assert.equal(h.elements.get("comparison-view").hidden,true);
  assert.match(h.elements.get("comparison-note").textContent,/Could not load both/);
});
test("changing a root blocks old continuation controls until the new first page loads", async () => {
  const h = harness(),
    pending = deferred();
  let calls = 0;
  h.context.response = pending.promise;
  h.context.called = () => calls++;
  h.run(
    "controls = () => {}; state.libraryLoaded = 100; state.libraryHasMore = true; api = async () => { called(); return response; };",
  );
  h.document.getElementById("service").value = "folder:/new-root";
  const first = h.run("loadLibrary()");
  await h.run("loadMoreLibrary()");
  assert.equal(calls, 1);
  assert.equal(h.elements.get("library-more").hidden, true);
  pending.resolve({ path: "/new-root", total: 0, items: [], has_more: false });
  await first;
  assert.equal(h.run("state.libraryLoaded"), 0);
});
test("a loaded folder continuation retains the earlier files", async () => {
  const h = harness();
  h.context.page = 0;
  h.run(
    'controls = () => {}; api = async () => ({path:"/root",total:2,items:[{path:page++ ? "/root/b.mp4" : "/root/a.mp4",size:50}],has_more:page<2});',
  );
  h.document.getElementById("service").value = "folder:/root";
  await h.run("loadLibrary()");
  await h.run("loadMoreLibrary()");
  assert.equal(h.elements.get("library-items").children.length, 2);
  assert.equal(h.run("state.libraryLoaded"), 2);
});

test("saving one profile does not replace a different selection made during the save", async () => {
  const h = harness(),
    pending = deferred();
  h.context.saved = pending.promise;
  h.run(
    'recipeFromControls = () => ({}); state.recipeRevision = 1; tool = async () => saved; loadRecipes = async () => {}; readRecipe = async () => { throw new Error("overwrote new selection"); };',
  );
  h.document.getElementById("recipe-name").value = "profile-a";
  const saving = h.run("saveRecipe()");
  h.run("state.recipeRevision = 2;");
  pending.resolve({});
  await saving;
  assert.equal(h.run("state.recipeSaving"), false);
});

test("expired Cloudflare Access session never exposes the local token form", async () => {
  const h = harness();
  h.run('state.authMode = "cloudflare_access"; controls = () => {};');
  h.context.fetch = async () => ({
    ok: false,
    status: 401,
    json: async () => ({ error: "Cloudflare Access authentication required" }),
  });
  await assert.rejects(h.run('api("bootstrap")'), /Cloudflare Access/);
  assert.equal(h.elements.get("login").hidden, true);
  assert.equal(h.elements.get("access-expired").hidden, false);
  assert.equal(h.elements.get("logout").hidden, true);
});

test("Cloudflare mode opens the workspace without a local sign-in or sign-out", async () => {
  const h = harness();
  h.document.querySelector = () => new Element();
  h.run(
    'api = async (path) => path === "auth-info" ? {auth_mode:"cloudflare_access"} : {roots:[],services:[],tools:[],transcode_enabled:true}; resetLibrary = () => {}; controls = () => {}; loadJobs = async () => {};',
  );
  await h.run("initialize()");
  assert.equal(h.run("state.authMode"), "cloudflare_access");
  assert.equal(h.elements.get("workspace").hidden, false);
  assert.equal(h.elements.get("login").hidden, true);
  assert.equal(h.elements.get("logout").hidden, true);
});

test("folder breadcrumbs navigate directly and clear stale file/search selection", async () => {
  const h = harness();
  h.context.paths = [];
  h.run(
    'controls = () => {}; api = async (request) => { paths.push(request); return {path:"/media",total:0,items:[],has_more:false}; }; state.folder = "/media/Series/Season 1"; state.folderSelected = new Set(["/media/old.mp4"]);',
  );
  h.document.getElementById("service").value = "folder:/media";
  h.document.getElementById("search").value = "episode";
  h.document.getElementById("path").value = "/media/old.mp4";
  h.run('folderBreadcrumbs("/media", state.folder);');
  await h.elements
    .get("folder-breadcrumbs")
    .children[0].listeners.get("click")();
  assert.equal(h.run("state.folder"), "/media");
  assert.equal(h.run("state.folderSelected.size"), 0);
  assert.equal(h.elements.get("search").value, "");
  assert.equal(h.elements.get("path").value, "");
  assert.match(h.context.paths[0], /path=%2Fmedia&q=&offset=0/);
});

test("Access edge login redirects invalidate stale dialogs without following login", async () => {
  for (const response of [
    { type: "opaqueredirect", status: 0 },
    { status: 302, type: "basic" },
    { status: 200, redirected: true, type: "basic" },
  ]) {
    const h = harness();
    let options;
    h.run('state.authMode = "cloudflare_access"; controls = () => {};');
    h.document.getElementById("workspace").hidden = false;
    h.document.getElementById("job-detail").showModal();
    h.document.getElementById("batch-review").showModal();
    h.context.fetch = async (_url, init) => {
      options = init;
      return {
        ...response,
        json: () => assert.fail("login HTML parsed as API JSON"),
      };
    };
    await assert.rejects(h.run('api("bootstrap")'), /Cloudflare Access/);
    assert.equal(options.redirect, "manual");
    assert.equal(h.elements.get("workspace").hidden, true);
    assert.equal(h.elements.get("job-detail").open, false);
    assert.equal(h.elements.get("batch-review").open, false);
    assert.equal(h.elements.get("access-expired").hidden, false);
    assert.equal(h.elements.get("login").hidden, true);
    assert.equal(h.run("serverReachable"), true);
  }
});

test("Access edge HTML denials invalidate authentication before decoding the body", async () => {
  for (const status of [401, 403]) {
    const h = harness();
    h.run('state.authMode = "cloudflare_access"; controls = () => {};');
    h.document.getElementById("workspace").hidden = false;
    h.document.getElementById("path-dialog").showModal();
    h.context.fetch = async () => ({
      ok: false,
      status,
      headers: { get: () => "text/html; charset=utf-8" },
      json: () => {
        assert.equal(h.elements.get("workspace").hidden, true);
        assert.equal(h.elements.get("path-dialog").open, false);
        throw new SyntaxError("HTML is not JSON");
      },
    });
    await assert.rejects(h.run('api("bootstrap")'), /Cloudflare Access/);
    assert.equal(h.elements.get("login").hidden, true);
    assert.equal(h.elements.get("access-expired").hidden, false);
    assert.equal(h.run("state.authRevision"), 1);
  }
});

test("Access edge redirect during auth discovery never exposes a local token form", async () => {
  const h = harness();
  h.run("controls = () => {};");
  h.context.fetch = async () => ({ type: "opaqueredirect", status: 0 });
  await assert.rejects(h.run('api("auth-info")'), /Cloudflare Access/);
  assert.equal(h.run("state.authMode"), "cloudflare_access");
  assert.equal(h.elements.get("login").hidden, true);
  assert.equal(h.elements.get("access-expired").hidden, false);
  assert.equal(h.elements.get("logout").hidden, true);
});

test("Access network outages and JSON permission failures do not report session expiry", async () => {
  const h = harness();
  h.run('state.authMode = "cloudflare_access"; controls = () => {};');
  h.document.getElementById("workspace").hidden = false;
  h.document.getElementById("access-expired").hidden = true;
  h.context.fetch = async () => {
    throw new TypeError("network unavailable");
  };
  await assert.rejects(h.run('api("bootstrap")'), /Cannot connect/);
  assert.equal(h.run("state.authRevision"), 0);
  assert.equal(h.elements.get("access-expired").hidden, true);
  assert.equal(h.elements.get("workspace").hidden, false);
  assert.equal(h.run("serverReachable"), false);
  h.context.fetch = async () => ({
    ok: false,
    status: 403,
    headers: { get: () => "application/json" },
    json: async () => ({ error: "missing same-origin request header" }),
  });
  await assert.rejects(h.run('api("bootstrap")'), /same-origin/);
  assert.equal(h.run("state.authRevision"), 0);
  assert.equal(h.elements.get("access-expired").hidden, true);
  assert.equal(h.elements.get("workspace").hidden, false);
});

test("folder measurements update every alias and resume on returning to Files", async () => {
 const h=harness();h.context.one=new Element();h.context.two=new Element();h.context.controls=()=>{};
 h.run('state.folder="/media";state.libraryRevision=1;state.libraryLoaded=1;state.tab="library";state.folderSizeTargets=new Map([["/media/child",[one,two]]]);$("service").value="folder:/media";api=async()=>({items:[{path:"/media/child",folder_size:{status:"ready",bytes:200}}]});');
 await h.run('updateFolderSizes(1,"/media","",1)');
 assert.equal(h.context.one.textContent,"200 B");assert.equal(h.context.two.textContent,"200 B");
 let calls=0;h.context.record=()=>calls++;
 h.run('updateFolderSizes=async()=>record();selectTab("more");selectTab("library");');
 assert.equal(calls,1);
 h.run('showFolderSize(one,{status:"calculating",measured_at:"0001-01-01T00:00:00Z"})');
 assert.doesNotMatch(h.context.one.title,/Measured/);
});

test("unsupported historical benchmarks open a usable new setup without retrying invalid inputs", async () => {
 const h=harness(); let writes=0;h.context.record=()=>writes++;
 h.run('controls=()=>{}; selectTab=tab=>{state.tab=tab;};setFileStep=step=>{state.fileStep=step;};jobControl=async()=>record();');
 const rail=h.run(`jobControls({id:"old",action_name:"benchmark_transcode",status:"failed",error:'unsupported input "media_type" for benchmark_transcode',source_path:"/media/episode.mkv"})`);
 assert.equal(rail.children.length,1);
 await rail.children[0].listeners.get('click')();
 assert.equal(h.elements.get('path').value,'/media/episode.mkv');
 assert.equal(h.elements.get('scope').value,'file');
 assert.equal(h.run('state.tab'),'library');assert.equal(h.run('state.fileStep'),'configure');assert.equal(writes,0);
});

test("worker loss disables retry and resume but preserves stop and read-only setup controls", () => {
 const h=harness();h.run('controls=()=>{};state.workerInfo={ready:false};');
 const retry=h.run('jobControls({id:"failed",action_name:"transcode_media",status:"failed"}).children[0]');
 assert.equal(retry.disabled,true);
 const rail=h.run('jobControls({id:"paused",action_name:"transcode_batch",status:"waiting_decision",waiting_options:[{decision:"resume"}]})');
 assert.equal(rail.children[0].disabled,true);assert.equal(rail.children[1].disabled,false);
 h.document.querySelectorAll=selector=>selector.includes('data-requires-worker')?[retry]:[];
 h.run('state.workerInfo={ready:true};renderWorkers();');assert.equal(retry.disabled,false);
 h.run('state.workerInfo={ready:false};renderWorkers();');assert.equal(retry.disabled,true);
});

test("select all includes unloaded folder files and applies the current filter without submitting",async()=>{
 const h=harness(), calls=[];
 h.context.calls=calls;
 h.run('controls=()=>{}; $("service").value="folder:/media";$("search").value="episode";state.folder="/media/season";state.libraryRevision=3;state.folderSelected=new Set(); api=async(path)=>{calls.push(path);return {paths:["/media/season/episode1.mkv","/media/season/episode2.mkv"]};};');
 await h.run('selectAllFiles()');
 assert.match(calls[0],/files=1/);assert.match(calls[0],/q=episode/);assert.doesNotMatch(calls[0],/offset=/);
 assert.equal(h.run('state.folderSelected.size'),2);
 assert.equal(h.elements.get("scope").value,"batch");
 assert.equal(h.elements.get("selected-only").checked,true);
 h.elements.get("clear-selected-files").listeners.get("click")();
 assert.equal(h.run('state.folderSelected.size'),0);
 assert.equal(h.elements.get("scope").value,"file");
});

test("late select-all replies cannot select another folder or expired session",async()=>{
 for(const change of ['state.libraryRevision++','state.authRevision++']) {
  const h=harness(),pending=deferred();h.context.pending=pending.promise;
  h.run('controls=()=>{};$("service").value="folder:/media";state.folder="/media/old";state.folderSelected=new Set();api=async()=>pending;');
  const select=h.run('selectAllFiles()');h.run(change);pending.resolve({paths:["/media/old/a.mkv"]});await select;
  assert.equal(h.run('state.folderSelected.size'),0);
 }
});

test("select all in an Arr collection selects files beyond the loaded page",async()=>{
 const h=harness(),calls=[]; h.context.calls=calls;
 h.run('controls=()=>{};$("service").value="sonarr";$("search").value="episode";state.media={id:42};state.files=new Map([[1,{id:1}]]);api=async(path)=>{calls.push(path);return {items:Array.from({length:105},(_,i)=>({id:i+1,path:"/media/episode"+(i+1)+".mkv"}))};};');
 await h.run('selectAllFiles()');
 assert.match(calls[0],/all=1/);assert.match(calls[0],/id=42/);assert.match(calls[0],/q=episode/);
 assert.equal(h.run('state.selected.size'),105);assert.equal(h.run('state.files.get(105).path'),'/media/episode105.mkv');
 assert.equal(h.elements.get('selected-only').checked,true);
});

test("replacement planning keeps the filename and replacement status before hashes are ready",()=>{
 const h=harness();
 const p=h.run('queuePresentation({action_name:"promote_transcode_candidate",status:"running",source_path:"/media/movie.mkv",workflow_actions:[{action_name:"transcode_media"}],savings:{source_bytes:1000,candidate_bytes:400}})');
 assert.equal(p.title,"movie.mkv");assert.equal(p.status,"Replacing");assert.match(p.summary,/Conversion complete/);
});

test("a batch with multiple active files never inherits one child's stale measurement or phase",()=>{
 const h=harness();
 assert.equal(h.run('queuePresentation({status:"waiting_external",batch:{},worker:{phase:"encoding",progress_is_stale:true},activity_waiting_condition:"worker_unreachable",activities:[{waiting_condition:"worker_unreachable"},{}]}).status'),'In progress');
 assert.equal(h.run('queuePresentation({status:"waiting_external",batch:{},activities:[{waiting_condition:"worker_unreachable"},{waiting_condition:"worker_unreachable"}]}).status'),'Worker offline');
});

test("batch progress counts resolved files independently of individual worker percentages", () => {
  const h = harness();
  const progress = h.run('telemetry({batch:{total:23,completed:20,running:2,queued:1},activities:[{},{}],worker:{progress:99,progress_is_stale:true}})');
  assert.equal(progress.children[0].textContent, "20 / 23 files processed");
  assert.equal(progress.children[1].textContent, "87%");
  assert.equal(progress.children[2].value, 20);
  assert.equal(progress.children[2].max, 23);
  const mixed = h.run('batchProgress({batch:{total:10,completed:3,failed:2,skip:1,cancelled:1,waiting_decision:2,running:1}})');
  assert.equal(mixed.children[0].textContent, "6 / 10 files processed");
  assert.equal(mixed.children[1].textContent, "60%");
  assert.match(mixed.children[0].title, /Cancelled files.*not counted/);
  assert.equal(h.run('batchProgress({batch:{dry_run:true,total:10,queued:10}})'), null);
  assert.equal(h.run('batchProgress({batch:{total:0}})'), null);
  assert.equal(h.run('batchProgress({batch:{completed:2}})'), null);
  assert.equal(h.run('batchProgress({status:"completed",batch:{total:2,completed:2}})').children[1].textContent, "100%");
});

test("a completed preview offers review and start, while active work retains cancellation", () => {
  const h = harness();
  h.run('state.workerInfo={ready:true};');
  const controls = h.run('jobControls({id:"p",status:"completed",batch:{dry_run:true,queued:8}})');
  assert.deepEqual(controls.children.map(c=>c.textContent), ["Review preview", "Start batch"]);
  assert.equal(controls.children[1].disabled, false);
  assert.match(h.run('queueOutcome({status:"completed",batch:{dry_run:true,queued:8}})'), /Nothing starts automatically/);
  assert.equal(h.run('queuePresentation({status:"completed",batch:{dry_run:true,outcome:"preview"}}).status'), "Preview complete");
  assert.deepEqual(h.run('jobControls({id:"p",status:"completed",batch:{dry_run:true,queued:0}})').children.map(c=>c.textContent), ["Review preview"]);
  assert.deepEqual(h.run('jobControls({id:"p",status:"completed",batch:{dry_run:true,queued:8},preview_execution_action_id:"encode"})').children.map(c=>c.textContent), ["View batch"]);
  assert.ok(h.run('jobControls({id:"p",status:"running",batch:{dry_run:true}})').children.some(c=>c.textContent === "Cancel"));
  h.run('state.workerInfo={ready:false};');
  const offline = h.run('jobControls({id:"p",status:"completed",batch:{dry_run:true,queued:8}})');
  assert.equal(offline.children[0].disabled, false);
  assert.equal(offline.children[1].disabled, true);
});

test("preview start is read-only until review approval and uses the source preview ID once", async () => {
  const h = harness(), writes = [], opens = [];
  h.context.plan = {id:"p",title:"Example",eligible:2,other:1,files:["One","Three"],settings:[{label:"Profile",value:"general-hevc"}]};
  h.context.record = (...args) => writes.push(args);
  h.context.open = id=>opens.push(id);
  h.run('controls=()=>{};api=async(path,body)=>body ? (record(path,body),{id:"encode"}) : plan; refreshWorkers=async()=>{state.workerInfo={ready:true}}; loadJobs=async()=>{}; openJob=async(id)=>open(id);');
  const pending = h.run('startBatchPreview("p")');
  await Promise.resolve();
  assert.equal(h.elements.get("action-review").open, true);
  assert.equal(h.elements.get("confirm-action-review").textContent, "Start batch");
  assert.equal(writes.length, 0);
  await h.run('startBatchPreview("p")');
  assert.equal(writes.length, 0);
  h.run('finishActionReview(true)');
  await pending;
  assert.equal(writes.length, 1);
  assert.equal(writes[0][0], "batch-preview");
  assert.equal(writes[0][1].id, "p");
  assert.deepEqual(opens, ["encode"]);
  assert.equal(h.run('state.busyJobs.size'), 0);
});

test("dismissed, changed, signed-out and worker-offline previews never submit", async () => {
  for (const mode of ["dismiss", "changed", "signed-out", "offline"]) {
    const h = harness(), writes = [];
    h.context.plan = {id:"p",title:"Example",eligible:2,files:["One","Three"],settings:[]};
    h.context.record = (...args)=>writes.push(args);
    h.run('controls=()=>{};api=async(path,body)=>body ? record(path,body) : plan; refreshWorkers=async()=>{state.workerInfo={ready:false}};');
    const pending = h.run('startBatchPreview("p")');
    await Promise.resolve();
    if (mode === "signed-out") h.run('state.authRevision++;');
    if (mode === "changed") h.context.plan = {...h.context.plan, eligible:3};
    h.run(`finishActionReview(${mode !== "dismiss"})`);
    if (mode === "changed") await assert.rejects(pending, /preview changed/);
    else if (mode === "offline") await assert.rejects(pending, /No job was submitted/);
    else await pending;
    assert.equal(writes.length, 0, mode);
  }
});

test("preview item statuses indicate eligibility, while failures and completed items retain distinct colors", async () => {
  const h = harness();
  h.run('state.detail="p";state.detailRevision=1;state.detailJob={batch:{dry_run:true}};$("job-detail").open=true;api=async()=>({total:2,items:[{status:"queued",display_label:"One",reasons:["explicit profile requested"]},{status:"skip",display_label:"Two",reasons:["Already HEVC; original kept"]}]});');
  await h.run('loadBatchItems("p",1)');
  const rows = h.elements.get("batch-items-list").children;
  assert.equal(rows[0].children[1].children[1].textContent, "Eligible");
  assert.equal(rows[0].children.length, 2);
  assert.equal(rows[1].children[1].children[1].textContent, "Skipped");
  assert.equal(rows[1].children.length, 3);
  assert.equal(h.run('statusClass("completed")'), "completed");
  assert.equal(h.run('statusClass("failed")'), "failed");
  assert.equal(h.run('statusClass("waiting_decision")'), "waiting_decision");
});
