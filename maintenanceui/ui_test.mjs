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
    globalThis.progressJob = {status:"waiting_external",waiting_condition:"benchmark_complete",worker:{
      benchmark_status:"running",phase:"evaluating_metrics",progress:73.3,progress_is_stale:true,
      last_progress_at:"2026-10-16T12:22:18.667341Z",worker_heartbeat_at:"2026-10-16T12:22:53Z",
      progress_details:{completed_units:22,total_units:30,candidate_number:2,sample_number:3,metric:"vmaf"}
    }};`);
  return h;
}
function progressText(element) {
  return [element.textContent, ...element.children.map(progressText)].join(" ");
}

test("long VMAF and CAMBI measurements show real units and heartbeat without declaring a stall", () => {
  for (const metric of ["vmaf", "cambi"]) {
    for (const progress of [70, 73.3]) {
      const h = metricProgressHarness();
      h.context.metric = metric;
      h.context.measuredProgress = progress;
      h.run('progressJob.worker.progress_details.metric=metric;progressJob.worker.progress=measuredProgress;');
      // Also cover a single metric lasting ten minutes, with only heartbeats.
      h.run('progressJob.worker.last_progress_at="2026-10-16T12:12:54Z";');
      const before = h.run('JSON.stringify(progressJob)');
      const rendered = h.run('telemetry(progressJob)');
      const text = progressText(rendered);
      assert.equal(h.run('queuePresentation(progressJob).status'), "Measuring quality");
      assert.equal(h.run('statusClass(queuePresentation(progressJob).status)'), "running");
      assert.match(text, /Measuring sample quality.*22\/30 sample tasks resolved.*Latest sample 3.*Candidate 2/);
      assert.ok(text.includes(metric.toUpperCase()));
      assert.match(text, /Worker heartbeat 1s ago.*Last advance 10m 0s ago/);
      assert.match(text, /Progress updates when a sample task finishes/);
      assert.ok(text.includes(`Last measured: ${progress.toFixed(1)}%`));
      assert.equal(rendered.children[1].value, progress);
      assert.doesNotMatch(text, /No updates|No recent progress|stale|About .* min/);
      assert.equal(h.run('JSON.stringify(progressJob)'), before);
    }
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
    assert.match(text, stale ? /No worker heartbeat for 4m 54s.*Measuring sample quality/ : /Worker heartbeat unavailable/);
    assert.doesNotMatch(text, /Navigatorr has not checked|The worker has not reported/);
    assert.match(text, /Last reported: 73.3%/);
    assert.equal(h.run('telemetry(progressJob).children.length'), 1);
  }
  const h = metricProgressHarness();
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:18:00Z";progressJob.worker.progress_is_stale=false;');
  assert.equal(h.run('queuePresentation(progressJob).status'), "No heartbeat", "fresh percentage must not hide a lost heartbeat");
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:22:24Z";');
  assert.equal(h.run('queuePresentation(progressJob).status'), "Measuring quality");
  h.run('Date.now=()=>Date.parse("2026-10-16T12:26:00Z");');
  assert.equal(h.run('queuePresentation(progressJob).status'), "No heartbeat", "heartbeat must expire without new API data");
});

test("a heartbeat delayed by a busy coordinator is shown calmly, and silence is a warning, not a failure", () => {
  const h = metricProgressHarness();
  // Ninety seconds without a sample: the coordinator was busy hashing another source.
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:21:24Z";progressJob.worker.last_worker_poll_at="2026-10-16T12:21:24Z";');
  assert.equal(h.run('queuePresentation(progressJob).status'), "Measuring quality");
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
  assert.match(text, /Estimated workflow progress · Not time remaining/);
  // Files that finished are forgotten; the smoothing map stays bounded.
  h.run('for(let i=0;i<100;i++)encodeEta("old-"+i,{transcode_phase:"encoding",speed:1},600);');
  assert.ok(h.run('encodeEtas.size') <= 64);
  h.run('Date.now=()=>Date.parse("2026-10-16T12:40:00Z");encodeEta("fresh",{transcode_phase:"encoding",speed:1},600);');
  assert.equal(h.run('encodeEtas.size'), 1);
  assert.equal(h.run('remainingLabel(45)'), "less than a minute left");
  assert.equal(h.run('remainingLabel(3600)'), "~1 h left");
});

test("validation, publication and sample encoding have discrete progress but require a fresh heartbeat", () => {
  for (const [phase, status] of [["analysing_video", "Analyzing video"], ["validating", "Verifying"], ["validating_quality", "Verifying quality"], ["publishing", "Saving candidate"], ["encoding_samples", "Encoding samples"]]) {
    const h = metricProgressHarness();
    h.context.phase = phase;
    h.run('progressJob.worker.phase=phase;if(phase!=="encoding_samples"){progressJob.worker.transcode_phase=phase;progressJob.worker.transcode_status="running";}progressJob.worker.speed=14.7;progressJob.worker.fps=238;progressJob.savings={eta_seconds:600};');
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
  for (const condition of ["worker_unreachable", "benchmark_retry", "transcode_retry", "worker_reconciling", "worker_busy"]) {
    h.context.condition = condition;
    h.run('progressJob.waiting_condition=condition;progressJob.waiting_reason="Waiting for coordination";');
    assert.equal(h.run('queuePresentation(progressJob).status'), condition === "worker_unreachable" ? "Worker offline" : condition === "worker_busy" ? "Waiting for a free worker slot" : "Waiting");
    assert.doesNotMatch(progressText(h.run('telemetry(progressJob)')), /Measuring sample quality|Worker heartbeat/);
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
    assert.doesNotMatch(h.run('queuePresentation(progressJob).status'), /No heartbeat|No updates|Measuring quality/);
    assert.doesNotMatch(progressText(h.run('telemetry(progressJob)')), /No recent|Worker heartbeat/);
  }
  for (const status of ["completed", "failed", "cancelled", "queued"]) {
    h.context.status = status;
    h.run('progressJob.status="running";progressJob.worker.benchmark_status=status;progressJob.worker.phase=status;');
    assert.doesNotMatch(progressText(h.run('telemetry(progressJob)')), /No recent|Worker heartbeat/);
  }
});

test("batch file detail and worker slots agree about a long metric and lost heartbeat", () => {
  const h = metricProgressHarness();
  h.run('progressJob.batch={total:2,running:1};progressJob.activities=[{id:"child",worker:progressJob.worker,waiting_condition:"benchmark_complete",current_step:0,stages:[{name:"wait_benchmark"},{name:"wait_transcode"}]}];');
  const label = h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)');
  assert.match(label, /Measuring sample quality.*Worker heartbeat received/);
  assert.doesNotMatch(label, /stale|No updates/);
  assert.match(progressText(h.run('batchProgress(progressJob)')), /Measuring sample quality.*Worker heartbeat received/);
  h.run('progressJob.worker.worker_heartbeat_at="2026-10-16T12:15:00Z";');
  assert.match(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), /No worker heartbeat for 7m 54s/);
  assert.match(progressText(h.run('batchProgress(progressJob)')), /No worker heartbeat for 7m 54s/);
  h.run('progressJob.worker.last_worker_poll_at="2026-10-16T12:15:00Z";');
  assert.match(h.run('batchFileActivity({status:"running",child_action_id:"child"},progressJob)'), /Navigatorr has not checked the worker for 7m 54s/);
  assert.match(progressText(h.run('batchProgress(progressJob)')), /Navigatorr has not checked the worker for 7m 54s/);
  h.run('renderWorkerActivity({available:true,activity:{details_available:true,worker_slots_total:1,worker_slots_used:1,jobs:[{id:"child",kind:"benchmark",file:"episode.mkv",phase:"evaluating_metrics",heartbeat_at:progressJob.worker.worker_heartbeat_at}]}});');
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
  assert.deepEqual(calls, ["action_detail", "action_detail"]);
  assert.equal(h.elements.get("batch-review").open, true);
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
  assert.equal(mixed.children[1].textContent, "60%");
  assert.match(mixed.children[0].title, /Cancelled files.*not counted/);
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

test("overall batch progress includes sample work, conversion and validation while retaining true finished counts", () => {
 const h=harness();
 const stages=["preflight","submit_benchmark","wait_benchmark","submit_transcode","wait_transcode","validate_result","accept_result"].map(name=>({name}));
 h.context.stages=stages;
 const progress=h.run('batchProgress({status:"waiting_external",batch:{total:2,running:2,completed:0},activities:[{id:"one",file:"one.mkv",current_step:2,stages,worker:{benchmark_phase:"evaluating_metrics",benchmark_progress_details:{completed_units:20,total_units:40}}},{id:"two",file:"two.mkv",current_step:5,stages}]})');
 assert.equal(progress.children[0].textContent,"0 / 2 files finished");
 assert.equal(progress.children[1].textContent,"~54%");
 assert.match(progress.children[3].textContent,/Measuring sample quality.*Verify/);
 const finish=h.run('batchProgress({status:"waiting_external",batch:{total:2,completed:2}})');
 assert.equal(finish.children[1].textContent,"~99%");assert.match(finish.children[3].textContent,/Finishing/);
});

test("full conversion progress does not show counters from completed sample testing", () => {
 const h=harness();
 const encoding=h.run('telemetry({worker:{transcode_status:"running",transcode_phase:"encoding",progress:64,progress_details:{completed_units:65,total_units:185}}})');
 assert.match(encoding.children[0].textContent,/Encoding full video/);
 assert.doesNotMatch(encoding.children[0].textContent,/sample tasks/);
 assert.equal(encoding.children[1].value,64);
 const testing=h.run('telemetry({worker:{benchmark_status:"running",benchmark_phase:"evaluating_metrics",progress:35,progress_details:{completed_units:65,total_units:185}}})');
 assert.match(testing.children[0].textContent,/65\/185 sample tasks/);
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

test("a nested benchmark contributes to its parent workflow once and replacement checks retain their share",()=>{
 const h=harness();h.context.stages=["preflight","submit_benchmark","wait_benchmark","submit_transcode","wait_transcode","validate_result","accept_result"].map(name=>({name}));
 const progress=h.run('batchProgress({status:"waiting_external",batch:{total:1,running:1},activities:[{id:"parent",file:"a.mkv",current_step:2,stages},{id:"benchmark",parent_action_id:"parent",file:"a.mkv",current_step:2,stages:stages.slice(0,3),worker:{benchmark_phase:"encoding_samples",progress:50}}]})');
 assert.equal(progress.children[1].textContent,"~36%");assert.equal(progress.children[3].textContent.split("a.mkv").length,2);
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
  assert.equal(row.children[1].children.length,1,'no repeated filename or reason');
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
  assert.match(text(rows[0]),/only the Much smaller goal converts it/);
  assert.match(text(rows[0]),/Change goal/);
  assert.match(text(rows[1]),/Much smaller · SDR 720p/);
});

test("compact replacement reviews warn about re-grabs and show read-only tracking", async () => {
  const h = harness();
  h.context.current = {id:"replace",status:"waiting_decision",waiting_options:[{decision:"approve"}],promotion:{original_path:"/media/one.mkv",candidate_path:"/media/.candidates/one.mkv",original_sha256:"original",candidate_sha256:"candidate",original_bytes:1000,candidate_bytes:400,regrab_warning:"May download the original again.",arr_monitored:true,arr_quality_profile:"Any"}};
  h.run('api = async () => ({jobs:[current]});');
  await h.run('reviewFilePromotion("replace")');
  const all = el=>[el,...el.children.flatMap(all)];
  const text = el=>all(el).map(e=>e.textContent).filter(Boolean).join(" ");
  const first = h.elements.get("file-review-data").children[0];
  assert.match(text(first), /download the original again/);
  assert.match(text(first), /Monitored · Quality profile: Any/);
  h.context.notice = {regrab_warning:"May download the original again.",arr_monitored:false};
  h.context.plan = {batch_id:"batch-a",digest:"sha256:plan",members:[{item_key:"e1",original_path:"/source.mkv",candidate_path:"/candidate.mkv"}]};
  h.run('tool = async (name, args) => ({data: args.key === "batch_promotion_notice" ? notice : plan});');
  await h.run('reviewBatchPromotion("batch-a")');
  const batch = h.elements.get("batch-review-data").children[0];
  assert.match(text(batch), /Not monitored/);
  h.context.notice = null;
  await h.run('reviewBatchPromotion("batch-a")');
  assert.doesNotMatch(text(h.elements.get("batch-review-data")), /download the original again/);
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
 assert.match(JSON.stringify(h.elements.get('reconfigure-profile-info').children),/Tests quality samples before full conversion/);
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
  assert.equal(options[2].textContent, 'Live action · CPU · Test samples');
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
  assert.match(facts, /Tests quality samples before full conversion/);
  assert.match(facts, /Profile ID: live-action-hevc/);
});

test("batch file context distinguishes scheduling, paused work and actual worker phases", () => {
  const h = harness();
  assert.match(h.run('batchFileActivity({status:"queued"},{status:"running"})'), /Not sent to the worker yet/);
  assert.match(h.run('batchFileActivity({status:"queued"},{paused:true})'), /Resume the batch/);
  assert.match(h.run('batchFileActivity({status:"queued",child_action_id:"child"},{})'), /File task created/);
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},{activities:[{id:"child",worker:{benchmark_phase:"evaluating_metrics"}}]})'), 'Measuring sample quality');
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
 const rendered=JSON.stringify(h.elements.get("detail-summary").children);assert.match(rendered,/Coordinator · Inventory/);assert.match(rendered,/20 ms/);assert.match(rendered,/Unknown/);assert.match(rendered,/CPU time is unknown/);assert.match(rendered,/phases may overlap/);assert.doesNotMatch(rendered,/Total.*520/);
});

test("phase cost detail shows known cache observations and distinguishes search budget from consumption",()=>{
 const h=harness();h.run('renderPhaseCosts($("detail-summary"),{worker:{benchmark_search_seconds:4,search_budget_seconds:600,operation_search_budget_seconds:3600},phase_costs:{worker_staging:{duration_ms:1,attempts:1,cache_hits:0,cache_misses:1}}});');const rendered=JSON.stringify(h.elements.get("detail-summary").children);assert.match(rendered,/0 \/ 1/);assert.match(rendered,/measured wall: 4 s/);assert.match(rendered,/Budget per file: 600 s/);assert.doesNotMatch(rendered,/Operation budget/);assert.match(rendered,/reservations are not measured consumption/);
});

test("phase cost detail names the extended search ceiling only when the worker reports one",()=>{
 const render=worker=>{const h=harness();h.context.job={worker,phase_costs:{}};h.run('renderPhaseCosts($("detail-summary"),job);');return JSON.stringify(h.elements.get("detail-summary").children);};
 assert.match(render({benchmark_search_seconds:900,search_budget_seconds:600,search_budget_max_seconds:1800}),/Budget per file: 600 s, extended to at most 1800 s while samples keep finishing\./);
 assert.match(render({benchmark_search_seconds:900,search_budget_seconds:600,search_budget_max_seconds:2400,search_budget_verify_seconds:600}),/extended to at most 2400 s while samples keep finishing, 3000 s while verifying a winner\./);
 assert.doesNotMatch(render({benchmark_search_seconds:4,search_budget_seconds:600}),/extended/);
 assert.doesNotMatch(render({benchmark_search_seconds:4,search_budget_seconds:600,search_budget_max_seconds:600}),/extended/);
 assert.doesNotMatch(render({benchmark_search_seconds:4,search_budget_max_seconds:1800}),/extended/);
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
  assert.match(text(rows[2]),/Needs attention.*Change goal.*Search reached its limit/);
  assert.doesNotMatch(rows.map(text).join(" "),/quality_not_met|per-source bounded|Profile: mode-size/);
  for (const [index,label] of [[0,"Candidate ready"],[1,"Skipped"],[2,"Needs attention"]]) {
    const badge=rows[index].children.find(el=>el.className.includes("status-action"));
    assert.ok(badge,`${label} is actionable`);
    await badge.listeners.get("click")();
  }
  await all(rows[0]).find(el=>el.textContent==="Replace file").listeners.get("click")();
  assert.deepEqual(calls,[["open","ready-child"],["settings","batch","skip"],["open","wait-child"],["replace","batch","ready-child"]]);
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

test("file details offer goal changes and keep-original decisions without duplicate cancellation",()=>{
  const h=harness();
  const labels=h.run('jobControls({id:"child",action_name:"transcode_media",status:"waiting_decision",reason_code:"search_budget_exhausted",mode:"size",parent_action_id:"batch",parent_item_key:"one",waiting_options:[{decision:"cancel"}]}).children.map(b=>b.textContent)');
  assert.ok(labels.includes("Keep original"));assert.ok(labels.includes("Change goal"));assert.ok(!labels.includes("Cancel"));
  const skipped=h.run('queuePresentation({action_name:"transcode_media",status:"completed",reason_code:"quality_not_met",candidate_ready:false})');
  assert.equal(skipped.status,"Skipped");assert.match(skipped.summary,/quality target; original kept/);
  const unsupported=h.run('queuePresentation({action_name:"transcode_media",status:"completed",reason_code:"unsupported_source",candidate_ready:false})');
  assert.equal(unsupported.status,"Skipped");assert.match(unsupported.summary,/does not support this source format yet/);
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
  assert.equal(rail.children.filter((b) => /\baction-icon\b/.test(b.className)).length, 3);
  const none = queueRail(["Archive", "Compare frames"]).rail;
  assert.equal(none.children.some((b) => /\baction-main\b/.test(b.className)), false);
});

test("icon-only queue actions keep their full name in aria-label and title", () => {
  const { rail } = queueRail(["Resume batch", "Stop batch · keep originals", "Change settings"]);
  const icons = rail.children.filter((b) => /\baction-icon\b/.test(b.className));
  assert.deepEqual(icons.map((b) => b.attrs["aria-label"]), ["Stop batch · keep originals", "Change settings"]);
  assert.deepEqual(icons.map((b) => b.title), ["Stop batch · keep originals", "Change settings"]);
  assert.match(icons[0].className, /\bdanger\b/);
  // The row hides the label; it is kept for the More menu, never as a second main action.
  assert.deepEqual(icons.map((b) => b.children.at(-1).className), ["action-label", "action-label"]);
  assert.equal(rail.children[0].attrs["aria-label"], "Resume batch");
});

test("with five or more icon actions, three show and the rest go into a More actions menu", () => {
  const { rail } = queueRail(["Review candidate", "Run one more test", "Keep original", "Change goal", "Archive", "View batch"]);
  assert.equal(rail.children.length, 5);
  const more = rail.children.at(-1);
  assert.equal(more.className, "more-actions");
  assert.equal(more.dataset.menuFor, "job-1");
  const [toggle, menu] = more.children;
  assert.equal(toggle.attrs["aria-label"], "More actions");
  assert.equal(toggle.title, "More actions");
  assert.deepEqual(menu.children.map((b) => b.attrs["aria-label"]), ["Archive", "View batch"]);
  // The same elements move, so their handlers and job-control state come along.
  assert.ok(menu.children.every((b) => b.listeners.has("click") && b.dataset.jobControl === "job-1"));
  more.open = true;
  more.listeners.get("click")({ target: { closest: () => menu.children[0] } });
  assert.equal(more.open, false);
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
  h.run('actionMenuKeydown({ key: "Escape" })');
  assert.equal(menu.open, false);
  assert.equal(other.menu.open, false);
  assert.equal(focused, true);
});

test("a podcast review card labels Review cuts instead of leaving it an icon", () => {
  const cuts = queueRail(["Ad library", "Review cuts", "Revise labels", "Archive"]).rail;
  assert.equal(cuts.children[0].attrs["aria-label"], "Review cuts");
  assert.match(cuts.children[0].className, /\baction-main\b/);
  const classify = queueRail(["Ad library", "View transcript", "Validate classifications"]).rail;
  assert.equal(classify.children[0].attrs["aria-label"], "Validate classifications");
});

test("four icon actions all show; a More menu never holds a single item", () => {
  const four = queueRail(["Review", "Run one more test", "Keep original", "Change goal", "View batch"]).rail;
  assert.equal(four.children.some((c) => c.className === "more-actions"), false);
  assert.deepEqual(
    four.children.filter((b) => /\baction-icon\b/.test(b.className)).map((b) => b.attrs["aria-label"]),
    ["Run one more test", "Keep original", "Change goal", "View batch"],
  );
  const five = queueRail(["Review", "Run one more test", "Keep original", "Cancel", "Change goal", "View batch"]).rail;
  const more = five.children.at(-1);
  assert.equal(more.className, "more-actions");
  assert.equal(five.children.filter((b) => /\baction-icon\b/.test(b.className)).length, 3);
  assert.equal(more.children[1].children.length, 2);
});

test("sample test results explain each rejected setting with its measurement and next step", () => {
  const h = harness();
  h.context.job = {id:"j",action_name:"transcode_media",mode:"quality",status:"completed",reason_code:"no_suitable_candidate",candidate_ready:false,source_path:"/media/Demo Film A.mkv",
    benchmark:{outcome:"no_suitable_candidate",message:"None of the tested configurations met this goal. Originals preserved.",next_step:"Keep the original, or explicitly choose another processing goal.",
      rejected:[{candidate_id:"x265-crf20",checks:[{code:"quality_vmaf_p5_below_minimum",reason:"The lower-quality portion is below the VMAF limit.",actual:91.2,limit:93}],failed_samples:[1,2],review_source_seconds:1834.5}]}};
  const text = allText(h.run("benchmarkReport(job)"));
  assert.match(text, /No setting passed|None of the tested configurations/);
  assert.match(text, /x265-crf20/);
  assert.match(text, /Measured 91\.2, limit 93\.0/);
  assert.match(text, /2 samples below the limit/);
  assert.match(text, /Worst moment to check in the source: 30:34/);
  assert.match(text, /Next step: .*another processing goal/);
  const row = h.run("queuePresentation(job)");
  assert.equal(row.status, "Skipped", "a goal that produced no file is not shown as completed");
  assert.match(h.run("queueOutcome(job)"), /No tested setting passed \(1 rejected\) · Original kept/);
  assert.equal(h.run('benchmarkReport({benchmark:{outcome:"testing",message:"x"}})'), null);
  const selected = allText(h.run('benchmarkReport({benchmark:{outcome:"selected",message:"Selected.",next_step:"Monitor the same action."}})'));
  assert.doesNotMatch(selected, /Next step/, "a normal selection needs no extra instruction");
  const larger = h.run('({row: queueOutcome({benchmark:{outcome:"selected",size_warning:"Estimated 6.3% larger than the original.",next_step:"Keep the original."}}), report: benchmarkReport({benchmark:{outcome:"selected",size_warning:"Estimated 6.3% larger than the original.",next_step:"Keep the original."}})})');
  assert.match(larger.row, /would make the file larger/);
  assert.match(allText(larger.report), /6\.3% larger.*Next step:\s+Keep the original/);
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

test("the encoding plan line says whether a sample test backs the settings", () => {
  const h = harness();
  const note = "No benchmark candidate was selected; the stored plan is the profile's untested starting configuration.";
  const untested = h.run(`encodingPlanNote({benchmark:{outcome:"no_acceptable_candidate",plan_source:"profile_base",plan_note:${JSON.stringify(note)}}})`);
  assert.match(allText(untested), /Encoding plan:.*untested starting configuration/);
  assert.match(untested.className, /plan-profile_base/);
  assert.match(allText(h.run('encodingPlanNote({benchmark:{outcome:"selected",plan_source:"benchmark_winner"}})')), /selected by the sample test/);
  assert.equal(h.run('encodingPlanNote({benchmark:{outcome:"testing"}})'), null, "a provisional preflight plan is not shown");
  assert.equal(h.run("encodingPlanNote({})"), null);
});

test("a job paused because quality checks are unavailable is not shown as still testing", () => {
  const h = harness();
  h.context.job = {id:"p",action_name:"transcode_media",mode:"compact",status:"waiting_decision",waiting_reason:"quality_vmaf_model_unavailable: v1_1080p_3h; original preserved",
    benchmark:{outcome:"quality_policy_unavailable",message:"No sample test was started: the worker cannot run the quality checks this goal requires. The original is preserved.",next_step:"Cancel to keep the original."}};
  const text = allText(h.run("benchmarkReport(job)"));
  assert.match(text, /Quality checks unavailable/);
  assert.match(text, /No sample test was started/);
  assert.match(text, /Next step: .*Cancel to keep the original/);
  assert.doesNotMatch(text, /quality_policy_unavailable/);
  assert.match(allText(h.run('benchmarkReport({benchmark:{outcome:"attention",message:"Search budget exhausted.",next_step:"Review."}})')), /Needs your decision/);
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

test("an extra benchmark round runs only after the evidence was shown and confirmed", async () => {
  for (const confirm of [false, true]) {
    const h = harness(), calls = [];
    h.context.record = (...args) => calls.push(args);
    h.context.job = {id:"review",action_name:"transcode_media",mode:"size",status:"waiting_decision",source_path:"/media/S03E05.mkv",
      waiting_reason:"Two benchmark rounds for these source bytes already found no acceptable candidate.",
      waiting_options:[{decision:"cancel",description:"Stop."},{decision:"run_once_after_review",description:"Explicitly authorize one additional bounded benchmark. Do not choose this automatically."}],
      benchmark:{outcome:"search_review_required",message:"Two earlier rounds found no acceptable candidate.",previous_unsuccessful_actions:["act-1"],proposed_candidates:[{id:"x265-crf23",video_codec:"libx265",quality:23}],
        next_step:"Use the displayed run_once_after_review decision only on explicit user instruction."}};
    h.run("jobControl=async(...args)=>record(...args);");
    const button = h.run("jobControls(job).children.find(b=>b.textContent===\"Run one more test\")");
    assert.ok(button, "the decision has a short label instead of the engine description");
    button.listeners.get("click")();
    for (let i = 0; i < 6; i++) await Promise.resolve();
    assert.equal(h.elements.get("action-review").open, true);
    assert.equal(calls.length, 0, "nothing is sent before the confirmation");
    const shown = allText(h.elements.get("action-review-content"));
    assert.match(shown, /x265-crf23/);
    assert.match(shown, /“Run one more test”/);
    assert.doesNotMatch(shown, /run_once_after_review/);
    h.elements.get(confirm ? "confirm-action-review" : "dismiss-action-review").listeners.get("click")();
    for (let i = 0; i < 12; i++) await Promise.resolve();
    assert.deepEqual(JSON.parse(JSON.stringify(calls)), confirm ? [["review","action_resume",{decision:"run_once_after_review"}]] : []);
  }
});

test("the search review shows earlier measurements, marks new settings and hides a repeat", () => {
  const h = harness();
  const vmaf = {mean:94.87,p5:91.5,min:90.3,worst_window_mean:92.8,worst_window_seconds:5};
  h.context.job = {id:"review",action_name:"transcode_media",mode:"size",status:"waiting_decision",source_path:"/media/episode.mkv",
    waiting_reason:"Two earlier test rounds already tried every setting this test would use, with the same or a more lenient quality requirement, and none was acceptable. Running it again would repeat them, so it is not offered. The original is unchanged.",
    waiting_options:[{decision:"cancel",description:"Stop and keep the original."}],
    benchmark:{outcome:"search_review_required",message:"Two earlier rounds found no acceptable candidate.",new_candidates:[],
      previous_rounds:[{action_id:"act-1",mode:"size",floor:{minimum:88,p5_minimum:84,worst_window_minimum:85},candidates:[{id:"crf16",video_codec:"libx265",quality:16,preset:"slow",vmaf}]}],
      proposed_candidates:[{id:"next-1",video_codec:"libx265",quality:16,preset:"slow"}],
      measured:[{candidate_id:"crf20",video_codec:"libx265",quality:20,passed:false,vmaf:{mean:90,p5:85.5,min:84,worst_window_mean:85.95,worst_window_seconds:5}}],
      next_step:"Every proposed candidate was already measured; run_once_after_review is not offered.",
      user_next_step:"Keep the original. Every setting this test would try already failed in earlier rounds, so testing them again is not offered."}};
  assert.equal(h.run('jobControls(job).children.find(b=>b.textContent==="Run one more test")'), undefined);
  const text = progressText(h.run('benchmarkReport(job)'));
  assert.match(text, /Earlier round 1 · Size goal \(VMAF ≥ 88, p5 ≥ 84, worst window ≥ 85\)/);
  assert.match(text, /crf16 · x265 CRF 16 · slow · VMAF mean 94.9 · p5 91.5 · worst 5 s 92.8 · lowest frame 90.3/);
  assert.match(text, /next-1 .* already tried/);
  assert.match(text, /crf20 .* p5 85.5 .* did not pass/);
  assert.match(text, /Next step: .*already failed in earlier rounds/);
  assert.doesNotMatch(text, /run_once|action/);
  assert.match(h.run('shortJobReason(job.waiting_reason)'), /already tried these settings; another test is not offered/);
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

test("the queue comes before podcast cleaning, which opens in its own dialog", () => {
  const jobs = markup.slice(markup.indexOf('<section id="jobs"'), markup.indexOf('<section id="stats"'));
  assert.ok(jobs.indexOf('class="queue-toolbar"') < jobs.indexOf('id="podcast-new"'));
  assert.ok(jobs.indexOf('id="podcast-new"') < jobs.indexOf('id="jobs-list"'));
  assert.doesNotMatch(jobs, /id="podcast-form"|<details id="podcast-new"/);
  const dialog = markup.slice(markup.indexOf('<dialog id="podcast-dialog"'), markup.indexOf("</dialog>", markup.indexOf('<dialog id="podcast-dialog"')));
  assert.match(dialog, /id="podcast-form"/);
  assert.match(dialog, /id="close-podcast"/);
});

test("clean podcast opens, lists podcasts and closes after starting", async () => {
  const h = harness();
  const calls = [];
  h.context.record = (name, args) => calls.push([name, args]);
  h.run('api=async(path)=>{record("api",path);return {enabled:true,podcasts:["genwhy"]}}; tool=async(name,args)=>{record(name,args);return {id:"podcast-job"}}; loadJobs=async()=>{}; openJob=async(id)=>record("open",id);');
  h.elements.get("podcast-new").listeners.get("click")();
  for (let i = 0; i < 6; i++) await Promise.resolve();
  assert.equal(h.elements.get("podcast-dialog").open, true);
  assert.deepEqual(calls[0], ["api", "podcasts"]);
  assert.equal(h.elements.get("podcast-id").children.length, 1);
  h.run('$("podcast-id").value="genwhy"; $("podcast-source").value="/media/p/e.mp3"; $("podcast-output").value="/media/p/e.clean.mp3";');
  h.elements.get("podcast-form").listeners.get("submit")({ preventDefault() {} });
  for (let i = 0; i < 12; i++) await Promise.resolve();
  const run = calls.find(([name]) => name === "action_run");
  assert.equal(run[1].action, "clean_podcast_ads");
  assert.deepEqual(JSON.parse(run[1].inputs), { podcast_id: "genwhy", path: "/media/p/e.mp3", output_path: "/media/p/e.clean.mp3" });
  assert.equal(h.elements.get("podcast-dialog").open, false);
  assert.deepEqual(calls.at(-1), ["open", "podcast-job"]);
});

test("size goal discloses that lossless Atmos and DTS:X become AAC", () => {
  const radio = markup.match(/<label><input type="radio" name="processing-mode" value="size" \/>[\s\S]*?<\/label>/)?.[0] || "";
  assert.match(radio, /including lossless Atmos and DTS:X, which lose their object and height channels/);
  assert.match(radio, /Atmos on E-AC-3 included, are copied/);
  assert.doesNotMatch(radio, /Atmos and DTS:X tracks are copied/);
  assert.match(source, /lossless Atmos\/DTS:X lose object and height channels; lossy tracks \(E-AC-3 Atmos included\) copied/);
});

test("compact goal says in plain language what it drops and still needs approval", () => {
  const radio = markup.match(/<label><input type="radio" name="processing-mode" value="compact" \/>[\s\S]*?<\/label>/)?.[0] || "";
  assert.match(radio, /Make it much smaller · SDR 1080p/);
  assert.match(radio, /HDR and Dolby Vision are removed/);
  assert.match(radio, /Atmos on E-AC-3 is kept/);
  assert.doesNotMatch(radio, /Atmos are dropped/);
  assert.match(radio, /keeping every language and subtitle/);
  assert.match(radio, /replacing the original still needs your approval/);
  assert.match(markup, /<option value="compact">Make it much smaller · SDR 1080p/);
  assert.match(source, /compact:`Much smaller · SDR \$\{item\.max_height \|\| 1080\}p`/);
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
  assert.match(progressText(waiting), /Step 3\/8 · Verify original and prepare recovery · Waiting for a hash slot/);
  assert.equal(meterOf(waiting), undefined);

  for (const [phase, label] of [
    ["verifying_original", "Re-checking the original"],
    ["verifying_candidate", "Re-checking the converted file"],
    ["copying_recovery", "Copying the recovery copy"],
    ["verifying_recovery", "Verifying the recovery copy"],
  ]) {
    h.run(`promotionJob.work={phase:"${phase}",bytes_read:10e9,total_bytes:20e9};`);
    const element = h.run('telemetry(promotionJob)');
    assert.match(progressText(element), new RegExp(`Step 3/8 · Verify original and prepare recovery · ${label} · 50%.*10\\.00 GB / 20\\.00 GB`));
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
  assert.match(progressText(h.run('telemetry(promotionJob)')), /Step 5\/8 · Remove original library file · Copying the recovery copy · 25%.*5\.00 GB \/ 20\.00 GB/);
  h.run('promotionJob.work={phase:"waiting_for_hash_slot",total_bytes:20e9};');
  assert.match(progressText(h.run('telemetry(promotionJob)')), /Step 5\/8 · Remove original library file · Waiting for a hash slot/);
  // Its full read of the original is shown too, with a measured bar.
  h.run('promotionJob.work={phase:"verifying_original",bytes_read:5e9,total_bytes:20e9};');
  const reread = h.run('telemetry(promotionJob)');
  assert.match(progressText(reread), /Step 5\/8 · Remove original library file · Re-checking the original · 25%.*5\.00 GB \/ 20\.00 GB/);
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
  assert.equal(new Set(pairs.filter(([full]) => full.startsWith("Check")).map(([, v]) => v)).size, 2);
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
    Object.assign(this, {doc, tagName: tag.toUpperCase(), children: [], parent: null, dataset: {}, className: "", textContent: "", id: "", listeners: new Map()});
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
  const job = position => ({activities:[{id:"child",waiting_condition:"worker_busy",work:{phase:"waiting_for_worker",queue_position:position},worker:{benchmark_phase:"completed"}}]});
  h.context.waitingJob = job;
  assert.equal(h.run('batchFileActivity({status:"waiting_for_slot",child_action_id:"child"},waitingJob(3))'), "Waiting for a worker slot · 2 ahead in line");
  assert.equal(h.run('batchFileActivity({status:"waiting_for_slot",child_action_id:"child"},waitingJob(1))'), "Waiting for a worker slot · Next in line");
  assert.equal(h.run('batchFileActivity({status:"waiting_for_slot",child_action_id:"child"},waitingJob(undefined))'), "Waiting for a worker slot");
  assert.equal(h.run('workerPhaseLabel("waiting_for_worker")'), "Waiting for a worker slot");
  // Once the worker accepts the job the phase is gone and worker progress shows.
  assert.equal(h.run('batchFileActivity({status:"running",child_action_id:"child"},{activities:[{id:"child",worker:{benchmark_phase:"evaluating_metrics"}}]})'), "Measuring sample quality");
  // A standalone job says how many are ahead of it.
  h.context.job = {action_name:"benchmark_transcode",status:"waiting_external",waiting_condition:"worker_busy",work:{phase:"waiting_for_worker",queue_position:3}};
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
  assert.match(text(reviews[0]), /Smaller files/);
  assert.match(text(reviews[0]), /Originals stay in place/);
  assert.doesNotMatch(text(reviews[0]), /already had/);

  // Two earlier no-winner rounds make another one an explicit decision; replacing originals drops the line.
  const third = base.replace('no_winner_rounds:1', 'no_winner_rounds:2').replace('replace_original:false', 'replace_original:true');
  await h.run(`rerunJob(${third})`);
  assert.match(text(reviews[2]), /This file already had 2 searches without a result/);
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
