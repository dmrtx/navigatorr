const $ = (id) => document.getElementById(id);
const state = {
  tab: "library",
  media: null,
  files: new Map(),
  selected: new Set(),
  libraryOffset: 0,
  libraryRevision: 0,
  jobsLoaded: 0,
  recipe: null,
  recipes: [],
  recipeDetails: {},
  recipeOffset: 0,
  detail: null,
  chunk: 0,
  stepOffset: 0,
  batchItemsOffset: 0,
  jobsRevision: 0,
  detailRevision: 0,
  authRevision: 0,
  authMode: "token",
  busyJobs: new Set(),
  submitting: false,
  approvingBatch: false,
  fileStep: "browse",
};
const names = {
  queued: "Queued",
  waiting_for_slot: "Waiting for slot",
  skip: "Skipped",
  review: "Needs review",
  pending: "Queued",
  running: "Running",
  waiting_external: "In progress",
  waiting_decision: "Needs decision",
  completed: "Completed",
  partial: "Partial",
  failed: "Failed",
  cancelled: "Cancelled",
  rejected: "Rejected",
};
const workflows = {
 clean_podcast_ads:"Podcast cleaning",
  transcode_media: "File transcode",
  transcode_batch: "File batch",
  benchmark_transcode: "Benchmark",
  promote_transcode_candidate: "Replacement",
};
function node(tag, text = "", className = "") {
  const n = document.createElement(tag);
  n.textContent = text;
  n.className = className;
  return n;
}
function jobIsBusy(id) {
  return state.busyJobs.has(id) || [...(state.commands?.values() || [])].some(command=>command.id===id && !["completed","failed","cancelled"].includes(command.status));
}
function button(text, onClick, className = "") {
  const b = node("button", text, className);
  b.type = "button";
  b.addEventListener("click", () => safe(async () => {
    const result = onClick();
    if (!result?.then) return result;
    b.disabled = true;
    b.setAttribute("aria-busy", "true");
    b.classList.add("button-pending");
    try { return await result; }
    finally {
      b.removeAttribute("aria-busy");
      b.classList.remove("button-pending");
      b.disabled = !serverReachable || jobIsBusy(b.dataset.jobControl) || (b.dataset.requiresWorker === "true" && !state.workerInfo?.ready);
    }
  }));
  return b;
}
function actionIcon(label) {
  if (typeof document.createElementNS !== "function") return null;
  const paths = {
    Reconfigure: "M4 7h16M4 17h16M8 4v6M16 14v6",
    "Change settings": "M4 7h16M4 17h16M8 4v6M16 14v6",
    "Try another profile": "M4 7h16M4 17h16M8 4v6M16 14v6",
    Retry: "M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M18 18A8 8 0 0 1 4 12",
    Review: "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Review preview": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Start batch": "M7 4l13 8-13 8Z",
    Cancel: "M6 6l12 12M6 18L18 6",
    "Replace file": "M4 7h16M16 3l4 4-4 4M20 17H4M8 13l-4 4 4 4",
    "Review replacements": "M9 12l2 2 4-4M5 4h14v16H5Z",
    "Review replacement": "M9 12l2 2 4-4M5 4h14v16H5Z",
    "Review candidates": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Review candidate": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Set up benchmark": "M8 5h12v15H8ZM4 16V2h12",
    "Keep originals": "M4 4h16v4H4ZM6 8v12h12V8M10 12h4",
    Archive: "M4 4h16v4H4ZM6 8v12h12V8M10 12h4",
    Restore: "M12 19V9M8 13l4-4 4 4M4 4h16v4H4Z",
    "View replacement": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "View batch": "M8 5h12v15H8ZM4 16V2h12",
    "View preview": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Accept quality loss": "M12 3 2 21h20ZM12 9v5M12 17v1",
    "Pause batch": "M8 4v16M16 4v16",
    "Resume batch": "M7 4l13 8-13 8Z",
    "Restore built-in profile": "M4 4v5h5M4 9a8 8 0 1 1 1 9",
    "Delete custom profile":
      "M3 6h18M9 6V3h6v3M6 6l1 15h10l1-15M10 10v7M14 10v7",
    Folder: "M3 6h7l2 2h9v12H3Z",
  };
  if (!paths[label]) return null;
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  for (const [name, value] of Object.entries({
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    "stroke-width": "1.7",
    "stroke-linecap": "round",
    "stroke-linejoin": "round",
    "aria-hidden": "true",
  }))
    svg.setAttribute(name, value);
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", paths[label]);
  svg.append(path);
  return svg;
}

function bytes(n) {
  if (n == null) return "—";
  const v = Number(n);
  if (!Number.isFinite(v)) return "—";
  if (v >= 1e9) return `${(v / 1e9).toFixed(2)} GB`;
  if (v >= 1e6) return `${(v / 1e6).toFixed(1)} MB`;
  if (v >= 1e3) return `${(v / 1e3).toFixed(1)} KB`;
  return `${Math.round(v)} B`;
}
function notify(text) {
  const notice = $("notice");
  const dialog = document.querySelector?.("dialog[open]");
  (dialog || document.querySelector?.("main"))?.append(notice);
  notice.className = dialog ? "dialog-notice" : "";
  notice.textContent = text;
  notice.hidden = false;
  clearTimeout(notify.timer);
  notify.timer = setTimeout(() => {
    $("notice").hidden = true;
  }, 9000);
}
document.querySelectorAll("dialog").forEach((dialog) => {
  dialog.addEventListener("close", () => {
    if (dialog.contains($("notice"))) {
      document.querySelector?.("main")?.append($("notice"));
      $("notice").className = "";
    }
  });
});
async function safe(fn) {
  try {
    return await fn();
  } catch (e) {
    if (serverReachable && e.message !== "sign in to Navigatorr")
      notify(e.message);
  }
}
let serverReachable =
  typeof navigator === "undefined" || navigator.onLine !== false;
function setConnection(online) {
  serverReachable = online;
  $("connection-status").hidden = online;
  document.body?.classList.toggle("disconnected", !online);
  document.querySelectorAll("[data-job-control]").forEach((button) => {
    button.disabled = !online || jobIsBusy(button.dataset.jobControl) ||
      (button.dataset.requiresWorker === "true" && !state.workerInfo?.ready);
  });
  document.querySelectorAll(".dialog-connection").forEach((notice) => {
    notice.hidden = online;
  });
  $("approve-batch-review").disabled = !online || state.approvingBatch;
  $("reject-batch-review").disabled = !online || state.approvingBatch;
  $("approve-file-review").disabled = !online || state.approvingFile;
  $("confirm-action-review").disabled = !online;
  document.querySelectorAll("[data-review-choice]").forEach(b => { b.disabled = !online; });
  // Keep the last observation for age/diagnostics; disconnected state makes
  // its verdict unknown and blocks writes without implying remote jobs stopped.
  renderWorkers();
  controls();
}
async function reconnect() {
  await api("bootstrap");
  if ($("workspace").hidden) await initialize();
  else {
    resumeCommandTracking();
    await loadJobs();
    if (state.tab === "library" && state.libraryError) await loadLibrary();
    if ($("job-detail").open) await refreshDetail();
  }
}
if (typeof window !== "undefined") {
  window.addEventListener("offline", () => setConnection(false));
  window.addEventListener("online", () => safe(reconnect));
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) safe(reconnect);
  });
}
function invalidateAuthentication() {
  finishActionReview(false);
  state.authRevision++;
  state.backupRevision = (state.backupRevision || 0) + 1;
  state.backupCleaning = null;
  state.backupsLoading = false;
  state.backupsVisible = false;
  state.backups = [];
  $("backup-list").replaceChildren();
  $("backup-summary").hidden = true;
  $("backup-progress").hidden = true;
  state.recipeDetails = {};
  state.workerInfo = null;
  state.activitySlotIDs = null;
  state.workerCheckedAt = 0;
  $("worker-status").hidden = true;
  $("workers-dialog").close();
  $("benchmark-comparison").close();
  state.comparisonRevision = (state.comparisonRevision || 0) + 1;
  $("comparison-frame").replaceChildren();
  clearTimeout(state.folderSizesTimer);
  state.libraryRevision++;
  state.jobsRevision++;
  state.detailRevision++;
  state.detail = null;
  state.batchApproval = null;
  state.replacementChoice = null;
  $("job-detail").close();
  $("batch-review").close();
  $("file-review").close();
  state.reconfigureRevision = (state.reconfigureRevision || 0) + 1;
  state.reconfigurePlan = null;
  $("reconfigure-batch").close();
  state.fileApproval = null;
  $("path-dialog").close();
  $("recipe-settings").close();
  $("replacement-choice").close();
  $("login").hidden = state.authMode !== "token";
  $("access-expired").hidden = state.authMode !== "cloudflare_access";
  $("workspace").hidden = true;
  $("logout").hidden = true;
  $("local-session").hidden = true;
}
function authenticationMessage() {
  return state.authMode === "cloudflare_access"
    ? "Cloudflare Access sign-in required. Reload to sign in."
    : "sign in to Navigatorr";
}
async function api(path, body) {
  if (body !== undefined && !serverReachable)
    throw new Error(
      "Offline. Changes are unavailable until the server responds.",
    );
  let res;
  try {
    res = await fetch(`/api/maintenance/${path}`, {
      method: body === undefined ? "GET" : "POST",
      redirect: "manual",
      headers: {
        "Content-Type": "application/json",
        "X-Navigatorr-Request": "1",
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    setConnection(false);
    throw new Error("Cannot connect to Navigatorr. Retrying automatically.");
  }
  if ([502, 503, 504].includes(res.status)) {
    setConnection(false);
    throw new Error("Navigatorr is unavailable. Retrying automatically.");
  }
  if (!serverReachable) setConnection(true);
  const redirected =
    res.type === "opaqueredirect" ||
    res.redirected === true ||
    (res.status >= 300 && res.status < 400);
  if (redirected) {
    // Access can intercept auth-info before the console knows its auth mode.
    // Never follow its login chain inside an API request or parse login HTML.
    if (state.authMode === "cloudflare_access" || path === "auth-info") {
      state.authMode = "cloudflare_access";
      invalidateAuthentication();
      throw new Error(authenticationMessage());
    }
    throw new Error("Unexpected API redirect. Reload the application.");
  }
  const contentType = res.headers?.get("Content-Type") || "";
  if (
    state.authMode === "cloudflare_access" &&
    res.status === 403 &&
    /text\/html/i.test(contentType)
  ) {
    invalidateAuthentication();
    throw new Error(authenticationMessage());
  }
  if (res.status === 401) {
    invalidateAuthentication();
    // Invalidate before parsing: an HTML denial must close stale approvals too.
    let denial;
    try {
      denial = await res.json();
    } catch {}
    throw new Error(denial?.error || authenticationMessage());
  }
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}
async function tool(name, args = {}) {
  const mutates = ["action_run","action_resume","action_retry","action_cancel"].includes(name) || name === "transcode_backups" && ["clean","discard_duplicate","discard"].includes(args.mode);
  const result = mutates
    ? await commandRequest("tool", {name,arguments:args,background:true})
    : await api("tool", {name,arguments:args});
  if (!result.content) return result;
  const text = result.content
    .filter((c) => c.type === "text")
    .map((c) => c.text)
    .join("\n");
  if (result.isError) throw new Error(text);
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

const commandLabels = {prepare_batch_promotion:"Preparing replacement review",review_batch_promotion:"Applying replacement decision",resume:"Applying decision",retry:"Retrying job",cancel:"Stopping job",candidate:"Applying candidate decision",clean:"Verifying recovery data",discard_duplicate:"Verifying duplicate",discard:"Removing recovery copy",reconfigure:"Updating batch settings"};
function readCommandReceipts() {
  try { return JSON.parse(sessionStorage.getItem("navigatorr_commands") || "{}"); } catch { return {}; }
}
function commandTarget(body) { return body.id || body.arguments?.id || body.arguments?.action_id; }
function trackCommand(request, receipt, result) {
  state.commands ||= new Map();
  const info = {request,...receipt,...result,id:result.id || receipt.id,status:result.status || "pending"};
  state.commands.set(receipt.command_id, info);
  renderCommandIndicators();
  document.querySelectorAll("[data-job-control]").forEach(button => {
    if (button.dataset.jobControl === info.id) button.disabled = !serverReachable || jobIsBusy(info.id) || (button.dataset.requiresWorker === "true" && !state.workerInfo?.ready);
  });
  return info;
}
function renderCommandIndicators() {
  document.querySelectorAll(".job-command").forEach(el=>el.remove());
  for (const command of state.commands?.values() || []) {
    const status = {pending:"Request accepted · Waiting to apply",running:"Applying",waiting_external:"Waiting to apply",completed:"Applied",failed:"Could not apply",cancelled:"Cancelled"}[command.status] || "Applying";
    const label = commandLabels[command.kind] || command.label || "Update";
    const text = `${label} · ${status}${command.error ? ` · ${shortJobReason(command.error)}` : command.waiting_reason ? ` · ${command.waiting_reason}` : ""}`;
    const targets = [...document.querySelectorAll("[data-operation-id]")].filter(el=>el.dataset.operationId === command.id);
    for (const target of targets) {
      const line = node("p", text, `job-command${command.status === "failed" ? " failed" : ""}`);
      line.setAttribute("role","status"); target.append(line);
    }
  }
}
const commandPolls = new Map();
function waitCommand(request, receipt) {
  if (commandPolls.has(receipt.command_id)) return commandPolls.get(receipt.command_id);
  const auth = state.authRevision;
  const pending = (async () => {
    while (auth === state.authRevision && !$("workspace").hidden) {
      const result = await api(`commands?id=${encodeURIComponent(receipt.command_id)}`);
      trackCommand(request,receipt,result);
      if (["completed","failed","cancelled"].includes(result.status)) {
        const latest = readCommandReceipts(); delete latest[request];
        sessionStorage.setItem("navigatorr_commands",JSON.stringify(latest));
        if (state.backupCleaning?.command_id === receipt.command_id) {
          state.backupCleaning.pendingResponse = false;
          state.backupCleaning.commandCompleted = result.status === "completed";
          state.backupCleaning.error = result.error;
          await safe(refreshBackupCleanup);
        }
        await safe(loadJobs);
        if (state.detail === result.id && $("job-detail").open) await safe(refreshDetail);
        if (result.status !== "completed") throw new Error(shortJobReason(result.error) || "The action could not be applied.");
        return {id:result.id,command_id:result.command_id};
      }
      await new Promise(resolve => setTimeout(resolve,1000));
    }
    throw new Error("Session ended. The saved action continues on the server.");
  })().finally(()=>commandPolls.delete(receipt.command_id));
  commandPolls.set(receipt.command_id,pending);
  return pending;
}
function resumeCommandTracking() {
  for (const [request,receipt] of Object.entries(readCommandReceipts())) {
    if (receipt.command_id && !commandPolls.has(receipt.command_id)) void safe(()=>waitCommand(request,receipt));
  }
}
function restoreCommands() {
  for (const [request,receipt] of Object.entries(readCommandReceipts())) {
    if (!receipt.command_id) continue;
    trackCommand(request,receipt,{status:"pending"});
    if (receipt.cleanup && !state.backupCleaning) state.backupCleaning = {...receipt.cleanup,auth:state.authRevision,command_id:receipt.command_id,pendingResponse:true};
    void safe(()=>waitCommand(request,receipt));
  }
  renderBackups(); renderBackupCleanup();
}
async function commandRequest(path, body, onAccepted) {
  const request = JSON.stringify([path,body]);
  const receipts = readCommandReceipts();
  const receipt = receipts[request] ||= {key:submissionID(),started:Date.now()};
  sessionStorage.setItem("navigatorr_commands",JSON.stringify(receipts));
  const result = receipt.command_id ? {command_id:receipt.command_id,id:receipt.id} : await api(path,{...body,key:receipt.key});
  if (!result.command_id) { delete receipts[request]; sessionStorage.setItem("navigatorr_commands",JSON.stringify(receipts)); return result; }
  receipt.command_id = result.command_id; receipt.id = result.id || commandTarget(body);
  receipt.label = commandLabels[body.kind || body.arguments?.mode || body.name?.replace("action_","")] || "Update";
  if (state.backupCleaning?.id === receipt.id) {
    const task = state.backupCleaning;
    receipt.cleanup = {id:task.id,name:task.name,mode:task.mode,started:task.started};
    task.command_id = receipt.command_id;
  }
  const latest = readCommandReceipts(); latest[request] = receipt;
  sessionStorage.setItem("navigatorr_commands",JSON.stringify(latest));
  trackCommand(request,receipt,{status:"pending"});
  onAccepted?.();
  notify("Request accepted. You can keep browsing; its status is shown with the job.");
  return waitCommand(request,receipt);
}

function showData(id, data) {
  $(id).hidden = false;
  $(id).textContent =
    typeof data === "string" ? data : JSON.stringify(data, null, 2);
}
function submissionID() {
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 15) | 64;
  b[8] = (b[8] & 63) | 128;
  const h = [...b].map((v) => v.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
function option(select, value, text = value) {
  const o = node("option", text);
  o.value = value;
  select.append(o);
  return o;
}
const routeTabs = {files:"library",queue:"jobs",profiles:"recipes",stats:"stats",settings:"advanced",more:"advanced"};
const routeStatuses = ["all","pending","running","waiting_external","waiting_decision","completed","failed","cancelled","archived"];
function navigationRoute(search = "") {
  const q = new URLSearchParams(search);
  return {
    tab:routeTabs[q.get("view")] || "library", explicit:q.has("view"),
    source:q.get("source") || "", folder:q.get("folder") || "",
    media:/^[1-9]\d*$/.test(q.get("media") || "") ? Number(q.get("media")) : null,
    file:q.get("file") || "", search:(q.get("q") || "").slice(0,256),
    sort:["name","name_desc","size_asc","size_desc"].includes(q.get("sort")) ? q.get("sort") : "name",
    status:routeStatuses.includes(q.get("status")) ? q.get("status") : "all",
    job:(q.get("job") || "").slice(0,200), profile:(q.get("profile") || "").slice(0,128),
    copies:q.get("copies") === "1", configure:q.get("configure") === "1",
    mode:["size","quality","x265_preserve"].includes(q.get("mode")) ? q.get("mode") : "quality", technical:q.get("technical") === "1",
  };
}
function navigationQuery() {
  const q = new URLSearchParams({view:Object.entries(routeTabs).find(([,tab])=>tab===state.tab)?.[0] || "files"});
  if (processingMode() !== "quality") q.set("mode", processingMode());
  if ($("technical").checked) q.set("technical", "1");
  if ($("service").value) q.set("source",$("service").value);
  if ($("service").value.startsWith("folder:") && state.folder) q.set("folder",state.folder);
  if (state.media?.id) q.set("media",state.media.id);
  if (state.fileStep === "configure") q.set("configure","1");
  if (state.fileStep === "configure" && $("scope").value === "file" && state.file?.path) q.set("file",state.file.path);
  if ($("search").value) q.set("q",$("search").value);
  if ($("library-sort").value && $("library-sort").value !== "name") q.set("sort",$("library-sort").value);
  if ($("job-filter").value && $("job-filter").value !== "all") q.set("status",$("job-filter").value);
  if ($("job-detail").open && state.detail) q.set("job",state.detail);
  if (state.tab === "recipes" && state.recipe?.name) q.set("profile",state.recipe.name);
  if (state.tab === "advanced" && state.backupsVisible) q.set("copies","1");
  return q.toString();
}
function writeNavigation(replace = false) {
  if (typeof window === "undefined" || !window.history || !state.routeReady || state.applyingNavigation || $("workspace").hidden) return;
  const url = `${window.location.pathname}?${navigationQuery()}`;
  if (url === window.location.pathname + window.location.search) return;
  window.history[replace ? "replaceState" : "pushState"]({navigatorr:true},"",url);
}
async function applyNavigation(route) {
  if (route.tab === "recipes" && route.profile !== state.recipe?.name && recipeIsDirty() && !await confirmRecipeDiscard()) { writeNavigation(true); return; }
  if (route.tab === "recipes" && route.profile !== state.recipe?.name) state.recipeBaseline = null;
  const revision = state.routeRevision = (state.routeRevision || 0) + 1;
  const auth = state.authRevision;
  const current = () => revision === state.routeRevision && auth === state.authRevision && !$("workspace").hidden;
  state.applyingNavigation = true;
  try {
    state.detailRevision++;
    if ($("job-detail").open) $("job-detail").close();
    if (!route.copies) {
      state.backupRevision = (state.backupRevision || 0) + 1;
      state.backupsLoading = false;
      state.backupsVisible = false;
      $("load-backups").disabled = false;
      $("backup-list").hidden = true;
      $("backup-summary").hidden = true;
      $("backup-progress").hidden = true;
    }
    if (route.tab === "recipes" && !route.profile) resetRecipeView();
    $("technical").checked = Boolean(route.technical);
    setProcessingMode(route.mode || "quality");
    const sources = [...state.info.roots.map(r=>`folder:${r}`), ...state.info.services.map(s=>s.name)];
    $("service").value = sources.includes(route.source) ? route.source : sources[0] || "";
    resetLibrary();
    $("library-sort").value = route.sort;
    renderLibrarySort();
    $("job-filter").value = route.status;
    $("search").value = route.search;
    $("library-search-label").hidden = !route.search;
    $("search-button").hidden = !route.search;
    $("toggle-library-search").setAttribute("aria-expanded",String(Boolean(route.search)));
    $("toggle-library-search").setAttribute("aria-label",route.search ? "Close search" : "Show search");
    $("toggle-library-search").classList?.toggle("search-open",Boolean(route.search));
    if ($("service").value.startsWith("folder:")) state.folder = route.folder || $("service").value.slice(7);
    else if (route.media && $("service").value) {
      try {
        const result = await api(`library?${new URLSearchParams({service:$("service").value,id:route.media,title:"1"})}`);
        if (!current()) return;
        if (result.media?.id !== route.media) throw new Error("This library title is unavailable.");
        state.media = result.media;
        $("selection-title").textContent = result.media.title || `Title ${route.media}`;
        $("back").hidden = false;
        $("scope").value = $("service").value === "sonarr" ? "batch" : "file";
      } catch (error) {
        if (!current()) return;
        notify(`${error.message} Choose a title from the library.`);
      }
    }
    const tab = !route.explicit && typeof matchMedia === "function" && matchMedia("(max-width:600px)").matches ? "jobs" : route.tab;
    selectTab(tab, true);
    state.libraryNeedsLoad = true;
    if (tab === "library" && $("service").value) {
      await loadLibrary();
      if (!current()) return;
      if (route.configure && !route.file && (state.folderSelected?.size || state.selected.size)) configureSelection(true,true);
      if (route.file) {
        const folder = $("service").value.startsWith("folder:");
        const page = await api(`${folder ? "folder" : "library"}?${new URLSearchParams(folder ? {path:state.folder,q:route.file.split("/").pop(),limit:100} : {service:$("service").value,id:state.media?.id || "",q:route.file.split("/").pop(),limit:100})}`);
        if (!current()) return;
        const file = page.items?.find(f=>(f.path || "")===route.file && !f.is_dir);
        if (file) {
          state.file = file; state.fileMedia = state.media; state.fileService = folder ? null : $("service").value;
          $("path").value = file.path; $("scope").value = "file"; setFileStep("configure"); controls();
        } else notify("The selected file is unavailable. Showing its folder or title.");
      }
    } else if (tab === "library") $("library-items").replaceChildren(node("p", "No media folders configured.", "empty"));
    if (!current()) return;
    setProcessingMode(route.mode || "quality");
    $("technical").checked = Boolean(route.technical);
    controls();
    if (tab === "recipes" && route.profile && !(recipeIsDirty() && route.profile === state.recipe?.name)) await readRecipe(route.profile);
    if (tab === "advanced" && route.copies) await loadBackups();
    if (tab === "jobs" || tab === "stats") await loadJobs();
    if (current() && route.job) await openJob(route.job);
  } finally {
    if (revision === state.routeRevision) { state.applyingNavigation=false; writeNavigation(true); }
  }
}
if (typeof window !== "undefined") window.addEventListener("popstate", () => {
  if (state.routeReady && !$("workspace").hidden) safe(() => applyNavigation(navigationRoute(window.location.search)));
});
function selectTab(tab, fromRoute = false) {
  if (!fromRoute && state.applyingNavigation) { state.routeRevision++; state.applyingNavigation=false; }
  state.tab = tab;
  document
    .querySelectorAll(".tab-content")
    .forEach((n) => (n.hidden = n.id !== tab));
  document
    .querySelectorAll("[data-tab]")
    .forEach((n) =>
      n.setAttribute("aria-current", n.dataset.tab === tab ? "page" : "false"),
    );

  if (typeof window !== "undefined") window.scrollTo({ top: 0 });
  writeNavigation();
  if (["jobs", "stats"].includes(tab) && !fromRoute) safe(loadJobs);
  if (tab === "library" && state.libraryNeedsLoad && !fromRoute) safe(loadLibrary);
  if (tab === "library" && $("service").value.startsWith("folder:") && state.folderSizeTargets?.size) {
    clearTimeout(state.folderSizesTimer);
    void updateFolderSizes(state.libraryRevision, state.folder, $("search").value, state.libraryLoaded);
  }
  if (tab === "recipes" && !fromRoute)
    safe(async () => {
      const route = state.routeRevision, auth = state.authRevision;
      await loadRecipes();
      if (state.tab === "recipes" && route === state.routeRevision && auth === state.authRevision && !state.recipe && state.recipes.length)
        await readRecipe(state.recipes[0], true);
    });
}
document
  .querySelectorAll("[data-tab]")
  .forEach((n) => n.addEventListener("click", () => selectTab(n.dataset.tab)));
$("login-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(async () => {
    await api("login", { token: $("token").value });
    $("token").value = "";
    await initialize();
  });
});
$("logout").addEventListener("click", () =>
  safe(async () => {
    await api("logout", {});
    $("workspace").hidden = true;
    $("login").hidden = false;
    $("logout").hidden = true;
  }),
);

function workerObservationFresh(worker) {
  const observed = Date.parse(worker.observed_at);
  const until = Date.parse(worker.fresh_until) || observed + 15000;
  return serverReachable && !worker.stale && Number.isFinite(observed) && observed <= Date.now() && Date.now() < until;
}
function schedulerSummary(info = state.workerInfo) {
  if (!serverReachable || !info) return "Scheduler · Unknown";
  const nodes = info.nodes || [];
  const ages = nodes.map(w => Date.parse(w.observed_at)).filter(Number.isFinite).map(at => Math.max(0, Math.floor((Date.now()-at)/1000)));
  const age = ages.length ? ` · ${Math.max(...ages)}s old` : "";
  const fresh = nodes.filter(w => workerObservationFresh(w) && ["ok", "degraded"].includes(w.scheduler_health));
  if (!fresh.length || fresh.length !== nodes.length) return "Scheduler · Unknown" + age;
  return (fresh.some(w => w.scheduler_health === "degraded") ? "Scheduler · Degraded" : "Scheduler · Normal") + age;
}
function renderWorkerDetails(host, info) {
  host.replaceChildren();
  for (const worker of info?.nodes || []) {
    const fresh = workerObservationFresh(worker);
    const row = node("div", "", "worker-node");
    row.append(node("strong", worker.name), node("span", !serverReachable ? "Unknown" : worker.ready ? "Reachable · Ready" : worker.reachable || worker.connected ? "Reachable · Blocked" : "Unavailable", "badge"));
    row.append(node("p", `Scheduler: ${fresh ? worker.scheduler_health || "unknown" : "unknown (outdated observation)"}`, "metadata"));
    row.append(node("p", `Last successful sweep: ${worker.last_success_at ? new Date(worker.last_success_at).toLocaleString() : "Unknown"}`, "metadata"));
    row.append(node("p", `Observation: ${worker.observed_at ? new Date(worker.observed_at).toLocaleString() : "Unknown"}${Number.isFinite(Date.parse(worker.observed_at)) ? ` · ${Math.max(0,Math.floor((Date.now()-Date.parse(worker.observed_at))/1000))}s old` : ""}`, "metadata"));
    for (const [name, sweep] of Object.entries(worker.sweeps || {})) row.append(node("p", `${name === "post_encode" ? "Post-encode" : "Queue"} sweep: ${fresh && sweep.observed_at ? sweep.last_error || sweep.consecutive_errors > 0 ? "degraded" : "normal" : "unknown"}${sweep.consecutive_errors ? ` · ${sweep.consecutive_errors} consecutive errors` : ""}`, "metadata"));
    if (worker.last_error?.message) row.append(node("p", `${fresh ? "Last error" : "Previous error"}: ${worker.last_error.class || "scheduler"} · ${worker.last_error.message}`, "muted"));
    host.append(row);
  }
  if (!info) host.append(node("p", serverReachable ? "Checking shared worker observation…" : "Server disconnected; worker and scheduler status are unknown. Jobs may continue remotely.", "muted"));
}
function renderWorkers() {
  const info = state.workerInfo;
  document.querySelectorAll('[data-job-control][data-requires-worker="true"]').forEach(button => {
    button.disabled = !serverReachable || !info?.ready || jobIsBusy(button.dataset.jobControl);
  });
  $("worker-status").hidden = !state.info || $("workspace").hidden;
  $("worker-status").textContent = !serverReachable ? "Worker · Unknown"
    : !info ? "Worker · Checking…" : info.ready ? "Worker · Connected" : info.nodes?.some(node=>node.connected) ? "Worker · Blocked" : "Worker · Unavailable";
  $("worker-status").dataset.ready = String(Boolean(info?.ready && serverReachable));
  $("worker-submit-status").textContent = !serverReachable ? "Server disconnected. New jobs are blocked."
    : !info ? "Checking video worker before submitting…"
    : info.ready ? "Video worker connected. Connection is checked again before submitting."
    : info.nodes?.[0]?.message || "Video worker offline or not ready. New jobs are blocked.";
  renderWorkerDetails($("worker-nodes"), info);
  renderWorkerDetails($("settings-worker"), info);
  $("activity-scheduler").textContent = schedulerSummary(info);
  $("worker-status").textContent += ` · ${schedulerSummary(info).replace("Scheduler · ", "Scheduler ")}`;
  $("workers-checked").textContent = info?.checked_at ? `Checked ${new Date(info.checked_at).toLocaleTimeString()}` : "";
}
async function refreshWorkers() {
  if (state.workerRequest) return state.workerRequest;
  const auth = state.authRevision;
  state.workerRequest = (async () => {
    try {
      const info = await api("workers");
      if (auth !== state.authRevision || $("workspace").hidden) return;
      state.workerInfo = info;
      state.workerCheckedAt = Date.now();
    } catch {
      if (auth === state.authRevision) state.workerInfo = null;
    } finally {
      state.workerRequest = null;
      renderWorkers();
      controls();
    }
  })();
  return state.workerRequest;
}
$("worker-status").addEventListener("click", () => {
  $("workers-dialog").showModal();
  void refreshWorkers();
});
$("close-workers").addEventListener("click", () => $("workers-dialog").close());
$("refresh-workers").addEventListener("click", () => void refreshWorkers());
async function initialize() {
  state.routeReady=false;
  const auth = await api("auth-info");
  state.authMode = auth.auth_mode;
  $("logout").hidden = state.authMode !== "token";
  $("local-session").hidden = state.authMode !== "token";
  const info = await api("bootstrap");
  state.info = info;
  $("login").hidden = true;
  $("workspace").hidden = false;
  $("worker-status").hidden = false;
  void refreshWorkers();
  $("logout").hidden = state.authMode !== "token";
  $("access-expired").hidden = true;
  $("service").replaceChildren();
  info.roots.forEach(
    (r) =>
      (option($("service"), `folder:${r}`, `Folder · ${r.split("/").pop() || r}`).title = r),
  );
  info.services.forEach((s) =>
    option($("service"), s.name, s.name === "radarr" ? "Radarr · Movies" : "Sonarr · TV"),
  );
  $("root").replaceChildren();
  info.roots.forEach((r) => {
    option($("root"), r, r.split("/").pop() || r).title = r;
  });

  document.querySelector('[data-tab="recipes"]').disabled =
    !info.tools.includes("recipe_list");
  $("promote-batch").disabled = !info.allow_destructive;
  $("enqueue").disabled = !info.transcode_enabled;
  if (!info.transcode_enabled)
    notify("Transcoding is disabled. You can still browse files and jobs.");
  if (info.tools.includes("recipe_list")) await loadRecipes(true);
  try {
    await applyNavigation(navigationRoute(typeof window !== "undefined" ? window.location.search : ""));
  } finally {
    if (!$("workspace").hidden) {
      state.routeReady=true;
      writeNavigation(true);
    }
  }
  restoreCommands();
  controls();
  if (!["jobs","stats"].includes(state.tab)) await loadJobs();
}

async function loadLibrary(more = false) {
  restoreLibraryDraft();
  const revision = ++state.libraryRevision;
  state.libraryLoading = true;
  if (!more) {
    state.libraryHasMore = false;
    $("library-more").hidden = true;
    $("library-items").replaceChildren(node("p", "Loading files…", "muted"));
  }
  controls();
  try {
    if (!more) state.libraryOffset = 0;
    else state.libraryOffset = state.libraryLoaded || 0;

    if ($("service").value.startsWith("folder:"))
      return await loadFolder(revision, more);
    const query = new URLSearchParams({
      service: $("service").value,
      q: $("search").value,
      offset: state.libraryOffset,
      limit: 100,
      sort: $("library-sort").value || "name",
    });
    if (state.media) query.set("id", state.media.id);
    const page = await api(`library?${query}`);
    if (revision !== state.libraryRevision) return;
    $("library-total").textContent =
      `${page.total} ${state.media ? "files" : "titles"}`;
    if (!more) $("library-items").replaceChildren();
    for (const item of page.items) {
      const row = node("div", "", "media-row");
      if (state.media) {
        const check = document.createElement("input");
        check.type = "checkbox";
        check.dataset.selection = String(item.id);
        check.checked = state.selected.has(item.id);
        check.setAttribute(
          "aria-label",
          `Select ${item.relativePath || item.path}`,
        );
        check.addEventListener("change", () => {
          check.checked
            ? state.selected.add(item.id)
            : state.selected.delete(item.id);
          fileSelectionChanged();
        });
        const checkLabel = node("label", "", "media-check");
        checkLabel.append(check);
        row.append(checkLabel);
        state.files.set(item.id, item);
      } else
        row.append(
          node(
            "span",
            $("service").value === "radarr" ? "▰" : "▤",
            "media-icon",
          ),
        );
      const title =
        item.title || item.relativePath || item.path || `File ${item.id}`;
      const b = button(
        "",
        async () => {
          if (state.media) {
            selectOneFile(item.id);
            $("path").value = item.path || "";
            state.file = item;
            state.fileService = $("service").value;
            state.fileMedia = state.media;
            $("scope").value = "file";
            controls();
            setFileStep("configure");
            writeNavigation();
            return;
          }
          saveLibraryDraft();
          state.draftKey = null;
          state.draftSeason = null;
          $("selected-only").checked = false;
          state.media = item;
          state.libraryOffset = 0;
          state.selected.clear();
          state.files.clear();
          $("path").value = "";
          state.file = null;
          state.fileMedia = null;
          state.fileService = null;
          $("selection-title").textContent = item.title;
          $("back").hidden = false;
          $("scope").value = $("service").value === "sonarr" ? "batch" : "file";
          await loadLibrary();
          controls();
        },
        "open-media",
      );
      b.append(node("span", title, "media-title"));
      b.append(
        node(
          "span",
          [
            item.year,
            item.mediaInfo?.videoCodec,
            item.seasonNumber != null ? `Season ${item.seasonNumber}` : null,
            item.episodeCount != null ? `${item.episodeCount} files` : null,
          ]
            .filter((v) => v != null)
            .join(" · "),
          "metadata",
        ),
      );
      row.append(b, node("span", bytes(item.size), "media-size"));
      $("library-items").append(row);
    }
    if (!page.items.length)
      $("library-items").append(
        node("p", "No matching files or titles.", "empty"),
      );
    state.libraryLoaded = state.libraryOffset + page.items.length;
    state.libraryHasMore = page.has_more;
    state.libraryError = false;
    state.libraryNeedsLoad = false;
    if (!more) writeNavigation();
    $("library-more").hidden = !page.has_more;
    if (state.media && $("service").value === "sonarr") {
      const selectedSeason = state.draftSeason ?? $("season").value;
      const seasons = page.seasons || [];
      $("season").replaceChildren();
      option($("season"), "", "All seasons");
      seasons.forEach(({seasonNumber:n,fileCount:count}) => option($("season"), n, `${n === 0 ? "Specials" : `Season ${n}`} · ${count} file${count === 1 ? "" : "s"}`));
      $("season").value = seasons.some(s => String(s.seasonNumber) === selectedSeason) ? selectedSeason : "";
      state.draftSeason = null;
    }
  } catch (error) {
    if (
      revision === state.libraryRevision &&
      !$("service").value.startsWith("folder:")
    )
      libraryFailure(error, more);
    throw error;
  } finally {
    if (revision === state.libraryRevision) {
      state.libraryLoading = false;
      controls();
    }
  }
}
function libraryFailure(error, more) {
  state.libraryError = true;
  const host = $("library-items");
  if (!more) host.replaceChildren();
  host.append(
    node("p", error.message || "Could not load files.", "muted"),
    button("Retry", () => loadLibrary(more), "quiet"),
  );
}
const draftFields = ["technical","scope","path","season","recursive","selected-only","promote-batch","profile","priority","custom","encoder","quality","preset","tune","rate-mode","bitrate","max-bitrate","audio","optimize","ladder","metric","vmaf-target","vmaf-min","ssim-target","ssim-min","sample-count","sample-seconds","final-validation","max-items","preserve-depth","min-savings","max-growth","media-kind"];
function libraryDraftKey() {
  const source = $("service").value;
  return source.startsWith("folder:") ? source : `${source}:${state.media?.id || "catalog"}`;
}
function saveLibraryDraft() {
  if (!state.draftKey || typeof sessionStorage === "undefined" || $("workspace").hidden) return;
  const fields = {};
  for (const id of draftFields) if ($(id)) fields[id] = {value:id === "season" ? state.draftSeason ?? $(id).value : $(id).value,checked:$(id).checked};
  const draft = {fields,mode:processingMode(),folderSelected:[...(state.folderSelected || [])],selected:[...state.selected],files:[...state.files].filter(([id])=>state.selected.has(id))};
  try { sessionStorage.setItem(`navigatorr_draft:${state.draftKey}`,JSON.stringify(draft)); } catch {}
}
function restoreLibraryDraft() {
  const key = libraryDraftKey();
  if (key === state.draftKey) return;
  saveLibraryDraft();
  state.draftKey = key;
  let draft;
  try { draft = JSON.parse(sessionStorage.getItem(`navigatorr_draft:${key}`)); } catch {}
  if (!draft) return;
  setProcessingMode(draft.mode || "quality");
  const root = $("service").value.slice(7).replace(/\/$/, "");
  state.folderSelected = new Set((draft.folderSelected || []).filter(path=>typeof path === "string" && path.startsWith(root+"/")));
  state.selected = new Set(draft.selected || []);
  for (const [id,file] of draft.files || []) state.files.set(id,file);
  for (const [id,field] of Object.entries(draft.fields || {})) if (draftFields.includes(id) && $(id)) {
    if (id === "profile" && field.value !== "auto" && !state.recipes.includes(field.value)) continue;
    if (id === "season") state.draftSeason = field.value;
    $(id).value = field.value; $(id).checked = field.checked;
  }
}
$("job-form").addEventListener("change", () => { renderCreationProfile(); saveLibraryDraft(); writeNavigation(true); });
$("job-form").addEventListener("input", () => { renderCreationProfile(); saveLibraryDraft(); });
function resetLibrary() {
  saveLibraryDraft();
  state.draftKey = null;
  state.draftSeason = null;
  $("selected-only").checked = false;
  state.allSelectionKeys=null;
  state.folderSelectionMembers=new Map();
  setFileStep("browse");
  state.libraryRevision++;
  state.libraryLoaded = 0;
  state.libraryHasMore = false;
  state.folderListing = null;
  state.folderHeaderKeys = null;
  $("season").value = "";
  $("size-order-note").hidden = true;
  $("library-more").hidden = true;
  state.media = null;
  state.folder = null;
  state.folderSelected = new Set();
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
  state.libraryOffset = 0;
  state.files.clear();
  state.selected.clear();
  $("path").value = "";
  $("back").hidden = true;
  $("selection-title").textContent = "Choose a movie or series";
  $("selection-title").hidden = false;
  $("folder-breadcrumbs").hidden = true;
  $("scope").value = "file";
  $("library-items").replaceChildren();
  controls();
}
$("service").addEventListener("change", () => {
  resetLibrary();
  safe(loadLibrary);
});
$("library-sort").addEventListener("change", () => safe(loadLibrary));
function renderLibrarySort() {
  const sort = $("library-sort").value || "name";
  const size = sort.startsWith("size");
  $("sort-name").setAttribute("aria-pressed",String(!size));
  $("sort-size").setAttribute("aria-pressed",String(size));
  $("sort-name-direction").textContent = size ? "" : sort === "name_desc" ? "↓" : "↑";
  $("sort-size-direction").textContent = size ? sort === "size_desc" ? "↓" : "↑" : "";
  $("sort-name").setAttribute("aria-label",`Sort by name${!size ? sort === "name_desc" ? ", descending" : ", ascending" : ""}`);
  $("sort-size").setAttribute("aria-label",`Sort by size${size ? sort === "size_desc" ? ", largest first" : ", smallest first" : ""}`);
}
async function toggleLibrarySort(column) {
  const current = $("library-sort").value;
  $("library-sort").value = column === "size" ? current === "size_desc" ? "size_asc" : "size_desc" : current === "name" ? "name_desc" : "name";
  renderLibrarySort();
  await loadLibrary();
}
$("sort-name").addEventListener("click", () => safe(() => toggleLibrarySort("name")));
$("sort-size").addEventListener("click", () => safe(() => toggleLibrarySort("size")));
$("reload-library").addEventListener("click", () => safe(loadLibrary));
$("back").addEventListener("click", () => {
  if ($("service").value.startsWith("folder:")) {
    const root = $("service").value.slice(7);
    state.folder =
      state.folder === root
        ? root
        : state.folder?.slice(0, state.folder.lastIndexOf("/")) || root;
    saveLibraryDraft();
    state.libraryOffset = 0;
    clearFileSelection();
  } else resetLibrary();
  safe(loadLibrary);
});
$("search-button").addEventListener("click", () => {
  state.libraryOffset = 0;
  safe(loadLibrary);
});
$("toggle-library-search").addEventListener("click", () => {
  const open = $("library-search-label").hidden;
  $("library-search-label").hidden = !open;
  $("search-button").hidden = !open;
  $("toggle-library-search").setAttribute("aria-expanded", String(open));
  $("toggle-library-search").setAttribute(
    "aria-label",
    open ? "Close search" : "Show search",
  );
  $("toggle-library-search").title = open ? "Close search" : "Search";
  $("toggle-library-search").classList?.toggle("search-open", open);
  if (open) $("search").focus();
  else {
    $("search").value = "";
    state.libraryOffset = 0;
    safe(loadLibrary);
  }
});
$("search").addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    state.libraryOffset = 0;
    safe(loadLibrary);
  }
});
async function loadMoreLibrary() {
  if (state.libraryLoading || !state.libraryHasMore || !serverReachable) return;
  state.libraryLoading = true;
  $("library-more").disabled = true;
  try {
    await loadLibrary(true);
  } finally {
    state.libraryLoading = false;
    $("library-more").disabled = false;
  }
}
$("library-more").addEventListener("click", () => safe(loadMoreLibrary));
if (typeof IntersectionObserver !== "undefined") {
  new IntersectionObserver(
    (entries) => {
      if (entries.some((e) => e.isIntersecting) && state.tab === "library")
        safe(loadMoreLibrary);
    },
    { rootMargin: "200px" },
  ).observe($("library-sentinel"));
}
const videoFile = (path) =>
  /\.(mkv|mp4|m4v|avi|mov|ts|m2ts|webm|mpg|mpeg)$/i.test(path);
function clearFileSelection() {
  $("path").value = "";
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
  controls();
}
async function navigateFolder(path) {
  state.allSelectionKeys=null;
  state.folderSelectionMembers=new Map();
  saveLibraryDraft();
  state.folder = path;
  state.libraryOffset = 0;
  $("search").value = "";
  clearFileSelection();
  await loadFolder();
}
function folderBreadcrumbs(root, path) {
  const host = $("folder-breadcrumbs");
  host.hidden = false;
  $("selection-title").hidden = true;
  host.replaceChildren();
  const parts =
    path === root
      ? []
      : path.slice(root.replace(/\/$/, "").length + 1).split("/");
  let current = root;
  const add = (label, target, last) => {
    const b = button(label, () => navigateFolder(target), "breadcrumb-button");
    b.title = target;
    if (last) b.setAttribute("aria-current", "location");
    host.append(b);
  };
  add(root.split("/").filter(Boolean).pop() || "/", root, parts.length === 0);
  parts.forEach((part, index) => {
    current = `${current.replace(/\/$/, "")}/${part}`;
    host.append(node("span", "/", "breadcrumb-separator"));
    add(part, current, index === parts.length - 1);
  });
}
async function loadFolder(revision = ++state.libraryRevision, more = false) {
  restoreLibraryDraft();
  clearTimeout(state.folderSizesTimer);
  state.libraryLoading = true;
  if (!more) {
    state.libraryHasMore = false;
    $("library-more").hidden = true;
    $("library-items").replaceChildren(node("p", "Loading files…", "muted"));
  }
  controls();
  try {
    if (!more) state.libraryOffset = 0;
    if (!more) {state.folderListing = null;state.folderHeaderKeys=null;}
    const root = $("service").value.slice(7);
    if (
      !state.folder ||
      (state.folder !== root &&
        !state.folder.startsWith(`${root.replace(/\/$/, "")}/`))
    )
      state.folder = root;
    state.folderSelected ||= new Set();
    const page = await api(
      `folder?${new URLSearchParams({ path: state.folder, q: $("search").value, offset: state.libraryOffset, sizes:"1", sort: $("library-sort").value || "name", listing:state.folderListing || "" })}`,
    );
    if (revision !== state.libraryRevision) return;
    state.folderListing = page.listing || null;
    state.folderHeaderKeys = !more && !page.has_more && !page.items.some(file=>file.is_dir) ? new Set(page.items.map(file=>file.path)) : null;
    $("size-order-note").hidden = !page.size_order_pending;
    $("selection-title").textContent =
      page.path === root
        ? root.split("/").pop() || root
        : page.path.slice(root.length + 1);
    $("selection-title").title = page.path;
    folderBreadcrumbs(root, page.path);
    $("back").hidden = state.folder === root;
    $("library-total").textContent =
      `${page.total} item${page.total === 1 ? "" : "s"}`;
    if (!more) {
      $("library-items").replaceChildren();
      state.folderSizeTargets = new Map();
    }
    state.folderSizeTargets ||= new Map();
    for (const file of page.items) {
      const row = node("div", "", "media-row");
      {
        const check = document.createElement("input");
        check.type = "checkbox";
        check.dataset.selection = file.path;
        if (file.is_dir) check.dataset.folder = file.path;
        check.checked = state.folderSelected.has(file.path);
        check.setAttribute(
          "aria-label",
          `Select ${file.path.split("/").pop()}`,
        );
        check.addEventListener("change", () => {
          if (file.is_dir) { safe(() => selectFolderFiles(file.path,check.checked)); return; }
          check.checked
            ? state.folderSelected.add(file.path)
            : state.folderSelected.delete(file.path);
          fileSelectionChanged();
        });
        const checkLabel = node("label", "", "media-check");
        checkLabel.append(check);
        row.append(checkLabel);
      }
      const sizeLabel = node("span", file.is_dir ? "Calculating…" : bytes(file.size), "media-size");
      if (file.is_dir) {
        showFolderSize(sizeLabel, file.folder_size || {status:"calculating"});
        const targets = state.folderSizeTargets.get(file.path) || [];
        targets.push(sizeLabel);
        state.folderSizeTargets.set(file.path, targets);
      }
      const title = button(
          file.path.split("/").pop(),
          async () => {
            if (file.is_dir) {
              await navigateFolder(file.path);
            } else {
              selectOneFile(file.path);
              $("path").value = file.path;
              state.file = file;
              state.fileMedia = null;
              state.fileService = null;
              $("scope").value = "file";
              setFileStep("configure");
              writeNavigation();
            }
            controls();
          },
          "media-title",
      );
      if (file.is_dir) {
        const icon = actionIcon("Folder");
        if (icon) {
          title.replaceChildren(icon, node("span", file.path.split("/").pop()));
          title.className += " folder-title";
        }
      }
      row.append(title, sizeLabel);
      $("library-items").append(row);
    }
    if (!page.items.length)
      $("library-items").append(
        node("p", "No videos or folders found.", "empty"),
      );
    state.libraryLoaded = state.libraryOffset + page.items.length;
    state.libraryHasMore = page.has_more;
    state.libraryError = false;
    state.libraryNeedsLoad = false;
    if (!more) writeNavigation();
    $("library-more").hidden = !page.has_more;
    controls();
    if (state.folderSizeTargets.size) {
      const pending = [...state.folderSizeTargets.values()].flat().some(label => label.dataset.pending === "1");
      state.folderSizesTimer = setTimeout(() => {
        void updateFolderSizes(revision, page.path, $("search").value, state.libraryLoaded);
      }, pending ? 3000 : 30000);
    }
  } catch (error) {
    if (revision === state.libraryRevision) libraryFailure(error, more);
    throw error;
  } finally {
    if (revision === state.libraryRevision) {
      state.libraryLoading = false;
      controls();
    }
  }
}
function showFolderSize(target, size) {
  target.dataset.pending = size?.status === "calculating" || size?.updating ? "1" : "0";
  target.textContent = size?.status === "calculating" ? "Calculating…"
    : size?.bytes != null ? `${size.status === "partial" ? "≥ " : ""}${bytes(size.bytes)}`
    : "Unavailable";
  target.title = size?.note || (size?.status !== "calculating" && size?.measured_at
    ? `Total file bytes, excluding symbolic links. Measured ${new Date(size.measured_at).toLocaleString()}. Saved across restarts; changes checked in the background.${size.updating ? " Updating; last measured total remains visible." : ""}`
    : "Measuring total file bytes in this folder.");
}
async function updateFolderSizes(revision, path, query, loaded) {
  const auth = state.authRevision;
  const current = () => revision === state.libraryRevision && auth === state.authRevision &&
    state.folder === path && state.tab === "library" && !$("workspace").hidden;
  if (!current() || !state.folderSizeTargets?.size) return;
  let pending = false;
  try {
    for (let offset = 0; offset < loaded; offset += 100) {
      const page = await api(`folder?${new URLSearchParams({path, q:query, offset, sizes:"1", sort:$("library-sort").value || "name", listing:state.folderListing || ""})}`);
      if (!current()) return;
      for (const item of page.items || []) {
        const target = state.folderSizeTargets.get(item.path);
        if (!target) continue;
        for (const label of Array.isArray(target) ? target : [target]) showFolderSize(label, item.folder_size);
        pending ||= item.folder_size?.status === "calculating" || item.folder_size?.updating;
      }
    }
    if (current()) state.folderSizesTimer = setTimeout(() => {
      void updateFolderSizes(revision, path, query, loaded);
    }, pending ? 3000 : 30000);
  } catch {
    if (current()) for (const targets of state.folderSizeTargets.values()) {
      for (const target of Array.isArray(targets) ? targets : [targets]) {
        if (target.textContent === "Calculating…") showFolderSize(target, {note:"Could not retrieve folder size. Refresh to try again."});
      }
    }
  }
}
async function browse(path) {
  if (
    ![...$("service").options].some(
      (o) => o.value === `folder:${$("root").value}`,
    )
  )
    return;
  $("service").value = `folder:${$("root").value}`;
  resetLibrary();
  state.folder = path;
  await loadFolder();
}
$("browse-root").addEventListener("click", () =>
  safe(async () => {
    await browse($("root").value);
    $("path-dialog").close();
  }),
);
$("path").addEventListener("input", () => {
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
  $("scope").value = "file";
  controls();
});
$("inspect").addEventListener("click", () =>
  safe(async () => {
    let report;
    try {
      report = await tool("inspect_media", { path: $("path").value });
    } catch (error) {
      if (/no such file or directory/i.test(error.message))
        throw new Error("File not found. Choose an existing file.");
      throw error;
    }
    showData(
      "inspection",
      [
        report.container,
        report.video_codec,
        report.resolution,
        report.bit_depth ? `${report.bit_depth}-bit` : null,
        bytes(report.size_bytes),
        report.duration_sec
          ? `${Math.round(report.duration_sec / 60)} min`
          : null,
      ]
        .filter(Boolean)
        .join(" · "),
    );
    $("path-dialog").close();
    setFileStep("configure");
  }),
);

function setFileStep(step) {
  const previous = state.fileStep;
  state.fileStep = step;
  $("library").dataset.fileStep = step;
  if (
    typeof matchMedia === "function" &&
    matchMedia("(max-width: 900px)").matches
  ) {
    $("library").scrollIntoView?.({ block: "start" });
    if (step === "configure") $("back-to-files").focus();
    else if (previous === "configure")
      ($("configure-selection").hidden
        ? $("service")
        : $("configure-selection")
      ).focus();
  }
}
function configureSelection(advance = true, preserveScope = false) {
  const folder = $("service").value.startsWith("folder:");
  const selected = folder
    ? [...(state.folderSelected || [])]
    : [...state.selected];
  if (selected.length > 1) {
    if (!preserveScope) {$("scope").value = "batch";$("selected-only").checked = true;}
  } else if (selected.length === 1) {
    const file = folder ? { path: selected[0] } : state.files.get(selected[0]);
    if (!file?.path) throw new Error("Choose a file with a valid path.");
    $("path").value = file.path;
    state.file = file;
    state.fileMedia = folder ? null : state.media;
    state.fileService = folder ? null : $("service").value;
    if (!preserveScope) $("scope").value = "file";
  } else if (!$("path").value.trim()) {
    throw new Error("Choose a file first.");
  }
  controls();
  if (advance) {setFileStep("configure"); writeNavigation();}
}
function selectOneFile(key) {
  if ($("service").value.startsWith("folder:"))
    state.folderSelected = new Set([key]);
  else state.selected = new Set([key]);
  $("library-items")
    .querySelectorAll('input[type="checkbox"]')
    .forEach((input) => {
      input.checked = input.dataset.selection === String(key);
    });
}
function fileSelectionChanged() {
  const count = $("service").value.startsWith("folder:")
    ? state.folderSelected?.size || 0
    : state.selected.size;
  const singleColumn =
    typeof matchMedia === "function" &&
    matchMedia("(max-width: 900px)").matches;
  if (!count) {
    $("scope").value = "file";
    $("selected-only").checked = false;
    clearFileSelection();
  } else if (!singleColumn) configureSelection(false);
  else controls();
}
async function selectAllFiles() {
  if (state.libraryLoading || state.selectionLoading) return;
  const revision = state.libraryRevision, auth = state.authRevision;
  const folder = $("service").value.startsWith("folder:");
  state.selectionLoading = true;
  controls();
  try {
    let keys, files;
    if (folder) keys = (await api(`folder?${new URLSearchParams({path:state.folder,files:"1",recursive:"1",q:$("search").value})}`)).paths;
    else {
      const page = await api(`library?${new URLSearchParams({service:$("service").value,id:state.media.id,q:$("search").value,all:"1"})}`);
      files = page.items;
      keys = files.map(file => file.id);
    }
    if (revision !== state.libraryRevision || auth !== state.authRevision) return;
    if (!keys.length) throw new Error("No videos found to select.");
    state.allSelectionKeys=new Set(keys);
    if (folder) state.folderSelected = new Set(keys);
    else {
      files.forEach(file => state.files.set(file.id,file));
      state.selected = new Set(keys);
    }
    $("library-items").querySelectorAll('input[type="checkbox"]').forEach(input => { input.checked = true; });
    fileSelectionChanged();
  } finally {
    state.selectionLoading = false;
    controls();
  }
}
async function selectFolderFiles(path, checked) {
  if (state.selectionLoading || state.libraryLoading) return;
  const revision=state.libraryRevision, auth=state.authRevision;
  state.selectionLoading=true; controls();
  try {
    const keys=(await api(`folder?${new URLSearchParams({path,files:"1",recursive:"1",q:$("search").value})}`)).paths;
    if (revision!==state.libraryRevision || auth!==state.authRevision) return;
    state.folderSelectionMembers ||= new Map();
    state.folderSelectionMembers.set(path,keys);
    for (const key of keys) checked ? state.folderSelected.add(key) : state.folderSelected.delete(key);
    fileSelectionChanged();
  } finally { state.selectionLoading=false; controls(); }
}
$("select-all-files").addEventListener("change", () => {
  if ($("select-all-files").checked) safe(selectAllFiles);
  else { state.folderSelected?.clear(); state.selected.clear(); $("library-items").querySelectorAll('input[type="checkbox"]').forEach(input=>input.checked=false); fileSelectionChanged(); }
});
$("clear-selected-files").addEventListener("click", () => {
  state.folderSelected?.clear();
  state.selected.clear();
  $("library-items").querySelectorAll('input[type="checkbox"]').forEach(input => { input.checked = false; });
  fileSelectionChanged();
});
function sourceSummary() {
  if ($("scope").value !== "batch")
    return $("path").value.split("/").pop() || "Choose a file.";
  const folder = $("service").value.startsWith("folder:");
  const count = folder ? state.folderSelected?.size || 0 : state.selected.size;
  const title =
    state.folder?.split("/").filter(Boolean).pop() ||
    state.media?.title ||
    "Choose a folder or series";
  return $("selected-only").checked
    ? `${count} selected ${count === 1 ? "file" : "files"} · ${title}`
    : `Batch: ${title}`;
}
function finishSubmission(mode) {
  if (mode === "encode") {
    state.selected.clear();
    state.folderSelected?.clear();
    $("library-items")
      .querySelectorAll('input[type="checkbox"]')
      .forEach((input) => {
        input.checked = false;
      });
    $("scope").value = "file";
    $("selected-only").checked = false;
    clearFileSelection();
  }
  setFileStep("browse");
}
$("configure-selection").addEventListener("click", () =>
  safe(configureSelection),
);
$("back-to-files").addEventListener("click", () => { setFileStep("browse"); writeNavigation(); });
$("use-container").addEventListener("click", () => safe(async () => {
  // Freeze discovery into the same explicit selection used by checkboxes.
  await selectAllFiles();
  configureSelection();
}));
function processingMode() {
  return document.querySelector?.('input[name="processing-mode"]:checked')?.value || "quality";
}
function setProcessingMode(mode) {
  document.querySelectorAll('input[name="processing-mode"]').forEach(input => { input.checked = input.value === mode; });
}
document.querySelectorAll('input[name="processing-mode"]').forEach(input => input.addEventListener("change", controls));
$("technical").addEventListener("change", controls);

function renderProfileFacts(output, data, name, preserveDepth) {
  output.replaceChildren();
  const description = data.description || data.record?.description;
  if (description) output.append(node("p", description, "metadata"));
  const p = data.profile || {}, v = p.video || {};
  const facts = node("dl", "", "profile-facts");
  const add = (label, value) => facts.append(node("dt", label), node("dd", value));
  add("Encoder", profileEncoder(p));
  add("Quality", profileRate(p));
  add("Process", p.optimization?.enabled ? "Tests quality samples before full conversion" : "Converts full video using this profile");
  add("Output depth", preserveDepth ? "Same as source · Overrides profile depth" : v.profile === "main10" || /10/.test(v.pixel_format || "") ? "10-bit" : v.pixel_format || v.profile ? "8-bit" : "Encoder default");
  if (v.spatial_aq != null) add("Image detail", v.spatial_aq ? "Adaptive allocation across image details" : "Standard allocation");
  add("Audio", p.audio?.mode === "copy" ? "Keep original audio tracks" : p.audio?.mode === "compact" ? "Compact · Convert lossless tracks to AAC" : p.audio?.mode || "Profile default");
  add("Output", p.container?.toUpperCase() || "Profile default");
  output.append(facts, node("p", `Profile ID: ${name}`, "metadata"));
}
function renderCreationProfile() {
  if (!$("technical").checked) {
    $("creation-profile-info").hidden = true;
    return;
  }
  const name = $("profile").value, output = $("creation-profile-info");
  output.hidden = name === "auto" && !$("custom").checked;
  if (!output.hidden) {
    if ($("custom").checked) {
      try { renderProfileFacts(output, {profile:profileFromControls()}, "Custom settings", $("preserve-depth").checked); }
      catch { output.textContent = "Complete the custom settings to see the effective profile."; }
    } else if (state.recipeDetails[name]) renderProfileFacts(output, state.recipeDetails[name], name, $("preserve-depth").checked);
    else output.textContent = "Profile details unavailable. Reload profiles before submitting.";
  }
  const minimum = $("min-savings").value || state.info?.min_savings_percent;
  $("creation-limits-note").textContent = `Minimum savings: ${minimum != null ? minimum + "%" : "server default"} · Originals stay in place until replacement is approved.`;
}
function controls() {
  renderCreationProfile();
  saveLibraryDraft();
  const batch = $("scope").value === "batch";
  const technical = $("technical").checked;
  $("legacy-controls").hidden = !technical;
  $("processing-goal").hidden = technical;
  $("processing-policy").hidden = technical;
  $("processing-policy").textContent = "Automatic analyzes samples and chooses an effective plan for your goal. Quality and savings must pass validation; a result is not guaranteed. Candidates only; replacing originals requires separate approval." + (!state.fileMedia && !state.media ? " Content type is unclassified; a conservative general-video policy applies." : "");
  $("enqueue").textContent = technical ? (batch ? "Create candidates" : "Create candidate") : "Process";
  $("source-summary").textContent = sourceSummary();
  $("batch-options").hidden = !batch;
  const folder = $("service").value.startsWith("folder:");
  const selectedCount = folder
    ? state.folderSelected?.size || 0
    : state.selected.size;
  const folderCount = folder ? new Set([...(state.folderSelected || [])].map(path=>path.slice(0,path.lastIndexOf("/")))).size : 0;
  $("selection-count").textContent = selectedCount ? `${selectedCount} selected${folder ? ` across ${folderCount} ${folderCount === 1 ? "folder" : "folders"}` : ""}` : "";
  $("configure-selection").hidden = !selectedCount && !$("path").value.trim();
  $("configure-selection").disabled = state.libraryLoading;
  $("use-container").hidden = folder
    ? !state.folder
    : $("service").value !== "sonarr" || !state.media;
  $("use-container").disabled = state.libraryLoading || state.selectionLoading;
  $("use-container").textContent = folder ? "Select folder" : "Select series";
  $("select-all-files").hidden = !folder && !state.files?.size;
  $("selection-toolbar").hidden = true;
  $("use-container").title = folder ? "Choose this folder, including its subfolders" : "Choose this series";
  $("select-all-files").disabled = state.libraryLoading || state.selectionLoading;
  const selectedKeys=folder ? state.folderSelected || new Set() : state.selected;
  $("library-items").querySelectorAll('input[type="checkbox"]').forEach(input=>{
    const key=folder ? input.dataset.selection : Number(input.dataset.selection);
    const keys=input.dataset.folder ? state.folderSelectionMembers?.get(input.dataset.folder) || [...(state.allSelectionKeys || selectedKeys)].filter(key=>key.startsWith(input.dataset.folder+"/")) : [key];
    const selected=keys.filter(key=>selectedKeys.has(key)).length;
    input.checked=keys.length>0 && selected===keys.length;
    input.indeterminate=selected>0 && !input.checked;
    input.disabled=Boolean(state.selectionLoading);
  });
  const headerKeys = state.allSelectionKeys || (folder ? state.folderHeaderKeys : state.media && !state.libraryHasMore ? new Set(state.files.keys()) : null);
  $("select-all-files").checked = headerKeys?.size>0 && [...headerKeys].every(key=>selectedKeys.has(key));
  $("select-all-files").indeterminate = !$("select-all-files").checked && (headerKeys ? [...headerKeys].some(key=>selectedKeys.has(key)) : selectedCount > 0);
  $("clear-selected-files").hidden = !selectedCount;
  $("clear-selected-files").disabled = state.libraryLoading || state.selectionLoading;
  $("configure-selection").disabled ||= state.selectionLoading;
  const explicitSelection = selectedCount > 0 && $("selected-only").checked;
  $("scope").closest("label").hidden = !technical || Boolean($("path").value.trim()) || explicitSelection;
  $("max-items").closest("label").hidden = explicitSelection;
  $("selected-only").closest("label").hidden = explicitSelection;
  $("media-kind").closest("label").hidden = !folder;
  $("season").closest("label").hidden = folder;
  $("recursive").closest("label").hidden = !folder || explicitSelection;
  $("promote-batch").closest("label").hidden =
    folder || $("service").value !== "sonarr";
  $("priority-label").hidden =
    !batch || $("profile").value !== "auto" || $("custom").checked;
  $("profile-mode-note").hidden = $("profile").value !== "auto" || $("custom").checked;
  $("profile-mode-note").textContent = "Automatic analyzes samples and chooses effective settings for the objective. Test samples is a technical benchmark: it does not convert the full video or replace originals.";
  $("custom-fields").hidden = !technical || !$("custom").checked;
  $("optimization-fields").hidden = !$("optimize").checked;
  const vt = $("encoder").value === "hevc_videotoolbox";
  $("vt-fields").hidden = !vt;
  $("x265-fields").hidden = vt;
  $("quality").max = vt ? "100" : "51";
  $("quality-label").firstChild.textContent = vt
    ? "Quality (higher is better)"
    : "CRF (lower is better)";
  $("quality").disabled = vt && $("rate-mode").value === "bitrate";
  $("bitrate").disabled = $("rate-mode").value !== "bitrate";
  $("max-bitrate").disabled = $("rate-mode").value !== "bitrate";
  $("preview").hidden = !technical || !batch;
  $("benchmark").hidden = !technical || batch;
  $("audio-note").hidden = $("audio").value !== "compact";
  const hasSource = batch
    ? Boolean(folder ? state.folder : state.media) &&
      (technical ? !$("selected-only").checked || selectedCount > 0 : selectedCount > 0)
    : Boolean($("path").value.trim());
  $("enqueue").disabled =
    !state.info?.transcode_enabled ||
    !state.workerInfo?.ready ||
    state.submitting ||
    state.selectionLoading ||
    state.libraryLoading ||
    !serverReachable ||
    !hasSource;
  $("benchmark").disabled =
    !state.info?.transcode_enabled ||
    !state.workerInfo?.ready ||
    state.submitting ||
    state.selectionLoading ||
    state.libraryLoading ||
    !serverReachable ||
    !hasSource;
  $("preview").disabled =
    !state.info?.transcode_enabled ||
    !state.workerInfo?.ready ||
    state.submitting ||
    state.selectionLoading ||
    state.libraryLoading ||
    !serverReachable ||
    !hasSource;
}
for (const id of [
  "scope",
  "custom",
  "optimize",
  "profile",
  "audio",
  "rate-mode",
])
  $(id).addEventListener("change", controls);
$("encoder").addEventListener("change", () => {
  $("quality").value = $("encoder").value === "libx265" ? "23" : "65";
  $("ladder").value =
    $("encoder").value === "libx265" ? "20,23,26" : "55,65,75";
  controls();
});
function numeric(id) {
  const n = Number($(id).value);
  if (!Number.isFinite(n)) throw new Error(`Invalid value: ${id}`);
  return n;
}
function profileFromControls() {
  const vt = $("encoder").value === "hevc_videotoolbox";
  const video = { codec: $("encoder").value, quality: numeric("quality") };
  if (vt && $("rate-mode").value === "bitrate") {
    video.quality = 0;
    video.average_bitrate_kbps = numeric("bitrate");
    if ($("max-bitrate").value) video.max_bitrate_kbps = numeric("max-bitrate");
  }
  if (!vt) {
    video.preset = $("preset").value;
    if ($("tune").value) video.tune = $("tune").value;
  }
  const p = {
    container: "mkv",
    video,
    audio: { mode: $("audio").value },
    subtitles: { mode: "preserve", convert_incompatible: true },
    preserve: { metadata: true, chapters: true, attachments: true },
    resilience: {
      max_attempts: 3,
      transient_retries: 2,
      retry_backoff_seconds: [5, 20],
      max_fallbacks: 0,
      fallbacks: [],
    },
    optimization: { enabled: false },
  };
  if ($("optimize").checked) {
    const values = $("ladder")
      .value.split(",")
      .map((v) => Number(v.trim()));
    if (!values.length || values.some((v) => !Number.isInteger(v) || v <= 0))
      throw new Error("Search values must be positive integers.");
    const quality = {
      preferred_metric: $("metric").value === "ssim" ? "ssim" : "vmaf",
      vmaf: {
        target: numeric("vmaf-target"),
        minimum: numeric("vmaf-min"),
        marginal_tolerance: 0.5,
      },
      ssim: {
        target: numeric("ssim-target"),
        minimum: numeric("ssim-min"),
        marginal_tolerance: 0.005,
      },
    };
    if ($("final-validation").value)
      quality.final_validation = { mode: $("final-validation").value };
    p.optimization = {
      enabled: true,
      sampling: {
        strategy: "distributed",
        sample_count: numeric("sample-count"),
        sample_seconds: numeric("sample-seconds"),
      },
      quality,
      search: {
        max_candidates: values.length,
        [vt && $("rate-mode").value === "bitrate"
          ? "bitrate_values"
          : "quality_values"]: values,
      },
    };
  }
  return p;
}

async function submitJob(mode = "encode") {
  if (state.submitting || state.selectionLoading) return;
  state.submitting = true;
  controls();
  try {
    notify("Submitting job…");
    return await buildAndSubmitJob(mode);
  } finally {
    state.submitting = false;
    controls();
  }
}
async function buildAndSubmitJob(mode = "encode") {
  const batch = $("scope").value === "batch";
  let name = batch ? "transcode_batch" : "transcode_media";
  const simple = !$("technical").checked && mode !== "benchmark";
  const inputs = simple ? {mode:processingMode()} : { preserve_source_bit_depth: $("preserve-depth").checked };
  const folderSource = $("service").value.startsWith("folder:");
  const explicitSelection = (folderSource ? state.folderSelected?.size : state.selected.size) > 0 && (simple || $("selected-only").checked);
  if (batch) {
    if ($("service").value.startsWith("folder:")) {
      if (explicitSelection) {
        if (!state.folderSelected?.size)
          throw new Error("Select at least one video.");
        inputs.paths = [...state.folderSelected];
      } else {
        const selection = await api(
          `folder?${new URLSearchParams({ path: state.folder, files: "1", recursive: $("recursive").checked ? "1" : "0" })}`,
        );
        inputs.paths = selection.paths;
      }
      if (!inputs.paths.length)
        throw new Error("No videos found in this folder.");
      if (!simple) inputs.media_type = $("media-kind").value;
    } else {
      if (!state.media || $("service").value !== "sonarr")
        throw new Error("Choose a series or folder for the batch.");
      Object.assign(inputs, { service: "sonarr", series_id: state.media.id });
      if (!explicitSelection && $("season").value !== "") inputs.season = Number($("season").value);
      if (explicitSelection) {
        if (!state.selected.size) throw new Error("Select at least one file.");
        inputs.episode_file_ids = [...state.selected];
      }
      if (!simple && $("promote-batch").checked) inputs.promote_candidates = true;
    }
    if (!simple && !explicitSelection && $("max-items").value) inputs.max_items = numeric("max-items");
    if (mode === "preview") inputs.dry_run = true;
  } else {
    if (!$("path").value.trim())
      throw new Error("Choose a file or enter its path.");
    inputs.path = $("path").value.trim();
    if (!simple && mode !== "benchmark") inputs.media_type = $("media-kind").value;
    if (state.fileMedia && mode !== "benchmark") {
      inputs.media_type = state.fileService === "radarr" ? "movie" : "tv";
      inputs.is_anime =
        state.fileMedia.seriesType === "anime" ||
        (state.fileMedia.genres || []).some((g) => g.toLowerCase() === "anime");
    }
    if (mode === "benchmark") name = "benchmark_transcode";
    else if (state.fileMedia)
      inputs.library_context = {
        service: state.fileService,
        id: state.fileMedia.id,
      };
  }
  if (!simple && $("custom").checked) {
    inputs.profile_config = profileFromControls();
    if (inputs.profile_config.optimization?.enabled)
      inputs.metric = $("metric").value;
  } else if (!simple) {
    inputs.profile = $("profile").value;
    if (batch && inputs.profile === "auto")
      inputs.priority = $("priority").value;
  }
  if (!simple && $("custom").checked && name !== "benchmark_transcode") {
    if ($("min-savings").value)
      inputs.min_savings_percent = numeric("min-savings");
    inputs.max_size_increase_percent = numeric("max-growth");
  }
  const request = JSON.stringify({ name, inputs });
  let receipt;
  try {
    receipt = JSON.parse(
      sessionStorage.getItem("navigatorr_submission") || "null",
    );
  } catch {}
  if (!receipt || receipt.request !== request) {
    receipt = { request, key: submissionID() };
    sessionStorage.setItem("navigatorr_submission", JSON.stringify(receipt));
  }
  $("enqueue").disabled = true;
  $("preview").disabled = true;
  $("benchmark").disabled = true;
  try {
    const r = await tool("action_run", {
      action: name,
      inputs: JSON.stringify(inputs),
      idempotency_key: receipt.key,
    });
    sessionStorage.removeItem("navigatorr_submission");
    // Context is display metadata only; actual promotion re-resolves and verifies
    // the library through the shared engine.
    if (state.fileMedia) {
      try {
        localStorage.setItem(
          `navigatorr_media:${r.id}`,
          JSON.stringify({
            service: state.fileService,
            id: state.fileMedia.id,
          }),
        );
      } catch {}
    }
    $("job-feedback").textContent = `Queued: ${r.id}`;
    state.jobsLoaded = 0;
    finishSubmission(mode);
    selectTab("jobs");
    await loadJobs();
    notify(`Job queued: ${r.id}.`);
  } finally {
    $("preview").disabled = false;
    $("benchmark").disabled = false;
    controls();
  }
}
$("job-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(() => submitJob());
});
$("preview").addEventListener("click", () => safe(() => submitJob("preview")));
$("benchmark").addEventListener("click", () =>
  safe(() => submitJob("benchmark")),
);

function focusedControl(container) {
  const active = document.activeElement;
  return container.contains(active) && active.dataset.jobControl
    ? { id: active.dataset.jobControl, label: active.textContent }
    : null;
}
function restoreControl(container, focus) {
  if (!focus) return;
  [...container.querySelectorAll("[data-job-control]")]
    .find(
      (b) => b.dataset.jobControl === focus.id && b.textContent === focus.label,
    )
    ?.focus({ preventScroll: true });
}
function savingsLine(savings, compact = false, replaced = false) {
  if (!savings) return null;
  const parts = [];
  if (savings.source_bytes != null)
    parts.push(`Source ${bytes(savings.source_bytes)}`);
  if (!replaced && savings.estimated_saved_bytes != null)
    parts.push(
      `${savings.estimate_kind === "sampled_benchmark" ? (compact ? "Estimate (samples)" : "Sample estimate") : compact ? "Estimate (profile)" : "Profile estimate"} ${bytes(savings.estimated_saved_bytes)}`,
    );
  if (
    !replaced &&
    savings.candidate_saved_bytes != null &&
    savings.realized_saved_bytes == null
  )
    parts.push(
      savings.candidate_saved_bytes < 0
        ? `Candidate grows by ${bytes(-savings.candidate_saved_bytes)}`
        : `${compact ? "Potential savings" : "Potential savings"} ${bytes(savings.candidate_saved_bytes)}`,
    );
  if (savings.realized_saved_bytes != null)
    parts.push(`Freed ${bytes(savings.realized_saved_bytes)}`);
  if (savings.partial)
    parts.push(`Partial measurement: ${savings.measured_files || 0} files`);
  return parts.length
    ? node("p", parts.join(" · "), "metadata savings-line")
    : null;
}
function batchReasonLabel(job, reason) {
  return job.batch?.failed > reason.count ? `Example (${reason.count} of ${job.batch.failed} failed files)` : `${reason.count} ${reason.count === 1 ? "file" : "files"}`;
}
function batchProgress(job) {
  const batch = visibleBatch(job);
  if (["completed", "failed", "cancelled"].includes(job.status)) return null;
  if (job.status === "waiting_decision" && !batch?.running) return null;
  if (!batch || batch.dry_run || batch.outcome === "no_changes" || !Number.isInteger(batch.total) || batch.total <= 0) return null;
  const count = key => Number.isInteger(batch[key]) && batch[key] > 0 ? batch[key] : 0;
  const processed = Math.min(batch.total, ["completed", "failed", "policy", "skip", "rejected"].reduce((total, key) => total + count(key), 0));
  // Workflow steps have equal estimated weight, rather than pretending their
  // durations are known. Worker telemetry supplies the measured current step.
  const byFile = new Map();
  const activities = job.activities || [];
  const activityIDs = new Set(activities.map(a => a.id).filter(Boolean));
  for (const activity of activities) {
    if (activity.parent_action_id && activityIDs.has(activity.parent_action_id)) continue;
    const stages = activity.stages || [];
    const step = Number(activity.current_step);
    const child = activities.find(a => a.parent_action_id === activity.id);
    const w = child?.worker || activity.worker || {};
    const phase = w.transcode_phase || w.benchmark_phase || w.phase;
    const details = w.progress_details || w.benchmark_progress_details;
    let fraction = 0;
    const stage = stages[step]?.name;
    const workerStage = ["wait_benchmark","wait_transcode"].includes(stage);
    if (workerStage && !w.progress_is_stale) {
      if (details?.total_units > 0) fraction = Math.max(0, Math.min(1, Number(details.completed_units || 0) / details.total_units));
      else if (Number.isFinite(Number(w.progress)) && w.progress != null && !["queued","preparing"].includes(phase)) fraction = Math.max(0, Math.min(1, Number(w.progress)/100));
    }
    if (stage === "preflight" && activity.work?.total_bytes > 0) fraction = Math.min(.8, Math.max(0, Number(activity.work.bytes_read || 0)/activity.work.total_bytes*.8));
    else if (stage === "preflight" && activity.work?.phase === "probing_source") fraction = .8;
    const progress = stages.length && Number.isInteger(step) ? Math.min(.99, (step + fraction)/stages.length) : 0;
    const key = activity.file || activity.id || "Active file";
    const prior = byFile.get(key);
    if (!prior || progress > prior.progress) byFile.set(key, {progress, label: `${key.split("/").pop() || "Active file"}: ${workerStage && phase ? workerPhaseLabel(phase) : stage === "preflight" && activity.work?.phase === "hashing_source" ? `Verifying source SHA-256 (${bytes(activity.work.bytes_read || 0)} / ${bytes(activity.work.total_bytes)})` : stageLabel(stage || "preflight")}${workerStage && w.progress_is_stale ? " · Last measurement; stale" : ""}`});
  }
  const partial = Math.min(count("running"), byFile.size) ? [...byFile.values()].slice(0,count("running")).reduce((sum,a)=>sum+a.progress,0) : 0;
  const active = operationIsActive(job.status);
  let estimate = (processed + partial) / batch.total;
  if (active && job.replacement_requested) {
    const promotion = batch.promotion;
    const conversion = Math.min(1,estimate);
    const replacement = promotion?.eligible > 0 ? Math.min(1,Number(promotion.promoted || 0)/promotion.eligible) : 0;
    estimate = conversion * .85 + replacement * .15;
  }
  const percent = Math.min(active ? 99 : 100, estimate * 100);
  const result = node("div", "", `job-progress batch-progress ${queuePresentation(job).status === "Failed" ? "failed" : queuePresentation(job).status === "Cancelled" ? "cancelled" : ""}`);
  const counts = node("span", `${processed} / ${batch.total} files finished`, "batch-progress-count");
  counts.title = "Processed includes completed, failed, skipped and rejected files. Cancelled files and files waiting for a decision are not counted as processed.";
  const value = node("strong", `${active ? "~" : ""}${active ? Math.min(99,Math.round(percent)) : Math.round(percent)}%`, "batch-progress-percent");
  value.title = active ? "Estimated overall workflow progress: finished files plus active workflow steps and measured worker work. Steps have equal weight; requested replacements reserve 15% of work. This is not a time estimate." : "Files processed";
  const bar = document.createElement("progress");
  bar.max = batch.total;
  bar.value = percent / 100 * batch.total;
  bar.setAttribute("aria-label", active ? `Estimated overall workflow progress ${percent.toFixed(1)}%` : `${processed} of ${batch.total} files processed`);
  result.append(counts, value, bar);

  if (active) {
    const phases = [...byFile.values()].map(a=>a.label);
    const waiting = count("waiting_decision") ? `${count("waiting_decision")} ${count("waiting_decision") === 1 ? "file needs" : "files need"} your decision` : "";
    result.append(node("span", [...phases,waiting].filter(Boolean).join(" · ") || (processed === batch.total ? "Finishing batch / replacement checks" : "Waiting for worker / preparing selected files"), "batch-progress-phase metadata"));
  }
  result.append(node("span", "Estimated workflow progress · Not time remaining", "batch-progress-explanation metadata"));
  return result;
}
function telemetry(job) {
  const batch = batchProgress(job);
  if (batch) return batch;
  const w = job.worker;
  const result = node(
    "div",
    "",
    `job-progress ${w ? "worker-progress" : "workflow-progress"}`,
  );
  if (job.activities?.length > 1) {
    result.append(node("span",`${job.activities.length} active files · See worker slots above for each file and phase`,"metadata"));
    return result;
  }
  if (workerUnavailable(job) || w?.progress_is_stale) {
    result.className = "job-progress stalled-progress";
    const measured = w?.progress ?? w?.last_known_progress?.progress;
    const last = measured != null && Number.isFinite(Number(measured))
      ? ` Last reported: ${Number(measured).toFixed(1)}%.` : "";
    result.append(node("span", (workerUnavailable(job)
      ? "Worker unreachable. Retrying connection automatically."
      : "No recent progress from the worker.") + last, "metadata"));
    return result;
  }
  if (!w) {
    const stages = job.stages || [];
    const current = stages[job.current_step];
    const text = current
      ? `Step ${job.current_step + 1}/${stages.length} · ${stageLabel(current.name)}`
      : job.waiting_reason || "Starting job";
    result.append(node("span", text, "metadata"));
    return result;
  }
  const phase = w.transcode_phase || w.benchmark_phase || w.phase;
  const phases = {
    queued: "Queued",
    preparing: "Preparing",
    encoding: "Encoding",
    validating: "Validating",
    publishing: "Publishing",
    completed: "Completed",
    benchmarking: "Calibrating",
  };
  const parts = [
    job.activity_file ? `Current: ${job.activity_file.split("/").pop()}` : null,
    workerPhaseLabel(phase) || phases[phase] || phase,
    w.queue_position > 0 ? `Queue #${w.queue_position}` : null,
    w.speed > 0 ? `${Number(w.speed).toFixed(2)}×` : null,
    w.fps > 0 ? `${Number(w.fps).toFixed(1)} fps` : null,
  ];
  const eta = job.savings?.eta_seconds;
  const details = w.progress_details || w.benchmark_progress_details;
  if (details?.total_units > 0) parts.push(`${details.completed_units}/${details.total_units} sample tasks`);
  if (Number.isFinite(eta) && eta > 0 && !w.progress_is_stale)
    parts.push(`About ${Math.ceil(eta / 60)} min`);
  result.append(
    node("span", parts.filter(Boolean).join(" · ") || job.progress, "metadata"),
  );
  const measured = w.progress ?? w.last_known_progress?.progress;
  if (!["queued", "preparing"].includes(phase) && measured != null && Number.isFinite(Number(measured))) {
    const p = Math.max(0, Math.min(100, Number(measured)));
    const meter = document.createElement("meter");
    meter.min = 0;
    meter.max = 100;
    meter.value = p;
    meter.setAttribute("aria-label", `Worker progress ${p.toFixed(1)}%`);
    result.append(meter, node("span", `${p.toFixed(1)}%`, "metadata"));
  }
  if (w.last_progress_at)
    result.append(
      node(
        "span",
        `Measured ${new Date(w.last_progress_at).toLocaleTimeString()}`,
        "metadata progress-observed-at",
      ),
    );
  return result;
}
function stageLabel(name) {
  const labels = {
    plan_promotion: "Verify original and candidate",
    approve_promotion: "Review replacement",
    preserve_original: "Save and verify recovery copy",
    import_candidate: "Import verified candidate",
    remove_old_file: "Remove original library file",
    rename_candidate: "Finalize filename",
    rescan_library: "Refresh library",
    finalize_promotion: "Verify replacement and clean up recovery copy",
    resolve_source: "Inspect source",
    resolve_media: "Inspect source",
    resolve_batch: "Inspect selected files",
    resolve_and_inspect: "Inspect selected files",
    preflight: "Inspect original and prepare conversion",
    submit_benchmark: "Start quality calibration",
    wait_benchmark: "Measure quality samples",
    submit_transcode: "Send conversion to worker",
    wait_transcode: "Convert file",
    schedule_batch: "Process selected files",
    promote_batch: "Replace approved files",
    benchmark: "Calibrate quality",
    benchmark_transcode: "Calibrate quality",
    build_plan: "Prepare conversion",
    execute_transcode: "Convert file",
    validate_result: "Verify converted file",
    accept_result: "Review converted file",
  };
  return labels[name] || String(name || "Processing").replaceAll("_", " ");
}
function renderSavings(data) {
  const savings = data.savings || {};
  $("saved-total").textContent = bytes(savings.realized_bytes ?? 0);
  $("saved-count").textContent =
    `${savings.completed_replacements || 0} verified replacement${savings.completed_replacements === 1 ? "" : "s"}`;
  $("candidate-total").textContent = bytes(
    savings.candidate_saved_bytes ?? savings.candidate_bytes ?? 0,
  );
  $("estimated-total").textContent = bytes(
    savings.estimated_saved_bytes ?? savings.estimated_bytes ?? 0,
  );
  $("savings-history").replaceChildren();
  for (const item of data.history || []) {
    const row = node("div", "", "savings-history-row");
    row.append(
      node(
        "span",
        `${item.source_path?.split("/").pop() || "Replacement"} · ${new Date(item.completed_at).toLocaleDateString("en")}`,
        "metadata",
      ),
      node(
        "strong",
        `${bytes(item.saved_bytes)} · total ${bytes(item.cumulative_bytes)}`,
        "metadata",
      ),
    );
    $("savings-history").append(row);
  }
  if (!$("savings-history").children.length)
    $("savings-history").append(
      node("p", "No verified replacements yet.", "muted"),
    );
}
async function jobControl(id, name, args = {}) {
  if (jobIsBusy(id)) return;
  state.busyJobs.add(id);
  document.querySelectorAll("[data-job-control]").forEach((b) => {
    if (b.dataset.jobControl === id) b.disabled = true;
  });
  try {
    await tool(name, { id, ...args });
    await Promise.all([state.detail === id && $("job-detail").open ? refreshDetail() : Promise.resolve(), loadJobs()]);
  } finally {
    state.busyJobs.delete(id);
    document.querySelectorAll("[data-job-control]").forEach((b) => {
      if (b.dataset.jobControl === id) b.disabled = !serverReachable || (b.dataset.requiresWorker === "true" && !state.workerInfo?.ready);
    });
  }
}
function finishActionReview(accepted) {
  const review = state.actionReview;
  state.actionReview = null;
  $("action-review").close();
  review?.resolve(accepted);
}
function reviewAction({title, message, content, confirmLabel, cancelLabel = "Back", choices = []}) {
  finishActionReview(false);
  if (!serverReachable || $("workspace").hidden) return Promise.resolve(false);
  $("action-review-title").textContent = title;
  $("action-review-content").replaceChildren(...(content || [node("p", message)]));
  if (choices.length) {
    const decisions = node("div", "", "candidate-decisions");
    for (const choice of choices) {
      const b = button(choice.label, () => { if (serverReachable) finishActionReview(choice.value); });
      b.dataset.reviewChoice = choice.value;
      b.disabled = !serverReachable;
      decisions.append(b);
    }
    $("action-review-content").append(decisions);
  }
  $("confirm-action-review").textContent = confirmLabel;
  $("confirm-action-review").disabled = !serverReachable;
  $("dismiss-action-review").textContent = cancelLabel;
  return new Promise(resolve => {
    state.actionReview = {resolve};
    $("action-review").showModal();
  });
}
$("close-action-review").addEventListener("click", () => finishActionReview(false));
$("dismiss-action-review").addEventListener("click", () => finishActionReview(false));
$("action-review").addEventListener("cancel", (event) => {
  event.preventDefault();
  finishActionReview(false);
});
$("confirm-action-review").addEventListener("click", () => {
  if (serverReachable) finishActionReview(true);
});
function candidateReview(job) {
  const match = String(job.waiting_reason || "").match(/Candidate file size \((\d+) bytes\) exceeds original \((\d+) bytes\) by ([\d.]+)%, which is greater than max_size_increase_percent \(([\d.]+)%\)/);
  return {
    source: match ? Number(match[2]) : job.savings?.source_bytes,
    candidate: match ? Number(match[1]) : job.savings?.candidate_bytes,
    reason: match ? `The candidate is ${match[3]}% larger. This job allows up to ${match[4]}% growth.` : shortJobReason(job.waiting_reason) || "The candidate did not meet this job's validation limits.",
  };
}
function decisionStamp(job, decision) {
  return JSON.stringify([job.id, job.status, job.current_step, job.waiting_reason, job.waiting_options?.find(option => option.decision === decision), job.savings, job.promotion]);
}
async function reviewJobDecision(id, decision) {
  if (state.reviewingDecision) return;
  state.reviewingDecision = id;
  const auth = state.authRevision;
  try {
    const read = async () => (await api(`operations?id=${encodeURIComponent(id)}`)).jobs?.[0];
    const job = await read();
    let candidatePlan = null, reviewed = job;
    if (decision === "accept_loss" && job?.action_name === "transcode_batch") {
      candidatePlan = await api(`batch-reconfigure?id=${encodeURIComponent(id)}&candidate=1`);
      reviewed = (await api(`operations?id=${encodeURIComponent(candidatePlan.candidate_id)}`)).jobs?.[0];
      if (!reviewed) throw new Error("Candidate details unavailable. Refresh the queue.");
    }
    if (auth !== state.authRevision || $("workspace").hidden) return;
    if (job?.status !== "waiting_decision" || !job.waiting_options?.some(option => option.decision === decision))
      throw new Error("This job is no longer waiting for that decision. Refresh the queue.");
    const stamp = decisionStamp(job, decision);
    const info = candidateReview(reviewed);
    const content = [node("p", candidatePlan?.title || queuePresentation(reviewed).title, "review-filename")];
    if (decision === "accept_loss") {
      for (const [label, size] of [["Original", info.source], ["Candidate", info.candidate]]) {
        const row = node("div", "", "review-file");
        row.append(node("span", label, "muted"), node("strong", bytes(size)));
        content.push(row);
      }
      content.push(node("p", info.reason));
      content.push(node("p", "Accept this candidate despite the validation result. Replacing the library file requires a separate review.", "muted"));
    } else content.push(node("p", shortJobReason(job.waiting_reason) || "Review this job before continuing."));
    const candidateChoices = decision === "accept_loss" ? [
      ...(job.waiting_options.some(o=>o.decision === "reject") ? [{label:"Reject · keep original",value:"reject"}] : []),
      ...(candidatePlan ? [{label:"Transcode with other settings",value:"reconfigure"}] : []),
    ] : [];
    const chosen = await reviewAction({title:decision === "accept_loss" ? "Review candidate" : "Review decision", content, choices:candidateChoices, cancelLabel:decision === "accept_loss" ? "Decide later" : "Back", confirmLabel:decision === "accept_loss" ? "Accept candidate" : "Approve"});
    if (!chosen) return;
    if (chosen === "reconfigure") { await reconfigureBatch(id,true,candidatePlan); return; }
    const latest = await read();
    if (auth !== state.authRevision || $("workspace").hidden) return;
    if (decisionStamp(latest || {}, decision) !== stamp)
      throw new Error("The candidate or decision changed. Review the job again.");
    const selected = chosen === "reject" ? "reject" : decision;
    if (candidatePlan) {
      await commandRequest("candidate-decision",{id,candidate_id:candidatePlan.candidate_id,decision_version:candidatePlan.decision_version,decision:selected,background:true});
      await Promise.all([loadJobs(),state.detail === id && $("job-detail").open ? refreshDetail() : Promise.resolve()]);
      notify(selected === "reject" ? "Candidate rejected. Original preserved; other batch files continue." : "Candidate accepted. Replacement requires separate approval.");
    } else await jobControl(id, "action_resume", {decision:selected});
  } finally {
    state.reviewingDecision = null;
  }
}
async function prepareReplacement(job) {
  if (jobIsBusy(job.id)) return;
  if (job.replacement_action_id) {
    await openJob(job.replacement_action_id);
    return;
  }
  const durable = job.replacement_context;
  if (
    durable?.service === "filesystem" ||
    (["sonarr", "radarr"].includes(durable?.service) &&
      Number.isInteger(
        durable[durable.service === "radarr" ? "movie_id" : "series_id"],
      ) &&
      durable[durable.service === "radarr" ? "movie_id" : "series_id"] > 0)
  ) {
    await enqueueReplacement(job, durable);
    return;
  }
  let legacy;
  try {
    legacy = JSON.parse(
      localStorage.getItem(`navigatorr_media:${job.id}`) || "null",
    );
  } catch {}
  const integrations = (state.info.services || []).filter((entry) =>
    ["sonarr", "radarr"].includes(entry.name),
  );
  if (!integrations.length && !["sonarr", "radarr"].includes(legacy?.service)) {
    await enqueueReplacement(job, { service: "filesystem" });
    return;
  }
  // Old browser-only hints cannot decide how an agent-created or historical
  // candidate is imported. Ask once, then show the engine's exact approval plan.
  state.replacementChoice = { job, legacy };
  const method = $("replacement-method");
  method.replaceChildren();
  option(method, "filesystem", "Replace local file");
  for (const integration of integrations)
    option(
      method,
      integration.name,
      integration.name === "radarr"
        ? "Import through Radarr"
        : "Import through Sonarr",
    );
  method.value = integrations.some((entry) => entry.name === legacy?.service)
    ? legacy.service
    : "filesystem";
  $("replacement-source").textContent = job.source_path || job.id;
  $("prepare-replacement-choice").dataset.jobControl = job.id;
  $("prepare-replacement-choice").disabled = !serverReachable;
  updateReplacementChoice();
  if (!$("replacement-choice").open) $("replacement-choice").showModal();
}
function updateReplacementChoice() {
  const service = $("replacement-method").value;
  $("replacement-library-id-label").hidden = service === "filesystem";
  const legacy = state.replacementChoice?.legacy;
  $("replacement-library-id").value =
    service === legacy?.service && Number.isInteger(legacy.id) && legacy.id > 0
      ? String(legacy.id)
      : "";
  $("replacement-library-id").setAttribute(
    "aria-label",
    service === "radarr" ? "Movie ID" : "Series ID",
  );
}
async function enqueueReplacement(job, context) {
  if (jobIsBusy(job.id)) return;
  state.busyJobs.add(job.id);
  try {
    const latest = (await api(`operations?id=${encodeURIComponent(job.id)}`))
      .jobs?.[0];
    if (latest?.replacement_action_id) {
      $("replacement-choice").close();
      await openJob(latest.replacement_action_id);
      return;
    }
    if (!latest?.candidate_ready)
      throw new Error(
        "This candidate is no longer ready for replacement. Refresh the queue.",
      );
    const inputs = { transcode_action_id: job.id, service: context.service };
    if (["sonarr", "radarr"].includes(context.service)) {
      const key = context.service === "radarr" ? "movie_id" : "series_id";
      if (!Number.isInteger(context[key]) || context[key] <= 0)
        throw new Error("Choose a positive library ID.");
      inputs[key] = context[key];
    } else if (context.service !== "filesystem")
      throw new Error("Choose a supported replacement method.");
    const result = await tool("action_run", {
      action: "promote_transcode_candidate",
      inputs: JSON.stringify(inputs),
      idempotency_key: submissionID(),
    });
    $("replacement-choice").close();
    await loadJobs();
    await openJob(result.id);
  } finally {
    state.busyJobs.delete(job.id);
  }
}
$("replacement-method").addEventListener("change", updateReplacementChoice);
$("dismiss-replacement-choice").addEventListener("click", () =>
  $("replacement-choice").close(),
);
$("replacement-choice").addEventListener("close", () => {
  state.replacementChoice = null;
});
$("prepare-replacement-choice").addEventListener("click", () =>
  safe(async () => {
    const selection = state.replacementChoice;
    if (!selection) return;
    const service = $("replacement-method").value;
    const context = { service };
    if (service !== "filesystem") {
      const id = Number($("replacement-library-id").value);
      if (!Number.isInteger(id) || id <= 0)
        throw new Error(
          "Enter a positive library ID before reviewing the replacement.",
        );
      context[service === "radarr" ? "movie_id" : "series_id"] = id;
    }
    await enqueueReplacement(selection.job, context);
  }),
);
function batchCounts(batch, compact = false) {
  if (compact) {
    const counts = [
      [batch.queued, batch.dry_run ? "eligible" : "queued"],
      [batch.running, "active"],
      [batch.waiting_for_slot, "waiting for slot"],
      [batch.completed, "completed"],
      [batch.failed, "failed"],
      [batch.policy, "below minimum savings"],
      [batch.cancelled, "cancelled"],
      [batch.rejected, "rejected"],
      [batch.waiting_decision ?? batch.review, "need a decision"],
      [batch.skip, "skipped"],
    ];
    return (
      `${batch.dry_run ? "Preview · " : ""}${batch.total ?? 0} ${batch.total === 1 ? "file" : "files"}` +
      counts
        .filter(([count]) => count > 0)
        .map(([count, label]) => ` · ${count} ${label}`)
        .join("")
    );
  }
  return `${batch.dry_run ? "Preview · " : ""}${batch.total ?? 0} files · ${batch.queued ?? 0} queued · ${batch.running ?? 0} active · ${batch.waiting_for_slot ?? 0} waiting for slot · ${batch.completed ?? 0} completed · ${batch.failed ?? 0} failed · ${batch.waiting_decision ?? batch.review ?? 0} need a decision · ${batch.skip ?? 0} skipped`;
}
function statusClass(status) {
  if (["completed", "Completed", "Replaced", "Candidate ready", "Candidates ready"].includes(status)) return "completed";
  if (["running", "Running", "waiting_external", "In progress", "Encoding", "Calibrating", "Preparing", "Verifying", "Replacing", "Saving candidate"].includes(status)) return "running";
  if (["failed", "Failed", "Worker offline", "No updates"].includes(status)) return "failed";
  if (["review", "waiting_decision", "Needs review", "Needs decision", "partial", "Partial", "Cancelling"].includes(status)) return "waiting_decision";
  if (["Preview", "Preview complete", "Eligible"].includes(status)) return "preview";
  return "neutral";
}
function statusBadge(label, status = label) {
  const result = node("span", "", `badge status-badge ${statusClass(status)}`);
  const icon = node("span", {completed:"✓", running:"●", failed:"!", waiting_decision:"!", preview:"◌", neutral:"–"}[statusClass(status)], "status-symbol");
  icon.setAttribute("aria-hidden", "true");
  result.append(icon, node("span", label));
  return result;
}
async function startBatchPreview(id) {
  if (jobIsBusy(id)) return;
  const auth = state.authRevision;
  const current = () => auth === state.authRevision && !$('workspace').hidden && serverReachable;
  state.busyJobs.add(id);
  setConnection(serverReachable);
  try {
    const read = () => api(`batch-preview?id=${encodeURIComponent(id)}`);
    const plan = await read();
    if (!current()) return;
    if (plan.execution_action_id) return await openJob(plan.execution_action_id);
    if (!plan.eligible) throw new Error("This preview has no eligible files. Review the skipped files or configure a new batch.");
    const content = [node("p", plan.title, "review-filename"), node("p", `${plan.eligible} eligible files will be checked again and encoded.${plan.other > 0 ? ` ${plan.other} skipped or review-only files will stay unchanged.` : ""}`)];
    for (const setting of plan.settings || []) {
      const row = node("div", "", "review-file");
      row.append(node("span", setting.label, "muted"), node("strong", setting.value));
      content.push(row);
    }
    const files = node("ul", "", "preview-file-list");
    for (const file of plan.files || []) files.append(node("li", file));
    content.push(files);
    if (plan.eligible > (plan.files?.length || 0)) content.push(node("p", `+${plan.eligible - plan.files.length} more files from this preview`, "metadata"));
    content.push(node("p", "The original preview stays as a record. Nothing starts until you choose Start batch.", "muted"));
    if (!await reviewAction({title:"Start this preview's batch?", content, confirmLabel:"Start batch"})) return;
    const latest = await read();
    if (!current()) return;
    if (latest.execution_action_id) return await openJob(latest.execution_action_id);
    if (JSON.stringify(plan) !== JSON.stringify(latest)) throw new Error("The preview changed. Review it again before starting.");
    await refreshWorkers();
    if (!current()) return;
    if (!state.workerInfo?.ready) throw new Error("Video worker offline or not ready. No job was submitted.");
    const result = await api("batch-preview", {id});
    if (!current()) return;
    await loadJobs();
    if (!current()) return;
    await openJob(result.id);
    notify("Batch started from the preview. Follow its progress here.");
  } finally {
    state.busyJobs.delete(id);
    setConnection(serverReachable);
  }
}
async function reconfigureBatch(id, candidate = false, savedPlan = null) {
  state.reconfigureScopeRevision=(state.reconfigureScopeRevision || 0)+1;
  const revision = state.reconfigureRevision = (state.reconfigureRevision || 0) + 1;
  const auth = state.authRevision;
  state.reconfigurePlan = null;
  $("reconfigure-form").hidden = true;
  $("reconfigure-files").replaceChildren();
  $("reconfigure-summary").textContent = "Loading saved selection…";
  $("reconfigure-batch").showModal();
  try {
    const candidateID = candidate ? savedPlan?.candidate_id : null;
    let plan = await api(`batch-settings?${new URLSearchParams({id,scope:candidateID ? "candidate" : "unfinished",...(candidateID ? {candidate_id:candidateID} : {})})}`);
    if (!candidateID && plan.selected === 0) plan = await api(`batch-settings?${new URLSearchParams({id,scope:"all"})}`);
    if (auth !== state.authRevision || revision !== state.reconfigureRevision || !$("reconfigure-batch").open) return;
    state.reconfigurePlan = plan;
    $("reconfigure-profile-info").hidden = true;
    $("reconfigure-note").textContent = "Same task · Originals kept · Previous attempts saved";
    $("reconfigure-title").textContent = plan.requires_explicit_profile ? "Try another profile" : "Change settings";
    $("reconfigure-summary").textContent = batchSettingsSummary(plan);
    $("reconfigure-scope").replaceChildren();
    if (plan.candidate_id) option($("reconfigure-scope"),"candidate","This file");
    option($("reconfigure-scope"),"unfinished","Unfinished files");
    option($("reconfigure-scope"),"all","All unreplaced files in this batch");
    $("reconfigure-scope").value = plan.scope;
    for (const file of plan.files) $("reconfigure-files").append(node("p",file));
    if (plan.selected > plan.files.length) $("reconfigure-files").append(node("p",`+${plan.selected-plan.files.length} more files`));
    $("reconfigure-profile").replaceChildren();
    option($("reconfigure-profile"), "", "Choose a profile…");
    option($("reconfigure-profile"), "same", "Keep current settings");
    for (const profile of state.recipes) if (profile !== "auto") option($("reconfigure-profile"),profile,profileOptionLabel(profile));
    $("reconfigure-profile").value = plan.requires_explicit_profile ? "" : "same";
    $("reconfigure-savings").value = plan.settings.min_savings_percent > 0 ? plan.settings.min_savings_percent : "";
    $("reconfigure-growth").value = plan.settings.max_size_increase_percent ?? "";
    state.reconfigureOriginalLimits = batchSettingsLimits();
    $("reconfigure-settings").textContent = `${plan.settings.preserve_source_bit_depth ? "Preserve source bit depth · " : ""}${plan.settings.promote_candidates ? "Ask for replacement approval after conversion" : "Create candidates; keep originals"}`;
    $("reconfigure-form").hidden = false;
    updateBatchSettingsProfile();
  } catch (error) {
    if (auth === state.authRevision && revision === state.reconfigureRevision)
      $("reconfigure-summary").textContent = error.message;
  }
}

function batchSettingsSummary(plan) {
  const selection = plan.selected === 1 && plan.files?.[0] ? plan.files[0] : `${plan.title || "Saved selection"} · ${plan.selected} files`;
  return `${selection}${plan.active > 0 ? ` · ${plan.active} active ${plan.active === 1 ? "file finishes" : "files finish"} first` : ""}`;
}
function updateBatchSettingsProfile() {
  const plan = state.reconfigurePlan;
  if (!plan) return;
  $("reconfigure-scope-label").hidden = plan.selected + plan.kept === 1;
  $("reconfigure-selection").hidden = plan.selected === 1;
  $("reconfigure-savings").placeholder = $("reconfigure-growth").placeholder = plan.settings?.mixed_limits ? "Different per file" : "Default";
  if (plan.requires_explicit_profile && $("reconfigure-profile").value === "same") $("reconfigure-profile").value = "";
  const same = [...$("reconfigure-profile").children].find(option=>option.value === "same");
  if (same) same.disabled = Boolean(plan.requires_explicit_profile);
  $("reconfigure-profile-note").textContent = plan.requires_explicit_profile ? "Automatic testing skipped this video. Choose a profile to try again." : "";
  $("reconfigure-profile-note").hidden = !plan.requires_explicit_profile;
  const currentProfiles = plan.settings?.current_profiles || [];
  $("reconfigure-current-profiles").textContent = currentProfiles.length ? `Saved profile: ${currentProfiles.map(profile=>`${profileOptionLabel(profile.name)}${currentProfiles.length > 1 ? ` (${profile.files} files)` : ""}`).join(" · ")}` : "";
  const profile = $("reconfigure-profile").value;
  $("reconfigure-settings").textContent = !profile ? "Choose a profile before starting another attempt." : profile !== "same" ? `Create and validate ${plan.selected === 1 ? "a candidate for this file" : `candidates for ${plan.selected} files`} with this profile. Originals stay in place.` : "Retry with each file’s current settings. Originals stay in place.";
  $("submit-reconfigure").disabled = !serverReachable || jobIsBusy(plan.id) || plan.selected === 0 || !$("reconfigure-profile").value;
  if (!jobIsBusy(plan.id)) $("submit-reconfigure").textContent = plan.selected === 1 ? "Create new candidate" : plan.selected > 1 ? `Apply to ${plan.selected} files` : "Apply settings";
}
async function showReconfigureProfile() {
  const name = $("reconfigure-profile").value, revision = state.reconfigureRevision, auth = state.authRevision;
  const output = $("reconfigure-profile-info");
  output.hidden = !name || name === "same";
  if (output.hidden) return;
  output.textContent = "Loading profile settings…";
  try {
    const data = state.recipeDetails[name] || await tool("recipe_get", {name});
    if (!$("reconfigure-batch").open || revision !== state.reconfigureRevision || auth !== state.authRevision || $("reconfigure-profile").value !== name) return;
    state.recipeDetails[name] = data;
    output.textContent = "";
    output.replaceChildren();
    renderProfileFacts(output, data, name, state.reconfigurePlan?.settings?.preserve_source_bit_depth);

  } catch {
    if (revision === state.reconfigureRevision && auth === state.authRevision && $("reconfigure-profile").value === name) output.textContent = "Could not load the profile details. Try selecting it again.";
  }
}
function batchSettingsLimits() {
  return JSON.stringify([String($("reconfigure-savings").value),String($("reconfigure-growth").value)]);
}
$("reconfigure-scope").addEventListener("change", () => safe(async () => {
  const plan=state.reconfigurePlan;
  if (!plan) return;
  const revision=state.reconfigureScopeRevision=(state.reconfigureScopeRevision || 0)+1;
  $("submit-reconfigure").disabled=true;
  try {
    const next=await api(`batch-settings?${new URLSearchParams({id:plan.id,scope:$("reconfigure-scope").value,...(plan.candidate_id ? {candidate_id:plan.candidate_id} : {})})}`);
    if (revision!==state.reconfigureScopeRevision || state.reconfigurePlan?.id!==plan.id || !$("reconfigure-batch").open) return;
    state.reconfigurePlan={...next,candidate_id:plan.candidate_id,decision_version:plan.decision_version};
    if (state.reconfigureOriginalLimits === batchSettingsLimits()) {
      $("reconfigure-savings").value = next.settings.min_savings_percent > 0 ? next.settings.min_savings_percent : "";
      $("reconfigure-growth").value = next.settings.max_size_increase_percent ?? "";
      state.reconfigureOriginalLimits = batchSettingsLimits();
    }
    $("reconfigure-summary").textContent=batchSettingsSummary(next);
    $("reconfigure-files").replaceChildren(...next.files.map(file=>node("p",file)));
    if (next.selected>next.files.length) $("reconfigure-files").append(node("p",`+${next.selected-next.files.length} more files`));
    updateBatchSettingsProfile();
  } catch (error) {
    if (revision===state.reconfigureScopeRevision && $("reconfigure-batch").open) {
      $("reconfigure-scope").value=state.reconfigurePlan.scope;
      updateBatchSettingsProfile();
    }
    throw error;
  }
}));
$("close-reconfigure").addEventListener("click", () => $("reconfigure-batch").close());
$("dismiss-reconfigure").addEventListener("click", () => $("reconfigure-batch").close());
$("reconfigure-profile").addEventListener("change", () => {
  updateBatchSettingsProfile();
  safe(showReconfigureProfile);
});
async function submitReconfiguredBatch() {
  const plan = state.reconfigurePlan;
  if (!plan || jobIsBusy(plan.id)) return;
  const revision = state.reconfigureRevision, auth = state.authRevision;
  const current = () => auth === state.authRevision && revision === state.reconfigureRevision && $("reconfigure-batch").open;
  if (!$("reconfigure-profile").value || (plan.requires_explicit_profile && $("reconfigure-profile").value === "same")) throw new Error("Choose a profile to try again.");
  const limit = id => {
    if (!$(id).value.trim()) return null;
    const value = Number($(id).value);
    if (!Number.isFinite(value)) throw new Error("Enter a valid savings or growth limit.");
    return value;
  };
  const body = {id:plan.id,scope:plan.scope,selection_version:plan.selection_version,profile:$("reconfigure-profile").value,min_savings_percent:limit("reconfigure-savings"),max_size_increase_percent:limit("reconfigure-growth")};
  body.preserve_limits = state.reconfigureOriginalLimits === batchSettingsLimits();
  let savedLimits;
  try { savedLimits = JSON.parse(state.reconfigureOriginalLimits); } catch {}
  body.preserve_savings = savedLimits?.[0] === String($("reconfigure-savings").value);
  body.preserve_growth = savedLimits?.[1] === String($("reconfigure-growth").value);
  if (plan.candidate_id) { body.candidate_id = plan.candidate_id; body.decision_version = plan.decision_version; }
  const request = JSON.stringify(body);
  let receipt;
  try { receipt = JSON.parse(sessionStorage.getItem("navigatorr_reconfigure") || "null"); } catch {}
  if (!receipt || receipt.request !== request) {
    receipt = {request,key:submissionID()};
    sessionStorage.setItem("navigatorr_reconfigure",JSON.stringify(receipt));
  }
  state.busyJobs.add(plan.id);
  $("submit-reconfigure").disabled = true;
  try {
    $("submit-reconfigure").textContent = "Applying settings…";
    await commandRequest("batch-settings", {...body,key:receipt.key}, () => {
      sessionStorage.removeItem("navigatorr_reconfigure");
      if (current()) $("reconfigure-batch").close();
    });
    if (auth !== state.authRevision) return;
    await loadJobs();
    notify("Settings updated in this batch. Active files finish first; previous attempts remain in file history.");
  } finally {
    state.busyJobs.delete(plan.id);
    if (current()) updateBatchSettingsProfile();
    setConnection(serverReachable);
  }
}
$("reconfigure-form").addEventListener("submit", event => { event.preventDefault(); safe(submitReconfiguredBatch); });
async function archiveJob(job, archived) {
  if (jobIsBusy(job.id)) return;
  const auth = state.authRevision;
  state.busyJobs.add(job.id);
  try {
    await api("archive", {id:job.id,archived});
    if (auth !== state.authRevision || $("workspace").hidden) return;
    if (state.detailJob?.id === job.id) $("job-detail").close();
    await loadJobs();
    notify(archived ? "Job archived. Find it under Archived to restore it." : "Job restored to the queue.");
  } finally {
    state.busyJobs.delete(job.id);
    setConnection(serverReachable);
  }
}
function jobControls(job, detail = false) {
  const actionRail = node("div", "", "job-actions");
  const add = (label, fn) => {
    const b = button(label, fn);
    if (label === "Start batch") b.className = "primary";
    const icon = actionIcon(label);
    if (icon) b.append(icon);
    b.dataset.jobControl = job.id;
    b.dataset.requiresWorker = String(
      (label === "Retry" && ["transcode_media", "transcode_batch", "benchmark_transcode"].includes(job.action_name) && !(job.action_name === "transcode_batch" && job.current_step >= 2)) ||
      label === "Resume batch" ||
      label === "Start batch" ||
      (job.action_name === "transcode_batch" && !job.batch?.promotion_plan_ready &&
        ["Review candidate", "Review replacements", "Keep originals"].includes(label))
    );
    b.disabled = !serverReachable || jobIsBusy(job.id) ||
      (b.dataset.requiresWorker === "true" && !state.workerInfo?.ready);
    actionRail.append(b);
  };
  if (job.archived) {
    add("Restore", () => archiveJob(job, false));
    return actionRail;
  }
  const decisionLabels = {
    approve: job.promotion ? "Review replacement" : "Review replacements",
    reject: "Keep originals",
    resume: "Resume batch",
    retry: "Retry",
    pause: "Pause batch",
    accept_loss: "Review candidate",
    abort: "Cancel",
  };
  for (const choice of job.waiting_options || []) {
    // Candidate rejection is offered in the review with its frozen file ID and
    // decision version; the parent queue row must not reject an unseen file.
    if (choice.decision === "reject" && job.waiting_options.some(option=>option.decision === "accept_loss")) continue;
    add(
      decisionLabels[choice.decision] || choice.description || choice.decision,
      async () => {
        if (choice.decision === "reject" && job.batch?.promotion_plan_ready) {
          const plan = await readBatchPromotionPlan(job.id);
          if (!plan?.digest) throw new Error("Reopen the replacement review before deciding.");
          await commandRequest("batch-candidates",{id:job.id,digest:plan.digest,decision:"reject"});
          return;
        }
        if (choice.decision === "approve" && job.batch?.promotion_plan_ready) {
          await reviewBatchPromotion(job.id);
          return;
        }
        if (choice.decision === "approve" && job.promotion) {
          await reviewFilePromotion(job.id);
          return;
        }
        if (["approve", "accept_loss"].includes(choice.decision)) {
          await reviewJobDecision(job.id, choice.decision);
          return;
        }
        await jobControl(job.id, "action_resume", {
          decision: choice.decision,
        });
      },
    );
    actionRail.children[actionRail.children.length - 1].title =
      decisionLabels[choice.decision] || choice.description || choice.decision;
  }
  if (job.podcast) {
 add("Ad library", () => viewPodcastAdLibrary(job.podcast.podcast_id));
 if (job.waiting_condition === "podcast_review") { add("Review cuts", () => reviewPodcastCuts(job.id)); add("Revise labels", () => revisePodcastLabels(job.id,job.podcast.cuts_digest)); }
 if (job.waiting_condition === "podcast_classification") { add("View transcript", () => viewPodcastTranscript(job.id)); if (job.podcast.total_blocks > 0 && job.podcast.classified_blocks === job.podcast.total_blocks) add("Validate classifications", () => jobControl(job.id,"action_resume",{decision:"plan"})); }
 }
 if (!job.podcast && job.status === "waiting_decision" && !job.waiting_options?.length)
    add("Review", () => openJob(job.id));
  if (job.status === "completed" && job.batch?.dry_run) {
    if (job.preview_execution_action_id) add("View batch", () => openJob(job.preview_execution_action_id));
    else {
      if (!detail) add("Review preview", () => openJob(job.id));
      if (job.batch.queued > 0) add("Start batch", () => startBatchPreview(job.id));
    }
  }
  if (job.action_name === "transcode_batch" && !job.batch?.dry_run && !job.batch?.promotion_plan_ready && ["completed","failed"].includes(job.status) && (job.remaining_candidates != null ? job.remaining_candidates > 0 : job.batch?.completed > 0 && !job.batch?.promotion?.promoted))
    add("Review candidates", () => reviewBatchCandidates(job.id));
  const reconfigurable = job.action_name === "transcode_batch" && !job.batch?.dry_run && !job.batch?.promotion_plan_ready;
  if (reconfigurable) add(job.batch?.outcome === "no_changes" ? "Try another profile" : "Change settings", () => reconfigureBatch(job.id));
  if (job.status === "failed" && !reconfigurable) {
    if (rejectedCandidate(job)) {
      if (job.parent_action_id) add("Change settings", () => reconfigureBatch(job.parent_action_id, true, {candidate_id:job.id}));
    } else if (job.action_name === "benchmark_transcode" && /unsupported input/.test(job.error || ""))
      add("Set up benchmark", () => {
        $("path").value = job.source_path || "";
        $("scope").value = "file";
        state.fileMedia = null;
        state.fileService = null;
        selectTab("library");
        setFileStep("configure");
        controls();
      });
    else add("Retry", () => jobControl(job.id, "action_retry"));
  }
  if (
    job.action_name === "transcode_batch" &&
    ["running", "waiting_external"].includes(job.status)
  )
    add("Pause batch", () =>
      jobControl(job.id, "action_resume", { decision: "pause" }),
    );
  if (
    ["pending", "running", "waiting_external", "waiting_decision"].includes(
      job.status,
    )
  )
    add("Cancel", async () => {
      if (await reviewAction({title:"Cancel job?", message:`Stop ${queuePresentation(job).title} and its active work.`, confirmLabel:"Cancel job", cancelLabel:"Keep job"}))
        await jobControl(job.id, "action_cancel", {
          reason: "Cancelled from maintenance UI",
        });
    });
  if (
    job.action_name === "transcode_media" &&
    job.status === "completed" &&
    job.candidate_ready &&
    !job.replacement_action_id &&
    state.info.allow_destructive
  )
    add("Replace file", () => prepareReplacement(job));
  if (job.replacement_action_id)
    add("View replacement", () => openJob(job.replacement_action_id));
  if (job.comparison_action_id)
    add("Compare frames", () => openComparison(job));
  if (job.preview_action_id && detail)
    add("View preview", () => openJob(job.preview_action_id));
  if (job.can_archive && ["completed", "failed", "cancelled"].includes(job.status))
    add("Archive", () => archiveJob(job, true));
  return actionRail;
}

async function openComparison(job) {
  const revision = state.comparisonRevision = (state.comparisonRevision || 0) + 1;
  const auth = state.authRevision;
  state.comparison = null;
  state.comparisonAction = job.comparison_action_id;
  $("comparison-file").textContent = queuePresentation(job).title;
  $("comparison-note").textContent = "Loading saved sample frames…";
  $("comparison-controls").hidden = true;
  $("comparison-view").hidden = true;
  $("comparison-metrics").textContent = "";
  $("comparison-frame").replaceChildren();
  $("benchmark-comparison").showModal();
  try {
    const comparison = await api(`benchmark-comparison?id=${encodeURIComponent(job.comparison_action_id)}`);
    if (auth !== state.authRevision || revision !== state.comparisonRevision || !$("benchmark-comparison").open) return;
    if (!Array.isArray(comparison.frames) || !comparison.frames.length) throw new Error("No saved frames for this benchmark.");
    state.comparison = comparison;
    $("comparison-sample").replaceChildren();
    comparison.frames.forEach((frame,index) => option($("comparison-sample"),index,`Sample ${index+1} · ${formatFrameTime(frame.source_seconds)}`));
    $("comparison-sample").value = "0";
    $("comparison-controls").hidden = false;
    showComparisonFrame();
  } catch (error) {
    if (revision === state.comparisonRevision && auth === state.authRevision)
      $("comparison-note").textContent = error.message || "Could not load the comparison. Check the worker connection and reopen it.";
  }
}
function formatFrameTime(seconds) {
  const s = Math.max(0,Number(seconds)||0);
  return `${Math.floor(s/60)}:${String(Math.floor(s%60)).padStart(2,"0")}.${String(Math.floor((s%1)*1000)).padStart(3,"0")}`;
}
function showComparisonFrame() {
  const frame = state.comparison?.frames[Number($("comparison-sample").value)];
  if (!frame) return;
  const revision = state.comparisonRevision = (state.comparisonRevision || 0) + 1;
  const host = $("comparison-frame");
  host.replaceChildren();
  host.className = "comparison-frame";
  host.style?.setProperty("--frame-width",`${frame.width}px`);
  $("comparison-zoom").textContent = "View at 100%";
  $("comparison-zoom").setAttribute("aria-pressed","false");
  $("comparison-slider").value = "50";
  $("comparison-note").textContent = "Loading both images…";
  $("comparison-view").hidden = true;
  $("comparison-metrics").textContent = [
    `${frame.width} × ${frame.height} · Frame ${frame.frame_index} · ${formatFrameTime(frame.source_seconds)}`,
    frame.vmaf != null ? `VMAF ${Number(frame.vmaf).toFixed(2)}` : null,
    frame.ssim != null ? `SSIM ${Number(frame.ssim).toFixed(4)}` : null,
  ].filter(Boolean).join(" · ");
  let loaded = 0, failed = false;
  for (const side of ["original","candidate"]) {
    const img = document.createElement("img");
    img.alt = `${side === "original" ? "Original reference" : "Selected candidate"} at ${formatFrameTime(frame.source_seconds)}`;
    if (side === "candidate") img.className = "comparison-candidate";
    img.addEventListener("load", () => {
      if (revision !== state.comparisonRevision || failed) return;
      if (++loaded === 2) { $("comparison-view").hidden = false; $("comparison-note").textContent = "Drag the slider or use its arrow keys to compare the same frame."; }
    });
    img.addEventListener("error", () => {
      if (revision !== state.comparisonRevision) return;
      failed = true;
      $("comparison-view").hidden = true;
      $("comparison-note").textContent = "Could not load both images. They may have expired, or the worker is disconnected. Reopen the comparison to retry.";
    });
    img.src = `/api/maintenance/benchmark-comparison?${new URLSearchParams({id:state.comparisonAction,image:frame.sample_index,side})}`;
    host.append(img);
  }
  host.append(node("span","","comparison-divider"));
}
$("close-comparison").addEventListener("click", () => $("benchmark-comparison").close());
$("benchmark-comparison").addEventListener("close", () => {
  state.comparisonRevision = (state.comparisonRevision || 0) + 1;
  state.comparison = null;
  $("comparison-frame").replaceChildren();
});
$("comparison-sample").addEventListener("change", showComparisonFrame);
$("comparison-slider").addEventListener("input", () => {
  const value = Math.max(0,Math.min(100,Number($("comparison-slider").value)||0));
  const host = $("comparison-frame");
  host.querySelector(".comparison-candidate").style.clipPath = `inset(0 ${100-value}% 0 0)`;
  host.querySelector(".comparison-divider").style.left = `${value}%`;
});
$("comparison-zoom").addEventListener("click", () => {
  const native = $("comparison-frame").classList.toggle("native-size");
  $("comparison-zoom").textContent = native ? "Fit to view" : "View at 100%";
  $("comparison-zoom").setAttribute("aria-pressed",String(native));
});
function shortJobReason(reason) {
  const text = String(reason || "")
    .replace(/\s+/g, " ")
    .trim();
  const reasons = {
    already_hevc: "Already HEVC; original kept",
    reasonable_size: "Already within the size target; original kept",
    not_oversized: "Already within the size target; original kept",
    below_min_savings: "Savings below the minimum; original kept",
    "10bit": "10-bit source requires review",
  };
  if (reasons[text]) return reasons[text];
  if (isCandidateRejection(text)) return "Candidate rejected by you; original kept";
  const growth = text.match(/Candidate file size \(\d+ bytes\) exceeds original \(\d+ bytes\) by ([\d.]+)%, which is greater than max_size_increase_percent \(([\d.]+)%\)/);
  if (growth) return `Candidate is ${growth[1]}% larger; allowed growth is ${growth[2]}%`;
  if (/no such file or directory/i.test(text)) return "File not found";
  if (/permission denied/i.test(text)) return "Permission denied";
  if (/unsupported input.*benchmark_transcode/.test(text)) return "Unsupported benchmark settings. Set up a new benchmark.";
  if (/smb_transport_error|connecting to SMB server/.test(text)) return "Worker cannot access media storage. Fix the connection before retrying.";
  if (/shared VMAF\/CAMBI calibration supports 8-bit SDR video below 45 fps/.test(text))
    return "Automatic testing does not support this video format";
  return text.length > 140 ? `${text.slice(0, 137)}…` : text;
}
function workerUnavailable(job) {
  return job.waiting_condition === "worker_unreachable" || job.activity_waiting_condition === "worker_unreachable";
}
function isCandidateRejection(reason) {
  return String(reason || "").includes("transcode candidate rejected by user decision");
}
function rejectedCandidate(job) {
  return job.status === "failed" && isCandidateRejection(job.error);
}
function savingsPolicyFailure(value) {
  return /benchmark winner predicts only -?\d+(?:\.\d+)?% savings, below required minimum \d+(?:\.\d+)?%|Full conversion not started; original kept/i.test(value || "");
}
function visibleBatch(job) {
  if (!job.batch) return null;
  const rejected = Math.min(job.batch.failed || 0, job.batch_files?.rejected_count ?? (job.batch_files?.reasons || []).filter(reason=>isCandidateRejection(reason.reason)).reduce((count,reason)=>count+reason.count,0));
  const policy = Math.min(Math.max(0,(job.batch.failed || 0)-rejected),job.batch_files?.policy_count || 0);
  const failed = (job.batch.failed || 0)-rejected-policy;
  return {...job.batch,failed,rejected,policy,outcome:rejected > 0 && rejected===job.batch.total && job.status === "completed" ? "rejected" : policy > 0 && policy===job.batch.total ? "minimum_savings" : policy > 0 && failed === 0 && job.batch.outcome === "failed" ? "no_changes" : job.batch.outcome};
}
function queuePresentation(job) {
  if (job.podcast) { const p=job.podcast; return {kind:"Podcast",title:job.source_path?.split("/").pop() || p.podcast_id || "Podcast",status:job.waiting_condition === "podcast_classification" ? "Awaiting LLM" : job.waiting_condition === "podcast_review" ? "Review cuts" : names[job.status] || job.status,summary:job.status === "failed" ? shortJobReason(job.error) : `${p.classified_blocks || 0}/${p.total_blocks || 0} blocks · ${((p.removed_ms || 0)/1000).toFixed(1)} seconds removed · Original kept`,showTelemetry:false}; }
 const batch = visibleBatch(job);
  const replacement = job.action_name === "promote_transcode_candidate";
  const kind = job.batch
    ? job.batch.total === 1 ? "File" : "Batch"
    : job.action_name === "promote_transcode_candidate"
      ? "Replacement"
      : job.action_name === "benchmark_transcode"
        ? "Benchmark"
        : "File";
  const source = job.source_path || job.promotion?.original_path;
  const context = job.batch_files?.context || job.batch?.title;
  const singleFile = job.batch_files?.file_count === 1 ? job.batch_files.files?.[0] : null;
  const title =
    singleFile?.display_label || singleFile?.file_path?.split("/").pop() || context ||
    source?.split("/").pop() ||
    workflows[job.action_name] ||
    job.action_name;
  const outcomes = {
    preview: "Preview",
    failed: "Failed",
    minimum_savings: "Minimum savings not met",
    cancelled: "Cancelled",
    needs_decision: "Needs decision",
    partial: "Partial",
    needs_review: "Needs review",
    no_changes: "Not converted",
    rejected: "Rejected",
    candidates_ready: "Candidates ready",
    promoted: "Replaced",
    cancelling: "Cancelling",
  };
  const phase = job.worker?.transcode_phase || job.worker?.benchmark_phase || job.worker?.phase;
  const multipleActive = job.activities?.length > 1;
  const offline = multipleActive
    ? job.activities.every(activity => activity.waiting_condition === "worker_unreachable")
    : workerUnavailable(job);
  const replacementComplete = job.savings?.realized_saved_bytes != null ||
    (job.replaced && job.status === "completed");
  const status = job.status === "failed" && !rejectedCandidate(job) && !savingsPolicyFailure(job.error) ? "Failed" : job.replaced_files > 0 && job.remaining_candidates > 0 ? "Candidates ready" : replacementComplete
    ? "Replaced"
    : rejectedCandidate(job) ? "Rejected" : savingsPolicyFailure(job.error) ? "Minimum savings not met" : outcomes[batch?.outcome] ||
      (job.candidate_ready && !job.replacement_action_id
        ? "Candidate ready"
        : ["running", "waiting_external"].includes(job.status)
          ? (offline ? "Worker offline" : multipleActive ? "In progress" : job.worker?.progress_is_stale ? "No updates" : {encoding:"Encoding",validating:"Verifying",publishing:"Saving candidate",benchmarking:"Calibrating",queued:"Queued on worker",preparing:"Preparing"}[phase] || (replacement || job.promotion ? "Replacing" : "In progress"))
          : names[job.status] || job.status);
  const s = job.savings || {},
    metrics = [];
  if (s.source_bytes != null) {
    metrics.push(
      s.candidate_bytes != null && !job.replaced
        ? `${bytes(s.source_bytes)} → ${bytes(s.candidate_bytes)}`
        : `${bytes(s.source_bytes)} source`,
    );
  }
  if (s.realized_saved_bytes != null)
    metrics.push(`${bytes(s.realized_saved_bytes)} freed`);
  else if (!job.replaced && !rejectedCandidate(job) && batch?.outcome !== "rejected" && s.candidate_saved_bytes != null)
    metrics.push(
      s.candidate_saved_bytes < 0
        ? `${bytes(-s.candidate_saved_bytes)} larger`
        : `${bytes(s.candidate_saved_bytes)} potential savings`,
    );
  else if (!job.replaced && !rejectedCandidate(job) && batch?.outcome !== "rejected" && s.estimated_saved_bytes != null)
    metrics.push(`${bytes(s.estimated_saved_bytes)} estimated savings`);
  let result;
  if (job.status === "failed")
    result = shortJobReason(job.error) || "Error details unavailable";
  else if (job.replaced && job.status === "completed") result = "Original replaced";
  else if (job.batch?.promotion_plan_ready)
    result = "Replacement approval required";
  else if (job.batch) result = batchCounts(batch, true);
  else if (job.status === "waiting_decision")
    result = job.promotion ? "Review replacement before files change" : shortJobReason(job.waiting_reason) || "Review required";
  else if (job.status === "cancelled") result = "Stopped";
  if (job.workflow_actions?.length && replacement && job.status !== "failed") {
    result = `Conversion complete · ${job.savings?.realized_saved_bytes != null ? "Replacement verified" : result || "Replacement in progress"}`;
  }
  if (job.replaced_files > 0) result += ` · ${job.replaced_files} replaced${job.remaining_candidates > 0 ? ` · ${job.remaining_candidates} candidates available` : ""}`;
  if (job.batch?.dry_run) result += " · Originals unchanged";
  else if (job.batch?.outcome === "no_changes") result += " · Originals kept";
  const summary = [kind, result, metrics.join(" · ")]
    .filter(Boolean)
    .join(" · ");
  return {
    title,
    context,
    status,
    summary,
    showTelemetry:
      ["running", "waiting_external"].includes(job.status),
  };
}
function compactQueueActions(actions) {
  for (const b of actions.children) {
    const icon = b.querySelector?.("svg");
    if (!icon) continue;
    const label = b.textContent.trim();
    if (["Set up benchmark", "Review candidates", "Review candidate", "Review replacement", "Review replacements", "Replace file", "Start batch", "Review preview", "View batch", "View preview", "Archive", "Restore", "Reconfigure", "Change settings", "Try another profile"].includes(label)) {
      b.replaceChildren(icon, node("span", label === "Replace file" ? "Replace" : label === "Set up benchmark" ? "Set up" : ["Start batch", "View batch", "View preview", "Archive", "Restore", "Reconfigure", "Change settings", "Try another profile"].includes(label) ? label : "Review"));
      b.className += ["Review preview", "View batch", "View preview", "Archive", "Restore", "Reconfigure", "Change settings"].includes(label) ? " queue-secondary" : " primary queue-primary";
      b.setAttribute("aria-label", label);
      b.title ||= label;
      continue;
    }
    b.setAttribute("aria-label", label);
    b.title ||= label;
    b.replaceChildren(node("span", label, "sr-only"), icon);
    b.className += " action-control";
    if (
      ["Replace file", "Review replacements", "Resume batch"].includes(label)
    ) {
      b.className += " primary";
    } else if (label === "Retry") b.className += " recovery";
    else if (["Cancel", "Pause batch"].includes(label))
      b.className += " dismissive";
  }
}
function queueOutcome(job) {
  if (job.archived) return "Archived · Restore to return to the queue";
  const batch = visibleBatch(job);
  if (rejectedCandidate(job) || batch?.outcome === "rejected") return "Candidate rejected · Original preserved";
  if (job.status === "waiting_decision") {
    if (job.paused || job.waiting_options?.some(option => option.decision === "resume")) return "Paused · Resume to continue the remaining files";
    if (job.batch?.promotion_plan_ready || job.promotion) return "Candidates verified · Waiting for replacement approval · Originals unchanged";
    return `${shortJobReason(job.waiting_reason) || "A candidate needs your decision"} · Originals unchanged`;
  }
  if (batch?.policy === batch?.total && batch?.policy > 0 || savingsPolicyFailure(job.error)) return "Sample test did not meet the savings requirement · Full conversion not started · Originals kept";
  if (job.status === "failed") return "Open job for error details";
  if (job.status === "cancelled") return "Stopped by user";
  if (job.status === "pending") return "Waiting to start";
  if (job.replaced_files > 0 && job.remaining_candidates > 0) return `${job.replaced_files} replacements verified · ${job.remaining_candidates} candidates still available; their originals remain in place`;
  if (job.savings?.realized_saved_bytes != null || job.replaced) return "Replacement verified";
  if (job.batch?.dry_run) return job.preview_execution_action_id ? "Batch started · View its progress" : job.batch.queued > 0 ? "Preview finished · Nothing starts automatically" : "Preview finished · No eligible files";
  if (job.batch?.outcome === "no_changes") return "Originals preserved";
  if (batch?.outcome === "failed") return "No candidates created · Change settings to try again";
  if (batch?.completed > 0) return `${batch.completed} ${batch.completed === 1 ? "candidate ready" : "candidates ready"} · Originals unchanged`;
  if (job.candidate_ready) return "Original preserved · Candidate available";
  if (job.action_name === "benchmark_transcode") return "Sample test complete · Original preserved";
  return "Finished · Open job for details";
}
function skippedBatchOutcome(job) {
  const result = node("div", "", "skipped-outcome");
  const reasons = job.batch_files?.reasons || [];
  const reason = reasons[0];
  const total = job.batch?.total || 0;
  const text = reason ? shortJobReason(reason.reason) : "No conversion was started";
  result.append(node("p", `${reason && reason.count < total ? `${reason.count} of ${total} files: ` : ""}${text}`, "skipped-reason"));
  result.append(node("p", "Choose another profile to try again. Your originals are unchanged.", "metadata"));
  return result;
}
async function loadJobs(more = false) {
  if (
    $("workspace").hidden ||
    (more && (state.jobsLoading || !state.jobsHasMore))
  )
    return;
  const revision = ++state.jobsRevision;
  state.jobsLoading = true;
  if (state.tab === "jobs" && (!state.activityCheckedAt || Date.now() - state.activityCheckedAt > 5000)) void refreshWorkerActivity();
  $("jobs-more").disabled = true;
  const status = $("job-filter").value;
  const offset = more ? state.jobsLoaded || 0 : 0;
  const target = more ? 25 : Math.max(25, state.jobsLoaded || 25);
  let data,
    page = [];
  try {
    // The server bounds each page to 100. Refresh the complete visible window.
    while (page.length < target) {
      data = await api(
        `operations?${new URLSearchParams({ group: "workflow", status, limit: Math.min(100, target - page.length), offset: offset + page.length })}`,
      );
      if (revision !== state.jobsRevision || $("workspace").hidden) return;
      if (!Array.isArray(data.jobs))
        throw new Error("Unexpected jobs response.");
      page.push(...data.jobs);
      if (!data.has_more || !data.jobs.length) break;
    }
    renderSavings(data);
    if (!more) state.operationJobs = new Map();
    state.operationJobs ||= new Map();
    for (const job of page) state.operationJobs.set(job.workflow_id || job.id, job);
    state.jobsLoaded = offset + page.length;
    state.jobsHasMore = data.has_more;
    const focus = focusedControl($("jobs-list"));
    $("jobs-list").replaceChildren();
    for (const job of state.operationJobs.values()) {
      const row = node("article", "", "job-row"),
        meta = node("div", "", "job-meta");
      row.dataset.operationId = job.id;
      if (job.batch) row.className += " batch-job";
      if (job.batch?.dry_run) row.className += " preview-job";
      if (["completed", "failed", "cancelled"].includes(job.status)) row.className += " terminal-job";
      const presentation = queuePresentation(job);
      const skipped = job.batch?.outcome === "no_changes" && !job.batch?.dry_run;
      if (skipped) row.className += " skipped-job";
      const singleFile = job.batch_files?.file_count === 1 ? job.batch_files.files?.[0] : null;
      const heading = node("h3", "", "job-heading");
      heading.append(node("span", job.number ? `#${job.number}` : "", "job-number"));
      heading.append(
        button(singleFile?.display_label || presentation.title, () => openJob(job.id), "job-title"),
      );
      heading.append(statusBadge(presentation.status));
      const summaryText = skipped ? [singleFile ? presentation.context : `${job.batch.total} files`, job.batch.total === 1 ? "Original kept" : "Originals kept"].filter(Boolean).join(" · ") : presentation.summary;
      const summary = node("p", summaryText, "metadata job-summary");
      summary.title = summaryText;
      meta.append(summary);
      if (!skipped && !singleFile && job.batch_files?.files?.length) {
        const files = node("p", job.batch_files.files.map(file => file.display_label || file.file_path?.split("/").pop()).join(" · ") + (job.batch_files.file_count > 2 ? ` · +${job.batch_files.file_count - 2} more` : ""), "metadata batch-file-preview");
        files.title = files.textContent;
        meta.append(files);
      }
      for (const reason of (skipped ? [] : job.batch_files?.reasons || []).slice(0, 1)) {
        const text = shortJobReason(reason.reason);
        const count = batchReasonLabel(job, reason);
        const line = node("p", `${count}: ${text}`, "metadata batch-reason");
        line.title = reason.reason;
        meta.append(line);
      }
      if (job.batch?.dry_run) meta.append(node("p", queueOutcome(job), "metadata preview-note"));
      const actions = jobControls(job);
      if (job.parent_action_id) {
        const parent = button(
          "View batch",
          () => openJob(job.parent_action_id),
          "quiet",
        );
        const icon = actionIcon("View batch");
        if (icon) parent.append(icon);
        actions.append(parent);
      }
      compactQueueActions(actions);
      row.append(heading, meta);
      const footer = node("div", "", "job-footer");
      footer.append(skipped ? skippedBatchOutcome(job) : presentation.showTelemetry ? telemetry(job) : batchProgress(job) || node("div", queueOutcome(job), "metadata job-outcome"));
      if (actions.children.length) {
        if (actions.children.length > 1)
          actions.classList.add("multiple-actions");
        footer.append(actions);
      }
      row.append(footer);
      $("jobs-list").append(row);
    }
    if (!state.operationJobs.size)
      $("jobs-list").append(node("p", "No jobs for this filter.", "empty"));
    renderCommandIndicators();
    restoreControl($("jobs-list"), focus);

    $("jobs-more").hidden = !data.has_more;
    $("jobs-page").textContent =
      `${state.operationJobs.size} of ${data.total} jobs`;
    $("active-count").textContent = data.active_count
      ? `(${data.active_count})`
      : "";
    $("jobs-updated").textContent =
      `Updated ${new Date().toLocaleTimeString("en")} · live`;
  } finally {
    if (revision === state.jobsRevision) {
      state.jobsLoading = false;
      $("jobs-more").disabled = false;
    }
  }
}

function workerPhaseLabel(phase) {
  return {
    queued:"Waiting for a worker slot",preparing:"Preparing source",reading_source:"Reading source from media storage",
    probing_source:"Inspecting source",extracting_samples:"Extracting reference samples",encoding_samples:"Encoding test samples",
    evaluating_metrics:"Measuring sample quality",estimating_final_size:"Estimating final file size",selecting_candidate:"Selecting the best candidate",
    capturing_comparison:"Saving comparison frames",encoding:"Encoding full video",validating:"Verifying output",publishing:"Saving candidate to media storage",
    benchmarking:"Calibrating quality",completed:"Completed",
  }[phase] || (phase ? phase.replaceAll("_"," ") : "Preparing job");
}
function activityElapsed(value) {
  const at = Date.parse(value || "");
  if (!Number.isFinite(at) || at < Date.UTC(2000,0,1)) return null;
  const seconds = Math.max(0,Math.floor((Date.now()-at)/1000));
  return seconds >= 60 ? `${Math.floor(seconds/60)}m ${seconds%60}s` : `${seconds}s`;
}
function renderWorkerActivity(data) {
  $("worker-slots").replaceChildren();
  if (!serverReachable || !data?.available) {
    $("worker-capacity").textContent = "Worker slots · Unknown";
    $("activity-checked").textContent = "";
    $("worker-slots").append(node("p",data?.message || "Worker disconnected; slot occupancy is unknown.","metadata"));
    return;
  }
  const activity = data.activity;
  $("worker-capacity").textContent = `Worker slots · ${activity.worker_slots_used}/${activity.worker_slots_total} occupied`;
  $("activity-checked").textContent = "Updated now · Job reservations";
  const jobs = activity.jobs || [];
  // Keep a live reservation in the same visual slot as other jobs finish.
  // Slot labels describe this node's reservations, not physical CPU cores.
  const assigned = state.activitySlotIDs ||= new Map();
  const live = new Set(jobs.map(job => job.id));
  for (const [id,slot] of assigned) if (!live.has(id) || slot >= activity.worker_slots_total) assigned.delete(id);
  for (const job of jobs) if (!assigned.has(job.id)) {
    const used = new Set(assigned.values());
    for (let slot=0;slot<Math.min(16,activity.worker_slots_total);slot++) if (!used.has(slot)) { assigned.set(job.id,slot);break; }
  }
  for (let i=0;i<Math.min(16,activity.worker_slots_total);i++) {
    const row = node("div","","worker-slot"), job = jobs.find(job => assigned.get(job.id) === i);
    if (!job) {
      const unknownOccupied = !activity.details_available && i < activity.worker_slots_used;
      row.append(node("strong",`Slot ${i+1} · ${unknownOccupied ? "Occupied" : "Available"}`));
      row.append(node("p",unknownOccupied ? "File details require the updated video worker." : "Ready for the next job.","metadata"));
    } else {
      const stale = !activityElapsed(job.heartbeat_at) || Date.now()-Date.parse(job.heartbeat_at)>30000;
      row.append(node("strong",`Slot ${i+1} · ${job.file}`));
      row.children[0].title = job.file;
      const phase = workerPhaseLabel(job.phase);
      const parts = [job.kind === "benchmark" ? "Sample benchmark" : "Full conversion",phase,activityElapsed(job.started_at) ? `${activityElapsed(job.started_at)} elapsed` : null];
      if (job.progress != null) parts.push(`${Number(job.progress).toFixed(1)}%${job.kind === "benchmark" ? " of sample work" : ""}`);
      row.append(node("p",parts.filter(Boolean).join(" · "),"metadata"));
      const detail = job.details;
      if (detail?.total_units>0) row.append(node("p",`${detail.completed_units}/${detail.total_units} sample tasks resolved${detail.sample_number ? ` · Latest sample ${detail.sample_number}` : ""}${detail.candidate_number ? ` · Candidate ${detail.candidate_number}` : ""}${detail.metric ? ` · ${detail.metric.toUpperCase()}` : ""}`,"metadata"));
      const last = activityElapsed(job.last_progress_at);
      row.append(node("p",stale ? "No recent worker heartbeat. Last reported state." : last ? `Last advance ${last} ago` : "Worker connected · Waiting for the next measurement", "metadata"));
      if (job.phase === "evaluating_metrics") row.append(node("p","Quality measurement compares the sample frames and can take longer than encoding them.","metadata"));
    }
    $("worker-slots").append(row);
  }
}
async function refreshWorkerActivity() {
  if (state.activityRequest || $("workspace").hidden) return;
  const auth = state.authRevision;
  state.activityRequest = true;
  try {
    const data = await api("worker-activity");
    if (auth !== state.authRevision || $("workspace").hidden) return;
    state.activityCheckedAt = Date.now();
    renderWorkerActivity(data);
  } catch {
    if (auth === state.authRevision) renderWorkerActivity(null);
  } finally { state.activityRequest = false; }
}
$("refresh-jobs").addEventListener("click", () => safe(() => loadJobs()));
$("job-filter").addEventListener("change", () => {
  state.jobsLoaded = 0;
  state.jobsHasMore = false;
  writeNavigation();
  safe(() => loadJobs());
});
$("jobs-more").addEventListener("click", () => safe(() => loadJobs(true)));
if (typeof IntersectionObserver !== "undefined") {
  new IntersectionObserver(
    (entries) => {
      if (
        entries.some((e) => e.isIntersecting) &&
        state.tab === "jobs" &&
        serverReachable
      )
        safe(() => loadJobs(true));
    },
    { rootMargin: "200px" },
  ).observe($("jobs-sentinel"));
}
function renderPhaseCosts(host, job) {
  const entries = Object.entries(job.phase_costs || {});
  const search = job.worker?.benchmark_search_seconds;
  const sourceBudget = job.worker?.search_budget_seconds;
  const operationBudget = job.worker?.operation_search_budget_seconds;
  if (!entries.length && search == null && sourceBudget == null && operationBudget == null) return;
  const detail = node("details", "", "phase-costs");
  detail.append(node("summary", "Measured phase costs"));
  detail.append(node("p", "Elapsed wall measurements include synchronous I/O. Coordinator measurements count returned invocations and exclude waits between them. CPU time is unknown; phases may overlap and are not added into a total.", "metadata"));
  if (search != null || sourceBudget != null || operationBudget != null) detail.append(node("p", `Sample search measured wall: ${search == null ? "Unknown" : search + " s"} · Source budget: ${sourceBudget == null ? "Unknown" : sourceBudget + " s"} · Operation budget: ${operationBudget == null ? "Unknown" : operationBudget + " s"}. Pending reservations are not measured consumption.`, "metadata"));
  const table = node("table");
  const header = node("tr");
  for (const label of ["Phase", "Wall time", "Invocations", "NAS read", "NAS written", "Cache hits / misses"]) header.append(node("th", label));
  table.append(header);
  const labels = {coordinator_inventory:"Coordinator · Inventory",coordinator_preflight:"Coordinator · Preflight",coordinator_source_hash:"Coordinator · Source hash",coordinator_probe:"Coordinator · Probe",coordinator_promotion:"Coordinator · Promotion"};
  for (const [phase, cost] of entries) {
    const row = node("tr");
    const label = labels[phase] || phase.replace(/^worker_benchmark_/, "Worker samples · ").replace(/^worker_/, "Worker · ").replaceAll("_", " ");
    row.append(node("td", label), node("td", Number.isFinite(cost.duration_ms) ? `${cost.duration_ms} ms` : "Unknown"), node("td", Number.isFinite(cost.attempts) ? String(cost.attempts) : "Unknown"), node("td", cost.nas_read_bytes == null ? "Unknown" : bytes(cost.nas_read_bytes)), node("td", cost.nas_written_bytes == null ? "Unknown" : bytes(cost.nas_written_bytes)), node("td", `${cost.cache_hits == null ? "Unknown" : cost.cache_hits} / ${cost.cache_misses == null ? "Unknown" : cost.cache_misses}`));
    table.append(row);
  }
  detail.append(table); host.append(detail);
}
async function openJob(id) {
  state.detail = id;
  state.chunk = 0;
  state.stepOffset = 0;
  state.batchItemsOffset = 0;
  state.batchItemsPaging = null;
  state.batchItemsHasMore = false;
  $("batch-items-panel").hidden = true;
  state.detailRevision++;
  if (!$("job-detail").open) $("job-detail").showModal();
  writeNavigation();
  $("detail-summary").replaceChildren(node("p", "Loading job…", "muted"));
  $("detail-controls").replaceChildren();
  $("detail-stages").replaceChildren();
  $("detail-related").replaceChildren();
  $("detail-content").textContent = "";
  $("detail-content").hidden = true;
  await refreshDetail();
}
async function refreshDetail() {
  const id = state.detail,
    revision = ++state.detailRevision;
  try {
    const data = await api(`operations?id=${encodeURIComponent(id)}`),
      job = data.jobs?.[0];
    if (!job) throw new Error("This job is no longer available.");
    if (
      state.detail !== id ||
      revision !== state.detailRevision ||
      !$("job-detail").open
    )
      return;
    $("load-detail").hidden = !job.logs_available;
    state.detailJob = job;
    const focus = focusedControl($("detail-controls"));
    const summary = $("detail-summary");
    summary.replaceChildren(
      node(
        "p",
        job.batch || job.source_path || job.promotion ? queuePresentation(job).title : job.id,
        "metadata",
      ),
      statusBadge(queuePresentation(job).status),
      node("p", queuePresentation(job).summary, "muted"),
    );
    if (job.podcast) { const p=job.podcast; summary.append(node("p",`${p.classified_blocks || 0}/${p.total_blocks || 0} transcript blocks classified · ${((p.removed_ms || 0)/1000).toFixed(1)} seconds removed · ${p.known_ad_units || 0} units verified by known ads`,"metadata")); if (p.output_path && job.status === "completed") summary.append(node("p",`Validated MP3: ${p.output_path}`,"metadata")); }
 renderPhaseCosts(summary, job);
    if (queuePresentation(job).showTelemetry) summary.append(telemetry(job));
    else if (batchProgress(job)) summary.append(batchProgress(job));
    else if (!job.batch?.dry_run && job.batch?.outcome !== "no_changes") summary.append(node("p", queueOutcome(job), "job-outcome metadata"));
    if (job.batch?.outcome === "no_changes") summary.append(skippedBatchOutcome(job));
    if (job.batch?.dry_run) summary.append(node("p", queueOutcome(job) + (job.batch.queued > 0 && !job.preview_execution_action_id ? ". Review the eligible files, then choose Start batch to convert them." : "."), "preview-notice"));
    if (job.batch?.total > 1 && !job.batch.dry_run) renderBatchCounts(summary, visibleBatch(job));
    if (visibleBatch(job)?.failed && job.batch_files?.reasons?.length) {
      const reason = job.batch_files.reasons[0];
      summary.append(node("p", `${batchReasonLabel(job,reason)}: ${shortJobReason(reason.reason)}`, "muted"));
    }
    summary.dataset.operationId = job.id;
    renderCommandIndicators();
    renderStages(job);
    const related = $("detail-related");
    related.replaceChildren();
    for (const action of job.workflow_actions || []) {
      if (action.id === job.id) continue;
      related.append(button(`${workflows[action.action_name] || action.action_name} · ${names[action.status] || action.status}`, () => openJob(action.id), "quiet"));
    }
    if (job.source_path) summary.append(node("p", job.source_path, "metadata"));
    if (job.promotion) {
      const location = job.promotion.new_path || job.promotion.original_path;
      if (location) summary.append(node("p", `Library: ${location.slice(0, location.lastIndexOf("/"))}`, "metadata"));
    }
    const controls = $("detail-controls");
    controls.replaceChildren(jobControls(job, true));
    $("batch-items-panel").hidden = !job.batch;
    if (job.batch) await loadBatchItems(id, revision, job.batch.total);
    restoreControl(controls, focus);
  } catch (error) {
    if (
      state.detail === id &&
      revision === state.detailRevision &&
      $("job-detail").open
    ) {
      $("detail-summary").replaceChildren(
        node("p", error.message || "Could not load this job.", "muted"),
        button("Retry", refreshDetail, "quiet"),
      );
      $("detail-controls").replaceChildren();
      $("detail-stages").replaceChildren();
      $("detail-related").replaceChildren();
      $("batch-items-panel").hidden = true;
      $("load-detail").hidden = true;
    }
  }
}
function batchFileActivity(item, job) {
  if (job?.batch?.dry_run) return "";
  if (item.retry_pending) return "New settings saved · Current attempt finishes before the next attempt starts";
  if (item.status === "queued" && !item.child_action_id) {
    if (job?.paused) return "Batch paused · Resume the batch to start this file";
    if (job?.status === "waiting_decision") return "Waiting for the batch decision before starting this file";
    if (["failed", "cancelled"].includes(job?.status)) return "Batch stopped · This file has not started";
    return "Waiting for batch scheduling · Not sent to the worker yet";
  }
  const activity = job?.activities?.find(activity => activity.id === item.child_action_id);
  if (activity?.waiting_condition === "worker_unreachable") return "Worker unreachable · Waiting for connection";
  const worker = activity?.worker;
  const phase = worker?.transcode_phase || worker?.benchmark_phase || worker?.phase;
  if (phase) return workerPhaseLabel(phase) + (worker.progress_is_stale ? " · Last measurement; stale" : "");
  if (activity?.work?.phase === "hashing_source") return `Verifying source${activity.work.total_bytes > 0 ? ` · ${bytes(activity.work.bytes_read || 0)} / ${bytes(activity.work.total_bytes)}` : ""}`;
  if (activity?.work?.phase) return workerPhaseLabel(activity.work.phase);
  if (item.status === "waiting_for_slot") return "Waiting for an available worker slot";
  if (item.status === "queued" && item.child_action_id) return "File task created · Waiting to start";
  return "";
}

async function loadBatchItems(id, revision) {
  const offset = state.batchItemsOffset;
  const response = await api(
    `batch-items?${new URLSearchParams({ id, offset, limit: 25 })}`,
  );
  if (
    id !== state.detail ||
    revision !== state.detailRevision ||
    offset !== state.batchItemsOffset ||
    !$("job-detail").open
  )
    return;
  $("batch-items-list").replaceChildren();
  for (const item of response.items || []) {
    const row = node("div", "", "batch-item-row");
    row.append(
      node("span", item.display_label || item.file_path, "metadata"),
      statusBadge(savingsPolicyFailure(item.error) ? "Below minimum savings" : rejectedCandidate(item) ? "Rejected" : state.detailJob?.batch?.dry_run && item.status === "queued" ? "Eligible" : names[item.status] || item.status || item.decision, savingsPolicyFailure(item.error) ? "neutral" : rejectedCandidate(item) ? "rejected" : state.detailJob?.batch?.dry_run && item.status === "queued" ? "Eligible" : item.status),
    );
    if (item.child_action_id)
      row.append(
        button("View file", () => openJob(item.child_action_id), "quiet"),
      );

    const activity = batchFileActivity(item, state.detailJob);
    if (activity) row.append(node("span", activity, "batch-file-activity"));
    if (item.requested_profile || item.profile) row.append(node("span", `${item.retry_pending ? "Next attempt" : "Profile"}: ${profileOptionLabel(item.requested_profile || item.profile)}`, "metadata batch-file-profile"));
    if (item.previous_attempts?.length) {
      const history=node("details","","batch-attempt-history");
      history.append(node("summary",`${item.previous_attempts.length} previous ${item.previous_attempts.length===1 ? "attempt" : "attempts"}`));
      item.previous_attempts.forEach((attempt,index)=>history.append(button(`Attempt ${index+1} · ${names[attempt.status] || attempt.status}`,()=>openJob(attempt.child_action_id),"quiet")));
      row.append(history);
    }
    for (const reason of [item.error, ...(item.reasons || [])].filter(reason => reason && !/^(explicit profile requested|eligible for transcode|New settings requested; previous attempt retained in history)$/i.test(reason)))
      row.append(node("span", shortJobReason(reason), "muted"));
    $("batch-items-list").append(row);
  }
  $("batch-items-note").textContent =
    `${response.items?.length || 0} of ${response.total} files`;
  state.batchItemsHasMore = response.has_more;
  $("batch-items-prev").disabled = offset === 0;
  $("batch-items-next").disabled = !response.has_more;
  $("batch-items-prev").hidden = offset === 0 && !response.has_more;
  $("batch-items-next").hidden = offset === 0 && !response.has_more;
}
async function changeBatchPage(delta) {
  if (state.batchItemsPaging || !state.detail) return;
  if (delta > 0 && !state.batchItemsHasMore) return;
  if (delta < 0 && state.batchItemsOffset === 0) return;
  const id = state.detail,
    revision = state.detailRevision;
  const previous = state.batchItemsOffset;
  const paging = { id, revision };
  state.batchItemsPaging = paging;
  state.batchItemsOffset = Math.max(0, previous + delta);
  $("batch-items-prev").disabled = true;
  $("batch-items-next").disabled = true;
  try {
    await loadBatchItems(id, revision);
  } catch (error) {
    if (state.detail === id && revision === state.detailRevision)
      state.batchItemsOffset = previous;
    throw error;
  } finally {
    if (state.batchItemsPaging === paging) {
      state.batchItemsPaging = null;
      if (state.detail === id && revision === state.detailRevision) {
        $("batch-items-prev").disabled = state.batchItemsOffset === 0;
        $("batch-items-next").disabled = !state.batchItemsHasMore;
      }
    }
  }
}
$("batch-items-prev").addEventListener("click", () =>
  safe(() => changeBatchPage(-25)),
);
$("batch-items-next").addEventListener("click", () =>
  safe(() => changeBatchPage(25)),
);
async function loadDetail() {
  const id = state.detail;
  const data = await api(`logs?id=${encodeURIComponent(id)}`);
  if (state.detail !== id || !$("job-detail").open) return;
  showData(
    "detail-content",
    [data.stdout_tail, data.stderr_tail].filter(Boolean).join("\n") ||
      "No worker output yet.",
  );
}

function renderBatchCounts(container, batch) {
  const counts = node("div", "", "batch-counts");
  if (batch.policy > 0) counts.append(statusBadge(`${batch.policy} Below minimum savings`, "neutral"));
  if (batch.rejected > 0) counts.append(statusBadge(`${batch.rejected} Rejected`, "rejected"));
  for (const [key, label, status] of [["completed","Completed","completed"],["running","Active","running"],["queued","Queued","queued"],["waiting_for_slot","Waiting for slot","queued"],["waiting_decision","Needs decision","waiting_decision"],["review","Needs review","review"],["failed","Failed","failed"],["skip","Skipped","skip"],["cancelled","Cancelled","cancelled"]]) {
    if (batch[key] > 0) counts.append(statusBadge(`${batch[key]} ${label}`, status));
  }
  container.append(counts);
}
function renderStages(job) {
  const list = $("detail-stages");
  list.replaceChildren();
  if (job.batch?.dry_run) return;
  for (const [index, storedStage] of (job.stages || []).entries()) {
    let stage = storedStage;
    const batch = visibleBatch(job);
    if (stage.name === "schedule_batch" && ["failed","partial"].includes(stage.status) && batch?.policy && !batch.failed) {
      stage = {...stage,status:batch.completed > 0 ? "partial" : "skip",note:`${batch.completed || 0} candidates · ${batch.policy} below minimum savings · Originals kept`};
    } else if (savingsPolicyFailure(job.error) && index === job.current_step) stage = {...stage,status:"skip",note:"Minimum savings not met; full conversion not started."};
    else if (stage.name === "schedule_batch" && ["failed","partial"].includes(stage.status) && batch?.rejected) {
      stage = {...stage,status:batch.failed > 0 || batch.completed > 0 ? "partial" : "rejected",note:[`${batch.completed || 0} candidates`,`${batch.rejected} rejected`,batch.failed > 0 ? `${batch.failed} failed` : ""].filter(Boolean).join(" · ")};
    } else if (rejectedCandidate(job) && index === job.current_step) stage = {...stage,status:"rejected",note:"Candidate rejected; original preserved."};
    const row = node("li", "", `stage ${stage.status}`);
    row.append(node("span", String(index + 1), "stage-number"), node("span", stageLabel(stage.name)), statusBadge(stage.status === "pending" ? "Next" : names[stage.status] || stage.status, stage.status));
    if (stage.note) row.append(node("span", stage.note, "stage-note"));
    if (index === job.current_step && ["running", "waiting_external", "waiting_decision"].includes(job.status)) row.setAttribute("aria-current", "step");
    list.append(row);
  }
}

async function reviewFilePromotion(id) {
  const auth = state.authRevision;
  const job = (await api(`operations?id=${encodeURIComponent(id)}`)).jobs?.[0];
  if (auth !== state.authRevision || $("workspace").hidden) return;
  if (!job?.promotion?.original_path || !job.promotion.candidate_path || !job.promotion.original_sha256 || !job.promotion.candidate_sha256 || job.status !== "waiting_decision" || !job.waiting_options?.some(option => option.decision === "approve"))
    throw new Error("This replacement is no longer waiting for approval. Refresh the queue.");
  state.fileApproval = { id, original: job.promotion.original_sha256, candidate: job.promotion.candidate_sha256 };
  const data = $("file-review-data");
  data.replaceChildren(node("p", job.promotion.original_path.split("/").pop(), "review-filename"));
  for (const [label, path, size] of [["Original", job.promotion.original_path, job.promotion.original_bytes], ["Verified candidate", job.promotion.candidate_path, job.promotion.candidate_bytes]]) {
    const row = node("div", "", "review-file");
    row.append(node("span", label, "muted"), node("strong", bytes(size)), node("span", path, "metadata review-path"));
    data.append(row);
  }
  const savings = savingsLine(job.savings);
  if (savings) data.append(savings);
  $("approve-file-review").disabled = !serverReachable || state.approvingFile;
  $("file-review").showModal();
}
$("dismiss-file-review").addEventListener("click", () => $("file-review").close());
$("file-review").addEventListener("close", () => { state.fileApproval = null; });
$("approve-file-review").addEventListener("click", () => safe(async () => {
  const approval = state.fileApproval;
  if (!approval || state.approvingFile || !serverReachable) return;
  state.approvingFile = true;
  $("approve-file-review").disabled = true;
  try {
    const current = (await api(`operations?id=${encodeURIComponent(approval.id)}`)).jobs?.[0];
    if (state.fileApproval !== approval || !$("file-review").open) return;
    if (current?.status !== "waiting_decision" || !current.waiting_options?.some(option => option.decision === "approve") || current.promotion?.original_sha256 !== approval.original || current.promotion?.candidate_sha256 !== approval.candidate)
      throw new Error("The replacement plan changed. Close this review and open it again.");
    await jobControl(approval.id, "action_resume", { decision: "approve" });
    $("file-review").close();
    await openJob(approval.id);
  } finally {
    state.approvingFile = false;
    $("approve-file-review").disabled = !serverReachable;
  }
}));

async function reviewBatchCandidates(id) {
  const auth = state.authRevision;
  const review = await api(`batch-candidates?id=${encodeURIComponent(id)}`);
  if (auth !== state.authRevision) return;
  const selected = new Set(review.members.map(member=>member.item_key));
  const content = [node("p", `${review.members.length} candidates available · Originals unchanged`, "review-filename"), node("p", "Select candidates to review for replacement. Verification runs before any original is replaced. You can open each file to compare frames or try different settings.", "metadata")];
  const all = node("input"); all.type="checkbox"; all.checked=true; all.setAttribute("aria-label","Select all candidates");
  const allLabel=node("label","","check");allLabel.append(all,node("span","Select all candidates")); content.push(allLabel);
  const checks=[];
  for (const member of review.members) {
    const row=node("div","","candidate-review-row"),label=node("label","","check"),check=node("input");
    check.type="checkbox";check.checked=true;checks.push(check);
    const name=member.episode || member.original_path.split("/").pop();
    label.append(check,node("span",name));
    check.addEventListener("change",()=>{check.checked?selected.add(member.item_key):selected.delete(member.item_key);all.checked=selected.size===review.members.length;all.indeterminate=selected.size>0&&!all.checked;$("confirm-action-review").disabled=!selected.size || !state.info.allow_destructive;});
    row.append(label,node("p",`${member.original_path} → ${member.candidate_path}`,"metadata backup-path"));
    row.append(button("View file",async()=>{finishActionReview(false);await openJob(member.transcode_action_id);},"text-button"));content.push(row);
  }
  all.addEventListener("change",()=>{selected.clear();checks.forEach((check,i)=>{check.checked=all.checked;if(all.checked)selected.add(review.members[i].item_key);});all.indeterminate=false;$("confirm-action-review").disabled=!selected.size || !state.info.allow_destructive;});
  if (review.excluded) content.push(node("p",`${review.excluded} other files are unfinished, have no candidate, or already have a replacement job. They will stay unchanged.`,"metadata"));
  if (!review.members.length) {notify("No unreserved candidates are available. Open the batch files to inspect their outcomes.");return;}
  if (!state.info.allow_destructive) content.push(node("p","Replacement is disabled in the server settings.","metadata"));
  const decision=reviewAction({title:"Review batch candidates",content,confirmLabel:"Prepare replacement review"});
  $("confirm-action-review").disabled=!state.info.allow_destructive;
  if (!await decision || auth!==state.authRevision) return;
  await commandRequest("batch-candidates",{id,version:review.version,item_keys:[...selected]},()=>notify("Replacement review requested. Originals are unchanged."));
  if (auth!==state.authRevision) return;
  await reviewBatchPromotion(id);
}
async function readBatchPromotionPlan(id) {
  const args = { id, section: "state", key: "batch_promotion_plan", chunk: 0 };
  const first = await tool("action_detail", args);
  let plan = first.data;
  if (first.total_chunks) {
    if (first.total_chunks > 128)
      throw new Error("This replacement plan is too large to display safely.");
    let content = first.content;
    for (let chunk = 1; chunk < first.total_chunks; chunk++)
      content += (await tool("action_detail", { ...args, chunk })).content;
    plan = JSON.parse(content);
  }
  if (
    plan?.batch_id !== id ||
    !plan.digest ||
    !Array.isArray(plan.members) ||
    !plan.members.length
  )
    throw new Error("Cannot verify the batch replacement list.");
  return plan;
}
async function reviewBatchPromotion(id) {
  const authRevision = state.authRevision;
  const plan = await readBatchPromotionPlan(id);
  if (authRevision !== state.authRevision || $("workspace").hidden) return;
  state.batchApproval = {id,digest:plan.digest};
  const host=$("batch-review-data");
  host.replaceChildren(node("p",`${plan.members.length} ${plan.members.length === 1 ? "candidate selected" : "candidates selected"}`,"metadata"));
  for (const member of plan.members) {
    const row=node("div","","candidate-review-row");
    row.append(node("strong",member.episode || member.original_path.split("/").pop()));
    for (const [label,path] of [["Original",member.original_path],["Candidate",member.candidate_path]]) {
      const line=node("p","","metadata backup-path");line.append(node("span",`${label}: `),node("span",path));row.append(line);
    }
    host.append(row);
  }
  $("batch-review").showModal();
}
$("dismiss-batch-review").addEventListener("click", () =>
  $("batch-review").close(),
);
async function decideBatchReplacement(decision) {
  const approval=state.batchApproval;
  if (!approval || state.approvingBatch) return;
  state.approvingBatch=true;
  $("approve-batch-review").disabled=$("reject-batch-review").disabled=true;
  try {
    await commandRequest("batch-candidates",{id:approval.id,digest:approval.digest,decision},()=>$("batch-review").close());
    await refreshDetail();
    await loadJobs();
  } finally {
    state.approvingBatch=false;
    $("approve-batch-review").disabled=$("reject-batch-review").disabled=!serverReachable;
  }
}
$("approve-batch-review").addEventListener("click",()=>safe(()=>decideBatchReplacement("approve")));
$("reject-batch-review").addEventListener("click",()=>safe(()=>decideBatchReplacement("reject")));
$("close-detail").addEventListener("click", () => $("job-detail").close());
$("job-detail").addEventListener("close", () => { state.detailRevision++; state.detail=null; writeNavigation(); });
$("load-detail").addEventListener("click", () => safe(loadDetail));

function profileEncoder(p) {
  return p?.video?.codec === "hevc_videotoolbox" ? "Hardware HEVC" : p?.video?.codec === "libx265" ? "Software HEVC · CPU" : p?.video?.codec || "Encoder unavailable";
}
function profileRate(p) {
  const v = p?.video || {};
  if (v.average_bitrate_kbps > 0) return `${v.average_bitrate_kbps} kbps target bitrate`;
  if (v.quality == null) return "Profile default";
  if (v.codec === "libx265") return `CRF ${v.quality} · Lower means higher quality${v.preset ? ` · ${v.preset} preset` : ""}`;
  if (v.codec === "hevc_videotoolbox") return `Quality ${v.quality} · Higher means higher quality`;
  return `Quality ${v.quality}`;
}
function profileOptionLabel(name) {
  const data = state.recipeDetails[name];
  if (!data) return name;
  const labels = {
    "general-hevc": `General · Quality ${data.profile?.video?.quality ?? "default"}`,
    "hevc-vt": `General · Quality ${data.profile?.video?.quality ?? "default"}`,
    "hevc-vt-balanced": "General · Balanced",
    "hevc-vt-quality": "General · Higher quality",
    "hevc-vt-space": "General · Smaller files",
    "anime-hevc": `Anime · Quality ${data.profile?.video?.quality ?? "default"}`,
    "anime-hevc-balanced": "Anime · Balanced",
    "anime-hevc-balanced-aq": "Anime · Balanced + adaptive detail",
    "anime-hevc-quality": "Anime · Higher quality + adaptive detail",
    "anime-hevc-space": "Anime · Smaller files",
    "anime-hevc-main10": "Anime · Balanced, 10-bit",
    "anime-hevc-main10-aq": "Anime · Balanced, 10-bit + adaptive detail",
    "live-action-hevc": "Live action",
    "live-action-hevc-vt": "Live action",
  };
  const p = data.profile || {};
  const category = /^anime/.test(name) ? "Anime" : /^live-action/.test(name) ? "Live action" : "General";
  const effective = p.video?.codec === "copy" ? "Keep video" : p.video?.quality != null ? `${p.video.codec === "libx265" ? "CRF" : "Quality"} ${p.video.quality}` : "Custom settings";
  const title = data.source === "active_bundle" ? labels[name] || name : `${category} · ${effective}`;
  const encoder = p.video?.codec === "hevc_videotoolbox" ? "Hardware" : p.video?.codec === "libx265" ? "CPU" : p.video?.codec;
  return [title, encoder, p.optimization?.enabled ? "Test samples" : "", data.source === "managed" ? `Custom (${name})` : ""].filter(Boolean).join(" · ");
}

async function loadRecipes(initial = false, more = false) {
  if (!more) {
    state.recipeOffset = 0;
    state.recipes = [];
    state.recipeDetails = {};
  }
  const auth = state.authRevision;
  const page = await tool("recipe_list", {
    offset: state.recipeOffset,
    limit: 100,
    include_selection_details: true,
  });
  if (auth !== state.authRevision) return;
  Object.assign(state.recipeDetails, page.selection_details || {});
  const names = [
    ...new Set([
      ...(page.active_bundle_profiles || []),
      ...(page.static_config_profiles || []),
      ...(page.managed_profiles || []).map((p) => p.name),
    ]),
  ].sort();
  state.recipes = [...new Set([...state.recipes, ...names])].sort();
  state.recipeOffset += page.managed_returned || 0;
  $("recipes-more").hidden = state.recipeOffset >= (page.managed_total || 0);
  const selected = $("profile").value;
  $("profile").replaceChildren();
  option($("profile"), "auto", "Automatic");
  state.recipes.forEach((n) => option($("profile"), n, profileOptionLabel(n)));
  $("profile").value = state.recipes.includes(selected) ? selected : "auto";
  renderRecipeList();
  if (initial) return;
}
$("recipes-more").addEventListener("click", () =>
  safe(() => loadRecipes(false, true)),
);
function recipeSnapshot() {
  try { return JSON.stringify({name:$("recipe-name").value,description:$("recipe-description").value,profile:recipeFromControls()}); }
  catch { return "invalid:" + $("recipe-name").value + ":" + $("recipe-description").value; }
}
function recipeIsDirty() { return state.recipeBaseline != null && recipeSnapshot() !== state.recipeBaseline; }
function markRecipeClean() { state.recipeBaseline = recipeSnapshot(); renderRecipeDirty(); }
function renderRecipeDirty() {
  const dirty = recipeIsDirty();
  $("recipe-dirty").textContent = dirty ? "Unsaved changes" : "No unsaved changes.";
  $("recipe-dirty").dataset.dirty = String(dirty);
}
async function confirmRecipeDiscard() {
  if (!recipeIsDirty()) return true;
  return reviewAction({title:"Discard unsaved profile changes?",message:"Your profile draft will be discarded. Saved profiles and active jobs are unchanged.",confirmLabel:"Discard changes"});
}
$("recipe-form").addEventListener("input", () => { state.recipeEditRevision = (state.recipeEditRevision || 0) + 1; renderRecipeDirty(); });
$("recipe-settings").addEventListener("input", () => { state.recipeEditRevision = (state.recipeEditRevision || 0) + 1; renderRecipeDirty(); });
if (typeof window !== "undefined") window.addEventListener("beforeunload", event => { if (recipeIsDirty()) { event.preventDefault(); event.returnValue = ""; } });
function resetRecipeView() {
  state.recipeRevision = (state.recipeRevision || 0) + 1;
  state.recipe = null;
  state.recipeBaseline = null;
  $("recipe-form").inert = true;
  $("recipe-name").value = "";
  $("recipe-description").value = "";
  $("recipe-source").textContent = "Choose a profile.";
  $("recipe-feedback").hidden = true;
  $("delete-recipe").disabled = true;
  $("delete-recipe").hidden = true;
  $("recipe-history").disabled = true;
  renderRecipeList();
}
async function readRecipe(name, replaceRoute = false) {
  if (recipeIsDirty() && !await confirmRecipeDiscard()) { renderRecipeList(); return; }
  const editRevision = state.recipeEditRevision || 0;
  const revision = (state.recipeRevision || 0) + 1;
  const auth = state.authRevision;
  state.recipeRevision = revision;
  $("recipe-form").inert = true;
  $("recipe-source").textContent = `Loading ${name}…`;
  try {
    const r = await tool("recipe_get", { name });
    if (revision !== state.recipeRevision || auth !== state.authRevision || editRevision !== (state.recipeEditRevision || 0))
      return;
    state.recipe = r;
    $("recipe-name").value = name;
    $("recipe-description").value = r.record?.description || r.description || "";
    setRecipeControls(r.profile);
    $("recipe-source").textContent =
      `${r.source === "managed" ? "Custom profile" : "Built-in profile"}${r.shadowed_profiles?.length ? " · Overrides built-in" : ""}`;
    $("recipe-save-effect").textContent = r.source === "managed" ? "Save creates a new managed version for new jobs. Active plans keep their saved settings." : "Save creates a managed override of this built-in profile for new jobs. The built-in bundle and active plans stay intact.";
    markRecipeClean();
    $("delete-recipe").disabled = r.source !== "managed";
    $("delete-recipe").hidden = r.source !== "managed";
    $("recipe-history").disabled = false;
    const resetLabel = r.shadowed_profiles?.length
      ? "Restore built-in profile"
      : "Delete custom profile";
    $("delete-recipe").setAttribute("aria-label", resetLabel);
    $("delete-recipe").title = resetLabel;
    const resetIcon = actionIcon(resetLabel);
    if (resetIcon) $("delete-recipe").replaceChildren(resetIcon);
    $("recipe-feedback").hidden = true;
    renderRecipeList();
    writeNavigation(replaceRoute);
  } catch (error) {
    if (revision === state.recipeRevision && auth === state.authRevision)
      $("recipe-source").textContent =
        `Could not load ${name}. Select it again to retry.`;
    // A failed read must not reactivate the previous profile's editable form.
    // Successful reads and new profile creation enable it in setRecipeControls.
    throw error;
  }
}
$("new-recipe").addEventListener("click", () =>
  safe(async () => {
    if (recipeIsDirty() && !await confirmRecipeDiscard()) return;
    state.recipe = null;
    $("recipe-name").value = "";
    $("recipe-description").value = "";
    $("recipe-source").textContent = "New profile";
    $("recipe-save-effect").textContent = "Save creates a managed profile for new jobs.";
    setRecipeControls(profileFromControls());
    markRecipeClean();
    $("recipe-feedback").hidden = true;
    $("delete-recipe").disabled = true;
    $("delete-recipe").hidden = true;
    $("recipe-history").disabled = true;
    renderRecipeList();
    writeNavigation();
  }),
);
async function saveRecipe() {
  if (state.recipeSaving) return;
  state.recipeSaving = true;
  const revision = state.recipeRevision;
  const auth = state.authRevision;
  const editRevision = state.recipeEditRevision || 0;
  try {
    const args = {
      name: $("recipe-name").value.trim(),
      description: $("recipe-description").value,
      profile: recipeFromControls(),
    };
    const q = args.profile.video.quality;
    const bitrate = args.profile.video.codec === "hevc_videotoolbox" && args.profile.video.average_bitrate_kbps > 0;
    if (!bitrate && (!Number.isFinite(q) || q < 1 || q > (args.profile.video.codec === "libx265" ? 51 : 100))) throw new Error("Quality is outside the effective encoder's supported scale");
    if (state.recipe?.record && state.recipe.name === args.name) {
      args.expected_generation = state.recipe.record.generation;
      args.expected_digest = state.recipe.record.digest;
    }
    const saved = await tool("recipe_save", args);
    if (auth !== state.authRevision) return;
    if (revision === state.recipeRevision) {
      // Update CAS metadata even if the user edited while the request was in
      // flight. Their newer controls stay untouched and remain dirty against
      // the precise submitted version, so a next save does not conflict with us.
      if (saved.record) state.recipe = {...(state.recipe || {}),name:args.name,source:"managed",record:saved.record};
      state.recipeBaseline = JSON.stringify({name:args.name,description:args.description,profile:args.profile});
      renderRecipeDirty();
    }
    await loadRecipes();
    if (revision === state.recipeRevision && auth === state.authRevision && editRevision === (state.recipeEditRevision || 0))
      await readRecipe(args.name);
    showData("recipe-feedback", "Profile saved. New jobs use this version; active plans are unchanged.");
    notify("Profile saved.");
  } catch (error) {
    if (revision === state.recipeRevision && auth === state.authRevision) showData("recipe-feedback", `Save failed: ${error.message}. Your draft is preserved. If this is a version conflict, review the current saved version before discarding or retrying.`);
    throw error;
  } finally {
    state.recipeSaving = false;
  }
}
$("recipe-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(saveRecipe);
});
$("delete-recipe").addEventListener("click", () =>
  safe(async () => {
    const r = state.recipe;
    const restore = Boolean(r?.shadowed_profiles?.length);
    if (
      !r?.record ||
      !await reviewAction({title:restore ? "Restore built-in profile?" : "Delete custom profile?", message:(restore ? `Restore the built-in settings for ${r.name}.` : `Remove ${r.name} from your custom profiles.`) + (recipeIsDirty() ? " Your unsaved draft will also be discarded." : ""), confirmLabel:restore ? "Restore profile" : "Delete profile"})
    )
      return;
    await tool("recipe_delete", {
      name: r.name,
      expected_generation: r.record.generation,
      expected_digest: r.record.digest,
    });
    state.recipe = null;
    state.recipeBaseline = null;
    state.recipeOffset = 0;
    state.recipes = [];
    state.recipeDetails = {};
    await loadRecipes();
    if (state.recipes.length) await readRecipe(state.recipes[0]);
    notify(restore ? "Built-in profile restored." : "Custom profile deleted.");
  }),
);
$("recipe-history").addEventListener("click", () =>
  safe(async () => {
    const result = await tool("recipe_history", {
      name: $("recipe-name").value,
    });
    const entries = result.history || [];
    showData(
      "recipe-feedback",
      entries.length
        ? entries
            .map(
              (e) =>
                `${e.operation || e.event || "Saved"} · ${new Date(e.created_at || e.at || e.timestamp).toLocaleString("en")}`,
            )
            .join("\n")
        : "No profile changes recorded.",
    );
  }),
);
function renderRecipeList() {
  $("recipe-picker").replaceChildren();
  option($("recipe-picker"), "", "Choose a profile");
  state.recipes.forEach((name) => option($("recipe-picker"), name));
  $("recipe-picker").value = state.recipe?.name || "";
  $("recipes-list").replaceChildren();
  const query = $("recipe-search").value.toLowerCase();
  for (const name of state.recipes.filter((n) =>
    n.toLowerCase().includes(query),
  )) {
    const b = button(name, () => readRecipe(name), "recipe-button");
    b.setAttribute("aria-pressed", String(state.recipe?.name === name));
    $("recipes-list").append(b);
  }
}
$("recipe-picker").addEventListener("change", () => {
  const name = $("recipe-picker").value;
  if (name) safe(() => readRecipe(name));
});
$("recipe-search").addEventListener("input", renderRecipeList);
function setRecipeControls(profile) {
  // New profile creation also supersedes any in-flight profile request.
  state.recipeRevision = (state.recipeRevision || 0) + 1;
  $("recipe-form").inert = false;
  state.recipeDraft = JSON.parse(JSON.stringify(profile));
  const video = profile.video || {};
  for (const [id, value] of Object.entries({
    "recipe-container": profile.container || "mkv",
    "recipe-encoder": video.codec || "libx265",
    "recipe-quality":
      video.average_bitrate_kbps > 0
        ? 0
        : (video.quality ?? (video.codec === "hevc_videotoolbox" ? 65 : 23)),
    "recipe-preset": video.preset || "medium",
    "recipe-audio": profile.audio?.mode || "copy",
  }))
    $(id).value = String(value);
  for (const key of ["metadata", "chapters", "attachments"])
    $("recipe-" + key).checked = profile.preserve?.[key] !== false;
  renderAdvancedProfile(profile);
  // Advanced rate-control inputs update the draft before bubbling here.
  $("recipe-extra-fields").oninput = recipeControls;
  recipeControls();
}
function recipeControls() {
  const software = $("recipe-encoder").value === "libx265";
  const bitrate =
    !software && state.recipeDraft?.video?.average_bitrate_kbps > 0;
  $("recipe-preset").disabled = !software;
  const presetLabel = $("recipe-preset").closest?.("label");
  if (presetLabel) presetLabel.hidden = !software;
  $("recipe-quality").disabled = bitrate;
  $("recipe-quality").max = software ? "51" : "100";
  $("recipe-quality").min = bitrate ? "0" : "1";
  $("recipe-quality-label").textContent = bitrate ? "Quality (bitrate mode)" : software ? "CRF · lower preserves more detail" : "VideoToolbox Q · higher preserves more detail";
  $("recipe-scale-note").textContent = bitrate ? "Bitrate is measured in kbps; estimated output size requires an applicable sample test." : software ? "x265 CRF: 1–51, lower favors quality. This is not a quality percentage. Estimated size: requires sample test." : "VideoToolbox Q: 1–100, higher favors quality. Q 70 is not 70% quality and cannot be compared to x265 CRF. Estimated size: requires sample test.";
  if (bitrate) $("recipe-quality").value = "0";
  else if (!software && Number($("recipe-quality").value) === 0)
    $("recipe-quality").value = "65";
}
$("recipe-encoder").addEventListener("change", () => {
  const p = recipeFromControls();
  const previous = state.recipeDraft?.video?.codec;
  const software = p.video.codec === "libx265";
  if (p.video.codec !== previous) {
    if (software) {
      for (const key of [
        "prioritize_speed",
        "spatial_aq",
        "realtime",
        "average_bitrate_kbps",
        "max_bitrate_kbps",
        "constant_bitrate",
        "qmin",
        "qmax",
        "gop_size",
        "b_frames",
        "closed_gop",
        "power_efficient",
        "max_ref_frames",
      ])
        delete p.video[key];
      p.video.preset = $("recipe-preset").value || "medium";
      p.video.quality = 23;
    } else {
      delete p.video.preset;
      delete p.video.tune;
      p.video.quality = 65;
    }
    if (p.optimization?.search) {
      p.optimization.search.quality_values = software
        ? [20, 23, 26]
        : [55, 65, 75];
      p.optimization.search.max_candidates = 3;
      delete p.optimization.search.bitrate_values;
    }
    setRecipeControls(p);
    renderRecipeDirty();
    notify(
      "Encoder changed. Incompatible settings and sample search values were reset; review before saving.",
    );
  } else recipeControls();
});
function recipeFromControls() {
  if (state.recipeAdvancedErrors?.size)
    throw new Error([...state.recipeAdvancedErrors.values()][0]);
  const p = JSON.parse(JSON.stringify(state.recipeDraft || {}));
  p.container = $("recipe-container").value;
  p.video ||= {};
  p.video.codec = $("recipe-encoder").value;
  p.video.quality =
    p.video.codec === "hevc_videotoolbox" && p.video.average_bitrate_kbps > 0
      ? 0
      : Number($("recipe-quality").value);
  if (p.video.codec === "libx265") p.video.preset = $("recipe-preset").value;
  p.audio ||= {};
  p.audio.mode = $("recipe-audio").value;
  p.preserve ||= {};
  for (const key of ["metadata", "chapters", "attachments"])
    p.preserve[key] = $("recipe-" + key).checked;
  return p;
}
// Defaults describe available controls; merely opening the dialog does not
// insert optional values into an existing profile.
function advancedProfileDefaults(profile) {
  const vt = profile.video?.codec === "hevc_videotoolbox";
  const bitrate = vt && profile.video?.average_bitrate_kbps > 0;
  const video = { profile: "", pixel_format: "" };
  if (vt) {
    Object.assign(video, {
      average_bitrate_kbps: null,
      max_bitrate_kbps: null,
      constant_bitrate: null,
      prioritize_speed: null,
      spatial_aq: null,
      realtime: null,
      qmin: null,
      qmax: null,
      gop_size: null,
      b_frames: null,
      closed_gop: null,
      power_efficient: null,
      max_ref_frames: null,
    });
  } else video.tune = "";
  return {
    video,
    preserve: { metadata: true, chapters: true, attachments: true },
    optimization: {
      enabled: false,
      sampling: {
        strategy: "distributed",
        sample_count: 3,
        sample_seconds: 20,
      },
      quality: {
        preferred_metric: "vmaf",
        vmaf: { target: 96, minimum: 95, marginal_tolerance: 0.5 },
        ssim: { target: 0.99, minimum: 0.98, marginal_tolerance: 0.005 },
      },
      search: {
        max_candidates: 3,
        [bitrate ? "bitrate_values" : "quality_values"]: bitrate
          ? [
              Math.round(profile.video.average_bitrate_kbps * 0.8),
              profile.video.average_bitrate_kbps,
              Math.round(profile.video.average_bitrate_kbps * 1.2),
            ]
          : vt
            ? [55, 65, 75]
            : [20, 23, 26],
      },
    },
  };
}
function mergeAdvancedDefaults(defaults, profile) {
  const result = JSON.parse(JSON.stringify(profile || {}));
  for (const [key, value] of Object.entries(defaults)) {
    if (value && typeof value === "object" && !Array.isArray(value))
      result[key] = mergeAdvancedDefaults(value, result[key]);
    else if (!(key in result)) result[key] = value;
  }
  return result;
}
function updatePreservationNote() {
  const note = document.querySelector?.(".preservation-note");
  if (!note) return;
  const kept = ["metadata", "chapters", "attachments"].filter(
    (key) => $("recipe-" + key).checked,
  );
  note.textContent =
    kept.length === 3
      ? "Metadata, chapters and attachments are preserved."
      : `Unsupported policy: ${["metadata", "chapters", "attachments"].filter((key) => !kept.includes(key)).join(", ")} must be enabled before saving.`;
}
function renderAdvancedProfile(profile) {
  const host = $("recipe-extra-fields");
  host.replaceChildren();
  state.recipeAdvancedErrors = new Map();
  const defaults = advancedProfileDefaults(profile);
  const view = mergeAdvancedDefaults(defaults, profile);
  const represented = new Set([
    "container",
    "video.codec",
    "video.quality",
    "video.preset",
    "audio.mode",
  ]);
  const booleans = new Set([
    "constant_bitrate",
    "prioritize_speed",
    "spatial_aq",
    "realtime",
    "closed_gop",
    "power_efficient",
  ]);
  const bounds = {
    average_bitrate_kbps: [1, 1000000],
    max_bitrate_kbps: [1, 1000000],
    qmin: [0, 69],
    qmax: [0, 69],
    gop_size: [1, 100000],
    b_frames: [0, 1],
    max_ref_frames: [1, 16],
    sample_count: [1, 20],
    sample_seconds: [0.1, 120],
    max_candidates: [1, 20],
  };
  const choices = {
    "video.profile": ["", "main", "main10"],
    "video.pixel_format": ["", "yuv420p", "p010le"],
    "video.tune": [
      "",
      "animation",
      "grain",
      "fastdecode",
      "zerolatency",
      "psnr",
      "ssim",
    ],
    "optimization.sampling.strategy": [
      "distributed",
      "uniform",
      "relative_positions",
    ],
    "optimization.quality.preferred_metric": ["vmaf", "ssim"],
  };
  const setValue = (path, value) => {
    const keys = path.split(".");
    let target = state.recipeDraft;
    for (const part of keys.slice(0, -1)) target = target[part] ||= {};
    if (value === undefined) delete target[keys.at(-1)];
    else target[keys.at(-1)] = value;
    if (path.startsWith("preserve.")) {
      $("recipe-" + keys.at(-1)).checked = value !== false;
      updatePreservationNote();
    }
  };
  const visit = (obj, path, parent) => {
    for (const [key, value] of Object.entries(obj)) {
      const current = path ? `${path}.${key}` : key;
      if (represented.has(current)) continue;
      const caption = key
        .replaceAll("_", " ")
        .replace(/^./, (c) => c.toUpperCase());
      if (value && typeof value === "object" && !Array.isArray(value)) {
        const group = node("fieldset");
        group.append(node("legend", caption));
        if (
          current === "video" &&
          profile.video?.codec === "hevc_videotoolbox"
        ) {
          const label = node("label", "Rate control");
          const select = document.createElement("select");
          select.dataset.profileField = "video.rate_control";
          option(select, "quality", "Quality");
          option(select, "bitrate", "Bitrate");
          select.value =
            profile.video.average_bitrate_kbps > 0 ? "bitrate" : "quality";
          select.addEventListener("change", () => {
            const p = state.recipeDraft;
            if (select.value === "bitrate") {
              p.video.average_bitrate_kbps ||= 3500;
              p.video.quality = 0;
              $("recipe-quality").value = "0";
            } else {
              delete p.video.average_bitrate_kbps;
              delete p.video.max_bitrate_kbps;
              delete p.video.constant_bitrate;
              p.video.quality = 65;
              $("recipe-quality").value = "65";
            }
            if (p.optimization?.search) {
              delete p.optimization.search.quality_values;
              delete p.optimization.search.bitrate_values;
              Object.assign(
                p.optimization.search,
                advancedProfileDefaults(p).optimization.search,
              );
            }
            renderAdvancedProfile(p);
            recipeControls();
          });
          label.append(select);
          group.append(label);
        }
        visit(value, current, group);
        if (group.children.length > 1) parent.append(group);
      } else if (
        Array.isArray(value) &&
        value.some((v) => typeof v === "object")
      ) {
        const group = node("fieldset");
        group.append(node("legend", caption));
        visit(value, current, group);
        if (group.children.length > 1) parent.append(group);
      } else {
        const label = node("label", caption);
        const optionalBoolean = booleans.has(key);
        const enumValues = choices[current];
        const input = document.createElement(
          optionalBoolean || enumValues ? "select" : "input",
        );
        input.dataset.profileField = current;
        const list = Array.isArray(value);
        if (optionalBoolean) {
          option(input, "", "Automatic");
          option(input, "true", "On");
          option(input, "false", "Off");
          input.value = value == null ? "" : String(value);
        } else if (enumValues) {
          for (const v of enumValues) option(input, v, v || "Automatic");
          if (value && !enumValues.includes(value)) option(input, value);
          input.value = value ?? "";
        } else {
          input.type =
            typeof value === "boolean"
              ? "checkbox"
              : typeof value === "number" || value === null
                ? "number"
                : "text";
          if (input.type === "checkbox") {
            input.checked = value;
            label.className = "check";
            if (current.startsWith("preserve.")) {
              label.textContent = `${caption} (required)`;
              input.disabled = value === true;
            }
          } else {
            input.value = list ? value.join(", ") : (value ?? "");
            if (value === null) input.placeholder = "Automatic";
            if (input.type === "number") {
              input.step =
                key === "sample_seconds" || current.includes(".quality.")
                  ? "any"
                  : "1";
              if (bounds[key]) [input.min, input.max] = bounds[key].map(String);
            }
          }
        }
        const bitrateOnly =
          current.startsWith("video.") &&
          [
            "average_bitrate_kbps",
            "max_bitrate_kbps",
            "constant_bitrate",
          ].includes(key);
        if (bitrateOnly)
          input.disabled = !(profile.video?.average_bitrate_kbps > 0);
        input.addEventListener(
          optionalBoolean || enumValues ? "change" : "input",
          () => {
            let next;
            if (optionalBoolean)
              next = input.value === "" ? undefined : input.value === "true";
            else if (input.type === "checkbox") {
              if (current.startsWith("preserve.")) {
                input.checked = true;
                input.disabled = true;
              }
              next = input.checked;
            } else if (list) {
              next = input.value.trim()
                ? input.value.split(",").map((v) => Number(v.trim()))
                : [];
              if (
                next.some((v) => !Number.isFinite(v)) ||
                input.value.split(",").some((v) => !v.trim())
              ) {
                state.recipeAdvancedErrors.set(
                  current,
                  `${caption} must be a comma-separated list of numbers.`,
                );
                input.setCustomValidity?.("Enter comma-separated numbers.");
                return;
              }
            } else if (input.type === "number") {
              if (!input.value.trim() && key === "average_bitrate_kbps") {
                state.recipeAdvancedErrors.set(
                  current,
                  "Average bitrate is required in bitrate mode.",
                );
                input.setCustomValidity?.("Enter an average bitrate.");
                return;
              }
              if (!input.value.trim()) next = undefined;
              else {
                next = Number(input.value);
                if (
                  !Number.isFinite(next) ||
                  (bounds[key] &&
                    (next < bounds[key][0] || next > bounds[key][1])) ||
                  (input.step === "1" && !Number.isInteger(next))
                ) {
                  state.recipeAdvancedErrors.set(
                    current,
                    `${caption} is outside its valid range.`,
                  );
                  input.setCustomValidity?.(
                    "Enter a value within the allowed range.",
                  );
                  return;
                }
              }
            } else next = input.value || undefined;
            state.recipeAdvancedErrors.delete(current);
            input.setCustomValidity?.("");
            setValue(current, next);
            if (current === "optimization.enabled" && next) {
              state.recipeDraft.optimization = mergeAdvancedDefaults(
                advancedProfileDefaults(state.recipeDraft).optimization,
                state.recipeDraft.optimization,
              );
            }
            recipeControls();
          },
        );
        label.append(input);
        parent.append(label);
      }
    }
  };
  visit(view, "", host);
  updatePreservationNote();
}
$("reload-profiles").addEventListener("click", () =>
  safe(async () => {
    if (!await reviewAction({title:"Load configured presets?",message:"Validate and activate the configured preset source for new jobs. Managed overrides, your editor draft and active plans are preserved.",confirmLabel:"Load presets"})) return;
    const r = await tool("recipe_reload");
    if (r.error) throw new Error(r.error);
    await loadRecipes();
    notify("Shared presets refreshed.");
  }),
);
$("rollback-profiles").addEventListener("click", () =>
  safe(async () => {
    const result = await tool("recipe_status");
    const status = result.bundle || {};
    if (!status.active_digest || !status.previous_digest) throw new Error("No previous validated preset version is available to review.");
    if (!await reviewAction({title:"Restore previous preset version?", message:`Replace active version ${status.active_version || status.active_digest.slice(0,12)} with previous version ${status.previous_version || status.previous_digest.slice(0,12)} for new jobs. Managed overrides, your editor draft, active plans and permissions are preserved.`, confirmLabel:"Restore reviewed version"})) return;
    const r = await tool("recipe_rollback", {expected_active_digest:status.active_digest,expected_previous_digest:status.previous_digest});
    if (r.error) throw new Error(r.error);
    await loadRecipes();
    notify("Previous profile bundle restored.");
  }),
);

$("open-path").addEventListener("click", () => $("path-dialog").showModal());
$("close-path").addEventListener("click", () => $("path-dialog").close());
$("recipe-advanced").addEventListener("click", () =>
  $("recipe-settings").showModal(),
);
$("close-recipe-settings").addEventListener("click", () =>
  $("recipe-settings").close(),
);
$("done-recipe-settings").addEventListener("click", () =>
  $("recipe-settings").close(),
);
function backupName(copy) {
  return copy.original_path?.split("/").pop() || copy.path?.split("/").slice(-2,-1)[0] || copy.action_id;
}
function backupReason(copy) {
  if (copy.duplicate_available) return "Original still present. Verify both SHA-256 hashes to remove only the redundant recovery data; the failed job stays unchanged.";
  if (copy.cleanup_available) return "The replacement will be verified before this recovery copy is removed.";
  if (operationIsActive(copy.status)) return "Job active. This recovery copy is retained until the replacement finishes.";
  if (copy.status === "cancelled") return "Job cancelled before cleanup. Review its details to resolve the replacement.";
  if (/stopped before final cleanup/.test(copy.reason || "")) return copy.delete_available ? "Verification is blocked by the earlier replacement failure. Remove copy discards the backup without repairing that job." : "Replacement failed before cleanup. Resolve the earlier error in job details first.";
  if (/was not approved/.test(copy.reason || "")) return "Replacement approval is required. This recovery copy is retained.";
  if (/allow_destructive/.test(copy.reason || "")) return "Recovery cleanup is disabled in the server settings.";
  return copy.reason || "Review job details before cleaning up this recovery copy.";
}
function operationIsActive(status) {
  return ["pending","running","waiting_external","waiting_decision"].includes(status);
}
function cleanupError(value) {
  let reason = value;
  try { reason = JSON.parse(value).error || value; } catch {}
  if (/imported file size differs from the validated candidate/.test(reason || "")) return "Sonarr reports a different size for the replacement than the validated candidate. The backup was kept. Use Remove copy if you no longer need that backup.";
  return reason || "Verification did not complete.";
}
function renderBackups() {
  $("backup-list").replaceChildren();
  for (const copy of state.backups || []) {
    const row = node("div", "", "backup-row");
    const heading = node("div", "", "backup-heading");
    heading.append(node("span", backupName(copy)), node("span", bytes(Number(copy.bytes || 0) + Number(copy.partial_bytes || 0)), "metadata"));
    row.append(heading);
    if (copy.original_path) row.append(node("p", `Original: ${copy.original_path}`, "backup-path"));
    if (copy.path) row.append(node("p", `Recovery: ${copy.path}${copy.partial_bytes ? ` · Partial copy: ${bytes(copy.partial_bytes)}` : ""}`, "backup-path"));
    row.append(node("p", backupReason(copy), `backup-reason${copy.cleanup_available ? "" : " waiting"}`));
    if (copy.error) row.append(node("p", `Last verification error: ${cleanupError(copy.error)}`, "backup-reason waiting"));
    const actions = node("div", "", "backup-actions");
    actions.append(button("View job", () => openJob(copy.action_id), "text-button"));
    if (copy.cleanup_available || copy.duplicate_available) {
      const clean = button(state.backupCleaning?.id === copy.action_id ? "Verifying…" : copy.duplicate_available ? "Verify duplicate & remove" : "Verify & clean up", () => cleanBackup(copy));
      clean.disabled = Boolean(state.backupCleaning) || !serverReachable;
      actions.append(clean);
    }
    if (copy.delete_available) {
      const remove = button("Remove copy", () => cleanBackup(copy,"discard"));
      remove.disabled = Boolean(state.backupCleaning) || !serverReachable;
      actions.append(remove);
    }
    row.dataset.operationId = copy.action_id;
    if (state.backupCleaning?.id === copy.action_id) row.append(node("p", state.backupCleaning.progress ? "Cleanup in progress · See live verification below" : "Request accepted · Waiting to start cleanup", "job-command"));
    row.append(actions);
    $("backup-list").append(row);
  }
  const copies = state.backups || [];
  const total = copies.reduce((sum,c)=>sum + Number(c.bytes || 0) + Number(c.partial_bytes || 0),0);
  $("backup-summary").hidden = !state.backupsVisible;
  $("backup-summary").textContent = `${copies.length} recovery ${copies.length === 1 ? "copy" : "copies"} · ${bytes(total)} retained${state.backupsLoading ? " · Loading remaining copies…" : ""}`;
  if (!copies.length && !state.backupsLoading) $("backup-list").append(node("p", "No retained recovery copies.", "muted"));
}
async function loadBackups() {
  if (state.backupsLoading) return;
  const revision = state.backupRevision = (state.backupRevision || 0) + 1;
  const auth = state.authRevision;
  state.backupsVisible = true;
  state.backupsLoading = true;
  state.backups = [];
  $("backup-list").hidden = false;
  $("load-backups").disabled = true;
  $("backups-more").hidden = true;
  writeNavigation();
  renderBackups();
  renderBackupCleanup();
  const offsets = new Set(), keys = new Set();
  let offset = 0;
  try {
    while (offset != null) {
      if (offsets.has(offset)) throw new Error("Recovery inventory did not advance. Reload to try again.");
      offsets.add(offset);
      const result = await tool("transcode_backups", {offset});
      if (revision !== state.backupRevision || auth !== state.authRevision) return;
      for (const copy of result.items || []) {
        const key = `${copy.action_id}:${copy.path || ""}`;
        if (!keys.has(key)) {keys.add(key);state.backups.push(copy);}
      }
      renderBackups();
      offset = result.next_offset;
    }
  } catch (error) {
    if (revision === state.backupRevision && auth === state.authRevision) {
      $("backup-progress").hidden = false;
      $("backup-progress").textContent = `Could not finish loading recovery copies: ${error.message}. Reload to try again.`;
    }
    throw error;
  } finally {
    if (revision === state.backupRevision) {
      state.backupsLoading = false;
      $("load-backups").disabled = false;
      $("load-backups").textContent = "Reload copies";
      renderBackups();
    }
  }
}
function renderBackupCleanup() {
  if (!state.backupCleaning) return;
  const task = state.backupCleaning;
  $("backup-progress").hidden = !state.backupsVisible;
  const p = task.progress;
  const phase = {checking_library:"Checking the active library file",verifying_replacement:"Verifying replacement SHA-256",verifying_recovery:"Verifying recovery SHA-256",removing_recovery:"Removing verified recovery copy",removing_copy:"Removing recovery copy",waiting_library:"Waiting for the library",completed:"Cleanup completed",stopped:"Cleanup stopped"}[p?.phase] || (task.checking ? "Checking cleanup outcome" : "Waiting for server to start verification");
  const notice = $("backup-progress");
  notice.classList.toggle("cleanup-stopped", p?.phase === "stopped");
  notice.replaceChildren(node("strong", phase), node("span", `${task.name} · ${Math.max(0,Math.floor((Date.now()-task.started)/1000))}s elapsed`));
  if (p?.total_bytes > 0 && p.phase.startsWith("verifying")) {
    const bar = document.createElement("progress");
    bar.max = p.total_bytes; bar.value = p.bytes_read || 0;
    bar.setAttribute("aria-label", phase);
    notice.append(bar, node("span", `${bytes(p.bytes_read || 0)} / ${bytes(p.total_bytes)} verified · ${Math.min(100,Math.floor((p.bytes_read || 0)/p.total_bytes*100))}%`));
  } else if (!["completed","stopped"].includes(p?.phase)) {
    const bar = document.createElement("progress"); bar.setAttribute("aria-label", phase); notice.append(bar);
  }
  if (p?.path) notice.append(node("span", p.path, "backup-path"));
}
async function finishBackupCleanup(job) {
  const task = state.backupCleaning;
  if (!task || task.auth !== state.authRevision) return;
  const currentObservation = task.commandCompleted || Date.parse(job.cleanup?.updated_at) >= task.started;
  if (task.mode === "discard" && !currentObservation && !task.error) { renderBackupCleanup(); return; }
  if (operationIsActive(job.status) || currentObservation && !["completed","stopped"].includes(job.cleanup?.phase)) { renderBackupCleanup(); return; }
  state.backupCleaning = null;
  $("backup-progress").hidden = !state.backupsVisible;
  $("backup-progress").replaceChildren();
  $("backup-progress").classList.toggle("cleanup-stopped", job.status !== "completed" && job.cleanup?.phase !== "completed");
  $("backup-progress").textContent = (task.mode === "discard" ? currentObservation && job.cleanup?.phase === "completed" : job.status === "completed" || job.cleanup?.phase === "completed") ? `Removed recovery data for ${task.name}. ${task.mode === "discard" ? "Backup removed by your choice; library files and job history unchanged." : task.mode === "discard_duplicate" ? "Original preserved; failed job unchanged." : "Replacement verified."}` : `Cleanup stopped for ${task.name}. ${cleanupError(job.cleanup?.error || task.error || job.error)} Remaining recovery data is listed below.`;
  if (state.backupsVisible) await loadBackups();
  await loadJobs();
}
async function refreshBackupCleanup() {
  const task = state.backupCleaning;
  if (!task || task.auth !== state.authRevision) return;
  renderBackupCleanup();
  if (task.polling) return;
  task.polling = true;
  try {
  const result = await api(`operations?${new URLSearchParams({id:task.id})}`);
  if (state.backupCleaning !== task || task.auth !== state.authRevision) return;
  const job = result.jobs?.find(j=>j.id===task.id);
  if (job) {
    // The request can still be pending while hashing. Never interpret the
    // previous failed checkpoint as the result of this cleanup attempt.
    const observed = Date.parse(job.cleanup?.updated_at);
    if (task.commandCompleted || Number.isFinite(observed) && observed >= task.started) task.progress = job.cleanup;
    renderBackupCleanup();
    if (!task.pendingResponse) await finishBackupCleanup(job);
  }
  } finally { task.polling = false; }
}
async function cleanBackup(copy, requestedMode = null) {
  if (state.backupCleaning) return;
  const auth = state.authRevision;
  const mode = requestedMode || (copy.duplicate_available ? "discard_duplicate" : "clean");
  const content = [node("p", backupName(copy), "cleanup-file"), node("p", `${bytes(Number(copy.bytes || 0)+Number(copy.partial_bytes || 0))} ${mode === "discard" ? "will be permanently removed" : "to remove if verification passes"}`, "cleanup-space"), node("p", mode === "discard" ? "Deletes this backup only, without verifying the replacement or repairing the old job. This cannot be undone. Library files are untouched." : mode === "discard_duplicate" ? "Checks that the original and recovery copy match the recorded SHA-256. Removes only redundant recovery data. The original and failed job stay unchanged." : "Checks the active library file and both SHA-256 hashes. If any check fails, the recovery copy stays.")];
  const locations = node("details", "", "cleanup-locations"); locations.append(node("summary", "File locations"), node("p", `Library file: ${copy.original_path || "See job details"}`, "backup-path"), node("p", `Copy to remove: ${copy.path || "See job details"}`, "backup-path")); content.push(locations);
  if (!await reviewAction({title:mode === "discard" ? "Remove recovery copy?" : "Verify and remove extra copy", content, confirmLabel:mode === "discard" ? "Remove copy" : "Verify & remove copy"}) || auth !== state.authRevision) return;
  if (state.backupCleaning) return;
  const task = state.backupCleaning = {id:copy.action_id,name:backupName(copy),mode,auth,started:Date.now(),pendingResponse:true};
  renderBackups();
  renderBackupCleanup();
  try {
    const result = await tool("transcode_backups", {mode,action_id:copy.action_id});
    if (state.backupCleaning !== task || auth !== state.authRevision) return;
    task.pendingResponse = false;
    task.commandCompleted = Boolean(result.command_id);
    const job = result.action || result;
    if (job.status) await finishBackupCleanup(job);
    else await refreshBackupCleanup();
  } catch (error) {
    if (state.backupCleaning !== task || auth !== state.authRevision) return;
    task.pendingResponse = false;
    task.checking = true;
    task.error = error.message;
    renderBackupCleanup();
    // A lost HTTP response does not authorize another cleanup request.
    // Poll the durable job before enabling the control again.
    await safe(refreshBackupCleanup);
    throw error;
  }
}
$("load-backups").addEventListener("click", () => safe(() => loadBackups()));
$("backups-more").addEventListener("click", () =>
  safe(() => loadBackups(true)),
);

let polling = false;
setInterval(async () => {
  if (polling || document.hidden) return;
  polling = true;
  try {
    if (!serverReachable) {
      await safe(reconnect);
      return;
    }
    if ($("workspace").hidden) return;
    resumeCommandTracking();
    await Promise.allSettled([
      !state.workerCheckedAt || Date.now() - state.workerCheckedAt > 10000 ? refreshWorkers() : Promise.resolve(renderWorkers()),
      !state.jobsLoading ? safe(() => loadJobs()) : Promise.resolve(),
      $("job-detail").open ? safe(refreshDetail) : Promise.resolve(),
      state.backupCleaning ? safe(refreshBackupCleanup) : Promise.resolve(),
    ]);
  } finally {
    polling = false;
  }
}, 5000);
if (typeof window !== "undefined") setConnection(serverReachable);

$("podcast-new").addEventListener("toggle", () => { if (!$("podcast-new").open) return; safe(async () => { const data=await api("podcasts"); const select=$("podcast-id"); select.replaceChildren(); for (const id of data.podcasts || []) option(select,id,id); $("podcast-create-status").textContent=data.enabled && data.podcasts?.length ? "" : "Enable a podcast in the server configuration first."; }); });
$("podcast-form").addEventListener("submit", event => { event.preventDefault(); safe(async () => { const result=await tool("action_run",{action:"clean_podcast_ads",inputs:JSON.stringify({podcast_id:$("podcast-id").value,path:$("podcast-source").value,output_path:$("podcast-output").value})}); $("podcast-new").open=false; await loadJobs(); await openJob(result.id || result.action?.id); }); });
async function reviewPodcastCuts(id) {
 const content=[]; let offset=0,review;
 do { review=await tool("podcast_review",{id,offset}); if (!review.digest) throw new Error("Could not read cut review."); for (const b of review.boundaries || []) content.push(node("p",`${(b.cut.start_ms/1000).toFixed(2)}–${(b.cut.end_ms/1000).toFixed(2)} s · ${b.cut.first_id}–${b.cut.last_id}: ${b.first.text} … ${b.last.text}. Before: ${b.before.map(u=>u.text).join(" ")} · After: ${b.after.map(u=>u.text).join(" ")}`,"metadata")); offset=review.next_offset; } while(review.has_more);
 content.unshift(node("p",`Remove ${(review.removed_ms/1000).toFixed(1)} seconds in ${review.total_cuts} cuts. A separate MP3 will be validated and published; the original is kept. This is a text boundary review, not a listening test.`));
 if (await reviewAction({title:"Review podcast cuts",content,confirmLabel:"Approve cuts and render"})) { await tool("podcast_review",{id,digest:review.digest,approve:true}); await jobControl(id,"action_resume",{decision:"render"}); }
}
async function revisePodcastLabels(id,digest) {
 if (!digest) { const review=await tool("podcast_review",{id,offset:0}); if (!review.digest || review.approved) throw new Error("Only unapproved cuts can be revised."); digest=review.digest; }
 await tool("podcast_review",{id,digest,reclassify:true});
 await loadJobs(); await openJob(id);
}
async function viewPodcastTranscript(id) {
 const content=[node("p","Your connected LLM reads these blocks through podcast_block and saves labels through podcast_classify. All blocks and overlaps must be covered before cuts can be planned.")]; let offset=0,manifest;
 do { manifest=await tool("podcast_blocks",{id,offset}); for (const b of manifest.blocks || []) content.push(button(`${b.id} · ${b.first_id}–${b.last_id} · ${b.classified ? "Classified" : "Pending"}`,()=>safe(async()=>{ const pages=[]; let at=0,page; do { page=await tool("podcast_block",{id,block_id:b.id,offset:at}); pages.push(...page.units.map(u=>node("p",`${u.id} · ${u.text}`,"metadata"))); at=page.next_offset; } while(page.has_more); await reviewAction({title:`Transcript ${b.id}`,content:pages,confirmLabel:"Close"}); }))); offset=manifest.next_offset; } while(manifest.has_more);
 await reviewAction({title:"Podcast transcript",content,confirmLabel:"Close"});
}

async function viewPodcastAdLibrary(podcastId) {
 const content=[node("p","Known ads are scoped to this podcast. Audio and original transcript text must both match. Revoking a reference blocks pending cuts that depend on it; reprocess those episodes to review them again.")]; let offset=0,page;
 do { page=await tool("podcast_ad_library",{podcast_id:podcastId,offset}); for (const ref of page.references || []) { const row=node("div"); row.append(node("p",`${ref.label} · ${(ref.duration_ms/1000).toFixed(2)} s · ${ref.first_id}–${ref.last_id} · ${ref.revoked ? "Revoked" : "Active"}`,"metadata")); row.append(node("p",ref.id,"metadata")); if (!ref.revoked) row.append(button("Revoke reference",()=>safe(async()=>{ await tool("podcast_ad_library",{podcast_id:podcastId,revoke:ref.id}); row.replaceChildren(node("p","Reference revoked. Pending episodes that used it need a new review.","metadata")); }))); content.push(row); } offset=page.next_offset; } while(page.has_more);
 if (!page.total_references) content.push(node("p","The library will learn confirmed ads when an approved episode finishes processing."));
 await reviewAction({title:"Podcast ad library",content,confirmLabel:"Close"});
}

safe(initialize);
