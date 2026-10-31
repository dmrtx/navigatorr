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
const css = await readFile(
  new URL("./assets/app.css", import.meta.url),
  "utf8",
);

test("automatic recovery keeps processing goals and sends no technical profile", async()=>{
  const h=harness();
  h.context.savedPlan={id:"automatic-batch",mode:"quality",scope:"all",selected:2,kept:0,active:0,files:["one","two"],selection_version:"reviewed",settings:{},requires_explicit_profile:true};
  h.run('state.info={};state.recipes=[];api=async()=>savedPlan;');
  await h.run('reconfigureBatch("automatic-batch")');
  assert.equal(h.elements.get("reconfigure-mode-label").hidden,false);
  assert.equal(h.elements.get("reconfigure-profile").hidden,true);
  assert.equal(h.elements.get("reconfigure-limits").hidden,true);
  assert.equal(h.elements.get("submit-reconfigure").disabled,false);
  const calls=[];h.context.record=(path,body)=>calls.push([path,body]);
  h.context.sessionStorage={getItem:()=>null,setItem(){},removeItem(){}};
  h.run('$("reconfigure-mode").value="size";submissionID=()=>"goal-receipt";commandRequest=async(path,body)=>{record(path,body);return {id:"automatic-batch"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};');
  await h.run('submitReconfiguredBatch()');
  assert.equal(calls[0][1].mode,"size");
  assert.equal(calls[0][1].selection_version,"reviewed");
  assert.equal("profile" in calls[0][1],false);
});
class Element {
  classList = { toggle() {}, add() {}, remove() {} };
  children = [];
  listeners = new Map();
  dataset = {};
  style = {};
  hidden = false;
  disabled = false;
  value = "";
  textContent = "";
  open = false;
  firstChild = {textContent:""};
  closest() { return this; }
  addEventListener(name, fn) {
    this.listeners.set(name, fn);
  }
  setAttribute() {}
  removeAttribute() {}
  append(...children) {
    this.children.push(...children);
  }
  prepend(...children) {
    this.children.unshift(...children);
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
// The radio groups behind the segmented controls, by name.
const segmentGroups = {
  "size-engine": ["fast", "small"],
  "size-budget-limit": ["", "siblings", "fixed"],
  "reconfigure-engine": ["fast", "small"],
  "reconfigure-budget-limit": ["", "siblings", "fixed"],
  "compact-height": ["1080", "2160"],
  "reconfigure-height": ["1080", "2160"],
};
function harness({ radios: withRadios = false, segments: withSegments = false } = {}) {
  const elements = new Map();
  let interval;
  // Registered before the script loads, because it attaches their listeners then.
  const segments = new Map(withSegments ? Object.entries(segmentGroups).map(([name, values]) => [name, values.map((value) => Object.assign(new Element(), { value, checked: false }))]) : []);
  // Real goal radios, so the change listener that marks a choice as explicit runs.
  const radios = withRadios
    ? ["size", "compact", "quality", "x265_preserve"].map((value) => Object.assign(new Element(), { value, checked: value === "size" }))
    : [];
  const document = {
    hidden: false,
    activeElement: null,
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, new Element());
      return elements.get(id);
    },
    createElement: () => new Element(),
    querySelectorAll: (selector) => (selector.includes("processing-mode") ? radios : segments.get(/name="([^"]+)"/.exec(selector)?.[1]) || []),
    querySelector: (selector) => (selector.includes("processing-mode") ? radios.find((r) => r.checked) || null : null),
  };
  const context = vm.createContext({
    document,
    console,
    URLSearchParams,
    AbortController,
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
    radios,
    segments,
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

test("podcast controls expose transcript coverage and exact cut review",()=>{
	const h=harness();h.context.job={id:"podcast",action_name:"clean_podcast_ads",status:"waiting_decision",waiting_condition:"podcast_classification",podcast:{total_blocks:2,classified_blocks:1}};
	let labels=h.run('jobControls(job).children.map(b=>b.textContent)');assert.ok(labels.includes("View transcript"));assert.ok(!labels.includes("Validate classifications"));
	h.run('job.podcast.classified_blocks=2');labels=h.run('jobControls(job).children.map(b=>b.textContent)');assert.ok(labels.includes("Validate classifications"));
	h.run('job.waiting_condition="podcast_review"');labels=h.run('jobControls(job).children.map(b=>b.textContent)');assert.ok(labels.includes("Review cuts"));assert.ok(labels.includes("Revise labels"));assert.ok(!labels.includes("View transcript"));
});

test("podcast cut review reads all pages and approves the returned digest before render",async()=>{
	const h=harness();h.context.calls=[];h.run('tool=async(name,args)=>{calls.push({name,args});return {digest:"frozen-cuts",total_cuts:2,removed_ms:1000,boundaries:[],next_offset:args.offset===0?1:2,has_more:args.offset===0}};reviewAction=async()=>true;jobControl=async(id,name,args)=>calls.push({id,name,args});');
	await h.run('reviewPodcastCuts("podcast")');const calls=h.context.calls;assert.equal(calls[0].args.offset,0);assert.equal(calls[1].args.offset,1);assert.equal(calls[2].args.digest,"frozen-cuts");assert.equal(calls[2].args.approve,true);assert.equal(calls[3].args.decision,"render");
});

test("podcast revision reopens only the current unapproved review and never renders",async()=>{
	const h=harness();h.context.calls=[];h.run('tool=async(name,args)=>{calls.push({name,args});return {digest:"pending-cuts",approved:false}};loadJobs=async()=>{};openJob=async()=>{};');
	await h.run('revisePodcastLabels("podcast")');assert.equal(h.context.calls.length,2);assert.equal(h.context.calls[1].args.digest,"pending-cuts");assert.equal(h.context.calls[1].args.reclassify,true);assert.ok(!h.context.calls.some(c=>c.args.approve || c.args.decision));
	h.run('calls=[];tool=async(name,args)=>{calls.push({name,args});if(!args.reclassify)throw new Error("cuts are not planned yet");return {reopened:true}};');
	await h.run('revisePodcastLabels("podcast","durable-pre-crash-cuts")');assert.equal(h.context.calls.length,1);assert.equal(h.context.calls[0].args.digest,"durable-pre-crash-cuts");assert.equal(h.context.calls[0].args.reclassify,true);
	h.run('calls=[];tool=async(name,args)=>{calls.push({name,args});return {digest:"approved-cuts",approved:true}};');
	await assert.rejects(h.run('revisePodcastLabels("podcast")'),/Only unapproved/);assert.equal(h.context.calls.length,1);
});

test("URL state preserves tabs, sources, folders, sorting, filters and open details", () => {
  const h=harness();
  h.run('state.tab="jobs";$("service").value="folder:/media";state.folder="/media/Series/Season 1";$("library-sort").value="size_desc";$("job-filter").value="archived";state.detail="job-one";$("job-detail").open=true;');
  const query=h.run('navigationQuery()');
  const route=h.run(`navigationRoute(${JSON.stringify(query)})`);
  assert.equal(route.tab,"jobs"); assert.equal(route.folder,"/media/Series/Season 1");
  assert.equal(route.source,"folder:/media"); assert.equal(route.sort,"size_desc");
  assert.equal(route.status,"archived"); assert.equal(route.job,"job-one");
  const invalid=h.run('navigationRoute("?view=bad&media=-1&sort=bad&status=bad")');
  assert.equal(invalid.tab,"library"); assert.equal(invalid.media,null); assert.equal(invalid.sort,"name"); assert.equal(invalid.status,"all");
});

test("URL updates add one browser history entry per navigation and canonical restoration replaces it", () => {
  const h=harness(), writes=[];
  h.context.window={location:{pathname:"/",search:""},history:{pushState(_s,_t,url){writes.push(["push",url]);h.context.window.location.search=url.slice(1);},replaceState(_s,_t,url){writes.push(["replace",url]);h.context.window.location.search=url.slice(1);}}};
  h.run('state.routeReady=true;state.tab="jobs";writeNavigation();writeNavigation();');
  assert.deepEqual(writes,[["push","/?view=queue"]]);
  h.run('state.applyingNavigation=true;state.tab="advanced";writeNavigation();state.applyingNavigation=false;writeNavigation(true);');
  assert.deepEqual(writes[1],["replace","/?view=settings"]);
});

test("deep links restore an Arr title and selected file; late routes cannot replace newer navigation", async () => {
  const h=harness(), pending=deferred();
  h.run('controls=()=>{};loadLibrary=async()=>{};loadJobs=async()=>{};state.info={roots:["/media"],services:[{name:"sonarr"}]};api=async()=>({media:{id:7,title:"Synthetic title"},items:[{id:1,path:"/media/title/episode.mkv"}]});');
  await h.run('applyNavigation(navigationRoute("?view=files&source=sonarr&media=7&file=%2Fmedia%2Ftitle%2Fepisode.mkv&sort=size_asc"))');
  assert.equal(h.run('state.media.id'),7);assert.equal(h.run('state.fileStep'),"configure");assert.equal(h.elements.get("path").value,"/media/title/episode.mkv");
  h.context.pending=pending.promise;
  h.run('api=async()=>pending;');
  const older=h.run('applyNavigation(navigationRoute("?view=files&source=sonarr&media=8"))');
  await h.run('applyNavigation(navigationRoute("?view=queue&source=folder%3A%2Fmedia&folder=%2Fmedia%2Fnew"))');
  pending.resolve({media:{id:8,title:"Old title"}});await older;
  assert.equal(h.run('state.tab'),"jobs");assert.equal(h.run('state.folder'),"/media/new");assert.equal(h.run('state.media'),null);
});

test("Back hides recovery inventory and late inventory cannot restore the copies route", async () => {
  const h=harness(), pending=deferred();
  h.context.pending=pending.promise;
  h.run('controls=()=>{};state.info={roots:[],services:[]};tool=async()=>pending;');
  const older=h.run('applyNavigation(navigationRoute("?view=more&copies=1"))');
  assert.equal(h.run('state.backupsVisible'),true);
  await h.run('applyNavigation(navigationRoute("?view=more"))');
  pending.resolve({items:[{action_id:"old",path:"/media/original.bak",bytes:1000}]});
  await older;
  assert.equal(h.run('state.backupsVisible'),false);
  assert.equal(h.run('state.backups.length'),0);
  assert.equal(h.elements.get("backup-list").hidden,true);
  assert.equal(h.elements.get("backup-summary").hidden,true);
  assert.doesNotMatch(h.run('navigationQuery()'),/copies=/);
});

test("Back clears the selected profile and supersedes its pending read", async () => {
  const h=harness(), pending=deferred();
  h.context.pending=pending.promise;
  h.run('controls=()=>{};state.info={roots:[],services:[]};state.recipe={name:"previous"};renderRecipeList=()=>{};tool=async()=>pending;');
  const older=h.run('applyNavigation(navigationRoute("?view=profiles&profile=old"))');
  await h.run('applyNavigation(navigationRoute("?view=profiles"))');
  pending.resolve({name:"old",profile:{container:"mkv",video:{codec:"libx265",quality:23}}});
  await older;
  assert.equal(h.run('state.recipe'),null);
  assert.equal(h.elements.get("recipe-name").value,"");
  assert.equal(h.elements.get("recipe-form").inert,true);
  assert.doesNotMatch(h.run('navigationQuery()'),/profile=/);
});

test("automatic profile selection replaces the tab route and cannot undo a newer Back navigation", async () => {
  for (const back of [false,true]) {
    const h=harness(), pending=deferred(), writes=[];
    h.context.window={location:{pathname:"/",search:"?view=queue"},scrollTo(){},history:{pushState(_s,_t,url){writes.push(["push",url]);h.context.window.location.search=url.slice(1);},replaceState(_s,_t,url){writes.push(["replace",url]);h.context.window.location.search=url.slice(1);}}};
    h.context.pending=pending.promise;
    h.run('controls=()=>{};state.info={roots:[],services:[]};state.routeReady=true;state.recipes=["default"];loadRecipes=async()=>pending;renderRecipeList=()=>{};renderAdvancedProfile=()=>{};tool=async()=>({name:"default",profile:{container:"mkv",video:{codec:"libx265",quality:23}}});selectTab("recipes");');
    if(back) await h.run('applyNavigation(navigationRoute("?view=profiles"))');
    pending.resolve();
    for(let i=0;i<12;i++) await Promise.resolve();
    assert.deepEqual(writes[0],["push","/?view=profiles"]);
    if(back) {assert.equal(h.run('state.recipe'),null);assert.equal(writes.length,1);}
    else assert.deepEqual(writes[1],["replace","/?view=profiles&profile=default"]);
  }
});

test("signed-out views never canonicalize or append navigation history", () => {
  const h=harness();let writes=0;
  h.context.window={location:{pathname:"/",search:"?view=profiles&profile=private"},history:{pushState(){writes++;},replaceState(){writes++;}}};
  h.run('state.routeReady=true;$("workspace").hidden=true;writeNavigation();writeNavigation(true);');
  assert.equal(writes,0);
});

test("cleanup finishing after Back does not reopen or reload the hidden recovery inventory", async () => {
  const h=harness();let reloads=0;
  h.context.record=()=>reloads++;
  h.run('controls=()=>{};state.info={roots:[],services:[]};state.backupsVisible=true;state.backupCleaning={id:"cleanup",name:"episode.mkv",auth:state.authRevision,started:Date.now()};loadBackups=async()=>record();loadJobs=async()=>{};');
  await h.run('applyNavigation(navigationRoute("?view=more"))');
  await h.run('finishBackupCleanup({status:"completed"})');
  assert.equal(reloads,0);
  assert.equal(h.elements.get("backup-progress").hidden,true);
  assert.equal(h.run('state.backupCleaning'),null);
});

test("column headers toggle both directions and persist the order through the library read", async () => {
  const h=harness(), orders=[];
  h.context.record=order=>orders.push(order);
  h.run('loadLibrary=async()=>record($("library-sort").value);$("library-sort").value="name";');
  await h.run('toggleLibrarySort("name")');await h.run('toggleLibrarySort("name")');await h.run('toggleLibrarySort("size")');await h.run('toggleLibrarySort("size")');
  assert.deepEqual(orders,["name_desc","name","size_desc","size_asc"]);
});

test("header checkbox selects recursive folder videos, shows partial selection and clears without submission", async () => {
  const h=harness(), requests=[];
  h.context.record=url=>{requests.push(url);return {paths:["/media/a.mkv","/media/nested/b.mp4"]};};
  h.run('$("service").value="folder:/media";state.folder="/media";state.folderSelected=new Set();api=async url=>record(url);');
  await h.run('selectAllFiles()');
  assert.match(requests[0],/recursive=1/);
  assert.equal(h.elements.get("select-all-files").checked,true);assert.equal(h.elements.get("select-all-files").indeterminate,false);
  h.run('state.folderSelected.delete("/media/a.mkv");controls();');
  assert.equal(h.elements.get("select-all-files").checked,false);assert.equal(h.elements.get("select-all-files").indeterminate,true);
  h.elements.get("select-all-files").checked=false;
  h.elements.get("select-all-files").listeners.get("change")();
  assert.equal(h.run('state.folderSelected.size'),0);assert.equal(h.elements.get("select-all-files").indeterminate,false);
  assert.equal(requests.length,1);
});

test("folder checkbox resolves only its videos, supports unchecking and ignores late replies", async () => {
  const h=harness(), pending=deferred();
  h.run('controls=()=>{};$("service").value="folder:/media";state.folderSelected=new Set(["/media/other.mkv"]);api=async()=>({paths:["/media/series/a.mkv","/media/series/b.mkv"]});');
  await h.run('selectFolderFiles("/media/series",true)');assert.equal(h.run('state.folderSelected.size'),3);
  await h.run('selectFolderFiles("/media/series",false)');assert.deepEqual([...h.run('state.folderSelected')],["/media/other.mkv"]);
  h.context.pending=pending.promise;h.run('api=async()=>pending;');
  const read=h.run('selectFolderFiles("/media/series",true)');h.run('state.libraryRevision++;state.folderSelected.clear();');
  pending.resolve({paths:["/media/series/a.mkv"]});await read;assert.equal(h.run('state.folderSelected.size'),0);
});

test("terminal jobs expose archive and archived jobs restore; active jobs never offer archive", async () => {
  const h=harness(), pending=deferred(), calls=[];
  h.context.record=(...args)=>{calls.push(args);return pending.promise;};
  h.run('controls=()=>{};loadJobs=async()=>{};api=async(...args)=>record(...args);');
  assert.ok(h.run('jobControls({id:"done",status:"completed",can_archive:true}).children.some(b=>b.textContent==="Archive")'));
  assert.deepEqual(h.run('jobControls({id:"old",status:"completed",archived:true}).children.map(b=>b.textContent)').join(),"Restore");
  assert.equal(h.run('jobControls({id:"active",status:"running",can_archive:false}).children.some(b=>b.textContent==="Archive")'),false);
  const first=h.run('archiveJob({id:"done"},true)');await h.run('archiveJob({id:"done"},true)');assert.equal(calls.length,1);
  pending.resolve({archived:true});await first;assert.equal(h.run('state.busyJobs.size'),0);
});

test("archiving a job offers to delete the temporary files its encode left, and keeps them on request", async () => {
  for (const [name, answer, wantPost, wantArchive] of [
    ["delete", true, true, true],
    ["keep", "keep", false, true],
    ["back", false, false, false],
  ]) {
    const h=harness(), calls=[], reviews=[];
    h.context.record=(path,body)=>{calls.push(body ? `POST ${path}` : `GET ${path}`);return path.startsWith("job-leftovers") ? (body ? {removed:2,kept:0,freed_bytes:3e9} : {id:"done",files:2,bytes:3e9,can_delete:true}) : {archived:true};};
    h.context.review=o=>{reviews.push(o);return Promise.resolve(answer);};
    h.run('state.info={allow_destructive:true};controls=()=>{};loadJobs=async()=>{};api=async(...args)=>record(...args);reviewAction=async o=>review(o);');
    await h.run('archiveJob({id:"done"},true)');
    assert.equal(reviews.length,1,name);
    assert.match(JSON.stringify(reviews[0].content),/2 temporary files \(3\.00 GB\)/,name);
    assert.equal(calls.includes("POST job-leftovers"),wantPost,name);
    assert.equal(calls.includes("POST archive"),wantArchive,name);
  }
});

test("archiving skips the question when nothing was left, when deleting is off, and when the lookup fails", async () => {
  for (const [info, reply] of [
    [{allow_destructive:true},{id:"done",files:0,bytes:0,can_delete:true}],
    [{allow_destructive:false},{id:"done",files:2,bytes:1,can_delete:true}],
    [{allow_destructive:true},new Error("offline")],
  ]) {
    const h=harness(), calls=[];
    h.context.record=(path,body)=>{calls.push(path);if(path.startsWith("job-leftovers")){if(reply instanceof Error)throw reply;return reply;}return {archived:true};};
    h.context.info=info;
    h.run('state.info=info;controls=()=>{};loadJobs=async()=>{};api=async(...args)=>record(...args);reviewAction=async()=>{throw new Error("must not ask");};');
    await h.run('archiveJob({id:"done"},true)');
    assert.ok(calls.includes("archive"));
  }
});

test("cancelling says how many temporary files the encode left", async () => {
  const h=harness(), notes=[];
  h.context.note=t=>notes.push(t);
  h.context.record=()=>({id:"j",files:3,bytes:2e9,can_delete:true});
  h.run('notify=note;api=async(...args)=>record(...args);');
  await h.run('noteJobLeftovers({id:"j"})');
  assert.equal(notes.length,1);
  assert.match(notes[0],/3 temporary files \(2\.00 GB\)/);
  assert.match(notes[0],/Archive the job to delete them/);
  h.context.record=()=>({id:"j",files:0,bytes:0,can_delete:true});
  await h.run('noteJobLeftovers({id:"j"})');
  assert.equal(notes.length,1);
});

test("recovery inventory loads through empty pages, deduplicates and exposes original locations and blocked reasons", async () => {
  const h=harness(), offsets=[];
  h.context.read=(_name,args)=>{offsets.push(args.offset);return args.offset===0 ? {items:[],next_offset:25} : {items:[{action_id:"copy",status:"failed",original_path:"/media/Series/episode.mkv",path:"/media/.recovery/original.bak",bytes:1000,partial_bytes:500,reason:"promotion stopped before final cleanup"}],next_offset:args.offset===25 ? 50:null};};
  h.run('tool=async(...args)=>read(...args);');await h.run('loadBackups()');
  assert.deepEqual(offsets,[0,25,50]);assert.equal(h.run('state.backups.length'),1);
  const row=h.elements.get("backup-list").children[0];
  assert.equal(row.children[0].children[0].textContent,"episode.mkv");assert.match(row.children[1].textContent,/Original: \/media\/Series\/episode.mkv/);
  assert.match(row.children[3].textContent,/earlier error/);assert.equal(row.children[4].children.length,1);
  assert.match(h.elements.get("backup-summary").textContent,/1 recovery copy · 1.5 KB retained/);
  assert.equal(h.elements.get("backups-more").hidden,true);
});

test("cleanup shows live work and is single-flight; dismissal and expired sessions cannot remove copies", async () => {
  for (const scenario of ["pending","dismissed","expired"]) {
    const h=harness(), pending=deferred();let writes=0;
    h.context.copy={action_id:"cleanup",original_path:"/media/episode.mkv",path:"/media/recovery/original.bak",bytes:1000,cleanup_available:true};
    h.context.remove=()=>{writes++;return pending.promise;};
    h.run(`reviewAction=async()=>${scenario!=="dismissed"};loadBackups=async()=>{};loadJobs=async()=>{};tool=async()=>remove();`);
    const clean=h.run('cleanBackup(copy)');for(let i=0;i<5;i++) await Promise.resolve();
    if(scenario==="dismissed") {await clean;assert.equal(writes,0);continue;}
    assert.match(h.elements.get("backup-progress").children.map(c=>c.textContent).join(" "),/Waiting for server.*episode.mkv.*elapsed/);
    await h.run('cleanBackup(copy)');assert.equal(writes,1);
    if(scenario==="expired") h.run('invalidateAuthentication()');
    pending.resolve({status:"completed"});await clean;
    assert.equal(h.run('state.backupCleaning'),null);
    if(scenario==="pending") assert.match(h.elements.get("backup-progress").textContent,/Removed recovery data/);
    else assert.equal(h.elements.get("backup-progress").hidden,true);
  }
});

test("lost cleanup responses keep removal disabled until the durable job outcome is known", async () => {
  const h=harness();let writes=0;
  h.context.write=()=>{writes++;throw new Error("Lost response");};
  h.run('reviewAction=async()=>true;loadBackups=async()=>{};loadJobs=async()=>{};tool=async()=>write();api=async()=>({jobs:[{id:"cleanup",status:"running"}]});');
  await assert.rejects(h.run('cleanBackup({action_id:"cleanup",original_path:"/media/a.mkv",cleanup_available:true})'),/Lost response/);
  assert.equal(h.run('state.backupCleaning.id'),"cleanup");assert.match(h.elements.get("backup-progress").children[0].textContent,/Checking cleanup outcome/);
  await h.run('cleanBackup({action_id:"cleanup"})');assert.equal(writes,1);
  h.run('api=async()=>({jobs:[{id:"cleanup",status:"completed"}]});');await h.run('refreshBackupCleanup()');assert.equal(h.run('state.backupCleaning'),null);
});

test("linked execution keeps preview access in job details and preserves the compact queue controls", () => {
  const h=harness();
  h.run('state.workerInfo={ready:true};');
  h.context.job={id:"execution",action_name:"transcode_batch",status:"waiting_external",preview_action_id:"preview"};
  assert.equal(h.run('jobControls(job).children.some(b=>b.textContent==="View preview")'),false);
  assert.equal(h.run('jobControls(job,true).children.some(b=>b.textContent==="View preview")'),true);
});

test("missing Arr deep links recover to the library and keep other tabs usable", async () => {
  const h=harness();let jobs=0;
  h.context.readJobs=()=>jobs++;
  h.run('controls=()=>{};loadLibrary=async()=>{};loadJobs=async()=>readJobs();state.info={roots:["/media"],services:[{name:"sonarr"}]};api=async()=>{throw new Error("Title unavailable");};');
  await h.run('applyNavigation(navigationRoute("?view=queue&source=sonarr&media=999"))');
  assert.equal(h.run('state.tab'),"jobs");assert.equal(h.run('state.media'),null);assert.equal(jobs,1);assert.equal(h.run('state.applyingNavigation'),false);
});

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

test("Sonarr series open a complete season list and seasons open only their episodes", async () => {
  const h=harness(), calls=[];
  h.context.record=path=>calls.push(path);
  h.run('controls=()=>{};$("service").value="sonarr";state.media={id:7,title:"Road Notes"};api=async(path)=>{record(path);const season=new URLSearchParams(path.split("?")[1]).get("season");return season===null ? {total:194,items:[{id:1,path:"/media/Season 1/one.mkv"}],has_more:true,seasons:[],season_groups:[{seasonNumber:1,fileCount:10,size:200},{seasonNumber:2,fileCount:12,size:300},{seasonNumber:10,fileCount:8,size:100},{seasonNumber:0,fileCount:2,size:50}]} : {total:1,has_more:false,seasons:[],items:[{id:2,path:"/media/Season 2/two.mkv",relativePath:"Season 2/two.mkv",seasonNumber:Number(season)}]};};');
  await h.run('loadLibrary()');
  assert.deepEqual(h.elements.get('library-items').children.map(row=>row.children[1].children[0].textContent),['Season 1','Season 2','Season 10','Specials']);
  assert.equal(h.elements.get('library-more').hidden,true);
  assert.equal(h.run('state.libraryHasMore'),false);
  assert.equal(h.run('state.files.size'),0,'season rows must not become selectable episode IDs');
  assert.equal(h.elements.get('library-total').textContent,'4 seasons · 194 files');
  await h.run('navigateSeriesSeason(2)');
  assert.equal(new URLSearchParams(calls.at(-1).split('?')[1]).get('season'),'2');
  assert.equal(h.elements.get('selection-title').textContent,'Road Notes · Season 2');
  assert.equal(h.elements.get('library-items').children[0].children[1].children[0].textContent,'two.mkv');
  assert.match(h.run('navigationQuery()'),/season=2/);
  h.elements.get('back').listeners.get('click')();
  for(let i=0;i<12;i++) await Promise.resolve();
  assert.equal(h.run('state.media.id'),7);
  assert.equal(h.run('state.librarySeason'),null);
  assert.equal(h.elements.get('library-items').children.length,4);
});

test("season selection is scoped across pages, preserves other seasons and drops late results",async()=>{
  const h=harness(), calls=[];
  h.context.record=path=>calls.push(path);
  h.run('controls=()=>{};$("service").value="sonarr";state.media={id:7};state.librarySeason=2;state.selected=new Set([101]);state.files.set(101,{id:101,path:"/media/one.mkv",seasonNumber:1});api=async(path)=>{record(path);return {items:[{id:201,path:"/media/two.mkv",seasonNumber:2},{id:202,path:"/media/three.mkv",seasonNumber:2}]}};');
  await h.run('selectAllFiles()');
  const query=new URLSearchParams(calls[0].split('?')[1]);
  assert.equal(query.get('all'),'1'); assert.equal(query.get('season'),'2');
  assert.deepEqual(Array.from(h.run('state.selected')),[101,201,202]);
  h.elements.get('select-all-files').checked=false;
  h.elements.get('select-all-files').listeners.get('change')();
  assert.deepEqual(Array.from(h.run('state.selected')),[101]);
  const pending=deferred();h.context.pending=pending.promise;
  h.run('api=async()=>pending;');
  const selecting=h.run('selectAllFiles()');
  h.run('state.libraryRevision++;state.librarySeason=10;');
  pending.resolve({items:[{id:203,path:"/media/four.mkv",seasonNumber:2}]});
  await selecting;
  assert.deepEqual(Array.from(h.run('state.selected')),[101]);
});

test("season routes preserve specials and unknown files and restore the same season on Back",async()=>{
  const h=harness(), calls=[];
  h.context.record=path=>calls.push(path);
  h.run('controls=()=>{};state.info={roots:[],services:[{name:"sonarr"}]};loadJobs=async()=>{};api=async(path)=>{record(path);return path.includes("title=1") ? {media:{id:7,title:"Road Notes"}} : {total:0,items:[],seasons:[],season_groups:[]};};');
  for(const season of ['0','2','unknown']) {
    await h.run(`applyNavigation(navigationRoute("?view=files&source=sonarr&media=7&season=${season}"))`);
    assert.equal(new URLSearchParams(calls.at(-1).split('?')[1]).get('season'),season);
    assert.match(h.run('navigationQuery()'),new RegExp('season='+season));
  }
  assert.equal(h.run('navigationRoute("?season=-1").season'),null);
  assert.equal(h.run('navigationRoute("?season=1.5").season'),null);
});

test("the season header ignores selections from other seasons before all pages are loaded",()=>{
  const h=harness();
  h.run('state.info={transcode_enabled:true};state.workerInfo={ready:true};$("service").value="sonarr";state.media={id:7};state.librarySeason=2;state.libraryHasMore=true;state.selected=new Set([101]);state.files.set(101,{id:101,seasonNumber:1,path:"/media/one.mkv"});controls();');
  assert.equal(h.elements.get('select-all-files').indeterminate,false);
  h.run('state.files.set(201,{id:201,seasonNumber:2,path:"/media/two.mkv"});state.selected.add(201);controls();');
  assert.equal(h.elements.get('select-all-files').indeterminate,true);
});

test("maintenance actions never use native browser confirmations", () => {
  assert.doesNotMatch(source, /\bconfirm\s*\(/);
});

test("candidate review reads only, shows measured sizes and rechecks before approval", async () => {
  const h = harness(), calls = [];
  h.context.fixture = {id:"candidate",status:"waiting_decision",waiting_reason:"Candidate file size (1910766036 bytes) exceeds original (1599600732 bytes) by 19.5%, which is greater than max_size_increase_percent (0.0%)",waiting_options:[{decision:"accept_loss"}],source_path:"/media/Spiralis.mkv"};
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

test("accepting a candidate offers Accept and replace only where replacement is enabled", async () => {
  for (const allowed of [true, false]) {
    const h = harness(), calls = [];
    h.context.fixture = {id:"candidate",status:"waiting_decision",waiting_reason:"Review quality",waiting_options:[{decision:"accept_loss"}],source_path:"/media/one.mkv"};
    h.context.record = (...args) => calls.push(args);
    h.run(`state.info.allow_destructive=${allowed}; continueToReplacement=async()=>{}; api=async()=>({jobs:[fixture]}); jobControl=async(...args)=>record(...args);`);
    const review = h.run('reviewJobDecision("candidate","accept_loss")');
    for (let i=0;i<6;i++) await Promise.resolve();
    assert.equal(h.elements.get("confirm-action-review").textContent, allowed ? "Accept and replace" : "Accept candidate");
    h.elements.get("confirm-action-review").listeners.get("click")();
    await review;
    // Accepting is one decision; the replacement itself is approved only in its own review.
    assert.deepEqual(JSON.parse(JSON.stringify(calls)), [["candidate","action_resume",{decision:"accept_loss"}]]);
  }
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

function metricProgressHarness() {
  const h = harness();
  h.run(`Date.now = () => Date.parse("2026-10-16T12:22:54Z");
    globalThis.progressJob = {status:"waiting_external",worker:{
      transcode_status:"running",transcode_phase:"validating_quality",progress:73.3,progress_is_stale:true,
      last_progress_at:"2026-10-16T12:22:18.667341Z",worker_heartbeat_at:"2026-10-16T12:22:53Z"
    }};`);
  return h;
}
function progressText(element) {
  return [element.textContent, ...element.children.map(progressText)].join(" ");
}

test("a long final quality measurement shows its phase and heartbeat without declaring a stall", () => {
  for (const progress of [70, 73.3]) {
    const h = metricProgressHarness();
    h.context.measuredProgress = progress;
    h.run('progressJob.worker.progress=measuredProgress;');
    // A single measurement can last ten minutes, with only heartbeats.
    h.run('progressJob.worker.last_progress_at="2026-10-16T12:12:54Z";');
    const before = h.run('JSON.stringify(progressJob)');
    const rendered = h.run('telemetry(progressJob)');
    const text = progressText(rendered);
    assert.equal(h.run('queuePresentation(progressJob).status'), "Verifying quality");
    assert.equal(h.run('statusClass(queuePresentation(progressJob).status)'), "running");
    assert.match(text, /Measuring final quality/);
    assert.match(text, /Worker heartbeat 1s ago.*Last advance 10m 0s ago/);
    assert.match(text, /Final quality validation can take several minutes/);
    assert.ok(text.includes(`Last measured: ${progress.toFixed(1)}%`));
    assert.equal(rendered.children[1].value, progress);
    assert.doesNotMatch(text, /No updates|No recent progress|stale|About .* min/);
    assert.equal(h.run('JSON.stringify(progressJob)'), before);
  }
});

test("a missing, invalid or expired heartbeat cannot make a long metric look live", () => {
  for (const heartbeat of [undefined, "invalid", "0001-01-01T00:00:00Z", "2026-10-16T12:25:00Z", "2026-10-16T12:18:00Z"]) {
    const h = metricProgressHarness();
    h.context.heartbeat = heartbeat;
    h.run('progressJob.worker.worker_heartbeat_at=heartbeat;');
    const text = progressText(h.run('telemetry(progressJob)'));
    const stale = heartbeat === "2026-10-16T12:18:00Z";
    assert.equal(h.run('queuePresentation(progressJob).status'), stale ? "No heartbeat" : "No updates");
    assert.match(text, stale ? /No worker heartbeat for 4m 54s.*Measuring final quality/ : /Worker heartbeat unavailable/);
    assert.doesNotMatch(text, /Navigatorr has not checked|The worker has not reported/);
    assert.match(text, /Last reported: 73.3%/);
    assert.equal(h.run('telemetry(progressJob).children.length'), 1);
  }
  const h = metricProgressHarness();
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:18:00Z";progressJob.worker.progress_is_stale=false;');
  assert.equal(h.run('queuePresentation(progressJob).status'), "No heartbeat", "fresh percentage must not hide a lost heartbeat");
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:22:24Z";');
  assert.equal(h.run('queuePresentation(progressJob).status'), "Verifying quality");
  h.run('Date.now=()=>Date.parse("2026-10-16T12:26:00Z");');
  assert.equal(h.run('queuePresentation(progressJob).status'), "No heartbeat", "heartbeat must expire without new API data");
});

test("a heartbeat delayed by a busy coordinator is shown calmly, and silence is a warning, not a failure", () => {
  const h = metricProgressHarness();
  // Ninety seconds without an update: the coordinator was busy hashing another source.
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:21:24Z";progressJob.worker.last_worker_poll_at="2026-10-16T12:21:24Z";');
  assert.equal(h.run('queuePresentation(progressJob).status'), "Verifying quality");
  const delayed = progressText(h.run('telemetry(progressJob)'));
  assert.match(delayed, /Worker heartbeat 1m 30s ago/);
  assert.doesNotMatch(delayed, /No recent|has not/);
  // Prolonged silence while Navigatorr keeps polling blames the worker, in amber.
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:15:00Z";progressJob.worker.last_worker_poll_at="2026-10-16T12:22:50Z";');
  assert.equal(h.run('queuePresentation(progressJob).status'), "No heartbeat");
  assert.match(progressText(h.run('telemetry(progressJob)')), /The worker has not reported for 7m 54s/);
  for (const status of ["No heartbeat", "No updates"]) assert.equal(h.run(`statusClass(${JSON.stringify(status)})`), "waiting_decision");
  assert.equal(h.run('statusClass("Worker offline")'), "failed");
  // A phone clock slightly behind the worker is not "unknown".
  assert.equal(h.run('workerHeartbeatState("2026-10-16T12:23:30Z")'), "fresh");
  assert.equal(h.run('workerHeartbeatState("2026-10-16T12:30:00Z")'), "unknown");
});

test("full encoding still warns about stale progress with a live heartbeat and hides stale rates", () => {
  const h = metricProgressHarness();
  h.run('progressJob.worker.transcode_status="running";progressJob.worker.transcode_phase="encoding";progressJob.worker.speed=14.7;progressJob.worker.fps=238;progressJob.savings={eta_seconds:600};');
  const stale = progressText(h.run('telemetry(progressJob)'));
  assert.equal(h.run('queuePresentation(progressJob).status'), "No updates");
  assert.match(stale, /No recent encoding progress.*Worker heartbeat 1s ago.*73.3%/);
  assert.doesNotMatch(stale, /14.70|238|About|left|sample tasks/);
  h.run('progressJob.worker.progress_is_stale=false;');
  const fresh = progressText(h.run('telemetry(progressJob)'));
  assert.equal(h.run('queuePresentation(progressJob).status'), "Encoding");
  assert.match(fresh, /Encoding full video.*14.70.*238.0 fps.*~10 min left/);
  assert.doesNotMatch(fresh, /sample tasks|VMAF/);
});

test("batch rows show a smoothed encode ETA and the files left, only while speed is known", () => {
  const h = metricProgressHarness();
  h.run(`progressJob.status="waiting_external";progressJob.batch={total:8,completed:2,running:1,queued:4,waiting_decision:1};
    progressJob.worker={transcode_status:"running",transcode_phase:"encoding",progress:40,speed:1.5,fps:36,progress_is_stale:false,
      last_progress_at:"2026-10-16T12:22:53Z",worker_heartbeat_at:"2026-10-16T12:22:53Z"};
    progressJob.activities=[{id:"child",file:"/media/episode.mkv",eta_seconds:4200,worker:progressJob.worker,current_step:1,stages:[{name:"wait_benchmark"},{name:"wait_transcode"}]}];`);
  let text = progressText(h.run('batchProgress(progressJob)'));
  assert.match(text, /episode.mkv: Encoding full video · ~1 h 10 min left/);
  assert.match(text, /Current encode ~1 h 10 min left · about 5 files left/);
  assert.doesNotMatch(text, /Not time remaining/);
  assert.match(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), /Encoding full video · ~1 h 10 min left/);
  // A noisy new measurement 30 s later moves the estimate only part of the way.
  h.run('Date.now=()=>Date.parse("2026-10-16T12:23:24Z");progressJob.activities[0].eta_seconds=3000;progressJob.worker.last_progress_at="2026-10-16T12:23:23Z";progressJob.worker.worker_heartbeat_at="2026-10-16T12:23:23Z";');
  text = progressText(h.run('batchProgress(progressJob)'));
  assert.match(text, /~1 h 4 min left/);
  // Unknown speed hides the ETA rather than inventing one.
  h.run('progressJob.worker.speed=0;');
  text = progressText(h.run('batchProgress(progressJob)'));
  assert.doesNotMatch(text, / left/);
  // No estimate line appears unless a time was measured.
  assert.doesNotMatch(text, /Estimated workflow progress|Not time remaining/);
  // Files that finished are forgotten; the smoothing map stays bounded.
  h.run('for(let i=0;i<100;i++)encodeEta("old-"+i,{transcode_phase:"encoding",speed:1},600);');
  assert.ok(h.run('encodeEtas.size') <= 64);
  h.run('Date.now=()=>Date.parse("2026-10-16T12:40:00Z");encodeEta("fresh",{transcode_phase:"encoding",speed:1},600);');
  assert.equal(h.run('encodeEtas.size'), 1);
  assert.equal(h.run('remainingLabel(45)'), "less than a minute left");
  assert.equal(h.run('remainingLabel(3600)'), "~1 h left");
});

test("validation and publication have discrete progress but require a fresh heartbeat", () => {
  for (const [phase, status] of [["analysing_video", "Analyzing video"], ["validating", "Verifying"], ["validating_quality", "Verifying quality"], ["publishing", "Saving candidate"]]) {
    const h = metricProgressHarness();
    h.context.phase = phase;
    h.run('progressJob.worker.phase=phase;progressJob.worker.transcode_phase=phase;progressJob.worker.transcode_status="running";progressJob.worker.speed=14.7;progressJob.worker.fps=238;progressJob.savings={eta_seconds:600};');
    assert.equal(h.run('queuePresentation(progressJob).status'), status);
    const text = progressText(h.run('telemetry(progressJob)'));
    assert.match(text, /Last measured: 73.3%.*Worker heartbeat/);
    assert.doesNotMatch(text, /14.70|238|About|left|No recent/);
    h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:15:00Z";');
    assert.equal(h.run('queuePresentation(progressJob).status'), "No heartbeat");
  }
});

test("external waits and confirmed recovery take precedence over a recent heartbeat", () => {
  const h = metricProgressHarness();
  for (const condition of ["worker_unreachable", "transcode_retry", "worker_reconciling", "worker_busy"]) {
    h.context.condition = condition;
    h.run('progressJob.waiting_condition=condition;progressJob.waiting_reason="Waiting for coordination";');
    assert.equal(h.run('queuePresentation(progressJob).status'), condition === "worker_unreachable" ? "Worker offline" : condition === "worker_busy" ? "Waiting for a free worker slot" : "Waiting");
    assert.doesNotMatch(progressText(h.run('telemetry(progressJob)')), /Measuring final quality|Worker heartbeat/);
  }
  h.run('progressJob.waiting_condition="transcode_complete";progressJob.worker.recovery_required=true;progressJob.waiting_reason="Publication requires intervention";');
  assert.equal(h.run('queuePresentation(progressJob).status'), "Needs decision");
  assert.match(progressText(h.run('telemetry(progressJob)')), /Publication requires intervention/);
});

test("terminal actions and decision waits cannot be replaced by telemetry warnings", () => {
  const h = metricProgressHarness();
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:00:00Z";');
  for (const status of ["completed", "failed", "cancelled", "waiting_decision"]) {
    h.context.status = status;
    h.run('progressJob.status=status;');
    assert.equal(h.run('queuePresentation(progressJob).showTelemetry'), false);
    assert.doesNotMatch(h.run('queuePresentation(progressJob).status'), /No heartbeat|No updates|Verifying quality/);
    assert.doesNotMatch(progressText(h.run('telemetry(progressJob)')), /No recent|Worker heartbeat/);
  }
  for (const status of ["completed", "failed", "cancelled", "queued"]) {
    h.context.status = status;
    h.run('progressJob.status="running";progressJob.worker.transcode_status=status;progressJob.worker.phase=status;');
    assert.doesNotMatch(progressText(h.run('telemetry(progressJob)')), /No recent|Worker heartbeat/);
  }
});

test("batch file detail and worker slots agree about a long measurement and lost heartbeat", () => {
  const h = metricProgressHarness();
  h.run('progressJob.batch={total:2,running:1};progressJob.activities=[{id:"child",worker:progressJob.worker,current_step:0,stages:[{name:"wait_transcode"}]}];');
  const label = h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)');
  // The file line stays quiet while the worker is alive; the batch progress text keeps the heartbeat.
  assert.equal(label, "Measuring final quality");
  assert.match(progressText(h.run('batchProgress(progressJob)')), /Measuring final quality.*Worker heartbeat received/);
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:15:00Z";');
  assert.match(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), /No worker heartbeat for 7m 54s/);
  assert.match(progressText(h.run('batchProgress(progressJob)')), /No worker heartbeat for 7m 54s/);
  h.run('progressJob.worker.last_worker_poll_at="2026-10-16T12:15:00Z";');
  assert.match(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), /Navigatorr has not checked the worker for 7m 54s/);
  assert.match(progressText(h.run('batchProgress(progressJob)')), /Navigatorr has not checked the worker for 7m 54s/);
  h.run('renderWorkerActivity({available:true,activity:{details_available:true,worker_slots_total:1,worker_slots_used:1,jobs:[{id:"child",kind:"transcode",file:"episode.mkv",phase:"validating_quality",heartbeat_at:progressJob.worker.worker_heartbeat_at}]}});');
  assert.match(progressText(h.elements.get("worker-slots")), /No recent worker heartbeat/);
});

test("new submissions avoid a duplicate browser worker probe and rely on durable server admission", async () => {
  const h=harness(); let builds=0, checks=0;
  h.context.check=()=>checks++;
  h.context.build=()=>builds++;
  h.run('controls=()=>{}; refreshWorkers=async()=>{check();}; buildAndSubmitJob=async()=>build();');
  await h.run('submitJob()');
  assert.equal(builds,1); assert.equal(checks,0);
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
  h.run('target.textContent="Calculating…"; target.dataset.pending="1"; state.folder="/old";state.tab="library";state.libraryRevision=1;state.folderSizeTargets=new Map([["/old/child",target]]);api=async()=>pending;$("workspace").hidden=false;');
  const load=h.run('updateFolderSizes(1,"/old","",1)');
  h.run('state.libraryRevision=2;state.folder="/new";');
  pending.resolve({items:[{path:"/old/child",folder_size:{status:"ready",bytes:100}}]});
  await load;
  assert.equal(h.context.target.textContent,"Calculating…");
});

test("folder sizes are only asked for while one is measuring, only for its page, with growing gaps, and never in a hidden tab", async () => {
  const h=harness();
  const mk=(page,pending)=>Object.assign(new Element(),{dataset:{page:String(page),pending:pending?"1":"0"}});
  const settled=mk(0,false), slow=mk(200,true), slowToo=mk(200,true);
  h.context.settled=settled;h.context.slow=slow;h.context.slowToo=slowToo;
  const delays=[], fetched=[];
  h.context.setTimeout=(fn,ms)=>{delays.push(ms);return 1;};
  h.context.record=url=>{fetched.push(url);return {items:[{path:"/m/slow",folder_size:{status:"ready",bytes:5}}]};};
  h.run('state.folder="/m";state.tab="library";state.libraryRevision=1;$("workspace").hidden=false;state.folderSizeTargets=new Map([["/m/settled",[settled]],["/m/slow",[slow]],["/m/slow2",[slowToo]]]);api=async url=>record(url);');
  assert.deepEqual([...h.run("pendingSizePages()")],[200]);
  // Each round waits longer, up to a ceiling.
  h.run('state.folderSizesAttempt=0;for(let i=0;i<8;i++)scheduleFolderSizes(1,"/m","",300)');
  assert.deepEqual(delays,[2000,3000,5000,8000,12000,20000,20000,20000]);
  // One request, for the page that holds the pending folders, not for all three.
  await h.run('updateFolderSizes(1,"/m","",300)');
  assert.equal(fetched.length,1);
  assert.match(fetched[0],/offset=200/);
  // Every size settled: no further request is scheduled.
  delays.length=0;
  h.run('slow.dataset.pending="0";slowToo.dataset.pending="0";scheduleFolderSizes(1,"/m","",300)');
  assert.deepEqual(delays,[]);
  // A hidden tab asks for nothing, even with a size still measuring.
  h.run('slow.dataset.pending="1";document.hidden=true;scheduleFolderSizes(1,"/m","",300)');
  assert.deepEqual(delays,[]);
});

test("coming back to a Files tab that has been open a while checks every page once", async () => {
  const h=harness();
  const label=Object.assign(new Element(),{dataset:{page:"0",pending:"0"}});
  h.context.label=label;
  const fetched=[];h.context.record=url=>{fetched.push(url);return {items:[]};};
  h.run('state.folder="/m";state.tab="library";state.libraryRevision=1;$("workspace").hidden=false;state.folderSizeTargets=new Map([["/m/a",[label]]]);state.folderSizesArgs={revision:1,path:"/m",query:"",loaded:250};api=async url=>record(url);state.folderSizesCheckedAt=Date.now();');
  h.run("resumeFolderSizes()");
  assert.equal(fetched.length,0);
  h.run("state.folderSizesCheckedAt=Date.now()-6*60*1000;resumeFolderSizes()");
  await Promise.resolve();await Promise.resolve();
  assert.deepEqual(fetched.map(u=>/offset=(\d+)/.exec(u)[1]),["0","100","200"]);
});

// Ticking several files at once: Shift+click and Shift+arrow tick a range.
function selectionRows(h, names, folders = []) {
  const rows = names.map(name => Object.assign(new Element(), { tagName: "INPUT", type: "checkbox", dataset: { selection: `/m/${name}`, ...(folders.includes(name) ? { folder: `/m/${name}` } : {}) }, checked: false }));
  h.run('$("service").value="folder:/m";state.folderSelected=new Set();fileSelectionChanged=()=>{};');
  h.elements.get("library-items").querySelectorAll = () => rows;
  return rows;
}
const press = (h, row, key, shiftKey = false) => { let stopped = false; h.elements.get("library-items").listeners.get("keydown")({ target: row, key, shiftKey, preventDefault() { stopped = true; } }); return stopped; };
const clickRow = (h, row, shiftKey = false) => { row.checked = !row.checked; h.elements.get("library-items").listeners.get("click")({ target: row, shiftKey }); };
const picked = h => [...h.run("state.folderSelected")].map(path => path.slice(3)).sort();

test("Shift+click ticks every file between the last one ticked and this one", () => {
  const h = harness();
  const rows = selectionRows(h, ["a", "b", "c", "d", "e"], ["dir"]);
  clickRow(h, rows[1]);
  clickRow(h, rows[3], true);
  assert.deepEqual(picked(h), ["b", "c", "d"]);
  assert.deepEqual(rows.map(r => r.checked), [false, true, true, true, false]);
  // Shift+click backwards, on an unticked row after a ticked one, unticks the range.
  clickRow(h, rows[4]);
  clickRow(h, rows[2], true);
  assert.deepEqual(picked(h), ["b"]);
});

test("Shift+arrow grows and shrinks a range from the row it started on; arrows alone only move", () => {
  const h = harness();
  const rows = selectionRows(h, ["a", "b", "c", "d", "e"]);
  let focused = null;
  rows.forEach(row => { row.focus = () => { focused = row; }; });
  assert.equal(press(h, rows[1], "ArrowDown"), true);
  assert.equal(focused, rows[2]);
  assert.deepEqual(picked(h), []);
  press(h, rows[1], "ArrowDown", true);
  assert.deepEqual(picked(h), ["b", "c"]);
  press(h, rows[2], "ArrowDown", true);
  assert.deepEqual(picked(h), ["b", "c", "d"]);
  // Back up: the row left behind is unticked, the starting row stays.
  press(h, rows[3], "ArrowUp", true);
  assert.deepEqual(picked(h), ["b", "c"]);
  press(h, rows[2], "ArrowUp", true);
  press(h, rows[1], "ArrowUp", true);
  assert.deepEqual(picked(h), ["a", "b"]);
  // The ends of the list stop the move, and other keys are left alone.
  assert.equal(press(h, rows[0], "ArrowUp"), false);
  assert.equal(press(h, rows[4], "ArrowDown"), false);
  assert.equal(press(h, rows[1], "Enter"), false);
});

test("a folder row is never part of a range", () => {
  const h = harness();
  const rows = selectionRows(h, ["a", "dir", "b"], ["dir"]);
  rows.forEach(row => { row.focus = () => {}; });
  clickRow(h, rows[0]);
  clickRow(h, rows[2], true);
  assert.deepEqual(picked(h), ["a", "b"]);
  press(h, rows[0], "ArrowDown", true);
  assert.deepEqual(picked(h), ["a", "b"]);
});

// Dragging over the checkboxes, and "Select range" for screens with no Shift key.
// A tap as the page sees it: the click, then the change event that records the row.
const tapRow = (h, row, shiftKey = false) => { clickRow(h, row, shiftKey); h.run(`state.folderSelected.${row.checked ? "add" : "delete"}(${JSON.stringify(row.dataset.selection)})`); };
const withRects = rows => rows.forEach((row, i) => { row.getBoundingClientRect = () => ({ top: i * 40, bottom: i * 40 + 40 }); });
const pointer = (h, type, rowIndex, extra = {}) => { const list = h.elements.get("library-items"); const row = extra.target; const event = { type, pointerId: 1, pointerType: "touch", button: 0, isPrimary: true, clientY: rowIndex * 40 + 20, target: row, preventDefault() {}, ...extra }; list.listeners.get(type)(event); };
const dragFrom = (h, rows, from) => pointer(h, "pointerdown", from, { target: rows[from] });
const dragTo = (h, to) => pointer(h, "pointermove", to);
const dragEnd = (h, to, type = "pointerup") => pointer(h, type, to);

test("dragging down the checkboxes ticks every file crossed; a tap on one row is left alone", () => {
  const h = harness();
  const rows = selectionRows(h, ["a", "b", "c", "d", "e"], ["dir"]);
  withRects(rows);
  // Down and up on the same row: nothing is ticked by the drag, the click does it.
  dragFrom(h, rows, 1); dragTo(h, 1); dragEnd(h, 1);
  assert.deepEqual(picked(h), []);
  tapRow(h, rows[0]);
  assert.deepEqual(picked(h), ["a"]);
  // Down on an empty row and drag down: that row and every one crossed are ticked.
  dragFrom(h, rows, 1); dragTo(h, 2); dragTo(h, 3);
  assert.deepEqual(picked(h), ["a", "b", "c", "d"]);
  // Back up one: the row left behind returns to how it was.
  dragTo(h, 2);
  assert.deepEqual(picked(h), ["a", "b", "c"]);
  dragEnd(h, 2);
  // The click that follows the release does not flip the last row back.
  let prevented = false;
  h.elements.get("library-items").listeners.get("click")({ target: rows[2], shiftKey: false, preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.deepEqual(picked(h), ["a", "b", "c"]);
  // The row the drag ended on is where Shift+click extends from.
  tapRow(h, rows[4], true);
  assert.deepEqual(picked(h), ["a", "b", "c", "d", "e"]);
});

test("a drag that starts on a ticked row unticks, upward too; a cancelled drag puts everything back", () => {
  const h = harness();
  const rows = selectionRows(h, ["a", "b", "c", "d"]);
  withRects(rows);
  rows.forEach(row => { row.checked = true; });
  h.run('state.folderSelected=new Set(["/m/a","/m/b","/m/c","/m/d"])');
  dragFrom(h, rows, 3); dragTo(h, 2); dragTo(h, 1);
  assert.deepEqual(picked(h), ["a"]);
  dragEnd(h, 1, "pointercancel");
  assert.deepEqual(picked(h), ["a", "b", "c", "d"]);
  assert.deepEqual(rows.map(r => r.checked), [true, true, true, true]);
  // After a cancel no click is swallowed.
  let prevented = false;
  h.elements.get("library-items").listeners.get("click")({ target: rows[0], shiftKey: false, preventDefault() { prevented = true; } });
  assert.equal(prevented, false);
});

test("a drag cannot start on a folder row, with a second finger or with another mouse button", () => {
  const h = harness();
  const rows = selectionRows(h, ["a", "dir", "b", "c"], ["dir"]);
  withRects(rows);
  dragFrom(h, rows, 1); dragTo(h, 2); dragTo(h, 3); dragEnd(h, 3);
  assert.deepEqual(picked(h), []);
  pointer(h, "pointerdown", 0, { target: rows[0], pointerType: "mouse", button: 2 }); dragTo(h, 2); dragEnd(h, 2);
  pointer(h, "pointerdown", 0, { target: rows[0], isPrimary: false }); dragTo(h, 2); dragEnd(h, 2);
  assert.deepEqual(picked(h), []);
  // A folder row in the middle of a drag is skipped, not ticked.
  dragFrom(h, rows, 0); dragTo(h, 1); dragTo(h, 2); dragEnd(h, 2);
  assert.deepEqual(picked(h), ["a", "b"]);
});

test("Select range arms the next tap as a Shift+click, only once a file is ticked", () => {
  const h = harness();
  const rows = selectionRows(h, ["a", "b", "c", "d"]);
  h.run("controls=()=>{}");
  const button = h.elements.get("select-range");
  // Nothing ticked yet: the button does not arm.
  button.listeners.get("click")();
  assert.equal(h.run("state.rangeArmed"), false);
  tapRow(h, rows[1]);
  assert.equal(h.run("rangeAnchorReady()"), true);
  button.listeners.get("click")();
  assert.equal(h.run("state.rangeArmed"), true);
  // Tapping the same button again cancels it.
  button.listeners.get("click")();
  assert.equal(h.run("state.rangeArmed"), false);
  button.listeners.get("click")();
  tapRow(h, rows[3]);
  assert.deepEqual(picked(h), ["b", "c", "d"]);
  assert.equal(h.run("state.rangeArmed"), false);
  // An armed tap on a ticked row still ticks the range rather than clearing it.
  rows[1].checked = false; h.run('state.folderSelected.delete("/m/b")');
  tapRow(h, rows[1]);
  h.run("state.selectionAnchor='/m/d'");
  button.listeners.get("click")();
  tapRow(h, rows[2]);
  assert.deepEqual(picked(h), ["b", "c", "d"]);
  // Unticking the anchor row disarms and hides the button in the real controls().
  const fresh = harness();
  const list = selectionRows(fresh, ["a", "b", "c"]);
  fresh.run('fileSelectionChanged=()=>{};state.libraryLoading=false;state.selectionLoading=false;state.selectionAnchor="/m/a";state.rangeArmed=true;');
  list[0].checked = false;
  fresh.run("state.rangeArmed=rangeAnchorReady()&&state.rangeArmed");
  assert.equal(fresh.run("state.rangeArmed"), false);
});

test("Encode again sends the file back through the goal it already has", () => {
  const h = harness();
  const request = plan => h.run(`sameSettingsRequest(${JSON.stringify(plan)})`);
  const base = { id: "b-1", item_key: "k", selection_version: "v1", decision_version: "d1" };
  const kept = { preserve_limits: true, preserve_savings: true, preserve_growth: true };
  // A batch made with a profile keeps each file's own.
  assert.deepEqual({ ...request({ ...base, settings: {} }) }, { id: "b-1", scope: "file", item_key: "k", selection_version: "v1", decision_version: "d1", profile: "same", ...kept });
  // A goal keeps the goal; size also keeps the engine and the size budget it was frozen with.
  const size = request({ ...base, mode: "size", settings: { allow_hardware: false, hardware_margin_percent: 12, size_budget_percent: 40 } });
  assert.deepEqual([size.mode, size.allow_hardware, size.hardware_margin_percent, size.size_budget_percent], ["size", false, 12, 40]);
  assert.equal("size_budget_bytes" in size, false);
  const siblings = request({ ...base, mode: "size", settings: { size_budget_source: "siblings" } });
  assert.deepEqual([siblings.allow_hardware, siblings.size_budget_source, siblings.size_budget_sibling_multiplier], [true, "siblings", 1.5]);
  assert.equal("allow_hardware" in request({ ...base, mode: "quality", settings: {} }), false);
});

test("a cancelled or failed file offers Encode again; a running or locked one does not", async () => {
  const items = [
    { item_key: "k-c", display_label: "Cancelled", status: "cancelled", child_action_id: "t-c", file_path: "/media/tv/C.mkv", can_reconfigure: true, mode: "size" },
    { item_key: "k-f", display_label: "Failed", status: "failed", child_action_id: "t-f", file_path: "/media/tv/F.mkv", can_reconfigure: true, mode: "size" },
    { item_key: "k-r", display_label: "Running", status: "running", child_action_id: "t-r", file_path: "/media/tv/R.mkv", can_reconfigure: true, mode: "size" },
    { item_key: "k-l", display_label: "Locked", status: "cancelled", child_action_id: "t-l", file_path: "/media/tv/L.mkv", mode: "size" },
  ];
  const h = harness();
  h.context.items = items;
  h.run('state.info={allow_destructive:true,roots:["/media"]};state.detail="batch-a";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:4});');
  await h.run('loadBatchItems("batch-a",2)');
  const rows = itemRows(h);
  assert.deepEqual(rows.map((row) => buttonLabels(row).includes("Encode again")), [true, true, false, false]);
  // It submits the file's own settings without opening the goal dialog.
  const calls = [];
  h.context.record = (path, body) => { calls.push([path, body]); return {}; };
  h.run('api=async()=>({id:"batch-a",item_key:"k-c",mode:"size",selection_version:"v",decision_version:"d",settings:{allow_hardware:true}});commandRequest=async(path,body)=>record(path,body);loadJobs=async()=>{};notify=()=>{};');
  await flatten(rows[0]).find((e) => e.textContent === "Encode again").listeners.get("click")();
  assert.equal(calls[0][0], "batch-settings");
  assert.deepEqual([calls[0][1].scope, calls[0][1].item_key, calls[0][1].mode], ["file", "k-c", "size"]);
  assert.equal(h.elements.get("reconfigure-batch")?.open || false, false);
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

test("multiple selections and folder discovery freeze the explicit batch before configuration", async () => {
  const h = harness();
  h.run(
    'controls = () => {}; $("service").value = "folder:/media"; state.folder = "/media/season"; state.folderSelected = new Set(["/media/season/one.mp4", "/media/season/two.mp4"]); configureSelection();',
  );
  assert.equal(h.elements.get("scope").value, "batch");
  assert.equal(h.elements.get("selected-only").checked, true);
  assert.equal(h.run("state.folderSelected.size"), 2);
  h.elements.get("back-to-files").listeners.get("click")();
  h.run('api=async()=>({paths:["/media/season/one.mp4","/media/season/two.mp4","/media/season/three.mp4"]});');
  await h.elements.get("use-container").listeners.get("click")();
  assert.equal(h.elements.get("scope").value, "batch");
  assert.equal(h.elements.get("selected-only").checked, true);
  assert.equal(h.run("state.folderSelected.size"), 3);
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

function submissionHarness(options) {
  const h = harness(options),
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
  assert.match(waiting.summary, /Your decision is needed/);
  assert.doesNotMatch(waiting.summary, /Review quality result/, "the reason is shown once, on the outcome line");
  assert.match(h.run('queueOutcome({status:"waiting_decision",waiting_reason:"Review quality result"})'), /Review quality result · Originals unchanged/);
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
  assert.match(replacedSource.summary, /Your decision is needed/);
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
  assert.match(result.children[0].textContent, /Step 3\/4.*Verify original and prepare recovery/);
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
  h.context.current = {id:"replace",status:"waiting_decision",waiting_options:[{decision:"approve"}],promotion:{original_path:"/media/one.mkv",candidate_path:"/media/.candidates/one.mkv",original_sha256:"original",candidate_sha256:"candidate",original_bytes:1000,candidate_bytes:400,plan_digest:"sha256:plan"}};
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

test("approving a replacement closes the review once the server accepts it", async () => {
  const h = harness(), events = [];
  h.context.current = {id:"replace",status:"waiting_decision",waiting_options:[{decision:"approve"}],promotion:{original_path:"/media/one.mkv",candidate_path:"/media/.candidates/one.mkv",original_sha256:"original",candidate_sha256:"candidate",original_bytes:1000,candidate_bytes:400,plan_digest:"sha256:plan"}};
  h.context.record = (...args) => events.push(args);
  h.run('api = async () => ({jobs:[current]}); openJob = async (id) => record("open", id); jobControl = async (id, name, args, opts) => { record(name, args.decision, $("file-review").open); opts.onAccepted(); record("accepted", $("file-review").open); };');
  await h.run('reviewFilePromotion("replace")');
  await h.elements.get("approve-file-review").listeners.get("click")();
  assert.deepEqual(events, [["action_resume", "approve", true], ["accepted", false]]);
  assert.equal(h.elements.get("file-review").open, false);
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
  // Search is a mode: the source toolbar gives way to the search row.
  assert.equal(h.elements.get("library-search-label").hidden, false);
  assert.equal(h.elements.get("library-tools").hidden, true);
  h.elements.get("search").value = "no matching file";
  toggle.listeners.get("click")();
  assert.equal(h.elements.get("library-search-label").hidden, true);
  assert.equal(h.elements.get("library-tools").hidden, false);
  assert.equal(h.elements.get("search").value, "");
  assert.equal(attrs["aria-expanded"], "false");
  assert.equal(attrs["aria-label"], "Show search");
  assert.equal(h.run("state.restoredSearch"), "");
});

const reloadCounter =
  'loadLibrary = async () => { state.restoredSearch = $("search").value; state.libraryReloads = (state.libraryReloads || 0) + 1; };';

test("one tap on the search row's close button clears the filter and restores the toolbar", () => {
  const h = harness();
  h.run(reloadCounter);
  h.document.getElementById("library-search-label").hidden = true;
  h.elements.get("toggle-library-search").listeners.get("click")();
  h.elements.get("search").value = "episode";
  h.run("state.libraryOffset = 40");
  h.elements.get("close-library-search").listeners.get("click")();
  assert.equal(h.elements.get("library-search-label").hidden, true);
  assert.equal(h.elements.get("library-tools").hidden, false);
  assert.equal(h.elements.get("search").value, "");
  assert.equal(h.run("state.libraryOffset"), 0);
  assert.equal(h.run("state.restoredSearch"), "");
  assert.equal(h.run("state.libraryReloads"), 1);
});

test("Escape in the search box closes search, while Enter still searches", () => {
  const h = harness();
  h.run(reloadCounter);
  h.document.getElementById("library-search-label").hidden = true;
  h.elements.get("toggle-library-search").listeners.get("click")();
  const key = h.elements.get("search").listeners.get("keydown");
  h.elements.get("search").value = "episode";
  let prevented = 0;
  key({ key: "Enter", preventDefault: () => prevented++ });
  assert.equal(h.run("state.restoredSearch"), "episode");
  assert.equal(h.elements.get("library-search-label").hidden, false);
  key({ key: "a", preventDefault: () => prevented++ });
  assert.equal(h.elements.get("library-search-label").hidden, false);
  key({ key: "Escape", preventDefault: () => prevented++ });
  assert.equal(h.elements.get("library-search-label").hidden, true);
  assert.equal(h.elements.get("library-tools").hidden, false);
  assert.equal(h.elements.get("search").value, "");
  assert.equal(h.run("state.restoredSearch"), "");
  assert.equal(h.run("state.libraryReloads"), 2);
  assert.equal(prevented, 2);
});

test("the Files header stays compact: one toolbar, one location row, and a one-line step 2", async () => {
  const css = await readFile(new URL("./assets/app.css", import.meta.url), "utf8");
  const browser = markup.slice(
    markup.indexOf('class="library-browser"'),
    markup.indexOf('id="library-items"'),
  );
  // The source select is labelled for assistive tech, not with visible text.
  assert.match(browser, /<select id="service" aria-label="Source">/);
  assert.doesNotMatch(browser, /<label>Source/);
  assert.doesNotMatch(browser, /section-label/);
  // Reload lives in the toolbar; the totals share the location row.
  const tools = browser.slice(
    browser.indexOf('id="library-tools"'),
    browser.indexOf('id="library-search-label"'),
  );
  assert.match(tools, /id="reload-library"/);
  assert.match(tools, /id="open-path"/);
  const location = browser.slice(
    browser.indexOf('class="breadcrumbs"'),
    browser.indexOf('id="selection-toolbar"'),
  );
  assert.match(location, /id="library-total"/);
  // On a phone, step 2 hides the browser and shows one back/summary line.
  assert.match(css, /#library\[data-file-step="configure"\] \.library-browser\s*\{\s*display: none;/);
  assert.match(css, /\.step-line\s*\{\s*display: grid;/);
  assert.match(css, /#job-form > h2\s*\{\s*position: absolute;/);
  const step = markup.slice(
    markup.indexOf('class="step-line"'),
    markup.indexOf('id="processing-goal"'),
  );
  assert.match(step, /id="back-to-files"/);
  assert.match(step, /id="source-summary"/);
  assert.match(step, /2 of 2/);
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
    "controls = () => {}; state.detail = 'old'; state.batchReview = {id:'old'};",
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
  assert.equal(h.run("state.batchReview"), null);
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
  h.run("api = async () => plan; controls = () => {};");
  const pending = h.run('reviewBatchReplacement("batch-a")');
  h.run("invalidateAuthentication()");
  plan.resolve({
    prepared: true,
    id: "batch-a",
    digest: "verified",
    approved: false,
    waiting: true,
    members: [{ item_key: "file" }],
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
  assert.equal(h.elements.get("job-detail-title").textContent, "b");
  assert.equal(h.run("state.detailJob.id"), "b");
});

test("a job the list already holds is shown at once, before the server answers", async () => {
  const h = harness(), answer = deferred();
  h.run('state.operationJobs = new Map([["wf", {id:"known", workflow_id:"wf", status:"failed", action_name:"transcode_media", source_path:"/media/a.mkv"}]]); api = async () => ({jobs:[await answerPromise]}); tool = async () => ({data:{}});');
  h.context.answerPromise = answer.promise;
  const opening = h.run('openJob("known")');
  // Nothing has come back yet, and the dialog already says what the list knew.
  assert.equal(h.run("state.detailJob.id"), "known");
  assert.equal(h.elements.get("detail-summary").dataset.operationId, "known");
  answer.resolve({id: "known", workflow_id: "wf", status: "completed", action_name: "transcode_media", source_path: "/media/a.mkv"});
  await opening;
  assert.equal(h.run("state.detailJob.status"), "completed", "the server's answer corrects it");
});

test("opening a job shows nothing of the one open before: its files and command history are cleared", async () => {
  const h = harness(), answer = deferred();
  h.run('api = async () => ({jobs:[await answerPromise]}); tool = async () => ({data:{}});');
  h.context.answerPromise = answer.promise;
  h.run('$("batch-items-list").replaceChildren({textContent:"old file"}); $("batch-items-note").textContent = "3 of 3 files"; $("detail-command-list").replaceChildren({textContent:"old command"}); $("detail-commands").hidden = false;');
  h.run('state.detailJob = {id:"old", batch:{total:3}};');
  const opening = h.run('openJob("next")');
  assert.equal(h.elements.get("batch-items-list").children.length, 0);
  assert.equal(h.elements.get("batch-items-note").textContent, "Loading files…");
  assert.equal(h.elements.get("detail-command-list").children.length, 0);
  assert.equal(h.elements.get("detail-commands").hidden, true);
  assert.equal(h.run("state.detailJob"), null);
  answer.resolve({id: "next", status: "completed", action_name: "transcode_media", source_path: "/media/a.mkv"});
  await opening;
});

test("opening another job moves focus off the cleared rows and clears the old announcement", async () => {
  const h = harness();
  h.run('api = async () => ({jobs:[{id:"next", status:"completed", action_name:"transcode_media"}]}); tool = async () => ({data:{}});');
  const button = {id: "row-button"};
  let focused = null;
  h.run('$("job-detail-title")').focus = () => { focused = "title"; };
  h.run('$("job-detail")').contains = el => el === button;
  h.document.activeElement = button;
  h.run('$("detail-command-announcer").textContent = "Retry started"; $("detail-summary").dataset.operationId = "old"; state.detail = "old"; $("job-detail").open = true;');
  await h.run('openJob("next")');
  assert.equal(focused, "title");
  // A dialog that was closed keeps showModal's own initial focus.
  focused = null;
  h.run('$("job-detail").open = false; state.detail = null;');
  await h.run('openJob("another")');
  assert.equal(focused, null);
  assert.equal(h.run('$("detail-command-announcer").textContent'), "");
  assert.notEqual(h.run('$("detail-summary").dataset.operationId'), "old");
});

test("the action bar of a file job shows View batch first", () => {
  const h = harness();
  const child = JSON.stringify({id: "c", status: "failed", action_name: "transcode_media", parent_action_id: "b", parent_batch_id: "b", parent_item_key: "k", mode: "x"});
  const labels = h.run(`detailActionBar(${child})`).map(c => c.textContent.trim());
  assert.equal(labels[0], "View batch");
});

test("an action on a file refreshes the open batch details at once, whichever job it targets", async () => {
  const h = harness();
  h.context.refreshed = 0;
  h.run('tool = async () => ({}); loadJobs = async () => {}; refreshDetail = async () => { refreshed++; }; $("job-detail").open = true; state.detail = "batch";');
  await h.run('jobControl("child-file", "action_resume", {decision: "recheck"})');
  assert.equal(h.context.refreshed, 1);
  // Nothing open, nothing to refresh.
  h.run('$("job-detail").open = false;');
  await h.run('jobControl("child-file", "action_resume", {decision: "recheck"})');
  assert.equal(h.context.refreshed, 1);
});

test("opening the job that is already open keeps its files and command history", async () => {
  const h = harness();
  h.run('api = async () => ({jobs:[{id:"same", status:"completed", action_name:"transcode_media"}]}); tool = async () => ({data:{}}); $("job-detail").open = true; state.detail = "same"; $("batch-items-list").replaceChildren({textContent:"kept"});');
  await h.run('openJob("same")');
  assert.equal(h.run('$("batch-items-list").children.length'), 1);
});

test("an archived job is announced before the list reloads", async () => {
  const h = harness(), reload = deferred(), order = [];
  h.context.record = (what) => order.push(what);
  h.run('state.info = {}; api = async () => ({archived:true}); notify = (m) => record("notify"); loadJobs = async () => { record("reload"); await reloadPromise; };');
  h.context.reloadPromise = reload.promise;
  const archiving = h.run('archiveJob({id:"done"}, true)');
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(order, ["notify", "reload"], "the person is told before the reload finishes");
  reload.resolve();
  await archiving;
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
    prepared: true,
    id: "batch-a",
    digest: "sha256:plan",
    approved: false,
    waiting: true,
    members: [
      {
        item_key: "episode-1",
        original_path: "/source.mkv",
        candidate_path: "/candidate.mkv",
      },
    ],
  };
  h.context.record = (name) => calls.push(name);
  h.run("api = async (path) => {record(path); return plan;}; tool = async (name) => {record(name); return {data:null};};");
  const controls = h.run(
    'jobControls({id:"batch-a",status:"waiting_decision",action_name:"transcode_batch",batch:{promotion_plan_ready:true},waiting_options:[{decision:"approve",description:"Aprobar"}]})',
  );
  await controls.children[0].listeners.get("click")();
  // The plan is read from the server's own list; nothing is resumed.
  assert.deepEqual(calls, ["batch-candidates?id=batch-a&plan=1", "action_detail"]);
  assert.equal(h.elements.get("batch-review").open, true);
  assert.equal(h.elements.get("reject-batch-review").hidden, false);
  assert.match(JSON.stringify(h.elements.get("batch-review-data").children), /\/source.mkv/);
  assert.match(
    JSON.stringify(h.elements.get("batch-review-data").children),
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

test("encoder change clears incompatible knobs", () => {
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
  // A stored optimization block is carried through untouched, not edited.
  assert.deepEqual(p.optimization.search, {bitrate_values: [3000, 4000], max_candidates: 2});
  assert.match(h.elements.get("notice").textContent, /reset/);
  h.elements.get("recipe-encoder").value = "hevc_videotoolbox";
  h.elements.get("recipe-encoder").listeners.get("change")();
  const vt = JSON.parse(h.run("JSON.stringify(recipeFromControls())"));
  assert.equal(vt.video.quality, 65);
  assert.equal(vt.video.preset, undefined);
  assert.equal(vt.video.tune, undefined);
  assert.deepEqual(vt.optimization.search, {bitrate_values: [3000, 4000], max_candidates: 2});
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

// The phone layout's rows: a node opens onto its slots (the table is the wide-screen view of the same data).
const slotRows=h=>h.elements.get("worker-slots").children[0].children.flatMap(details=>details.children[1].children);

test("worker activity shows occupied slots, final measurement, elapsed time and unknown connection honestly", () => {
  const h=harness();
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:2,worker_slots_used:2,jobs:[
    {id:"first",file:"sample-one.mkv",kind:"transcode",phase:"validating_quality",started_at:new Date(Date.now()-120000).toISOString(),heartbeat_at:new Date().toISOString(),last_progress_at:new Date(Date.now()-10000).toISOString(),progress:84.4,},
    {id:"second",file:"sample-two.mkv",kind:"transcode",phase:"encoding",heartbeat_at:new Date().toISOString(),progress:30},
  ]}};
  h.run('renderWorkerActivity(activity)');
  assert.match(h.elements.get("worker-capacity").textContent,/2\/2 occupied/);
  const first=slotRows(h)[0];
  assert.match(first.children[0].textContent,/sample-one/);
  // One short line under the bar; the freshness of the last measurement is its tooltip.
  assert.match(first.children[2].textContent,/^Encode finished · measuring quality · Full conversion · .*elapsed$/);
  assert.match(first.children[2].title,/Last advance 10s ago/);
  assert.equal(first.children.length,3);
  // The encode's last 84.4% lingers in the progress file; once it is over the bar sweeps without a number.
  const after=first.children[1];
  assert.match(after.className,/batch-progress slot-progress/);
  assert.deepEqual(after.children.map(c=>c.textContent),["Measuring quality",""]);
  assert.equal(after.children.some(c=>c.className==="batch-progress-percent"),false);
  assert.equal(after.children[1].className,"slot-progress-bar");
  h.run('activity.activity.jobs.shift(); activity.activity.worker_slots_used=1;renderWorkerActivity(activity)');
  assert.match(slotRows(h)[0].children[0].textContent,/Slot 1 · Available/);
  assert.equal(slotRows(h)[0].children.length,1,"a free slot is a single line");
  assert.match(slotRows(h)[1].children[0].textContent,/Slot 2 · sample-two/);
  h.run('renderWorkerActivity({available:false})');
  assert.match(h.elements.get("worker-capacity").textContent,/Unknown/);
  assert.doesNotMatch(h.elements.get("worker-capacity").textContent,/0\/2/);
});

test("a slot shows the encode's measured percentage in the batch bar, and a still muted one once the worker goes quiet", () => {
  const h=harness();
  const beat=new Date().toISOString();
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:3,worker_slots_used:3,jobs:[
    {id:"enc",file:"sample-one.mkv",kind:"transcode",phase:"encoding",heartbeat_at:beat,last_progress_at:beat,progress:42.46,encoding:"x265 CRF 22",speed:4.944,fps:118.36},
    {id:"pass1",file:"sample-two.mkv",kind:"transcode",phase:"analysing_video",heartbeat_at:beat,progress:100},
    {id:"quiet",file:"sample-three.mkv",kind:"transcode",phase:"encoding",heartbeat_at:new Date(Date.now()-600000).toISOString(),progress:12},
  ]}};
  h.run('renderWorkerActivity(activity)');
  const rows=slotRows(h);
  const encoding=rows[0].children[1];
  assert.deepEqual(encoding.children.map(c=>c.className||""),["batch-progress-count","batch-progress-percent",""]);
  assert.deepEqual(encoding.children.map(c=>c.textContent).slice(0,2),["Encoding","42.5%"]);
  assert.equal(encoding.children[2].max,100);
  assert.equal(encoding.children[2].value,42.46);
  assert.doesNotMatch(encoding.className,/stale/);
  // Pass 1 has no progress of its own; a stray number is not shown.
  const analysing=rows[1].children[1];
  assert.deepEqual(analysing.children.map(c=>c.textContent),["Analyzing",""]);
  assert.match(rows[1].children[2].textContent,/^Pass 1 of 2 · no percentage yet · Full conversion/);
  // The encode being run and its speed lead the one line under an encoding slot's bar.
  assert.match(rows[0].children[2].textContent,/^x265 CRF 22 · 4\.94× · Full conversion/);
  assert.match(rows[0].children[2].title,/118\.4 fps/);
  assert.deepEqual(rows.slice(0,3).map(row=>row.children.length),[3,3,3]);
  // A worker that stopped reporting keeps its last value but loses the live look.
  const quiet=rows[2].children[1];
  assert.match(quiet.className,/stale/);
  assert.equal(quiet.children[1].textContent,"12.0%");
  assert.match(rows[2].children[2].textContent,/^No recent worker heartbeat · Full conversion/);
  assert.match(rows[2].children[2].className,/warn/);
});

test("an encoding slot with nothing to report yet still shows a single short line", () => {
  const h=harness();
  const beat=new Date().toISOString();
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:2,worker_slots_used:2,jobs:[
    {id:"fresh",file:"sample-one.mkv",kind:"transcode",phase:"encoding",heartbeat_at:beat},
    {id:"reading",file:"sample-two.mkv",kind:"transcode",phase:"reading_source",heartbeat_at:beat,encoding:"x265 CRF 22"},
  ]}};
  h.run('renderWorkerActivity(activity)');
  const rows=slotRows(h);
  assert.match(rows[0].children[2].textContent,/^Waiting for the first update · Full conversion/);
  assert.match(rows[1].children[2].textContent,/^x265 CRF 22 · Reading the source · Full conversion/);
  assert.deepEqual(rows.map(row=>row.children.length),[3,3]);
});

test("slot progress reuses the green batch bar and only animates while the worker is alive", () => {
  assert.match(css, /\.worker-slot \.slot-progress \{[^}]*margin-top:6px;/);
  assert.match(css, /\.slot-progress-bar::after \{[^}]*background: var\(--forest\);[^}]*animation: slot-progress-sweep/);
  assert.match(css, /\.batch-progress\.stale \.slot-progress-bar::after \{ background: var\(--muted\); animation: none; width: 0; \}/);
  assert.match(css, /@media \(prefers-reduced-motion: reduce\) \{\s*\.slot-progress-bar::after \{ animation: none;/);
  assert.match(css, /\.worker-slot \.batch-progress\.slot-progress \.batch-progress-count,\s*\.worker-slot \.batch-progress\.slot-progress \.batch-progress-percent \{ line-height: 1\.4; \}/);
});

test("worker activity labels video and podcast slots and shows each pool's use", () => {
  const h=harness();
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:3,worker_slots_used:2,
    slots:{ceiling:3,host_cpus:10,split:true,video:{total:2,used:1},podcast:{total:1,used:1}},
    jobs:[
      {id:"pod",file:"episode.mp3",kind:"podcast",phase:"transcribing",heartbeat_at:new Date().toISOString()},
      {id:"vid",file:"movie.mkv",kind:"transcode",phase:"encoding",heartbeat_at:new Date().toISOString(),progress:10},
    ]}};
  h.run('renderWorkerActivity(activity)');
  assert.match(h.elements.get("worker-slot-usage").textContent,/Video 1\/2 · Podcast 1\/1 · Ceiling 3/);
  assert.equal(h.elements.get("worker-slot-usage").hidden,false);
  assert.equal(h.elements.get("worker-slot-split").hidden,false);
  assert.equal(h.elements.get("slot-video").value,2);
  assert.equal(h.elements.get("slot-podcast").value,1);
  const rows=slotRows(h);
  assert.equal(rows.length,3);
  assert.match(rows[0].children[0].textContent,/Video slot 1 · movie\.mkv/);
  assert.match(rows[1].children[0].textContent,/Video slot 2 · Available/);
  assert.match(rows[2].children[0].textContent,/Podcast slot 1 · episode\.mp3/);
  assert.match(rows[2].children[2].textContent,/Podcast processing/);
  // The node's row says what is running and how the pools are used.
  const summary=h.elements.get("worker-slots").children[0].children[0].children[0];
  assert.match(summary.children.map(c=>c.textContent).join(" "),/Worker 2\/3 Video 1\/2 · Podcast 1\/1/);
  // An edit that has not been saved survives the next poll.
  h.elements.get("slot-video").value=1;
  h.run('state.slotFormDirty=true;renderWorkerActivity(activity)');
  assert.equal(h.elements.get("slot-video").value,1);
  h.run('state.slotFormDirty=false;renderWorkerActivity(activity)');
  assert.equal(h.elements.get("slot-video").value,2);
  // A worker that predates slots, or one that cannot be split, hides the form.
  h.run('activity.activity.slots=undefined;renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-slot-usage").hidden,true);
  assert.match(slotRows(h)[0].children[0].textContent,/^Slot 1/);
  h.run('activity.activity.slots={ceiling:1,split:false,video:{total:1,used:0},podcast:{total:1,used:0}};renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-slot-split").hidden,true);
});

test("parallel jobs on the node are always shown and editable from one, and a lower value says the split resets", async () => {
  const h=harness();
  h.context.calls=[];
  h.run('api=async(path,body)=>{calls.push([path,body]);return {applied:true,warnings:[]}};refreshWorkerActivity=async()=>{};');
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:1,worker_slots_used:1,jobs:[],
    slots:{ceiling:1,ceiling_limit:10,ceiling_source:"config",host_cpus:10,split:false,video:{total:1,used:1},podcast:{total:1,used:0}}}};
  h.run('renderWorkerActivity(activity)');
  // One job: the editor is visible in Settings, not tucked away; only the split needs two.
  assert.equal(h.elements.get("settings-node").hidden,false);
  assert.equal(h.elements.get("worker-parallel-summary").hidden,true);
  assert.equal(h.elements.get("worker-parallel-form").hidden,false);
  assert.equal(h.elements.get("parallel-jobs").value,1);
  assert.equal(h.elements.get("parallel-jobs").max,10);
  assert.match(h.elements.get("worker-parallel-limit").textContent,/^1–10 · From the worker config$/);
  assert.equal(h.elements.get("worker-slot-split").hidden,true);
  await h.run('saveParallelJobs(2)');
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.calls[0])),["worker-slots",{video_slots:0,podcast_slots:0,ceiling:2}]);
  assert.match(h.elements.get("worker-parallel-note").textContent,/Applied\. Takes effect as the next job starts/);
  // Raised and split: the split editor appears and a lower value warns before saving.
  h.run('activity.activity.worker_slots_total=3;activity.activity.slots={ceiling:3,ceiling_limit:10,ceiling_source:"override",host_cpus:10,split:true,video:{total:2,used:1},podcast:{total:1,used:0}};renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-slot-split").hidden,false);
  assert.match(h.elements.get("worker-parallel-limit").textContent,/^1–10 · Set here; overrides the worker config$/);
  assert.equal(h.elements.get("parallel-jobs").value,3);
  h.elements.get("parallel-jobs").value=2;
  h.elements.get("parallel-jobs").listeners.get("input")();
  assert.match(h.elements.get("worker-parallel-note").textContent,/split \(2 \+ 1\) does not fit 2 parallel jobs, so all slots will be shared again/);
  // An unsaved edit survives the next poll.
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("parallel-jobs").value,2);
  h.elements.get("parallel-jobs").value=3;
  h.elements.get("parallel-jobs").listeners.get("input")();
  assert.equal(h.elements.get("worker-parallel-note").textContent,"");
  await h.run('saveParallelJobs(4)');
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.calls[1][1])),{video_slots:2,podcast_slots:1,ceiling:4});
  // A worker that predates the setting shows the value read-only and says why.
  h.run('activity.activity.slots={ceiling:2,split:false,video:{total:2,used:0},podcast:{total:2,used:0}};renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-parallel-summary").textContent,"2");
  assert.equal(h.elements.get("worker-parallel-summary").hidden,false);
  assert.equal(h.elements.get("worker-parallel-form").hidden,true);
  assert.match(h.elements.get("worker-parallel-limit").textContent,/predates setting this here\. Update it/);
  // One that predates slots altogether still shows its total.
  h.run('activity.activity.slots=undefined;activity.activity.worker_slots_total=1;renderWorkerActivity(activity)');
  assert.equal(h.elements.get("settings-node").hidden,false);
  assert.equal(h.elements.get("worker-parallel-summary").textContent,"1");
  assert.equal(h.elements.get("worker-parallel-form").hidden,true);
  // Disconnected: nothing to show.
  h.run('renderWorkerActivity({available:false})');
  assert.equal(h.elements.get("settings-node").hidden,true);
});

test("the x265 limit is offered only by a worker that reports it, keeps unsaved picks and is saved without touching the split", async () => {
  const h=harness();
  h.context.calls=[];
  h.context.stored={video_slots:0,podcast_slots:0};
  h.run('api=async(path,body)=>{if(!body)return {settings:stored};calls.push([path,body]);return {applied:true,warnings:[]}};refreshWorkerActivity=async()=>{};');
  h.elements.get("x265-limit").options=[{value:"0"},{value:"1"}];
  const slots=x265=>({ceiling:2,ceiling_limit:10,ceiling_source:"override",host_cpus:10,split:false,video:{total:2,used:1},podcast:{total:2,used:0},...(x265?{x265}:{})});
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:2,worker_slots_used:1,jobs:[],slots:slots({total:0,used:0})}};
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-x265-row").hidden,false);
  assert.equal(h.elements.get("x265-limit").value,"0");
  assert.match(h.elements.get("worker-x265-limit").textContent,/^No limit/);
  assert.equal(h.elements.get("worker-slot-usage").hidden,true);
  // A limit is shown with its use, in the usage line too.
  h.context.activity.activity.slots=slots({total:1,used:1});
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("x265-limit").value,"1");
  assert.match(h.elements.get("worker-x265-limit").textContent,/At most 1 x265 encode at once · 1 running now/);
  assert.equal(h.elements.get("worker-slot-usage").hidden,false);
  assert.equal(h.elements.get("worker-slot-usage").textContent,"x265 1/1 · Ceiling 2");
  // An unsaved pick survives the next poll.
  h.elements.get("x265-limit").value="0";
  h.elements.get("x265-limit").listeners.get("change")();
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("x265-limit").value,"0");
  // Saving sends the limit with the stored split, and no ceiling.
  await h.run('saveX265Limit(1)');
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.calls[0])),["worker-slots",{video_slots:0,podcast_slots:0,x265_slots:1}]);
  assert.match(h.elements.get("worker-x265-note").textContent,/Applied\. Takes effect as the next job starts/);
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("x265-limit").value,"1");
  // The split sent back is the stored one, even while the worker still runs another.
  h.context.activity.activity.slots={...slots({total:1,used:0}),split:true,video:{total:1,used:0},podcast:{total:1,used:0}};
  h.context.stored={video_slots:2,podcast_slots:1};
  h.run('renderWorkerActivity(activity)');
  await h.run('saveX265Limit(0)');
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.calls[1])),["worker-slots",{video_slots:2,podcast_slots:1,x265_slots:0}]);
  // The parallel jobs form leaves the limit alone: it sends no x265 field.
  h.run('renderWorkerActivity(activity)');
  await h.run('saveParallelJobs(3)');
  assert.equal("x265_slots" in h.context.calls[2][1],false);
  // A worker that predates the limit never shows it; one parallel job hides it until a limit is set.
  h.context.activity.activity.slots=slots(null);
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-x265-row").hidden,true);
  h.context.activity.activity.slots={...slots({total:0,used:0}),ceiling:1};
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-x265-row").hidden,true);
  h.context.activity.activity.slots={...slots({total:1,used:0}),ceiling:1};
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-x265-row").hidden,false);
});

test("the slot table names the encoder of a full conversion", () => {
  const h=harness();
  h.elements.get("x265-limit").options=[{value:"0"},{value:"1"}];
  const job=(id,file,encoding)=>({id,file,kind:"transcode",phase:"encoding",heartbeat_at:new Date().toISOString(),progress:10,encoding});
  assert.equal(h.run('slotKind({kind:"transcode",encoding:"x265 CRF 22 (step 1 of 3)"})'),"Full conversion · x265");
  assert.equal(h.run('slotKind({kind:"transcode",encoding:"VideoToolbox at quality 65 (step 1 of 4)"})'),"Full conversion · VideoToolbox");
  assert.equal(h.run('slotKind({kind:"transcode"})'),"Full conversion");
  assert.equal(h.run('slotKind({kind:"podcast",encoding:"x265"})'),"Podcast processing");
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:2,worker_slots_used:2,jobs:[job("a","one.mkv","x265 CRF 22"),job("b","two.mkv","VideoToolbox quality 65")],
    slots:{ceiling:2,split:false,video:{total:2,used:2},podcast:{total:2,used:0},x265:{total:1,used:1}}}};
  h.run('renderWorkerActivity(activity)');
  const rows=slotRows(h);
  assert.match(rows[0].children[2].textContent,/Full conversion · x265/);
  assert.match(rows[1].children[2].textContent,/Full conversion · VideoToolbox/);
});

test("a slot's file opens that job's details when the job is known, and is plain text when it is not", () => {
  const h=harness();
  h.elements.get("settings-node-pick").options=[];
  const job=(id,file,extra)=>({id,file,kind:"transcode",phase:"encoding",heartbeat_at:new Date().toISOString(),progress:10,...extra});
  h.context.opened=[];
  h.run('openJob = id => opened.push(id);');
  h.context.activity={available:true,workers:[{name:"m1",available:true,activity:{details_available:true,worker_slots_total:2,worker_slots_used:2,jobs:[job("job-a","one.mkv",{action_id:"a"}),job("job-b","two.mkv")],
    slots:{ceiling:2,split:false,video:{total:2,used:2},podcast:{total:2,used:0}}}}]};
  h.run('renderWorkerActivity(activity)');
  const rows=h.elements.get("worker-slots").children[1].children[0].children[1].children;
  const link=rows[0].children[1].children[0];
  assert.equal(link.className,"slot-file-link");
  assert.equal(link.textContent,"one.mkv");
  link.listeners.get("click")();
  assert.deepEqual(h.context.opened,["a"]);
  assert.equal(rows[1].children[1].children.length,0);
  assert.equal(rows[1].children[1].textContent,"two.mkv");
});

test("worker activity gives each node a row that opens onto its slots, and a table of what runs for wide screens", () => {
  const h=harness();
  h.elements.get("settings-node-pick").options=[];
  const job=(id,file)=>({id,file,kind:"transcode",phase:"encoding",heartbeat_at:new Date().toISOString(),progress:10});
  const node=(name,jobs)=>({name,available:true,activity:{details_available:true,worker_slots_total:1,worker_slots_used:jobs.length,jobs,
    slots:{ceiling:1,split:false,video:{total:1,used:jobs.length},podcast:{total:0,used:0}}}});
  h.context.activity={available:true,workers:[node("m1",[job("a","one.mkv")]),node("m4",[])]};
  h.run('renderWorkerActivity(activity)');
  const [compact,table]=h.elements.get("worker-slots").children;
  assert.equal(compact.className,"slots-compact");
  assert.equal(table.className,"slots-table");
  const [busy,idle]=compact.children;
  const summaryText=d=>d.children[0].children.map(c=>c.textContent).join(" ");
  // A node with something running opens by itself; an idle one stays shut, in one line.
  assert.equal(busy.open,true);
  assert.equal(idle.open,false);
  assert.match(summaryText(busy),/m1 1\/1/);
  assert.match(summaryText(idle),/m4 0\/1 · all available/);
  assert.match(busy.children[1].children[0].children[0].textContent,/one\.mkv/);
  assert.equal(busy.dataset.state,"busy");
  assert.equal(idle.dataset.state,"idle");
  // The table lists what runs, names the node, and counts the free slots in a line.
  const rows=table.children[0].children[1].children;
  assert.equal(rows.length,1);
  assert.equal(rows[0].children[0].textContent,"m1 · Slot 1");
  assert.equal(rows[0].children[1].textContent,"one.mkv");
  // The phase and the bar have a cell each, so every bar lines up whatever the phase is called.
  assert.deepEqual(table.children[0].children[0].children[0].children.map(c=>c.textContent),["Slot","File","Phase","Progress","Kind","Elapsed"]);
  assert.equal(rows[0].children[2].children[0].textContent,"Encoding");
  assert.equal(rows[0].children[2].children[1].textContent,"Waiting for the first update");
  assert.equal(rows[0].children[3].children[1].textContent,"10.0%");
  assert.equal(rows[0].children[4].textContent,"Full conversion");
  assert.equal(table.children[1].textContent,"1 slot available · m4 1");
  // What a person opened or shut stays that way on the next refresh.
  idle.children[0].listeners.get("click")();
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-slots").children[0].children[1].open,true);
  busy.children[0].listeners.get("click")();
  h.run('renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-slots").children[0].children[0].open,false);
  // A node that is down keeps its row, open, and says why.
  h.run('activity.workers[1]={name:"m4",available:false,message:"Worker disconnected"};renderWorkerActivity(activity)');
  const down=h.elements.get("worker-slots").children[0].children[1];
  assert.equal(down.dataset.state,"down");
  assert.equal(down.open,true);
  assert.match(summaryText(down),/m4 Worker disconnected/);
  assert.equal(down.children[1].children.length,0);
  assert.equal(h.elements.get("worker-slots").children[1].children.at(-1).textContent,"m4 · Worker disconnected");
  // A single node that predates names still gets a row.
  h.run('activity={available:true,workers:[activity.workers[0]]};renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-slots").children[0].children.length,1);
});

test("the scheduler is a chip beside the slots and the settings link is an icon", () => {
  assert.match(markup,/<div class="activity-heading">.*id="activity-scheduler" class="sched-chip"/s);
  assert.match(markup,/id="worker-open-settings"[^>]*aria-label="Change in Settings"/);
  assert.match(css,/@media \(min-width: 901px\) \{\s*\.slots-compact \{ display:none; \}\s*\.slots-table \{ display:block; \}/);
  assert.match(css,/\.sched-chip\[data-state="degraded"\]/);
  const h=harness();
  h.run('state.workerInfo={ready:true,nodes:[{name:"A",scheduler_health:"degraded",stale:false,observed_at:new Date().toISOString()}]};renderWorkers();');
  assert.equal(h.elements.get("activity-scheduler").dataset.state,"degraded");
  h.run('state.workerInfo.nodes[0].scheduler_health="ok";renderWorkers();');
  assert.equal(h.elements.get("activity-scheduler").dataset.state,"ok");
  h.run('state.workerInfo.nodes[0].stale=true;renderWorkers();');
  assert.equal(h.elements.get("activity-scheduler").dataset.state,"unknown");
});

test("activity stays read-only and links to the node settings only when something there is editable", () => {
  const h=harness();
  h.context.selected=[];
  h.run('selectTab=tab=>selected.push(tab)');
  h.context.activity={available:true,activity:{details_available:true,worker_slots_total:2,worker_slots_used:0,jobs:[],
    slots:{ceiling:2,ceiling_limit:10,ceiling_source:"config",host_cpus:10,split:false,video:{total:1,used:0},podcast:{total:1,used:0}}}};
  h.run('renderWorkerActivity(activity)');
  // Activity keeps the status; the editors are in Settings and the link leads there.
  assert.match(h.elements.get("worker-capacity").textContent,/0\/2 occupied/);
  assert.equal(slotRows(h).length,2);
  assert.equal(h.elements.get("worker-open-settings").hidden,false);
  assert.equal(h.elements.get("settings-node").hidden,false);
  assert.equal(h.elements.get("worker-slot-split").hidden,false);
  let focused=0;
  h.document.getElementById("settings-node-title").focus=()=>{focused++};
  h.elements.get("worker-open-settings").listeners.get("click")();
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.selected)),["advanced"]);
  assert.equal(focused,1,"focus moves to the Transcode node heading");
  assert.match(markup,/id="settings-node-title"[^>]*tabindex="-1"/);
  assert.match(markup,/id="parallel-jobs"[^>]*aria-describedby="worker-parallel-limit worker-parallel-note"/);
  // Below two parallel jobs there is no split to edit, but the ceiling still is.
  h.run('activity.activity.worker_slots_total=1;activity.activity.slots={ceiling:1,ceiling_limit:10,ceiling_source:"config",split:false,video:{total:1,used:0},podcast:{total:1,used:0}};renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-slot-split").hidden,true);
  assert.equal(h.elements.get("worker-open-settings").hidden,false);
  // An old worker reports no ceiling limit and one job: read-only, nothing to link to.
  h.run('activity.activity.slots={ceiling:1,split:false,video:{total:1,used:0},podcast:{total:1,used:0}};renderWorkerActivity(activity)');
  assert.equal(h.elements.get("worker-parallel-form").hidden,true);
  assert.equal(h.elements.get("worker-parallel-summary").hidden,false);
  assert.match(h.elements.get("worker-parallel-limit").textContent,/predates setting this here/);
  assert.equal(h.elements.get("worker-slot-split").hidden,true);
  assert.equal(h.elements.get("worker-open-settings").hidden,true);
  // Disconnected: no editor and no link.
  h.run('renderWorkerActivity({available:false})');
  assert.equal(h.elements.get("worker-open-settings").hidden,true);
});

test("opening Settings refreshes the worker report that fills the node block", () => {
  const h=harness();
  h.context.refreshed=0;
  h.run('refreshWorkerActivity=async()=>{refreshed++};state.activityCheckedAt=0;selectTab("advanced")');
  assert.equal(h.context.refreshed,1);
  h.run('state.activityCheckedAt=Date.now();selectTab("advanced")');
  assert.equal(h.context.refreshed,1);
});

test("the parallel jobs editor and the slot split live in Settings, not Activity", () => {
  const section=(id)=>{const start=markup.indexOf(`<section id="${id}"`);return markup.slice(start,markup.indexOf("</section>",start))};
  const activity=section("worker-activity"), settings=section("settings-node");
  for (const id of ["parallel-jobs","worker-parallel-form","worker-slot-form","worker-slot-split"]) {
    assert.ok(!activity.includes(`id="${id}"`),`${id} still in Activity`);
    assert.ok(settings.includes(`id="${id}"`),`${id} missing from the node settings`);
  }
  assert.ok(activity.includes('id="worker-open-settings"'));
  assert.ok(markup.indexOf('id="settings-node"') > markup.indexOf('<section id="advanced"'));
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
    'recipeFromControls = () => ({video:{codec:"libx265",quality:23}}); state.recipeRevision = 1; tool = async () => saved; loadRecipes = async () => {}; readRecipe = async () => { throw new Error("overwrote new selection"); };',
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

test("the first screen does not wait for the profiles", async () => {
  const boot = (search, loadRecipes) => {
    const h = harness();
    h.document.querySelector = () => new Element();
    h.run(
      `api = async (path) => path === "auth-info" ? {auth_mode:"cloudflare_access"} : {roots:[],services:[],tools:["recipe_list"],transcode_enabled:true}; resetLibrary = () => {}; controls = () => {}; loadJobs = async () => {}; window = {location:{search:${JSON.stringify(search)}}}; loadRecipes = ${loadRecipes}; applyNavigation = async () => { globalThis.__order.push("navigation"); };`,
    );
    h.run("globalThis.__order = []; var __release; var __profiles = new Promise(r => { __release = r; });");
    return h;
  };
  // A profile load that has not answered yet leaves the queue free to draw.
  let h = boot("?view=queue", 'async () => { globalThis.__order.push("recipes start"); await __profiles; globalThis.__order.push("recipes done"); }');
  await h.run("initialize()");
  assert.deepEqual(JSON.parse(h.run("JSON.stringify(globalThis.__order)")), ["recipes start", "navigation"]);
  h.run("__release()");
  await h.run("__profiles");
  // A link straight into the Profiles tab needs them first.
  h = boot("?view=recipes", 'async () => { globalThis.__order.push("recipes start"); globalThis.__order.push("recipes done"); }');
  await h.run("initialize()");
  assert.deepEqual(JSON.parse(h.run("JSON.stringify(globalThis.__order)")), ["recipes start", "recipes done", "navigation"]);
  // A failed profile load is told to the person and does not stop the console opening.
  h = boot("?view=queue", 'async () => { throw new Error("profiles unavailable"); }');
  await h.run("initialize()");
  await h.run("Promise.resolve()");
  assert.equal(h.elements.get("workspace").hidden, false);
  assert.match(h.elements.get("notice").textContent, /profiles unavailable/);
});

test("token mode reached through Access opens without the token form or a local sign-out", async () => {
  const h = harness();
  h.document.querySelector = () => new Element();
  h.run(
    'api = async (path) => path === "auth-info" ? {auth_mode:"token",session:"cloudflare_access"} : {roots:[],services:[],tools:[],transcode_enabled:true}; resetLibrary = () => {}; controls = () => {}; loadJobs = async () => {};',
  );
  await h.run("initialize()");
  assert.equal(h.run("state.authMode"), "token");
  assert.equal(h.elements.get("workspace").hidden, false);
  assert.equal(h.elements.get("login").hidden, true);
  assert.equal(h.elements.get("logout").hidden, true);
  assert.equal(h.elements.get("local-session").hidden, true);
});

test("expired Access login in token mode closes the workspace and asks for a reload, not the token", async () => {
  for (const response of [
    { type: "opaqueredirect", status: 0 },
    { status: 403, headers: { get: () => "text/html; charset=utf-8" } },
  ]) {
    const h = harness();
    h.run('state.authMode = "token"; state.accessSession = true; controls = () => {};');
    h.document.getElementById("workspace").hidden = false;
    h.document.getElementById("batch-review").showModal();
    h.context.fetch = async () => ({
      ...response,
      json: () => assert.fail("Access HTML parsed as API JSON"),
    });
    await assert.rejects(h.run('api("bootstrap")'), /Cloudflare Access/);
    assert.equal(h.elements.get("workspace").hidden, true);
    assert.equal(h.elements.get("batch-review").open, false);
    assert.equal(h.elements.get("access-expired").hidden, false);
    assert.equal(h.elements.get("login").hidden, true);
    assert.equal(h.run("state.authRevision"), 1);
  }
});

test("token mode without an Access session still treats redirects as unexpected", async () => {
  const h = harness();
  h.run('state.authMode = "token"; state.accessSession = false; controls = () => {};');
  h.context.fetch = async () => ({ type: "opaqueredirect", status: 0 });
  await assert.rejects(h.run('api("bootstrap")'), /Unexpected API redirect/);
  assert.equal(h.run("state.authMode"), "token");
});

test("token mode with a local session keeps its sign-out", async () => {
  const h = harness();
  h.document.querySelector = () => new Element();
  h.run(
    'api = async (path) => path === "auth-info" ? {auth_mode:"token"} : {roots:[],services:[],tools:[],transcode_enabled:true}; resetLibrary = () => {}; controls = () => {}; loadJobs = async () => {};',
  );
  await h.run("initialize()");
  assert.equal(h.elements.get("logout").hidden, false);
  assert.equal(h.elements.get("local-session").hidden, false);
});

test("folder breadcrumbs preserve selected files while clearing the focused file/search", async () => {
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
  assert.equal(h.run("state.folderSelected.size"), 1);
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
 h.run('state.folder="/media";state.libraryRevision=1;state.libraryLoaded=1;state.tab="library";state.folderSizeTargets=new Map([["/media/child",[one,two]]]);$("service").value="folder:/media";api=async()=>({items:[{path:"/media/child",folder_size:{status:"ready",bytes:200}}]});one.dataset.pending="1";two.dataset.pending="1";');
 await h.run('updateFolderSizes(1,"/media","",1)');
 assert.equal(h.context.one.textContent,"200 B");assert.equal(h.context.two.textContent,"200 B");
 let calls=0;h.context.record=()=>calls++;
 // Nothing is still measuring and the sizes are fresh: coming back asks for nothing.
 h.run('one.dataset.pending="0";two.dataset.pending="0";state.folderSizesArgs={revision:1,path:"/media",query:"",loaded:1};state.folderSizesCheckedAt=Date.now();updateFolderSizes=async()=>record();selectTab("more");selectTab("library");');
 assert.equal(calls,0);
 // Something still measuring resumes at once.
 h.run('one.dataset.pending="1";selectTab("more");selectTab("library");');
 assert.equal(calls,1);
 h.run('showFolderSize(one,{status:"calculating",measured_at:"0001-01-01T00:00:00Z"})');
 assert.doesNotMatch(h.context.one.title,/Measured/);
 // A size that has not started says it waits; once it runs it says it measures.
 h.run('showFolderSize(one,{status:"calculating",queued:true,updating:true})');
 assert.equal(h.context.one.textContent,"En cola…");assert.equal(h.context.one.dataset.pending,"1");
 h.run('showFolderSize(one,{status:"calculating",updating:true})');
 assert.equal(h.context.one.textContent,"Calculating…");
});

test("file marks say what a batch did with a file and roll a folder up to what needs attention", () => {
 const h=harness();
 const mark=status=>h.run(`fileMark(${JSON.stringify(status)})`);
 assert.equal(mark({state:"none"}),null);
 assert.equal(mark(null),null);
 const replaced=mark({state:"replaced"});
 assert.equal(replaced.className,"file-mark replaced");
 assert.equal(replaced.title,"Replaced with the converted file");
 assert.equal(replaced.children[0].textContent,"✓");
 // Failing to convert is the quietest mark and says the file itself is fine.
 const failed=mark({state:"failed"});
 assert.match(failed.title,/did not finish/);assert.match(failed.title,/untouched/);
 // A folder shows its loudest state, a count, and everything it holds in the tooltip.
 const folder=mark({state:"working",count:5,counts:{working:2,queued:3,converted:1}});
 assert.equal(folder.className,"file-mark working");
 assert.equal(folder.children.at(-1).textContent,"5");
 assert.equal(folder.title,"2 converting · 3 queued · 1 converted");
 assert.equal(folder.getAttribute?.("aria-label") ?? folder.title,"2 converting · 3 queued · 1 converted");
 assert.equal(mark({state:"attention",count:1,counts:{attention:1}}).children.length,1,"a single file is never numbered");
});

test("file marks label library titles, seasons, episodes and folder entries", async () => {
 const h=harness();h.context.controls=()=>{};
 const rows=()=>h.elements.get("library-items").children;
 const marksOf=row=>row.children.flatMap(function find(c){return [c,...(c.children||[]).flatMap(find)]}).filter(c=>String(c.className).startsWith("file-mark "));
 h.run('$("service").value="sonarr";api=async()=>({total:2,has_more:false,seasons:[],items:[{id:1,title:"Road Notes",path:"/tv/Road Notes",nav_status:{state:"working",count:4,counts:{working:1,queued:3}}},{id:2,title:"Untouched",path:"/tv/Untouched"}]});');
 await h.run("loadLibrary()");
 assert.deepEqual(rows().map(row=>marksOf(row).map(m=>m.className)),[["file-mark working"],[]]);
 assert.ok(h.run("state.fileMarks.has('title:1')")&&h.run("state.fileMarks.has('title:2')"));
 h.run('state.media={id:7,title:"Road Notes"};api=async()=>({total:2,has_more:true,seasons:[],items:[],season_groups:[{seasonNumber:1,fileCount:10,size:200,nav_status:{state:"converted",count:10,counts:{converted:10}}},{seasonNumber:0,fileCount:2,size:50}]});');
 await h.run("loadLibrary()");
 assert.deepEqual(rows().map(row=>marksOf(row).map(m=>m.className)),[["file-mark converted"],[]]);
 assert.ok(h.run("state.fileMarks.has('season:1')")&&h.run("state.fileMarks.has('season:0')"));
 h.run('api=async()=>({total:1,has_more:false,seasons:[],items:[{id:9,path:"/tv/Season 1/one.mkv",nav_status:{state:"replaced"}}]});');
 await h.run("loadLibrary()");
 assert.deepEqual(marksOf(rows()[0]).map(m=>m.className),["file-mark replaced"]);
 assert.ok(h.run("state.fileMarks.has('file:9')"));
 h.run('state.media=null;$("service").value="folder:/media";state.folder="/media";api=async()=>({path:"/media",total:2,has_more:false,listing:"l1",items:[{name:"Shows",path:"/media/Shows",is_dir:true,nav_status:{state:"queued",count:3,counts:{queued:3}}},{name:"a.mkv",path:"/media/a.mkv",size:5,nav_status:{state:"failed"}},{name:"b.mkv",path:"/media/b.mkv",size:5}]});');
 await h.run("loadLibrary()");
 assert.deepEqual(rows().map(row=>marksOf(row).map(m=>m.className)),[["file-mark queued"],["file-mark failed"],[]]);
 assert.deepEqual([...h.run("[...state.fileMarks.keys()]")].sort(),["folder:/media/Shows","folder:/media/a.mkv","folder:/media/b.mkv"]);
});

test("a poll changes only the marks that moved, and never touches a list that is being replaced", async () => {
 const h=harness();h.context.controls=()=>{};
 // The basic stub has no parent links, so removing a mark is modelled here.
 class Mark extends Element{remove(){const host=this.host;if(host)host.children=host.children.filter(c=>c!==this)}}
 h.document.createElement=()=>new Mark();
 const originalAppend=Element.prototype.append;
 Element.prototype.append=function(...children){for(const c of children)if(c instanceof Mark)c.host=this;return originalAppend.apply(this,children)};
 try{
  h.run('state.tab="library";state.libraryLoaded=2;state.libraryRevision=4;$("workspace").hidden=false;$("service").value="radarr";state.media=null;');
  const names=[new Element(),new Element()];h.context.markHosts=names;
  h.run('state.fileMarks=new Map();markFile("title:1",markHosts[0],{state:"queued"});markFile("title:2",markHosts[1],null);');
  assert.equal(names[0].children.length,1);assert.equal(names[1].children.length,0);
  const kept=names[0].children[0];
  h.run('api=async()=>({total:2,items:[{id:1,nav_status:{state:"queued"}},{id:2,nav_status:{state:"converted"}}]});');
  await h.run("refreshFileMarks()");
  assert.equal(names[0].children[0],kept,"an unchanged mark is not rebuilt");
  assert.equal(names[1].children[0].className,"file-mark converted");
  h.run('api=async()=>({total:2,items:[{id:1,nav_status:{state:"replaced"}},{id:2}]});');
  await h.run("refreshFileMarks()");
  assert.deepEqual(names.map(n=>n.children.map(c=>c.className)),[["file-mark replaced"],[]]);
  // A reply that arrives after the list was reloaded must not paint the old rows.
  const gate=deferred();h.context.gate=gate.promise;
  h.run('api=async()=>{await gate;return {total:1,items:[{id:1,nav_status:{state:"failed"}}]}};');
  const late=h.run("refreshFileMarks()");
  h.run("state.libraryRevision++;state.fileMarks=new Map();");
  gate.resolve();await late;
  assert.deepEqual(names[0].children.map(c=>c.className),["file-mark replaced"]);
  // Nothing is requested while the list is loading or another tab is open.
  let calls=0;h.context.count=()=>calls++;
  h.run('state.fileMarks=new Map([["title:1",{host:markHosts[0],mark:null,signature:""}]]);api=async()=>{count();return {items:[]}};state.libraryLoading=true;');
  await h.run("refreshFileMarks()");
  h.run('state.libraryLoading=false;state.tab="more";');
  await h.run("refreshFileMarks()");
  assert.equal(calls,0);
 }finally{Element.prototype.append=originalAppend}
});

test("file marks refresh when returning to Files and ride the page poll only while it is open", () => {
 assert.match(source,/else if \(tab === "library"\) void refreshFileMarks\(\);/);
 assert.match(source,/state\.tab === "library" && Date\.now\(\) - \(state\.fileMarksCheckedAt \|\| 0\) > 10000 \? refreshFileMarks\(\)/);
});

test("file marks are small, quiet and never animate under reduced motion", () => {
 const rule=selector=>{const at=css.indexOf(`${selector} {`);assert.ok(at>=0,`${selector} rule`);return css.slice(at,css.indexOf("}",at))};
 const base=rule(".file-mark");
 assert.match(base,/height: 14px/);assert.match(base,/min-width: 14px/);
 assert.match(base,/border-radius: 7px/,"a one-glyph mark is a circle, a counted one a pill");
 assert.match(base,/font: 500 9px/);
 // The failed conversion is the faintest; "kept" is only a little stronger.
 assert.match(rule(".file-mark.failed"),/opacity: \.5/);
 assert.match(rule(".file-mark.kept"),/opacity: \.7/);
 assert.match(css,/\.file-mark\.working::before \{[^}]*animation: pending-spin/);
 assert.match(css,/prefers-reduced-motion: reduce\) \{ \.file-mark\.working::before \{ animation: none; \} \}/);
 assert.match(css,/@keyframes pending-spin/);
 assert.match(css,/\.folder-title \.file-mark \{ margin-left: 0; \}/);
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

test("batch result badges use the library outcome when the coordinator has finished", () => {
  const h=harness();
  for(const [outcome,status,color] of [["failed","Failed","failed"],["cancelled","Cancelled","neutral"],["partial","Partial","waiting_decision"],["needs_decision","Needs decision","waiting_decision"]]) {
    h.context.fixture={action_name:"transcode_batch",status:"completed",batch:{outcome,total:8,failed:8}};
    const label=h.run('queuePresentation(fixture).status');
    assert.equal(label,status);
    h.context.label=label;
    assert.equal(h.run('statusClass(label)'),color);
  }
  assert.equal(h.run('queuePresentation({status:"completed",batch:{outcome:"preview",dry_run:true,total:8,queued:8}}).status'),"Preview");
  assert.equal(h.run('queuePresentation({status:"completed",batch:{outcome:"candidates_ready",total:8,completed:8}}).status'),"Candidates ready");
});

test("batch progress counts resolved files independently of individual worker percentages", () => {
  const h = harness();
  const progress = h.run('telemetry({batch:{total:23,completed:20,running:2,queued:1},activities:[{},{}],worker:{progress:99,progress_is_stale:true}})');
  assert.equal(progress.children[0].textContent, "20 / 23 files finished");
  assert.equal(progress.children[1].textContent, "87%");
  assert.equal(progress.children[2].value, 20);
  assert.equal(progress.children[2].max, 23);
  const mixed = h.run('batchProgress({batch:{total:10,completed:3,failed:2,skip:1,cancelled:1,waiting_decision:2,running:1}})');
  assert.equal(mixed.children[0].textContent, "6 / 10 files finished");
  assert.equal(mixed.children[1].textContent, "67%");
  assert.match(mixed.children[0].title, /Cancelled files.*not counted/);
  // A cancelled file is not work left to do: one file done, one cancelled is complete, not half done.
  const cancelledOne = h.run('batchProgress({status:"waiting_external",batch:{total:2,completed:1,cancelled:1}})');
  assert.equal(cancelledOne.children[1].textContent, "~99%");
  assert.equal(cancelledOne.children[2].max, 1);
  assert.equal(h.run('batchProgress({batch:{dry_run:true,total:10,queued:10}})'), null);
  assert.equal(h.run('batchProgress({batch:{total:0}})'), null);
  assert.equal(h.run('batchProgress({batch:{completed:2}})'), null);
  assert.equal(h.run('batchProgress({status:"completed",batch:{total:2,completed:2}})'), null);
});

test("a completed preview offers review and start, while active work retains cancellation", () => {
  const h = harness();
  h.run('state.workerInfo={ready:true};');
  const controls = h.run('jobControls({id:"p",status:"completed",batch:{dry_run:true,queued:8}})');
  assert.deepEqual(controls.children.map(c=>c.textContent), ["Review preview", "Start batch"]);
  assert.equal(controls.children[1].disabled, false);
  assert.match(h.run('queueOutcome({status:"completed",batch:{dry_run:true,queued:8}})'), /Nothing starts automatically/);
  assert.equal(h.run('queuePresentation({status:"completed",batch:{dry_run:true,outcome:"preview"}}).status'), "Preview");
  assert.deepEqual(h.run('jobControls({id:"p",status:"completed",batch:{dry_run:true,queued:0}})').children.map(c=>c.textContent), ["Review preview"]);
  assert.deepEqual(h.run('jobControls({id:"p",status:"completed",batch:{dry_run:true,queued:8},preview_execution_action_id:"encode"})').children.map(c=>c.textContent), ["View batch"]);
  // A file's job opened on its own leads back to its batch in the details, not in the list.
  h.context.opened=[];
  h.run('openJob = id => opened.push(id);');
  const child='{id:"c",action_name:"transcode_media",status:"waiting_external",parent_batch_id:"b"}';
  assert.ok(h.run(`jobControls(${child}, true)`).children.some(c=>c.textContent==="View batch"));
  h.run(`jobControls(${child}, true)`).children.find(c=>c.textContent==="View batch").listeners.get("click")();
  assert.deepEqual(h.context.opened,["b"]);
  // Not twice when the job already offers that batch as its preview.
  assert.ok(!h.run('jobControls({id:"c",action_name:"transcode_media",status:"waiting_external",parent_batch_id:"b",preview_action_id:"b"}, true)').children.some(c=>c.textContent==="View batch"));
  // Without a found batch there is nothing to go back to.
  assert.ok(!h.run('jobControls({id:"c",action_name:"transcode_media",status:"waiting_external",parent_action_id:"gone"}, true)').children.some(c=>c.textContent==="View batch"));
  // It is one of the two buttons a running file's details show, beside Cancel.
  const bar=h.run(`detailActionBar(${child})`).map(c=>c.textContent.trim());
  assert.ok(bar.includes("View batch"), bar.join("|"));
  assert.ok(!h.run(`jobControls(${child}, false)`).children.some(c=>c.textContent==="View batch"));
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
  // A row is its toggle (name, state) and a body that stays hidden until opened.
  const stateOf = row => row.children[0].children[1].children[0].children[1].textContent;
  assert.equal(stateOf(rows[0]), "Eligible");
  assert.equal(rows[0].children[1].children.length, 0);
  assert.equal(stateOf(rows[1]), "Skipped");
  assert.equal(rows[1].children[1].children.length, 1);
  assert.equal(rows[1].children[1].hidden, true);
  assert.equal(h.run('statusClass("completed")'), "completed");
  assert.equal(h.run('statusClass("failed")'), "failed");
  assert.equal(h.run('statusClass("waiting_decision")'), "waiting_decision");
});

test("finished failed batches show an outcome and offer reconfiguration instead of success progress or blind retry",()=>{
  const h=harness();
  const job='{id:"b",action_name:"transcode_batch",status:"completed",batch:{total:8,failed:8,outcome:"failed"}}';
  assert.equal(h.run(`batchProgress(${job})`),null);
  assert.match(h.run(`queueOutcome(${job})`),/No candidates created/);
  h.run('state.workerInfo={ready:false}');
  const controls=h.run(`jobControls(${job})`);
  assert.equal(controls.children[0].textContent,'Change settings');
  assert.equal(controls.children[0].disabled,false);
  assert.ok(!h.run(`jobControls({...${job},status:"failed"})`).children.some(b=>b.textContent==='Retry'));
  assert.equal(h.run('batchReasonLabel({batch:{failed:8}},{count:1})'),'Example (1 of 8 failed files)');
  assert.equal(h.run('batchReasonLabel({batch:{failed:8}},{count:8})'),'8 files');
});

test("reconfiguration loads saved settings without sending work and ignores a dismissed or expired form",async()=>{
  const h=harness(),writes=[];
  h.context.plan={id:'b',title:'Example',selected:2,kept:1,files:['One','Three'],settings:{profile_label:'Original custom profile',min_savings_percent:15,max_size_increase_percent:0,promote_candidates:true}};
  h.context.record=(...args)=>writes.push(args);
  h.run('api=async(path,body)=>body?(record(path,body),{}):plan;state.recipes=["general-hevc"]');
  await h.run('reconfigureBatch("b")');
  assert.equal(writes.length,0);
  assert.equal(h.elements.get('reconfigure-profile').value,'same');
  assert.equal(h.elements.get('reconfigure-savings').value,15);
  assert.equal(h.elements.get('reconfigure-growth').value,0);
  assert.equal(h.elements.get('reconfigure-files').children.length,2);
  const pending=deferred();h.context.pending=pending.promise;
  h.run('api=async()=>pending');
  const next=h.run('reconfigureBatch("b")');
  h.run('$("reconfigure-batch").close()');pending.resolve(h.context.plan);await next;
  assert.equal(h.run('state.reconfigurePlan'),null);
});

test("reconfiguration keeps the same batch and a stable receipt across lost responses without an extra worker probe",async()=>{
  const h=harness(),writes=[],saved=new Map();
  h.context.sessionStorage={getItem:k=>saved.get(k)||null,setItem:(k,v)=>saved.set(k,v),removeItem:k=>saved.delete(k)};
  h.context.record=(...args)=>writes.push(args);
  h.run('state.reconfigurePlan={id:"b",scope:"all",selection_version:"v"};state.reconfigureRevision=1;$("reconfigure-batch").open=true;$("reconfigure-profile").value="same";$("reconfigure-priority").value="balanced";$("reconfigure-savings").value="15";$("reconfigure-growth").value="0";submissionID=()=>"attempt-key";setConnection=()=>{};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};refreshWorkers=async()=>{throw new Error("extra worker probe")};api=async(path,body)=>{record(path,body);throw new Error("Lost reply")};');
  await assert.rejects(h.run('submitReconfiguredBatch()'),/Lost reply/);
  await assert.rejects(h.run('submitReconfiguredBatch()'),/Lost reply/);
  assert.equal(writes.length,2);assert.equal(writes[0][1].key,writes[1][1].key);
  assert.equal(writes[0][0],'batch-settings'); assert.equal(writes[0][1].scope,'all');
  assert.equal(writes[0][1].id,'b'); assert.equal(writes[0][1].min_savings_percent,15);
  assert.deepEqual(Object.keys(writes[0][1]).sort(),['id','key','max_size_increase_percent','min_savings_percent','preserve_growth','preserve_limits','preserve_savings','profile','scope','selection_version'].sort());
  const pending=deferred();h.context.pending=pending.promise;
  h.run('api=async()=>pending');
  const next=h.run('submitReconfiguredBatch()');
  await h.run('submitReconfiguredBatch()');
  h.run('invalidateAuthentication()');pending.resolve({id:"b"});await next;
  assert.equal(h.run('state.reconfigurePlan'),null);
});

test("overall batch progress includes conversion and validation while retaining true finished counts", () => {
 const h=harness();
 const stages=["preflight","submit_benchmark","wait_benchmark","submit_transcode","wait_transcode","validate_result","accept_result"].map(name=>({name}));
 h.context.stages=stages;
 const progress=h.run('batchProgress({status:"waiting_external",batch:{total:2,running:2,completed:0},activities:[{id:"one",file:"one.mkv",current_step:4,stages,worker:{transcode_phase:"validating_quality"}},{id:"two",file:"two.mkv",current_step:5,stages}]})');
 assert.equal(progress.children[0].textContent,"0 / 2 files finished");
 assert.equal(progress.children[1].textContent,"~64%");
 assert.match(progress.children[3].textContent,/Measuring final quality.*Verify/);
 const finish=h.run('batchProgress({status:"waiting_external",batch:{total:2,completed:2}})');
 assert.equal(finish.children[1].textContent,"~99%");assert.match(finish.children[3].textContent,/Finishing/);
});

test("pending cleanup polls measured bytes without interpreting an old failed checkpoint as its result", async () => {
 const h=harness();
 h.run('state.backupsVisible=true;state.backupCleaning={id:"copy",auth:state.authRevision,name:"episode.mkv",started:Date.now()-1000,pendingResponse:true};api=async()=>({jobs:[{id:"copy",status:"failed",cleanup:{phase:"verifying_recovery",bytes_read:500,total_bytes:1000,updated_at:new Date().toISOString()}}]});');
 await h.run('refreshBackupCleanup()');
 assert.ok(h.run('state.backupCleaning'));
 const parts=h.elements.get("backup-progress").children;
 assert.match(parts[0].textContent,/recovery SHA-256/);assert.equal(parts[2].value,500);assert.match(parts[3].textContent,/50%/);
 h.run('api=async()=>({jobs:[{id:"copy",status:"failed",cleanup:{phase:"completed",updated_at:"2000-01-01T00:00:00Z"}}]});');
 await h.run('refreshBackupCleanup()');assert.equal(h.run('state.backupCleaning.progress.phase'),"verifying_recovery");
});

test("duplicate cleanup reports removal separately from a failed conversion and sends only the duplicate mode", async () => {
 const h=harness();let args;
 h.context.write=(_name,value)=>{args=value;return {status:"failed",error:"Old conversion error",cleanup:{phase:"completed"}};};
 h.run('reviewAction=async()=>true;tool=async(...args)=>write(...args);loadBackups=async()=>{};loadJobs=async()=>{};');
 await h.run('cleanBackup({action_id:"copy",original_path:"/media/a.mkv",duplicate_available:true,bytes:1000})');
 assert.equal(args.mode,"discard_duplicate");assert.match(h.elements.get("backup-progress").textContent,/Original preserved; failed job unchanged/);
});

test("replacement checks retain their share of batch progress",()=>{
 const h=harness();
 const replacing=h.run('batchProgress({status:"waiting_external",replacement_requested:true,batch:{total:2,completed:2,promotion:{eligible:2,promoted:1}}})');assert.equal(replacing.children[1].textContent,"~93%");
});

test("a lost duplicate-cleanup reply retains the lock while byte observations show ongoing verification",async()=>{
 const h=harness();h.run('state.backupCleaning={id:"copy",auth:state.authRevision,name:"a.mkv",started:Date.now()-1000};api=async()=>({jobs:[{id:"copy",status:"failed",cleanup:{phase:"verifying_replacement",bytes_read:50,total_bytes:100,updated_at:new Date().toISOString()}}]});');
 await h.run('refreshBackupCleanup()');assert.ok(h.run('state.backupCleaning'));assert.equal(h.run('state.backupCleaning.progress.bytes_read'),50);
});

test("candidate review offers rejection, later and settings for exactly the waiting file", async()=>{
 for(const chosen of ['reject','reconfigure','later']) {
  const h=harness(),writes=[];
  h.context.parent={id:'batch',action_name:'transcode_batch',status:'waiting_decision',waiting_reason:'Candidate too large',waiting_options:[{decision:'reject'},{decision:'accept_loss'}],savings:{source_bytes:9000000000,candidate_bytes:12000000000}};
  h.context.child={id:'child',action_name:'transcode_media',status:'waiting_decision',source_path:'/media/episode.mkv',waiting_reason:'Candidate file size (2000000000 bytes) exceeds original (1000000000 bytes) by 100.0%, which is greater than max_size_increase_percent (0.0%)'};
  h.context.plan={id:'batch',candidate_id:'child',decision_version:'reviewed',title:'Episode 1',selected:1,kept:7,files:['Episode 1'],settings:{profile_label:'Original profile',min_savings_percent:15}};
  h.context.record=(...args)=>writes.push(args);
  h.run('api=async(path,body)=>body?(record(path,body),{}):path.includes("batch-reconfigure") || path.includes("batch-settings") ? {...plan,scope:"candidate"}:{jobs:[path.includes("id=child")?child:parent]};commandRequest=async(path,body)=>api(path,body);loadJobs=async()=>{};notify=()=>{};');
  const review=h.run('reviewJobDecision("batch","accept_loss")');
  for(let i=0;i<12;i++) await Promise.resolve();
  const content=h.elements.get('action-review-content').children;
  assert.equal(content[0].textContent,'Episode 1');
  assert.equal(content[1].children[1].textContent,'1.00 GB');
  assert.equal(content[2].children[1].textContent,'2.00 GB');
  assert.equal(h.elements.get('dismiss-action-review').textContent,'Decide later');
  const choices=content.at(-1).children;
  assert.deepEqual(choices.map(b=>b.textContent),['Reject · keep original','Transcode with other settings']);
  if(chosen==='later') h.elements.get('dismiss-action-review').listeners.get('click')();
  else await choices[chosen==='reject'?0:1].listeners.get('click')();
  await review;
  if(chosen==='reject') {
   assert.equal(writes.length,1);
   assert.equal(writes[0][0],'candidate-decision');
   assert.equal(writes[0][1].candidate_id,'child');assert.equal(writes[0][1].decision,'reject');
  } else assert.equal(writes.length,0);
  if(chosen==='reconfigure') {
   assert.equal(h.elements.get('reconfigure-batch').open,true);
   assert.equal(h.elements.get('submit-reconfigure').textContent,'Create new candidate');
   assert.equal(h.run('state.reconfigurePlan.candidate_id'),'child');
  }
 }
});

test("explicit copy removal offers one confirmation and never resumes or verifies old jobs",async()=>{
 const h=harness(),writes=[];
 h.context.copy={action_id:'old',delete_available:true,original_path:'/media/episode.mkv',path:'/media/recovery/original.bak',bytes:1000};
 h.context.record=(...args)=>writes.push(args);
 h.run('tool=async(...args)=>{record(...args);return {status:"failed",cleanup:{phase:"completed",updated_at:new Date().toISOString()}}};loadJobs=async()=>{};loadBackups=async()=>{};');
 const removal=h.run('cleanBackup(copy,"discard")');
 for(let i=0;i<5;i++) await Promise.resolve();
 assert.equal(writes.length,0);
 assert.equal(h.elements.get('confirm-action-review').textContent,'Remove copy');
 assert.match(h.elements.get('action-review-content').children[2].textContent,/cannot be undone/);
 h.elements.get('confirm-action-review').listeners.get('click')();await removal;
 assert.equal(writes.length,1);assert.equal(writes[0][1].mode,'discard');
 assert.match(h.elements.get('backup-progress').textContent,/by your choice/);
 assert.doesNotMatch(h.elements.get('backup-progress').textContent,/Replacement verified/);
});

test("moving a cancelled promotion's original back asks once, then reports it",async()=>{
 const h=harness(),writes=[];
 h.context.copy={action_id:'old',restore_available:true,original_path:'/media/episode.mkv',path:'/media/recovery/original.bak',bytes:1000};
 assert.match(h.run('backupReason(copy)'),/Move it back/);
 h.context.record=(...args)=>writes.push(args);
 h.run('tool=async(...args)=>{record(...args);return {status:"cancelled",cleanup:{phase:"completed",updated_at:new Date().toISOString()}}};loadJobs=async()=>{};loadBackups=async()=>{};');
 const restoring=h.run('cleanBackup(copy,"restore")');
 for(let i=0;i<5;i++) await Promise.resolve();
 assert.equal(writes.length,0);
 assert.equal(h.elements.get('confirm-action-review').textContent,'Move back');
 assert.match(h.elements.get('action-review-content').children[2].textContent,/nothing is deleted or overwritten/);
 h.elements.get('confirm-action-review').listeners.get('click')();await restoring;
 assert.equal(writes.length,1);assert.equal(writes[0][1].mode,'restore');
 assert.match(h.elements.get('backup-progress').textContent,/back to its library path/);
 assert.doesNotMatch(h.elements.get('backup-progress').textContent,/Removed|Replacement verified/);
});

 test("a durable command exposes pending work, polls its receipt and preserves it on connection loss",async()=>{
  const h=harness(),saved=new Map(),calls=[];
  h.context.sessionStorage={getItem:k=>saved.get(k)||null,setItem:(k,v)=>saved.set(k,v),removeItem:k=>saved.delete(k)};
  h.context.record=(...args)=>calls.push(args);
  h.run('submissionID=()=>"receipt";notify=()=>{};api=async(path,body)=>{record(path,body);return body ? {command_id:"cmd",id:"batch"} : {status:"completed",id:"batch"};}');
  const result=await h.run('commandRequest("tool",{name:"action_cancel",arguments:{id:"batch"},background:true})');
  assert.equal(result.id,'batch'); assert.equal(calls[0][1].key,'receipt'); assert.equal(calls[1][0],'commands?id=cmd');
  assert.equal(saved.get('navigatorr_commands'),'{}');
  h.run('api=async(path,body)=>{record(path,body);if(body)return {command_id:"lost",id:"batch"};throw new Error("connection lost")};');
  await assert.rejects(h.run('commandRequest("tool",{name:"action_cancel",arguments:{id:"batch"},background:true})'),/connection lost/);
  assert.match(saved.get('navigatorr_commands'),/lost/);
  const before=calls.filter(([,body])=>body).length;
  h.run('api=async(path,body)=>{record(path,body);return {status:"completed",id:"batch"}}');
  await h.run('commandRequest("tool",{name:"action_cancel",arguments:{id:"batch"},background:true})');
  assert.equal(calls.filter(([,body])=>body).length,before,'lost response must not resubmit the mutation');
 });

test("cleanup completion uses the confirmed command receipt when browser and server clocks differ", async()=>{
 const h=harness();
 h.run('state.backupsVisible=true;state.backupCleaning={id:"copy",auth:state.authRevision,name:"episode.mkv",mode:"discard",started:Date.now(),commandCompleted:true};loadBackups=async()=>{};loadJobs=async()=>{};');
 await h.run('finishBackupCleanup({status:"failed",cleanup:{phase:"completed",updated_at:"2000-01-01T00:00:00Z"}})');
 assert.equal(h.run('state.backupCleaning'),null);
 assert.match(h.elements.get('backup-progress').textContent,/Removed recovery data/);
});

test("an omitted video shows its reason and another-profile action instead of conversion progress", async () => {
  const h=harness();
  h.context.job={id:'omitted',number:580,action_name:'transcode_batch',status:'completed',can_archive:true,batch:{title:'The Peripheral',total:1,skip:1,outcome:'no_changes'},batch_files:{context:'The Peripheral · Season 1',file_count:1,files:[{display_label:'The Peripheral - S01E04'}],reasons:[{count:1,reason:'shared VMAF/CAMBI calibration supports 8-bit SDR video below 45 fps; original preserved'}]}};
  assert.equal(h.run('queuePresentation(job).status'),'Not converted');
  assert.equal(h.run('batchProgress(job)'),null);
  assert.equal(h.run('jobControls(job).children[0].textContent'),'Try another profile');
  h.run('api=async()=>({jobs:[job],total:1,has_more:false});renderSavings=()=>{};');
  await h.run('loadJobs()');
  const row=h.elements.get('jobs-list').children[0];
  assert.equal(row.children[0].children[1].textContent,'The Peripheral - S01E04');
  assert.deepEqual(row.children[1].children.map(c=>c.className.split(' ')[0]).filter(c=>c!=='job-sentence'),['metadata'],'no repeated filename or reason');
  assert.match(allText(row.children[1].children.find(c=>c.className.startsWith('job-sentence'))).trim(),/^Try another profile/);
  const outcome=row.children[2].children[0];
  assert.match(outcome.children[0].textContent,/Automatic testing does not support/);
  assert.match(outcome.children[1].textContent,/Choose another profile/);
});

test("a goal batch skipped for an unsupported source explains it and does not offer a new goal", async () => {
  const h=harness();
  h.context.job={id:'cover',number:581,action_name:'transcode_batch',status:'completed',mode:'size',can_archive:true,batch:{title:'Show',total:2,skip:2,outcome:'no_changes'},batch_files:{context:'Show · Season 1',file_count:2,files:[{display_label:'Show - S01E01'},{display_label:'Show - S01E02'}],reasons:[{count:2,reason:'unsupported_source'}]}};
  const labels=h.run('jobControls(job).children.map(b=>b.textContent)');
  assert.ok(!labels.includes("Change goal"));
  const outcome=h.run('skippedBatchOutcome(job)');
  assert.match(outcome.children[0].textContent,/Automatic testing does not support this source format yet/);
  assert.match(outcome.children[1].textContent,/changing the goal will not help/);
  assert.doesNotMatch(outcome.children[1].textContent,/None of the tested settings/);
  h.context.job.batch_files.reasons=[{count:1,reason:'unsupported_source'},{count:1,reason:'quality_not_met'}];
  assert.ok(h.run('jobControls(job).children.map(b=>b.textContent)').includes("Change goal"),"other skip reasons still offer a new goal");
  assert.match(h.run('skippedBatchOutcome(job)').children[1].textContent,/You can change the goal/);
  const child=h.run('jobControls({id:"child",action_name:"transcode_media",status:"completed",reason_code:"unsupported_source",mode:"size",parent_action_id:"batch",parent_item_key:"one"}).children.map(b=>b.textContent)');
  assert.ok(!child.includes("Change goal"));
  // A goal batch's reconfigure dialog only switches goals, so nothing that
  // picks an explicit profile is lost; a profile batch keeps that path.
  h.context.job={...h.context.job,mode:undefined,batch_files:{...h.context.job.batch_files,reasons:[{count:2,reason:'unsupported_source'}]}};
  assert.ok(h.run('jobControls(job).children.map(b=>b.textContent)').includes("Try another profile"));
});

test("HFR sources skipped for a missing worker capability say so and offer no goal change", async () => {
  const h=harness();
  h.context.job={id:'hfr',number:582,action_name:'transcode_batch',status:'completed',mode:'size',can_archive:true,batch:{title:'Clips',total:2,skip:2,outcome:'no_changes'},batch_files:{context:'Clips',file_count:2,files:[{display_label:'Clip 1'},{display_label:'Clip 2'}],reasons:[{count:2,reason:'hfr_quality_unavailable'}]}};
  assert.ok(!h.run('jobControls(job).children.map(b=>b.textContent)').includes("Change goal"));
  const outcome=h.run('skippedBatchOutcome(job)');
  assert.match(outcome.children[0].textContent,/cannot measure quality at this frame rate yet/);
  assert.match(outcome.children[1].textContent,/frame rate yet, so changing the goal will not help/);
  h.context.job.batch_files.reasons=[{count:1,reason:'hfr_quality_unavailable'},{count:1,reason:'unsupported_source'}];
  assert.ok(!h.run('jobControls(job).children.map(b=>b.textContent)').includes("Change goal"));
  assert.match(h.run('skippedBatchOutcome(job)').children[1].textContent,/does not support this source format yet/);
  const child=h.run('jobControls({id:"child",action_name:"transcode_media",status:"completed",reason_code:"hfr_quality_unavailable",mode:"size",parent_action_id:"batch",parent_item_key:"one"}).children.map(b=>b.textContent)');
  assert.ok(!child.includes("Change goal"));
  h.context.items=[{item_key:"hfr",display_label:"Clip",status:"skip",can_reconfigure:true,mode:"size",reasons:["hfr_quality_unavailable"]}];
  h.run('state.info={};state.detail="batch";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:1});');
  await h.run('loadBatchItems("batch",2)');
  const row=h.elements.get("batch-items-list").children[0];
  const all = el=>[el,...el.children.flatMap(all)];
  const text=all(row).map(el=>el.textContent).filter(Boolean).join(" ");
  assert.match(text,/cannot measure quality at this frame rate yet/);
  assert.doesNotMatch(text,/Change goal/);
});

test("a compact file skipped for unreadable frame evidence can be tried again", async () => {
  const h=harness();
  assert.match(h.run('shortJobReason("compact_frame_evidence_unavailable")'),/Could not read this video's frames.*try again/);
  const child=h.run('jobControls({id:"child",action_name:"transcode_media",status:"completed",reason_code:"compact_frame_evidence_unavailable",mode:"compact",parent_action_id:"batch",parent_item_key:"one"}).children.map(b=>b.textContent)');
  assert.ok(child.includes("Change goal"));
  h.context.items=[{item_key:"dv",display_label:"Clip",status:"skip",can_reconfigure:true,mode:"compact",reasons:["compact_frame_evidence_unavailable"]}];
  h.run('state.info={};state.detail="batch";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:1});');
  await h.run('loadBatchItems("batch",2)');
  const row=h.elements.get("batch-items-list").children[0];
  const all = el=>[el,...el.children.flatMap(all)];
  const text=all(row).map(el=>el.textContent).filter(Boolean).join(" ");
  assert.match(text,/Could not read this video's frames/);
  assert.match(text,/Change goal/);
});

test("an unsupported-source file row offers no goal change", async () => {
  const h=harness();
  h.context.items=[
    {item_key:"cover",display_label:"Cover",status:"skip",can_reconfigure:true,mode:"size",reasons:["unsupported_source"]},
    {item_key:"other",display_label:"Other",status:"skip",can_reconfigure:true,mode:"size",reasons:["quality_not_met"]}
  ];
  h.run('state.info={};state.detail="batch";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:2});');
  await h.run('loadBatchItems("batch",2)');
  const rows=h.elements.get("batch-items-list").children;
  const all = row=>[row,...row.children.flatMap(all)];
  const text = row=>all(row).map(el=>el.textContent).filter(Boolean).join(" ");
  assert.match(text(rows[0]),/does not support this source format yet/);
  assert.doesNotMatch(text(rows[0]),/Change goal/);
  assert.match(text(rows[1]),/Change goal/);
});

test("an HDR file skipped by a size goal can change to the compact goal", async () => {
  const h=harness();
  h.context.items=[
    {item_key:"hdr",display_label:"HDR",status:"skip",can_reconfigure:true,mode:"size",reasons:["hdr_requires_compact"]},
    {item_key:"small",display_label:"Small",status:"skip",can_reconfigure:true,mode:"compact",max_height:720,reasons:["compact_not_needed"]}
  ];
  h.run('state.info={};state.detail="batch";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:2});');
  await h.run('loadBatchItems("batch",2)');
  const rows=h.elements.get("batch-items-list").children;
  const all = row=>[row,...row.children.flatMap(all)];
  const text = row=>all(row).map(el=>el.textContent).filter(Boolean).join(" ");
  assert.match(text(rows[0]),/only the Shrink to 1080p SDR goal converts it/);
  assert.match(text(rows[0]),/Change goal/);
  assert.match(text(rows[1]),/Goal: Shrink to 720p SDR/);
});

test("compact replacement reviews warn about re-grabs and show read-only tracking", async () => {
  const h = harness();
  h.context.current = {id:"replace",status:"waiting_decision",waiting_options:[{decision:"approve"}],promotion:{original_path:"/media/one.mkv",candidate_path:"/media/.candidates/one.mkv",original_sha256:"original",candidate_sha256:"candidate",original_bytes:1000,candidate_bytes:400,plan_digest:"sha256:plan",regrab_warning:"May download the original again.",arr_monitored:true,arr_quality_profile:"Any"}};
  h.run('api = async () => ({jobs:[current]});');
  await h.run('reviewFilePromotion("replace")');
  const all = el=>[el,...el.children.flatMap(all)];
  const text = el=>all(el).map(e=>e.textContent).filter(Boolean).join(" ");
  const first = h.elements.get("file-review-data").children[0];
  assert.match(text(first), /download the original again/);
  assert.match(text(first), /Monitored · Quality profile: Any/);
  h.context.notice = {regrab_warning:"May download the original again.",arr_monitored:false};
  h.context.plan = {prepared:true,id:"batch-a",digest:"sha256:plan",approved:false,waiting:true,members:[{item_key:"e1",original_path:"/source.mkv",candidate_path:"/candidate.mkv"}]};
  h.run('api = async () => plan; tool = async (name, args) => ({data: args.key === "batch_promotion_notice" ? notice : null});');
  await h.run('reviewBatchReplacement("batch-a")');
  const batch = h.elements.get("batch-review-notice").children[0];
  assert.match(text(batch), /Not monitored/);
  h.context.notice = null;
  await h.run('reviewBatchReplacement("batch-a")');
  assert.doesNotMatch(text(h.elements.get("batch-review-notice")), /download the original again/);
});

test("an all-skipped retry selects the saved files and requires another profile without sending work",async()=>{
  const h=harness(), reads=[], writes=[];
  h.context.reads=reads;h.context.writes=writes;
  h.context.plan={id:'b',title:'The Peripheral',scope:'all',selected:1,files:['S01E04'],requires_explicit_profile:true,settings:{profile_label:"Keep each file's current profile",max_size_increase_percent:0}};
  h.run('state.recipes=["general-hevc"];api=async(path,body)=>{if(body){writes.push(body);return {}}reads.push(path);return path.includes("scope=unfinished")?{...plan,scope:"unfinished",selected:0,files:[]}:plan};');
  await h.run('reconfigureBatch("b")');
  assert.equal(reads.length,2);
  assert.equal(writes.length,0);
  assert.equal(h.elements.get('reconfigure-scope').value,'all');
  assert.equal(h.elements.get('reconfigure-profile').value,'');
  assert.equal(h.elements.get('submit-reconfigure').disabled,true);
  assert.match(h.elements.get('reconfigure-profile-note').textContent,/Choose a profile/);
  await assert.rejects(h.run('submitReconfiguredBatch()'),/Choose a profile/);
  assert.equal(writes.length,0);
  h.run('$("reconfigure-profile").value="general-hevc";updateBatchSettingsProfile()');
  assert.equal(h.elements.get('submit-reconfigure').disabled,false);
  h.run('$("reconfigure-profile").value="same";updateBatchSettingsProfile()');
  assert.equal(h.elements.get('submit-reconfigure').disabled,true);
});

test("declining a candidate is distinct from an error and cannot blindly resume that rejected candidate",()=>{
 const h=harness();
 h.context.job={id:'file',parent_action_id:'season',action_name:'transcode_media',status:'failed',error:'transcode candidate rejected by user decision; original file remains untouched'};
 assert.equal(h.run('queuePresentation(job).status'),'Rejected');
 assert.match(h.run('queueOutcome(job)'),/Candidate rejected/);
 assert.equal(h.run('jobControls(job).children[0].textContent'),'Change settings');
 assert.equal(h.run('jobControls(job).children.some(button=>button.textContent==="Retry")'),false);
 const original=h.run('({total:8,failed:4,queued:2,running:1,waiting_decision:1,outcome:"needs_decision"})');
 h.context.batch=original;
 h.context.feedback={reasons:[{count:3,reason:'transcode candidate rejected by user decision; original file remains untouched'},{count:1,reason:'Worker connection failed'}]};
 const displayed=h.run('visibleBatch({status:"waiting_decision",batch,batch_files:feedback})');
 assert.equal(displayed.failed,1);assert.equal(displayed.rejected,3);assert.equal(original.failed,4);
 assert.match(h.run('queuePresentation({status:"waiting_decision",batch,batch_files:feedback}).summary'),/3 rejected/);
 assert.match(h.run('queuePresentation({status:"waiting_decision",batch,batch_files:feedback}).summary'),/1 failed/);
 assert.equal(h.run('queuePresentation({status:"completed",batch:{total:3,failed:3,outcome:"failed"},batch_files:{reasons:[feedback.reasons[0]]}}).status'),'Rejected');
});

test("rejected, cancelled and superseded jobs say what happened to the candidate file",()=>{
 const h=harness();
 h.context.job={id:'file',action_name:'transcode_media',status:'failed',error:'transcode candidate rejected by user decision; original file remains untouched; candidate file removed',candidate_cleanup:{reason:'rejected',outcome:'removed',bytes_freed:2500000000}};
 assert.equal(h.run('queueOutcome(job)'),'Candidate rejected · Original preserved · Candidate file removed (2.50 GB)');
 assert.equal(h.run('queuePresentation(job).status'),'Rejected');
 assert.equal(h.run('shortJobReason(job.error)'),'Candidate rejected by you; original kept; candidate file removed');
 h.context.kept={id:'file',action_name:'transcode_media',status:'cancelled',waiting_reason:'stopped; candidate file kept: replacement review references this candidate',candidate_cleanup:{reason:'cancelled',outcome:'kept',detail:'replacement review references this candidate'}};
 assert.equal(h.run('queueOutcome(kept)'),'Stopped by user · Candidate file kept: replacement review references this candidate');
 h.context.superseded={id:'file',action_name:'transcode_media',status:'completed',candidate_cleanup:{reason:'superseded',outcome:'removed',bytes_freed:0}};
 assert.equal(h.run('queueOutcome(superseded)'),'Replaced by new batch settings · Original preserved · No candidate file was left');
 assert.equal(h.run('queueOutcome({status:"cancelled"})'),'Stopped by user');
});

test("completed batch details retain the server's actual stage outcomes",()=>{
 const h=harness();
 h.run('renderStages({status:"completed",batch:{outcome:"failed"},stages:[{name:"resolve_and_inspect",status:"completed"},{name:"schedule_batch",status:"failed",note:"0 converted · 3 failed"},{name:"promote_batch",status:"skip",note:"No eligible candidates"}]})');
 assert.equal(h.elements.get('detail-stages').children.length,3);
 assert.equal(h.elements.get('detail-stages').children[1].children[3].textContent,"0 converted · 3 failed");
 assert.equal(h.run('batchProgress({status:"failed",batch:{total:3,failed:3}})'),null);
 assert.equal(h.run('queueOutcome({status:"completed",batch:{total:3,completed:3,outcome:"candidates_ready"}})'),'3 candidates ready · Originals unchanged');
});

test("decisions and pauses show the actual stop reason instead of fabricated worker progress",()=>{
 const h=harness();
 const decision={status:"waiting_decision",batch:{total:8,waiting_decision:1,queued:7},waiting_reason:"Candidate file size (200 bytes) exceeds original (100 bytes) by 100.0%, which is greater than max_size_increase_percent (0.0%)"};
 h.context.decision=decision;
 assert.equal(h.run('batchProgress(decision)'),null);
 assert.match(h.run('queueOutcome(decision)'),/100.0% larger.*0.0%.*Originals unchanged/);
 assert.match(h.run('queueOutcome({status:"waiting_decision",paused:true})'),/^Paused/);
 assert.match(h.run('queueOutcome({status:"waiting_decision",batch:{promotion_plan_ready:true}})'),/Waiting for replacement approval/);
 const active=h.run('batchProgress({status:"waiting_external",batch:{total:3,running:1,waiting_decision:1}})');
 assert.match(active.children[3].textContent,/1 file needs your decision/);
});

test("job details show the decision reason where the user must act",async()=>{
 const h=harness();
 h.run('api=async()=>({jobs:[{id:"decision",action_name:"transcode_batch",status:"waiting_decision",batch:{total:1,waiting_decision:1},waiting_reason:"Candidate file size (200 bytes) exceeds original (100 bytes) by 100.0%, which is greater than max_size_increase_percent (0.0%)"}]});loadBatchItems=async()=>{};');
 await h.run('openJob("decision")');
 assert.ok(h.elements.get('detail-summary').children.some(element=>/100.0% larger/.test(element.textContent)));
});

test("rejected candidates remain rejected in the workflow stages instead of turning back into failures",()=>{
 const h=harness();
 h.run('renderStages({status:"completed",batch:{total:1,failed:1},batch_files:{rejected_count:1},stages:[{name:"schedule_batch",status:"failed",note:"0 converted · 1 failed"}]})');
 const row=h.elements.get('detail-stages').children[0];
 assert.match(row.className,/rejected/);
 assert.equal(row.children[3].textContent,'0 candidates · 1 rejected');
 assert.equal(h.run('queuePresentation({status:"completed",batch:{total:1,failed:1},batch_files:{rejected_count:1},savings:{source_bytes:1000,estimated_saved_bytes:400}}).summary').includes('estimated savings'),false);
});

test("queue controls open candidate review instead of rejecting an unseen batch file",()=>{
 const h=harness();
 const controls=h.run('jobControls({id:"b",action_name:"transcode_batch",status:"waiting_decision",batch:{total:8},waiting_options:[{decision:"reject"},{decision:"accept_loss"}]})');
 assert.ok(controls.children.some(button=>button.textContent === "Review candidate"));
 assert.ok(!controls.children.some(button=>button.textContent === "Keep originals"));
});

test("profile details describe the encoder and ignore an obsolete selection",async()=>{
 const h=harness(),pending=deferred();
 h.run('$("reconfigure-batch").open=true;state.reconfigureRevision=1;$("reconfigure-profile").value="general-hevc";');
 h.context.pending=pending.promise;
 h.run('tool=async()=>pending');
 const request=h.run('showReconfigureProfile()');
 h.run('$("reconfigure-profile").value="hevc-vt"');
 pending.resolve({profile:{video:{codec:"libx265",quality:23,preset:"medium"},audio:{mode:"copy"},container:"mkv"}});
 await request;
 assert.doesNotMatch(h.elements.get('reconfigure-profile-info').textContent,/Software HEVC/);
 h.run('tool=async()=>({profile:{video:{codec:"hevc_videotoolbox",average_bitrate_kbps:4000},audio:{mode:"copy"},container:"mkv"}})');
 await h.run('showReconfigureProfile()');
 assert.match(JSON.stringify(h.elements.get('reconfigure-profile-info').children),/Hardware HEVC.*4000 kbps.*Keep original audio tracks.*MKV/);
 h.run('state.recipeDetails={};tool=async()=>({profile:{video:{codec:"libx265",quality:24,preset:"slow"},optimization:{enabled:true}}})');
 await h.run('showReconfigureProfile()');
 assert.match(JSON.stringify(h.elements.get('reconfigure-profile-info').children),/Not available.*samples.*removed/);
});

test("changing only the profile preserves current limits, including mixed per-file settings",async()=>{
 const h=harness(),requests=[];
 h.context.plan={id:'b',scope:'all',title:'Season',selected:2,kept:0,files:['One','Two'],selection_version:'version',settings:{profile_label:'Current profiles',mixed_limits:true,max_size_increase_percent:null,min_savings_percent:null}};
 h.context.requests=requests;
 h.run('api=async()=>plan;state.recipes=["general-hevc"];loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};setConnection=()=>{};submissionID=()=>"key";commandRequest=async(path,body)=>{requests.push(body);return {id:"b"}};sessionStorage={getItem:()=>null,setItem:()=>{},removeItem:()=>{}};');
 await h.run('reconfigureBatch("b")');
 h.run('$("reconfigure-profile").value="general-hevc"');
 await h.run('submitReconfiguredBatch()');
 assert.equal(requests[0].preserve_limits,true);
 await h.run('reconfigureBatch("b")');
 h.run('$("reconfigure-savings").value="12"');
 await h.run('submitReconfiguredBatch()');
 assert.equal(requests[1].preserve_limits,false);
 assert.equal(requests[1].preserve_savings,false);
 assert.equal(requests[1].preserve_growth,true);
 assert.equal(requests[1].min_savings_percent,12);
});


test("profile choices describe effective hardware and custom overrides while preserving IDs", async () => {
  const h = harness();
  h.run('renderRecipeList=()=>{};tool=async()=>({active_bundle_profiles:["anime-hevc-quality","live-action-hevc"],managed_profiles:[{name:"anime-hevc-quality"}],managed_returned:1,managed_total:1,selection_details:{"anime-hevc-quality":{source:"managed",description:"My CPU preset",profile:{video:{codec:"libx265",quality:25}}},"live-action-hevc":{source:"active_bundle",profile:{video:{codec:"libx265",quality:24},optimization:{enabled:true}}}}})');
  await h.run('loadRecipes(true)');
  const options = h.elements.get('profile').children;
  assert.equal(options[1].value, 'anime-hevc-quality');
  assert.equal(options[1].textContent, 'Anime · CRF 25 · CPU · Custom (anime-hevc-quality)');
  // A profile that calibrates with samples is rejected by the server, so it
  // is not offered; its label still says what it is.
  assert.equal(options.length, 2);
  assert.equal(h.run('profileOptionLabel("live-action-hevc")'), 'Live action · CPU · Test samples');
  h.run('state.recipeDetails["anime-hevc-space"]={source:"active_bundle",profile:{video:{codec:"hevc_videotoolbox",quality:55}}}');
  assert.equal(h.run('profileOptionLabel("anime-hevc-space")'), 'Anime · Smaller files · Hardware');
});

test("cached selection details show meaningful settings without another profile request", async () => {
  const h = harness();
  h.run('$("reconfigure-batch").open=true;$("reconfigure-profile").value="live-action-hevc";state.recipeDetails={"live-action-hevc":{source:"active_bundle",profile:{video:{codec:"libx265",quality:24,preset:"slow",profile:"main"},optimization:{enabled:true}}}};tool=async()=>{throw new Error("extra request")};');
  await h.run('showReconfigureProfile()');
  const facts = JSON.stringify(h.elements.get('reconfigure-profile-info').children);
  assert.match(facts, /Software HEVC.*CPU/);
  assert.match(facts, /Lower means higher quality.*slow preset/);
  assert.match(facts, /Not available.*samples.*removed/);
  assert.match(facts, /Profile ID: live-action-hevc/);
});

test("batch file context distinguishes scheduling, paused work and actual worker phases", () => {
  const h = harness();
  // A file waiting its turn says nothing more than its Queued badge.
  assert.equal(h.run('batchFileActivity({status:"queued"},{status:"running"})'), "");
  assert.match(h.run('batchFileActivity({status:"queued"},{paused:true})'), /Resume the batch/);
  assert.match(h.run('batchFileActivity({status:"queued",child_action_id:"child"},{})'), /File task created/);
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},{activities:[{id:"child",worker:{transcode_phase:"validating_quality"}}]})'), 'Measuring final quality');
  assert.match(h.run('batchFileActivity({retry_pending:true},{})'), /Current attempt finishes/);
});

test("creation exposes the same effective profile facts and depth precedence as reconfiguration",()=>{
 const h=harness();
 h.run('$("technical").checked=true;state.info={min_savings_percent:15};$("profile").value="anime-hevc-main10";$("preserve-depth").checked=true;state.recipeDetails={"anime-hevc-main10":{description:"Balanced anime",profile:{video:{codec:"hevc_videotoolbox",quality:65,profile:"main10",pixel_format:"p010le"},audio:{mode:"compact"},container:"mkv"}}};renderCreationProfile();');
 let facts=JSON.stringify(h.elements.get("creation-profile-info").children);
 assert.match(facts,/Same as source.*Overrides profile depth/);assert.match(facts,/Convert lossless tracks to AAC/);assert.match(facts,/Converts full video/);assert.match(facts,/Quality 65/);
 assert.match(h.elements.get("creation-limits-note").textContent,/15%/);
 h.run('$("preserve-depth").checked=false;renderCreationProfile();');
 facts=JSON.stringify(h.elements.get("creation-profile-info").children);assert.match(facts,/10-bit/);assert.doesNotMatch(facts,/Same as source/);
});

test("selection and configuration drafts survive reload and are isolated by source and series",()=>{
 const h=harness(), saved=new Map();h.context.sessionStorage={getItem:key=>saved.get(key),setItem:(key,value)=>saved.set(key,value)};
 h.run('$("service").value="folder:/media";state.draftKey=libraryDraftKey();state.folderSelected=new Set(["/media/Series/Season 1/a.mkv","/media/Series/Season 2/b.mkv"]);$("profile").value="hevc-vt";$("preserve-depth").checked=false;state.recipes=["hevc-vt"];saveLibraryDraft();state.draftKey=null;state.folderSelected=new Set();$("profile").value="auto";restoreLibraryDraft();');
 assert.equal(h.run('state.folderSelected.size'),2);assert.equal(h.elements.get("profile").value,"hevc-vt");assert.equal(h.elements.get("preserve-depth").checked,false);
 h.run('state.draftKey=null;state.folderSelected=new Set();$("service").value="folder:/other";restoreLibraryDraft();');assert.equal(h.run('state.folderSelected.size'),0);
});

test("minimum-savings policy is distinct from technical failures in mixed batches",()=>{
 const h=harness();
 let view=h.run('queuePresentation({status:"completed",action_name:"transcode_batch",batch:{total:8,failed:8,outcome:"failed"},batch_files:{policy_count:8}})');
 assert.equal(view.status,"Minimum savings not met");assert.match(view.summary,/8 below minimum savings/);assert.doesNotMatch(view.summary,/8 failed/);
 const counts=h.run('visibleBatch({batch:{total:5,failed:3,completed:2,outcome:"partial"},batch_files:{policy_count:2}})');assert.equal(counts.failed,1);assert.equal(counts.policy,2);assert.equal(counts.completed,2);
 const item=h.run('savingsPolicyFailure("Estimated output is 33.2% larger; at least 15% savings required. Full conversion not started; original kept.")');assert.equal(item,true);
});

test("a finished batch exposes candidates as its next action before settings or archive",()=>{
 const h=harness();h.run('state.info={allow_destructive:true};');
 const actions=h.run('jobControls({id:"batch",action_name:"transcode_batch",status:"completed",can_archive:true,batch:{completed:23,total:23,outcome:"candidates_ready"}})');
 assert.deepEqual(actions.children.map(c=>c.textContent),["Review candidates","Change settings","Archive"]);
 const preview=h.run('jobControls({id:"preview",action_name:"transcode_batch",status:"completed",batch:{dry_run:true,completed:0,queued:2}})');assert.equal(preview.children.some(c=>c.textContent==="Review candidates"),false);
});

test("restoring a receipt reattaches cleanup and polls the same command without writing again",async()=>{
 const h=harness(), saved=new Map();h.context.sessionStorage={getItem:key=>saved.get(key),setItem:(key,value)=>saved.set(key,value)};
 saved.set('navigatorr_commands',JSON.stringify({'same request':{command_id:'cmd',id:'copy',label:'Removing copy',cleanup:{id:'copy',name:'episode.mkv',mode:'discard',started:Date.now()}}}));
 let writes=0;h.context.request=(path,body)=>{if(body)writes++;return {command_id:'cmd',id:'copy',status:'completed',kind:'discard'}};
 h.run('api=async(path,body)=>request(path,body);loadJobs=async()=>{};refreshBackupCleanup=async()=>{state.cleanupRestored=state.backupCleaning.commandCompleted;};notify=()=>{};restoreCommands();');
 for(let i=0;i<12;i++)await Promise.resolve();
 assert.equal(writes,0);assert.equal(h.run('state.backupCleaning.name'),'episode.mkv');assert.equal(h.run('state.cleanupRestored'),true);assert.equal(saved.get('navigatorr_commands'),'{}');
});

test("remaining candidates can be reviewed after a partial batch replacement",()=>{
 const h=harness();h.run('state.info={allow_destructive:true}');
 const actions=h.run('jobControls({id:"batch",action_name:"transcode_batch",status:"completed",remaining_candidates:1,batch:{total:2,completed:2,promotion:{promoted:1}}})');
 assert.equal(actions.children.some(c=>c.textContent==="Review candidates"),true);
 const done=h.run('jobControls({id:"batch",action_name:"transcode_batch",status:"completed",remaining_candidates:0,batch:{total:2,completed:2,promotion:{promoted:1}}})');
 assert.equal(done.children.some(c=>c.textContent==="Review candidates"),false);
});

test("saved season stays pending until series options are available",()=>{
 const h=harness(),saved=new Map();h.context.sessionStorage={getItem:key=>saved.get(key),setItem:(key,value)=>saved.set(key,value)};
 saved.set('navigatorr_draft:sonarr:10',JSON.stringify({fields:{season:{value:"2",checked:false}}}));
 h.run('$("service").value="sonarr";state.media={id:10};restoreLibraryDraft();');
 assert.equal(h.run('state.draftSeason'),"2");
 // Real select elements clear values when their option does not exist yet.
 h.run('$("season").value="";saveLibraryDraft();');
 assert.equal(JSON.parse(saved.get('navigatorr_draft:sonarr:10')).fields.season.value,"2");
});

test("empty batch counts do not manufacture a savings-policy rejection",()=>{
 const h=harness();
 assert.equal(h.run('visibleBatch({status:"waiting_decision",batch:{total:0,failed:0,outcome:"needs_decision"}}).outcome'),"needs_decision");
});

test("restoring a one-file season draft preserves its batch scope",()=>{
 const h=harness();h.run('controls=()=>{};$("service").value="sonarr";$("scope").value="batch";state.selected=new Set([101]);state.files=new Map([[101,{path:"/media/a.mkv"}]]);state.media={id:10};configureSelection(false,true)');
 assert.equal(h.elements.get('scope').value,'batch');
 assert.equal(h.run('state.file.path'),'/media/a.mkv');
});

test("partial replacement does not claim that all originals were replaced",()=>{
 const h=harness();
 const view=h.run('queuePresentation({status:"completed",action_name:"transcode_batch",replaced_files:1,remaining_candidates:1,savings:{realized_saved_bytes:100},batch:{total:2,completed:2,outcome:"promoted"}})');
 assert.equal(view.status,'Candidates ready');assert.match(view.summary,/1 replaced.*1 candidates available/);
 assert.equal(h.run('queuePresentation({status:"failed",savings:{realized_saved_bytes:100},batch:{total:2,completed:1,failed:1,outcome:"partial"}}).status'),'Failed');
});

test("processing modes omit stale tuning and keep anime library context", async () => {
  for (const mode of ["size","quality","x265_preserve","compact"]) {
    const {h}=submissionHarness();let sent;
    h.context.capture=(_tool,args)=>{sent=JSON.parse(args.inputs);return {id:"submitted"};};
    h.run(`processingMode=()=>${JSON.stringify(mode)};$("custom").checked=true;$("min-savings").value="2";$("max-growth").value="40";$("priority").value="savings";$("preserve-depth").checked=false;state.fileMedia={id:7,seriesType:"anime"};state.fileService="sonarr";tool=async(...args)=>capture(...args);`);
    await h.run('buildAndSubmitJob("encode")');
    assert.deepEqual(sent,{mode,path:"/media/one.mp4",media_type:"tv",is_anime:true,library_context:{service:"sonarr",id:7}});
  }
});

test("explicit mode batches cannot be truncated or expanded by hidden discovery fields", async () => {
  for (const source of ["folder:/media","sonarr"]) {
    const {h}=submissionHarness();let sent;
    h.context.capture=(_tool,args)=>{sent=JSON.parse(args.inputs);return {id:"submitted"};};
    h.run(`$("service").value=${JSON.stringify(source)};$("scope").value="batch";$("selected-only").checked=false;$("max-items").value="1";$("season").value="10";$("recursive").checked=true;$("promote-batch").checked=true;state.folderSelected=new Set(["/media/a.mkv","/media/b.mkv"]);state.selected=new Set([8,9]);state.media={id:4};tool=async(...args)=>capture(...args);api=async()=>{throw new Error("expanded selection")};`);
    await h.run('buildAndSubmitJob("encode")');
    assert.equal(sent.max_items,undefined);assert.equal(sent.season,undefined);assert.equal(sent.promote_candidates,undefined);
    assert.deepEqual(source==="sonarr"?sent.episode_file_ids:sent.paths,source==="sonarr"?[8,9]:["/media/a.mkv","/media/b.mkv"]);
  }
});

test("legacy profile submissions retain compatibility without hidden inactive savings overrides",async()=>{
 const {h}=submissionHarness();let sent;h.context.capture=(_tool,args)=>{sent=JSON.parse(args.inputs);return {id:"submitted"};};
 h.run('$("technical").checked=true;$("custom").checked=false;$("max-growth").value="40";tool=async(...args)=>capture(...args);');
 await h.run('buildAndSubmitJob("encode")');assert.equal(sent.profile,"general-hevc");assert.equal(sent.mode,undefined);assert.equal(sent.min_savings_percent,undefined);assert.equal(sent.max_size_increase_percent,undefined);
});

test("scheduler unknown, degradation and recovery share one observation across visible surfaces",()=>{
 const h=harness();h.run('state.workerInfo={ready:true,nodes:[{name:"Fixture",ready:true,scheduler_health:"degraded",observed_at:new Date().toISOString(),observation_age_seconds:4,last_error:{class:"dependency",message:"Unavailable"}}]};renderWorkers();');
 assert.match(h.elements.get("worker-status").textContent,/Scheduler Degraded/);assert.match(h.elements.get("activity-scheduler").textContent,/Degraded/);
 assert.deepEqual(JSON.stringify(h.elements.get("settings-worker").children),JSON.stringify(h.elements.get("worker-nodes").children));
 h.run('state.workerInfo.nodes[0].stale=true;renderWorkers();');assert.match(h.elements.get("activity-scheduler").textContent,/Unknown/);
 h.run('state.workerInfo.nodes[0].stale=false;state.workerInfo.nodes[0].scheduler_health="ok";state.workerInfo.nodes[0].last_error=null;renderWorkers();');assert.match(h.elements.get("activity-scheduler").textContent,/Normal/);assert.doesNotMatch(JSON.stringify(h.elements.get("settings-worker").children),/Unavailable/);
 h.run('setConnection(false);');assert.match(h.elements.get("activity-scheduler").textContent,/Unknown/);assert.equal(h.run('state.workerInfo.nodes.length'),1);
});

test("failed and conflicting saves preserve dirty editor values and announce the failure",async()=>{
 const h=harness();h.run('renderAdvancedProfile=()=>{};renderRecipeList=()=>{};setRecipeControls({container:"mkv",video:{codec:"libx265",quality:23}});state.recipe={name:"fixture",source:"managed",record:{generation:2,digest:"reviewed"}};$("recipe-name").value="fixture";$("recipe-description").value="Saved";markRecipeClean();$("recipe-description").value="Unsaved text";renderRecipeDirty();tool=async()=>{throw new Error("version conflict")};');
 await assert.rejects(h.run('saveRecipe()'),/version conflict/);assert.equal(h.elements.get("recipe-description").value,"Unsaved text");assert.equal(h.run('recipeIsDirty()'),true);assert.match(h.elements.get("recipe-feedback").textContent,/draft is preserved/);
});

test("discard cancellation keeps a draft and does not read another profile",async()=>{
 const h=harness();h.run('renderAdvancedProfile=()=>{};renderRecipeList=()=>{};setRecipeControls({video:{codec:"libx265",quality:23}});$("recipe-name").value="fixture";markRecipeClean();$("recipe-description").value="Edited";reviewAction=async()=>false;tool=async()=>{throw new Error("discarded draft")};');
 await h.run('readRecipe("another")');assert.equal(h.elements.get("recipe-description").value,"Edited");assert.equal(h.run('recipeIsDirty()'),true);
});

test("restoration submits the exact reviewed bundle digests and handles conflicts",async()=>{
 const h=harness();let sent;h.context.capture=(_tool,args)=>{sent=args;return {error:"recipe version conflict"};};
 h.run('reviewAction=async()=>true;tool=async(name,args)=>name==="recipe_status"?{bundle:{active_version:"2",active_digest:"current",previous_version:"1",previous_digest:"previous"}}:capture(name,args);');
 await h.elements.get("rollback-profiles").listeners.get("click")();assert.deepEqual(JSON.parse(JSON.stringify(sent)),{expected_active_digest:"current",expected_previous_digest:"previous"});
 assert.match(h.elements.get("notice").textContent,/conflict/);
});

test("an old observation expires locally even if the response stale flag was false",()=>{
 const h=harness();h.run('state.workerInfo={ready:true,nodes:[{name:"Fixture",scheduler_health:"ok",stale:false,observed_at:new Date(Date.now()-60000).toISOString()}]};renderWorkers();');assert.match(h.elements.get("worker-status").textContent,/Scheduler Unknown/);assert.match(h.elements.get("activity-scheduler").textContent,/Unknown/);
});

test("simple folder discovery freezes every path and ignores old hidden limits and content",async()=>{
 const {h}=submissionHarness();let sent;h.context.capture=(_tool,args)=>{sent=JSON.parse(args.inputs);return {id:"submitted"};};
 h.run('$("scope").value="batch";state.folder="/media";state.folderSelected.clear();$("max-items").value="1";$("media-kind").value="anime";api=async()=>({paths:["/media/a.mkv","/media/b.mkv","/media/c.mkv"]});tool=async(...args)=>capture(...args);');
 await h.run('buildAndSubmitJob("encode")');assert.deepEqual(sent.paths,["/media/a.mkv","/media/b.mkv","/media/c.mkv"]);assert.equal(sent.max_items,undefined);assert.equal(sent.media_type,undefined);
});

test("history leaving and returning to the same dirty profile preserves its draft without a discard prompt",async()=>{
 const h=harness();h.run('controls=()=>{};loadJobs=async()=>{};loadRecipes=async()=>{};state.info={roots:[],services:[]};renderAdvancedProfile=()=>{};renderRecipeList=()=>{};setRecipeControls({video:{codec:"libx265",quality:23}});state.recipe={name:"fixture"};$("recipe-name").value="fixture";markRecipeClean();$("recipe-description").value="Unsaved";reviewAction=async()=>{throw new Error("misleading discard prompt")};tool=async()=>{throw new Error("overwrote draft")};');
 await h.run('applyNavigation(navigationRoute("?view=queue"))');await h.run('applyNavigation(navigationRoute("?view=profiles&profile=fixture"))');assert.equal(h.elements.get("recipe-description").value,"Unsaved");assert.equal(h.run('recipeIsDirty()'),true);
});

test("editing during save retains the newer draft and advances CAS metadata for the next save",async()=>{
 const h=harness(), pending=deferred();let requests=[];h.context.pending=pending.promise;h.context.capture=args=>{requests.push(JSON.parse(JSON.stringify(args)));return pending.promise;};
 h.run('renderAdvancedProfile=()=>{};renderRecipeList=()=>{};loadRecipes=async()=>{};setRecipeControls({video:{codec:"libx265",quality:23}});state.recipe={name:"fixture",record:{generation:1,digest:"old"}};$("recipe-name").value="fixture";$("recipe-description").value="Submitted";markRecipeClean();tool=async(_name,args)=>capture(args);');
 const saving=h.run('saveRecipe()');h.run('$("recipe-description").value="Newer draft";state.recipeEditRevision=(state.recipeEditRevision||0)+1;');pending.resolve({record:{generation:2,digest:"saved"}});await saving;
 assert.equal(h.elements.get("recipe-description").value,"Newer draft");assert.equal(h.run('recipeIsDirty()'),true);assert.equal(h.run('state.recipe.record.generation'),2);
 h.context.capture=args=>{requests.push(JSON.parse(JSON.stringify(args)));return {record:{generation:3,digest:"next"}};};h.run('readRecipe=async()=>{};');await h.run('saveRecipe()');assert.equal(requests[1].expected_generation,2);assert.equal(requests[1].expected_digest,"saved");assert.equal(requests[1].description,"Newer draft");
});

test("initial worker response can render controls before folder selection initializes",()=>{
 const h=harness();h.run('state.info={transcode_enabled:true};state.workerInfo={ready:true};$("service").value="folder:/media";state.folderSelected=undefined;controls();');assert.equal(h.elements.get("enqueue").disabled,true);assert.equal(h.elements.get("selection-count").textContent,"");
});

test("simple series processing requires a frozen selection before admission",()=>{
 const h=harness();h.run('state.info={transcode_enabled:true};state.workerInfo={ready:true};$("service").value="sonarr";state.media={id:7};$("scope").value="batch";$("selected-only").checked=false;controls();');assert.equal(h.elements.get("enqueue").disabled,true);
 h.run('state.selected.add(8);controls();');assert.equal(h.elements.get("enqueue").disabled,false);
});

test("phase cost detail distinguishes measured wall intervals from unknown bytes and CPU",()=>{
 const h=harness();h.run('renderPhaseCosts($("detail-summary"),{phase_costs:{coordinator_inventory:{duration_ms:20,attempts:1,active_compute_ms:null},worker_encode:{duration_ms:500,attempts:2,nas_read_bytes:null,nas_written_bytes:400}}});');
 const rendered=JSON.stringify(h.elements.get("detail-summary").children);assert.match(rendered,/Coordinator · Inventory/);assert.match(rendered,/20 ms/);assert.match(rendered,/Unknown/);assert.match(rendered,/CPU time is only shown where the worker measured it/);assert.match(rendered,/can overlap/);
 h.run('$("detail-summary").children.length=0;renderPhaseCosts($("detail-summary"),{batch:{total:2},phase_costs:{coordinator_inventory:{duration_ms:20,attempts:1}}})');assert.equal(h.elements.get("detail-summary").children.length,0);assert.doesNotMatch(rendered,/Total.*520/);
});

test("a search that ran out of time on a busy worker says so and suggests trying again, whole",()=>{
 const h=harness();
 const busy="Ran out of time while the worker was busy with other jobs; this says nothing about the file. Try again when it is idle. Original preserved";
 h.context.busy=busy;
 assert.equal(h.run('shortJobReason(busy)'),busy);
 const job={id:"child",action_name:"transcode_media",status:"waiting_decision",reason_code:"search_budget_exhausted",waiting_reason:busy,mode:"size"};
 h.context.job=job;
 assert.equal(h.run('queuePresentation(job).status'),"Needs attention");
});


test("batch outcome buttons reveal current decisions, readable skips and ready replacements while another file waits", async () => {
  const h = harness();
  h.context.items = [
    {item_key:"ready",child_action_id:"ready-child",display_label:"Ready",status:"completed",candidate_ready:true,mode:"size",reasons:["per-source bounded samples required before full encode"]},
    {item_key:"skip",child_action_id:"skip-child",display_label:"Skipped",status:"skip",reason_code:"quality_not_met",can_reconfigure:true,mode:"size",reasons:["per-source bounded samples required before full encode","quality_not_met"]},
    {item_key:"wait",child_action_id:"wait-child",display_label:"Waiting",status:"waiting_decision",reason_code:"search_budget_exhausted",waiting_reason:"Search reached its limit; original preserved",can_reconfigure:true,mode:"size",reasons:["per-source bounded samples required before full encode"]}
  ];
  const calls=[]; h.context.record=(...args)=>calls.push(args);
  h.run('state.info={allow_destructive:true};state.detail="batch";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"waiting_decision",batch:{}};api=async()=>({items,total:3});openJob=async(id)=>record("open",id);reconfigureBatch=async(id,_candidate,selection)=>record("settings",id,selection.item_key);replaceBatchFile=async(id,child)=>record("replace",id,child);');
  await h.run('loadBatchItems("batch",2)');
  const rows=h.elements.get("batch-items-list").children;
  const all = row=>[row,...row.children.flatMap(all)];
  const text = row=>all(row).map(el=>el.textContent).filter(Boolean).join(" ");
  assert.match(text(rows[0]),/Candidate ready.*Replace file/);
  assert.match(text(rows[1]),/quality target; original kept/);
  assert.match(text(rows[2]),/Needs attention.*Search reached its limit.*Change goal/);
  assert.doesNotMatch(rows.map(text).join(" "),/quality_not_met|per-source bounded|Profile: mode-size/);
  // A row is closed until it is opened, except one that needs a decision, which
  // opens itself once; all of a file's actions wait inside the row.
  rows.forEach((row, index) => {
    const needsYou = index === 2;
    assert.equal(row.dataset.open, String(needsYou));
    assert.equal(row.children[1].hidden, !needsYou);
    assert.equal(row.children[0].children.some(el=>el.className.includes("status-action")), false, "the state is a label, not a second button");
  });
  const click = (row,label)=>all(row).find(el=>el.textContent===label).listeners.get("click")();
  await click(rows[1],"Change goal");
  await click(rows[2],"Open job");
  await click(rows[0],"Replace file");
  assert.deepEqual(calls,[["settings","batch","skip"],["open","wait-child"],["replace","batch","ready-child"]]);
});

test("file recovery freezes one item across review and submission and never falls back to the whole batch",async()=>{
  const h=harness(), calls=[];
  h.context.record=(path,body)=>calls.push([path,body]);
  h.context.plan={id:"batch",scope:"file",item_key:"skipped",selection_version:"frozen",selected:1,kept:9,files:["One omitted file"],settings:{},mode:"size",decision_version:"review-version"};
  h.run('api=async(path)=>{record(path);return plan};');
  await h.run('reconfigureBatch("batch",false,{item_key:"skipped"})');
  assert.match(calls[0][0],/scope=file&item_key=skipped/);
  assert.equal(h.elements.get("reconfigure-scope").value,"file");
  h.context.sessionStorage={getItem:()=>null,setItem(){},removeItem(){}};
  h.run('submissionID=()=>"receipt";commandRequest=async(path,body)=>record(path,body);loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};');
  await h.run('submitReconfiguredBatch()');
  assert.equal(calls[1][1].scope,"file");
  assert.equal(calls[1][1].item_key,"skipped");
  assert.equal(calls[1][1].decision_version,"review-version");
  assert.equal(calls[1][1].selection_version,"frozen");
  assert.equal(calls[1][1].mode,"size");
});

test("a ready candidate can change its goal from its row and its job, and the dialog says the candidate is removed",async()=>{
  const h=harness(), calls=[];
  h.context.record=(...args)=>calls.push(args);
  const ready={item_key:"ready",child_action_id:"ready-child",display_label:"Ready",status:"completed",candidate_ready:true,can_reconfigure:true,mode:"size",file_path:"/media/tv/Ready.mkv"};
  h.context.items=[ready,{...ready,item_key:"planned",replacement_plan:"review"},{...ready,item_key:"locked",can_reconfigure:false}];
  h.run('state.info={allow_destructive:true};state.detail="batch";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:3});reconfigureBatch=async(id,candidate,selection)=>record(id,candidate,selection);');
  await h.run('loadBatchItems("batch",2)');
  const rows=h.elements.get("batch-items-list").children;
  const all=row=>[row,...row.children.flatMap(all)];
  const labels=row=>all(row.children[1]).filter(e=>e.dataset?.jobControl).map(e=>e.textContent);
  assert.deepEqual(labels(rows[0]),["Replace file","Change goal","Open job"]);
  assert.deepEqual(all(rows[0]).filter(e=>e.dataset?.jobControl&&String(e.className).includes("jd-primary")).map(e=>e.textContent),["Replace file"]);
  // A candidate already in a replacement plan, or a file the server locked, keeps no goal change.
  assert.ok(!labels(rows[1]).includes("Change goal"));
  assert.ok(!labels(rows[2]).includes("Change goal"));
  await all(rows[0]).find(e=>e.textContent==="Change goal").listeners.get("click")();
  assert.equal(JSON.stringify(calls),JSON.stringify([["batch",false,{item_key:"ready",ready_candidate:true}]]));
  const child={id:"ready-child",action_name:"transcode_media",status:"completed",candidate_ready:true,mode:"size",parent_action_id:"batch",parent_item_key:"ready"};
  const jobLabels=job=>h.run(`jobControls(${JSON.stringify(job)}).children.map(b=>b.textContent)`);
  assert.ok(jobLabels(child).includes("Change goal"));
  assert.ok(!jobLabels({...child,replacement_plan:"queued"}).includes("Change goal"));
  assert.ok(!jobLabels({...child,replacement_action_id:"r"}).includes("Change goal"));
  // The dialog names what happens to the ready candidate; other file revisions keep the batch wording.
  const dialog=harness();
  dialog.context.plan={id:"batch",scope:"file",item_key:"ready",selection_version:"v",selected:1,kept:0,files:["Ready"],settings:{},mode:"size"};
  dialog.run('api=async()=>plan;');
  await dialog.run('reconfigureBatch("batch",false,{item_key:"ready",ready_candidate:true})');
  assert.equal(dialog.elements.get("reconfigure-note").textContent,"Same task · Original kept · Its ready candidate is removed");
  await dialog.run('reconfigureBatch("batch",false,{item_key:"ready"})');
  assert.equal(dialog.elements.get("reconfigure-note").textContent,"Same task · Originals kept · Replaced candidates removed");
});

test("file details offer goal changes and keep-original decisions without duplicate cancellation",()=>{
  const h=harness();
  const labels=h.run('jobControls({id:"child",action_name:"transcode_media",status:"waiting_decision",reason_code:"search_budget_exhausted",mode:"size",parent_action_id:"batch",parent_item_key:"one",waiting_options:[{decision:"cancel"}]}).children.map(b=>b.textContent)');
  assert.ok(labels.includes("Keep original"));assert.ok(labels.includes("Change goal"));assert.ok(!labels.includes("Cancel"));
  const skipped=h.run('queuePresentation({action_name:"transcode_media",status:"completed",reason_code:"quality_not_met",candidate_ready:false})');
  assert.equal(skipped.status,"Not converted");assert.match(skipped.summary,/quality target; original kept/);
  const unsupported=h.run('queuePresentation({action_name:"transcode_media",status:"completed",reason_code:"unsupported_source",candidate_ready:false})');
  assert.equal(unsupported.status,"Not converted");assert.match(unsupported.summary,/does not support this source format yet/);
});

test("replacement from a batch row rechecks the exact candidate and ignores stale or unrelated files",async()=>{
  const h=harness();let prepared;
  h.context.record=job=>prepared=job;
  h.run('api=async()=>({jobs:[{id:"child",parent_action_id:"batch",candidate_ready:true}]});prepareReplacement=async(job)=>record(job);');
  await h.run('replaceBatchFile("batch","child")');assert.equal(prepared.id,"child");
  h.run('api=async()=>({jobs:[{id:"child",parent_action_id:"other",candidate_ready:true}]});');
  await assert.rejects(h.run('replaceBatchFile("batch","child")'),/candidate changed/);
  h.run('api=async()=>({jobs:[{id:"child",parent_action_id:"batch",candidate_ready:false}]});');
  await assert.rejects(h.run('replaceBatchFile("batch","child")'),/candidate changed/);
  // A second tap on a file whose replacement already started goes to that replacement.
  prepared=null;
  h.run('api=async()=>({jobs:[{id:"child",parent_action_id:"batch",candidate_ready:false,replacement_action_id:"rep"}]});');
  await h.run('replaceBatchFile("batch","child")');assert.equal(prepared.replacement_action_id,"rep");
});


test("polling restores the same file control when multiple rows have identical actions",()=>{
  const h=harness();
  const container=new Element(),first=new Element(),second=new Element();
  first.dataset={jobControl:"batch",batchItemKey:"one"};second.dataset={jobControl:"batch",batchItemKey:"two"};
  first.textContent=second.textContent="Change goal";container.children=[first,second];
  h.context.container=container;h.document.activeElement=second;
  let focused;first.focus=()=>focused="one";second.focus=()=>focused="two";
  h.run('restoreControl(container,focusedControl(container))');
  assert.equal(focused,"two");
});

const allText = (el) => [el.textContent, ...(el.children || []).map(allText)].join(" ");

function queueRail(labels) {
  const h = harness();
  h.document.createElement = () => {
    const e = new Element();
    e.attrs = {};
    e.setAttribute = (key, value) => (e.attrs[key] = value);
    return e;
  };
  h.context.labels = labels;
  const rail = h.run(`(() => {
    const rail = node("div", "", "job-actions");
    for (const label of labels) {
      const b = button(label, () => {});
      b.dataset.jobControl = "job-1";
      rail.append(b);
    }
    compactQueueActions(rail);
    return rail;
  })()`);
  return { h, rail };
}

test("a queue card labels at most one main action, the most important, first", () => {
  const { rail } = queueRail(["Keep original", "Retry", "Review candidate", "Change goal"]);
  const mains = rail.children.filter((b) => /\baction-main\b/.test(b.className));
  assert.equal(mains.length, 1);
  assert.equal(mains[0].attrs["aria-label"], "Review candidate");
  assert.equal(rail.children[0], mains[0]);
  // The other three are icons in view; no menu is needed for them.
  const icons = rail.children.filter((b) => /\baction-icon\b/.test(b.className));
  assert.deepEqual(icons.map((b) => b.attrs["aria-label"]), ["Keep original", "Retry", "Change goal"]);
  assert.equal(rail.children.some((b) => b.className === "more-actions"), false);
  const none = queueRail(["Archive", "Compare frames"]).rail;
  assert.equal(none.children.some((b) => /\baction-main\b/.test(b.className)), false);
  assert.deepEqual(none.children.map((b) => b.attrs["aria-label"]), ["Compare frames", "Archive"]);
});

test("the usual actions stay in view as icons with their full name in aria-label and title", () => {
  const { rail } = queueRail(["Change settings", "Pause batch", "Cancel"]);
  assert.deepEqual(rail.children.map((b) => b.attrs["aria-label"]), ["Pause batch", "Cancel", "Change settings"]);
  assert.deepEqual(rail.children.map((b) => b.title), ["Pause batch", "Cancel", "Change settings"]);
  assert.match(rail.children[1].className, /\bdanger\b/);
  assert.ok(rail.children.every((b) => /\baction-icon\b/.test(b.className) && b.children.at(-1).className === "action-label"));
});

test("each kind of corner button keeps its own column from row to row", () => {
  const slots = (labels) => queueRail(labels).rail.children.map((b) => [b.attrs["aria-label"] || "More", b.dataset.slot]);
  assert.deepEqual(slots(["Pause batch", "Cancel", "Show in Files"]), [["Pause batch", "1"], ["Cancel", "2"], ["Show in Files", "3"]]);
  assert.deepEqual(slots(["Show in Files", "Cancel"]), [["Cancel", "2"], ["Show in Files", "3"]]);
  assert.deepEqual(slots(["Show in Files", "Archive"]), [["Show in Files", "2"], ["Archive", "3"]]);
  assert.deepEqual(slots(["Archive", "Show in Files"]), [["Show in Files", "2"], ["Archive", "3"]]);
  assert.deepEqual(slots(["Archive"]), [["Archive", "3"]]);
  assert.match(css, /\.job-row > \.job-actions \{\s*display: grid;\s*grid-template-columns: repeat\(3, var\(--action-height\)\);/);
  assert.match(css, /\[data-slot="3"\] \{ grid-column: 3; \}/);
});

test("the corner buttons share a centre line with the title and its status badge", () => {
  const block = css.match(/\.job-row > \.job-actions \{\s*position: absolute;[^}]*\}/)[0];
  assert.match(block, /top: calc\(6px \+ \(24px - var\(--action-height\)\) \/ 2\);/);
  assert.doesNotMatch(css, /@media \(min-width: 901px\) \{\s*\.job-row > \.job-actions \{ top/);
});

test("a single other action is an icon in view, not a menu", () => {
  const { rail } = queueRail(["Run again"]);
  assert.equal(rail.children.length, 1);
  assert.match(rail.children[0].className, /\baction-icon\b/);
  assert.equal(rail.children[0].attrs["aria-label"], "Run again");
});

test("with more than three icons the common ones stay and the rest go to More, which closes after a choice", () => {
  const { rail } = queueRail(["Review candidate", "Run one more test", "Keep original", "Change goal", "Archive", "View batch"]);
  // Review is the main action; Archive stays in view with the first of the rest by rank, and the others are in the menu.
  const more = rail.children.find((b) => b.className === "more-actions");
  assert.ok(more);
  assert.equal(rail.children.length, 1 + 2 + 1);
  // Archive keeps the right edge, after the menu.
  assert.equal(rail.children.at(-1).attrs["aria-label"], "Archive");
  assert.equal(rail.children.at(-1).dataset.slot, "3");
  const shown = rail.children.slice(1).filter((b) => b !== more).map((b) => b.attrs["aria-label"]);
  assert.ok(shown.includes("Archive"));
  assert.equal(more.dataset.menuFor, "job-1");
  const [toggle, menu] = more.children;
  assert.equal(toggle.attrs["aria-label"], "More actions");
  assert.equal(toggle.title, "More actions");
  const inMenu = menu.children.map((b) => b.attrs["aria-label"]);
  assert.equal(new Set([...shown, ...inMenu]).size, 5);
  assert.ok(!inMenu.includes("Archive"));
  // The same elements move, so their handlers and job-control state come along.
  assert.ok(menu.children.every((b) => b.listeners.has("click") && b.dataset.jobControl === "job-1"));
  more.open = true;
  more.listeners.get("click")({ target: { closest: () => menu.children[0] } });
  assert.equal(more.open, false);
});

test("a Stop batch stays in view while Resume and other less common actions fold into More", () => {
  const { rail } = queueRail(["Resume batch", "Stop batch · keep originals", "Change settings", "View batch", "Archive"]);
  const labels = rail.children.filter((b) => b.className !== "more-actions").map((b) => b.attrs["aria-label"]);
  assert.ok(labels.includes("Resume batch") || rail.children[0].attrs["aria-label"] === "Resume batch");
  const menu = rail.children.find((b) => b.className === "more-actions").children[1].children.map((b) => b.attrs["aria-label"]);
  assert.ok(!menu.includes("Resume batch"));
  assert.ok(menu.length >= 2);
});

function moreMenu(id, open) {
  const toggle = new Element();
  const menu = new Element();
  menu.dataset = { menuFor: id };
  menu.open = open;
  menu.querySelector = () => toggle;
  toggle.closest = () => menu;
  return { menu, toggle };
}
function menuList(menus, open) {
  const list = new Element();
  list.querySelector = () => open || null;
  list.querySelectorAll = () => menus;
  list.contains = (el) => menus.includes(el);
  return list;
}

test("a refresh keeps focus on a closed More toggle and leaves its menu closed", () => {
  const h = harness();
  const before = moreMenu("job-1", false), after = moreMenu("job-1", true);
  let focused = false;
  after.toggle.focus = () => (focused = true);
  h.document.activeElement = before.toggle;
  h.context.oldList = menuList([before.menu]);
  h.context.newList = menuList([after.menu]);
  h.run("restoreActionMenu(newList, openActionMenu(oldList))");
  assert.equal(focused, true);
  assert.equal(after.menu.open, false);
});

test("a refresh reopens an open More menu and keeps focus on its item", () => {
  const h = harness();
  const before = moreMenu("job-1", true), after = moreMenu("job-1", false);
  let toggleFocused = false;
  after.toggle.focus = () => (toggleFocused = true);
  const item = new Element();
  item.closest = () => before.menu;
  item.getAttribute = () => "View batch";
  // View batch has no data-job-control, so the menu restores it by name.
  let itemFocused = null;
  const items = ["Change goal", "View batch"].map((name) => {
    const b = new Element();
    b.getAttribute = () => name;
    b.focus = () => (itemFocused = name);
    return b;
  });
  after.menu.querySelectorAll = () => items;
  h.document.activeElement = item;
  h.context.oldList = menuList([before.menu], before.menu);
  h.context.newList = menuList([after.menu]);
  h.run("restoreActionMenu(newList, openActionMenu(oldList))");
  assert.equal(after.menu.open, true);
  assert.equal(itemFocused, "View batch");
  assert.equal(toggleFocused, false);
  // A focus outside every menu records nothing.
  h.document.activeElement = new Element();
  h.document.activeElement.closest = () => null;
  assert.equal(h.run("openActionMenu(newList)"), null);
});

test("Escape closes open More menus and returns focus to the toggle it was in", () => {
  const h = harness();
  const { menu, toggle } = moreMenu("job-1", true);
  const other = moreMenu("job-2", true);
  let focused = false;
  toggle.focus = () => (focused = true);
  const item = new Element();
  item.closest = () => menu;
  h.document.activeElement = item;
  h.document.querySelectorAll = () => [menu, other.menu];
  h.run('actionMenuKeydown({ key: "Tab" })');
  assert.equal(menu.open, true);
  h.context.escape = { key: "Escape", prevented: false, preventDefault() { this.prevented = true; } };
  h.run('actionMenuKeydown(escape)');
  assert.equal(menu.open, false);
  assert.equal(other.menu.open, false);
  assert.equal(focused, true);
  // Inside a dialog, the same Escape must not also close the dialog.
  assert.equal(h.context.escape.prevented, true);
});

test("a podcast review card labels Review cuts instead of leaving it an icon", () => {
  const cuts = queueRail(["Ad library", "Review cuts", "Revise labels", "Archive"]).rail;
  assert.equal(cuts.children[0].attrs["aria-label"], "Review cuts");
  assert.match(cuts.children[0].className, /\baction-main\b/);
  const classify = queueRail(["Ad library", "View transcript", "Validate classifications"]).rail;
  assert.equal(classify.children[0].attrs["aria-label"], "Validate classifications");
});

test("size mode details report the verification attempts and each window's search and final score", () => {
  const h = harness();
  const text = allText(h.run(`sizeModeNote({size_mode:{final_quality:{vmaf_mean:91,vmaf_p5:88,fresh_vmaf_mean:89.5,fresh_vmaf_p5:86},verification:{windows:"final_grid",attempts:[{role:"smallest",outcome:"quality_failed"},{role:"safest",outcome:"passed"},{role:"bisect",outcome:"passed"}]},windows:[{start_seconds:600,role:"discovery",search:{vmaf_mean:93},final:{vmaf_mean:91}},{start_seconds:1275,role:"unsearched",final:{vmaf_mean:84}},{start_seconds:1500,role:"fresh",final:{vmaf_mean:89}}]}})`));
  assert.match(text, /Verified on the final validation windows: smallest failed quality → safest passed → between passed/);
  assert.match(text, /Window 10:00 · discovery · search 93\.0 → final 91\.0 mean \(−2\.0\)/);
  assert.match(text, /Window 21:15 · unsearched · search — → final 84\.0 mean(?! \()/);
  assert.match(text, /Window 25:00 · fresh · search — → final 89\.0 mean/);
  assert.match(text, /Fresh windows VMAF 89\.5 mean · 86\.0 p5 · reported only/);
  assert.doesNotMatch(allText(h.run('sizeModeNote({size_mode:{final_quality:{vmaf_mean:91,vmaf_p5:86}}})')), /Window|Verified|Fresh/);
});

test("size mode details say when the search time limit ended verification", () => {
  const h = harness();
  const text = allText(h.run('sizeModeNote({size_mode:{verification:{windows:"final_grid",attempts:[{role:"smallest",outcome:"quality_failed"}],stopped_by:"time_cap",unverified:2}}})'));
  assert.match(text, /Verification stopped at the search time limit; 2 settings were never checked/);
  assert.match(allText(h.run('sizeModeNote({size_mode:{verification:{windows:"final_grid",attempts:[],stopped_by:"time_cap",unverified:1}}})')), /1 setting was never checked/);
});

test("size mode details keep the failed media engine attempt beside the x265 result", () => {
  const h = harness();
  const text = allText(h.run('sizeModeNote({size_mode:{encoder:"libx265",final_quality:{vmaf_mean:94,vmaf_p5:88},hardware_continuation:{outcome:"continued",message:"The VideoToolbox result failed the final quality check; continued with x265.",failed_quality:{vmaf_mean:93,vmaf_p5:80.2,worst_window_mean:88.1,worst_window_seconds:10,failures:["VMAF p5 80.2 is below the minimum 84.0"]}}}})'));
  assert.match(text, /failed the final quality check; continued with x265/);
  assert.match(text, /VideoToolbox final VMAF 93\.0 mean · 80\.2 p5 · 88\.1 worst 10 s window/);
  assert.match(text, /Failed: VMAF p5 80\.2 is below the minimum 84\.0/);
  assert.match(text, /Final VMAF 94\.0 mean · 88\.0 p5/);
  assert.match(allText(h.run('sizeModeNote({size_mode:{final_quality:{vmaf_mean:91,vmaf_p5:82,failed_checks:["quality_vmaf_p5_below_minimum"]}}})')), /Failed checks: vmaf p5 below minimum/);
});

test("size mode details show the failed media engine attempt's windows beside the x265 ones", () => {
  const h = harness();
  const text = allText(h.run('sizeModeNote({size_mode:{encoder:"libx265",final_quality:{vmaf_mean:94,vmaf_p5:88},windows:[{start_seconds:600,role:"discovery",search:{vmaf_mean:94},final:{vmaf_mean:93}}],hardware_continuation:{outcome:"continued",message:"continued with x265.",windows:[{start_seconds:600,role:"discovery",search:{vmaf_mean:93},final:{vmaf_mean:84}}]}}})'));
  assert.match(text, /VideoToolbox window 10:00 · discovery · search 93\.0 → final 84\.0 mean \(−9\.0\)/);
  assert.match(text, /(^|[^g] )Window 10:00 · discovery · search 94\.0 → final 93\.0 mean \(−1\.0\)/);
});

test("size mode details show the expected size, final VMAF and compacted audio", () => {
  const h = harness();
  const text = allText(h.run('sizeModeNote({size_mode:{expected_bytes:2150000000,main10:true,final_quality:{vmaf_mean:93.44,vmaf_p5:89.12,worst_window_mean:88.04,worst_window_seconds:10},compacted_audio:[{stream_index:0,from_codec:"dts",to_codec:"aac"},{stream_index:2,from_codec:"pcm_s24le",to_codec:"aac"}]}})'));
  assert.match(text, /Expected size 2\.15 GB · 10-bit HEVC/);
  assert.match(text, /Final VMAF 93\.4 mean · 89\.1 p5 · 88\.0 worst 10 s window/);
  assert.match(text, /Audio compacted: track 1 DTS-HD MA → AAC, track 3 PCM → AAC/);
  assert.doesNotMatch(allText(h.run('sizeModeNote({size_mode:{final_quality:{vmaf_mean:91,vmaf_p5:86}}})')), /worst|Audio/);
  assert.match(allText(h.run('sizeModeNote({size_mode:{final_quality:{vmaf_mean:91}}})')), /91\.0 mean · n\/a p5/);
  assert.equal(h.run('codecLabel("dts")'), "DTS");
  assert.equal(h.run("sizeModeNote({})"), null);
  assert.equal(h.run("sizeModeNote({size_mode:{}})"), null);
  const copied = allText(h.run('sizeModeNote({size_mode:{expected_bytes:6500000000,video_copy:"verified",compacted_audio:[{stream_index:0,from_codec:"truehd",to_codec:"aac"}]}})'));
  assert.match(copied, /Expected size 6\.50 GB/);
  assert.match(copied, /Video copied unchanged · verified bit-identical/);
  assert.doesNotMatch(copied, /10-bit|VMAF/);
  assert.match(allText(h.run('sizeModeNote({size_mode:{video_copy:"failed"}})')), /Video copy check failed · original kept/);
  assert.match(allText(h.run('sizeModeNote({size_mode:{video_copy:"pending"}})')), /will be copied unchanged · not yet verified/);
  assert.match(h.run('shortJobReason("audio_estimate_unavailable")'), /could not be read/);
  assert.match(h.run('shortJobReason("audio_compaction_insufficient")'), /lossless audio alone would not save enough/);
});

test("a job paused because quality checks are unavailable is not shown as still testing", () => {
  const h = harness();
  h.context.job = {id:"p",action_name:"transcode_media",mode:"compact",status:"waiting_decision",waiting_reason:"quality_vmaf_model_unavailable: v1_1080p_3h; original preserved",
    benchmark:{outcome:"quality_policy_unavailable",message:"No encode was started: the worker cannot run the quality checks this goal requires. The original is preserved.",next_step:"Cancel to keep the original."}};
  const text = allText(h.run("resultReport(job)"));
  assert.match(text, /Quality checks unavailable/);
  assert.match(text, /No encode was started/);
  assert.match(text, /Next step: .*Cancel to keep the original/);
  assert.doesNotMatch(text, /quality_policy_unavailable/);
  assert.match(allText(h.run('resultReport({benchmark:{outcome:"attention",message:"The worker needs a decision.",next_step:"Review."}})')), /Needs your decision/);
});

test("the worker details explain why a connected worker still refuses jobs", () => {
  const h = harness();
  h.context.host = new Element();
  const note = "The worker is ready, but its deep checks failed, and job submission runs the same checks: transcode jobs will be refused until the doctor error is fixed.";
  h.run(`renderWorkerDetails(host, {nodes:[{name:"Video worker",connected:true,reachable:true,ready:false,admission_note:${JSON.stringify(note)}}]})`);
  assert.match(allText(h.context.host), /deep checks failed.*refused/);
  h.run('renderWorkerDetails(host, {nodes:[{name:"Video worker",connected:true,reachable:true,ready:true}]})');
  assert.doesNotMatch(allText(h.context.host), /deep checks/);
});

test("negative sizes and machine reason codes are readable", () => {
  const h = harness();
  assert.match(h.run("bytes(-408021893)"), /^−\d/);
  assert.doesNotMatch(h.run('queuePresentation({action_name:"benchmark_transcode",status:"completed",savings:{source_bytes:6000000000,estimated_bytes:6400000000,estimated_saved_bytes:-400000000}}).summary'), /-\d+ B|−.*savings/);
  for (const raw of ["quality_vmaf_model_unavailable: v1_1080p_3h is missing", "no_suitable_candidate", "some_new_engine_code"]) {
    assert.doesNotMatch(h.run(`shortJobReason(${JSON.stringify(raw)})`), /[a-z]+_[a-z]+/, raw);
  }
});

test("the process button says how many files it starts", () => {
  const h = harness();
  h.run('$("path").value="/media/one.mkv"; controls();');
  assert.equal(h.elements.get("enqueue").textContent, "Process file");
});

test("technical profiles live in a labelled Advanced section that keeps the goal visible", () => {
  const form = markup.slice(markup.indexOf('<form id="job-form"'), markup.indexOf("</form>", markup.indexOf('<form id="job-form"')));
  const advanced = form.slice(form.indexOf('<details id="advanced-profiles"'), form.indexOf("</details>", form.indexOf('<details id="advanced-profiles"')));
  assert.match(advanced, /<summary>.*Advanced: technical profiles/);
  assert.match(advanced, /id="technical"/);
  assert.match(advanced, /id="legacy-controls"/);
  assert.match(advanced, /goal above is not used/);
  assert.ok(form.indexOf('id="processing-goal"') < form.indexOf('id="advanced-profiles"'));
  assert.doesNotMatch(markup, /Technical tools and legacy profiles/);
});

test("turning technical profiles on marks the goal as not used instead of hiding it", () => {
  const h = harness();
  h.run('$("path").value="/media/one.mkv"; $("technical").checked=true; controls();');
  const el = (id) => h.elements.get(id);
  assert.equal(el("processing-goal").hidden, false);
  assert.equal(el("processing-goal").disabled, true);
  assert.equal(el("goal-overridden").hidden, false);
  assert.equal(el("advanced-profiles").open, true);
  assert.equal(el("legacy-controls").hidden, false);
  assert.equal(el("advanced-state").textContent, "On · goal not used");
  assert.equal(el("enqueue").textContent, "Create candidate");
  // Collapsing the section is respected; the summary and goal note still say it is on.
  h.run('$("advanced-profiles").open=false; controls();');
  assert.equal(el("advanced-profiles").open, false);
  assert.equal(el("goal-overridden").hidden, false);
  h.run('$("technical").checked=false; controls();');
  assert.equal(el("processing-goal").disabled, false);
  assert.equal(el("goal-overridden").hidden, true);
  assert.equal(el("legacy-controls").hidden, true);
  assert.equal(el("advanced-state").textContent, "Off");
  assert.equal(el("enqueue").textContent, "Process file");
});

test("the goal is sent only while technical profiles are off", async () => {
  const { h } = submissionHarness();
  const sent = [];
  h.context.capture = (_tool, args) => { sent.push(JSON.parse(args.inputs)); return { id: "submitted" }; };
  h.run('processingMode=()=>"size"; tool=async(...args)=>capture(...args); $("technical").checked=false;');
  await h.run('buildAndSubmitJob("encode")');
  h.run('$("technical").checked=true; $("path").value="/media/one.mp4"; $("scope").value="file";');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[0].mode, "size");
  assert.equal(sent[0].profile, undefined);
  assert.equal(sent[1].mode, undefined);
  assert.equal(sent[1].profile, "general-hevc");
});

test("Audio only is sent as the size goal with audio_only and no encoder or size limit", async () => {
  const { h } = submissionHarness();
  const sent = [];
  h.context.capture = (_tool, args) => { sent.push(JSON.parse(args.inputs)); return { id: "submitted" }; };
  h.run('processingMode=()=>"audio_only"; tool=async(...args)=>capture(...args); $("technical").checked=false; $("allow-hardware").checked=true; $("path").value="/media/one.mp4"; $("scope").value="file";');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[0].mode, "size");
  assert.equal(sent[0].audio_only, true);
  for (const key of ["allow_hardware", "size_budget_percent", "size_budget_bytes", "size_budget_source"]) assert.equal(key in sent[0], false, key);
  h.run('processingMode=()=>"size"; $("path").value="/media/two.mp4";');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal("audio_only" in sent[1], false);
  // Its own options stay hidden: the video is copied, so there is no encoder or limit to pick.
  const form = harness();
  form.run('processingMode=()=>"audio_only"; controls();');
  assert.equal(form.elements.get("hardware-option").hidden, true);
  assert.equal(form.elements.get("size-budget-option").hidden, true);
});

test("the media engine option belongs to the smaller files goal and starts on", () => {
  const form = markup.slice(markup.indexOf('<form id="job-form"'), markup.indexOf("</form>", markup.indexOf('<form id="job-form"')));
  const goal = form.slice(form.indexOf('<fieldset id="processing-goal"'), form.indexOf("</fieldset>", form.indexOf('<fieldset id="processing-goal"')));
  assert.match(goal, /id="allow-hardware" type="checkbox" checked hidden \/>/);
  assert.match(goal, /Fast · media engine/);
  assert.match(goal, /Smallest · x265/);
  assert.doesNotMatch(goal, /hardware-margin|x265's size/);
  const h = harness();
  const el = (id) => h.elements.get(id);
  h.run('processingMode=()=>"quality"; controls();');
  assert.equal(el("hardware-option").hidden, true);
  h.run('processingMode=()=>"size"; $("allow-hardware").checked=true; controls();');
  assert.equal(el("hardware-option").hidden, false);
  h.run('$("allow-hardware").checked=false; controls();');
  h.run('processingMode=()=>"compact"; controls();');
  assert.equal(el("hardware-option").hidden, true);
  // Ticking the box in a browser fires its change event, which must re-run controls().
  h.run('processingMode=()=>"size"; $("allow-hardware").checked=true; controls();');
  const onChange = el("allow-hardware").listeners.get("change");
  assert.equal(typeof onChange, "function", "the checkbox has no change handler");
  h.run('$("allow-hardware").checked=false;');
  onChange();
  assert.equal(el("hardware-option").hidden, false);
});

test("the smaller files goal always says whether the media engine is allowed", async () => {
  const { h } = submissionHarness();
  const sent = [];
  h.context.capture = (_tool, args) => { sent.push(JSON.parse(args.inputs)); return { id: "submitted" }; };
  h.run('processingMode=()=>"size"; tool=async(...args)=>capture(...args); $("technical").checked=false; $("allow-hardware").checked=false; $("path").value="/media/one.mp4"; $("scope").value="file";');
  await h.run('buildAndSubmitJob("encode")');
  h.run('$("allow-hardware").checked=true; $("path").value="/media/two.mp4";');
  await h.run('buildAndSubmitJob("encode")');
  h.run('processingMode=()=>"quality"; $("path").value="/media/three.mp4";');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[0].mode, "size");
  assert.equal(sent[0].allow_hardware, false);
  assert.equal("hardware_margin_percent" in sent[0], false);
  assert.equal(sent[1].mode, "size");
  assert.equal(sent[1].allow_hardware, true);
  assert.equal("hardware_margin_percent" in sent[1], false);
  assert.equal(sent[2].mode, "quality");
  assert.equal(sent[2].allow_hardware, undefined);
  assert.equal("hardware_margin_percent" in sent[2], false);
  assert.equal(sent.length, 3);
});

test("changing the goal to smaller files carries the media engine choice", async () => {
  const markupGoal = markup.slice(markup.indexOf('id="reconfigure-hardware-option"'), markup.indexOf('id="reconfigure-profile"'));
  assert.match(markupGoal, /id="reconfigure-hardware" type="checkbox" checked hidden/);
  assert.doesNotMatch(markupGoal, /hardware-margin/);
  const open = async (plan) => {
    const h = harness();
    h.context.savedPlan = plan;
    h.run('state.info={};state.recipes=[];api=async()=>savedPlan;');
    await h.run('reconfigureBatch("b")');
    return h;
  };
  const base = { id: "b", mode: "quality", scope: "all", selected: 2, kept: 0, active: 0, files: ["one", "two"], selection_version: "v1" };
  // A batch that never said anything gets the default: on.
  let h = await open({ ...base, settings: { allow_hardware: null, hardware_margin_percent: null } });
  const el = (id) => h.elements.get(id);
  assert.equal(el("reconfigure-hardware-option").hidden, true, "only the smaller files goal shows it");
  h.run('$("reconfigure-mode").value="size"; updateReconfigureHardware();');
  assert.equal(el("reconfigure-hardware-option").hidden, false);
  assert.equal(el("reconfigure-hardware").checked, true);
  const calls = [];
  h.context.record = (path, body) => calls.push([path, body]);
  h.context.sessionStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  h.run('submissionID=()=>"goal-1";commandRequest=async(path,body)=>{record(path,body);return {id:"b"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};');
  await h.run("submitReconfiguredBatch()");
  assert.equal(calls[0][1].mode, "size");
  assert.equal(calls[0][1].allow_hardware, true);
  assert.equal("hardware_margin_percent" in calls[0][1], false);
  // Turned off: sent as an explicit false.
  h.run('$("reconfigure-hardware").checked=false; updateReconfigureHardware(); submissionID=()=>"goal-2";');
  await h.run("submitReconfiguredBatch()");
  assert.equal(calls[1][1].allow_hardware, false);
  // Files frozen with different choices say so instead of pretending to agree.
  h = await open({ ...base, settings: { allow_hardware: true, hardware_margin_percent: 15, hardware_mixed: true } });
  assert.match(h.elements.get("reconfigure-settings").textContent, /different media engine choices/);
  h = await open({ ...base, settings: { allow_hardware: true, hardware_margin_percent: 15, hardware_mixed: false } });
  assert.doesNotMatch(h.elements.get("reconfigure-settings").textContent, /different media engine choices/);
  // A saved choice is shown as saved, and other goals send nothing about it.
  h = await open({ ...base, settings: { allow_hardware: false, hardware_margin_percent: 30 } });
  assert.equal(h.elements.get("reconfigure-hardware").checked, false);
  h.context.record = (path, body) => calls.push([path, body]);
  h.context.sessionStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  h.run('$("reconfigure-mode").value="compact"; submissionID=()=>"goal-3";commandRequest=async(path,body)=>{record(path,body);return {id:"b"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};');
  await h.run("submitReconfiguredBatch()");
  assert.equal(calls[2][1].mode, "compact");
  assert.equal("allow_hardware" in calls[2][1], false);
});

test("a VideoToolbox winner says why it was chosen next to the encoder line", () => {
  const h = harness();
  const reason = "25000 bytes against 23500 for the smallest software candidate, within the 15% margin";
  const hardware = h.run(`sizeModeNote({size_mode:{expected_bytes:2150000000,encoder:"hevc_videotoolbox",hardware:true,hardware_reason:${JSON.stringify(reason)}}})`);
  const rows = hardware.children.map((row) => allText(row));
  const at = rows.findIndex((row) => /^Encoder: VideoToolbox/.test(row));
  assert.ok(at >= 0, "encoder line present");
  assert.equal(rows[at + 1], `Chosen because: ${reason}`);
  // No reason recorded (an older decision), x265, or a reason without the hardware flag: no line.
  const without = h.run('sizeModeNote({size_mode:{encoder:"hevc_videotoolbox",hardware:true}})');
  assert.doesNotMatch(allText(without), /Chosen because/);
  const software = h.run('sizeModeNote({size_mode:{encoder:"libx265",hardware_reason:"stale"}})');
  assert.doesNotMatch(allText(software), /Chosen because/);
});

test("podcast cleaning is not launched from the main UI, but a podcast job in the queue can still be reviewed", () => {
  for (const id of ["podcast-new", "podcast-dialog", "podcast-form", "close-podcast", "podcast-id", "podcast-source", "podcast-output"]) assert.doesNotMatch(markup, new RegExp(`id="${id}"`));
  assert.doesNotMatch(source, /\$\("podcast-(new|form|dialog)"\)|\$\("close-podcast"\)/);
  assert.match(source, /async function reviewPodcastCuts/);
  assert.match(source, /async function viewPodcastTranscript/);
  assert.match(source, /async function viewPodcastAdLibrary/);
  const h = harness();
  h.run('state.info={allow_destructive:false};');
  const controls = h.run('jobControls({id:"p-1",status:"waiting_decision",waiting_condition:"podcast_review",podcast:{podcast_id:"x",cuts_digest:"d"}})');
  assert.ok(flatten(controls).some((c) => c.textContent === "Review cuts"));
});

test("size goal discloses which audio becomes AAC", () => {
  const radio = markup.match(/<label class="goal-head"><input type="radio" name="processing-mode" value="size" checked \/>[\s\S]*?<\/label>/)?.[0] || "";
  assert.match(radio, /Use it for most files, 4K included\. Same look and resolution, at least 15% smaller; lossless and high-bitrate audio becomes AAC\./);
  assert.match(source, /lossless Atmos\/DTS:X lose object and height channels; lossy tracks \(E-AC-3 Atmos included\) copied/);
  assert.match(source, /AAC-LC · lossless tracks, and lossy tracks at least 1\.25× the target, become AAC-LC/);
  assert.match(source, /"size_aac_bed" \? "AAC-LC · [^"]*Atmos keeps its 5\.1\/7\.1 bed, objects dropped"/);
});

test("compact goal says in plain language what it drops and still needs approval", () => {
  const radio = markup.match(/<label class="goal-head"><input type="radio" name="processing-mode" value="compact" \/>[\s\S]*?<\/label>/)?.[0] || "";
  assert.match(radio, /<strong id="goal-title-compact" class="goal-title">Shrink to 1080p SDR<\/strong>/);
  assert.match(radio, /Use it for HDR and Dolby Vision, or 4K you don't need\. Converted to SDR, at least 30% smaller; lossless audio becomes AAC\./);
  assert.match(markup, /<option value="compact">Shrink to SDR · At least 30% smaller, HDR removed/);
  assert.match(source, /item\.mode === "compact" \? shrinkLabel\(item\.max_height\)/);
});

test("a source hash queued behind others reads as waiting, not stuck at zero bytes", () => {
  const h = metricProgressHarness();
  h.run('progressJob.activities=[{id:"child",work:{phase:"waiting_for_hash_slot",total_bytes:1000}}];');
  const waiting = h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)');
  assert.equal(waiting, "Waiting to verify source");
  h.run('progressJob.activities[0].work={phase:"hashing_source",bytes_read:0,total_bytes:1000};');
  assert.match(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), /^Verifying source · /);
});

function finishedEncodeHarness() {
  const h = harness();
  h.run(`Date.now = () => Date.parse("2026-10-16T12:22:54Z");
    globalThis.progressJob = {id:"file",status:"running",current_step:6,
      stages:["preflight","submit_benchmark","wait_benchmark","submit_transcode","wait_transcode","validate_result","accept_result"].map(name=>({name})),
      worker:{transcode_status:"completed",transcode_phase:"completed",progress:100,progress_is_stale:true,
        last_progress_at:"2026-10-16T12:00:00Z",worker_heartbeat_at:"2026-10-16T12:22:50Z"},
      work:{phase:"probing_source"}};
    progressJob.activities=[{...progressJob,id:"child"}];`);
  return h;
}
const meterOf = element => element.children.find(child => child.max != null);

test("a finished encode shows the original's re-check, never Completed", () => {
  const h = finishedEncodeHarness();
  // Preflight's last phase is all an older coordinator leaves behind.
  let text = progressText(h.run('telemetry(progressJob)'));
  assert.match(text, /Conversion finished · Review converted file/);
  assert.match(text, /Encode finished /);
  assert.doesNotMatch(text, /Completed|100\.0%|Measured|Inspecting source/);
  assert.equal(h.run('queuePresentation(progressJob).status'), "In progress");

  h.run('progressJob.work={phase:"waiting_for_hash_slot",total_bytes:20e9};progressJob.activities[0].work=progressJob.work;');
  const waiting = h.run('telemetry(progressJob)');
  assert.match(progressText(waiting), /Waiting to verify original/);
  assert.equal(meterOf(waiting), undefined);
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), "Waiting to verify original");

  h.run('progressJob.work={phase:"verifying_original",bytes_read:12e9,total_bytes:20e9};progressJob.activities[0].work=progressJob.work;');
  const checking = h.run('telemetry(progressJob)');
  text = progressText(checking);
  assert.match(text, /Checking the original is unchanged.*12\.00 GB \/ 20\.00 GB.*Encode finished /);
  assert.doesNotMatch(text, /Completed|100\.0%|Measured/);
  assert.equal(meterOf(checking).max, 20e9);
  assert.equal(meterOf(checking).value, 12e9);
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), "Checking the original is unchanged · 12.00 GB / 20.00 GB");

  // Preflight queues its own source hash; that is not the original's re-check.
  h.run('progressJob.current_step=0;progressJob.work={phase:"waiting_for_hash_slot",total_bytes:20e9};');
  assert.doesNotMatch(progressText(h.run('telemetry(progressJob)')), /Waiting to verify original/);

  // Once the step hands the candidate over, telemetry is no longer current work.
  h.run('progressJob.current_step=6;progressJob.status="waiting_decision";progressJob.work={phase:"verifying_original",bytes_read:20e9,total_bytes:20e9};');
  assert.equal(h.run('postEncodeWork(progressJob)'), null);
  // A finished calibration is not a finished conversion.
  assert.equal(h.run('postEncodeWork({status:"running",worker:{benchmark_status:"completed",phase:"completed"}})'), null);
});

test("the original's re-check keeps a waiting reason and also shows for a skipped conversion", () => {
  const h = finishedEncodeHarness();
  h.run('progressJob.activities[0].waiting_condition="worker_reconciling";progressJob.activities[0].waiting_reason="Coordinator request interrupted";');
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), "Conversion finished · Review converted file · Coordinator request interrupted");

  // A skipped conversion re-checks the original without any worker telemetry.
  h.run('delete progressJob.worker;progressJob.activities=[{...progressJob,id:"child"}];');
  h.run('progressJob.work=progressJob.activities[0].work={phase:"waiting_for_hash_slot",total_bytes:20e9};');
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), "Waiting to verify original");
  assert.match(progressText(h.run('telemetry(progressJob)')), /Waiting to verify original/);
  h.run('progressJob.work=progressJob.activities[0].work={phase:"verifying_original",bytes_read:1e9,total_bytes:20e9};');
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), "Checking the original is unchanged · 1.00 GB / 20.00 GB");
  const text = progressText(h.run('telemetry(progressJob)'));
  assert.match(text, /Checking the original is unchanged.*1\.00 GB \/ 20\.00 GB/);
  assert.doesNotMatch(text, /Encode finished/);
  // After a calibration only, too: its "Completed" is not the current work.
  h.run('progressJob.worker={benchmark_status:"completed",phase:"completed"};');
  assert.match(progressText(h.run('telemetry(progressJob)')), /Checking the original is unchanged/);
});

test("preparing recovery names and measures each stage instead of a frozen step", () => {
  const h = harness();
  h.run(`globalThis.promotionJob = {id:"promote",status:"running",current_step:2,
    stages:["plan_promotion","approve_promotion","preserve_original","import_candidate","remove_old_file","rename_candidate","rescan_library","finalize_promotion"].map(name=>({name}))};`);
  // Without coordinator work the step itself is all there is to show.
  assert.match(progressText(h.run('telemetry(promotionJob)')), /Step 3\/8 · Verify original and prepare recovery/);

  h.run('promotionJob.work={phase:"waiting_for_hash_slot",total_bytes:20e9};');
  const waiting = h.run('telemetry(promotionJob)');
  assert.match(progressText(waiting), /Waiting for a hash slot.*Step 3\/8 · Verify original and prepare recovery/);
  assert.equal(meterOf(waiting), undefined);

  for (const [phase, label] of [
    ["verifying_original", "Re-checking the original"],
    ["verifying_candidate", "Re-checking the converted file"],
    ["copying_recovery", "Copying the recovery copy"],
    ["verifying_recovery", "Verifying the recovery copy"],
  ]) {
    h.run(`promotionJob.work={phase:"${phase}",bytes_read:10e9,total_bytes:20e9};`);
    const element = h.run('telemetry(promotionJob)');
    assert.match(progressText(element), new RegExp(`${label} · 50%.*10\\.00 GB / 20\\.00 GB.*Step 3/8 · Verify original and prepare recovery`));
    assert.equal(meterOf(element).max, 20e9);
    assert.equal(meterOf(element).value, 10e9);
  }
  // Only the whole file reads as 100%.
  h.run('promotionJob.work={phase:"copying_recovery",bytes_read:20e9-1,total_bytes:20e9};');
  assert.match(h.run('postEncodeWork(promotionJob).label'), / · 99%$/);

  // The same stage reads correctly as a batch replacement's file activity.
  h.run('promotionJob.work={phase:"verifying_recovery",bytes_read:1e9,total_bytes:4e9};');
  h.run('globalThis.batchJob={status:"running",activities:[{...promotionJob,id:"promote"}]};');
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"promote"},batchJob)'), "Verifying the recovery copy · 25% · 1.00 GB / 4.00 GB");

  // Another step's leftover work is not the recovery copy's.
  h.run('promotionJob.current_step=3;');
  assert.equal(h.run('preserveWork(promotionJob)'), null);
  assert.match(progressText(h.run('telemetry(promotionJob)')), /Step 4\/8 · Import verified candidate/);
  // An original that cannot be moved aside is copied while it is removed.
  h.run('promotionJob.current_step=4;promotionJob.work={phase:"copying_recovery",bytes_read:5e9,total_bytes:20e9};');
  assert.match(progressText(h.run('telemetry(promotionJob)')), /Copying the recovery copy · 25%.*5\.00 GB \/ 20\.00 GB.*Step 5\/8 · Remove original library file/);
  h.run('promotionJob.work={phase:"waiting_for_hash_slot",total_bytes:20e9};');
  assert.match(progressText(h.run('telemetry(promotionJob)')), /Waiting for a hash slot.*Step 5\/8 · Remove original library file/);
  // Its full read of the original is shown too, with a measured bar.
  h.run('promotionJob.work={phase:"verifying_original",bytes_read:5e9,total_bytes:20e9};');
  const reread = h.run('telemetry(promotionJob)');
  assert.match(progressText(reread), /Re-checking the original · 25%.*5\.00 GB \/ 20\.00 GB.*Step 5\/8 · Remove original library file/);
  assert.equal(meterOf(reread).max, 20e9);
  // Any other phase there is still not recovery work.
  h.run('promotionJob.work={phase:"verifying_candidate",bytes_read:5e9,total_bytes:20e9};');
  assert.equal(h.run('preserveWork(promotionJob)'), null);
  // A stopped promotion shows no current work.
  h.run('promotionJob.current_step=2;promotionJob.status="failed";');
  assert.equal(h.run('postEncodeWork(promotionJob)'), null);
});

test("full-output checks and publication after 100% name the converted file and the encode's end", () => {
  for (const [phase, label] of [["validating", "Verifying converted file"], ["publishing", "Saving converted file"]]) {
    const h = finishedEncodeHarness();
    h.context.phase = phase;
    h.run('progressJob.worker.transcode_status="running";progressJob.worker.transcode_phase=phase;');
    const text = progressText(h.run('telemetry(progressJob)'));
    assert.match(text, new RegExp(`${label}.*100\\.0%.*Encode finished `));
    assert.doesNotMatch(text, /Measured /);
  }
  const h = finishedEncodeHarness();
  h.run('progressJob.worker={transcode_status:"running",transcode_phase:"encoding",progress:64,last_progress_at:"2026-10-16T12:22:50Z",worker_heartbeat_at:"2026-10-16T12:22:50Z"};');
  const text = progressText(h.run('telemetry(progressJob)'));
  assert.match(text, /Measured /);
  assert.doesNotMatch(text, /Encode finished/);
});

test("every shortened main action label is a prefix of its accessible name", () => {
  const h = harness();
  const pairs = JSON.parse(h.run("JSON.stringify(Object.entries(shortActionLabels))"));
  assert.ok(pairs.length > 0);
  for (const [full, visible] of pairs) assert.ok(full.startsWith(visible), `${visible} is not a prefix of ${full}`);
  assert.deepEqual(pairs.filter(([full]) => full.startsWith("Check")).map(([, v]) => v), ["Check library"]);
  // Trying a failed save again is a short label already, and a main action.
  assert.ok(!pairs.some(([full]) => full === "Try again"));
  assert.equal(h.run('mainQueueActions.includes("Try again")'), true);
});

function commandHarness(commands) {
  const h = harness();
  h.context.fixture = commands;
  h.run('state.commands=new Map(fixture.map(c=>[c.command_id,c]));');
  return h;
}
const chipOf = (h, id, now) => JSON.parse(h.run(`JSON.stringify((({kind,text,command})=>({kind,text,command:command.command_id}))(commandChip(${JSON.stringify(id)},${now})||{kind:null,text:null,command:{}}))`));

test("command chip prefers a failure, then work in flight, then a fresh Applied", () => {
  const now = 1_000_000;
  const h = commandHarness([
    {command_id:"a1", id:"job-a", kind:"retry", label:"Retrying job", status:"running", started:now - 60_000, seenAt:now - 60_000},
    {command_id:"a2", id:"job-a", kind:"cancel", label:"Stopping job", status:"failed", error:"Already finished.", started:now - 5_000, seenAt:now - 4_000},
    {command_id:"b1", id:"job-b", kind:"resume", label:"Applying decision", status:"completed", started:now - 3_000, seenAt:now - 1_000},
    {command_id:"b2", id:"job-b", kind:"reconfigure", label:"Updating batch settings", status:"pending", started:now - 20_000, seenAt:now - 20_000},
    {command_id:"c1", id:"job-c", kind:"retry", label:"Retrying job", status:"failed", started:now - 90_000, seenAt:now - 80_000},
    {command_id:"c2", id:"job-c", kind:"reconfigure", label:"Updating batch settings", status:"running", started:now - 10_000, seenAt:now - 10_000},
    {command_id:"d1", id:"job-d", kind:"retry", label:"Retrying job", status:"failed", started:now - 10_000, seenAt:now - 9_000},
    {command_id:"e1", id:"job-e", kind:"resume", label:"Applying decision", status:"waiting_external", started:now - 10_000, seenAt:now - 10_000},
  ]);
  assert.deepEqual(chipOf(h, "job-a", now), {kind:"failed", text:"Stop failed", command:"a2"});
  assert.deepEqual(chipOf(h, "job-b", now), {kind:"active", text:"Updating settings", command:"b2"});
  // A newer command replaces an older failure.
  assert.deepEqual(chipOf(h, "job-c", now), {kind:"active", text:"Updating settings", command:"c2"});
  assert.deepEqual(chipOf(h, "job-d", now), {kind:"failed", text:"Retry failed", command:"d1"});
  assert.deepEqual(chipOf(h, "job-e", now), {kind:"active", text:"Waiting to apply", command:"e1"});
  assert.equal(h.run(`commandChip("job-none",${now})`), null);
  h.run('state.commands.get("d1").dismissed=true;');
  assert.equal(h.run(`commandChip("job-d",${now})`), null);
});

test("Applied expires about eight seconds after completion was first seen and cancelled shows nothing", () => {
  const now = 2_000_000;
  const h = commandHarness([
    {command_id:"f1", id:"job-f", kind:"resume", label:"Applying decision", status:"completed", started:now - 60_000, seenAt:now - 7_000},
    {command_id:"g1", id:"job-g", kind:"resume", label:"Applying decision", status:"completed", started:now - 60_000, seenAt:now - 9_000},
    {command_id:"h1", id:"job-h", kind:"cancel", label:"Stopping job", status:"cancelled", started:now - 2_000, seenAt:now - 1_000},
  ]);
  const fresh = JSON.parse(h.run(`JSON.stringify(commandChip("job-f",${now}))`));
  assert.equal(fresh.kind, "applied");
  assert.equal(fresh.text, "Applied");
  assert.equal(fresh.expires, now + 1_000);
  assert.equal(h.run(`commandChip("job-g",${now})`), null);
  assert.equal(h.run(`commandChip("job-h",${now})`), null);
});

test("tracking keeps the first time a status was seen and announces only changes, together and by job", () => {
  const h = commandHarness([]);
  const timers = [];
  h.context.setTimeout = (fn) => timers.push(fn);
  h.run('renderCommandIndicators=()=>{};state.operationJobs=new Map([["w12",{id:"job-t",number:12}],["w13",{id:"job-u",number:13}]]);');
  h.context.receipt = {key:"k", started:1, command_id:"t1", id:"job-t", label:"Retrying job"};
  h.context.other = {key:"l", started:2, command_id:"u1", id:"job-u", label:"Stopping job"};
  h.run('trackCommand({kind:"retry"},receipt,{status:"running"});trackCommand({kind:"cancel"},other,{status:"running"})');
  const firstSeen = h.run('state.commands.get("t1").seenAt');
  h.run('trackCommand({kind:"retry"},receipt,{status:"running"})');
  assert.equal(h.run('state.commands.get("t1").seenAt'), firstSeen);
  assert.equal(timers.length, 0);
  // Two changes in the same moment are read as one message.
  h.run('trackCommand({kind:"retry"},receipt,{status:"failed",error:"No worker was free."});trackCommand({kind:"cancel"},other,{status:"completed"})');
  assert.equal(timers.length, 1);
  timers.shift()();
  assert.equal(h.elements.get("command-announcer").textContent, "Job #12: Retrying job, failed. No worker was free. Job #13: Stopping job, applied.");
  assert.equal(h.elements.get("detail-command-announcer").textContent, "");
  // The open job detail makes the page inert, so it speaks from inside the dialog.
  h.context.third = {key:"m", started:3, command_id:"t2", id:"job-t", label:"Retrying job"};
  h.run('$("job-detail").open=true;trackCommand({kind:"retry"},third,{status:"pending"});trackCommand({kind:"retry"},third,{status:"running"})');
  timers.shift()();
  assert.equal(h.elements.get("detail-command-announcer").textContent, "Job #12: Retrying job, applying.");
  assert.equal(h.elements.get("command-announcer").textContent, "");
  // After a reload, the first poll of a restored command is not news unless it finished.
  h.context.restored = {key:"n", started:4, command_id:"u2", id:"job-u", label:"Stopping job"};
  h.run('trackCommand({kind:"cancel"},restored,{status:"pending"}).restored=true;trackCommand({kind:"cancel"},restored,{status:"running"})');
  assert.equal(timers.length, 0);
  h.run('trackCommand({kind:"cancel"},restored,{status:"failed",error:"Already stopped."})');
  timers.shift()();
  assert.equal(h.elements.get("detail-command-announcer").textContent, "Job #13: Stopping job, failed. Already stopped.");
});

// Just enough DOM for the chip renderer: class and data selectors, :scope
// children, closest, removal and focus.
class FakeNode {
  constructor(doc, tag = "div") {
    Object.assign(this, {doc, tagName: tag.toUpperCase(), children: [], parent: null, dataset: {}, style: {}, className: "", textContent: "", id: "", listeners: new Map()});
  }
  get classList() {
    const names = () => this.className.split(/\s+/).filter(Boolean);
    return {
      add: (c) => { if (!names().includes(c)) this.className = [...names(), c].join(" "); },
      remove: (c) => { this.className = names().filter((n) => n !== c).join(" "); },
      toggle: (c, on = !names().includes(c)) => (on ? this.classList.add(c) : this.classList.remove(c)),
      contains: (c) => names().includes(c),
    };
  }
  setAttribute(name, value) { (this.attrs ||= {})[name] = String(value); }
  getAttribute(name) { return this.attrs?.[name] ?? null; }
  removeAttribute(name) { delete this.attrs?.[name]; }
  addEventListener(name, fn) { this.listeners.set(name, fn); }
  append(...nodes) { for (const n of nodes) { n.parent?.drop(n); n.parent = this; this.children.push(n); } }
  before(n) { n.parent?.drop(n); n.parent = this.parent; this.parent.children.splice(this.parent.children.indexOf(this), 0, n); }
  drop(n) { this.children = this.children.filter((c) => c !== n); n.parent = null; }
  remove() { if (this.contains(this.doc.activeElement)) this.doc.activeElement = this.doc.body; this.parent?.drop(this); }
  replaceChildren(...nodes) { for (const c of [...this.children]) c.remove(); this.append(...nodes); }
  contains(n) { for (let x = n; x; x = x.parent) if (x === this) return true; return false; }
  focus() { this.doc.activeElement = this; }
  is(simple) {
    const data = simple.match(/^\[data-([a-z-]+)\]$/);
    if (data) return this.dataset[data[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase())] != null;
    if (/^[a-z]+$/.test(simple)) return this.tagName === simple.toUpperCase();
    return simple.startsWith(".") && this.classList.contains(simple.slice(1));
  }
  // "A B": B inside an A.
  isWithin(compound, root = null) {
    const [last, ...outer] = compound.split(/\s+/).reverse();
    if (!this.is(last)) return false;
    let x = this.parent;
    for (const part of outer) {
      while (x && x !== root && !x.is(part)) x = x.parent;
      if (!x || x === root) return false;
      x = x.parent;
    }
    return true;
  }
  matches(selector) { return selector.split(",").some((s) => this.isWithin(s.trim())); }
  closest(selector) { for (let x = this; x; x = x.parent) if (x.matches(selector)) return x; return null; }
  descendants() { return this.children.flatMap((c) => [c, ...c.descendants()]); }
  querySelectorAll(selector) {
    return selector.split(",").map((s) => s.trim()).flatMap((s) =>
      s.startsWith(":scope > ") ? this.children.filter((c) => c.is(s.slice(9))) : this.descendants().filter((c) => c.isWithin(s, this)));
  }
  querySelector(selector) { return this.descendants().find((n) => this.querySelectorAll(selector).includes(n)) || null; }
}
function commandDom(commands) {
  const h = commandHarness(commands);
  const doc = h.document;
  doc.body = new FakeNode(doc, "body");
  doc.activeElement = doc.body;
  doc.createElement = (tag) => new FakeNode(doc, tag);
  doc.querySelectorAll = (selector) => doc.body.querySelectorAll(selector);
  const part = (tag, className, parent) => { const n = new FakeNode(doc, tag); n.className = className; parent.append(n); return n; };
  h.context.card = (id, kind = "job") => {
    const row = new FakeNode(doc, kind === "job" ? "article" : "div");
    row.className = `${kind}-row`;
    row.dataset.operationId = id;
    if (kind === "job") {
      part("button", "job-title", part("h3", "job-heading", row));
      part("div", "job-footer", row);
    } else {
      part("div", "backup-heading", row);
      part("button", "", part("div", "backup-actions", row)).textContent = "View job";
    }
    doc.body.append(row);
    return row;
  };
  return h;
}
const clickAndSettle = async (button) => { button.listeners.get("click")(); for (let i = 0; i < 5; i++) await Promise.resolve(); };

test("keyboard focus on Dismiss survives redraws and moves to the card when the line goes", async () => {
  const h = commandDomFixture();
  const doc = h.document;
  h.run('card("job-a");card("job-b");renderCommandIndicators();');
  const firstDismiss = doc.body.querySelector(".command-dismiss");
  assert.equal(firstDismiss.closest(".command-failure").dataset.commandId, "fa");
  firstDismiss.focus();
  // A status tick of another job's command leaves the failure line in place.
  h.run('state.commands.get("rb").status="pending";renderCommandIndicators();');
  assert.equal(doc.activeElement, firstDismiss);
  // A queue refresh rebuilds every card; focus moves to the new Dismiss button.
  h.run('{const focus=commandFocus();document.body.replaceChildren();card("job-a");card("job-b");renderCommandIndicators();restoreCommandFocus(focus);}');
  assert.notEqual(doc.activeElement, firstDismiss);
  assert.equal(doc.activeElement.className, "command-dismiss");
  assert.equal(doc.activeElement.closest(".command-failure").dataset.commandId, "fa");
  assert.equal(doc.activeElement.closest("[data-operation-id]").dataset.operationId, "job-a");
  await clickAndSettle(doc.activeElement);
  assert.equal(doc.body.querySelector(".command-failure"), null);
  assert.equal(doc.activeElement.className, "job-title");
  assert.equal(doc.activeElement.closest("[data-operation-id]").dataset.operationId, "job-a");
});
function commandDomFixture() {
  return commandDom([
    {command_id:"fa", id:"job-a", kind:"retry", label:"Retrying job", status:"failed", error:"No worker was free.", started:2, seenAt:0},
    {command_id:"rb", id:"job-b", kind:"reconfigure", label:"Updating batch settings", status:"running", started:1, seenAt:0},
  ]);
}

test("dismissing a failure on a recovery copy keeps focus on that row", async () => {
  const h = commandDom([{command_id:"fc", id:"copy-a", kind:"clean", label:"Verifying recovery data", status:"failed", error:"The sizes differ.", started:1, seenAt:0}]);
  const doc = h.document;
  h.run('card("copy-a","backup");renderCommandIndicators();');
  const row = doc.body.querySelector("[data-operation-id]");
  const chip = row.querySelector(".command-chip");
  assert.equal(chip.parent.className, "backup-heading");
  assert.equal(chip.children.at(-1).textContent, "Check failed");
  const dismiss = row.querySelector(".command-dismiss");
  dismiss.focus();
  await clickAndSettle(dismiss);
  assert.equal(row.querySelector(".command-failure"), null);
  assert.equal(doc.activeElement.textContent, "View job");
});

test("the cleanup notice on a recovery copy replaces its in-flight chip", () => {
  const h = commandDom([{command_id:"cc", id:"copy-b", kind:"clean", label:"Verifying recovery data", status:"running", started:1, seenAt:0}]);
  h.run('card("copy-b","backup");renderCommandIndicators();');
  assert.ok(h.document.body.querySelector(".command-chip"));
  h.run('{const row=document.querySelectorAll("[data-operation-id]")[0];const notice=document.createElement("p");notice.className="job-command";row.append(notice);renderCommandIndicators();}');
  assert.equal(h.document.body.querySelector(".command-chip"), null);
});

test("redraws keep a single Applied expiry timer", () => {
  const h = commandDom([{command_id:"ac", id:"job-c", kind:"resume", label:"Applying decision", status:"completed", started:1, seenAt:0}]);
  const pending = new Map();
  let next = 0;
  h.context.setTimeout = (fn) => { pending.set(++next, fn); return next; };
  h.context.clearTimeout = (id) => pending.delete(id);
  h.run('state.commands.get("ac").seenAt=commandClock();card("job-c");');
  for (let i = 0; i < 4; i++) h.run('renderCommandIndicators()');
  assert.equal(pending.size, 1);
  assert.equal(h.document.body.querySelector(".command-chip").className, "command-chip applied");
  h.run('state.commands.get("ac").seenAt=commandClock()-9000;');
  [...pending.values()][0]();
  assert.equal(h.document.body.querySelector(".command-chip"), null);
  assert.equal(pending.size, 0);
});

test("job detail lists this session's commands newest first and hides when there are none", () => {
  const now = 3_000_000;
  const h = commandHarness([
    {command_id:"m1", id:"job-m", kind:"resume", label:"Applying decision", status:"completed", started:now - 300_000},
    {command_id:"m3", id:"job-m", kind:"reconfigure", label:"Updating batch settings", status:"waiting_external", waiting_reason:"Waiting for a free worker", started:now - 5_000},
    {command_id:"m2", id:"job-m", kind:"retry", label:"Retrying job", status:"failed", error:"The worker went offline.", started:now - 120_000},
    {command_id:"x1", id:"job-other", kind:"retry", label:"Retrying job", status:"running", started:now - 1_000},
  ]);
  h.run(`state.detail="job-m";renderCommandHistory(${now});`);
  assert.equal(h.elements.get("detail-commands").hidden, false);
  const items = h.elements.get("detail-command-list").children;
  assert.deepEqual(items.map((li) => li.className), ["command-history-item active", "command-history-item failed", "command-history-item applied"]);
  const text = (el) => [el.textContent, ...el.children.map(text)].join("");
  assert.deepEqual(items.map(text), [
    "Updating batch settings · Waiting to applyWaiting for a free workerJust now",
    "Retrying job · FailedThe worker went offline.2 min ago",
    "Applying decision · Applied5 min ago",
  ]);
  h.run(`state.detail="job-empty";renderCommandHistory(${now});`);
  assert.equal(h.elements.get("detail-commands").hidden, true);
  assert.equal(h.elements.get("detail-command-list").children.length, 0);
});

test("a queue refresh keeps keyboard focus on a job title, also after Dismiss moved it there", async () => {
  const h = commandDom([{command_id:"ft", id:"job-a", kind:"retry", label:"Retrying job", status:"failed", error:"No worker was free.", started:1, seenAt:0}]);
  const doc = h.document;
  const list = new FakeNode(doc);
  doc.body.append(list);
  h.elements.set("jobs-list", list);
  h.context.request = (path) => path === "worker-activity" ? {available:false} : {
    jobs:[{id:"job-a", number:1, action_name:"transcode_media", status:"failed"}, {id:"job-b", number:2, action_name:"transcode_media", status:"completed"}],
    total:2, has_more:false,
  };
  h.run('api=async(path)=>request(path);renderSavings=()=>{};state.tab="jobs";');
  await h.run("loadJobs()");
  const titleOf = (id) => list.querySelectorAll("[data-operation-id]").find((r) => r.dataset.operationId === id).querySelector(".job-title");
  const before = titleOf("job-b");
  before.focus();
  await h.run("loadJobs()");
  assert.notEqual(titleOf("job-b"), before);
  assert.equal(doc.activeElement, titleOf("job-b"));
  // Dismiss on a failure hands focus to the title; the next refresh keeps it there.
  list.querySelector(".command-dismiss").focus();
  await clickAndSettle(doc.activeElement);
  assert.equal(doc.activeElement, titleOf("job-a"));
  await h.run("loadJobs()");
  assert.equal(doc.activeElement, titleOf("job-a"));
});

test("a goal batch that is still encoding offers its waiting decision, and the filter counts every decision", async () => {
  const h = commandDom([]);
  const doc = h.document;
  const list = new FakeNode(doc);
  doc.body.append(list);
  h.elements.set("jobs-list", list);
  h.context.request = (path) => path === "worker-activity" ? {available:false} : {
    jobs:[
      {id:"waiting", number:1, action_name:"transcode_batch", status:"waiting_decision", needs_decision:true},
      {id:"encoding", number:2, action_name:"transcode_batch", status:"waiting_external", needs_decision:true, batch:{total:3, running:1, waiting_decision:1}},
      {id:"quiet", number:3, action_name:"transcode_media", status:"completed"},
    ],
    total:3, has_more:false, decision_count:2,
  };
  h.run('api=async(path)=>request(path);renderSavings=()=>{};state.tab="jobs";');
  await h.run("loadJobs()");
  assert.equal(h.elements.get("job-filter-decision").textContent, "Needs decision (2)");
  const row = (id) => list.querySelectorAll("[data-operation-id]").find((r) => r.dataset.operationId === id);
  // The flag is not a button: the sentence under the title carries the way in.
  assert.equal(row("encoding").querySelectorAll(".status-action").length, 0);
  assert.deepEqual(row("encoding").querySelectorAll(".action-main").map((b) => b.textContent), ["Review"]);
  assert.equal(row("quiet").querySelectorAll(".action-main").length, 0);
  assert.equal(list.querySelectorAll(".decision-badge").length, 1);
  h.context.request = () => ({jobs:[], total:0, has_more:false, decision_count:0});
  await h.run("loadJobs()");
  assert.equal(h.elements.get("job-filter-decision").textContent, "Needs decision");
});

test("the decision flag is a small icon on the title line, not a second badge row", () => {
  assert.match(css, /\.job-heading > \.status-badge\.decision-badge \{[^}]*grid-column: 2;[^}]*grid-row: 1;[^}]*width: 20px;[^}]*height: 20px;[^}]*min-height: 0;/);
  assert.match(css, /\.job-heading > \.decision-badge > span:not\(\.status-symbol\) \{ display: none; \}/);
  assert.match(css, /\.job-heading:has\(> \.decision-badge\) > \.job-title \{ padding-right: 26px; \}/);
  // With a command chip the status badge owns column 4; the flag must not take its cell.
  assert.match(css, /\.job-heading:has\(> \.command-chip\) > \.badge:not\(\.decision-badge\) \{ grid-column: 4; grid-row: 1; \}/);
  // On a phone the chip shares the status badge's row; the decision icon keeps the title's right edge.
  assert.match(css, /\.job-row > \.job-heading:has\(> \.command-chip\) > \.status-badge\.decision-badge \{ grid-column: 2 \/ -1; \}/);
  assert.doesNotMatch(css, /\.decision-badge \{[^}]*grid-row: [23]/);
  // Explicitly placing the flag would otherwise push the title and status badge out of their cells.
  assert.match(css, /\.job-heading:has\(> \.decision-badge\) > \.job-title \{ grid-column: 2; grid-row: 1; \}/);
  assert.match(css, /\.job-heading:has\(> \.decision-badge\) > \.status-badge:not\(\.decision-badge\) \{ grid-column: 3; grid-row: 1; \}/);
});

test("a recovery copy redraw keeps focus on its button, or the first enabled one while it verifies", () => {
  const h = commandDom([]);
  const doc = h.document;
  const list = new FakeNode(doc);
  doc.body.append(list);
  h.elements.set("backup-list", list);
  h.run('state.backups=[{action_id:"copy-a",bytes:1,delete_available:true,cleanup_available:true},{action_id:"copy-b",bytes:1,delete_available:true}];renderBackups();');
  const buttonsOf = (id) => list.querySelectorAll("[data-operation-id]").find((r) => r.dataset.operationId === id).querySelectorAll(".backup-actions button");
  const remove = buttonsOf("copy-b").find((b) => b.textContent === "Remove copy");
  remove.focus();
  h.run("renderBackups()");
  assert.notEqual(doc.activeElement, remove);
  assert.equal(doc.activeElement, buttonsOf("copy-b").find((b) => b.textContent === "Remove copy"));
  // A button that appears ahead of the focused one does not take its focus.
  h.run('state.backups[1].cleanup_available=true;renderBackups();');
  assert.equal(buttonsOf("copy-b").length, 3);
  assert.equal(doc.activeElement, buttonsOf("copy-b").find((b) => b.textContent === "Remove copy"));
  buttonsOf("copy-a").find((b) => b.textContent === "Verify & clean up").focus();
  h.run('state.backupCleaning={id:"copy-a"};renderBackups();');
  assert.equal(doc.activeElement, buttonsOf("copy-a")[0]);
  assert.equal(doc.activeElement.textContent, "View job");
});

test("only a command that is still waiting shows its waiting reason", () => {
  const h = commandDom([]);
  const timers = [];
  h.context.setTimeout = (fn) => timers.push(fn);
  h.run('state.operationJobs=new Map([["w",{id:"job-w",number:7}]]);card("job-w");');
  h.context.receipt = {key:"k", started:1, command_id:"w1", id:"job-w", label:"Retrying job"};
  h.run('trackCommand({kind:"retry"},receipt,{status:"waiting_external",waiting_reason:"Waiting for a free worker slot"})');
  const chip = () => h.document.body.querySelector(".command-chip");
  assert.equal(chip().title, "Retrying job · Waiting to apply · Waiting for a free worker slot");
  // A new reason for the same status replaces the stale tooltip.
  h.run('trackCommand({kind:"retry"},receipt,{status:"waiting_external",waiting_reason:"Waiting for the worker to come back"})');
  assert.equal(chip().title, "Retrying job · Waiting to apply · Waiting for the worker to come back");
  timers.length = 0;
  // A reason that outlives the wait is not read out.
  h.run('trackCommand({kind:"retry"},receipt,{status:"completed",waiting_reason:"Waiting for the worker to come back"})');
  timers.shift()();
  assert.equal(h.elements.get("command-announcer").textContent, "Job #7: Retrying job, applied.");
  h.run('state.detail="job-w";renderCommandHistory();');
  const text = (el) => [el.textContent, ...el.children.map(text)].join("");
  assert.doesNotMatch(text(h.elements.get("detail-command-list")), /Waiting for/);
  // Nor while it runs or is pending after the wait.
  for (const status of ["running", "pending"]) {
    h.context.status = status;
    h.run('state.commands.get("w1").status=status;');
    assert.doesNotMatch(h.run('commandChip("job-w").title'), /Waiting for/);
  }
});

test("a single job waiting for a full worker shows a slot wait, never a failure", () => {
  const h = harness();
  for (const job of [
    {action_name:"transcode_media",status:"waiting_external",waiting_condition:"worker_busy",waiting_reason:"Waiting for a free worker slot to start the benchmark",source_path:"/media/Sample Film One (2010).mkv"},
    // A finished benchmark leaves telemetry behind while the encode waits.
    {action_name:"transcode_media",status:"waiting_external",waiting_condition:"worker_busy",source_path:"/media/Sample Film One (2010).mkv",worker:{benchmark_status:"completed",benchmark_phase:"completed"}},
    {action_name:"benchmark_transcode",status:"waiting_external",waiting_condition:"worker_busy",source_path:"/media/Sample Film One (2010).mkv"},
  ]) {
    h.context.job = job;
    const view = h.run("queuePresentation(job)");
    assert.equal(view.status, "Waiting for a free worker slot");
    assert.notEqual(h.run("statusClass(queuePresentation(job).status)"), "failed");
    assert.equal(view.showTelemetry, true);
    assert.match(h.run("telemetry(job)").children[0].textContent, /continues automatically when a slot is free/);
  }
  // A batch keeps its own slot counts and the generic waiting label.
  h.context.job = {action_name:"transcode_batch",status:"waiting_external",waiting_condition:"worker_busy",batch:{total:3,running:2,waiting_for_slot:1}};
  assert.equal(h.run("queuePresentation(job).status"), "Waiting");
});

test("a file waiting in the coordinator's slot queue shows its place, not the last preflight phase", () => {
  const h = harness();
  const job = position => ({activities:[{id:"child",waiting_condition:"worker_busy",work:{phase:"waiting_for_worker",queue_position:position},worker:{transcode_phase:"completed"}}]});
  h.context.waitingJob = job;
  assert.equal(h.run('batchFileActivity({status:"waiting_for_slot",child_action_id:"child"},waitingJob(3))'), "Waiting for a worker slot · 2 ahead in line");
  assert.equal(h.run('batchFileActivity({status:"waiting_for_slot",child_action_id:"child"},waitingJob(1))'), "Waiting for a worker slot · Next in line");
  assert.equal(h.run('batchFileActivity({status:"waiting_for_slot",child_action_id:"child"},waitingJob(undefined))'), "Waiting for a worker slot");
  assert.equal(h.run('workerPhaseLabel("waiting_for_worker")'), "Waiting for a worker slot");
  // Once the worker accepts the job the phase is gone and worker progress shows.
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},{activities:[{id:"child",worker:{transcode_phase:"validating_quality"}}]})'), "Measuring final quality");
  // A standalone job says how many are ahead of it.
  h.context.job = {action_name:"transcode_media",status:"waiting_external",waiting_condition:"worker_busy",work:{phase:"waiting_for_worker",queue_position:3}};
  assert.match(h.run("telemetry(job)").children[0].textContent, /2 jobs are ahead of it in line\./);
  h.context.job.work.queue_position = 1;
  assert.match(h.run("telemetry(job)").children[0].textContent, /It is next in line\./);
});

test("phase cost detail shows measured worker CPU time and keeps an older worker's unknown", () => {
  const h = harness();
  h.run('renderPhaseCosts($("detail-summary"),{phase_costs:{worker_encode:{duration_ms:500,attempts:1,active_compute_ms:1800},worker_probe:{duration_ms:40,attempts:1,active_compute_ms:null}}});');
  const rendered = JSON.stringify(h.elements.get("detail-summary").children);
  assert.match(rendered, /CPU time/);
  assert.match(rendered, /1800 ms/);
  assert.match(rendered, /Unknown/);
});

test("an old worker-busy failure reads plainly and hides the worker URL", () => {
  const h = harness();
  for (const error of [
    "benchmark_submit failed (worker_busy): transcode http POST http://worker.example.test/v1/benchmarks: status 429: worker busy: maximum parallel jobs (2) reached",
    "submit failed (worker_busy): transcode http POST http://worker.example.test/v1/jobs: status 409: worker busy: maximum parallel jobs (2) reached",
  ]) {
    h.context.job = {action_name:"transcode_media",status:"failed",error,source_path:"/media/Sample Film One (2010).mkv"};
    const view = h.run("queuePresentation(job)");
    assert.equal(view.status, "Failed");
    assert.match(view.summary, /The worker was full and nothing ran\. Retry when a slot is free\./);
    assert.doesNotMatch(view.summary, /http|429|409|example/);
    assert.equal(h.run("workerBusyFailure(job.error)"), true);
  }
  assert.equal(h.run('workerBusyFailure("ssh: handshake failed")'), false);
  assert.equal(h.run('shortJobReason("benchmark_submit failed (worker_busy): status 429")'), "The worker was full and nothing ran. Retry when a slot is free.");
});

test("a cancelled or no-winner standalone file offers Run again with a confirmation and a fresh key", async () => {
  const h = harness();
  h.run('state.workerInfo={ready:true}');
  const base = '{id:"old",action_name:"transcode_media",status:"completed",mode:"size",reason_code:"no_suitable_candidate",source_path:"/media/Show/S01E01.mkv",rerun:{source_path:"/media/Show/S01E01.mkv",mode:"size",replace_original:false,no_winner_rounds:1}}';
  const labels = (job) => h.run(`jobControls(${job}).children.map(b=>b.textContent)`);
  assert.ok(labels(base).includes("Run again"));
  assert.ok(labels(`{...${base},status:"cancelled",reason_code:"user_cancelled"}`).includes("Run again"));
  // Hidden for candidates, decisions, batch children, replacements and unsupported sources.
  for (const patch of ['candidate_ready:true', 'status:"waiting_decision"', 'parent_item_key:"epfile-1"', 'replacement_action_id:"r"', 'reason_code:"unsupported_source"', 'rerun:undefined'])
    assert.ok(!labels(`{...${base},${patch}}`).includes("Run again"), patch);
  // It needs a ready worker, like Retry.
  const control = h.run(`jobControls(${base}).children.find(b=>b.textContent==="Run again")`);
  assert.equal(control.dataset.requiresWorker, "true");
  h.run('state.workerInfo={ready:false}');
  assert.equal(h.run(`jobControls(${base}).children.find(b=>b.textContent==="Run again")`).disabled, true);
  h.run('state.workerInfo={ready:true}');

  const calls = [], reviews = [];
  h.context.calls = calls; h.context.reviews = reviews;
  h.run('api=async(path,body)=>{calls.push({path,body});return {id:"new"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};let n=0;submissionID=()=>"key-"+(++n);reviewAction=async(r)=>{reviews.push(r);return true};');
  await h.run(`rerunJob(${base})`);
  await h.run(`rerunJob(${base})`);
  assert.equal(calls.length, 2);
  assert.ok(calls.every(c => c.path === "rerun" && c.body.id === "old"));
  assert.notEqual(calls[0].body.key, calls[1].body.key);
  const text = (r) => r.content.map(n => n.textContent).join(" ");
  assert.match(text(reviews[0]), /S01E01\.mkv/);
  assert.match(text(reviews[0]), /Goal: Save space/);
  assert.match(text(reviews[0]), /Originals stay in place/);
  assert.doesNotMatch(text(reviews[0]), /already had/);

  // Two earlier no-winner rounds make another one an explicit decision; replacing originals drops the line.
  const third = base.replace('no_winner_rounds:1', 'no_winner_rounds:2').replace('replace_original:false', 'replace_original:true');
  await h.run(`rerunJob(${third})`);
  assert.match(text(reviews[2]), /This file already had 2 rounds without a result/);
  assert.doesNotMatch(text(reviews[2]), /Originals stay in place/);
  assert.match(text(reviews[2]), /original is replaced only after you review and approve it/);

  // A lost response followed by another click replays the same key; success starts a new one.
  const sent = [];
  h.context.sent = sent;
  h.run('let lose=true;api=async(path,body)=>{sent.push(body.key);if(lose){lose=false;throw new Error("Lost reply")}return {id:"new"}}');
  await assert.rejects(h.run(`rerunJob(${base})`), /Lost reply/);
  await h.run(`rerunJob(${base})`);
  await h.run(`rerunJob(${base})`);
  assert.equal(sent[0], sent[1]);
  assert.notEqual(sent[1], sent[2]);

  // Declining the confirmation sends nothing.
  h.run('reviewAction=async()=>false');
  await h.run(`rerunJob(${base})`);
  assert.equal(calls.length, 3);
  assert.equal(h.run('state.rerunKeys.has("old")'), false);
});

test("a VideoToolbox quality ladder whose every step missed offers Try with x265", async () => {
  const h = harness();
  h.run('state.workerInfo={ready:true}');
  const base = '{id:"old",action_name:"transcode_media",status:"completed",mode:"size",reason_code:"quality_not_met",source_path:"/media/Show/S01E01.mkv",rerun:{source_path:"/media/Show/S01E01.mkv",mode:"size",replace_original:false,no_winner_rounds:1,without_hardware:true}}';
  const labels = (job) => h.run(`jobControls(${job}).children.map(b=>b.textContent)`);
  assert.ok(labels(base).includes("Try with x265"));
  assert.ok(labels(base).includes("Run again"));
  assert.ok(!labels(base.replace(",without_hardware:true", "")).includes("Try with x265"));
  for (const patch of ['candidate_ready:true', 'status:"waiting_decision"', 'parent_item_key:"epfile-1"', 'replacement_action_id:"r"', 'rerun:undefined'])
    assert.ok(!labels(`{...${base},${patch}}`).includes("Try with x265"), patch);
});

test("a job whose settings ran the removed search offers the goal form instead of Run again", async () => {
  const h = harness();
  h.run('state.workerInfo={ready:true}');
  const why = "This job's settings ran the sample search, which was removed, so it cannot run again as it was. Choose a goal for the file instead.";
  const base = `{id:"old",action_name:"transcode_media",status:"completed",reason_code:"no_suitable_candidate",source_path:"/media/Show/S01E01.mkv",rerun_refused:${JSON.stringify(why)}}`;
  const labels = (job) => h.run(`jobControls(${job}).children.map(b=>b.textContent)`);
  assert.ok(labels(base).includes("Choose a goal"));
  assert.ok(!labels(base).includes("Run again"));
  const choose = h.run(`jobControls(${base}).children.find(b=>b.textContent==="Choose a goal")`);
  assert.equal(choose.title, why);
  assert.equal(choose.dataset.requiresWorker, "false");
  for (const patch of ['candidate_ready:true', 'status:"waiting_decision"', 'parent_item_key:"epfile-1"', 'replacement_action_id:"r"', 'rerun_refused:undefined'])
    assert.ok(!labels(`{...${base},${patch}}`).includes("Choose a goal"), patch);
  h.run('selectTab=()=>{};setFileStep=()=>{};controls=()=>{}');
  await choose.listeners.get("click")();
  assert.equal(h.run('$("path").value'), "/media/Show/S01E01.mkv");
  assert.equal(h.run('$("scope").value'), "file");
});

test("a standalone file that missed its size limit offers the goal form its next step names", async () => {
  const h = harness();
  h.run('state.workerInfo={ready:true}');
  const base = '{id:"old",action_name:"transcode_media",status:"completed",mode:"size",reason_code:"insufficient_savings",source_path:"/media/Show/S01E01.mkv",benchmark:{outcome:"size_rejected",user_next_step:"Keep the original, or choose another goal."}}';
  const labels = (job) => h.run(`jobControls(${job}).children.map(b=>b.textContent)`);
  assert.ok(labels(base).includes("Choose a goal"));
  assert.ok(!labels(base).includes("Run again"));
  // The server offers the same goal again too, and both stay.
  const again = labels(`{...${base},rerun:{source_path:"/media/Show/S01E01.mkv",mode:"size",replace_original:false,no_winner_rounds:1}}`);
  assert.ok(again.includes("Run again") && again.includes("Choose a goal"));
  // Reviewed, batch-owned or already offered another way: nothing doubles up.
  for (const patch of ['candidate_ready:true', 'status:"failed"', 'parent_item_key:"epfile-1"', 'replacement_action_id:"r"', 'rerun_refused:"why"', 'benchmark:{outcome:"no_suitable_candidate"}'])
    assert.equal(labels(`{...${base},${patch}}`).filter((l) => l === "Choose a goal").length, patch.startsWith("rerun_refused") ? 1 : 0, patch);
  h.run('selectTab=()=>{};setFileStep=()=>{};controls=()=>{}');
  await h.run(`jobControls(${base}).children.find(b=>b.textContent==="Choose a goal")`).listeners.get("click")();
  assert.equal(h.run('$("path").value'), "/media/Show/S01E01.mkv");
  assert.equal(h.run('$("scope").value'), "file");
});

test("a media engine budget encode below the quality floor offers Try with x265, which reruns with the engine off", async () => {
  const h = harness();
  h.run('state.workerInfo={ready:true}');
  const base = '{id:"old",action_name:"transcode_media",status:"completed",mode:"size",reason_code:"quality_not_met",source_path:"/media/Show/S01E01.mkv",rerun:{source_path:"/media/Show/S01E01.mkv",mode:"size",replace_original:false,no_winner_rounds:1},size_mode:{encoder:"hevc_videotoolbox",hardware:true,size_budget:{budget_bytes:4000000000,outcome:"below_quality_floor",retry_without_hardware:true}}}';
  const labels = (job) => h.run(`jobControls(${job}).children.map(b=>b.textContent)`);
  assert.ok(labels(base).includes("Try with x265"));
  // Only the below-floor record offers it, and never for a reviewed or batch-owned job.
  assert.ok(!labels(base.replace("retry_without_hardware:true", "retry_without_hardware:false")).includes("Try with x265"));
  for (const patch of ['candidate_ready:true', 'status:"waiting_decision"', 'parent_item_key:"epfile-1"', 'replacement_action_id:"r"', 'rerun:undefined'])
    assert.ok(!labels(`{...${base},${patch}}`).includes("Try with x265"), patch);
  assert.equal(h.run(`jobControls(${base}).children.find(b=>b.textContent==="Try with x265")`).dataset.requiresWorker, "true");

  const calls = [], reviews = [];
  h.context.calls = calls; h.context.reviews = reviews;
  h.run('api=async(path,body)=>{calls.push({path,body});return {id:"new"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};let n=0;submissionID=()=>"key-"+(++n);reviewAction=async(r)=>{reviews.push(r);return true};');
  await h.run(`rerunJob(${base}, {withoutHardware: true})`);
  await h.run(`rerunJob(${base})`);
  assert.equal(calls[0].body.id, "old");
  assert.equal(calls[0].body.key, "key-1");
  assert.equal(calls[0].body.without_hardware, true);
  assert.equal(calls[1].body.without_hardware, undefined, "a plain Run again keeps the engine choice");
  const text = (r) => r.content.map(n => n.textContent).join(" ");
  assert.match(text(reviews[0]), /encodes with x265: slower, but it usually keeps more quality at the same size/);
  assert.equal(reviews[0].confirmLabel, "Try with x265");
  // The ladder's over-budget steps read as steps, not as searched settings.
  assert.match(h.run('sizeBudgetPointLabel({kind:"budget_rung_over",video_codec:"hevc_videotoolbox",quality:65,bytes:4100000000,measured:true,within_budget:false})'), /^VideoToolbox quality 65 · 4\.10 GB · VMAF not measured yet · over budget$/);
  assert.match(h.run('sizeBudgetPointLabel({kind:"budget_rung_over",video_codec:"hevc_videotoolbox",bitrate_kbps:4700,bytes:4100000000,measured:true,within_budget:false})'), /^VideoToolbox average bitrate 4700 kbps · /);
});

// ---- Size budget ----

test("the smaller files goal offers an optional size budget in both forms", () => {
  const form = markup.slice(markup.indexOf('<form id="job-form"'), markup.indexOf("</form>", markup.indexOf('<form id="job-form"')));
  for (const prefix of ["size-budget", "reconfigure-budget"]) {
    assert.match(markup, new RegExp(`id="${prefix}-kind"`));
    assert.match(markup, new RegExp(`id="${prefix}-percent"[^>]*min="5"[^>]*max="85"`));
    assert.match(markup, new RegExp(`id="${prefix}-gb"[^>]*min="0.05"`));
    assert.match(markup, new RegExp(`id="${prefix}-multiplier"[^>]*min="1"[^>]*max="4"[^>]*value="1.5"`));
  }
  assert.match(form, /id="size-budget-option"/);
  assert.match(markup, /Match other episodes/);
  const h = harness();
  const el = (id) => h.elements.get(id);
  h.run('processingMode=()=>"quality"; controls();');
  assert.equal(el("size-budget-option").hidden, true);
  h.run('processingMode=()=>"size"; controls();');
  assert.equal(el("size-budget-option").hidden, false);
  assert.equal(el("size-budget-percent-label").hidden, true);
  h.run('$("size-budget-kind").value="percent"; controls();');
  assert.equal(el("size-budget-percent-label").hidden, false);
  assert.equal(el("size-budget-gb-label").hidden, true);
  h.run('$("size-budget-kind").value="siblings"; controls();');
  assert.equal(el("size-budget-multiplier-label").hidden, false);
  assert.equal(typeof el("size-budget-kind").listeners.get("change"), "function");
});

test("the size budget is sent explicitly, one kind at a time, and checked before sending", async () => {
  const attempt = async (setup, mode = "size") => {
    const { h } = submissionHarness();
    let sent = null;
    h.context.capture = (_tool, args) => { sent = JSON.parse(args.inputs); return { id: "submitted" }; };
    h.run(`processingMode=()=>${JSON.stringify(mode)};tool=async(...args)=>capture(...args);${setup}`);
    await h.run('buildAndSubmitJob("encode")');
    return sent;
  };
  const refused = async (setup, message) => {
    const { h } = submissionHarness();
    let sent = null;
    h.context.capture = (_tool, args) => { sent = args; return { id: "submitted" }; };
    h.run(`processingMode=()=>"size";tool=async(...args)=>capture(...args);${setup}`);
    await assert.rejects(h.run('buildAndSubmitJob("encode")'), message);
    assert.equal(sent, null, "nothing is sent for an invalid budget");
  };
  let inputs = await attempt('$("size-budget-kind").value="";');
  assert.equal(["size_budget_percent", "size_budget_bytes", "size_budget_source", "size_budget_sibling_multiplier"].some((k) => k in inputs), false);
  inputs = await attempt('$("size-budget-kind").value="percent"; $("size-budget-percent").value="33"; $("size-budget-gb").value="2";');
  assert.equal(inputs.size_budget_percent, 33);
  assert.equal("size_budget_bytes" in inputs, false, "the hidden GB field must not leak");
  inputs = await attempt('$("size-budget-kind").value="gb"; $("size-budget-gb").value="1.5";');
  assert.equal(inputs.size_budget_bytes, 1500000000);
  assert.equal("size_budget_percent" in inputs, false);
  inputs = await attempt('$("size-budget-kind").value="siblings"; $("size-budget-multiplier").value="2";');
  assert.equal(inputs.size_budget_source, "siblings");
  assert.equal(inputs.size_budget_sibling_multiplier, 2);
  inputs = await attempt('$("size-budget-kind").value="siblings"; $("size-budget-multiplier").value="";');
  assert.equal(inputs.size_budget_sibling_multiplier, 1.5);
  for (const [setup, message] of [
    ['$("size-budget-kind").value="percent"; $("size-budget-percent").value="4";', /5 to 85/],
    ['$("size-budget-kind").value="percent"; $("size-budget-percent").value="90";', /5 to 85/],
    ['$("size-budget-kind").value="percent"; $("size-budget-percent").value="";', /5 to 85/],
    ['$("size-budget-kind").value="gb"; $("size-budget-gb").value="0.01";', /0\.05/],
    ['$("size-budget-kind").value="gb"; $("size-budget-gb").value="abc";', /0\.05/],
    ['$("size-budget-kind").value="siblings"; $("size-budget-multiplier").value="5";', /1\.0 to 4\.0/],
    ['$("size-budget-kind").value="siblings"; $("size-budget-multiplier").value="0.5";', /1\.0 to 4\.0/],
  ]) await refused(setup, message);
  // Other goals send no budget even if the fields still hold one.
  inputs = await attempt('$("size-budget-kind").value="percent"; $("size-budget-percent").value="33";', "quality");
  assert.equal("size_budget_percent" in inputs, false);
});

test("changing the goal shows the frozen size budget and sends the chosen one", async () => {
  const open = async (settings) => {
    const h = harness();
    h.context.savedPlan = { id: "b", mode: "quality", scope: "all", selected: 2, kept: 0, active: 0, files: ["one", "two"], selection_version: "v1", settings };
    h.run("state.info={};state.recipes=[];api=async()=>savedPlan;");
    await h.run('reconfigureBatch("b")');
    return h;
  };
  const el = (h, id) => h.elements.get(id);
  let h = await open({ size_budget_percent: 33 });
  assert.equal(el(h, "reconfigure-budget-kind").value, "percent");
  assert.equal(el(h, "reconfigure-budget-percent").value, 33);
  h = await open({ size_budget_bytes: 2000000000 });
  assert.equal(el(h, "reconfigure-budget-kind").value, "gb");
  assert.equal(el(h, "reconfigure-budget-gb").value, 2);
  h = await open({ size_budget_source: "siblings", size_budget_sibling_multiplier: 2 });
  assert.equal(el(h, "reconfigure-budget-kind").value, "siblings");
  assert.equal(el(h, "reconfigure-budget-multiplier").value, 2);
  h = await open({ size_budget_mixed: true });
  assert.equal(el(h, "reconfigure-budget-kind").value, "");
  h.run('$("reconfigure-mode").value="size"; updateReconfigureHardware();');
  assert.equal(el(h, "reconfigure-budget-option").hidden, false);
  assert.match(el(h, "reconfigure-settings").textContent, /different size budgets/);
  const calls = [];
  h.context.record = (path, body) => calls.push([path, body]);
  h.context.sessionStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  h.run('submissionID=()=>"budget-1";commandRequest=async(path,body)=>{record(path,body);return {id:"b"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};');
  await h.run("submitReconfiguredBatch()");
  assert.equal("size_budget_percent" in calls[0][1], false);
  h.run('$("reconfigure-budget-kind").value="percent"; $("reconfigure-budget-percent").value="40"; submissionID=()=>"budget-2";');
  await h.run("submitReconfiguredBatch()");
  assert.equal(calls[1][1].size_budget_percent, 40);
  h.run('$("reconfigure-budget-kind").value="siblings"; $("reconfigure-budget-multiplier").value="9"; submissionID=()=>"budget-3";');
  await assert.rejects(h.run("submitReconfiguredBatch()"), /1.0 to 4.0/);
  h.run('$("reconfigure-mode").value="compact"; submissionID=()=>"budget-4";');
  await h.run("submitReconfiguredBatch()");
  assert.equal("size_budget_source" in calls[2][1], false);
});

const budgetRecord = {
  trigger: "passing_candidate_over_budget", source_bytes: 9000000000, budget_bytes: 3000000000, budget_percent: 33, target_bytes: 2910000000, video_bitrate_kbps: 2400,
  over_budget_candidate_id: "cand_crf24", over_budget_estimated_bytes: 4000000000,
  quality_target: { vmaf_mean_minimum: 88 }, quality_floor: { vmaf_mean_minimum: 70, vmaf_p5_minimum: 60, vmaf_worst_window_minimum: 65 },
  message: "Below the quality target, within your size budget.",
  outcome: "below_quality_target_within_budget", candidate_bytes: 2850000000, below_target: ["VMAF mean 79.5 is below the target 88.0"],
  curve: [
    { kind: "searched", candidate_id: "cand_crf24", video_codec: "libx265", bytes: 4000000000, passed: true, within_budget: false, vmaf_mean: 90 },
    { kind: "searched", candidate_id: "cand_crf30", video_codec: "libx265", bytes: 2500000000, passed: false, within_budget: true, vmaf_mean: 76 },
    { kind: "budget_fallback", bytes: 2850000000, measured: true, within_budget: true, vmaf_mean: 79.5 },
  ],
};

test("a size budget result states the outcome, the budget against the result and every point", () => {
  const h = harness();
  h.context.sizeMode = { encoder: "libx265", expected_bytes: 2850000000, size_budget: budgetRecord };
  const lines = h.run("sizeModeLines(sizeMode)");
  assert.ok(lines.includes("Expected size 2.85 GB"));
  assert.ok(lines.includes("Encoder: x265 (software)"));
  assert.ok(lines.includes("Below the quality target, within your size budget."));
  assert.ok(lines.some((l) => /Size budget 3\.00 GB \(33% of the source\) · aims at 2\.91 GB · video 2400 kbps/.test(l)));
  assert.ok(lines.some((l) => /Result 2\.85 GB of the 3\.00 GB budget \(95\.0%\)/.test(l)));
  assert.ok(lines.some((l) => /cand_crf24, about 4\.00 GB\) is larger than the budget/.test(l)));
  assert.ok(lines.some((l) => /Quality target VMAF mean ≥ 88 · floor 70 mean, 60 p5, 65 worst window/.test(l)));
  assert.ok(lines.some((l) => l === "Below target: VMAF mean 79.5 is below the target 88.0"));
  h.context.job = { size_mode: { encoder: "libx265", size_budget: budgetRecord } };
  const note = h.run("sizeModeNote(job)");
  const all = (n) => [n, ...(n.children || []).flatMap(all)];
  const kids = all(note);
  const badge = kids.find((c) => /size-budget-badge/.test(c.className || ""));
  assert.equal(badge.textContent, "Below quality target — within size budget");
  assert.equal(kids.some((c) => c.tagName === "svg"), false, "no chart without SVG support");
  const points = kids.filter((c) => /size-budget-points/.test(c.className || ""));
  assert.equal(points.length, 1, "the plain-text point list is the fallback");
  const items = all(points[0]).filter((c) => c.textContent && /VMAF/.test(c.textContent) && !c.children?.length).map((c) => c.textContent);
  assert.ok(items.some((t) => /Budget encode · x265 two-pass · 2\.85 GB · VMAF 79\.5 · within budget/.test(t)));
  assert.ok(items.some((t) => /cand_crf24 · est\. 4\.00 GB · VMAF 90\.0 · met the quality target · over budget/.test(t)));
  assert.ok(items.some((t) => /cand_crf30 .* missed the quality target · within budget/.test(t)));
});

test("a size budget draws a compact chart when the browser can", () => {
  const h = harness();
  class Svg extends Element {
    constructor(tagName) { super(); this.tagName = tagName; this.attrs = {}; }
    setAttribute(name, value) { this.attrs[name] = value; if (name === "class") this.className = value; }
  }
  h.document.createElementNS = (_ns, name) => new Svg(name);
  h.context.record = budgetRecord;
  const svg = h.run("sizeBudgetChart(record)");
  const all = (n) => [n, ...(n.children || []).flatMap(all)];
  const parts = all(svg);
  assert.equal(svg.tagName, "svg");
  assert.match(svg.attrs["aria-label"], /3 points, budget 3\.00 GB/);
  assert.equal(parts.filter((c) => c.className === "point passed").length, 1);
  assert.equal(parts.filter((c) => c.className === "point").length, 1);
  assert.equal(parts.filter((c) => c.className === "point fallback").length, 1);
  assert.equal(parts.filter((c) => c.className === "budget-line").length, 1);
  assert.equal(parts.filter((c) => c.className === "target-line").length, 1);
  const budgetX = Number(parts.find((c) => c.className === "budget-line").attrs.x1);
  const fallbackX = Number(parts.find((c) => c.className === "point fallback").attrs.cx);
  const targetY = Number(parts.find((c) => c.className === "target-line").attrs.y1);
  const fallbackY = Number(parts.find((c) => c.className === "point fallback").attrs.cy);
  assert.ok(fallbackX < budgetX, "the 2.85 GB result sits left of the 3 GB budget line");
  assert.ok(fallbackY > targetY, "a VMAF of 79.5 is below the 88 target line");
  assert.equal(h.run("sizeBudgetChart({budget_bytes: 1, curve: []})"), null);
});

test("a VideoToolbox budget encode shows the encoder comparison, the choice and the bitrate correction", () => {
  const h = harness();
  const vt = {
    ...budgetRecord,
    encoder_choice: { video_codec: "hevc_videotoolbox", reason: "videotoolbox_within_tolerance", message: "Within 1.0 VMAF of x265." },
    bitrate_correction: { first_job_id: "job-1", first_candidate_bytes: 3200000000, first_bitrate_kbps: 2400, bitrate_kbps: 2190 },
    curve: [
      budgetRecord.curve[0],
      { kind: "budget_measured", candidate_id: "budget-vt", video_codec: "hevc_videotoolbox", bitrate_kbps: 2400, bytes: 2950000000, within_budget: true, vmaf_mean: 80.1, vmaf_p5: 71.2, chosen: true },
      { kind: "budget_measured", candidate_id: "budget-x265", video_codec: "libx265", bitrate_kbps: 2400, bytes: 2900000000, within_budget: true, vmaf_mean: 80.6, vmaf_p5: 71.9 },
      { kind: "budget_overshoot", video_codec: "hevc_videotoolbox", bitrate_kbps: 2400, bytes: 3200000000, measured: true, within_budget: false, vmaf_mean: 80.4 },
      { kind: "budget_fallback", video_codec: "hevc_videotoolbox", bitrate_kbps: 2190, bytes: 2850000000, measured: true, within_budget: true, vmaf_mean: 79.5 },
    ],
  };
  const lines = h.run(`sizeBudgetLines(${JSON.stringify(vt)})`);
  assert.ok(lines.includes("Encoder at the budget: VideoToolbox — Within 1.0 VMAF of x265."));
  assert.ok(lines.includes("VideoToolbox overshot at 2400 kbps (3.20 GB); encoded again at 2190 kbps"));
  const labels = vt.curve.map((p) => h.run(`sizeBudgetPointLabel(${JSON.stringify(p)})`));
  assert.match(labels[1], /^Compared at the budget · VideoToolbox 2400 kbps · chosen · est\. 2\.95 GB · VMAF 80\.1 \(p5 71\.2\) · within budget$/);
  assert.match(labels[2], /^Compared at the budget · x265 two-pass 2400 kbps · est\. /);
  assert.match(labels[3], /^First budget encode · VideoToolbox 2400 kbps · 3\.20 GB · VMAF 80\.4 · over budget$/);
  assert.match(labels[4], /^Budget encode · VideoToolbox 2190 kbps · 2\.85 GB/);
  class Svg extends Element {
    constructor(tagName) { super(); this.tagName = tagName; this.attrs = {}; }
    setAttribute(name, value) { this.attrs[name] = value; if (name === "class") this.className = value; }
  }
  h.document.createElementNS = (_ns, name) => new Svg(name);
  h.context.record = vt;
  const all = (n) => [n, ...(n.children || []).flatMap(all)];
  const parts = all(h.run("sizeBudgetChart(record)"));
  assert.equal(parts.filter((c) => c.className === "point compared chosen").length, 1);
  assert.equal(parts.filter((c) => c.className === "point compared").length, 1);
  assert.equal(parts.filter((c) => c.className === "point overshoot").length, 1);
  assert.equal(parts.filter((c) => c.className === "point fallback").length, 1);
});

test("a planned budget encode shows its target and an unmeasured point, not the over-budget winner", () => {
  const h = harness();
  const planned = { ...budgetRecord, outcome: "", candidate_bytes: 0, below_target: [], curve: [budgetRecord.curve[0], { kind: "budget_fallback", bytes: 2910000000, within_budget: true }] };
  h.context.job = { size_mode: { encoder: "libx265", expected_bytes: 2910000000, size_budget: planned } };
  const text = JSON.stringify(h.run("sizeModeLines(job.size_mode)"));
  assert.match(text, /Expected size 2\.91 GB/);
  assert.doesNotMatch(text, /VideoToolbox/);
  assert.doesNotMatch(text, /Result /);
  const list = JSON.stringify(h.run("job.size_mode.size_budget.curve.map(sizeBudgetPointLabel)"));
  assert.match(list, /planned 2\.91 GB · VMAF not measured yet · within budget/);
  assert.equal(h.run('sizeBudgetNote(job).children.some(c => /size-budget-badge/.test(c.className))'), false, "no outcome badge before the result");
});

test("a budget that was not applied says why", () => {
  const h = harness();
  h.context.job = { size_mode: { encoder: "libx265", size_budget_note: "Only 1 sibling." } };
  assert.ok(h.run("sizeModeLines(job.size_mode)").includes("Only 1 sibling."));
});

test("sibling-matched budgets name their basis, the file's ratio and the compared files", () => {
  const h = harness();
  const siblings = { scope: "season", median_bitrate_kbps: 1600, multiplier: 1.5, count: 5, resolution_class: "1080p", source_bitrate_kbps: 4800, ratio: 3, files: [
    { name: "E02.mkv", bytes: 700000000, duration_seconds: 2640, bitrate_kbps: 1600, width: 1920, height: 1080 },
    { name: "E03.mkv", bytes: 690000000, duration_seconds: 2600, bitrate_kbps: 1590, width: 1920, height: 1080 },
  ] };
  h.context.job = { size_mode: { encoder: "libx265", size_budget: { ...budgetRecord, siblings } } };
  const lines = h.run("sizeModeLines(job.size_mode)");
  assert.ok(lines.includes("Budget from 5 sibling episodes (median 1.60 Mbps × 1.5) in the season folder · 1080p"));
  assert.ok(lines.includes("This file averages 4.80 Mbps, 3.0× the siblings' median"));
  const note = h.run("sizeBudgetNote(job)");
  const all = (n) => [n, ...(n.children || []).flatMap(all)];
  const details = all(note).find((c) => /size-budget-siblings/.test(c.className || ""));
  assert.equal(details.open, false, "the file list stays collapsed");
  const rows = all(details).filter((c) => /E0\d\.mkv/.test(c.textContent || "") && !c.children?.length).map((c) => c.textContent);
  assert.deepEqual(rows, ["E02.mkv · 700.0 MB · 1.60 Mbps · 1920×1080 · 44 min", "E03.mkv · 690.0 MB · 1.59 Mbps · 1920×1080 · 43 min"]);
  assert.match(all(details)[1].textContent, /Sibling episodes \(2\)/);
});

test("the plan note and queue labels say a budget candidate is below the quality target", () => {
  const h = harness();
  h.context.job = { benchmark: { plan_source: "size_budget", plan_note: "x" }, size_mode: { size_budget: budgetRecord } };
  assert.match(allTextOf(h.run("encodingPlanNote(job)")), /Encoded at your size budget \(below the quality target\)\./);
  h.run('job.size_mode.size_budget = {...job.size_mode.size_budget, outcome:"meets_quality_target_within_budget"}');
  assert.match(allTextOf(h.run("encodingPlanNote(job)")), /\(within the quality target\)/);
  assert.equal(h.run('resultOutcomes.size_budget'), "Encoded at your size budget");
  assert.equal(h.run('belowTargetLabel("below_quality_target_within_budget")'), "Below quality target — within size budget");
  assert.equal(h.run('belowTargetLabel("meets_quality_target_within_budget")'), "");
  assert.equal(h.run('belowTargetNotice("")'), null);
  assert.match(allTextOf(h.run('belowTargetNotice("below_quality_target_within_budget")')), /Compare it with the original before approving/);
  h.context.ready = { candidate_ready: true, size_budget_outcome: "below_quality_target_within_budget" };
  assert.equal(h.run("queueOutcome(ready)"), "Original preserved · Candidate available · Below quality target — within size budget");
  h.run("ready.size_budget_outcome = undefined");
  assert.equal(h.run("queueOutcome(ready)"), "Original preserved · Candidate available");
  assert.equal(h.run('savingsLine({source_bytes: 9e9, estimated_saved_bytes: 6e9, estimate_kind: "size_budget"}).textContent').match(/Budget estimate/) !== null, true);
});
function allTextOf(n) {
  return [n.textContent, ...(n.children || []).map(allTextOf)].join(" ");
}

test("a size budget ladder run shows the full encode, not a result card", () => {
  const h = harness();
  h.context.job = {id:"j",action_name:"transcode_media",mode:"size",status:"waiting_external",transcode:{message:"Automatic testing and verification; originals are preserved."},
    benchmark:{outcome:"size_budget_ladder",message:"Encoding with VideoToolbox at quality 65 (step 1 of 4) to fit the 2.0 GB budget.",progress_scope:"Worker progress is for the full encode with VideoToolbox at quality 65 (step 1 of 4), not a sample test."}};
  assert.equal(h.run("resultReport(job)"), null, "no result card while encoding");
  assert.equal(h.run("jobOutcomeText(job)"), "Encoding with VideoToolbox at quality 65 (step 1 of 4) to fit the 2.0 GB budget.");
  assert.doesNotMatch(h.run("queueOutcome(job)"), /[Ss]ample/);
  // Other runs keep their wording.
  h.context.other = {status:"waiting_external",transcode:{message:"Automatic testing and verification; originals are preserved."},benchmark:{outcome:"testing"}};
  assert.equal(h.run("jobOutcomeText(other)"), h.run("shortJobReason(other.transcode.message)"));
});

test("a file skipped for its goal offers one button that starts a new job under the right goal", async () => {
  const h = harness();
  h.run('state.workerInfo={ready:true}');
  const skipped = (reason, mode, remedy) => `{id:"old",action_name:"transcode_media",status:"completed",mode:"${mode}",reason_code:"${reason}",source_path:"/media/movie.mkv",transcode:{message:""},rerun:{source_path:"/media/movie.mkv",mode:"${mode}",replace_original:false,no_winner_rounds:0,remedy_mode:"${remedy}"}}`;
  const hdr = skipped("hdr_requires_compact", "size", "compact");
  const sdr = skipped("compact_not_needed", "compact", "size");
  const labels = (job) => h.run(`jobControls(${job}).children.map(b=>b.textContent)`);
  assert.ok(labels(hdr).includes("Convert to SDR 1080p"));
  assert.ok(labels(sdr).includes("Make it smaller"));
  // The same wrong goal is never offered again, and the other remedy is not mixed in.
  assert.ok(!labels(hdr).includes("Run again") && !labels(sdr).includes("Run again"));
  assert.ok(!labels(hdr).includes("Make it smaller") && !labels(sdr).includes("Convert to SDR 1080p"));
  // Not for batch children, candidates, decisions, replacements, or jobs the server did not mark.
  for (const patch of ['parent_item_key:"epfile-1"', 'candidate_ready:true', 'status:"waiting_decision"', 'replacement_action_id:"r"', 'rerun:undefined'])
    assert.ok(!labels(`{...${hdr},${patch}}`).includes("Convert to SDR 1080p"), patch);
  const control = h.run(`jobControls(${hdr}).children.find(b=>b.textContent==="Convert to SDR 1080p")`);
  assert.equal(control.className, "primary");
  assert.equal(control.dataset.requiresWorker, "true");
  h.run('state.workerInfo={ready:false}');
  assert.equal(h.run(`jobControls(${hdr}).children.find(b=>b.textContent==="Convert to SDR 1080p")`).disabled, true);
  h.run('state.workerInfo={ready:true}');

  const calls = [], reviews = [];
  h.context.calls = calls; h.context.reviews = reviews;
  h.run('api=async(path,body)=>{calls.push({path,body});return {id:"new"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};let n=0;submissionID=()=>"key-"+(++n);reviewAction=async(r)=>{reviews.push(r);return true};');
  await h.run(`rerunJob(${hdr}, {mode:"compact"})`);
  await h.run(`rerunJob(${sdr}, {mode:"size"})`);
  assert.deepEqual(calls.map(c => [c.path, c.body.id, c.body.mode]), [["rerun", "old", "compact"], ["rerun", "old", "size"]]);
  assert.notEqual(calls[0].body.key, calls[1].body.key);
  const text = (r) => r.content.map(n => n.textContent).join(" ");
  assert.match(text(reviews[0]), /Converts to 1080p SDR and removes HDR\. At least 30% smaller\./);
  assert.match(text(reviews[0]), /Originals stay in place\./);
  assert.match(text(reviews[1]), /Keeps the look, at least 15% smaller\./);
  assert.equal(reviews[0].confirmLabel, "Convert to SDR 1080p");
  const replacing = hdr.replace("replace_original:false", "replace_original:true");
  await h.run(`rerunJob(${replacing}, {mode:"compact"})`);
  assert.match(text(reviews[2]), /The original is replaced only after you approve the result\./);
  // A plain Run again sends no mode.
  await h.run(`rerunJob(${hdr})`);
  assert.equal(calls[calls.length - 1].body.mode, undefined);
  // Declining sends nothing and frees the key.
  h.run('reviewAction=async()=>false');
  const before = calls.length;
  await h.run(`rerunJob(${hdr}, {mode:"compact"})`);
  assert.equal(calls.length, before);
  assert.equal(h.run('state.rerunKeys.has("old:compact")'), false);
});

test("a skipped file's reason appears once in its job details", async () => {
  for (const reason of ["hdr_requires_compact", "compact_not_needed"]) {
    const h = harness();
    h.context.reason = reason;
    h.run('api=async()=>({jobs:[{id:"skip",action_name:"transcode_media",status:"completed",mode:"size",reason_code:reason,source_path:"/media/movie.mkv",transcode:{message:""},savings:{source_bytes:63000000000},phase_costs:{worker_probe:{duration_ms:40,attempts:1}}}]});');
    await h.run('openJob("skip")');
    const text = h.run('shortJobReason(reason)');
    const rendered = JSON.stringify(h.elements.get("detail-summary").children);
    assert.equal(rendered.split(text).length - 1, 1, `${reason} repeated in ${rendered}`);
    assert.match(rendered, /63\.00 GB source/);
  }
});

// Goal step: the server reads the first chosen file and recommends a goal.
function goalProfileFixture(overrides = {}) {
  const skip = { status: "skip", reason_code: "hdr_requires_compact" };
  return {
    profile: { resolution: "4K", dynamic_range: "HDR10", video_codec: "HEVC", bit_depth: 10, size_bytes: 63e9 },
    goals: { size: skip, compact: { status: "ok" }, quality: skip, x265_preserve: skip },
    recommended: "compact",
    ...overrides,
  };
}
function goalHarness() {
  const h = harness({ radios: true });
  h.run('$("scope").value = "file"; $("service").value = "folder:/media"; $("path").value = "/media/movie.mkv";');
  const click = (value) => {
    h.radios.forEach((r) => (r.checked = r.value === value));
    h.radios.find((r) => r.value === value).listeners.get("change")();
  };
  const note = (mode) => h.elements.get(`goal-note-${mode}`);
  return { h, click, note, line: () => h.elements.get("source-profile"), mode: () => h.run("processingMode()"), more: () => h.run('$("more-goals")') };
}

test("the goal step preselects the recommended goal, labels it, and names why the others skip", async () => {
  const { h, note, line, mode, more } = goalHarness();
  h.context.profile = goalProfileFixture();
  h.run("var calls = []; api = async (path) => { calls.push(path); return profile; };");
  h.run("controls()");
  // While the file is read, the form's own default stays out of sight.
  assert.equal(h.run("state.goalProfile.status"), "loading");
  assert.equal(more().open, false);
  assert.equal(line().hidden, true);
  await h.run('loadGoalProfile("/media/movie.mkv")');
  assert.equal(h.run("calls.join()"), "source-profile?path=%2Fmedia%2Fmovie.mkv");
  assert.equal(mode(), "compact");
  assert.equal(line().hidden, false);
  assert.equal(line().textContent, "4K · HDR10 · HEVC · 10-bit · 63.00 GB");
  assert.equal(note("compact").textContent, "Recommended for this file");
  assert.equal(note("compact").hidden, false);
  assert.equal(note("size").textContent, "Skips this file: HDR source");
  assert.equal(note("quality").textContent, "Skips this file: HDR source");
  assert.equal(note("x265_preserve").hidden, false);
  assert.equal(h.radios.find((r) => r.checked).value, "compact");
});

test("the recommendation keeps More goals closed when it is outside it", async () => {
  const { h, mode, more, note } = goalHarness();
  h.context.profile = goalProfileFixture({
    profile: { resolution: "1080p", dynamic_range: "SDR", video_codec: "H.264", bit_depth: 8, size_bytes: 4e9 },
    goals: { size: { status: "ok" }, compact: { status: "skip", reason_code: "compact_not_needed" }, quality: { status: "ok" }, x265_preserve: { status: "ok" } },
    recommended: "size",
  });
  h.run("api = async () => profile; controls()");
  await h.run('loadGoalProfile("/media/movie.mkv")');
  assert.equal(mode(), "size");
  assert.equal(more().open, false);
  assert.equal(note("size").textContent, "Recommended for this file");
  assert.equal(note("compact").textContent, "Skips this file: already SDR at 1080p or smaller");
  assert.equal(note("quality").hidden, true);
});

test("More goals opens for a selected goal inside it", async () => {
  const { h, click, more } = goalHarness();
  h.run("api = async () => profile; controls()");
  h.context.profile = goalProfileFixture();
  await h.run('loadGoalProfile("/media/movie.mkv")');
  assert.equal(more().open, false);
  click("x265_preserve");
  assert.equal(more().open, true);
});

test("a goal the user chose before the profile arrives is never overridden", async () => {
  const { h, click, mode, more, note } = goalHarness();
  const pending = deferred();
  h.context.pending = pending.promise;
  h.run("api = async () => pending; controls()");
  const load = h.run('loadGoalProfile("/media/movie.mkv")');
  click("x265_preserve");
  assert.equal(more().open, true, "an explicit pick inside More goals opens it");
  pending.resolve(goalProfileFixture());
  await load;
  assert.equal(mode(), "x265_preserve");
  assert.equal(note("compact").textContent, "Recommended for this file", "the advice is still shown");
  assert.equal(h.radios.find((r) => r.checked).value, "x265_preserve");
});

test("a response for a file the user has since left is ignored", async () => {
  const { h, mode, line } = goalHarness();
  const first = deferred(), second = deferred();
  h.context.waits = { "/media/a.mkv": first.promise, "/media/b.mkv": second.promise };
  h.run('api = async (path) => waits[decodeURIComponent(path.split("=")[1])]; $("path").value = "/media/a.mkv"; controls();');
  const late = h.run('loadGoalProfile("/media/a.mkv")');
  h.run('$("path").value = "/media/b.mkv"; controls();');
  const current = h.run('loadGoalProfile("/media/b.mkv")');
  first.resolve(goalProfileFixture({ profile: { resolution: "720p", dynamic_range: "SDR", video_codec: "H.264", bit_depth: 8, size_bytes: 1e9 }, recommended: "x265_preserve" }));
  await late;
  assert.equal(h.run("state.goalProfile.key"), "/media/b.mkv");
  assert.equal(h.run("state.goalProfile.status"), "loading");
  assert.equal(line().hidden, true);
  assert.equal(mode(), "size");
  second.resolve(goalProfileFixture());
  await current;
  assert.equal(mode(), "compact");
  assert.match(line().textContent, /^4K · HDR10/);
});

test("changing the file starts a new visit, so the new recommendation applies again", async () => {
  const { h, click, mode } = goalHarness();
  h.context.profile = goalProfileFixture();
  h.run("api = async () => profile; controls()");
  await h.run('loadGoalProfile("/media/movie.mkv")');
  click("quality");
  assert.equal(h.run("state.goalExplicit"), true);
  h.run('$("path").value = "/media/other.mkv"; controls()');
  assert.equal(h.run("state.goalExplicit"), false);
  await h.run('loadGoalProfile("/media/other.mkv")');
  assert.equal(mode(), "compact");
});

test("a failed read leaves the goals usable with only a muted note", async () => {
  const { h, mode, line, more, note } = goalHarness();
  h.run('api = async () => { throw new Error("could not read this file"); }; controls();');
  await h.run('loadGoalProfile("/media/movie.mkv")');
  assert.equal(line().hidden, false);
  assert.equal(line().textContent, "Could not read this file");
  assert.equal(h.run("state.goalProfile.status"), "error");
  assert.equal(mode(), "size");
  assert.equal(more().open, false, "the default goal is already in view");
  for (const goal of ["size", "compact", "quality", "x265_preserve"]) assert.equal(note(goal).hidden, true);
  assert.equal(h.run("goalSkipWarning()"), "");
  // A malformed answer is treated the same way.
  h.run('api = async () => ({}); $("path").value = "/media/next.mkv"; controls();');
  await h.run('loadGoalProfile("/media/next.mkv")');
  assert.equal(h.run("state.goalProfile.status"), "error");
});

test("unticking the first file of a batch keeps the goal the user chose", async () => {
  const { h, click, mode } = goalHarness();
  h.context.profile = goalProfileFixture({ goals: { size: { status: "ok" }, compact: { status: "ok" }, quality: { status: "ok" }, x265_preserve: { status: "ok" } }, recommended: "size" });
  h.run('$("scope").value = "batch"; state.folderSelected = new Set(["/media/Show/a.mkv", "/media/Show/b.mkv", "/media/Show/c.mkv"]); api = async () => profile; controls();');
  await h.run('loadGoalProfile("/media/Show/a.mkv")');
  click("quality");
  h.run('state.folderSelected.delete("/media/Show/a.mkv"); controls();');
  assert.equal(h.run("state.goalProfile.key"), "/media/Show/b.mkv");
  assert.equal(h.run("state.goalExplicit"), true);
  await h.run('loadGoalProfile("/media/Show/b.mkv")');
  assert.equal(mode(), "quality");
  // Down to one file it is still the same visit.
  h.run('state.folderSelected.delete("/media/Show/b.mkv"); controls();');
  assert.equal(h.run("state.goalExplicit"), true);
  await h.run('loadGoalProfile("/media/Show/c.mkv")');
  assert.equal(mode(), "quality");
});

test("leaving a file cancels its read, and a busy server is asked once more", async () => {
  const { h, mode, line } = goalHarness();
  const signals = [];
  h.context.signals = signals;
  h.context.setTimeout = (fn) => { fn(); return 0; };
  h.context.answers = [Object.assign(new Error("busy reading another file; try again"), { status: 429 }), goalProfileFixture()];
  // The goal also reads the file's tracks; this test is about the profile read.
  h.run('api = async (path, body, opts) => { if (path.startsWith("source-tracks")) return {audio: [], subtitles: []}; signals.push(opts.signal); const a = answers.shift(); if (a instanceof Error) throw a; return a; }; $("path").value = "/media/a.mkv"; controls();');
  await h.run('loadGoalProfile("/media/a.mkv")');
  assert.equal(signals.length, 2, "one retry after 429");
  assert.equal(mode(), "compact");
  assert.match(line().textContent, /^4K · HDR10/);
  // A read still running when the user moves on is aborted.
  const pending = deferred();
  h.context.pending = pending.promise;
  h.run('api = async (path, body, opts) => { if (path.startsWith("source-tracks")) return {audio: [], subtitles: []}; signals.push(opts.signal); return pending; }; $("path").value = "/media/b.mkv"; controls();');
  const load = h.run('loadGoalProfile("/media/b.mkv")');
  const signal = signals.at(-1);
  h.run('$("path").value = "/media/c.mkv"; controls();');
  assert.equal(signal.aborted, true);
  pending.resolve(goalProfileFixture());
  await load;
  assert.equal(h.run("state.goalProfile.key"), "/media/c.mkv");
});

test("a cancelled request does not mark the server unreachable", async () => {
  const h = harness();
  h.run('var offline = 0; setConnection = (up) => { if (!up) offline++; };');
  h.context.fetch = (url, opts) => Promise.reject(Object.assign(new Error("aborted"), { name: "AbortError" }));
  const controller = new AbortController();
  controller.abort();
  h.context.signal = controller.signal;
  await assert.rejects(h.run('api("source-profile?path=x", undefined, {signal})'));
  assert.equal(h.run("offline"), 0);
});

test("a folder or batch reads the first video and says so", async () => {
  const { h, line, note } = goalHarness();
  h.context.profile = goalProfileFixture();
  h.run('$("scope").value = "batch"; $("service").value = "folder:/media"; state.folderSelected = new Set(["/media/Show/first.mkv", "/media/Show/second.mkv"]); api = async () => profile; controls();');
  assert.equal(h.run("state.goalProfile.key"), "/media/Show/first.mkv");
  await h.run('loadGoalProfile("/media/Show/first.mkv")');
  assert.equal(line().textContent, "4K · HDR10 · HEVC · 10-bit · 63.00 GB · based on the first file");
  assert.equal(note("size").textContent, "Skips the first file: HDR source");
});

test("submitting a goal the server says will skip asks first, and says so plainly", async () => {
  const { h, click } = goalHarness();
  h.context.profile = goalProfileFixture();
  h.run("api = async () => profile; controls()");
  await h.run('loadGoalProfile("/media/movie.mkv")');
  click("size");
  const reviews = [];
  h.context.reviews = reviews;
  h.context.answer = false;
  h.run("reviewAction = async (review) => { reviews.push(review); return answer; }; buildAndSubmitJob = async () => { reviews.push('submitted'); };");
  await h.run("submitJob()");
  assert.equal(reviews.length, 1);
  assert.equal(reviews[0].content[0].textContent, "Save space will skip this file: HDR source. The original is kept and nothing is converted.");
  assert.equal(reviews[0].confirmLabel, "Process anyway");
  h.context.answer = true;
  await h.run("submitJob()");
  assert.equal(reviews.at(-1), "submitted");
  // The advised goal submits without a question.
  click("compact");
  reviews.length = 0;
  await h.run("submitJob()");
  assert.deepEqual(reviews, ["submitted"]);
});

test("goal names are the same in the form, the reconfigure dialog and job rows", () => {
  const form = markup.slice(markup.indexOf('<fieldset id="processing-goal"'), markup.indexOf("</fieldset>", markup.indexOf('<fieldset id="processing-goal"')));
  const visible = form.slice(0, form.indexOf('<details id="more-goals"'));
  const more = form.slice(form.indexOf('<details id="more-goals"'));
  assert.match(visible, /value="size" checked[\s\S]*<strong class="goal-title">Save space<\/strong>[\s\S]*value="compact"[\s\S]*<strong id="goal-title-compact" class="goal-title">Shrink to 1080p SDR<\/strong>[\s\S]*value="audio_only"[\s\S]*<strong class="goal-title">Audio only<\/strong>[\s\S]*id="goal-note-audio_only"/);
  assert.doesNotMatch(visible, /value="quality"|value="x265_preserve"/);
  assert.match(more, /<summary>More goals<\/summary>/);
  assert.doesNotMatch(more, /checked/);
  assert.match(more, /value="quality"[\s\S]*<strong class="goal-title">Strict quality<\/strong>[\s\S]*Use it when picture quality matters more than space; may save little\./);
  assert.match(more, /value="x265_preserve"[\s\S]*<strong class="goal-title">Convert to HEVC<\/strong>[\s\S]*Use it to make a library HEVC-only; HEVC files are left alone\./);
  const select = markup.match(/<select id="reconfigure-mode">[\s\S]*?<\/select>/)[0];
  for (const name of ["Save space", "Shrink to SDR", "Audio only", "Strict quality", "Convert to HEVC"]) assert.ok(select.includes(name), name);
  assert.doesNotMatch(markup + source, /Prioritize quality|Make it much smaller|Reduce size|Convert to x265/);
  const h = harness();
  assert.equal(h.run("JSON.stringify(rerunGoals)"), JSON.stringify({ size: "Save space", compact: "Shrink to 1080p SDR", audio_only: "Audio only", quality: "Strict quality", x265_preserve: "Convert to HEVC" }));
});

// ---- Segmented controls ----

// Choosing a segment the way a click does: check the radio, fire its change.
function pickSegment(h, name, value) {
  const radio = h.segments.get(name).find((r) => r.value === value);
  assert.ok(radio, `no ${value} segment in ${name}`);
  radio.checked = true;
  radio.listeners.get("change")();
}
const checkedSegment = (h, name) => h.segments.get(name).find((r) => r.checked)?.value;

test("the segmented controls are named radio groups over the same fields", () => {
  for (const [name, values] of Object.entries(segmentGroups)) {
    const group = [...markup.matchAll(new RegExp(`<input type="radio" name="${name}" value="([^"]*)"`, "g"))].map((m) => m[1]);
    assert.deepEqual(group, values, name);
    assert.match(markup, new RegExp(`role="radiogroup" aria-labelledby="[\\w-]+">\\s*<label><input type="radio" name="${name}"`));
  }
  // The fields the radios drive are still the ones the request builders read.
  assert.match(markup, /<select id="size-budget-kind"[^>]*hidden/);
  assert.match(markup, /Fast · media engine/);
  assert.match(markup, /Smallest · x265/);
  assert.match(markup, /Like other episodes/);
  assert.match(css, /\.segmented label:has\(input:focus-visible\) \{[^}]*outline: 2px solid var\(--focus\)/);
});

test("segmented controls send the same payloads as the fields they drive", async () => {
  const submit = async (options, setup) => {
    const { h } = submissionHarness(options);
    let sent = null;
    h.context.capture = (_tool, args) => { sent = JSON.parse(args.inputs); return { id: "submitted" }; };
    h.run('processingMode=()=>"size";tool=async(...args)=>capture(...args);');
    await setup(h);
    await h.run('buildAndSubmitJob("encode")');
    return sent;
  };
  const fields = (code) => (h) => h.run(code);
  const picks = (...steps) => (h) => {
    for (const [name, value, before] of steps) {
      if (before) h.run(before);
      pickSegment(h, name, value);
    }
  };
  const same = async (label, oldWay, newWay) => {
    assert.deepEqual(await submit({ segments: true }, newWay), await submit({}, oldWay), label);
  };
  await same("media engine on", fields('$("allow-hardware").checked=true;'), picks(["size-engine", "fast"]));
  await same("media engine off", fields('$("allow-hardware").checked=false;'), picks(["size-engine", "small"]));
  await same("no limit", fields('$("size-budget-kind").value="";'), picks(["size-budget-limit", ""]));
  await same("fixed GB", fields('$("size-budget-kind").value="gb"; $("size-budget-gb").value="1.5";'), picks(["size-budget-limit", "fixed", '$("size-budget-gb").value="1.5";']));
  await same("fixed percent", fields('$("size-budget-kind").value="percent"; $("size-budget-percent").value="33";'), (h) => {
    h.run('$("size-budget-percent").value="33";');
    pickSegment(h, "size-budget-limit", "fixed");
    h.run('$("size-budget-unit").value="percent";');
    h.elements.get("size-budget-unit").listeners.get("change")();
  });
  await same("like other episodes", fields('$("size-budget-kind").value="siblings"; $("size-budget-multiplier").value="2";'), picks(["size-budget-limit", "siblings", '$("size-budget-multiplier").value="2";']));
  await same("like other episodes, default multiplier", fields('$("size-budget-kind").value="siblings"; $("size-budget-multiplier").value="";'), picks(["size-budget-limit", "siblings"]));
  const inputs = await submit({ segments: true }, picks(["size-engine", "small"], ["size-budget-limit", "fixed", '$("size-budget-gb").value="2";']));
  assert.equal(inputs.allow_hardware, false);
  assert.equal(inputs.size_budget_bytes, 2000000000);
  assert.equal("size_budget_percent" in inputs, false);
  assert.equal("size_budget_source" in inputs, false);
  // A fixed limit with nothing typed is still refused before anything is sent.
  await assert.rejects(submit({ segments: true }, picks(["size-budget-limit", "fixed"])), /0\.05/);
});

test("the change-goal dialog's segments drive its own fields", async () => {
  const h = harness({ segments: true });
  h.context.savedPlan = { id: "b", mode: "quality", scope: "all", selected: 2, kept: 0, active: 0, files: ["one", "two"], selection_version: "v1", settings: {} };
  h.run("state.info={};state.recipes=[];api=async()=>savedPlan;");
  await h.run('reconfigureBatch("b")');
  h.run('$("reconfigure-mode").value="size"; updateReconfigureHardware();');
  assert.equal(checkedSegment(h, "reconfigure-engine"), "fast");
  assert.equal(checkedSegment(h, "reconfigure-budget-limit"), "");
  pickSegment(h, "reconfigure-engine", "small");
  assert.equal(h.elements.get("reconfigure-hardware").checked, false);
  assert.equal(checkedSegment(h, "reconfigure-engine"), "small");
  pickSegment(h, "reconfigure-budget-limit", "siblings");
  assert.equal(h.elements.get("reconfigure-budget-kind").value, "siblings");
  assert.equal(h.elements.get("reconfigure-budget-multiplier-label").hidden, false);
  h.run('$("reconfigure-budget-multiplier").value="2.5";');
  const calls = [];
  h.context.record = (path, body) => calls.push([path, body]);
  h.context.sessionStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  h.run('submissionID=()=>"seg-1";commandRequest=async(path,body)=>{record(path,body);return {id:"b"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};');
  await h.run("submitReconfiguredBatch()");
  assert.equal(calls[0][1].allow_hardware, false);
  assert.equal(calls[0][1].size_budget_source, "siblings");
  assert.equal(calls[0][1].size_budget_sibling_multiplier, 2.5);
});

test("each render shows the field's value in its segments and the matching options", () => {
  const h = harness({ segments: true });
  const el = (id) => h.elements.get(id);
  h.run('processingMode=()=>"size"; $("allow-hardware").checked=false; $("size-budget-kind").value="gb"; controls();');
  assert.equal(checkedSegment(h, "size-engine"), "small");
  assert.equal(checkedSegment(h, "size-budget-limit"), "fixed");
  assert.equal(el("size-budget-fixed").hidden, false);
  assert.equal(el("size-budget-gb-label").hidden, false);
  assert.equal(el("size-budget-percent-label").hidden, true);
  assert.equal(el("size-budget-unit").value, "gb");
  h.run('$("allow-hardware").checked=true; $("size-budget-kind").value="percent"; controls();');
  assert.equal(checkedSegment(h, "size-engine"), "fast");
  assert.equal(checkedSegment(h, "size-budget-limit"), "fixed");
  assert.equal(el("size-budget-percent-label").hidden, false);
  assert.equal(el("size-budget-unit").value, "percent");
  h.run('$("size-budget-kind").value="siblings"; controls();');
  assert.equal(checkedSegment(h, "size-budget-limit"), "siblings");
  assert.equal(el("size-budget-fixed").hidden, true);
  assert.equal(el("size-budget-multiplier-label").hidden, false);
  h.run('$("size-budget-kind").value=""; controls();');
  assert.equal(checkedSegment(h, "size-budget-limit"), "");
  assert.equal(el("size-budget-multiplier-label").hidden, true);
});

test("the multiplier stepper stays between 1 and 4 and disables at its limits", () => {
  const h = harness({ segments: true });
  const el = (id) => h.elements.get(id);
  const click = (id) => el(id).listeners.get("click")();
  h.run('processingMode=()=>"size"; $("size-budget-kind").value="siblings"; $("size-budget-multiplier").value="1.5"; controls();');
  click("size-budget-multiplier-up");
  assert.equal(el("size-budget-multiplier").value, "1.6");
  click("size-budget-multiplier-down");
  click("size-budget-multiplier-down");
  assert.equal(el("size-budget-multiplier").value, "1.4");
  h.run('$("size-budget-multiplier").value="1.1";');
  click("size-budget-multiplier-down");
  assert.equal(el("size-budget-multiplier").value, "1");
  assert.equal(el("size-budget-multiplier-down").disabled, true);
  assert.equal(el("size-budget-multiplier-up").disabled, false);
  click("size-budget-multiplier-down");
  assert.equal(el("size-budget-multiplier").value, "1");
  h.run('$("size-budget-multiplier").value="3.9";');
  click("size-budget-multiplier-up");
  click("size-budget-multiplier-up");
  assert.equal(el("size-budget-multiplier").value, "4");
  assert.equal(el("size-budget-multiplier-up").disabled, true);
  // A multiplier typed by hand updates the buttons without a full render.
  h.run('$("size-budget-multiplier").value="2";');
  el("size-budget-multiplier").listeners.get("input")();
  assert.equal(el("size-budget-multiplier-up").disabled, false);
});

test("a percent limit reads out what it comes to, and other limits say nothing they cannot know", () => {
  const h = harness();
  const el = (id) => h.elements.get(id);
  const readout = (setup) => {
    h.run(`${setup} updateSizeBudgetFields("size-budget");`);
    return el("size-budget-readout").hidden ? "" : el("size-budget-readout").textContent;
  };
  h.run('state.goalProfile={status:"ready",batch:false,data:{profile:{size_bytes:8400000000}}};');
  assert.equal(readout('$("size-budget-kind").value="percent"; $("size-budget-percent").value="25";'), "≤ 2.10 GB");
  h.run("state.goalProfile.batch=true;");
  assert.equal(readout('$("size-budget-percent").value="50";'), "≤ 4.20 GB for the first file");
  assert.equal(readout('$("size-budget-percent").value="4";'), "", "out of range");
  assert.equal(readout('$("size-budget-percent").value="";'), "", "nothing typed");
  assert.equal(readout('$("size-budget-kind").value="gb"; $("size-budget-gb").value="2";'), "");
  assert.equal(readout('$("size-budget-kind").value="siblings";'), "", "sibling bitrates are only known to the server");
  h.run('state.goalProfile={status:"loading"};');
  assert.equal(readout('$("size-budget-kind").value="percent"; $("size-budget-percent").value="25";'), "");
});

// ---- Job details ----

const detailStageNames = ["resolve_source", "preflight", "submit_benchmark", "wait_benchmark", "submit_transcode", "wait_transcode", "validate_result"];
const ladderMessage = "Encoding the whole file with x265 CRF 24 (step 2 of 4) and checking it against this goal's quality limits.";
const stagesAt = (current, status = "running") => detailStageNames.map((name, i) => ({ name, status: i < current ? "completed" : i === current ? status : "pending" }));
function detailJob(extra = {}) {
  return {
    id: "detail-job", action_name: "transcode_media", status: "running", mode: "size",
    source_path: "/media/Example Show/Season 1/Example Show S01E01.mkv", current_step: 5,
    stages: stagesAt(5),
    savings: { source_bytes: 8400000000, estimated_saved_bytes: 3100000000, eta_seconds: 1500 },
    worker: { transcode_phase: "encoding", progress: 42.5, speed: 1.35, progress_is_stale: false, worker_heartbeat_at: new Date(Date.now() - 4000).toISOString(), last_progress_at: new Date(Date.now() - 3000).toISOString() },
    benchmark: { outcome: "size_budget_ladder", message: ladderMessage, plan_source: "size_budget", plan_note: "Encoded at your size budget." },
    transcode: { message: ladderMessage },
    size_mode: { expected_bytes: 5200000000, encoder: "libx265", size_budget: { budget_bytes: 5400000000 } },
    phase_costs: { worker_probe: { duration_ms: 420, attempts: 1, active_compute_ms: 300 } },
    logs_available: true,
    ...extra,
  };
}
async function openDetail(job) {
  const h = harness();
  h.context.detailData = job;
  h.run('api=async(path)=>path.startsWith("logs")?{stdout_tail:"frame=1 fps=30",stderr_tail:""}:{jobs:[detailData]}; tool=async()=>({data:{}});');
  await h.run(`openJob(${JSON.stringify(job.id)})`);
  return h;
}
const nodesWithClass = (n, cls) => [...(String(n.className || "").split(" ").includes(cls) ? [n] : []), ...(n.children || []).flatMap((c) => nodesWithClass(c, cls))];
const settle = () => new Promise((resolve) => setImmediate(resolve));

test("job details lead with a one-line title, a status chip and fact chips", async () => {
  const h = await openDetail(detailJob());
  const el = (id) => h.elements.get(id);
  assert.match(el("job-detail-title").textContent, /Example Show S01E01/);
  assert.equal(el("job-detail-title").title, "/media/Example Show/Season 1/Example Show S01E01.mkv");
  assert.equal(el("copy-detail-path").hidden, false);
  assert.ok(el("detail-status").children.length === 1, "one status chip");
  const chips = nodesWithClass(el("detail-summary"), "detail-fact").map((n) => n.textContent);
  assert.deepEqual(chips, ["8.40 GB → ~5.20 GB", "x265 CRF 24", "Try 2 of 4", "Limit ≤ 5.40 GB"]);
  // Nothing says "unknown" for what it does not know.
  const bare = await openDetail(detailJob({ savings: {}, size_mode: undefined, benchmark: undefined, transcode: { message: "" } }));
  assert.deepEqual(nodesWithClass(bare.elements.get("detail-summary"), "detail-fact").map((n) => n.textContent), []);
  assert.doesNotMatch(allTextOf(bare.elements.get("detail-summary")), /unknown/i);
});

test("job details have one progress line with the worker's heartbeat", async () => {
  const h = await openDetail(detailJob());
  const [hero] = nodesWithClass(h.elements.get("detail-summary"), "jd-hero");
  assert.ok(hero, "no progress");
  assert.match(nodesWithClass(hero, "jd-headline")[0].textContent, /^42\.5% · 1\.35×/);
  assert.equal(nodesWithClass(hero, "bar").length, 1, "one bar");
  const [live] = nodesWithClass(hero, "jd-live");
  // The dot is the first child, the words the second.
  assert.equal(live.children[1].textContent, "Worker live");
  assert.match(live.title, /^Last heartbeat /);
  // Without a heartbeat the dot is left out; the rest stays.
  const quiet = await openDetail(detailJob({ worker: { transcode_phase: "encoding", progress: 10, speed: 1, progress_is_stale: false } }));
  const quietHero = nodesWithClass(quiet.elements.get("detail-summary"), "jd-hero")[0];
  assert.equal(nodesWithClass(quietHero, "jd-live").length, 0);
  assert.doesNotMatch(allTextOf(quietHero), /undefined|NaN/);
  // A silent worker is named as such rather than live.
  const silent = await openDetail(detailJob({ worker: { transcode_phase: "encoding", progress: 10, speed: 1, progress_is_stale: false, worker_heartbeat_at: new Date(Date.now() - 600000).toISOString() } }));
  const silentDots = nodesWithClass(silent.elements.get("detail-summary"), "jd-live");
  assert.ok(silentDots.length === 0 || silentDots[0].textContent === "Worker silent");
});

test("job details without a worker still say why the job waits or what it checks", async () => {
  const text = (n) => [n.textContent || "", ...(n.children || []).map(text)].join(" ");
  const waiting = await openDetail(detailJob({ status: "waiting_external", worker: undefined, waiting_condition: "worker_busy", work: { phase: "waiting_for_worker", queue_position: 3 } }));
  assert.match(text(waiting.elements.get("detail-summary")), /running as many jobs as it allows.*2 jobs are ahead of it in line/);
  const offline = await openDetail(detailJob({ worker: undefined, waiting_condition: "worker_unreachable" }));
  assert.match(text(offline.elements.get("detail-summary")), /Worker unreachable\. Retrying connection automatically\./);
  const checking = await openDetail(detailJob({ worker: undefined, work: { phase: "verifying_original", bytes_read: 100, total_bytes: 1000 } }));
  assert.match(text(checking.elements.get("detail-summary")), /100 B \/ 1(\.0)? KB/);
  const plain = await openDetail(detailJob({ worker: undefined }));
  assert.equal(nodesWithClass(plain.elements.get("detail-summary"), "jd-hero").length + nodesWithClass(plain.elements.get("detail-summary"), "workflow-progress").length, 0);
});

test("job details name the current step in one line, not a stepper", async () => {
  const h = await openDetail(detailJob());
  const stepOf = (view) => nodesWithClass(view.elements.get("detail-summary"), "jd-step").map((n) => n.textContent);
  assert.deepEqual(stepOf(h), ["Step 6 of 7 · Convert file"]);
  assert.equal(nodesWithClass(h.elements.get("detail-summary"), "job-steps").length, 0);
  const failed = await openDetail(detailJob({ status: "failed", error: "ffmpeg exited with status 1", worker: undefined, stages: stagesAt(5, "failed") }));
  assert.deepEqual(stepOf(failed), ["Failed at step 6 of 7 · Convert file"]);
  const done = await openDetail(detailJob({ status: "completed", worker: undefined, stages: detailStageNames.map((name) => ({ name, status: "completed" })) }));
  // A finished job already says "Completed" in its chip; a step line would repeat it.
  assert.deepEqual(stepOf(done), []);
});

test("job details print the plan once, in its own row, and not under the summary", async () => {
  const h = await openDetail(detailJob());
  const summary = allTextOf(h.elements.get("detail-summary"));
  const plan = allTextOf(h.elements.get("detail-plan"));
  assert.match(plan, /Encoded at your size budget/);
  assert.doesNotMatch(summary, /Encoded at your size budget/);
  assert.doesNotMatch(summary, /step 2 of 4/);
  const everywhere = [summary, plan, allTextOf(h.elements.get("detail-stages")), allTextOf(h.elements.get("detail-path"))].join(" \n ");
  assert.equal(everywhere.match(/Encoded at your size budget/g).length, 1);
  assert.equal(h.elements.get("detail-row-plan").hidden, false);
  // A job with no plan has no Plan row.
  const bare = await openDetail(detailJob({ benchmark: undefined, transcode: undefined, size_mode: undefined }));
  assert.equal(bare.elements.get("detail-row-plan").hidden, true);
});

test("job details keep plan, steps, phase costs, path and worker log in collapsible rows", async () => {
  assert.match(markup, /<details id="detail-row-plan" class="detail-row" hidden>\s*<summary>Plan<\/summary>/);
  assert.match(markup, /<details id="detail-row-steps" class="detail-row" hidden>\s*<summary>All steps<\/summary>\s*<ol id="detail-stages"/);
  assert.match(markup, /<details id="detail-row-path" class="detail-row" hidden>\s*<summary>Path<\/summary>/);
  assert.match(markup, /<details id="detail-row-log" class="detail-row" hidden>\s*<summary>Worker log<\/summary>\s*<pre id="detail-content">/);
  assert.doesNotMatch(markup, /<details id="detail-row-[a-z]+"[^>]* open/, "rows start collapsed");
  const h = await openDetail(detailJob());
  const el = (id) => h.elements.get(id);
  for (const key of ["plan", "steps", "path", "log"]) {
    assert.equal(el(`detail-row-${key}`).hidden, false, key);
    assert.equal(el(`detail-row-${key}`).open, false, `${key} starts collapsed`);
  }
  assert.equal(el("detail-stages").children.length, 7);
  const costs = nodesWithClass(el("detail-costs"), "phase-costs");
  assert.equal(costs.length, 1);
  assert.match(allTextOf(el("detail-path")), /Example Show S01E01\.mkv/);
  // A row the user opened stays open across a refresh, and the log loads when it opens.
  el("detail-row-steps").open = true;
  el("detail-row-log").open = true;
  el("detail-row-log").listeners.get("toggle")();
  await settle();
  assert.match(el("detail-content").textContent, /frame=1 fps=30/);
  await h.run("refreshDetail()");
  assert.equal(el("detail-row-steps").open, true);
  assert.equal(el("detail-row-log").open, true);
  // A job without logs has no log row.
  const noLogs = await openDetail(detailJob({ logs_available: false }));
  assert.equal(noLogs.elements.get("detail-row-log").hidden, true);
});

test("a failed step opens the steps row once, and closing it sticks", async () => {
  const failedJob = (id) => detailJob({ id, status: "failed", error: "ffmpeg exited with status 1", worker: undefined, stages: stagesAt(5, "failed") });
  const h = await openDetail(failedJob("failed-a"));
  const el = (id) => h.elements.get(id);
  assert.equal(el("detail-row-steps").open, true);
  assert.equal(el("detail-row-log").open, false, "the failed step already explains the failure");
  el("detail-row-steps").open = false;
  await h.run("refreshDetail()");
  assert.equal(el("detail-row-steps").open, false, "closing the row sticks across refreshes");
  // Opening another job starts over.
  h.context.detailData = failedJob("failed-b");
  await h.run('openJob("failed-b")');
  assert.equal(el("detail-row-steps").open, true);
});

test("a failure no step explains opens the worker log", async () => {
  const h = await openDetail(detailJob({ status: "failed", error: "worker lost", worker: undefined, stages: stagesAt(5, "pending") }));
  const el = (id) => h.elements.get(id);
  assert.equal(el("detail-row-log").open, true);
  assert.equal(el("detail-row-steps").open, false);
});

test("cancel keeps its handler and confirmation, in the action bar that closes the dialog", async () => {
  // Header, status, files, details, then the pinned actions.
  const order = ["jd-header", 'id="detail-summary"', 'id="batch-items-panel"', 'id="detail-rows"', 'id="detail-controls"'].map((marker) => markup.indexOf(marker));
  assert.deepEqual(order, [...order].sort((a, b) => a - b));
  assert.ok(order.every((index) => index > 0));
  const h = await openDetail(detailJob());
  const calls = [];
  h.context.record = (name, id, args) => calls.push([name, id, args]);
  h.run("jobControl = async (id, name, args) => record(name, id, args); loadJobs = async () => {};");
  const all = (n) => [n, ...(n.children || []).flatMap(all)];
  const cancel = all(h.elements.get("detail-controls")).find((n) => n.textContent === "Cancel" && n.listeners.has("click"));
  assert.ok(cancel, "cancel button");
  h.run("reviewAction = async () => false;");
  await cancel.listeners.get("click")();
  assert.deepEqual(calls, [], "declining the confirmation cancels nothing");
  h.run("reviewAction = async (options) => { state.asked = options; return true; };");
  await cancel.listeners.get("click")();
  assert.equal(h.run("state.asked.title"), "Cancel job?");
  assert.equal(JSON.stringify(calls), JSON.stringify([["action_cancel", "detail-job", { reason: "Cancelled from maintenance UI" }]]));
});

test("the copy button copies the full path", async () => {
  const h = await openDetail(detailJob());
  const copied = [];
  h.context.navigator = { clipboard: { writeText: async (text) => { copied.push(text); } } };
  h.run("notify = () => {};");
  await h.elements.get("copy-detail-path").listeners.get("click")();
  await settle();
  assert.deepEqual(copied, ["/media/Example Show/Season 1/Example Show S01E01.mkv"]);
});

// ---- A job that left no file says so, and why ----

const sizeRejectedMessage = "The audio, subtitles and other streams alone take 1.14 GB, which leaves no usable video bitrate within the 1.00 GB budget. At least 1.20 GB would be needed for this file; raise the budget, or remove audio or subtitle tracks first. Original preserved.";
const sizeRejectedNextStep = "Raise the size budget, or remove audio or subtitle tracks, then run it again.";
function notConvertedJob(extra = {}) {
  return detailJob({
    status: "completed", reason_code: "size_budget_exceeded", candidate_ready: false, replaced: false, worker: undefined,
    stages: stagesAt(7, "completed"), current_step: 7,
    savings: { source_bytes: 2790000000, estimated_saved_bytes: 1820000000 },
    benchmark: { outcome: "size_rejected", message: sizeRejectedMessage, user_next_step: sizeRejectedNextStep },
    transcode: { message: "" },
    size_mode: { expected_bytes: 970000000, encoder: "libx265", size_budget: { budget_bytes: 1000000000 } },
    ...extra,
  });
}

test("a size-rejected file is Not converted, says why and shows no savings", async () => {
  const h = harness();
  h.context.job = notConvertedJob();
  const row = h.run("queuePresentation(job)");
  assert.equal(row.status, "Not converted");
  assert.match(row.summary, /1\.14 GB/);
  assert.match(row.summary, /1\.00 GB budget/);
  assert.match(row.summary, /original kept/i);
  assert.match(row.summary, /2\.79 GB source/);
  assert.doesNotMatch(row.summary, /estimated savings|potential savings|freed/);
  assert.equal(h.run("statusClass(queuePresentation(job).status)"), "neutral");
  // The job's details carry the full message and the next step.
  const detail = await openDetail(notConvertedJob());
  const text = allTextOf(detail.elements.get("detail-summary"));
  assert.match(allTextOf(detail.elements.get("detail-status")), /Not converted/);
  assert.match(text, /Result/);
  assert.match(text, /At least 1\.20 GB would be needed/);
  assert.match(text, /Next step:\s*Raise the size budget/);
  assert.doesNotMatch(text, /estimated savings/);
  // The chips name what is known, not an encode that never ran.
  const chips = (d) => nodesWithClass(d.elements.get("detail-summary"), "detail-fact").map((n) => n.textContent);
  assert.deepEqual(chips(detail), ["2.79 GB", "Limit ≤ 1.00 GB"]);
  const measured = await openDetail(notConvertedJob({ savings: { source_bytes: 2790000000, candidate_bytes: 2000000000 } }));
  assert.deepEqual(chips(measured), ["2.79 GB", "Limit ≤ 1.00 GB"]);
});

test("a reason code the UI has never heard of still reads as a sentence", () => {
  const h = harness();
  h.context.noMessage = notConvertedJob({ reason_code: "future_new_reason", benchmark: undefined });
  const bare = h.run("queuePresentation(noMessage)");
  assert.equal(bare.status, "Not converted");
  assert.match(bare.summary, /Future new reason/);
  assert.doesNotMatch(bare.summary, /future_new_reason|estimated savings/);
  h.context.withMessage = notConvertedJob({ reason_code: "future_new_reason", benchmark: { outcome: "future_outcome", message: "The worker could not read this file's frame rate. Original preserved." } });
  const said = h.run("queuePresentation(withMessage)");
  assert.equal(said.status, "Not converted");
  assert.match(said.summary, /The worker could not read this file's frame rate; original kept/);
  // Neither an empty job reason nor an empty message drops the sentence.
  h.context.nothing = notConvertedJob({ reason_code: undefined, benchmark: undefined });
  assert.match(h.run("queuePresentation(nothing)").summary, /No converted file was made/);
});

test("a long reason is trimmed at a boundary, keeps its numbers and the original-kept suffix", () => {
  const h = harness();
  const reason = (message) => { h.context.probe = notConvertedJob({ benchmark: { outcome: "quality_rejected", message } }); return h.run("queuePresentation(probe).summary"); };
  const long = "The encoded result did not meet the quality requirements on the whole file: VMAF mean 91.2 is below the minimum 93.0; VMAF 5th percentile 71.4 is below the minimum 80.0; the worst 10 second window scored 52.5 against a floor of 60.0. Original preserved.";
  const text = reason(long);
  assert.match(text, /…; original kept/);
  assert.match(text, /minimum 93\.0…; original kept/, "cut at a clause, numbers whole");
  assert.ok(text.length < 210, text);
  // A sentence that already says so is not repeated.
  assert.match(reason("The goal was not met; original preserved. Raise the budget."), /preserved · 2\.79 GB source$/);
  assert.doesNotMatch(reason("The goal was not met; original preserved. Raise the budget."), /original kept/i);
  assert.equal((reason("Nothing met the goal; original kept.").match(/original kept/gi) || []).length, 1);
  // An abbreviation does not end the sentence.
  assert.match(reason("The file needs approx. 5.00 GB for this goal. Raise the budget."), /approx\. 5\.00 GB for this goal; original kept/);
});

test("only a job that left no file and replaced nothing is Not converted", () => {
  const h = harness();
  const status = (patch) => { h.context.probe = notConvertedJob(patch); return h.run("queuePresentation(probe).status"); };
  assert.equal(status({}), "Not converted");
  assert.equal(status({ reason_code: "quality_not_met" }), "Not converted");
  assert.equal(status({ candidate_ready: true }), "Candidate ready");
  assert.equal(status({ replaced: true }), "Replaced");
  assert.equal(status({ replacement_action_id: "replacement" }), "Completed");
  assert.equal(status({ candidate_cleanup: { reason: "superseded", outcome: "removed", bytes_freed: 0 } }), "Completed");
  assert.equal(status({ status: "failed", error: "transcode candidate rejected by user decision; candidate file removed" }), "Rejected");
  assert.equal(status({ status: "failed", error: "ffmpeg exited with status 1" }), "Failed");
  // Declining the replacement review touched nothing: it is not a failure.
  const declined = { action_name: "promote_transcode_candidate", status: "failed", error: "candidate promotion rejected; original and candidate remain untouched" };
  assert.equal(status(declined), "Kept original");
  assert.equal(status({ ...declined, error: "candidate promotion rejected; original and candidate remain untouched", promotion: undefined }), "Kept original");
  assert.equal(status({ ...declined, error: "integrity violation: SHA-256 changed" }), "Failed");
  h.context.declinedJob = notConvertedJob(declined);
  assert.match(h.run("shortJobReason(declinedJob.error)"), /declined by you/);
  assert.equal(h.run("statusClass(queuePresentation(declinedJob).status)"), "neutral");
  assert.equal(status({ status: "cancelled" }), "Cancelled");
  assert.equal(status({ action_name: "promote_transcode_candidate" }), "Completed");
  // A job with a retired candidate is not Not converted, but keeps its reason in the details.
  h.context.retired = notConvertedJob({ benchmark: undefined, reason_code: "quality_not_met", candidate_cleanup: { reason: "superseded", outcome: "removed", bytes_freed: 0 } });
  assert.match(h.run("jobOutcomeText(retired)"), /quality target; original kept/);
});

test("a batch with files still waiting is not Replaced because one file was", () => {
  const h = harness();
  const batch = { total: 11, completed: 1, waiting_decision: 4 };
  const job = { id: "b", action_name: "transcode_media", status: "waiting_decision", batch, savings: { realized_saved_bytes: 1000 }, replaced_files: 1, remaining_candidates: 0 };
  h.context.probe = job;
  assert.notEqual(h.run("queuePresentation(probe).status"), "Replaced");
  h.context.probe = { ...job, status: "completed", batch: { total: 11, completed: 11 } };
  assert.equal(h.run("queuePresentation(probe).status"), "Replaced");
});

// ---- One-step batch replacement approval ----
const flatten = (el) => [el, ...(el.children || []).flatMap(flatten)];
const textOf = (el) => flatten(el).map((e) => e.textContent).filter(Boolean).join(" ");
const sampleMember = (key, name, extra = {}) => ({ item_key: key, episode: name, transcode_action_id: `t-${key}`, original_path: `/media/${name}.mkv`, candidate_path: `/media/.candidates/${name}.mkv`, candidate_sha256: `sha-${key}`, ...extra });
const sampleFacts = (source, candidate, extra = {}) => ({ source_bytes: source, candidate_bytes: candidate, saved_bytes: source - candidate, vmaf_mean: 93.4, vmaf_worst_window: 90.2, tracks_message: "Kept English and Japanese audio", ...extra });
const threeMembers = () => [sampleMember("a", "Sample Show S01E01"), sampleMember("b", "Sample Show S01E02"), sampleMember("c", "Sample Show S01E03")];
const threeFacts = () => ({ "t-a": sampleFacts(4e9, 2.4e9), "t-b": sampleFacts(3e9, 1.8e9), "t-c": sampleFacts(2e9, 1.2e9, { size_budget_outcome: "below_quality_target_within_budget" }) });
const preparedPlan = (members, digest = "sha256:new", extra = {}) => ({ prepared: true, id: "batch-a", digest, approved: false, waiting: true, service: "filesystem", members, member_facts: threeFacts(), ...extra });
// A review harness: plan reads come from a queue, every command is recorded.
function batchReviewHarness({ planReads, members = threeMembers(), notice = null }) {
  const h = harness();
  h.run("state.info = {allow_destructive:true};");
  h.context.calls = [];
  h.context.planReads = planReads;
  h.context.review = { id: "batch-a", version: "v1", service: "filesystem", excluded: 0, members, member_facts: threeFacts() };
  h.context.notice = notice;
  h.run(`api = async (path) => { calls.push(["api", path]); if (path.includes("plan=1")) return planReads.shift(); return review; };
    tool = async () => ({data: notice});
    commandRequest = async (path, body, onAccepted, decision) => { calls.push(["command", path, JSON.parse(JSON.stringify(body)), JSON.parse(JSON.stringify(decision))]); onAccepted?.(); return {id: body.id}; };`);
  return h;
}
const commandCalls = (h) => plain(h.context.calls.filter((c) => c[0] === "command"));
const click = (h, id) => h.elements.get(id).listeners.get("click")();

test("one dialog shows each file's sizes, savings, quality and tracks, and the total it frees", async () => {
  const h = batchReviewHarness({ planReads: [{ prepared: false }] });
  await h.run('reviewBatchReplacement("batch-a")');
  assert.equal(h.elements.get("batch-review").open, true);
  const rows = h.elements.get("batch-review-data").children;
  // Select all, then one row per file.
  assert.equal(rows.length, 4);
  const first = textOf(rows[1]);
  assert.match(first, /Sample Show S01E01/);
  assert.match(first, /4\.00 GB → 2\.40 GB · Saves 1\.60 GB \(40%\)/);
  assert.match(first, /Quality VMAF 93 · worst 90/);
  assert.match(first, /Kept English and Japanese audio/);
  assert.match(textOf(rows[3]), /Below quality target — within size budget/);
  // Paths are behind a disclosure that is closed by default.
  const paths = flatten(rows[1]).find((e) => /replacement-paths/.test(e.className));
  assert.ok(paths);
  assert.equal(flatten(paths).find((e) => e.textContent === "Show paths") !== undefined, true);
  assert.equal(paths.open, false);
  assert.equal(h.elements.get("batch-review-total").textContent, "Frees 3.60 GB across 3 files");
  assert.equal(h.elements.get("approve-batch-review").textContent, "Replace 3 files");
  assert.equal(h.elements.get("approve-batch-review").disabled, false);
  // Nothing to reject yet: the secondary button appears only for a prepared plan.
  assert.equal(h.elements.get("reject-batch-review").hidden, true);
  // No per-row lookups of the transcode jobs.
  assert.equal(h.context.calls.some((c) => /operations\?id=/.test(c[1] || "")), false);
});

test("one click prepares the checked files, reads the plan back, verifies it and approves its digest", async () => {
  const prepared = preparedPlan(threeMembers());
  const h = batchReviewHarness({ planReads: [{ prepared: false }, { prepared: false }, prepared] });
  await h.run('reviewBatchReplacement("batch-a")');
  await click(h, "approve-batch-review");
  const commands = commandCalls(h);
  assert.equal(commands.length, 2);
  assert.deepEqual(commands[0].slice(1, 3), ["batch-candidates", { id: "batch-a", version: "v1", item_keys: ["a", "b", "c"] }]);
  assert.equal(commands[0][3].label, "Prepare replacement of 3 files");
  assert.equal(commands[0][3].verb, "Preparing replacement of 3 files");
  assert.deepEqual(commands[1].slice(1, 3), ["batch-candidates", { id: "batch-a", digest: "sha256:new", decision: "approve" }]);
  assert.deepEqual(commands[1][3], { label: "Replace 3 files", verb: "Replacing 3 files", failure: "Replacement failed" });
  // The plan was read before preparing and after it, never approved unread.
  const order = h.context.calls.map((c) => (c[0] === "command" ? c[2].decision || "prepare" : "read"));
  assert.deepEqual(order.filter((x) => x !== "read"), ["prepare", "approve"]);
  assert.ok(order.lastIndexOf("read") < order.indexOf("approve"));
  assert.equal(h.elements.get("batch-review").open, false);
});

test("only the checked files are prepared and the button and total follow the checks", async () => {
  const prepared = preparedPlan([sampleMember("a", "Sample Show S01E01"), sampleMember("c", "Sample Show S01E03")]);
  const h = batchReviewHarness({ planReads: [{ prepared: false }, { prepared: false }, prepared] });
  await h.run('reviewBatchReplacement("batch-a")');
  const rows = h.elements.get("batch-review-data").children;
  // A checkable file hangs its box in the margin so its facts line up under the name.
  assert.ok(String(rows[1].className).split(" ").includes("selectable"));
  const input = flatten(rows[2]).find((e) => e.listeners?.has("change"));
  input.checked = false;
  input.listeners.get("change")();
  assert.equal(h.elements.get("approve-batch-review").textContent, "Replace 2 files");
  assert.equal(h.elements.get("batch-review-total").textContent, "Frees 2.40 GB across 2 files");
  await click(h, "approve-batch-review");
  const commands = commandCalls(h);
  assert.deepEqual(commands[0][2].item_keys, ["a", "c"]);
  assert.equal(commands[1][2].decision, "approve");
  assert.equal(commands[1][3].label, "Replace 2 files");
  // Nothing selected cannot be sent.
  const input2 = flatten(h.elements.get("batch-review-data").children[1]).find((e) => e.listeners?.has("change"));
  input2.checked = false;
  input2.listeners.get("change")();
  const input3 = flatten(h.elements.get("batch-review-data").children[3]).find((e) => e.listeners?.has("change"));
  input3.checked = false;
  input3.listeners.get("change")();
  assert.equal(h.elements.get("approve-batch-review").disabled, true);
});

test("a prepared list that differs from what was shown is not approved; the difference is shown and asks again", async () => {
  const different = preparedPlan([sampleMember("a", "Sample Show S01E01"), sampleMember("b", "Sample Show S01E02"), sampleMember("d", "Sample Show S01E04")], "sha256:other");
  const h = batchReviewHarness({ planReads: [{ prepared: false }, { prepared: false }, different, different] });
  await h.run('reviewBatchReplacement("batch-a")');
  await click(h, "approve-batch-review");
  // Prepared once, never approved.
  assert.deepEqual(commandCalls(h).map((c) => c[2].decision || "prepare"), ["prepare"]);
  assert.equal(h.elements.get("batch-review").open, true);
  const notice = textOf(h.elements.get("batch-review-notice"));
  assert.match(notice, /Not in the prepared list: Sample Show S01E03/);
  assert.match(notice, /Added to the prepared list: Sample Show S01E04/);
  assert.match(textOf(h.elements.get("batch-review-data")), /Sample Show S01E04/);
  assert.doesNotMatch(textOf(h.elements.get("batch-review-data")), /Sample Show S01E03/);
  assert.equal(h.elements.get("reject-batch-review").hidden, false);
  // The person sees the server's list and confirms it: approved with its digest.
  await click(h, "approve-batch-review");
  const last = commandCalls(h).at(-1);
  assert.deepEqual(last.slice(1, 3), ["batch-candidates", { id: "batch-a", digest: "sha256:other", decision: "approve" }]);
});

test("a changed candidate path or hash under the same file is a difference", async () => {
  const swapped = threeMembers().map((m) => (m.item_key === "b" ? { ...m, candidate_sha256: "sha-changed" } : m));
  const h = batchReviewHarness({ planReads: [{ prepared: false }, { prepared: false }, preparedPlan(swapped)] });
  await h.run('reviewBatchReplacement("batch-a")');
  await click(h, "approve-batch-review");
  assert.deepEqual(commandCalls(h).map((c) => c[2].decision || "prepare"), ["prepare"]);
  assert.match(textOf(h.elements.get("batch-review-notice")), /Changed since you reviewed it: Sample Show S01E02/);
});

test("a plan that already exists for another selection is shown, not replaced or approved", async () => {
  const existing = preparedPlan([sampleMember("a", "Sample Show S01E01")], "sha256:existing");
  const h = batchReviewHarness({ planReads: [{ prepared: false }, existing] });
  await h.run('reviewBatchReplacement("batch-a")');
  await click(h, "approve-batch-review");
  assert.equal(commandCalls(h).length, 0);
  assert.match(textOf(h.elements.get("batch-review-notice")), /Not in the prepared list: Sample Show S01E02/);
  assert.equal(h.elements.get("approve-batch-review").textContent, "Replace 1 file");
});

test("a plan opened from the queue shows the same dialog and approves only the digest it showed", async () => {
  const plan = preparedPlan([sampleMember("a", "Sample Show S01E01"), sampleMember("b", "Sample Show S01E02")], "sha256:d1");
  const h = batchReviewHarness({ planReads: [plan, { ...plan }] });
  await h.run('reviewBatchReplacement("batch-a")');
  assert.equal(h.elements.get("approve-batch-review").textContent, "Replace 2 files");
  assert.equal(h.elements.get("reject-batch-review").hidden, false);
  assert.equal(h.elements.get("batch-review-data").children.length, 2);
  await click(h, "approve-batch-review");
  assert.deepEqual(commandCalls(h).map((c) => c[2].decision), ["approve"]);
  assert.equal(commandCalls(h)[0][2].digest, "sha256:d1");
  // A plan whose digest changed while the dialog was open is shown again.
  const h2 = batchReviewHarness({ planReads: [plan, { ...plan, digest: "sha256:d2" }] });
  await h2.run('reviewBatchReplacement("batch-a")');
  await click(h2, "approve-batch-review");
  assert.equal(commandCalls(h2).length, 0);
  assert.match(textOf(h2.elements.get("batch-review-notice")), /list changed while this review was open/);
});

test("an approved or not-waiting plan does not open the dialog", async () => {
  const h = batchReviewHarness({ planReads: [preparedPlan(threeMembers(), "sha256:x", { approved: true, waiting: false })] });
  await assert.rejects(h.run('reviewBatchReplacement("batch-a")'), /already approved/);
  assert.equal(h.elements.get("batch-review").open, false);
  const h2 = batchReviewHarness({ planReads: [preparedPlan(threeMembers(), "sha256:x", { waiting: false })] });
  await assert.rejects(h2.run('reviewBatchReplacement("batch-a")'), /not waiting for approval/);
});

test("Keep originals rejects the prepared plan by digest and names the decision", async () => {
  const plan = preparedPlan([sampleMember("a", "Sample Show S01E01"), sampleMember("b", "Sample Show S01E02")], "sha256:d1");
  const h = batchReviewHarness({ planReads: [plan, { ...plan }] });
  await h.run('reviewBatchReplacement("batch-a")');
  await click(h, "reject-batch-review");
  const command = commandCalls(h)[0];
  assert.deepEqual(command.slice(1, 3), ["batch-candidates", { id: "batch-a", digest: "sha256:d1", decision: "reject" }]);
  assert.equal(command[3].label, "Keep 2 originals");
  assert.equal(command[3].verb, "Keeping 2 originals");
});

test("a re-grab warning that appears after preparing is shown before anything is approved", async () => {
  const notice = { regrab_warning: "May download the original again.", arr_monitored: true };
  const h = batchReviewHarness({ planReads: [{ prepared: false }, { prepared: false }, preparedPlan(threeMembers())], notice });
  await h.run('reviewBatchReplacement("batch-a")');
  await click(h, "approve-batch-review");
  assert.deepEqual(commandCalls(h).map((c) => c[2].decision || "prepare"), ["prepare"]);
  assert.match(textOf(h.elements.get("batch-review-notice")), /download the original again/);
});

test("the one-click approval needs a re-grab notice it could verify for the plan it approves", async () => {
  const run = async (readNotice, planReads) => {
    const h = batchReviewHarness({ planReads });
    h.context.readNotice = readNotice;
    h.run("tool = async () => readNotice();");
    await h.run('reviewBatchReplacement("batch-a")');
    return h;
  };
  const approvals = (h) => commandCalls(h).filter((c) => c[2].decision === "approve").length;
  const reads = () => [{ prepared: false }, { prepared: false }, preparedPlan(threeMembers())];
  // The server stored none (the key is not found): a batch that is not compact.
  let h = await run(async () => { throw new Error('key "batch_promotion_notice" not found in section state (available keys: [x])'); }, reads());
  await click(h, "approve-batch-review");
  assert.equal(approvals(h), 1);
  // Any other failure to read it stops the approval instead of passing unwarned.
  h = await run(async () => { throw new Error("network down"); }, reads());
  await click(h, "approve-batch-review");
  assert.match(h.elements.get("notice").textContent, /Cannot verify the re-grab warning/);
  assert.equal(approvals(h), 0);
  // A notice computed for another plan is stale, however it reads.
  h = await run(async () => ({ data: { regrab_warning: "Old.", plan_digest: "sha256:earlier" } }), reads());
  await click(h, "approve-batch-review");
  assert.match(h.elements.get("notice").textContent, /earlier plan/);
  assert.equal(approvals(h), 0);
  // The notice for this plan is shown before anything is approved.
  h = await run(async () => ({ data: { regrab_warning: "May download the original again.", plan_digest: "sha256:new" } }), reads());
  await click(h, "approve-batch-review");
  assert.equal(approvals(h), 0);
  assert.match(textOf(h.elements.get("batch-review-notice")), /download the original again/);
});

test("replacement stays disabled in the dialog when the server setting is off", async () => {
  const h = batchReviewHarness({ planReads: [{ prepared: false }] });
  h.run("state.info = {allow_destructive:false};");
  await h.run('reviewBatchReplacement("batch-a")');
  assert.equal(h.elements.get("approve-batch-review").disabled, true);
  assert.match(textOf(h.elements.get("batch-review-notice")), /disabled in the server settings/);
});

// ---- Decisions named in chips and history ----
test("chips and the history name the decision that was sent", () => {
  const now = 5_000_000;
  const h = commandHarness([
    { command_id: "r1", id: "batch-a", kind: "review_batch_promotion", status: "running", started: now - 4_000, decision: { label: "Replace 3 files", verb: "Replacing 3 files", failure: "Replacement failed" } },
    { command_id: "p1", id: "batch-p", kind: "prepare_batch_promotion", status: "pending", started: now - 4_000, decision: { label: "Prepare replacement of 3 files", verb: "Preparing replacement of 3 files", failure: "Preparation failed" } },
    { command_id: "k1", id: "batch-k", kind: "review_batch_promotion", status: "running", started: now - 4_000, decision: { label: "Keep 3 originals", verb: "Keeping 3 originals", failure: "Keeping originals failed" } },
    { command_id: "f1", id: "batch-f", kind: "review_batch_promotion", status: "failed", error: "x", started: now - 4_000, decision: { label: "Replace 3 files", verb: "Replacing 3 files", failure: "Replacement failed" } },
  ]);
  assert.equal(chipOf(h, "batch-a", now).text, "Replacing 3 files");
  assert.equal(chipOf(h, "batch-p", now).text, "Preparing replacement of 3 files");
  assert.equal(chipOf(h, "batch-k", now).text, "Keeping 3 originals");
  assert.equal(chipOf(h, "batch-f", now).text, "Replacement failed");
  h.run(`state.detail="batch-a";renderCommandHistory(${now});`);
  assert.match(textOf(h.elements.get("detail-command-list").children[0]), /^Replace 3 files\s+· Applying/);
  // A command with no named decision keeps its generic wording.
  const plain = commandHarness([{ command_id: "z1", id: "job-z", kind: "retry", label: "Retrying job", status: "running", started: now - 1_000 }]);
  assert.equal(chipOf(plain, "job-z", now).text, "Retrying");
});

test("a command request stores the decision it was sent for", async () => {
  const h = harness();
  const stored = {};
  h.context.crypto = { getRandomValues: (b) => b.fill(7) };
  h.context.sessionStorage = { getItem: (k) => stored[k] ?? null, setItem: (k, v) => { stored[k] = v; }, removeItem() {} };
  h.run('api = async () => ({command_id:"cmd-1", id:"batch-a"}); waitCommand = async () => ({id:"batch-a"}); notify = () => {};');
  await h.run('commandRequest("batch-candidates", {id:"batch-a", digest:"d", decision:"approve"}, undefined, {label:"Replace 3 files", verb:"Replacing 3 files", failure:"Replacement failed"})');
  const receipts = JSON.parse(stored.navigatorr_commands);
  assert.equal(Object.values(receipts)[0].decision.label, "Replace 3 files");
});

// ---- Queue row, progress and members of an approved replacement ----
const replacingBatch = (extra = {}) => ({
  id: "batch-a", action_name: "transcode_batch", status: "running", batch: { total: 3, completed: 3, promotion_plan_ready: true },
  replacement: {
    approved: true, total: 3, done: 1, failed: 0, frees_bytes: 5160000000, freed_bytes: 1600000000,
    members: [
      { item_key: "k-a", name: "Sample Show S01E01", phase: "done", freed_bytes: 1600000000 },
      { item_key: "k-b", name: "Sample Show S01E02", phase: "backup", percent: 55 },
      { item_key: "k-c", name: "Sample Show S01E03", phase: "waiting" },
    ],
  },
  ...extra,
});

test("the queue row names what is being replaced and what it frees", () => {
  const h = harness();
  h.context.job = replacingBatch();
  const presentation = JSON.parse(h.run("JSON.stringify(queuePresentation(job))"));
  assert.equal(presentation.status, "Replacing");
  assert.match(presentation.summary, /Replacing 3 files · frees 5\.16 GB/);
  assert.doesNotMatch(presentation.summary, /approval required/);
  assert.equal(h.run("replacementHeadline(job)"), "Replacing 3 files · frees 5.16 GB");
  h.run('job.replacement.done = 3; job.replacement.members.forEach(m => { m.phase = "done"; }); job.replacement.freed_bytes = 5160000000;');
  assert.equal(h.run("replacementHeadline(job)"), "Replaced 3 files · freed 5.16 GB");
});

test("the progress headline and one line per file describe the real stage, with no estimate", () => {
  const h = harness();
  h.context.job = replacingBatch();
  const block = h.run("replacementProgressBlock(job)");
  const text = textOf(block);
  assert.match(text, /Replacing 3 files · 1 of 3 done/);
  // A row names the files being worked on; finished and waiting ones are in the headline's count.
  assert.match(text, /Sample Show S01E02 Backing up the original \(55%\)/);
  assert.doesNotMatch(text, /S01E01|S01E03/);
  assert.doesNotMatch(text, /Estimated workflow progress|Not time remaining|%\s*$/);
  // Each stage reads as a plain step.
  const steps = JSON.parse(h.run(`JSON.stringify(["checking","backup","backup_check","swap","final_check","done","waiting"].map(phase => replacementStepText({phase})))`));
  assert.deepEqual(steps, ["Checking the files", "Backing up the original", "Checking the backup", "Swapping in the new file", "Checking the new file", "Done", "Waiting"]);
  // The batch progress block is this block while a replacement runs.
  assert.match(textOf(h.run("batchProgress(job)")), /1 of 3 done/);
});

test("a row lists the files being worked on, at most four, and counts the rest", () => {
  const h = harness();
  h.context.job = replacingBatch({replacement: {approved: true, total: 8, done: 1, failed: 0, members: Array.from({length: 8}, (_, i) => ({item_key: `k${i}`, name: `Show S01E0${i + 1}`, phase: i === 0 ? "done" : i < 7 ? "backup" : "waiting"}))}});
  const text = textOf(h.run("replacementProgressBlock(job)"));
  assert.match(text, /Replacing 8 files · 1 of 8 done/);
  assert.match(text, /S01E02/);
  assert.match(text, /S01E05/);
  assert.doesNotMatch(text, /S01E01|S01E06|S01E08/);
  assert.match(text, /and 2 more/);
});

const failedReplacement = () => replacingBatch({
  status: "failed", error: "1 batch promotion(s) need attention: item-key-9: promotion validation failed: audio bitrate mismatch (truncated",
  replacement: {
    approved: true, total: 3, done: 2, failed: 1, frees_bytes: 5160000000, freed_bytes: 2800000000,
    members: [
      { item_key: "k-a", name: "Sample Show S01E01", phase: "done", freed_bytes: 1600000000 },
      { item_key: "k-b", name: "Sample Show S01E02", phase: "done", freed_bytes: 1200000000 },
      { item_key: "k-c", name: "Sample Show S01E03", phase: "failed", failure: { reason: "final check before replacing found an audio mismatch", original_intact: true, detail: "promotion validation failed: audio bitrate mismatch" } },
    ],
  },
});

test("a batch with one failed replacement says so in plain words, without item keys", () => {
  const h = harness();
  h.context.job = failedReplacement();
  const summary = h.run("queuePresentation(job).summary");
  assert.match(summary, /2 of 3 replaced · 1 failed: final check before replacing found an audio mismatch · original kept/);
  assert.doesNotMatch(summary, /item-key|need attention|truncated/);
  const text = textOf(h.run("batchProgress(job)"));
  assert.match(text, /2 of 3 replaced · 1 failed/);
  assert.match(text, /Sample Show S01E03 Stopped: final check before replacing found an audio mismatch · original kept/);
  assert.doesNotMatch(text, /item-key/);
});

test("the batch headline says 'original kept' only when every failure left the original untouched", () => {
  const h = harness();
  const headline = (failures) => {
    const members = failures.map((f, i) => ({ item_key: `k-${i}`, name: `File ${i}`, phase: "failed", failure: { reason: "the library did not accept the new file", ...f } }));
    h.context.job = { replacement: { approved: true, total: failures.length + 1, done: 1, failed: failures.length, members } };
    return h.run("replacementHeadline(job)");
  };
  const inPlace = { original_state: "in_place", original_intact: true }, recovery = { original_state: "recovery", original_intact: false }, unclear = { original_state: "unclear", original_intact: false };
  assert.match(headline([inPlace]), /original kept$/);
  assert.match(headline([inPlace, inPlace]), /originals kept$/);
  const safe = headline([recovery]);
  assert.match(safe, /original in Recovery$/);
  assert.doesNotMatch(safe, /kept|intact/);
  assert.match(headline([inPlace, recovery]), /check Recovery$/);
  assert.match(headline([unclear]), /check Recovery$/);
  const step = (f) => h.run(`replacementStepText({phase:"failed", failure:${JSON.stringify({ reason: "x", ...f })}})`);
  assert.match(step(recovery), /original in Recovery/);
  assert.match(step(inPlace), /original kept/);
  assert.match(step(unclear), /check Recovery/);
});

async function batchItemsHarness(items, { detailJob = { status: "completed", batch: {} }, destructive = true } = {}) {
  const h = harness();
  h.context.items = items;
  h.context.detailJob = detailJob;
  h.run(`state.info={allow_destructive:${destructive}};state.detail="batch-a";state.detailRevision=2;$("job-detail").open=true;state.detailJob=detailJob;api=async()=>({items,total:items.length});`);
  await h.run('loadBatchItems("batch-a",2)');
  return h;
}
const itemRows = (h) => h.elements.get("batch-items-list").children;
const buttonLabels = (row) => flatten(row).filter((e) => e.type === "button").map((e) => e.textContent);

test("a member of the approved plan that has not started is queued, with no way to replace it again", async () => {
  const items = [
    { item_key: "k-a", display_label: "Sample Show S01E01", status: "completed", child_action_id: "t-a", replacement_plan: "queued", mode: "size", reason_code: "eligible" },
    { item_key: "k-b", display_label: "Sample Show S01E02", status: "completed", child_action_id: "t-b", replacement_plan: "queued" },
  ];
  const h = await batchItemsHarness(items, { detailJob: { status: "running", batch: {}, replacement: { approved: true, total: 2, members: [] } } });
  const row = textOf(itemRows(h)[0]);
  assert.match(row, /Sample Show S01E01/);
  assert.match(row, /Replacement queued \(approved\)/);
  assert.doesNotMatch(row, /Replace file|awaiting approval|View replacement|Candidate verified/);
  assert.deepEqual(buttonLabels(itemRows(h)[0]).filter((l) => /Replace|replacement/i.test(l)), []);
});

test("a candidate waiting for a plan decision says so, and a candidate still unplanned offers Replace file", async () => {
  const items = [
    { item_key: "k-a", display_label: "Sample Show S01E01", status: "completed", child_action_id: "t-a", replacement_plan: "review" },
    { item_key: "k-b", display_label: "Sample Show S01E02", status: "completed", child_action_id: "t-b", candidate_ready: true },
  ];
  const h = await batchItemsHarness(items);
  assert.match(textOf(itemRows(h)[0]), /Replacement awaiting approval/);
  assert.deepEqual(buttonLabels(itemRows(h)[0]).filter((l) => l === "Replace file"), []);
  assert.deepEqual(buttonLabels(itemRows(h)[1]).filter((l) => l === "Replace file"), ["Replace file"]);
});

test("buttons that only open something are not disabled by a busy batch, and none show before they can work", async () => {
  const items = [
    { item_key: "k-a", display_label: "Sample Show S01E01", status: "completed", child_action_id: "t-a", replacement_action_id: "p-a", replacement_status: "running" },
    { item_key: "k-b", display_label: "Sample Show S01E02", status: "completed", child_action_id: "t-b", replacement_plan: "queued" },
  ];
  const h = harness();
  h.context.items = items;
  h.run('state.info={allow_destructive:true};state.detail="batch-a";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"running",batch:{}};state.busyJobs.add("batch-a");api=async()=>({items,total:2});');
  await h.run('loadBatchItems("batch-a",2)');
  const viewReplacement = flatten(itemRows(h)[0]).find((e) => e.textContent === "View replacement");
  assert.ok(viewReplacement);
  assert.equal(viewReplacement.disabled, false);
  // The queued file has no replacement yet, so no such button exists for it.
  assert.equal(buttonLabels(itemRows(h)[1]).includes("View replacement"), false);
  // The same rule holds when the connection returns: navigation stays usable.
  h.run("setConnection(false); setConnection(true);");
  assert.equal(flatten(itemRows(h)[0]).find((e) => e.textContent === "View replacement").disabled, false);
});

test("the single-file job view offers no Replace file while the batch plan covers the candidate", () => {
  const h = harness();
  h.run("state.info={allow_destructive:true};");
  const labels = (job) => JSON.stringify(h.run(`jobControls(${JSON.stringify(job)}).children.map(b => b.textContent)`));
  const base = { id: "t-a", action_name: "transcode_media", status: "completed", candidate_ready: true, parent_action_id: "batch-a" };
  assert.match(labels(base), /Replace file/);
  assert.doesNotMatch(labels({ ...base, replacement_plan: "queued" }), /Replace file/);
  assert.doesNotMatch(labels({ ...base, replacement_plan: "review" }), /Replace file/);
});

const failedItem = (extra = {}) => ({
  item_key: "k-c", display_label: "Sample Show S01E03", status: "completed", child_action_id: "t-c", replacement_action_id: "p-c", replacement_status: "failed",
  replacement_failure: { reason: "final check before replacing found an audio mismatch", original_state: "in_place", original_intact: true, detail: "promotion validation failed: audio bitrate mismatch" },
  retry_action_id: "batch-a", reason_code: "eligible", ...extra,
});

test("a failed replacement row says why, that the original is still in place, and offers retry and details", async () => {
  const h = await batchItemsHarness([failedItem()], { detailJob: { status: "failed", batch: {} } });
  const row = itemRows(h)[0];
  const text = textOf(row);
  assert.match(text, /Replacement failed/);
  assert.match(text, /Replacement stopped: final check before replacing found an audio mismatch/);
  assert.match(text, /Your original file is still in place\./);
  assert.deepEqual(buttonLabels(row).filter((l) => ["Retry replacement", "Keep original", "View details"].includes(l)), ["Retry replacement", "View details"]);
  assert.doesNotMatch(text, /recovery copy/);
  // Technical detail is behind a disclosure; the key and the recipe reason are not shown.
  const technical = flatten(row).find((e) => /technical-reason/.test(e.className));
  assert.match(textOf(technical), /audio bitrate mismatch/);
  assert.doesNotMatch(text, /k-c|eligible/);
  // Retry goes to the action that re-runs only that replacement, naming the file.
  h.context.retries = [];
  h.run("jobControl = async (...args) => retries.push(JSON.parse(JSON.stringify(args)));");
  await flatten(row).find((e) => e.textContent === "Retry replacement").listeners.get("click")();
  const retry = plain(h.context.retries[0]);
  assert.deepEqual(retry.slice(0, 3), ["batch-a", "action_retry", {}]);
  assert.equal(retry[3].decision.label, "Retry replacement of Sample Show S01E03");
});

test("the failure text says where the original is, from the server's state, and never calls it intact when it may have changed", async () => {
  const row = async (original_state) => {
    const h = await batchItemsHarness([failedItem({ replacement_failure: { reason: "the library did not accept the new file", original_state, original_intact: original_state === "in_place", detail: "library did not accept the new file" }, retry_action_id: "p-c" })], { detailJob: { status: "failed", batch: {} } });
    return textOf(itemRows(h)[0]);
  };
  const safe = await row("recovery");
  assert.match(safe, /Your original is safe in its recovery copy\. Check Recovery before trying again\./);
  assert.doesNotMatch(safe, /intact|in place|original kept/i);
  const unclear = await row("unclear");
  assert.match(unclear, /Check Recovery to see where your original is before trying again\./);
  assert.doesNotMatch(unclear, /intact|in place|safe in its recovery|original kept/i);
  assert.match(await row("in_place"), /Your original file is still in place\./);
});

test("Retry goes to the batch for a file that stopped early, to the file itself for one that stopped later, and to nobody when it cannot change", async () => {
  const retries = async (item, detailJob) => {
    const h = await batchItemsHarness([item], { detailJob });
    h.context.calls = [];
    h.run("jobControl = async (...args) => calls.push(JSON.parse(JSON.stringify(args)));");
    const button = flatten(itemRows(h)[0]).find((e) => e.type === "button" && /^Retry/.test(e.textContent));
    if (!button) return { label: null, calls: [] };
    await button.listeners.get("click")();
    return { label: button.textContent, calls: plain(h.context.calls) };
  };
  const failedBatch = { status: "failed", batch: {}, replacement: { approved: true, failed: 1, batch_retry_count: 1, members: [] } };
  const early = await retries(failedItem({ retry_action_id: "batch-a" }), failedBatch);
  assert.equal(early.label, "Retry replacement");
  assert.deepEqual(early.calls[0].slice(0, 2), ["batch-a", "action_retry"]);
  assert.equal(early.calls[0][3].decision.label, "Retry replacement of Sample Show S01E03");
  const late = await retries(failedItem({ retry_action_id: "p-c" }), failedBatch);
  assert.equal(late.label, "Retry replacement");
  assert.deepEqual(late.calls[0].slice(0, 2), ["p-c", "action_retry"]);
  // A failure only the batch recorded would fail the same way again: nothing to retry, the reason still shows.
  const recorded = await retries(failedItem({ replacement_action_id: undefined, replacement_status: undefined, retry_action_id: undefined }), failedBatch);
  assert.equal(recorded.label, null);
});

test("one batch Retry that re-runs several failed files says how many", async () => {
  const detailJob = { status: "failed", batch: {}, replacement: { approved: true, failed: 3, batch_retry_count: 2, members: [] } };
  const h = await batchItemsHarness([failedItem({ retry_action_id: "batch-a" }), failedItem({ item_key: "k-d", display_label: "Sample Show S01E04", retry_action_id: "batch-a" })], { detailJob });
  h.context.calls = [];
  h.run("jobControl = async (...args) => calls.push(JSON.parse(JSON.stringify(args)));");
  const button = flatten(itemRows(h)[0]).find((e) => /^Retry/.test(e.textContent));
  assert.equal(button.textContent, "Retry 2 failed replacements");
  await button.listeners.get("click")();
  assert.equal(plain(h.context.calls)[0][3].decision.label, "Retry 2 failed replacements");
  // A file's own retry is never described as retrying more than that file.
  const own = await batchItemsHarness([failedItem({ retry_action_id: "p-c" })], { detailJob });
  assert.deepEqual(buttonLabels(itemRows(own)[0]).filter((l) => /^Retry/.test(l)), ["Retry replacement"]);
});

test("a batch with failed replacements has one Retry, on each file, and no second batch-level button unless only the batch recorded the failure", () => {
  const h = harness();
  h.run("state.info={allow_destructive:true};");
  const labels = (job) => plain(h.run(`jobControls(${JSON.stringify(job)}).children.map(b => b.textContent)`));
  const batch = { id: "batch-a", action_name: "transcode_batch", status: "failed", batch: { total: 3, promotion_plan_ready: true } };
  assert.ok(labels(batch).includes("Retry"));
  // A file with a retry of its own, or a failure no retry can fix (approve_promotion), leaves only the per-file button.
  assert.ok(!labels({ ...batch, replacement: { approved: true, total: 3, failed: 1, retryable: 1 } }).includes("Retry"));
  assert.ok(!labels({ ...batch, replacement: { approved: true, total: 3, failed: 1 } }).includes("Retry"));
  // Only the batch recorded the failure: no file can retry, so a transient error is retried from the batch.
  assert.ok(labels({ ...batch, replacement: { approved: true, total: 3, failed: 1, batch_recorded_failed: 1 } }).includes("Retry"));
  assert.ok(!labels({ ...batch, replacement: { approved: true, total: 3, failed: 2, retryable: 1, batch_recorded_failed: 1 } }).includes("Retry"));
});

test("an approved replacement drops the completed and failed chips its headline already states", () => {
  const h = harness();
  const chips = (batch, omit) => textOf(h.run(`(() => { const c = document.createElement("div"); renderBatchCounts(c, ${JSON.stringify(batch)}, ${JSON.stringify(omit)}); return c; })()`));
  const batch = { completed: 3, failed: 1, skip: 2 };
  assert.match(chips(batch, []), /3 Completed/);
  const honest = chips(batch, ["completed", "failed"]);
  assert.doesNotMatch(honest, /Completed|Failed/);
  assert.match(honest, /2 Skipped/);
});

test("a file that was replaced shows no recipe reason, and a failed encode still does", async () => {
  const items = [
    { item_key: "k-a", display_label: "Sample Show S01E01", status: "completed", child_action_id: "t-a", replacement_action_id: "p-a", replacement_status: "completed", replaced: true, reason_code: "quality_not_met" },
    { item_key: "k-b", display_label: "Sample Show S01E02", status: "skip", child_action_id: "t-b", reason_code: "already_hevc" },
  ];
  const h = await batchItemsHarness(items);
  assert.doesNotMatch(textOf(itemRows(h)[0]), /quality target/);
  assert.match(textOf(itemRows(h)[1]), /Already HEVC/);
});

test("Where shows the new file and the backup with copy buttons, short by default", () => {
  const h = harness();
  const where = h.run('whereBlock({new_path:"/media/Sample Show/S01E01.mkv", backup_path:"/recovery/S01E01.mkv.bak"})');
  const text = textOf(where);
  assert.match(text, /Where/);
  assert.match(text, /New file:\s+\/media\/Sample Show\/S01E01\.mkv/);
  assert.match(text, /Original backup:\s+\/recovery\/S01E01\.mkv\.bak/);
  assert.match(text, /removed|stays until you remove it from Recovery/);
  assert.equal(flatten(where).filter((e) => e.textContent === "Copy").length, 2);
  assert.match(textOf(h.run('whereBlock({new_path:"/media/x.mkv", backup_removed:true})')), /Original backup:\s+removed after the new file was verified/);
  assert.equal(h.run("whereBlock(null)"), null);
  assert.equal(h.run("whereBlock({})"), null);
});

test("the single-file review uses the same card, the total it frees and a Keep original option", async () => {
  const h = harness();
  h.context.current = { id: "replace", status: "waiting_decision", waiting_options: [{ decision: "approve" }, { decision: "reject" }], replacement_facts: sampleFacts(4e9, 2.4e9), promotion: { original_path: "/media/Sample Show S01E01.mkv", candidate_path: "/media/.candidates/Sample Show S01E01.mkv", original_sha256: "o", candidate_sha256: "c", plan_digest: "sha256:plan" } };
  h.run("api = async () => ({jobs:[current]});");
  await h.run('reviewFilePromotion("replace")');
  const text = textOf(h.elements.get("file-review-data"));
  assert.match(text, /Sample Show S01E01/);
  assert.match(text, /4\.00 GB → 2\.40 GB · Saves 1\.60 GB \(40%\)/);
  assert.match(text, /Quality VMAF 93 · worst 90/);
  assert.equal(h.elements.get("file-review-total").textContent, "Frees 1.60 GB");
  assert.equal(h.elements.get("reject-file-review").hidden, false);
});

test("a review that opens by itself keeps its buttons off until the layout settles, so a stray tap cannot land on Replace file or Keep original", async () => {
  const h = harness();
  h.context.current = { id: "replace", status: "waiting_decision", waiting_options: [{ decision: "approve" }, { decision: "reject" }], replacement_facts: sampleFacts(4e9, 2.4e9), promotion: { original_path: "/media/Sample Show S01E01.mkv", candidate_path: "/media/.candidates/Sample Show S01E01.mkv", original_sha256: "o", candidate_sha256: "c", plan_digest: "sha256:plan" } };
  const reject = h.elements.get("reject-file-review");
  const approve = h.elements.get("approve-file-review");
  const timers = [];
  h.context.setTimeout = (fn, ms) => { timers.push({ fn, ms }); return 1; };
  h.run('serverReachable = true; state.fileApproval = {id:"replace", digest:"", original:"", candidate:""}; showFileReview(current, state.fileApproval, {unprompted: true})');
  assert.equal(reject.disabled && approve.disabled, true, "both stay off right after it opens");
  assert.equal(timers.length, 1);
  assert.ok(timers[0].ms >= 500);
  timers[0].fn();
  assert.ok(!reject.disabled && !approve.disabled, "and wake once the layout has settled");
  // A review the person asked for is ready at once.
  timers.length = 0;
  h.run('state.fileApproval = {id:"replace", digest:"", original:"", candidate:""}; showFileReview(current, state.fileApproval)');
  assert.equal(timers.length, 0);
  assert.ok(!reject.disabled && !approve.disabled);
});

// Languages to keep: the Settings editor, the goal form's track picker, and the
// track report in the job details.
const plain = (value) => JSON.parse(JSON.stringify(value ?? null));

test("language entries accept names, codes and older spellings and say why one is refused", () => {
  const h = harness();
  const parse = (text) => h.run(`parseLanguage(${JSON.stringify(text)})`);
  assert.equal(parse("French"), "fra");
  assert.equal(parse("fre"), "fra");
  assert.equal(parse("FR"), "fra");
  assert.equal(parse(" Spanish (spa) "), "spa");
  assert.equal(parse("untagged"), "und");
  // Any other three letters go to the server, which checks them against ISO 639-2.
  assert.equal(parse("xyz"), "xyz");
  assert.equal(parse(""), "");
  assert.equal(parse("english please"), "");
  assert.equal(parse("e"), "");
  const canonical = (tag) => h.run(`canonicalLanguage(${JSON.stringify(tag)})`);
  assert.equal(canonical("es-419"), "spa");
  assert.equal(canonical("ger"), "deu");
  assert.equal(canonical(""), "und");
  assert.equal(h.run('languageName("fre")'), "French");
  assert.equal(h.run('languageName("und")'), "Untagged");
  assert.equal(h.run('languageName("xyz")'), "xyz");
  assert.deepEqual(plain(h.run('addLanguageTo(["eng"], "Spanish")')), { languages: ["eng", "spa"] });
  assert.match(h.run('addLanguageTo(["eng"], "English")').message, /English is already in the list/);
  assert.match(h.run('addLanguageTo([], "und")').message, /Untagged tracks are always kept/);
  assert.match(h.run('addLanguageTo([], "??")').message, /Enter a language/);
  assert.equal(h.run('sameLanguages(["eng", "spa"], ["spa", "eng"])'), true);
  assert.equal(h.run('sameLanguages(["eng"], ["eng", "spa"])'), false);
});

function languagesHarness() {
  const h = harness();
  h.context.calls = [];
  h.run(`api = async (path, body, opts) => {
    calls.push([path, body, opts?.method]);
    if (opts?.method === "PUT") {
      if (body.languages.includes("xyz")) throw new Error('"xyz" is not an ISO 639-2 language code');
      return { languages: [...body.languages].sort(), default: ["eng", "jpn", "spa"], is_default: false, revision: 2 };
    }
    return { languages: ["eng", "jpn", "spa"], default: ["eng", "jpn", "spa"], is_default: true, revision: 1 };
  };`);
  const el = (id) => h.elements.get(id);
  const chips = () => el("keep-languages-list").children.map((chip) => chip.children[0].textContent);
  return { h, el, chips };
}

test("Settings lists the languages to keep with readable names and disables what does nothing", async () => {
  const { h, el, chips } = languagesHarness();
  h.run("renderKeepLanguages()");
  assert.equal(el("keep-languages-state").textContent, "Loading…");
  assert.equal(el("keep-language-add").disabled, true, "nothing to edit before the list is read");
  await h.run("loadKeepLanguages()");
  assert.deepEqual(plain(h.context.calls), [["keep-languages", null, null]]);
  assert.deepEqual(chips(), ["English", "Japanese", "Spanish"]);
  assert.equal(el("keep-languages-list").children[0].children[1].textContent, "eng");
  assert.equal(el("keep-languages-state").textContent, "Default");
  assert.equal(el("keep-languages-apply").disabled, true, "nothing changed yet");
  assert.equal(el("keep-languages-reset").disabled, true, "already the default");
  assert.equal(el("keep-languages-off").disabled, false);
  assert.equal(el("keep-language-add").disabled, false);
});

test("Settings stages additions and removals and saves them with Apply", async () => {
  const { h, el, chips } = languagesHarness();
  await h.run("loadKeepLanguages()");
  // Add by name, both with the button and with Enter, which must not submit the job form.
  el("keep-language-input").value = "French";
  el("keep-language-add").listeners.get("click")();
  assert.deepEqual(chips(), ["English", "French", "Japanese", "Spanish"]);
  assert.equal(el("keep-language-input").value, "");
  assert.equal(el("keep-languages-state").textContent, "Not saved yet");
  assert.equal(el("keep-languages-apply").disabled, false);
  let prevented = 0;
  el("keep-language-input").value = "German";
  el("keep-language-input").listeners.get("keydown")({ key: "Enter", preventDefault: () => prevented++ });
  assert.equal(prevented, 1);
  assert.deepEqual(chips(), ["English", "French", "German", "Japanese", "Spanish"]);
  el("keep-language-input").listeners.get("keydown")({ key: "a", preventDefault: () => prevented++ });
  assert.equal(prevented, 1, "other keys are left alone");
  // Remove one with its own button.
  el("keep-languages-list").children[2].children[2].listeners.get("click")();
  assert.deepEqual(chips(), ["English", "French", "Japanese", "Spanish"]);
  // A refused entry says why and changes nothing.
  el("keep-language-input").value = "English";
  el("keep-language-add").listeners.get("click")();
  assert.match(el("keep-languages-note").textContent, /already in the list/);
  assert.equal(chips().length, 4);
  // Nothing is sent until Apply.
  assert.equal(h.context.calls.length, 1);
  await h.run("saveKeepLanguages(keepLanguages.draft)");
  assert.deepEqual(plain(h.context.calls[1]), ["keep-languages", { languages: ["eng", "jpn", "spa", "fra"] }, "PUT"]);
  assert.match(el("keep-languages-note").textContent, /^Saved\. It applies to jobs created from now on/);
  assert.equal(el("keep-languages-state").textContent, "Custom");
  assert.equal(el("keep-languages-apply").disabled, true);
  assert.equal(el("keep-languages-reset").disabled, false);
});

test("Settings resets to the default and turns the setting off", async () => {
  const { h, el, chips } = languagesHarness();
  await h.run("loadKeepLanguages()");
  el("keep-languages-off").listeners.get("click")();
  assert.deepEqual(chips(), []);
  assert.equal(el("keep-languages-list").hidden, true);
  assert.equal(el("keep-languages-state").textContent, "Not saved yet");
  assert.equal(el("keep-languages-off").disabled, true);
  await h.run("saveKeepLanguages(keepLanguages.draft)");
  assert.deepEqual(plain(h.context.calls[1]), ["keep-languages", { languages: [] }, "PUT"]);
  assert.equal(el("keep-languages-state").textContent, "Off: every track is kept");
  el("keep-languages-reset").listeners.get("click")();
  assert.deepEqual(chips(), ["English", "Japanese", "Spanish"]);
  assert.equal(el("keep-languages-state").textContent, "Not saved yet");
  await h.run("saveKeepLanguages(keepLanguages.draft)");
  assert.deepEqual(plain(h.context.calls[2][1]), { languages: ["eng", "jpn", "spa"] });
  assert.equal(el("keep-languages-state").textContent, "Default");
});

test("Settings shows the server's refusal and a reload never replaces an edited draft", async () => {
  const { h, el, chips } = languagesHarness();
  await h.run("loadKeepLanguages()");
  el("keep-language-input").value = "xyz";
  el("keep-language-add").listeners.get("click")();
  await h.run("saveKeepLanguages(keepLanguages.draft)");
  assert.match(el("keep-languages-note").textContent, /"xyz" is not an ISO 639-2 language code/);
  assert.equal(el("keep-languages-state").textContent, "Not saved yet");
  assert.ok(chips().includes("xyz"), "the draft is kept so it can be corrected");
  assert.equal(el("keep-languages-apply").disabled, false, "and saved again");
  // Another tab changed the saved list; the unsaved draft stays.
  h.run('api = async () => ({ languages: ["eng"], default: ["eng", "jpn", "spa"], is_default: false, revision: 3 });');
  await h.run("loadKeepLanguages()");
  assert.deepEqual(plain(h.run("keepLanguages.saved")), ["eng"]);
  assert.ok(chips().includes("xyz"));
  // A list that cannot be read is said so and not edited.
  const failed = languagesHarness();
  failed.h.run('api = async () => { throw new Error("down"); };');
  await failed.h.run("loadKeepLanguages()");
  assert.equal(failed.el("keep-languages-state").textContent, "Could not load the list");
  assert.equal(failed.el("keep-language-add").disabled, true);
});

function keepFixture() {
  return {
    languages: ["eng", "jpn", "spa"],
    audio: [
      { index: 1, language: "eng", title: "Surround", codec: "eac3", channels: 6, default: true, original: true, keep: true },
      { index: 2, language: "fra", title: "", codec: "ac3", channels: 6, keep: false },
      { index: 3, language: "und", title: "Commentary", codec: "aac", channels: 2, keep: true },
      { index: 4, language: "jpn", title: "", codec: "dts", channels: 6, keep: true },
    ],
    subtitles: [
      { index: 5, language: "spa", title: "", codec: "subrip", keep: true },
      { index: 6, language: "deu", title: "Forced", codec: "subrip", forced: true, keep: false },
      { index: 7, language: "und", title: "", codec: "hdmv_pgs_subtitle", keep: true },
    ],
  };
}
function keepHarness() {
  const h = harness({ radios: true });
  h.context.tracks = keepFixture();
  h.run(`$("scope").value = "file"; $("service").value = "folder:/media"; $("path").value = "/media/movie.mkv"; $("technical").checked = false;
    keepLanguages.status = "ready"; keepLanguages.saved = ["eng", "jpn", "spa"]; keepLanguages.draft = ["eng", "jpn", "spa"]; keepLanguages.defaults = ["eng", "jpn", "spa"];
    var queries = []; api = async (path) => { queries.push(path); return tracks; };
    controls = () => { refreshKeepTracks(); renderKeepTracks(); };`);
  const el = (id) => h.elements.get(id);
  const open = async () => {
    h.run("controls()");
    await h.run("loadKeepTracks(keepTracks.key, keepTracks.seq)");
  };
  const boxes = (kind) => plain(h.run(`keepTracks.boxes.filter(e => e.kind === "${kind}").map(e => e.box.checked)`));
  const inputs = (single = true, path = "/media/movie.mkv") => plain(h.run(`keepTrackInputs(${single}, ${JSON.stringify(path)})`));
  return { h, el, open, boxes, inputs };
}

test("the picker lists the file's tracks, checked as the server's rules keep them, and a default submit sends nothing", async () => {
  const { h, el, open, boxes, inputs } = keepHarness();
  h.run("controls()");
  assert.equal(el("keep-tracks").hidden, false);
  assert.equal(el("keep-tracks-status").textContent, "Reading the file's tracks…");
  await h.run("loadKeepTracks(keepTracks.key, keepTracks.seq)");
  assert.deepEqual(plain(h.run("queries")), ["source-tracks?path=%2Fmedia%2Fmovie.mkv"]);
  assert.deepEqual(boxes("audio"), [true, false, true, true]);
  assert.deepEqual(boxes("subtitles"), [true, false, true]);
  assert.equal(el("keep-audio").hidden, false);
  assert.equal(el("keep-audio-list").children.length, 4);
  assert.equal(el("keep-subtitle-list").children.length, 3);
  assert.equal(el("keep-tracks-summary").textContent, "Saved list: English, Japanese, Spanish");
  assert.equal(el("keep-tracks-status").textContent, "Keeping 3 of 4 audio tracks and 2 of 3 subtitles.");
  assert.equal(el("keep-tracks-reset").hidden, true);
  // Language, codec, channels and title read as one line; badges say what the track is.
  const first = el("keep-audio-list").children[0].children[0];
  assert.equal(first.children[1].children[0].textContent, "English");
  assert.equal(first.children[1].children[1].textContent, "E-AC-3 · 5.1 · Surround");
  assert.deepEqual(first.children[2].children.map((badge) => badge.textContent), ["Original", "Default"]);
  assert.equal(el("keep-audio-list").children[2].children[0].children[1].children[0].textContent, "Untagged");
  assert.deepEqual(inputs(), {}, "the automatic choice sends no stream lists");
  assert.deepEqual(inputs(false), {});
});

// Every leaf's text under a preview, in order.
const leaves = (n) => (n.children.length ? n.children.flatMap(leaves) : [n.textContent]);
function sizedFixture() {
  const out = (output, kbps, bytes) => ({ output, kbps, bytes });
  const tracks = keepFixture();
  Object.assign(tracks, { size_bytes: 10e9, duration_sec: 5000, video_bytes: 3e9, video_approx: true });
  Object.assign(tracks.audio[0], { bytes: 4e9, kbps: 6400, outputs: { size: out("aac", 384, 240e6), size_he: out("he_aac", 160, 100e6), compact: out("aac", 384, 240e6) } });
  Object.assign(tracks.audio[1], { bytes: 280e6, kbps: 448, outputs: { size: out("copy", 0, 280e6), size_he: out("he_aac", 160, 100e6), compact: out("copy", 0, 280e6) } });
  Object.assign(tracks.audio[2], { bytes: 80e6, kbps: 128, outputs: { size: out("copy", 0, 80e6), size_he: out("he_aac", 64, 40e6), compact: out("copy", 0, 80e6) } });
  Object.assign(tracks.audio[3], { bytes: 2.5e9, kbps: 4000, outputs: { size: out("aac", 384, 240e6), size_he: out("he_aac", 160, 100e6), compact: out("aac", 384, 240e6) } });
  tracks.subtitles[0].bytes = 50e3;
  tracks.subtitles[1].bytes = 40e3;
  return tracks;
}

test("one file's tracks show their size and what the goal makes of them, and audio heavier than the video is called out", async () => {
  const { h, el, open } = keepHarness();
  h.context.tracks = sizedFixture();
  await open();
  const plan = (list, i) => el(list).children[i].children[0].children[1].children[2];
  assert.equal(plan("keep-audio-list", 0).textContent, "4.00 GB (40%) → AAC 384 kbps · 240.0 MB");
  assert.equal(plan("keep-audio-list", 1).textContent, "280.0 MB (3%) → Removed");
  assert.equal(plan("keep-audio-list", 2).textContent, "80.0 MB (1%) → Copied");
  assert.equal(plan("keep-subtitle-list", 0).textContent, "50.0 KB");
  assert.equal(plan("keep-subtitle-list", 2).hidden, true, "a subtitle of unknown size kept says nothing");
  const preview = leaves(el("keep-tracks-preview"));
  assert.equal(el("keep-tracks-preview").hidden, false);
  assert.deepEqual(preview.slice(0, 10), [
    "File", "10.00 GB",
    "Video", "≈ 3.00 GB · 30% · re-encoded, its new size is set by the encode",
    "Audio kept", "6.58 GB · 66% → about 560.0 MB",
    "Subtitles kept", "50.0 KB",
    "Tracks removed", "−280.0 MB · 3%",
  ]);
  assert.ok(preview.includes("Audio and track choices alone save about 6.30 GB, 63% of the file; the video encode saves on top of that."));
  assert.ok(preview.includes("Stronger audio (HE-AAC: stereo 64, 5.1 160 kbps) would save about 320.0 MB more. A size budget switches to it when the budget needs it."));
  assert.ok(preview.includes("Audio weighs more than the video in this file (6.58 GB against 3.00 GB). After this goal it is about 560.0 MB."));
  assert.equal(el("keep-tracks-summary").textContent, "Audio outweighs video · Saved list: English, Japanese, Spanish");

  // Under compact the stronger level is not offered; a goal that copies the audio says so.
  h.run('setProcessingMode("compact"); renderKeepTracks()');
  assert.ok(!leaves(el("keep-tracks-preview")).some((t) => t.startsWith("Stronger audio")));
  h.run('streamPreview($("keep-tracks-preview"), keepTracks.data, "quality", () => true)');
  const copied = leaves(el("keep-tracks-preview"));
  assert.ok(copied.includes("Audio kept") && copied.includes("6.86 GB · 69% · copied"));
  assert.ok(copied.some((t) => t.includes("This goal copies it, so most of the file stays as it is")));

  // A converted track of unknown size stays out of both audio totals, and a
  // conversion that grows a track still reads as a conversion, not a copy.
  h.run(`streamPreview($("keep-tracks-preview"), {size_bytes: 1e9, video_bytes: 300e6, subtitles: [], audio: [
    {index: 1, keep: true, bytes: 500e6, outputs: {compact: {output: "copy", bytes: 500e6}}},
    {index: 2, keep: true, bytes: 0, outputs: {compact: {output: "aac", kbps: 384, bytes: 200e6}}}]}, "compact", (k, t) => t.keep)`);
  let shown = leaves(el("keep-tracks-preview"));
  assert.ok(shown.includes("500.0 MB · 50% → about 500.0 MB"), shown.join(" | "));
  assert.ok(shown.includes("One kept audio track states no size, so it is left out of these totals."));
  assert.ok(shown.some((t) => t.startsWith("Audio weighs more than the video") && t.endsWith("After this goal it is about 500.0 MB.")));
  h.run(`streamPreview($("keep-tracks-preview"), {size_bytes: 1e9, video_bytes: 300e6, subtitles: [], audio: [
    {index: 1, keep: true, bytes: 400e6, outputs: {compact: {output: "aac", kbps: 128, bytes: 500e6}}}]}, "compact", (k, t) => t.keep)`);
  shown = leaves(el("keep-tracks-preview"));
  assert.ok(shown.includes("400.0 MB · 40% → about 500.0 MB"), shown.join(" | "));
  assert.ok(!shown.some((t) => t.includes("This goal copies it")));

  // A file the probe could not size shows no preview.
  h.run('streamPreview($("keep-tracks-preview"), {audio: [], subtitles: []}, "size", () => true)');
  assert.equal(el("keep-tracks-preview").hidden, true);
});

test("Audio only previews the video as copied and what the audio alone saves", () => {
  const h = harness();
  h.context.tracks = sizedFixture();
  h.run('streamPreview($("keep-tracks-preview"), tracks, "audio_only", (k, t) => t.keep)');
  const shown = leaves(h.elements.get("keep-tracks-preview"));
  assert.ok(shown.includes("≈ 3.00 GB · 30% · copied unchanged"), shown.join(" | "));
  assert.ok(shown.includes("6.58 GB · 66% → about 560.0 MB"));
  assert.ok(shown.includes("This goal saves about 6.30 GB, 63% of the file, from the audio and the tracks removed; the video stays as it is."));
  assert.ok(!shown.some((t) => t.startsWith("Stronger audio")));
  assert.equal(h.run('trackOutputText(tracks.audio[0], "audio_only")'), "AAC 384 kbps · 240.0 MB");
  // Nothing converts and nothing is removed: the goal has nothing to save.
  h.run(`streamPreview($("keep-tracks-preview"), {size_bytes: 1e9, video_bytes: 800e6, subtitles: [], audio: [
    {index: 1, keep: true, bytes: 100e6, outputs: {size: {output: "copy", bytes: 100e6}}}]}, "audio_only", () => true)`);
  assert.ok(leaves(h.elements.get("keep-tracks-preview")).some((t) => t.startsWith("Nothing here gets smaller")));
});

test("Change goal offers Audio only, shows a saved one and sends it as the size goal", async () => {
  const h = harness();
  h.context.plan = { id: "b", mode: "size", scope: "all", selected: 2, kept: 0, active: 0, files: ["one", "two"], selection_version: "v1", settings: { allow_hardware: true, audio_only: true } };
  h.run('state.info={};state.recipes=[];api=async()=>plan;');
  await h.run('reconfigureBatch("b")');
  const el = (id) => h.elements.get(id);
  assert.equal(el("reconfigure-mode").value, "audio_only");
  h.run("updateReconfigureHardware()");
  assert.equal(el("reconfigure-hardware-option").hidden, true);
  const calls = [];
  h.context.record = (path, body) => calls.push(body);
  h.context.sessionStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  h.run('submissionID=()=>"audio-1";commandRequest=async(path,body)=>{record(path,body);return {id:"b"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};');
  await h.run("submitReconfiguredBatch()");
  assert.equal(calls[0].mode, "size");
  assert.equal(calls[0].audio_only, true);
  assert.equal("allow_hardware" in calls[0], false);
  // Save space picked instead sends no audio_only.
  h.run('$("reconfigure-mode").value="size"; updateReconfigureHardware(); submissionID=()=>"audio-2";');
  await h.run("submitReconfiguredBatch()");
  assert.equal(calls[1].mode, "size");
  assert.equal("audio_only" in calls[1], false);
  // Encoding one file again keeps its goal.
  assert.equal(h.run('JSON.stringify(sameSettingsRequest({id:"b",item_key:"f",mode:"size",settings:{audio_only:true,allow_hardware:true}}))').includes('"audio_only":true'), true);
  assert.equal(h.run('sameSettingsRequest({id:"b",item_key:"f",mode:"size",settings:{audio_only:true}}).allow_hardware'), undefined);
});

test("one file's Change goal previews its streams under the goal picked", async () => {
  const h = harness();
  h.context.tracks = sizedFixture();
  h.context.plan = { id: "batch", mode: "size", item_key: "f1", source_path: "/media/movie.mkv", scope: "file", selected: 1, kept: 0, files: ["movie.mkv"], settings: {} };
  h.run(`var queries = []; api = async (path) => { queries.push(path); return path.startsWith("source-tracks") ? tracks : plan; };`);
  await h.run('reconfigureBatch("batch", false, {item_key: "f1"})');
  await h.run("new Promise(resolve => resolve())");
  assert.ok(h.run("queries").includes("source-tracks?path=%2Fmedia%2Fmovie.mkv"));
  const el = (id) => h.elements.get(id);
  assert.equal(el("reconfigure-preview").hidden, false);
  assert.ok(leaves(el("reconfigure-preview")).includes("6.58 GB · 66% → about 560.0 MB"));
  assert.ok(leaves(el("reconfigure-preview")).includes("Tracks as the saved language list keeps them."));
  // Strict quality keeps and copies every track.
  el("reconfigure-mode").value = "quality";
  el("reconfigure-mode").listeners.get("change")();
  assert.ok(leaves(el("reconfigure-preview")).includes("6.86 GB · 69% · copied"));
  assert.ok(!leaves(el("reconfigure-preview")).includes("Tracks as the saved language list keeps them."));
  // A wider scope is not this file any more: the preview goes until it is.
  h.context.plan = { ...h.context.plan, scope: "all", selected: 3, files: ["a.mkv", "b.mkv", "c.mkv"], source_path: undefined };
  el("reconfigure-scope").value = "all";
  el("reconfigure-scope").listeners.get("change")();
  assert.equal(el("reconfigure-preview").hidden, true);
  el("reconfigure-scope").value = "file";
  el("reconfigure-mode").listeners.get("change")();
  assert.equal(el("reconfigure-preview").hidden, false);
});

test("hand-picked tracks send explicit lists, and picking the automatic choice again sends none", async () => {
  const { h, el, open, boxes, inputs } = keepHarness();
  await open();
  h.run('toggleTrack("subtitles", 6, true)');
  assert.deepEqual(boxes("subtitles"), [true, true, true]);
  assert.deepEqual(inputs(), { keep_audio_streams: [1, 3, 4], keep_subtitle_streams: [5, 6, 7] });
  assert.equal(el("keep-tracks-summary").textContent, "Tracks chosen by hand");
  assert.equal(el("keep-tracks-reset").hidden, false);
  assert.equal(el("keep-tracks-status").textContent, "Keeping 3 of 4 audio tracks and 3 of 3 subtitles.");
  h.run('toggleTrack("audio", 2, true); toggleTrack("subtitles", 5, false)');
  assert.deepEqual(inputs(), { keep_audio_streams: [1, 2, 3, 4], keep_subtitle_streams: [6, 7] });
  // The boxes are the user's own again once they match what the rules would do.
  h.run('toggleTrack("audio", 2, false); toggleTrack("subtitles", 5, true); toggleTrack("subtitles", 6, false)');
  assert.equal(h.run("keepTracks.picks"), null);
  assert.deepEqual(inputs(), {});
  assert.equal(el("keep-tracks-reset").hidden, true);
  // The reset button drops hand-picked boxes.
  h.run('toggleTrack("audio", 4, false)');
  assert.deepEqual(boxes("audio"), [true, false, true, false]);
  el("keep-tracks-reset").listeners.get("click")();
  assert.deepEqual(boxes("audio"), [true, false, true, true]);
  assert.deepEqual(inputs(), {});
  // Only this file's picks apply to it, and a batch never takes stream lists.
  h.run('toggleTrack("audio", 4, false)');
  assert.deepEqual(inputs(true, "/media/other.mkv"), {});
  assert.deepEqual(inputs(false), {});
});

test("a file always keeps at least one audio track", async () => {
  const { h, open, boxes, inputs } = keepHarness();
  await open();
  h.run('toggleTrack("audio", 1, false); toggleTrack("audio", 3, false)');
  assert.deepEqual(boxes("audio"), [false, false, false, true]);
  assert.equal(h.run("keepTracks.boxes.find(e => e.kind === 'audio' && e.track.index === 4).box.disabled"), true, "the last one cannot be unticked");
  assert.equal(h.run("keepTracks.boxes.find(e => e.kind === 'audio' && e.track.index === 3).box.disabled"), false);
  h.run('toggleTrack("audio", 4, false)');
  assert.deepEqual(boxes("audio"), [false, false, false, true]);
  assert.deepEqual(inputs(), { keep_audio_streams: [4], keep_subtitle_streams: [5, 7] });
  // A box change goes through the checkbox's own listener.
  h.run('keepTracks.boxes[0].box.checked = true; keepTracks.boxes[0].box.listeners.get("change")()');
  assert.deepEqual(boxes("audio"), [true, false, false, true]);
  // Should an empty audio list ever reach a submit, it is refused rather than sent.
  h.run("keepTracks.picks.audio.clear()");
  assert.throws(() => h.run('keepTrackInputs(true, "/media/movie.mkv")'), /Keep at least one audio track/);
});

test("this job's languages and every track change what the picker reads and sends", async () => {
  const { h, el, open, boxes, inputs } = keepHarness();
  await open();
  h.run('toggleTrack("subtitles", 6, true)');
  // Editing this job's languages starts from the saved list and reads the file again.
  h.run('setKeepScope("job")');
  assert.deepEqual(plain(h.run("keepTracks.languages")), ["eng", "jpn", "spa"]);
  assert.equal(h.run("keepTracks.picks"), null, "the boxes follow the languages again");
  assert.equal(h.run("keepTracks.key"), "path=%2Fmedia%2Fmovie.mkv&keep_languages=eng%2Cjpn%2Cspa");
  assert.equal(el("keep-job-languages").hidden, false);
  assert.equal(el("keep-tracks-summary").textContent, "This job: English, Japanese, Spanish");
  assert.deepEqual(inputs(), { keep_languages: ["eng", "jpn", "spa"] });
  h.context.tracks = { ...keepFixture(), languages: ["eng", "fra", "jpn", "spa"] };
  h.context.tracks.audio[1].keep = true;
  el("keep-job-input").value = "French";
  el("keep-job-add").listeners.get("click")();
  assert.deepEqual(plain(h.run("keepTracks.languages")), ["eng", "jpn", "spa", "fra"]);
  await h.run("loadKeepTracks(keepTracks.key, keepTracks.seq)");
  assert.equal(h.run("queries.at(-1)"), "source-tracks?path=%2Fmedia%2Fmovie.mkv&keep_languages=eng%2Cjpn%2Cspa%2Cfra");
  assert.deepEqual(boxes("audio"), [true, true, true, true]);
  assert.deepEqual(inputs(), { keep_languages: ["eng", "jpn", "spa", "fra"] });
  // Removing a language goes through its chip.
  el("keep-job-list").children.find((chip) => chip.children[0].textContent === "French").children[2].listeners.get("click")();
  assert.deepEqual(plain(h.run("keepTracks.languages")), ["eng", "jpn", "spa"]);
  // Every track: an empty list, which the server reads as no filtering.
  h.run('setKeepScope("all")');
  assert.equal(h.run("keepTracks.key"), "path=%2Fmedia%2Fmovie.mkv&keep_languages=");
  assert.deepEqual(inputs(), { keep_languages: [] });
  assert.equal(el("keep-tracks-summary").textContent, "Keep every track");
  assert.equal(el("keep-job-languages").hidden, true);
  // Back to the saved list sends nothing again.
  h.run('setKeepScope("saved")');
  assert.deepEqual(inputs(), {});
  // A batch has no track list but still takes the language choice.
  h.run('setKeepScope("job"); toggleTrack("audio", 1, true)');
  assert.deepEqual(inputs(false), { keep_languages: ["eng", "jpn", "spa"] });
});

test("a language list for a batch is chosen without reading any file's tracks", () => {
  const { h, el } = keepHarness();
  h.run('$("scope").value = "batch"; state.folderSelected = new Set(["/media/a.mkv", "/media/b.mkv"]); setKeepScope("job"); renderKeepTracks();');
  assert.equal(el("keep-tracks").hidden, false);
  assert.equal(el("keep-tracks-file").hidden, true);
  assert.equal(h.run("queries.length"), 0);
  assert.deepEqual(plain(h.run("keepTrackInputs(false, '/media/a.mkv')")), { keep_languages: ["eng", "jpn", "spa"] });
  // Goals that keep no tracks, and technical profiles, hide the whole choice.
  h.run('$("scope").value = "file"; $("technical").checked = true; renderKeepTracks();');
  assert.equal(el("keep-tracks").hidden, true);
  h.run('$("technical").checked = false; document.querySelectorAll = () => []; processingMode = () => "quality"; renderKeepTracks();');
  assert.equal(el("keep-tracks").hidden, true);
});

test("a new file starts from the automatic choice, and a stale or failed read cannot change the boxes", async () => {
  const { h, el, open, boxes } = keepHarness();
  await open();
  h.run('toggleTrack("audio", 4, false)');
  // Another file: picks are gone at once and the old tracks are not shown for it.
  h.run('$("path").value = "/media/other.mkv"; controls();');
  assert.equal(h.run("keepTracks.picks"), null);
  assert.equal(h.run("keepTracks.path"), "/media/other.mkv");
  assert.equal(el("keep-audio-list").children.length, 0);
  assert.equal(el("keep-tracks-status").textContent, "Reading the file's tracks…");
  // A read for the file the user left finishes late and is ignored.
  const late = deferred();
  h.context.late = late.promise;
  h.run("api = async () => late;");
  const staleKey = h.run("keepTracks.key"), staleSeq = h.run("keepTracks.seq");
  const pending = h.run(`loadKeepTracks(${JSON.stringify(staleKey)}, ${staleSeq})`);
  h.run('$("path").value = "/media/third.mkv"; controls();');
  late.resolve(keepFixture());
  await pending;
  assert.equal(h.run("keepTracks.data"), null);
  assert.equal(h.run("keepTracks.status"), "loading");
  // The server's own refusal is shown; any other failure leaves the job on the languages.
  h.run('api = async () => { throw Object.assign(new Error("keep_languages: not a language"), { status: 400 }); };');
  await h.run("loadKeepTracks(keepTracks.key, keepTracks.seq)");
  assert.equal(el("keep-tracks-status").textContent, "keep_languages: not a language");
  h.run('api = async () => { throw new Error("down"); };');
  await h.run("loadKeepTracks(keepTracks.key, keepTracks.seq)");
  assert.equal(el("keep-tracks-status").textContent, "Could not read this file's tracks. The job still follows the languages.");
  assert.equal(h.run("keepTracks.status"), "error");
  assert.deepEqual(plain(h.run("keepTrackInputs(true, '/media/third.mkv')")), {});
  assert.deepEqual(boxes("audio"), []);
});

test("saving a different list in Settings refreshes the picker's pre-checked tracks", async () => {
  const { h, open } = keepHarness();
  await open();
  const before = h.run("queries.length");
  h.run('api = async (path, body, opts) => opts?.method === "PUT" ? { languages: ["eng", "fra"], default: ["eng", "jpn", "spa"] } : tracks;');
  await h.run('saveKeepLanguages(["eng", "fra"])');
  assert.deepEqual(plain(h.run("keepLanguages.saved")), ["eng", "fra"]);
  assert.equal(h.run("keepTracks.status"), "loading", "the file is read again with the new list");
  assert.ok(h.run("keepTracks.key") !== "", "and the key stays for the same file");
  assert.equal(h.run("queries.length"), before, "the read itself waits for the debounce");
});

test("submitting a single Save space or Shrink job sends the picker's choice, and other goals send none", async () => {
  const { h } = submissionHarness({ radios: true });
  const sent = [];
  h.context.capture = (_tool, args) => { sent.push(JSON.parse(args.inputs)); return { id: "submitted" }; };
  h.run('tool = async (...args) => capture(...args); $("technical").checked = false; keepLanguages.saved = ["eng"];');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[0].mode, "size");
  assert.equal("keep_languages" in sent[0] || "keep_audio_streams" in sent[0], false, "the saved list is applied by the server");
  // Hand-picked tracks, for this file only.
  h.run(`keepTracks.path = "/media/one.mp4"; keepTracks.scope = "job"; keepTracks.languages = ["eng", "fra"];
    keepTracks.picks = { audio: new Set([4, 1]), subtitles: new Set([5]) }; $("path").value = "/media/one.mp4"; $("scope").value = "file"; state.folderSelected = new Set(["/media/one.mp4"]);`);
  await h.run('buildAndSubmitJob("encode")');
  assert.deepEqual(sent[1].keep_languages, ["eng", "fra"]);
  assert.deepEqual(sent[1].keep_audio_streams, [1, 4]);
  assert.deepEqual(sent[1].keep_subtitle_streams, [5]);
  // A finished submission starts the next file from the saved list.
  assert.equal(h.run("keepTracks.scope"), "saved");
  assert.equal(h.run("keepTracks.picks"), null);
  // Quality keeps every track as it always did, and says nothing about them.
  h.run('processingMode = () => "quality"; keepTracks.scope = "job"; keepTracks.languages = ["fra"]; $("path").value = "/media/one.mp4"; state.folderSelected = new Set(["/media/one.mp4"]); state.fileStep = "configure";');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[2].mode, "quality");
  assert.equal("keep_languages" in sent[2], false);
  // The shrink goal takes the same choice.
  h.run('processingMode = () => "compact"; keepTracks.scope = "all"; $("path").value = "/media/one.mp4"; state.folderSelected = new Set(["/media/one.mp4"]); state.fileStep = "configure";');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[3].mode, "compact");
  assert.deepEqual(sent[3].keep_languages, []);
});

function trackReportFixture(overrides = {}) {
  return {
    id: "finished",
    action_name: "transcode_media",
    status: "completed",
    mode: "size",
    source_path: "/media/Show/Episode 1.mkv",
    candidate_ready: true,
    tracks_message: "Kept 3 of 4 audio tracks (eng, und, jpn) and 2 of 3 subtitles; dropping the rest saves about 700.0 MB",
    tracks: {
      audio: {
        total: 4,
        kept: [
          { index: 1, language: "eng", title: "Surround", codec: "eac3", bytes: 1200000000 },
          { index: 3, language: "und", title: "Commentary", codec: "aac", bytes: 90000000 },
          { index: 4, language: "jpn", codec: "dts", bytes: 1800000000 },
        ],
        dropped: [{ index: 2, language: "fre", codec: "ac3", bytes: 700000000 }],
      },
      subtitles: {
        total: 3,
        kept: [{ index: 5, language: "spa", codec: "subrip" }, { index: 7, language: "", codec: "hdmv_pgs_subtitle", bytes: 4000000 }],
        dropped: [{ index: 6, language: "deu", title: "Forced", codec: "subrip" }],
      },
      bytes_saved: 700000000,
      languages: ["eng", "jpn", "spa"],
      message: "Kept 3 of 4 audio tracks (eng, und, jpn) and 2 of 3 subtitles; dropping the rest saves about 700.0 MB",
    },
    ...overrides,
  };
}

test("the job details report what was kept and dropped, in a line with the tracks behind it", () => {
  const h = harness();
  h.context.job = trackReportFixture();
  const box = h.run("trackReport(job)");
  const [line, details] = box.children;
  assert.equal(line.children[0].textContent, "Tracks: ");
  assert.match(line.children[1].textContent, /^Kept 3 of 4 audio tracks .* saves about 700\.0 MB$/);
  assert.equal(details.className, "track-report-details");
  const headings = details.children.filter((child) => child.textContent.includes(" kept"));
  assert.deepEqual(headings.map((heading) => heading.textContent), ["Audio · 3 of 4 kept", "Subtitles · 2 of 3 kept"]);
  const rows = (list) => list.children.map((row) => row.children.map((cell) => cell.textContent).join(" | "));
  const lists = details.children.filter((child) => child.className === "track-report-list");
  assert.deepEqual(rows(lists[0]), [
    "Kept | English | E-AC-3 · Surround · 1.20 GB",
    "Dropped | French | AC-3 · 700.0 MB",
    "Kept | Untagged | AAC · Commentary · 90.0 MB",
    "Kept | Japanese | DTS · 1.80 GB",
  ]);
  assert.deepEqual(lists[0].children.map((row) => row.className), ["track-report-row kept", "track-report-row dropped", "track-report-row kept", "track-report-row kept"]);
  assert.deepEqual(rows(lists[1]), [
    "Kept | Spanish | SRT",
    "Dropped | German | SRT · Forced",
    "Kept | Untagged | PGS · 4.0 MB",
  ]);
});

test("the track report is one line where only the message is known, and absent without one", () => {
  const h = harness();
  // A queue row carries the message and not the lists.
  h.context.job = trackReportFixture({ tracks: undefined });
  assert.equal(h.run("trackReport(job)").children.length, 1);
  h.context.job = trackReportFixture();
  h.context.job.tracks.audio.total = 0;
  h.context.job.tracks.subtitles.total = 0;
  assert.equal(h.run("trackReport(job)").children.length, 1, "nothing to expand");
  h.context.job = trackReportFixture({ tracks_message: "", tracks: undefined });
  assert.equal(h.run("trackReport(job)"), null);
  h.context.job = { id: "plain", action_name: "transcode_media", status: "completed" };
  assert.equal(h.run("trackReport(job)"), null);
});

test("the track list stays open for its job while the details refresh", () => {
  const h = harness();
  h.context.job = trackReportFixture();
  const first = h.run("trackReport(job)").children[1];
  assert.equal(first.open, false);
  first.open = true;
  first.listeners.get("toggle")();
  assert.equal(h.run("state.trackReportOpen"), "finished");
  assert.equal(h.run("trackReport(job).children[1].open"), true);
  h.run('job = { ...job, id: "another" }');
  assert.equal(h.run("trackReport(job).children[1].open"), false);
  first.open = false;
  first.listeners.get("toggle")();
  assert.equal(h.run("state.trackReportOpen"), null);
});

test("untagged tracks that were kept are detected from the report, not from a manual choice", () => {
  const h = harness();
  const kept = (tracks) => h.run(`keptUntagged(${JSON.stringify(tracks)})`);
  const report = trackReportFixture().tracks;
  assert.equal(kept(report), true);
  assert.equal(kept({ ...report, manual: true }), false, "hand-picked tracks were chosen, not defaulted");
  assert.equal(kept({ audio: { total: 1, kept: [{ index: 1, language: "eng" }], dropped: [{ index: 2, language: "und" }] } }), false, "dropped does not count");
  assert.equal(kept({ subtitles: { total: 1, kept: [{ index: 3, language: "" }], dropped: [] } }), true, "an empty tag is untagged");
  assert.equal(kept(null), false);
  assert.equal(kept({}), false);
});

test("Choose tracks is offered in the details of a finished file that kept untagged tracks, and nowhere else", () => {
  const h = harness();
  const labels = (job, detail) => {
    h.context.job = job;
    return h.run(`jobControls(job, ${detail}).children.map(b => b.textContent)`);
  };
  const job = trackReportFixture();
  assert.ok(labels(job, true).includes("Choose tracks"));
  assert.ok(!labels(job, false).includes("Choose tracks"), "queue rows carry no track lists");
  for (const change of [
    { replaced: true },
    { candidate_ready: false },
    { candidate_ready: undefined },
    { replacement_action_id: "replacement" },
    { status: "running" },
    { mode: "quality" },
    { source_path: "" },
    { action_name: "transcode_batch" },
    { tracks: { ...job.tracks, manual: true } },
    { tracks: { ...job.tracks, audio: { total: 1, kept: [{ index: 1, language: "eng" }], dropped: [] }, subtitles: { total: 0, kept: [], dropped: [] } } },
  ]) assert.ok(!labels({ ...job, ...change }, true).includes("Choose tracks"), JSON.stringify(Object.keys(change)));
  assert.ok(labels({ ...job, mode: "compact" }, true).includes("Choose tracks"));
});

test("Choose tracks opens the goal form for the file with its goal and languages, and the track list open", async () => {
  const h = harness({ radios: true });
  h.run('selectTab = (tab) => { state.tab = tab; }; setFileStep = (step) => { state.fileStep = step; }; keepLanguages.status = "ready"; keepLanguages.saved = ["eng", "jpn", "spa"]; controls = () => { renderKeepTracks(); };');
  h.run('$("technical").checked = false; $("job-detail").open = true; $("keep-tracks").open = false;');
  h.context.job = trackReportFixture({ mode: "compact" });
  const choose = h.run('jobControls(job, true)').children.find((b) => b.textContent === "Choose tracks");
  await choose.listeners.get("click")();
  assert.equal(h.elements.get("job-detail").open, false, "the dialog gives way to the form");
  assert.equal(h.elements.get("path").value, "/media/Show/Episode 1.mkv");
  assert.equal(h.elements.get("scope").value, "file");
  assert.equal(h.run("processingMode()"), "compact");
  assert.equal(h.run("state.goalExplicit"), true);
  assert.equal(h.run("state.tab"), "library");
  assert.equal(h.run("state.fileStep"), "configure");
  assert.equal(h.run("keepTracks.scope"), "job");
  assert.deepEqual(plain(h.run("keepTracks.languages")), ["eng", "jpn", "spa"]);
  assert.equal(h.elements.get("keep-tracks").open, true);
  // A job made with no language filter starts the form on every track.
  h.context.job = trackReportFixture({ tracks: { ...trackReportFixture().tracks, languages: [] } });
  await h.run('jobControls(job, true)').children.find((b) => b.textContent === "Choose tracks").listeners.get("click")();
  assert.equal(h.run("keepTracks.scope"), "all");
  // Opening a file the ordinary way takes no goal or languages with it.
  h.run('openGoalForm("/media/plain.mkv")');
  assert.equal(h.elements.get("path").value, "/media/plain.mkv");
});

test("a batch's file rows carry the sentence about the tracks they kept", async () => {
  const h = harness();
  h.run(`state.detail = "batch"; state.detailRevision = 1; state.batchItemsOffset = 0; $("job-detail").open = true;
    api = async () => ({ items: [
      { item_key: "1", display_label: "one.mkv", status: "completed", tracks_message: "Kept 2 of 3 audio tracks (eng, und)" },
      { item_key: "2", display_label: "two.mkv", status: "completed" },
    ] });`);
  await h.run('loadBatchItems("batch", 1)');
  const rows = h.elements.get("batch-items-list").children;
  const tracks = (row) => row.children[1].children.filter((cell) => String(cell.className).includes("batch-file-tracks")).map((cell) => cell.textContent);
  assert.deepEqual(tracks(rows[0]), ["Kept 2 of 3 audio tracks (eng, und)"]);
  assert.deepEqual(tracks(rows[1]), []);
});

test("the markup carries the languages editor and the picker inside the forms they belong to", () => {
  const section = markup.slice(markup.indexOf('<section id="settings-languages"'), markup.indexOf("</section>", markup.indexOf('<section id="settings-languages"')));
  for (const id of ["keep-languages-state", "keep-language-input", "keep-language-add", "keep-languages-list", "keep-languages-apply", "keep-languages-reset", "keep-languages-off", "keep-languages-note", "language-options"])
    assert.match(section, new RegExp(`id="${id}"`), id);
  assert.match(section, /original language and untagged tracks are always kept/i);
  assert.match(section, /id="keep-languages-note"[^>]*role="status"/);
  const form = markup.slice(markup.indexOf('<form id="job-form"'), markup.indexOf("</form>", markup.indexOf('<form id="job-form"')));
  const picker = form.slice(form.indexOf('<details id="keep-tracks"'), form.indexOf("</details>", form.indexOf('<details id="keep-tracks"')));
  assert.ok(picker, "the picker is part of the goal step");
  for (const id of ["keep-tracks-summary", "keep-job-languages", "keep-job-list", "keep-job-input", "keep-job-add", "keep-job-note", "keep-tracks-file", "keep-tracks-status", "keep-audio", "keep-audio-list", "keep-subtitles", "keep-subtitle-list", "keep-tracks-reset"])
    assert.match(picker, new RegExp(`id="${id}"`), id);
  assert.deepEqual([...picker.matchAll(/name="keep-scope" value="(\w+)"/g)].map((m) => m[1]), ["saved", "job", "all"]);
  assert.match(picker, /keep at least one/i);
  // The job form is a form: an editor inside it must not open another one.
  assert.doesNotMatch(picker, /<form\b/);
  assert.ok(form.indexOf('<fieldset id="processing-goal"') < form.indexOf('<details id="keep-tracks"'), "the choice follows the goal");
  assert.match(css, /\.track-list\s*\{[^}]*overflow-y:\s*auto/);
});

test("Choose tracks starts the new job with the earlier job's goal, size limit, encoder and library", async () => {
  const { h } = submissionHarness({ radios: true });
  const sent = [];
  h.context.capture = (_tool, args) => { sent.push(JSON.parse(args.inputs)); return { id: "submitted" }; };
  h.run('tool = async (...args) => capture(...args); $("technical").checked = false; $("path").value = ""; state.folderSelected = new Set(); state.selected.clear(); keepLanguages.status = "ready"; keepLanguages.saved = ["eng"];');
  const choose = async (job) => {
    h.context.job = job;
    await h.run("jobControls(job, true)").children.find((b) => b.textContent === "Choose tracks").listeners.get("click")();
  };
  // Shrink to 720p for a series episode: the height has no control in the form, so it is carried.
  await choose(trackReportFixture({ mode: "compact", goal: { mode: "compact", max_height: 720, media_type: "tv", is_anime: true }, replacement_context: { service: "sonarr", series_id: 7, source: "job_inputs" } }));
  assert.equal(h.run("processingMode()"), "compact");
  assert.equal(h.run("state.fileService"), "sonarr");
  assert.equal(h.run("state.fileMedia.id"), 7);
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[0].mode, "compact");
  assert.equal(sent[0].max_height, 720);
  assert.equal(sent[0].path, "/media/Show/Episode 1.mkv");
  assert.equal(sent[0].media_type, "tv");
  assert.equal(sent[0].is_anime, true);
  assert.deepEqual(plain(sent[0].library_context), { service: "sonarr", id: 7 });
  assert.deepEqual(sent[0].keep_languages, ["eng", "jpn", "spa"], "the languages the earlier job used");
  assert.equal(h.run("state.goalCarry"), null, "what was carried is used once");
  // Save space with the media engine off and a limit like the other episodes.
  await choose(trackReportFixture({ goal: { mode: "size", allow_hardware: false, allow_slow_preset: true, size_budget_source: "siblings", size_budget_sibling_multiplier: 1.8 }, replacement_context: { service: "filesystem", source: "parent_batch" } }));
  assert.equal(h.run("state.fileMedia"), null, "a folder library has no series to attach");
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[1].mode, "size");
  assert.equal(sent[1].allow_hardware, false);
  assert.equal(sent[1].allow_slow_preset, true);
  assert.equal(sent[1].size_budget_source, "siblings");
  assert.equal(sent[1].size_budget_sibling_multiplier, 1.8);
  assert.equal("max_height" in sent[1], false);
  assert.equal("library_context" in sent[1], false);
  // A budget in bytes and the media engine on.
  await choose(trackReportFixture({ goal: { mode: "size", size_budget_bytes: 1500000000 } }));
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[2].allow_hardware, true);
  assert.equal(sent[2].size_budget_bytes, 1500000000);
  // The carried settings belong to that goal: choosing another one sends none of them.
  await choose(trackReportFixture({ mode: "compact", goal: { mode: "compact", max_height: 720 } }));
  h.run('processingMode = () => "size"');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[3].mode, "size");
  assert.equal("max_height" in sent[3], false);
  // And an ordinary open of the form carries nothing.
  await choose(trackReportFixture({ mode: "compact", goal: { mode: "compact", max_height: 720 } }));
  h.run('openGoalForm("/media/plain.mkv")');
  assert.equal(h.run("state.goalCarry"), null);
  assert.equal(h.run("state.fileMedia"), null);
});

test("cancelling Choose tracks drops the carried goal settings and the Shrink card names the carried height", async () => {
  const { h } = submissionHarness({ radios: true });
  const sent = [];
  h.context.capture = (_tool, args) => { sent.push(JSON.parse(args.inputs)); return { id: "submitted" }; };
  h.run('tool = async (...args) => capture(...args); $("technical").checked = false; $("path").value = ""; state.folderSelected = new Set(); state.selected.clear(); keepLanguages.status = "ready"; keepLanguages.saved = ["eng"];');
  const title = () => h.elements.get("goal-title-compact").textContent;
  h.context.job = trackReportFixture({ mode: "compact", goal: { mode: "compact", max_height: 720, allow_slow_preset: true } });
  await h.run("jobControls(job, true)").children.find((b) => b.textContent === "Choose tracks").listeners.get("click")();
  assert.equal(title(), "Shrink to 720p SDR");
  // Back to the files cancels it: nothing stays carried, and the card is whole again.
  h.elements.get("back-to-files").listeners.get("click")();
  assert.equal(h.run("state.goalCarry"), null);
  assert.equal(title(), "Shrink to 1080p SDR");
  // A later ordinary pick of the same file sends none of it.
  h.run('$("path").value = "/media/Show/Episode 1.mkv"; setProcessingMode("compact");');
  await h.run('buildAndSubmitJob("encode")');
  assert.equal(sent[0].mode, "compact");
  assert.equal("max_height" in sent[0], false);
  assert.equal("allow_slow_preset" in sent[0], false);
  // A carried height is named only for its file and goal.
  await h.run("jobControls(job, true)").children.find((b) => b.textContent === "Choose tracks").listeners.get("click")();
  assert.equal(title(), "Shrink to 720p SDR");
  h.run('$("path").value = "/media/other.mkv"; renderShrinkTitle()');
  assert.equal(title(), "Shrink to 1080p SDR");
  h.context.job = trackReportFixture({ goal: { mode: "size" } });
  await h.run("jobControls(job, true)").children.find((b) => b.textContent === "Choose tracks").listeners.get("click")();
  assert.equal(title(), "Shrink to 1080p SDR");
});

test("Shrink offers 1080p or Keep 4K, and only Keep 4K names a height", async () => {
  const { h } = submissionHarness({ radios: true, segments: true });
  const sent = [];
  h.context.capture = (_tool, args) => { sent.push(JSON.parse(args.inputs)); return { id: "submitted" }; };
  h.run('tool = async (...args) => capture(...args); $("technical").checked = false; keepLanguages.status = "ready"; keepLanguages.saved = ["eng"]; setProcessingMode("compact");');
  const pick = (name, value) => h.run(`{const r=shrinkRadios("${name}").find(r=>r.value==="${value}");shrinkRadios("${name}").forEach(x=>x.checked=x===r);r.listeners.get("change")();renderShrinkTitle();}`);
  const title = () => h.elements.get("goal-title-compact").textContent;
  // A submit clears the form, so each one names the file again.
  const submit = () => h.run('$("path").value = "/media/one.mp4"; buildAndSubmitJob("encode")');
  await submit();
  assert.equal("max_height" in sent[0], false);
  pick("compact-height", "2160");
  assert.equal(title(), "Shrink to 4K SDR");
  await submit();
  assert.equal(sent[1].max_height, 2160);
  // An earlier job's 720 is carried until a resolution is chosen.
  h.run('$("path").value = "/media/one.mp4"; applyGoalSettings("/media/one.mp4", {mode:"compact", max_height:720})');
  assert.equal(title(), "Shrink to 720p SDR");
  pick("compact-height", "1080");
  assert.equal(title(), "Shrink to 1080p SDR");
  await submit();
  assert.equal("max_height" in sent[2], false);
  // An earlier job at 4K shows its height in the choice instead.
  h.run('applyGoalSettings("/media/one.mp4", {mode:"compact", max_height:2160})');
  assert.equal(h.run('shrinkHeight("compact-height")'), 2160);
  assert.equal(h.run('"max_height" in state.goalCarry.inputs'), false);
  // Encode again keeps a batch file's frozen height; the batch row names it.
  assert.equal(h.run('sameSettingsRequest({id:"b",item_key:"f",mode:"compact",settings:{max_height:2160}}).max_height'), 2160);
  assert.equal(h.run("shrinkLabel(2160)"), "Shrink to 4K SDR");
  assert.equal(h.run("shrinkLabel(0)"), "Shrink to 1080p SDR");
});

test("a file without audio takes subtitle picks alone", async () => {
  const { h, el, open, boxes, inputs } = keepHarness();
  h.context.tracks = { ...keepFixture(), audio: [] };
  await open();
  assert.equal(el("keep-audio").hidden, true);
  assert.deepEqual(boxes("audio"), []);
  assert.equal(el("keep-tracks-status").textContent, "Keeping 2 of 3 subtitles.");
  h.run('toggleTrack("subtitles", 6, true)');
  assert.deepEqual(inputs(), { keep_subtitle_streams: [5, 6, 7] }, "an empty audio list would be refused");
  h.run('toggleTrack("subtitles", 6, false)');
  assert.deepEqual(inputs(), {});
});

test("saving the list in Settings resets hand picks only where the form follows the saved list", async () => {
  const { h, open } = keepHarness();
  await open();
  const changed = { languages: ["eng", "fra"], default: ["eng", "jpn", "spa"] };
  // This job's own languages do not depend on the saved list.
  h.run('setKeepScope("job")');
  await h.run("loadKeepTracks(keepTracks.key, keepTracks.seq)");
  h.run('toggleTrack("audio", 4, false)');
  const key = h.run("keepTracks.key");
  h.context.changed = changed;
  h.run("applyKeepLanguages(changed, true)");
  assert.deepEqual(plain(h.run("keepLanguages.saved")), ["eng", "fra"]);
  assert.equal(h.run("keepTracks.key"), key);
  assert.deepEqual(plain(h.run("[...keepTracks.picks.audio]")), [1, 3]);
  // Every track neither.
  h.run('setKeepScope("all")');
  await h.run("loadKeepTracks(keepTracks.key, keepTracks.seq)");
  h.run('toggleTrack("audio", 4, false)');
  h.context.changed = { languages: ["eng"], default: [] };
  h.run("applyKeepLanguages(changed, true)");
  assert.notEqual(h.run("keepTracks.picks"), null);
  // The saved list does: the boxes follow it again.
  h.run('setKeepScope("saved")');
  await h.run("loadKeepTracks(keepTracks.key, keepTracks.seq)");
  h.run('toggleTrack("audio", 4, false)');
  assert.notEqual(h.run("keepTracks.picks"), null);
  h.context.changed = { languages: ["eng", "spa"], default: [] };
  h.run("applyKeepLanguages(changed, true)");
  assert.equal(h.run("keepTracks.picks"), null);
  assert.equal(h.run("keepTracks.status"), "loading");
  assert.equal(h.run("keepTracks.notice"), "The boxes follow the languages again.");
});

test("one file takes one source slot at a time: its tracks are read after its profile", async () => {
  const { h, open } = keepHarness();
  h.context.timers = [];
  h.context.setTimeout = (fn) => { h.context.timers.push(fn); return h.context.timers.length; };
  h.context.profile = goalProfileFixture();
  h.run('var reads = []; api = async (path) => { reads.push(path.split("?")[0]); return path.startsWith("source-tracks") ? tracks : profile; };');
  // Opening the file starts the profile read; the tracks wait for it.
  h.run("refreshGoalProfile(); refreshKeepTracks();");
  assert.equal(h.run("state.goalProfile.status"), "loading");
  assert.equal(h.run("keepTracks.status"), "loading");
  const profileTimer = h.context.timers.length;
  assert.equal(profileTimer, 1, "only the profile read is scheduled");
  // Changing the languages meanwhile does not start a second read.
  h.run('setKeepScope("job")');
  assert.equal(h.context.timers.length, profileTimer);
  await h.run('loadGoalProfile("/media/movie.mkv")');
  assert.deepEqual(plain(h.run("reads")), ["source-profile"]);
  assert.equal(h.context.timers.length, profileTimer + 1, "the profile is in; now the tracks are scheduled");
  await h.context.timers.at(-1)();
  assert.deepEqual(plain(h.run("reads")), ["source-profile", "source-tracks"]);
  assert.equal(h.run("keepTracks.status"), "ready");
  // With the profile already read, a language change reads the tracks at once.
  h.run('keepTracks.languages = ["eng"]; refreshKeepTracks();');
  assert.equal(h.context.timers.length, profileTimer + 2);
  // A failed profile read does not hold the tracks back either.
  h.run('$("path").value = "/media/next.mkv"; api = async (path) => { if (!path.startsWith("source-tracks")) throw new Error("no"); return tracks; }; refreshGoalProfile(); refreshKeepTracks();');
  const before = h.context.timers.length;
  await h.run('loadGoalProfile("/media/next.mkv")');
  assert.equal(h.run("state.goalProfile.status"), "error");
  assert.equal(h.context.timers.length, before + 1);
});

// An encode that finished but could not be saved is tried again as it is.
test("a batch row whose encode finished but was not saved offers Try again, not a goal change", async () => {
  const item = { item_key: "k-s", display_label: "Sample Show S04E13", status: "waiting_decision", child_action_id: "child-s", waiting_decisions: ["recheck", "cancel"], can_reconfigure: false, mode: "size", reason_code: "eligible" };
  const h = await batchItemsHarness([item], { detailJob: { status: "waiting_decision", batch: {} } });
  const row = itemRows(h)[0];
  assert.deepEqual(buttonLabels(row).filter((l) => /Try again|Change goal|Change settings/.test(l)), ["Try again"]);
  const control = flatten(row).find((e) => e.textContent === "Try again");
  // It acts on the file's own job, needs its worker, and is the row's main action.
  assert.equal(control.dataset.jobControl, "child-s");
  assert.equal(control.dataset.requiresWorker, "true");
  assert.match(control.className, /primary/);
  h.context.calls = [];
  h.run("jobControl = async (...args) => calls.push(JSON.parse(JSON.stringify(args)));");
  await control.listeners.get("click")();
  const [id, name, args, options] = plain(h.context.calls[0]);
  assert.deepEqual([id, name, args], ["child-s", "action_resume", { decision: "recheck" }]);
  assert.equal(options.decision.label, "Try again");
  // A row that is only waiting for another reason has no such button.
  const other = await batchItemsHarness([{ ...item, waiting_decisions: ["cancel"] }], { detailJob: { status: "waiting_decision", batch: {} } });
  assert.equal(buttonLabels(itemRows(other)[0]).includes("Try again"), false);
});

test("Try again on a job card resumes with recheck, with no confirmation and no goal change beside it", async () => {
  const h = harness();
  h.run("state.info={allow_destructive:true};");
  h.context.asked = 0;
  h.run("confirmDecision = async () => { asked++; return true; };");
  h.context.calls = [];
  h.run("jobControl = async (...args) => calls.push(JSON.parse(JSON.stringify(args)));");
  const job = { id: "child-s", action_name: "transcode_media", status: "waiting_decision", mode: "size", parent_action_id: "batch", parent_item_key: "k-s", waiting_options: [{ decision: "recheck" }, { decision: "cancel" }] };
  const labels = h.run(`jobControls(${JSON.stringify(job)}).children.map(b => b.textContent)`);
  assert.ok(labels.includes("Try again"));
  assert.ok(!labels.includes("Change goal") && !labels.includes("Change settings"));
  const rail = h.run(`jobControls(${JSON.stringify(job)})`);
  const control = rail.children.find((b) => b.textContent === "Try again");
  assert.equal(control.dataset.requiresWorker, "true");
  await control.listeners.get("click")();
  assert.equal(h.context.asked, 0);
  assert.deepEqual(plain(h.context.calls[0]), ["child-s", "action_resume", { decision: "recheck" }]);
  // The batch row's main queue actions keep it as one of the visible buttons.
  assert.ok(h.run("mainQueueActions").includes("Try again"));
});

test("a failed save says one thing, whatever storage error came with it", () => {
  const h = harness();
  const say = (text) => h.run(`shortJobReason(${JSON.stringify(text)})`);
  const plainFailure = say("Encoding finished for job abc but publication requires intervention: publish failed");
  assert.match(plainFailure, /Encoding finished, but the file could not be saved\. The encoded copy is kept\. Use Try again/);
  // The storage wording never replaces it, and an old worker's note is passed on.
  const storage = say("Encoding finished for job abc but publication requires intervention: smb_transport_error connecting to SMB server");
  assert.equal(storage, plainFailure);
  assert.match(say("Encoding finished for job abc but publication requires intervention: x The worker has not been updated to retry a publication; update it first."), /The worker has not been updated to retry a publication; update it first\.$/);
  // A storage failure with no finished encode still points at the connection.
  assert.match(say("smb_transport_error: connecting to SMB server"), /Fix the connection/);
});

test("Show in Files opens the Files tab on the folder and file, inside the root that holds it", () => {
  const h = harness();
  h.run('state.info={roots:["/media","/media/tv"]};');
  const query = (path) => h.run(`filesRouteFor(${JSON.stringify(path)})`);
  const params = new URLSearchParams(query("/media/tv/Show/Season 4/Show S04E13.mkv"));
  // The longest root wins, so the file is shown under the folder it was added from.
  assert.equal(params.get("view"), "files");
  assert.equal(params.get("source"), "folder:/media/tv");
  assert.equal(params.get("folder"), "/media/tv/Show/Season 4");
  assert.equal(params.get("file"), "/media/tv/Show/Season 4/Show S04E13.mkv");
  assert.equal(params.get("locate"), "1", "showing a file does not open its encoding form");
  // A sibling folder that merely shares a prefix is not inside the root.
  assert.equal(query("/media2/clip.mkv"), null);
  assert.equal(query("relative/clip.mkv"), null);
  assert.equal(query(""), null);
  assert.equal(new URLSearchParams(query("/media/clip.mkv")).get("folder"), "/media");
  // The route round-trips through the same parser the address bar uses.
  const route = h.run(`navigationRoute("?${params.toString().replace(/"/g, "")}")`);
  assert.equal(route.tab, "library");
  assert.equal(route.file, "/media/tv/Show/Season 4/Show S04E13.mkv");
  assert.equal(route.locate, true);
  assert.equal(h.run('navigationRoute("?view=files&file=%2Fa.mkv")').locate, false);
});

test("Show in Files goes to the file, and says so when the file is outside the media folders", async () => {
  const h = harness();
  h.run('state.info={roots:["/media"]};');
  h.context.routes = [];
  h.context.notes = [];
  h.run("applyNavigation = async (route) => routes.push(route); notify = (text) => notes.push(text);");
  await h.run('showInFiles("/media/tv/Show S04E13.mkv")');
  assert.equal(h.context.routes.length, 1);
  assert.equal(h.context.routes[0].file, "/media/tv/Show S04E13.mkv");
  assert.equal(h.context.routes[0].source, "folder:/media");
  await h.run('showInFiles("/elsewhere/clip.mkv")');
  assert.equal(h.context.routes.length, 1);
  assert.match(h.context.notes[0], /outside the media folders/);
});

test("every job card and open job offers its file or folder in Files", () => {
  const h = harness();
  h.run('state.info={allow_destructive:true,roots:["/media"]};');
  const labels = (job, detail = false) => h.run(`jobControls(${JSON.stringify(job)}, ${detail}).children.map(b => b.textContent)`);
  const base = { id: "t-a", action_name: "transcode_media", source_path: "/media/tv/Show S04E13.mkv", mode: "size" };
  assert.ok(labels({ ...base, status: "cancelled" }).includes("Show in Files"));
  assert.ok(labels({ ...base, status: "failed" }).includes("Show in Files"));
  // Every job offers it, finished or running, in the queue and when open.
  for (const status of ["completed", "running", "waiting_decision", "pending"]) {
    assert.ok(labels({ ...base, status }).includes("Show in Files"), status);
    assert.ok(labels({ ...base, status }, true).includes("Show in Files"), status);
  }
  assert.ok(labels({ ...base, status: "completed", batch: { dry_run: true } }).includes("Show in Files"));
  // A batch of several files shows their folder.
  const folderJob = { id: "b-1", action_name: "transcode_batch", status: "running", batch_files: { file_count: 3, folder: "/media/tv/Show/Season 4", files: [] } };
  assert.ok(labels(folderJob).includes("Show in Files"));
  h.context.routes = [];
  h.run("applyNavigation = async (route) => routes.push(route);");
  h.run(`jobControls(${JSON.stringify(folderJob)}).children.find(b => b.textContent === "Show in Files")`).listeners.get("click")();
  assert.equal(h.context.routes[0].folder, "/media/tv/Show/Season 4");
  assert.equal(h.context.routes[0].file, "");
  // No file, a file outside the folders: nothing to show.
  assert.ok(!labels({ id: "t-b", action_name: "transcode_media", status: "cancelled" }).includes("Show in Files"));
  assert.ok(!labels({ ...base, status: "cancelled", source_path: "/elsewhere/clip.mkv" }).includes("Show in Files"));
  // A replacement job knows its file from the promotion.
  assert.ok(labels({ id: "p-a", action_name: "file_promotion", status: "cancelled", promotion: { original_path: "/media/tv/Show S04E13.mkv" } }).includes("Show in Files"));
  // It only opens something, so a busy job does not disable it.
  const control = h.run(`jobControls(${JSON.stringify({ ...base, status: "failed" })}).children.find(b => b.textContent === "Show in Files")`);
  assert.equal(control.dataset.requiresWorker, "false");
  assert.ok(h.run('navigationLabels').includes("Show in Files"));
});

test("a batch file is one collapsed line: its name, its state and saving; the actions are inside", async () => {
  const items = [
    { item_key: "k-c", display_label: "Ready.mkv", status: "completed", candidate_ready: true, saved_bytes: 1.9e9, child_action_id: "t-c", file_path: "/media/tv/Ready.mkv", can_reconfigure: true, mode: "size" },
    { item_key: "k-x", display_label: "Cancelled.mkv", status: "cancelled", child_action_id: "t-x", file_path: "/media/tv/X.mkv", can_reconfigure: true, mode: "size" },
    { item_key: "k-w", display_label: "Waiting.mkv", status: "queued" },
  ];
  const h = harness();
  h.run('state.info={allow_destructive:true,roots:["/media"]};state.detail="batch-a";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};');
  h.context.items = items;
  h.run('api=async()=>({items,total:3});');
  await h.run('loadBatchItems("batch-a",2)');
  const rows = itemRows(h);
  const flat = (row) => flatten(row);
  const classed = (row, name) => flat(row).find((e) => String(e.className || "").split(" ").includes(name));
  // A row is a head and a body; the head is one button that opens the body.
  for (const row of rows) {
    assert.equal(row.children.length, 2);
    assert.equal(row.children[0].type, "button");
    assert.equal(row.children[0].dataset.fileToggle, row.dataset.itemKey);
    assert.equal(row.dataset.open, "false");
    assert.equal(row.children[1].hidden, true);
  }
  assert.equal(classed(rows[0], "jd-file-name").textContent, "Ready.mkv");
  // The name is no longer a second link or button: the head is the only control until it opens.
  assert.ok(!rows.some((row) => flat(row.children[0]).some((e) => e !== row.children[0] && e.type === "button")));
  // The saving is shown only where the server gave one.
  assert.match(classed(rows[0], "jd-file-saved").textContent, /1\.90 GB/);
  assert.equal(classed(rows[1], "jd-file-saved"), undefined);
  // Inside: Replace file is the one primary action; the rest are plain secondary buttons.
  const labels = (row) => flat(row.children[1]).filter((e) => e.dataset?.jobControl).map((e) => e.textContent);
  assert.deepEqual(labels(rows[0]), ["Replace file", "Change goal", "Show in Files", "Open job"]);
  assert.deepEqual(flat(rows[0].children[1]).filter((e) => e.dataset?.jobControl && String(e.className).includes("jd-primary")).map((e) => e.textContent), ["Replace file"]);
  assert.ok(flat(rows[0].children[1]).filter((e) => e.dataset?.jobControl).every((e) => e.dataset.batchItemKey === "k-c"));
  // The primary action of a cancelled file is Encode again.
  assert.deepEqual(flat(rows[1].children[1]).filter((e) => e.dataset?.jobControl && String(e.className).includes("jd-primary")).map((e) => e.textContent), ["Encode again"]);
  // A file that has not started has nothing to do.
  assert.deepEqual(labels(rows[2]), []);
  // Opening a row is remembered across the list being rebuilt.
  rows[0].children[0].listeners.get("click")();
  assert.equal(rows[0].dataset.open, "true");
  assert.equal(rows[0].children[1].hidden, false);
});

test("a file that failed opens itself once, and closing it sticks across refreshes", async () => {
  const items = [
    { item_key: "k-f", display_label: "Failed.mkv", status: "failed", child_action_id: "t-f", file_path: "/media/tv/F.mkv", can_reconfigure: true, mode: "size" },
    { item_key: "k-ok", display_label: "Fine.mkv", status: "completed", child_action_id: "t-ok" },
  ];
  const h = harness();
  h.context.items = items;
  h.run('state.info={allow_destructive:true,roots:["/media"]};state.detail="batch-a";state.detailRevision=2;state.detailAutoOpened=new Set();state.openFiles=new Set();$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:2});');
  await h.run('loadBatchItems("batch-a",2)');
  let rows = itemRows(h);
  assert.deepEqual(rows.map((row) => row.dataset.open), ["true", "false"]);
  assert.equal(rows[0].children[1].hidden, false);
  // The person closes it; the next refresh does not open it again.
  rows[0].children[0].listeners.get("click")();
  assert.equal(rows[0].dataset.open, "false");
  await h.run('loadBatchItems("batch-a",2)');
  rows = itemRows(h);
  assert.deepEqual(rows.map((row) => row.dataset.open), ["false", "false"]);
});

test("every batch row offers Show in Files when its file is inside the media folders", async () => {
  const items = [
    { item_key: "k-f", display_label: "Failed", status: "failed", child_action_id: "t-f", file_path: "/media/tv/F.mkv" },
    { item_key: "k-c", display_label: "Cancelled", status: "cancelled", child_action_id: "t-c", file_path: "/media/tv/C.mkv" },
    { item_key: "k-s", display_label: "Skipped", status: "skip", file_path: "/media/tv/S.mkv", reasons: ["quality_not_met"] },
    { item_key: "k-r", display_label: "Running", status: "running", child_action_id: "t-r", file_path: "/media/tv/R.mkv" },
    { item_key: "k-o", display_label: "Outside", status: "failed", child_action_id: "t-o", file_path: "/elsewhere/O.mkv" },
  ];
  const h = harness();
  h.context.items = items;
  h.run('state.info={allow_destructive:true,roots:["/media"]};state.detail="batch-a";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:5});');
  await h.run('loadBatchItems("batch-a",2)');
  const rows = itemRows(h);
  const shows = rows.map((row) => buttonLabels(row).includes("Show in Files"));
  assert.deepEqual(shows, [true, true, true, true, false]);
  const control = flatten(rows[0]).find((e) => e.textContent === "Show in Files");
  // It names the file's own job and keeps working while the batch is busy.
  assert.equal(control.dataset.jobControl, "t-f");
  assert.equal(control.dataset.navigation, "true");
  h.context.routes = [];
  h.run("applyNavigation = async (route) => routes.push(route);");
  await control.listeners.get("click")();
  assert.equal(h.context.routes[0].file, "/media/tv/F.mkv");
});

function drainInfo(drain) {
  return { ready: true, nodes: [], drain: { enabled: true, revision: 1, running: 0, publication_pending: 0, queued: 0, ready_for_deploy: false, message: "", workers: [], jobs: [], ...drain } };
}

test("worker drain is hidden until the server reports it, and starts off with a single Drain action", () => {
  const h = harness();
  h.run("state.workerInfo = {ready:true,nodes:[]}; renderWorkers();");
  assert.equal(h.elements.get("worker-drain").hidden, true);
  h.context.info = drainInfo({ enabled: false });
  h.run("state.workerInfo = info; renderWorkers();");
  assert.equal(h.elements.get("worker-drain").hidden, false);
  assert.equal(h.elements.get("worker-drain-toggle").textContent, "Pause all");
  assert.doesNotMatch(h.elements.get("worker-status").textContent, /Pausing|Paused/);
});

test("a draining worker shows its progress, lists open jobs and offers Resume", () => {
  const h = harness();
  h.context.info = drainInfo({
    running: 1, publication_pending: 1, queued: 3, message: "x",
    workers: [{ name: "alpha", reachable: true, running: 1, publication_pending: 1, ready_for_deploy: false, reason: "1 running" }],
    jobs: [
      { action_id: "t-1", worker: "alpha", file: "Running.mkv", state: "running" },
      { action_id: "t-2", worker: "alpha", file: "Unsaved.mkv", state: "publication_pending", path: "/media/tv/Unsaved.mkv" },
      { action_id: "t-3", file: "Waiting.mkv", state: "queued" },
    ],
  });
  h.run("state.workerInfo = info; renderWorkers();");
  assert.equal(h.elements.get("worker-drain-toggle").textContent, "Resume");
  assert.equal(h.elements.get("worker-drain-status").textContent, "Pausing — 1 file still encoding, 1 file encoded but not saved yet");
  assert.match(h.elements.get("worker-drain-note").textContent, /alpha: 1 running/);
  assert.match(h.elements.get("worker-drain-note").textContent, /Paused batches stay paused/);
  assert.match(h.elements.get("worker-status").textContent, /Pausing$/);
  // Queued jobs are counted but never listed as open work.
  const rows = h.elements.get("worker-drain-jobs").children;
  assert.equal(rows.length, 2);
  assert.deepEqual(buttonLabels(rows[0]), ["View file"]);
  assert.deepEqual(buttonLabels(rows[1]), ["Try again", "View file", "Show in Files"]);
});

test("the pause banner tells files encoding from files waiting for a turn on a worker", () => {
  const h = harness();
  h.context.info = drainInfo({
    running: 4, waiting: 2, queued: 5,
    workers: [{ name: "alpha", reachable: true, running: 4, waiting: 2, ready_for_deploy: false, reason: "4 running" }],
    jobs: [
      { action_id: "t-1", worker: "alpha", file: "Encoding.mkv", state: "running" },
      { action_id: "t-2", worker: "alpha", file: "Lined.mkv", state: "running", waiting: true },
    ],
  });
  h.run("state.workerInfo = info; renderWorkers();");
  assert.equal(h.elements.get("worker-drain-status").textContent, "Pausing — 2 files still encoding, 2 waiting for a turn on a worker");
  const labels = h.elements.get("worker-drain-jobs").children.map((row) => flatten(row).filter((e) => e.className === "badge").map((e) => e.textContent).join());
  assert.deepEqual(labels, ["Encoding · alpha", "Waiting for a turn · alpha"]);
  h.context.info = drainInfo({ running: 3, waiting: 3 });
  h.run("state.workerInfo = info; renderWorkers();");
  assert.equal(h.elements.get("worker-drain-status").textContent, "Pausing — 3 waiting for a turn on a worker");
});

test("worker drain reports ready for deploy only when the server says so", () => {
  const h = harness();
  h.context.info = drainInfo({ ready_for_deploy: true });
  h.run("state.workerInfo = info; renderWorkers();");
  assert.equal(h.elements.get("worker-drain-status").textContent, "Paused — ready to deploy");
  assert.match(h.elements.get("worker-status").textContent, /Paused$/);
});

test("an encode that could not be saved offers Try again through the same recheck as the queue", async () => {
  const h = harness();
  h.context.info = drainInfo({ publication_pending: 1, jobs: [{ action_id: "t-2", file: "Unsaved.mkv", state: "publication_pending", path: "/media/tv/Unsaved.mkv" }] });
  h.run("state.workerInfo = info; renderWorkers();");
  const retry = flatten(h.elements.get("worker-drain-jobs").children[0]).find((e) => e.textContent === "Try again");
  assert.equal(retry.dataset.jobControl, "t-2");
  assert.equal(retry.dataset.requiresWorker, "true");
  h.context.calls = [];
  h.run('jobControl = async (...args) => calls.push(args); refreshWorkers = async () => {};');
  await retry.listeners.get("click")();
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.calls[0].slice(0, 3))), ["t-2", "action_resume", { decision: "recheck" }]);
  assert.equal(h.context.calls[0][3].decision.label, "Try again");
});

test("Show in Files from the drain list closes the dialog and opens the file's folder", async () => {
  const h = harness();
  h.context.info = drainInfo({ publication_pending: 1, jobs: [{ action_id: "t-2", file: "Unsaved.mkv", state: "publication_pending", path: "/media/tv/Unsaved.mkv" }] });
  h.run('state.info={allow_destructive:true,roots:["/media"]}; state.workerInfo = info; renderWorkers();');
  h.context.routes = [];
  h.run("applyNavigation = async (route) => routes.push(route);");
  h.document.getElementById("workers-dialog").open = true;
  const files = flatten(h.elements.get("worker-drain-jobs").children[0]).find((e) => e.textContent === "Show in Files");
  assert.equal(files.dataset.navigation, "true");
  await files.listeners.get("click")();
  assert.equal(h.document.getElementById("workers-dialog").open, false);
  assert.equal(h.context.routes[0].file, "/media/tv/Unsaved.mkv");
});

test("the drain button turns the drain on, then off, with one request each", async () => {
  const h = harness();
  h.context.requests = [];
  h.run(`state.workerInfo = {ready:true,nodes:[]}; renderWorkers(); notify = () => {}; refreshWorkers = async () => {};
    api = async (path, body) => { requests.push([path, body]); return { enabled: body.enabled, running: 0, publication_pending: 0, queued: 0, ready_for_deploy: false, message: "", jobs: [], workers: [] }; };`);
  h.context.info = drainInfo({ enabled: false });
  h.run("state.workerInfo = info; renderWorkers();");
  await h.elements.get("worker-drain-toggle").listeners.get("click")();
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.requests[0])), ["worker-drain", { enabled: true }]);
  assert.equal(h.elements.get("worker-drain-toggle").textContent, "Resume");
  await h.elements.get("worker-drain-toggle").listeners.get("click")();
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.requests[1])), ["worker-drain", { enabled: false }]);
  assert.equal(h.elements.get("worker-drain-toggle").textContent, "Pause all");
});

test("a job held by the drain says it is waiting for a worker deploy", () => {
  const h = harness();
  const observed = h.run('workerProgressObservation({status:"waiting_external",waiting_condition:"worker_drain"})');
  assert.equal(observed.status, "Held for worker deploy");
  assert.equal(observed.active, false);
  assert.equal(observed.warning, false);
});


test("Pause all in the Queue, the header and the workers dialog show one state", () => {
  const h = harness();
  h.run("state.workerInfo = {ready:true,nodes:[]}; renderWorkers();");
  assert.equal(h.elements.get("queue-drain-toggle").hidden, true);
  h.context.info = drainInfo({ enabled: false });
  h.run("state.workerInfo = info; renderWorkers();");
  assert.equal(h.elements.get("queue-drain-toggle").hidden, false);
  assert.equal(h.elements.get("queue-drain-label").textContent, "Pause all");
  assert.equal(h.elements.get("queue-drain-status").textContent, "Running");
  const states = [
    [{ running: 2, queued: 8 }, "Pausing — 2 files still encoding · 8 files held back", /Pausing$/],
    [{ running: 1, publication_pending: 1 }, "Pausing — 1 file still encoding, 1 file encoded but not saved yet", /Pausing$/],
    [{}, "Pausing — checking the workers", /Pausing$/],
    [{ ready_for_deploy: true }, "Paused — ready to deploy", /Paused$/],
  ];
  for (const [drain, text, header] of states) {
    h.context.info = drainInfo(drain);
    h.run("state.workerInfo = info; renderWorkers();");
    assert.equal(h.elements.get("queue-drain-label").textContent, "Resume");
    assert.equal(h.elements.get("queue-drain-status").textContent, text);
    assert.equal(h.elements.get("worker-drain-status").textContent, text.split(" · ")[0]);
    assert.match(h.elements.get("worker-status").textContent, header);
  }
  assert.equal(h.elements.get("queue-meta").dataset.state, "ready");
});

test("the Queue button pauses and resumes with the same single request as the dialog", async () => {
  const h = harness();
  h.context.requests = [];
  h.run(`notify = () => {}; refreshWorkers = async () => {};
    api = async (path, body) => { requests.push([path, body.enabled]); return { enabled: body.enabled, running: 0, publication_pending: 0, queued: 0, ready_for_deploy: false, message: "", jobs: [], workers: [] }; };`);
  h.context.info = drainInfo({ enabled: false });
  h.run("state.workerInfo = info; renderWorkers();");
  await h.elements.get("queue-drain-toggle").listeners.get("click")();
  assert.equal(h.elements.get("queue-drain-label").textContent, "Resume");
  assert.equal(h.elements.get("worker-drain-toggle").textContent, "Resume");
  await h.elements.get("worker-drain-toggle").listeners.get("click")();
  assert.equal(h.elements.get("queue-drain-label").textContent, "Pause all");
  assert.equal(JSON.stringify(h.context.requests), '[["worker-drain",true],["worker-drain",false]]');
});

test("a normal scheduler stays out of the header, a doubtful one is shown", () => {
  const h = harness();
  h.run('state.workerInfo={ready:true,nodes:[{name:"A",connected:true,scheduler_health:"ok",observed_at:new Date().toISOString(),last_success_at:new Date().toISOString(),sweeps:{queue:{observed_at:new Date().toISOString()}}}]};renderWorkers();');
  assert.equal(h.elements.get("worker-status").textContent, "Worker · Connected");
  h.run('state.workerInfo={ready:true,nodes:[{name:"A",connected:true,scheduler_health:"ok",observed_at:new Date(Date.now()-600000).toISOString()}]};renderWorkers();');
  assert.match(h.elements.get("worker-status").textContent, /Scheduler Unknown/);
});

test("the header keeps only brand and worker status; install, open path and reload live in quieter places", () => {
  const header = markup.slice(markup.indexOf('<header class="masthead">'), markup.indexOf("</header>"));
  assert.match(header, /id="worker-status"/);
  assert.doesNotMatch(header, /id="install-app"/);
  const settings = markup.slice(markup.indexOf('<section id="advanced"'), markup.indexOf("</main>"));
  assert.match(settings, /id="install-app"/);
  const tools = markup.slice(markup.indexOf('id="library-tools"'), markup.indexOf('id="library-search-label"'));
  const menu = tools.slice(tools.indexOf('id="library-actions"'));
  assert.match(menu, /id="open-path"/);
  assert.match(menu, /id="reload-library"/);
  assert.equal((tools.match(/<button/g) || []).length, 3, "search plus the two menu items");
  const queue = markup.slice(markup.indexOf('<section id="jobs"'), markup.indexOf('<section id="stats"'));
  assert.ok(queue.indexOf('id="queue-drain-toggle"') < queue.indexOf('id="jobs-list"'));
});

// The palette and dark-mode contrast checks run with the rest of the UI tests.
// Imported last, so the harness above is defined before any test starts.
await import("./theme_test.mjs");

test("a batch shows one bar for how far along it is and a legend of what each file is doing", () => {
  const h = harness();
  h.context.batch = { total: 12, completed: 3, running: 2, failed: 1 };
  const parts = h.run("batchSegments(batch)");
  assert.equal(parts.legend, "3 done · 1 failed · 2 running · 6 waiting");
  // One fill: the finished and failed files, a third of the batch.
  assert.deepEqual(parts.bar.children.map(child => [child.className, Math.round(parseFloat(child.style.width))]), [["segment-ok", 33]]);
  h.context.batch = { total: 8, completed: 6, review: 2 };
  assert.equal(h.run("batchSegments(batch).legend"), "6 done · 2 need you");
  // A single state is reported so the queue row can leave the bar out.
  h.context.batch = { total: 13, completed: 13 };
  assert.equal(h.run("batchSegments(batch).kinds"), 1);
  h.context.batch = { total: 8, completed: 6, review: 2 };
  assert.equal(h.run("batchSegments(batch).kinds"), 2);
  h.context.batch = { total: 1, completed: 1 };
  assert.equal(h.run("batchSegments(batch)"), null);
  h.context.batch = { total: 4, completed: 4, dry_run: true };
  assert.equal(h.run("batchSegments(batch)"), null);
});

test("running files add to the bar only as far as their measured steps", () => {
  const h = harness();
  h.context.batch = { total: 2, running: 2 };
  assert.equal(Math.round(parseFloat(h.run("batchSegments(batch, 0.5)").bar.children[0].style.width)), 50);
  // Without a measurement they add nothing.
  assert.equal(parseFloat(h.run("batchSegments(batch)").bar.children[0].style.width), 0);
  // A cancelled file is not work left to do.
  h.context.batch = { total: 4, completed: 1, cancelled: 2 };
  assert.equal(Math.round(parseFloat(h.run("batchSegments(batch)").bar.children[0].style.width)), 50);
});

test("a job title is a real link to the job that still opens in place on a plain click", () => {
  const h = harness();
  h.run('globalThis.opened = []; openJob = id => opened.push(id);');
  const link = h.run('jobLink("Demo Show", "a b")');
  assert.equal(link.href, "?view=queue&job=a%20b");
  assert.match(link.className, /job-title/);
  let prevented = false;
  link.listeners.get("click")({ preventDefault: () => { prevented = true; } });
  assert.equal(prevented, true);
  assert.deepEqual(JSON.parse(JSON.stringify(h.context.opened)), ["a b"]);
  link.listeners.get("click")({ metaKey: true, preventDefault: () => assert.fail("modified clicks keep the browser default") });
  link.listeners.get("click")({ button: 1, preventDefault: () => assert.fail("middle clicks keep the browser default") });
  assert.equal(h.context.opened.length, 1);
});

test("a file the probe could not read is described plainly", () => {
  const h = harness();
  h.context.reason = "mediainspect failed: ffprobe tool_exit after 58ms: exit status 1";
  assert.equal(h.run("shortJobReason(reason)"), "Couldn't read the video file");
});

test("a failed batch with no error of its own says its first reason instead of a filler", async () => {
  const h = harness();
  h.run(`state.jobs = [];`);
  h.context.job = { id: "f", number: 1, action_name: "transcode_batch", status: "failed", batch: { total: 6, failed: 6 }, batch_files: { reasons: [{ count: 6, reason: "smb_transport_error: i/o timeout" }] } };
  assert.equal(h.run("queueOutcome(job)"), "Open job for error details");
  assert.match(h.run("shortJobReason(job.batch_files.reasons[0].reason)"), /cannot access media storage/i);
});

test("a selection is titled by what the server named it, a single file by its own name, a Sonarr batch by its series", () => {
  const h = metricProgressHarness();
  const title = (job) => h.run(`queuePresentation(${JSON.stringify(job)}).title`);
  const base = {id: "j", action_name: "transcode_batch", status: "waiting_external"};
  // Several files chosen across folders: the server's name replaces "Selected files".
  assert.equal(title({...base, batch: {title: "Selected files", total: 2}, batch_files: {file_count: 2, name: "Demo Movie (2001)", files: []}}), "Demo Movie (2001)");
  // The folder label still wins when every file is in one folder.
  assert.equal(title({...base, batch: {title: "Selected files", total: 2}, batch_files: {file_count: 2, context: "Demo Show · Season 1", name: "Demo Show", files: []}}), "Demo Show · Season 1");
  // One file is called by its file name, whatever the batch is called.
  assert.equal(title({...base, batch: {title: "Selected files", total: 1}, batch_files: {file_count: 1, name: "Demo Clip", files: [{file_path: "/lib/Demo Clip.mkv"}]}}), "Demo Clip.mkv");
  // A real series title is never replaced by a name made from file names.
  assert.equal(title({...base, batch: {title: "Demo Show", total: 12}, batch_files: {file_count: 12, name: "Demo Show - S01", files: []}}), "Demo Show");
  // Nothing better known: the old name stays.
  assert.equal(title({...base, batch: {title: "Selected files", total: 2}, batch_files: {file_count: 2, files: []}}), "Selected files");
});


function storageFixture(overrides = {}) {
  const day = (n, bytes) => ({ t: `2026-10-${String(n).padStart(2, "0")}T12:00:00Z`, bytes });
  return {
    helper: true,
    measuring: false,
    measured_at: "2026-10-25T12:00:00Z",
    volumes: [
      {
        id: "/m/Anime",
        roots: ["/m/Anime", "/m/Movies"],
        total_bytes: 20e12,
        used_bytes: 12e12,
        free_bytes: 7.9e12,
        accounted_bytes: 11e12,
        other_bytes: 1e12,
        source: "helper",
        trend: { points: [day(1, 13e12), day(10, 12.6e12), day(25, 12e12)], change_7d: -3e11, change_30d: -1e12 },
      },
    ],
    folders: [
      { path: "/m/Anime", name: "Anime", volume: "/m/Anime", bytes: 7e12, source: "helper", trend: { points: [], change_7d: -2e11 } },
      { path: "/m/Movies", name: "Movies", volume: "/m/Anime", bytes: 4e12, source: "helper", trend: { points: [] } },
      { path: "/m/Anime/Podcasts", name: "Podcasts", volume: "/m/Anime", inside: "/m/Anime", bytes: 1e11, source: "helper", trend: { points: [] } },
    ],
    ...overrides,
  };
}
function storageHarness() {
  const h = harness();
  h.document.createElementNS = () => new Element();
  return h;
}

test("sizes in the terabytes are not shown as thousands of gigabytes", () => {
  const h = harness();
  assert.equal(h.run("bytes(12.34e12)"), "12.34 TB");
  assert.equal(h.run("bytes(999e9)"), "999.00 GB");
});

test("Stats shows the NAS total against what is used, what is free and what each folder holds", () => {
  const h = storageHarness();
  h.context.data = storageFixture();
  h.run("renderStorage(data)");
  const card = h.elements.get("storage-volumes").children[0];
  const text = allText(card);
  assert.match(text, /NAS/);
  assert.match(text, /12\.00 TB used of 20\.00 TB/);
  assert.match(text, /7\.90 TB free · 60% full/);
  for (const name of ["Anime", "Movies", "Everything else", "Free"]) assert.ok(text.includes(name), name);
  assert.match(text, /7\.00 TB.*35%/s);
  // The recovery section says how much was freed, from the volume's own trend.
  assert.match(text, /300\.00 GB freed in 7 days/);
  assert.match(text, /1\.00 TB freed in 30 days/);
});

test("the bar counts each served folder once and leaves a nested folder to the one holding it", () => {
  const h = storageHarness();
  h.context.data = storageFixture();
  h.run("renderStorage(data)");
  const bar = h.elements.get("storage-volumes").children[0].children.find((c) => c.className === "storage-bar");
  // Anime, Movies and "everything else": the nested Podcasts is not a segment.
  assert.equal(bar.children.length, 3);
  const widths = bar.children.map((s) => parseFloat(s.style.width));
  assert.equal(JSON.stringify(widths), "[35,20,5]");
  const card = allText(h.elements.get("storage-volumes").children[0]);
  assert.match(card, /Podcasts/);
  assert.match(card, /inside Anime/);
});

test("a change is only reported for a window the history actually covers", () => {
  const h = storageHarness();
  h.context.young = storageFixture({
    volumes: [{ ...storageFixture().volumes[0], trend: { points: [], change_all: 5e9 } }],
    folders: storageFixture().folders.map((f) => ({ ...f, trend: { points: [] } })),
  });
  h.run("renderStorage(young)");
  const text = allText(h.elements.get("storage-volumes").children[0]);
  assert.match(text, /5\.00 GB more since the first measurement/);
  assert.doesNotMatch(text, /in 7 days|in 30 days/);
  assert.equal(h.run("storageChanges({}).length"), 0);
  assert.equal(h.run("storageChanges({change_7d:0})[0]"), "No change in 7 days");
});

test("a trend with fewer than two days of samples draws no chart", () => {
  const h = storageHarness();
  assert.equal(h.run('storageSparkline({points:[{t:"2026-10-25T00:00:00Z",bytes:1}]}, "Used")'), null);
  const chart = h.run('storageSparkline({points:[{t:"2026-10-24T00:00:00Z",bytes:3e12},{t:"2026-10-25T00:00:00Z",bytes:2e12}]}, "Used space")');
  assert.ok(chart);
});

test("Stats says so when nothing has been measured yet, and while measuring", () => {
  const h = storageHarness();
  h.run("renderStorage({volumes:[],folders:[]})");
  assert.match(h.elements.get("storage-status").textContent, /Not measured yet/);
  assert.equal(h.elements.get("storage-refresh").disabled, false);
  h.run("renderStorage({volumes:[],folders:[],measuring:true})");
  assert.match(h.elements.get("storage-status").textContent, /Measuring now/);
  assert.equal(h.elements.get("storage-refresh").disabled, true);
  assert.equal(h.elements.get("storage-refresh").textContent, "Measuring…");
});

test("the status says where the numbers came from and surfaces what could not be measured", () => {
  const h = storageHarness();
  h.context.data = storageFixture({ notes: ["1 of 3 folders could not be sized completely."], error: "disk is full" });
  h.run("renderStorage(data)");
  const status = h.elements.get("storage-status").textContent;
  assert.match(status, /inside the NAS/);
  assert.match(status, /1 of 3 folders could not be sized completely/);
  assert.match(status, /failed: disk is full/);
  const mount = storageFixture();
  mount.volumes[0].source = "mount";
  mount.folders.forEach((f) => (f.source = "mount"));
  h.context.data = mount;
  h.run("renderStorage(data)");
  assert.match(h.elements.get("storage-status").textContent, /through the network share/);
});

test("folders whose volume was not read are still listed", () => {
  const h = storageHarness();
  h.context.data = storageFixture({ volumes: [] });
  h.run("renderStorage(data)");
  const text = allText(h.elements.get("storage-volumes"));
  assert.match(text, /Anime/);
  assert.match(text, /Movies/);
});

test("measuring asks the server, then polls until it finishes; a late answer for another tab is dropped", async () => {
  const h = storageHarness();
  h.context.calls = [];
  h.context.timers = [];
  h.run('state.tab="stats"; setTimeout=(fn,ms)=>{timers.push(ms);return 1}; api=async(path,body)=>{calls.push([path,body]);return {volumes:[],folders:[],measuring:true}};');
  await h.run("measureStorage()");
  assert.equal(JSON.stringify(h.context.calls[0]), '["storage",{}]');
  assert.equal(JSON.stringify(h.context.timers), "[3000]");
  assert.equal(h.elements.get("storage-refresh").textContent, "Measuring…");
  h.run("timers.length = 0; state.tab = 'jobs'");
  await h.run("loadStorage()");
  assert.equal(h.context.timers.length, 0, "a response arriving after leaving Stats must not keep polling");
  h.run('state.tab="stats"; api=async()=>({volumes:[],folders:[],measuring:false}); timers.length = 0;');
  await h.run("loadStorage()");
  assert.equal(h.context.timers.length, 0, "no polling once the measurement is done");
});

test("Stats opens with the storage numbers and the markup and styles for them exist", () => {
  for (const id of ["storage-refresh", "storage-status", "storage-volumes"]) assert.ok(markup.includes(`id="${id}"`), id);
  for (const cls of [".storage-bar", ".storage-legend", ".storage-c0", ".storage-c5", ".storage-other", ".storage-free", ".storage-trend polyline"]) assert.ok(css.includes(cls), cls);
  assert.match(source, /if \(tab !== "stats"\) clearTimeout\(state\.storageTimer\)/);
  assert.match(source, /else if \(!fromRoute\) safe\(loadStorage\)/);
});

test("files in a worker's line are not counted or labelled as running", () => {
  const h = metricProgressHarness();
  h.run('progressJob.batch={total:6,running:4,queued_on_worker:3,completed:2};');
  assert.equal(h.run('visibleBatch(progressJob).running'), 1);
  assert.match(h.run('batchCounts(visibleBatch(progressJob), true)'), /1 active · 3 waiting for a worker/);
  assert.equal(h.run('segmentParts(visibleBatch(progressJob)).find(part=>part.kind==="run").n'), 1);
  // Never more than the server says is running.
  h.run('progressJob.batch={total:3,running:1,queued_on_worker:5};');
  assert.equal(h.run('visibleBatch(progressJob).running'), 0);
  // A batch whose every active file is in a worker's line is queued, not in progress.
  h.run('progressJob.status="waiting_external";progressJob.activities=[{worker:{transcode_phase:"queued"}},{worker:{transcode_phase:"queued"}}];progressJob.worker={transcode_phase:"queued",transcode_status:"queued"};');
  assert.equal(h.run('queuePresentation(progressJob).status'), "Queued on worker");
  h.run('progressJob.activities=[{worker:{transcode_phase:"encoding"}},{worker:{transcode_phase:"queued"}}];');
  assert.equal(h.run('queuePresentation(progressJob).status'), "In progress");
});

test("Show in Files only locates the file: it is ticked and in view, and its encoding form stays closed", async () => {
  const h = harness();
  h.context.calls = [];
  h.run(`controls = () => {}; loadJobs = async () => {}; state.info = {roots: ["/media"], services: []};
    loadLibrary = async () => {};
    api = async () => ({items: [{path: "/media/tv/ep.mkv", is_dir: false}]});
    locateFileRow = async (path) => calls.push(path);`);
  await h.run('applyNavigation(navigationRoute("?view=files&source=folder%3A%2Fmedia&folder=%2Fmedia%2Ftv&file=%2Fmedia%2Ftv%2Fep.mkv&locate=1"))');
  assert.deepEqual(h.context.calls, ["/media/tv/ep.mkv"]);
  assert.equal(h.run("state.fileStep"), "browse");
  assert.equal(h.elements.get("path").value, "");
  // A saved link that asks for the form (configure=1) still opens it.
  await h.run('applyNavigation(navigationRoute("?view=files&source=folder%3A%2Fmedia&folder=%2Fmedia%2Ftv&file=%2Fmedia%2Ftv%2Fep.mkv&configure=1"))');
  assert.equal(h.run("state.fileStep"), "configure");
  assert.equal(h.elements.get("path").value, "/media/tv/ep.mkv");
});

test("locating a file loads further pages of its folder until the row is there", async () => {
  const h = harness();
  h.context.ticked = [];
  h.context.loads = 0;
  h.run(`state.libraryHasMore = true;
    let present = false;
    const row = {dataset: {selection: "/media/z.mkv"}, closest: () => ({classList: {add() {}}, scrollIntoView() { scrolled = true; }})};
    var scrolled = false;
    fileChecks = () => present ? [row] : [];
    loadLibrary = async () => { loads++; if (loads === 3) present = true; };
    selectOneFile = (key) => ticked.push(key);
    fileSelectionChanged = () => {};`);
  await h.run('locateFileRow("/media/z.mkv")');
  assert.equal(h.context.loads, 3);
  assert.deepEqual(h.context.ticked, ["/media/z.mkv"]);
  assert.equal(h.run("scrolled"), true);
  // A row that never turns up ends when the folder has no more pages.
  h.context.loads = 0;
  h.run('state.libraryHasMore = false; fileChecks = () => []; var told = []; notify = text => told.push(text);');
  await h.run('locateFileRow("/media/missing.mkv")');
  assert.equal(h.context.loads, 0);
  assert.equal(h.run("told.length"), 1);
});

test("returning to Activity restores how far down the list was; other tabs and re-clicks start at the top", async () => {
  const h = harness();
  const scrolls = [];
  h.context.window = { scrollY: 1400, scrollTo: ({ top }) => scrolls.push(top), history: { pushState() {}, replaceState() {} }, location: { pathname: "/", search: "" } };
  h.run('controls = () => {}; loadJobs = async () => {}; loadLibrary = async () => {}; state.tab = "jobs";');
  // The list is followed while it is on screen, so the Back button (which can
  // move the page before the tab changes) does not lose the place.
  h.run("rememberJobsScroll()");
  h.context.window.scrollY = 0;
  h.run('selectTab("library")');
  h.run("rememberJobsScroll()");
  assert.equal(h.run("state.jobsScroll"), 1400);
  h.run('selectTab("jobs")');
  assert.deepEqual(scrolls, [0, 1400]);
  assert.equal(h.run("state.jobsRestore"), 1400);
  // A different filter is a different list: it starts at the top.
  h.run('selectTab("library"); $("job-filter").value = "failed"; selectTab("jobs")');
  assert.equal(scrolls.at(-1), 0);
  assert.equal(h.run("state.jobsRestore"), null);
  h.run('$("job-filter").value = ""; selectTab("library"); selectTab("jobs")');
  assert.equal(scrolls.at(-1), 1400);
  // Tapping Activity while on it goes to the top, not back to an old position.
  h.run('selectTab("jobs")');
  assert.equal(scrolls.at(-1), 0);
  assert.equal(h.run("state.jobsRestore"), null);
  // The list is rebuilt after loading, so the position is set again once it is as long as it was.
  const h2 = harness();
  const scrolls2 = [];
  h2.context.window = { scrollY: 0, scrollTo: ({ top }) => scrolls2.push(top), history: { pushState() {}, replaceState() {} }, location: { pathname: "/", search: "" } };
  h2.run('api = async () => ({jobs: [{id: "j1", action_name: "transcode_media", status: "completed"}], total: 1, has_more: false}); state.tab = "jobs"; state.jobsRestore = 900;');
  await h2.run("loadJobs()");
  assert.deepEqual(scrolls2, [900]);
  assert.equal(h2.run("state.jobsRestore"), null);
  // A load that fails does not leave the position waiting for a later refresh.
  const h3 = harness();
  const scrolls3 = [];
  h3.context.window = { scrollY: 0, scrollTo: ({ top }) => scrolls3.push(top), history: { pushState() {}, replaceState() {} }, location: { pathname: "/", search: "" } };
  h3.run('api = async () => { throw new Error("down"); }; state.tab = "jobs"; state.jobsRestore = 900;');
  await h3.run("loadJobs().catch(() => {})");
  assert.equal(h3.run("state.jobsRestore"), null);
});

test("a batch paused on request reads Paused, not Needs decision", () => {
  const h = metricProgressHarness();
  h.run('progressJob.status="waiting_decision";progressJob.paused=true;progressJob.waiting_options=[{decision:"resume"}];');
  assert.equal(h.run('queuePresentation(progressJob).status'), "Paused");
  assert.equal(h.run('statusClass("Paused")'), "neutral");
  h.run('progressJob.paused=false;');
  assert.equal(h.run('queuePresentation(progressJob).status'), "Needs decision");
});

test("queue rows reserve the same room for their buttons so the badges line up", async () => {
  const app = await readFile(new URL("./assets/app.js", import.meta.url), "utf8");
  assert.match(css, /\.job-row\.has-menu \{ --rail: 3; \}/);
  assert.doesNotMatch(css, /\.job-row\.rail-[23]/);
  assert.doesNotMatch(app, /rail-\$\{/);
});

test("Stats charts what was freed per day and links the recent and waiting files to their jobs", () => {
  const h = commandDom([]);
  const doc = h.document;
  for (const id of ["freed-chart", "freed-summary", "candidate-count", "recent-replaced", "ready-list", "saved-total", "saved-count", "candidate-total", "estimated-total"]) {
    const el = new FakeNode(doc, id.endsWith("list") || id === "recent-replaced" ? "ul" : "div");
    el.id = id;
    doc.body.append(el);
    h.elements.set(id, el);
  }
  const day = (date, saved, count) => ({date, saved_bytes: saved, count, cumulative_bytes: saved});
  const days = Array.from({length: 30}, (_, i) => day(`2026-10-${String(i + 1).padStart(2, "0")}`, 0, 0));
  days[28] = day("2026-10-29", 2 * 1024 ** 3, 2);
  days[29] = day("2026-10-30", 1024 ** 3, 1);
  h.context.data = {
    savings: {realized_bytes: 3 * 1024 ** 3, completed_replacements: 3, candidate_saved_bytes: 5 * 1024 ** 3, candidate_count: 9},
    daily: days,
    history: [{action_id: "old", source_path: "/m/Old.mkv", saved_bytes: 1024 ** 3, completed_at: "2020-01-01T00:00:00Z"}, {action_id: "new", source_path: "/m/New.mkv", saved_bytes: 2 * 1024 ** 3, completed_at: new Date().toISOString()}],
    ready: [{action_id: "cand", source_path: "/m/Next.mkv", source_bytes: 4 * 1024 ** 3, candidate_bytes: 3 * 1024 ** 3, saved_bytes: 1024 ** 3}],
  };
  h.run("renderSavings(data)");
  const bars = h.elements.get("freed-chart").querySelectorAll("i");
  assert.equal(bars.length, 30);
  assert.equal(bars.filter((b) => "empty" in b.dataset).length, 28);
  assert.ok("today" in bars[29].dataset);
  assert.match(h.elements.get("freed-summary").textContent, /^Today 1\.07 GB · 7 days 3\.22 GB · 30 days 3\.22 GB$/);
  assert.equal(h.elements.get("candidate-count").textContent, "9 files · Potential savings");
  const recent = h.elements.get("recent-replaced").querySelectorAll("a");
  assert.deepEqual(recent.map((a) => a.textContent), ["New.mkv", "Old.mkv"], "newest replacement first");
  assert.equal(recent[0].href, "?view=queue&job=new");
  assert.equal(h.elements.get("ready-list").querySelectorAll("a")[0].href, "?view=queue&job=cand");
  assert.match(h.elements.get("ready-list").children.at(-1).textContent, /^8 more waiting/);
  h.run("renderSavings({savings:{}, daily:[], history:[], ready:[]})");
  assert.match(h.elements.get("freed-chart").children[0].textContent, /Nothing freed/);
  assert.match(h.elements.get("recent-replaced").children[0].textContent, /Nothing has been replaced yet/);
});

test("the masthead and the tab bar stay pinned while the page scrolls, except on phones", () => {
  const wide = /@media \(min-width: 601px\) \{([\s\S]*?)\n\}\n/.exec(css.slice(css.indexOf(".tabs {")))[1];
  assert.match(wide, /\.masthead \{\s*position: sticky;\s*top: 0;\s*z-index: 26;\s*height: var\(--masthead-h\);\s*background: var\(--paper\);/);
  assert.match(wide, /\.tabs \{\s*position: sticky;\s*top: var\(--masthead-h\);/);
  // A long worker status is cut with an ellipsis at every width (a button label is one line).
  assert.match(css, /#worker-status \{ min-width: 0; max-width: 100%; overflow: hidden; text-overflow: ellipsis; \}/);
  assert.match(css, /@media \(min-width: 601px\) \{[\s\S]*?#worker-status \{ max-width: 46vw; \}/);
  assert.match(css, /button \{[^}]*white-space: nowrap;/);
  // On phones the tab bar is the fixed bottom bar and the masthead scrolls away.
  assert.match(css, /@media \(max-width: 600px\) \{[\s\S]*?\.tabs \{\s*position: fixed;/);
  assert.doesNotMatch(css.slice(css.indexOf("@media (max-width: 600px) {")), /\.masthead \{[^}]*position: sticky/);
});

test("a disabled action-bar button is flat grey at full opacity, not faded on top of grey", () => {
  assert.match(css, /\.jd-file-actions \{ gap: 6px; \}\n\.jd-file-actions > \.jd-primary, \.jd-file-actions > \.jd-secondary \{ padding-inline: 10px; \}/);
  assert.match(css, /\.jd-primary:disabled, \.jd-secondary:disabled \{[^}]*color: var\(--on-disabled\);[^}]*opacity: 1;/);
});

test("the dialog's primary button keeps a readable hover and its bar is as compact as the other buttons", () => {
  assert.match(css, /\.jd-primary:not\(:disabled\):hover \{ border-color: var\(--primary-hover\); background: var\(--primary-hover\); \}/);
  assert.match(css, /\.jd-actions \{ justify-content: flex-end; \}\n\.jd-actions > \.jd-primary, \.jd-actions > \.jd-secondary \{ flex: 0 1 auto; padding-inline: 14px; \}/);
});

test("every button takes the compact size, with a mouse or a finger", () => {
  const block = css.slice(css.lastIndexOf("/* One button size everywhere"));
  assert.match(block, /^:root \{ --control-size: 30px; --control-radius: 5px;/m);
  // Fields share the buttons' corner and height, and a 16px touch field is as tall as a 13px one.
  assert.match(block, /input:not\(\[type="checkbox"\]\):not\(\[type="radio"\]\), select, textarea \{ border-radius: var\(--control-radius\); \}/);
  assert.match(block, /select \{ line-height: 18px; \}/);
  assert.match(block, /^button, \.jd-primary, \.jd-secondary, \.jd-menu-item,/m);
  assert.match(block, /min-height: var\(--control-size\);\s*padding-block: 2px;\s*font-size: 13px;\s*border-radius: 5px;/);
  // Nothing below it is conditional on the pointer, so a touch screen is not left out.
  assert.doesNotMatch(block, /@media/);
});

test("a swipe settles open past half the tray, archives past most of the row, and springs back otherwise", () => {
  const h = harness();
  const settle = (dx, tray, width, canArchive) => h.run(`swipeSettle(${dx}, ${tray}, ${width}, ${canArchive})`);
  assert.equal(settle(20, 142, 352, true), "closed");
  assert.equal(settle(80, 142, 352, true), "open");
  assert.equal(settle(142, 142, 352, false), "open");
  assert.equal(settle(220, 142, 352, true), "archive");
  // A row that cannot be archived never archives, however far the finger goes.
  assert.equal(settle(300, 142, 352, false), "open");
  // The tray resists past its own width and never follows a drag to the left.
  assert.equal(h.run("swipeOffset(100, 142)"), 100);
  assert.equal(h.run("swipeOffset(242, 142)"), 192);
  assert.equal(h.run("swipeOffset(-30, 142)"), 0);
});

test("the swipe tray is only for touch screens and every shortened label names a real action", () => {
  const block = css.slice(css.indexOf("/* Touch screens: a job's actions wait behind a swipe"));
  assert.match(block, /@media \(hover: none\) and \(pointer: coarse\) \{\s*\.job-row\.has-menu \{ --swipe: 0px; overflow-x: clip; touch-action: pan-y;/);
  // Nothing that reaches the desktop rail is changed outside that query.
  assert.doesNotMatch(block.slice(0, block.indexOf("@media")), /\.job-actions/);
  // A tray tile is one line and as tall as the row, like every button beside it.
  assert.match(block, /\.job-row\.has-menu > \.job-actions > \.action-icon \{[^}]*min-height: 44px;[^}]*\}/);
  const labels = source.match(/const trayLabels = (\{[^}]*\});/)?.[1];
  assert.ok(labels, "trayLabels is declared");
  for (const full of Object.keys(JSON.parse(labels)))
    assert.ok(source.includes(`"${full}"`), `${full} is a label the app uses`);
});

test("a sheet closes past a quarter of its height or a quick flick, and springs back otherwise", () => {
  const h = harness();
  const settle = (dy, height, velocity) => h.run(`sheetSettle(${dy}, ${height}, ${velocity})`);
  assert.equal(settle(20, 800, 0), "back");
  assert.equal(settle(100, 800, 0.1), "back");
  assert.equal(settle(140, 800, 0), "close");
  // A short sheet closes at a quarter of itself, not at the full distance.
  assert.equal(settle(60, 200, 0), "close");
  // A quick flick closes early, but a tiny nudge never does.
  assert.equal(settle(60, 800, 1), "close");
  assert.equal(settle(30, 800, 2), "back");
});

test("the sheet drag moves the sheet and leaves closing to each dialog's own close button", () => {
  const body = source.slice(source.indexOf("function sheetCloseButton"), source.indexOf("// More actions menus close"));
  // Closing is the dialog's own button, so confirmations and clean-up still run.
  assert.match(body, /button\.click\(\)/);
  assert.doesNotMatch(body, /\.close\(\)|\.requestClose\(/);
  assert.match(css, /\.dialog-heading,\s*\.jd-header \{\s*touch-action: none;/);
  // Every dialog has a close button the drag can use.
  for (const dialog of markup.match(/<dialog[\s\S]*?<\/dialog>/g))
    assert.match(dialog, /<button[^>]*aria-label="Close/, dialog.slice(0, 60));
});

test("the server's next steps replace Run again, the remedy and x265, the first as the primary", async () => {
  const h = harness();
  h.run('state.workerInfo={ready:true}');
  const steps = '[{id:"audio_only",label:"Audio only",detail:"Keep the video.",inputs:{mode:"size",audio_only:true}},{id:"x265",label:"Try with x265",detail:"Slower."},{id:"tracks",label:"Choose tracks",detail:"Pick."},{id:"same",label:"Run again unchanged",detail:"Same."}]';
  const base = `{id:"old",action_name:"transcode_media",status:"completed",mode:"size",reason_code:"insufficient_savings",source_path:"/media/Show/S01E01.mkv",rerun:{source_path:"/media/Show/S01E01.mkv",mode:"size",replace_original:false,no_winner_rounds:2,without_hardware:true},next_steps:${steps}}`;
  const rail = h.run(`jobControls(${base})`);
  const labels = rail.children.map(b => b.textContent);
  assert.deepEqual(labels.filter(l => ["Audio only","Try with x265","Choose tracks","Run again unchanged","Run again"].includes(l)), ["Audio only","Try with x265","Choose tracks","Run again unchanged"]);
  assert.equal(rail.children.find(b => b.textContent === "Audio only").className, "primary");
  assert.equal(rail.children.find(b => b.textContent === "Choose tracks").dataset.requiresWorker, "false");
  assert.equal(rail.children.find(b => b.textContent === "Try with x265").dataset.requiresWorker, "true");
  // Hidden once there is a candidate or a decision.
  for (const patch of ['candidate_ready:true', 'status:"waiting_decision"', 'replacement_action_id:"r"'])
    assert.ok(!h.run(`jobControls({...${base},${patch}}).children.map(b=>b.textContent)`).includes("Audio only"), patch);

  const calls = [], reviews = [];
  h.context.calls = calls; h.context.reviews = reviews;
  h.run('api=async(path,body)=>{calls.push({path,body});return {id:"new"}};loadJobs=async()=>{};openJob=async()=>{};notify=()=>{};let n=0;submissionID=()=>"key-"+(++n);reviewAction=async(r)=>{reviews.push(r);return true};let opened="";openGoalForm=p=>{opened=p};');
  await h.run(`runNextStep(${base}, ${base}.next_steps[0])`);
  assert.equal(JSON.stringify(calls[0]), JSON.stringify({path: "rerun", body: {id: "old", key: "key-1", step: "audio_only"}}));
  assert.match(reviews[0].content.map(n => n.textContent).join(" "), /Keep the video\./);
  await h.run(`runNextStep(${base}, ${base}.next_steps[3])`);
  assert.match(reviews[1].content.map(n => n.textContent).join(" "), /already had 2 rounds/);
  await h.run(`runNextStep(${base}, ${base}.next_steps[2])`);
  assert.equal(h.run("opened"), "/media/Show/S01E01.mkv");
  assert.equal(calls.length, 2);

});

test("a batch file applies a next step as a goal change for that file", async () => {
  const h = harness();
  const sent = [];
  h.context.sent = sent;
  h.run('api=async()=>({id:"batch-a",item_key:"k",mode:"size",selection_version:"v",decision_version:"d",settings:{allow_hardware:true,hardware_margin_percent:30}});commandRequest=async(path,body)=>{sent.push([path,body]);return {}};loadJobs=async()=>{};refreshOpenDetail=async()=>{};notify=()=>{};reviewAction=async()=>true;');
  await h.run('applyBatchStep("batch-a","k",{id:"audio_only",label:"Audio only",detail:"d",inputs:{mode:"size",audio_only:true}})');
  assert.equal(sent[0][0], "batch-settings");
  assert.deepEqual([sent[0][1].scope, sent[0][1].item_key, sent[0][1].mode, sent[0][1].audio_only, "allow_hardware" in sent[0][1]], ["file", "k", "size", true, false]);
  await h.run('applyBatchStep("batch-a","k",{id:"x265",label:"Try with x265",detail:"d"})');
  assert.deepEqual([sent[1][1].mode, sent[1][1].allow_hardware, "hardware_margin_percent" in sent[1][1]], ["size", false, false]);
  // Declining sends nothing.
  h.run("reviewAction=async()=>false");
  await h.run('applyBatchStep("batch-a","k",{id:"budget",label:"Aim",detail:"d",inputs:{mode:"size",size_budget_percent:80}})');
  assert.equal(sent.length, 2);
  // A new budget keeps x265 for a file that ran without the media engine.
  h.run('reviewAction=async()=>true;api=async()=>({id:"batch-a",item_key:"k",mode:"size",selection_version:"v",decision_version:"d",settings:{allow_hardware:false}})');
  await h.run('applyBatchStep("batch-a","k",{id:"budget",label:"Aim",detail:"d",inputs:{mode:"size",size_budget_percent:60}})');
  assert.deepEqual([sent[2][1].size_budget_percent, sent[2][1].allow_hardware], [60, false]);
  // x265 under another goal opens the goal form for the file instead.
  const opened = [];
  h.context.opened = opened;
  h.run('api=async()=>({id:"batch-a",item_key:"k",mode:"quality",selection_version:"v",decision_version:"d",settings:{}});reconfigureBatch=async(...a)=>{opened.push(a)}');
  await h.run('applyBatchStep("batch-a","k",{id:"x265",label:"Try with x265",detail:"d"})');
  assert.equal(sent.length, 3);
  assert.equal(JSON.stringify(opened[0]), JSON.stringify(["batch-a", false, {item_key: "k"}]));
});

test("batch rows list the next steps and say what happened", async () => {
  const items = [
    { item_key: "k-s", display_label: "Missed", status: "skip", child_action_id: "t-s", file_path: "/media/tv/S.mkv", can_reconfigure: true, mode: "size", reason_code: "insufficient_savings",
      explanation: { happened: "The new file came out 9% smaller.", learned: ["Video is 2 GB."] },
      next_steps: [{ id: "audio_only", label: "Audio only", detail: "d", inputs: { mode: "size", audio_only: true } }, { id: "same", label: "Run again unchanged", detail: "d" }] },
    { item_key: "k-c", display_label: "Cancelled", status: "cancelled", child_action_id: "t-c", file_path: "/media/tv/C.mkv", can_reconfigure: true, mode: "size", next_steps: [{ id: "same", label: "Run again", detail: "d" }] },
  ];
  const h = harness();
  h.context.items = items;
  h.run('state.info={allow_destructive:true,roots:["/media"]};state.detail="batch-a";state.detailRevision=2;$("job-detail").open=true;state.detailJob={status:"completed",batch:{}};api=async()=>({items,total:2});');
  await h.run('loadBatchItems("batch-a",2)');
  const rows = itemRows(h);
  const first = buttonLabels(rows[0]);
  assert.ok(first.includes("Audio only") && first.includes("Encode again") && first.includes("Change goal"), first.join());
  assert.ok(flatten(rows[0]).some(e => e.textContent === "The new file came out 9% smaller."));
  // "same" becomes the one Encode again, not two.
  assert.equal(buttonLabels(rows[1]).filter(l => l === "Encode again").length, 1);
});

test("the explanation says what happened and what was learned", () => {
  const h = harness();
  assert.equal(h.run("explanationReport(null)"), null);
  const box = h.run('explanationReport({happened:"It came out 9% smaller.",learned:["Audio is 41%.","Converting the audio alone would save 1 GB."]})');
  const text = flatten(box).map(e => e.textContent).join(" | ");
  assert.match(text, /What happened: /);
  assert.match(text, /Converting the audio alone/);
});
