const $ = (id) => document.getElementById(id);
const goalNames = {size: "Save space", compact: "Shrink to 1080p SDR", quality: "Strict quality", x265_preserve: "Convert to HEVC"};
const moreGoals = ["quality", "x265_preserve"];
const state = {
  tab: "library",
  media: null,
  librarySeason: null,
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
  detailAutoOpened: new Set(),
  openFiles: new Set(),
  detailCostsOpen: false,
  authRevision: 0,
  authMode: "token",
  busyJobs: new Set(),
  submitting: false,
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
// A button that only opens something stays usable while its job is busy.
function jobControlDisabled(b) {
  if (!serverReachable) return true;
  if (b.dataset.navigation === "true") return false;
  return jobIsBusy(b.dataset.jobControl) || (b.dataset.requiresWorker === "true" && !state.workerInfo?.ready);
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
    const spinner = node("span", "", "spinner");
    spinner.dataset.spinner = "";
    spinner.setAttribute("aria-hidden", "true");
    b.prepend ? b.prepend(spinner) : b.append(spinner);
    try { return await result; }
    finally {
      spinner.remove?.();
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
    "Change goal": "M4 7h16M4 17h16M8 4v6M16 14v6",
    "Try another profile": "M4 7h16M4 17h16M8 4v6M16 14v6",
    Retry: "M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M18 18A8 8 0 0 1 4 12",
    "Run again": "M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M18 18A8 8 0 0 1 4 12",
    "Try with x265": "M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M18 18A8 8 0 0 1 4 12",
    "Convert to SDR 1080p": "M4 7h16M4 17h16M8 4v6M16 14v6",
    "Make it smaller": "M4 7h16M4 17h16M8 4v6M16 14v6",
    Review: "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Review preview": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Start batch": "M7 4l13 8-13 8Z",
    Cancel: "M6 6l12 12M6 18L18 6",
    "Replace file": "M4 7h16M16 3l4 4-4 4M20 17H4M8 13l-4 4 4 4",
    "Review replacements": "M9 12l2 2 4-4M5 4h14v16H5Z",
    "Review replacement": "M9 12l2 2 4-4M5 4h14v16H5Z",
    "Review candidates": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Review candidate": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Choose a goal": "M8 5h12v15H8ZM4 16V2h12",
    "Choose tracks": "M4 7h16M4 17h16M9 4v6M15 14v6",
    // Keeping originals is a decision, not filing the job away: a shield, not a box.
    "Keep original": "M12 3l7 3v6c0 4-3 7-7 9-4-2-7-5-7-9V6ZM9 12l2 2 4-4",
    "Keep originals": "M12 3l7 3v6c0 4-3 7-7 9-4-2-7-5-7-9V6ZM9 12l2 2 4-4",
    "Stop batch · keep originals": "M6 6h12v12H6Z",
    "Compare frames": "M4 5h16v14H4ZM12 5v14",
    "Run one more test": "M9 3h6M10 3v6l-5 9a2 2 0 0 0 2 3h10a2 2 0 0 0 2-3l-5-9V3",
    "Try again": "M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M18 18A8 8 0 0 1 4 12",
    "Show in Files": "M3 6h6l2 2h10v11H3Z",
    "Check library again": "M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14ZM20 20l-4-4",
    "Ad library": "M4 6h16M4 12h16M4 18h10",
    "Review cuts": "M9 7a3 3 0 1 1-6 0 3 3 0 0 1 6 0ZM9 17a3 3 0 1 1-6 0 3 3 0 0 1 6 0ZM8.5 8.5 20 19M8.5 15.5 20 5",
    "Revise labels": "M4 20h4L19 9l-4-4L4 16Z",
    "View transcript": "M5 4h14v16H5ZM8 8h8M8 12h8M8 16h5",
    "Validate classifications": "M5 12l5 5 9-10",
    Archive: "M4 4h16v4H4ZM6 8v12h12V8M10 12h4",
    Restore: "M12 19V9M8 13l4-4 4 4M4 4h16v4H4Z",
    // Opening another job reads as "look", not "review": an eye, a play frame
    // and stacked layers keep them apart from Review and Choose a goal.
    "View replacement": "M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12ZM12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6Z",
    "View batch": "M12 3l9 5-9 5-9-5ZM3 13l9 5 9-5",
    "View preview": "M4 5h16v14H4ZM10 9l5 3-5 3Z",
    "Accept quality loss": "M12 3 2 21h20ZM12 9v5M12 17v1",
    "Pause batch": "M8 4v16M16 4v16",
    "Resume batch": "M7 4l13 8-13 8Z",
    "Restore built-in profile": "M4 4v5h5M4 9a8 8 0 1 1 1 9",
    "Delete custom profile":
      "M3 6h18M9 6V3h6v3M6 6l1 15h10l1-15M10 10v7M14 10v7",
    Folder: "M3 6h7l2 2h9v12H3Z",
    // Command status: applied and failed.
    Applied: "M5 12l5 5 9-10",
    Failed: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18ZM12 7.5v5.5M12 16.5v.5",
    "More actions": "M5 11a1 1 0 1 1 0 2 1 1 0 0 1 0-2ZM12 11a1 1 0 1 1 0 2 1 1 0 0 1 0-2ZM19 11a1 1 0 1 1 0 2 1 1 0 0 1 0-2Z",
    // Fallback for job actions without their own icon (new server decisions).
    Action: "M5 12h14M13 6l6 6-6 6",
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
  if (v < 0) return `−${bytes(-v)}`;
  if (v >= 1e12) return `${(v / 1e12).toFixed(2)} TB`;
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
    button.disabled = jobControlDisabled(button);
  });
  document.querySelectorAll(".dialog-connection").forEach((notice) => {
    notice.hidden = online;
  });
  syncBatchReview();
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
    if (!document.hidden) { safe(reconnect); resumeFolderSizes(); }
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
  clearTimeout(state.folderSizesTimer);
  state.libraryRevision++;
  state.jobsRevision++;
  state.detailRevision++;
  state.detail = null;
  state.batchReview = null;
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
  $("login").hidden = accessAuthenticated() || state.authMode !== "token";
  $("access-expired").hidden = !accessAuthenticated();
  $("workspace").hidden = true;
  $("logout").hidden = true;
  $("local-session").hidden = true;
}
// Access-only mode, or token mode whose browser arrived signed in through Access:
// either way an expired Access login is renewed by reloading, not by the token form.
function accessAuthenticated() {
  return state.authMode === "cloudflare_access" || state.accessSession === true;
}
function authenticationMessage() {
  return accessAuthenticated()
    ? "Cloudflare Access sign-in required. Reload to sign in."
    : "sign in to Navigatorr";
}
async function api(path, body, { signal, method } = {}) {
  if (body !== undefined && !serverReachable)
    throw new Error(
      "Offline. Changes are unavailable until the server responds.",
    );
  let res;
  try {
    res = await fetch(`/api/maintenance/${path}`, {
      method: method || (body === undefined ? "GET" : "POST"),
      redirect: "manual",
      headers: {
        "Content-Type": "application/json",
        "X-Navigatorr-Request": "1",
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
  } catch (err) {
    // A request the console cancelled says nothing about the server.
    if (signal?.aborted) throw err;
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
    if (accessAuthenticated() || path === "auth-info") {
      if (!accessAuthenticated()) state.authMode = "cloudflare_access";
      invalidateAuthentication();
      throw new Error(authenticationMessage());
    }
    throw new Error("Unexpected API redirect. Reload the application.");
  }
  const contentType = res.headers?.get("Content-Type") || "";
  if (
    accessAuthenticated() &&
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
  if (!res.ok) throw Object.assign(new Error(data.error || `HTTP ${res.status}`), { status: res.status });
  return data;
}
async function tool(name, args = {}, onAccepted, decision) {
  const mutates = ["action_run","action_resume","action_retry","action_cancel"].includes(name) || name === "transcode_backups" && ["clean","discard_duplicate","discard","restore"].includes(args.mode);
  const result = mutates
    ? await commandRequest("tool", {name,arguments:args,background:true}, onAccepted, decision)
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

const commandLabels = {prepare_batch_promotion:"Preparing replacement review",review_batch_promotion:"Applying replacement decision",resume:"Applying decision",retry:"Retrying job",cancel:"Stopping job",candidate:"Applying candidate decision",clean:"Verifying recovery data",discard_duplicate:"Verifying duplicate",discard:"Removing recovery copy",restore:"Moving original back",reconfigure:"Updating batch settings"};
function readCommandReceipts() {
  try { return JSON.parse(sessionStorage.getItem("navigatorr_commands") || "{}"); } catch { return {}; }
}
function commandTarget(body) { return body.id || body.arguments?.id || body.arguments?.action_id; }
function trackCommand(request, receipt, result) {
  state.commands ||= new Map();
  const previous = state.commands.get(receipt.command_id);
  const info = {request,...receipt,...result,id:result.id || receipt.id,status:result.status || "pending"};
  // When this page first saw the current status: Applied shows for a while
  // after completion, measured from here rather than with a timer per job.
  info.seenAt = previous?.status === info.status ? previous.seenAt : commandClock();
  if (previous?.dismissed) info.dismissed = true;
  state.commands.set(receipt.command_id, info);
  // After a reload the first poll replaces a placeholder; only an outcome is news.
  const placeholder = previous?.restored && commandActiveStatuses.includes(info.status);
  if (previous && previous.status !== info.status && !placeholder) announceCommand(info);
  renderCommandIndicators();
  document.querySelectorAll("[data-job-control]").forEach(button => {
    if (button.dataset.jobControl === info.id) button.disabled = jobControlDisabled(button);
  });
  return info;
}
// Command status on a job card: one compact chip under the status badge.
// Monotonic, so a wall-clock change cannot keep Applied up or hide it early.
const commandClock = () => globalThis.performance?.now?.() ?? Date.now();
const commandActiveStatuses = ["pending", "running", "waiting_external"];
const appliedChipMs = 8000;
const commandVerbs = {"Preparing replacement review":"Preparing review","Applying replacement decision":"Applying decision","Applying decision":"Applying decision","Retrying job":"Retrying","Stopping job":"Stopping","Applying candidate decision":"Applying decision","Verifying recovery data":"Verifying","Verifying duplicate":"Verifying","Removing recovery copy":"Removing copy","Updating batch settings":"Updating settings"};
const commandFailures = {"Preparing replacement review":"Review failed","Applying replacement decision":"Decision failed","Applying decision":"Decision failed","Retrying job":"Retry failed","Stopping job":"Stop failed","Applying candidate decision":"Decision failed","Verifying recovery data":"Check failed","Verifying duplicate":"Check failed","Removing recovery copy":"Removal failed","Updating batch settings":"Update failed"};
const commandStatusText = {pending:"Waiting to apply",running:"Applying",waiting_external:"Waiting to apply",completed:"Applied",failed:"Failed",cancelled:"Cancelled"};
// A command that carries the decision it was sent for names it (for example
// "Replace 3 files"); the others use the label of their kind.
function commandLabel(command) { return command.decision?.label || commandLabels[command.kind] || command.label || "Update"; }
// The engine keeps waiting_reason when a command runs again after a wait, and
// a cancelled one stores its cancel reason there. Only a waiting one says why.
function commandWaitingReason(command) { return command.status === "waiting_external" ? command.waiting_reason || "" : ""; }
function jobCommands(id) {
  return [...(state.commands?.values() || [])].filter((c) => c.id === id).sort((a, b) => (b.started || 0) - (a.started || 0));
}
// A failed command wins (until the job's next command or a dismissal), then
// one still in flight, then one completed within the last few seconds.
// Cancelled commands show no chip.
function commandChip(id, now = commandClock()) {
  const commands = jobCommands(id);
  const newest = commands[0];
  if (!newest) return null;
  const label = commandLabel(newest);
  if (newest.status === "failed" && !newest.dismissed)
    return {kind:"failed", command:newest, text:newest.decision?.failure || commandFailures[label] || "Action failed", title:`${label} · Failed`};
  const active = commands.find((c) => !c.status || commandActiveStatuses.includes(c.status));
  if (active) {
    const activeLabel = commandLabel(active);
    const text = active.status === "waiting_external" ? "Waiting to apply" : active.decision?.verb || commandVerbs[activeLabel] || "Applying";
    const reason = commandWaitingReason(active);
    return {kind:"active", command:active, text, title:`${activeLabel} · ${commandStatusText[active.status] || "Applying"}${reason ? ` · ${reason}` : ""}`};
  }
  const done = commands.find((c) => c.status === "completed");
  if (done && now - (done.seenAt || 0) < appliedChipMs)
    return {kind:"applied", command:done, text:"Applied", title:`${commandLabel(done)} · Applied`, expires:(done.seenAt || 0) + appliedChipMs};
  return null;
}
function commandSymbol(kind) {
  if (kind === "active") {
    const spinner = node("span", "", "command-spinner");
    spinner.setAttribute("aria-hidden", "true");
    return spinner;
  }
  return actionIcon({applied:"Applied", failed:"Failed", cancelled:"Cancel"}[kind]);
}
function commandChipNode(chip) {
  const el = node("span", "", `command-chip ${chip.kind}`);
  el.title = chip.title;
  const symbol = commandSymbol(chip.kind);
  if (symbol) el.append(symbol);
  el.append(node("span", chip.text));
  return el;
}
function commandFailureLine(command) {
  const line = node("p", "", "command-failure");
  line.dataset.commandId = command.command_id;
  const reason = shortJobReason(command.error) || "The action could not be applied.";
  const text = node("span", reason);
  text.title = reason;
  // The redraw removes the line and moves focus to the same card.
  const dismiss = button("", () => {
    command.dismissed = true;
    renderCommandIndicators();
  }, "command-dismiss");
  dismiss.setAttribute("aria-label", "Dismiss");
  dismiss.title = "Dismiss";
  const icon = actionIcon("Cancel");
  if (icon) dismiss.append(icon);
  line.append(text, dismiss);
  return line;
}
function commandJobName(id) {
  const job = state.detailJob?.id === id ? state.detailJob : [...(state.operationJobs?.values() || [])].find((j) => j.id === id);
  if (job?.number) return `Job #${job.number}`;
  return state.backups?.some((copy) => copy.action_id === id) ? "Recovery copy" : "Job";
}
// Screen readers hear a status change once, not every time the queue redraws
// its cards. Changes that land together are read as one message.
const commandAnnouncements = [];
function announceCommand(command) {
  const status = (commandStatusText[command.status] || "Applying").toLowerCase();
  const reason = command.status === "failed" ? shortJobReason(command.error) : commandWaitingReason(command);
  const detail = reason ? ` ${/[.!?]$/.test(reason) ? reason : `${reason}.`}` : "";
  commandAnnouncements.push(`${commandJobName(command.id)}: ${commandLabel(command)}, ${status}.${detail}`);
  if (commandAnnouncements.length === 1) setTimeout(flushCommandAnnouncements, 250);
}
function flushCommandAnnouncements() {
  const text = commandAnnouncements.splice(0).join(" ");
  // The open job detail is modal and makes the page behind it inert, so
  // speak from the live region inside the dialog while it is open.
  const inDialog = Boolean($("job-detail").open);
  $("detail-command-announcer").textContent = inDialog ? text : "";
  $("command-announcer").textContent = inDialog ? "" : text;
}
// Keyboard focus on a Dismiss button survives redraws of the cards.
function commandFocus() {
  const line = document.activeElement?.closest?.(".command-failure");
  if (!line) return null;
  return {commandId: line.dataset.commandId, operationId: line.closest("[data-operation-id]")?.dataset.operationId};
}
function commandTargets() {
  return [...document.querySelectorAll("[data-operation-id]")].filter((target) => target.id !== "detail-summary");
}
function restoreCommandFocus(focus) {
  if (!focus) return;
  const targets = commandTargets();
  const line = targets.map((target) => target.querySelector(".command-failure")).find((l) => l?.dataset.commandId === focus.commandId);
  const card = targets.find((target) => target.dataset.operationId === focus.operationId);
  const next = line?.querySelector(".command-dismiss") || card?.querySelector(".job-title, .backup-actions button");
  if (next && document.activeElement !== next) next.focus({ preventScroll: true });
}
let commandChipTimer = null;
function renderCommandIndicators() {
  const focus = commandFocus();
  const now = commandClock();
  let expires = Infinity;
  for (const target of commandTargets()) {
    let chip = commandChip(target.dataset.operationId, now);
    // The backup cleanup notice already says that work is under way.
    if (chip?.kind === "active" && target.querySelector(".job-command")) chip = null;
    if (chip?.expires) expires = Math.min(expires, chip.expires);
    // Leave an unchanged chip or failure line in place so focus stays put.
    const key = chip ? JSON.stringify([chip.kind, chip.command.command_id, chip.command.status, chip.text, chip.title]) : "";
    const current = target.querySelector(".command-chip");
    if ((current?.dataset.key || "") !== key) {
      current?.remove();
      if (chip) {
        const next = commandChipNode(chip);
        next.dataset.key = key;
        (target.querySelector(".job-heading, .backup-heading") || target).append(next);
      }
    }
    const failed = chip?.kind === "failed" ? chip.command.command_id : "";
    const line = target.querySelector(".command-failure");
    if ((line?.dataset.commandId || "") !== failed) {
      line?.remove();
      if (failed) {
        const next = commandFailureLine(chip.command);
        const footer = target.querySelector(":scope > .job-footer, :scope > .backup-actions");
        if (footer) footer.before(next); else target.append(next);
      }
    }
  }
  renderCommandHistory();
  restoreCommandFocus(focus);
  // One redraw when the next Applied chip expires, in case nothing else redraws.
  clearTimeout(commandChipTimer);
  commandChipTimer = expires < Infinity ? setTimeout(renderCommandIndicators, Math.max(0, expires - now) + 50) : null;
}
function commandAge(started, now = Date.now()) {
  if (!started) return "";
  const seconds = Math.max(0, Math.round((now - started) / 1000));
  if (seconds < 10) return "Just now";
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
  return `${Math.floor(seconds / 3600)} h ago`;
}
// Job detail: every command sent for this job in this session, newest first.
function commandHistoryItem(command, now) {
  const kind = command.status === "completed" ? "applied" : command.status === "failed" ? "failed" : command.status === "cancelled" ? "cancelled" : "active";
  const item = node("li", "", `command-history-item ${kind}`);
  const symbol = commandSymbol(kind);
  if (symbol) item.append(symbol);
  const body = node("div", "", "command-history-body");
  const head = node("p", "", "command-history-label");
  head.append(node("span", commandLabel(command)), node("span", ` · ${commandStatusText[command.status] || "Applying"}`, "command-history-status"));
  body.append(head);
  const detail = command.status === "failed" ? shortJobReason(command.error) : commandWaitingReason(command);
  if (detail) body.append(node("p", detail, "metadata"));
  item.append(body);
  const age = commandAge(command.started, now);
  if (age) item.append(node("span", age, "metadata command-history-age"));
  return item;
}
function renderCommandHistory(now = Date.now()) {
  const commands = state.detail ? jobCommands(state.detail) : [];
  $("detail-commands").hidden = !commands.length;
  $("detail-command-list").replaceChildren(...commands.map((command) => commandHistoryItem(command, now)));
  syncDetailSections();
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
    trackCommand(request,receipt,{status:"pending"}).restored = true;
    if (receipt.cleanup && !state.backupCleaning) state.backupCleaning = {...receipt.cleanup,auth:state.authRevision,command_id:receipt.command_id,pendingResponse:true};
    void safe(()=>waitCommand(request,receipt));
  }
  renderBackups(); renderBackupCleanup();
}
async function commandRequest(path, body, onAccepted, decision) {
  const request = JSON.stringify([path,body]);
  const receipts = readCommandReceipts();
  const receipt = receipts[request] ||= {key:submissionID(),started:Date.now()};
  sessionStorage.setItem("navigatorr_commands",JSON.stringify(receipts));
  const result = receipt.command_id ? {command_id:receipt.command_id,id:receipt.id} : await api(path,{...body,key:receipt.key});
  if (!result.command_id) { delete receipts[request]; sessionStorage.setItem("navigatorr_commands",JSON.stringify(receipts)); return result; }
  receipt.command_id = result.command_id; receipt.id = result.id || commandTarget(body);
  receipt.label = commandLabels[body.kind || body.arguments?.mode || body.name?.replace("action_","")] || "Update";
  if (decision) receipt.decision = decision;
  if (state.backupCleaning?.id === receipt.id) {
    const task = state.backupCleaning;
    receipt.cleanup = {id:task.id,name:task.name,mode:task.mode,started:task.started};
    task.command_id = receipt.command_id;
  }
  const latest = readCommandReceipts(); latest[request] = receipt;
  sessionStorage.setItem("navigatorr_commands",JSON.stringify(latest));
  trackCommand(request,receipt,{status:"pending"});
  onAccepted?.();
  notify("Request accepted. Its status shows on the job.");
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
  const season = q.get("season");
  return {
    tab:routeTabs[q.get("view")] || "library", explicit:q.has("view"),
    source:q.get("source") || "", folder:q.get("folder") || "",
    media:/^[1-9]\d*$/.test(q.get("media") || "") ? Number(q.get("media")) : null,
    season:season === "unknown" ? season : /^(0|[1-9]\d*)$/.test(season || "") && Number.isSafeInteger(Number(season)) ? Number(season) : null,
    file:q.get("file") || "", search:(q.get("q") || "").slice(0,256),
    sort:["name","name_desc","size_asc","size_desc"].includes(q.get("sort")) ? q.get("sort") : "name",
    status:routeStatuses.includes(q.get("status")) ? q.get("status") : "all",
    job:(q.get("job") || "").slice(0,200), profile:(q.get("profile") || "").slice(0,128),
    copies:q.get("copies") === "1", configure:q.get("configure") === "1", locate:q.get("locate") === "1",
    mode:["size","quality","x265_preserve","compact"].includes(q.get("mode")) ? q.get("mode") : "size", modeExplicit:q.has("mode"), technical:q.get("technical") === "1",
  };
}
function navigationQuery() {
  const q = new URLSearchParams({view:Object.entries(routeTabs).find(([,tab])=>tab===state.tab)?.[0] || "files"});
  if (processingMode() !== "size") q.set("mode", processingMode());
  if ($("technical").checked) q.set("technical", "1");
  if ($("service").value) q.set("source",$("service").value);
  if ($("service").value.startsWith("folder:") && state.folder) q.set("folder",state.folder);
  if (state.media?.id) q.set("media",state.media.id);
  if (seriesSeasonSelected()) q.set("season",state.librarySeason);
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
// locateFileRow ticks a file's row in the folder list and scrolls to it, loading
// further pages of the folder until the row is there. The encoding form is not
// opened: step 2 is the person's to ask for.
async function locateFileRow(path, current = () => true) {
  const row = () => fileChecks().find(input => input.dataset.selection === path);
  for (let pages = 0; !row() && state.libraryHasMore && pages < 40; pages++) {
    await loadLibrary(true);
    if (!current()) return;
  }
  const check = row();
  if (!check) {
    if (current()) notify("That file is not in the list shown. Its folder is open.");
    return;
  }
  selectOneFile(path);
  fileSelectionChanged();
  const line = check.closest(".media-row");
  document.querySelectorAll?.(".media-row.located").forEach(n => n.classList.remove("located"));
  line?.classList.add("located");
  line?.scrollIntoView?.({block: "center"});
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
    setProcessingMode(route.mode || "size");
    if (route.modeExplicit) state.goalExplicit = true;
    const sources = [...state.info.roots.map(r=>`folder:${r}`), ...state.info.services.map(s=>s.name)];
    $("service").value = sources.includes(route.source) ? route.source : sources[0] || "";
    resetLibrary();
    $("library-sort").value = route.sort;
    renderLibrarySort();
    $("job-filter").value = route.status;
    $("search").value = route.search;
    setLibrarySearchMode(Boolean(route.search));
    if ($("service").value.startsWith("folder:")) state.folder = route.folder || $("service").value.slice(7);
    else if (route.media && $("service").value) {
      try {
        const result = await api(`library?${new URLSearchParams({service:$("service").value,id:route.media,title:"1"})}`);
        if (!current()) return;
        if (result.media?.id !== route.media) throw new Error("This library title is unavailable.");
        state.media = result.media;
        state.librarySeason = $("service").value === "sonarr" ? route.season ?? null : null;
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
        const query = new URLSearchParams(folder ? {path:state.folder,q:route.file.split("/").pop(),limit:100} : {service:$("service").value,id:state.media?.id || "",q:route.file.split("/").pop(),limit:100});
        if (!folder && seriesSeasonSelected()) query.set("season",state.librarySeason);
        const page = await api(`${folder ? "folder" : "library"}?${query}`);
        if (!current()) return;
        const file = page.items?.find(f=>(f.path || "")===route.file && !f.is_dir);
        if (file && route.locate && !route.configure && folder) {
          // Only where the file is: ticked in its folder and scrolled to, with its
          // encoding form left for the person to open.
          await locateFileRow(file.path, current);
        } else if (file) {
          state.file = file; state.fileMedia = state.media; state.fileService = folder ? null : $("service").value;
          $("path").value = file.path; $("scope").value = "file"; setFileStep("configure"); controls();
        } else notify("The selected file is unavailable. Showing its folder or title.");
      }
    } else if (tab === "library") $("library-items").replaceChildren(node("p", "No media folders configured.", "empty"));
    if (!current()) return;
    setProcessingMode(route.mode || "size");
    $("technical").checked = Boolean(route.technical);
    controls();
    if (tab === "recipes" && route.profile && !(recipeIsDirty() && route.profile === state.recipe?.name)) await readRecipe(route.profile);
    if (tab === "advanced" && route.copies) await loadBackups();
    if (tab === "jobs" || tab === "stats") await loadJobs();
    if (tab === "stats") void safe(loadStorage);
    if (current() && route.job) await openJob(route.job);
  } finally {
    if (revision === state.routeRevision) { state.applyingNavigation=false; writeNavigation(true); }
  }
}
if (typeof window !== "undefined") window.addEventListener("popstate", () => {
  if (state.routeReady && !$("workspace").hidden) safe(() => applyNavigation(navigationRoute(window.location.search)));
});
// rememberJobsScroll follows the Activity list while it is on screen, so the
// place it was left at is known even when the browser has already moved the
// page by the time the tab changes (the Back button).
function rememberJobsScroll() {
  if (state.tab !== "jobs" || state.jobsRestore != null || typeof window === "undefined") return;
  state.jobsScroll = window.scrollY || 0;
  state.jobsScrollFilter = $("job-filter").value;
}
if (typeof window !== "undefined") window.addEventListener("scroll", rememberJobsScroll, {passive: true});
function selectTab(tab, fromRoute = false) {
  if (!fromRoute && state.applyingNavigation) { state.routeRevision++; state.applyingNavigation=false; }
  // Leaving Activity remembers how far down it was, so coming back (the Back
  // button, or its tab) lands on the same job instead of the top.
  const cameFrom = state.tab;
  state.tab = tab;
  document
    .querySelectorAll(".tab-content")
    .forEach((n) => (n.hidden = n.id !== tab));
  document
    .querySelectorAll("[data-tab]")
    .forEach((n) =>
      n.setAttribute("aria-current", n.dataset.tab === tab ? "page" : "false"),
    );

  state.jobsRestore = tab === "jobs" && cameFrom !== "jobs" && state.jobsScroll > 0 && state.jobsScrollFilter === $("job-filter").value ? state.jobsScroll : null;
  if (typeof window !== "undefined") window.scrollTo({ top: state.jobsRestore || 0 });
  writeNavigation();
  if (["jobs", "stats"].includes(tab) && !fromRoute) safe(loadJobs);
  if (tab !== "stats") clearTimeout(state.storageTimer);
  else if (!fromRoute) safe(loadStorage);
  // The node's job settings come from the worker activity report.
  if (tab === "advanced" && (!state.activityCheckedAt || Date.now() - state.activityCheckedAt > 5000)) void refreshWorkerActivity();
  if (tab === "advanced") void loadKeepLanguages();
  if (tab === "library" && state.libraryNeedsLoad && !fromRoute) safe(loadLibrary);
  else if (tab === "library") void refreshFileMarks();
  if (tab === "library" && $("service").value.startsWith("folder:") && state.folderSizeTargets?.size) {
    resumeFolderSizes();
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
    if (serverReachable && worker.admission_note) row.append(node("p", worker.admission_note, "worker-admission"));
    row.append(node("p", `Scheduler: ${fresh ? worker.scheduler_health || "unknown" : "unknown (outdated observation)"}`, "metadata"));
    row.append(node("p", `Last successful sweep: ${worker.last_success_at ? new Date(worker.last_success_at).toLocaleString() : "Unknown"}`, "metadata"));
    row.append(node("p", `Observation: ${worker.observed_at ? new Date(worker.observed_at).toLocaleString() : "Unknown"}${Number.isFinite(Date.parse(worker.observed_at)) ? ` · ${Math.max(0,Math.floor((Date.now()-Date.parse(worker.observed_at))/1000))}s old` : ""}`, "metadata"));
    for (const [name, sweep] of Object.entries(worker.sweeps || {})) row.append(node("p", `${name === "post_encode" ? "Post-encode" : "Queue"} sweep: ${fresh && sweep.observed_at ? sweep.last_error || sweep.consecutive_errors > 0 ? "degraded" : "normal" : "unknown"}${sweep.consecutive_errors ? ` · ${sweep.consecutive_errors} consecutive errors` : ""}`, "metadata"));
    if (worker.last_error?.message) row.append(node("p", `${fresh ? "Last error" : "Previous error"}: ${worker.last_error.class || "scheduler"} · ${worker.last_error.message}`, "muted"));
    host.append(row);
  }
  if (!info) host.append(node("p", serverReachable ? "Checking shared worker observation…" : "Server disconnected; worker and scheduler status are unknown. Jobs may continue remotely.", "muted"));
}
// Pause all (worker drain): nothing new goes to the workers, files already
// encoding finish and are saved, and paused batches stay paused after Resume.
// The header, the Queue and the workers dialog all read this one summary.
const filesWord = n => `${n} ${n === 1 ? "file" : "files"}`;
function drainSummary(drain) {
  if (!drain?.enabled) return null;
  if (drain.ready_for_deploy) return {state: "ready", label: "Paused", text: "Paused — ready to deploy"};
  const parts = [];
  const waiting = Math.min(drain.waiting || 0, drain.running || 0);
  const encoding = (drain.running || 0) - waiting;
  if (encoding) parts.push(`${filesWord(encoding)} still encoding`);
  if (waiting) parts.push(`${waiting} waiting for a turn on a worker`);
  if (drain.publication_pending) parts.push(`${filesWord(drain.publication_pending)} encoded but not saved yet`);
  return {state: "pausing", label: "Pausing", text: `Pausing — ${parts.join(", ") || "checking the workers"}`};
}
const drainJobLabels = {running: "Encoding", waiting: "Waiting for a turn", publication_pending: "Not saved yet"};
function drainJobRow(job) {
  const row = node("div", "", "worker-drain-job");
  row.append(node("strong", job.file || job.action_id), node("span", `${drainJobLabels[job.waiting ? "waiting" : job.state]}${job.worker ? ` · ${job.worker}` : ""}`, "badge"));
  const actions = node("div", "", "batch-file-actions");
  if (job.state === "publication_pending") {
    const retry = button("Try again", () => jobControl(job.action_id, "action_resume", {decision: "recheck"}, {decision: {label: "Try again", verb: "Trying to save the file again", failure: "Try again failed"}}).then(() => refreshWorkers()), "primary");
    retry.dataset.jobControl = job.action_id;
    retry.dataset.requiresWorker = "true";
    retry.disabled = jobControlDisabled(retry);
    actions.append(retry);
  }
  const view = button("View file", () => { $("workers-dialog").close(); return openJob(job.action_id); });
  view.dataset.jobControl = job.action_id;
  view.dataset.navigation = "true";
  actions.append(view);
  if (job.path) {
    const files = button("Show in Files", () => { $("workers-dialog").close(); return showInFiles(job.path); });
    files.dataset.jobControl = job.action_id;
    files.dataset.navigation = "true";
    actions.append(files);
  }
  row.append(actions);
  return row;
}
function renderWorkerDrain(info) {
  const drain = serverReachable ? info?.drain : null;
  const summary = drainSummary(drain);
  const label = drain?.enabled ? "Resume" : "Pause all";
  const toggle = $("queue-drain-toggle");
  toggle.hidden = !drain;
  toggle.title = label;
  toggle.setAttribute("aria-label", label);
  toggle.classList.toggle("on", Boolean(drain?.enabled));
  $("queue-drain-label").textContent = label;
  // One status line: a dot and what the workers are doing, ahead of "Updated".
  $("queue-meta").dataset.state = summary?.state || "running";
  $("queue-drain-dot").hidden = !drain;
  $("queue-drain-status").hidden = !drain;
  $("queue-drain-status").textContent = summary ? `${summary.text}${drain.queued ? ` · ${filesWord(drain.queued)} held back` : ""}` : "Running";
  $("worker-drain").hidden = !drain;
  if (!drain) return;
  const open = (drain.jobs || []).filter(job => job.state !== "queued");
  $("worker-drain-status").textContent = summary
    ? summary.text
    : "Pause all stops new files from reaching the workers while the ones already encoding finish. Use it before deploying a worker.";
  $("worker-drain-jobs").replaceChildren(...open.slice(0, 10).map(drainJobRow));
  if (open.length > 10) $("worker-drain-jobs").append(node("p", `And ${open.length - 10} more.`, "metadata"));
  const parts = [];
  if (drain.enabled) {
    for (const worker of drain.workers || []) parts.push(`${worker.name}: ${worker.ready_for_deploy ? "ready for deploy" : worker.reason || "not ready"}`);
    parts.push("Paused batches stay paused after Resume.");
  }
  $("worker-drain-note").textContent = parts.join(" · ");
  $("worker-drain-toggle").textContent = label;
  $("worker-drain-toggle").classList.toggle("primary", Boolean(drain.enabled));
  $("worker-drain-toggle").disabled = !serverReachable;
  $("queue-drain-toggle").disabled = !serverReachable;
}
// One switch for the whole system, from the Queue or the workers dialog.
function toggleWorkerDrain() {
  return safe(async () => {
    const enabled = !state.workerInfo?.drain?.enabled;
    const toggles = [$("queue-drain-toggle"), $("worker-drain-toggle")];
    toggles.forEach(toggle => { toggle.disabled = true; });
    try {
      const drain = await api("worker-drain", {enabled});
      state.workerInfo = {...state.workerInfo, drain};
      renderWorkers();
      notify(enabled ? "Paused. Nothing new goes to the workers; files already encoding finish." : "Resumed. Held files start again; paused batches stay paused.");
      await refreshWorkers();
    } finally {
      toggles.forEach(toggle => { toggle.disabled = !serverReachable; });
    }
  });
}
$("queue-drain-toggle").addEventListener("click", toggleWorkerDrain);
$("worker-drain-toggle").addEventListener("click", toggleWorkerDrain);
// A menu of secondary actions closes once one of them is chosen.
$("library-actions").addEventListener("click", event => { if (event.target?.closest?.("button")) $("library-actions").open = false; });
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
    : info.message || info.nodes?.[0]?.message || "Video worker offline or not ready. New jobs are blocked.";
  renderWorkerDetails($("worker-nodes"), info);
  renderWorkerDetails($("settings-worker"), info);
  renderWorkerDrain(info);
  // A normal scheduler is the default and stays in the dialog; only a doubt belongs in the header.
  const scheduler = schedulerSummary(info);
  $("activity-scheduler").textContent = scheduler;
  $("activity-scheduler").dataset.state = scheduler.startsWith("Scheduler · Normal") ? "ok" : scheduler.startsWith("Scheduler · Degraded") ? "degraded" : "unknown";
  if (serverReachable && info && !scheduler.startsWith("Scheduler · Normal")) $("worker-status").textContent += ` · ${scheduler.replace("Scheduler · ", "Scheduler ")}`;
  const paused = serverReachable ? drainSummary(info?.drain) : null;
  if (paused) $("worker-status").textContent += ` · ${paused.label}`;
  $("worker-status").title = $("worker-status").textContent;
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
  // A browser that arrived through Access has no local session to sign out of.
  state.accessSession = auth.session === "cloudflare_access";
  state.localSession = state.authMode === "token" && !state.accessSession;
  $("logout").hidden = !state.localSession;
  $("local-session").hidden = !state.localSession;
  const info = await api("bootstrap");
  state.info = info;
  $("login").hidden = true;
  $("workspace").hidden = false;
  $("worker-status").hidden = false;
  void refreshWorkers();
  // The picker in the goal form names the saved languages.
  void loadKeepLanguages();
  $("logout").hidden = !state.localSession;
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
  // Profiles fill a menu and the Profiles tab; the first screen is the queue or a
  // folder and does not wait for them. They load beside it, and only a link
  // straight into the Profiles tab waits.
  const route = navigationRoute(typeof window !== "undefined" ? window.location.search : "");
  const recipesLoaded = info.tools.includes("recipe_list") ? loadRecipes(true) : null;
  if (recipesLoaded && route.tab === "recipes") await recipesLoaded;
  else recipesLoaded?.catch((e) => {
    if (serverReachable && e.message !== "sign in to Navigatorr") notify(e.message);
  });
  try {
    await applyNavigation(route);
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

// What a conversion batch has done with a file, or with the files in a folder,
// title or season: one small mark after the name. The queued and working marks
// are drawn in CSS; failed is the quietest, since the file itself is fine and
// only its conversion did not finish.
const fileMarkInfo = {
  attention: {glyph: "!", label: "Needs your decision", many: n => `${n} ${n === 1 ? "needs" : "need"} a decision`},
  working: {label: "Converting now", many: n => `${n} converting`},
  queued: {label: "Added · waiting for its turn", many: n => `${n} queued`},
  converted: {glyph: "✓", label: "Converted · the original is untouched", many: n => `${n} converted`},
  replaced: {glyph: "✓", label: "Replaced with the converted file", many: n => `${n} replaced`},
  kept: {glyph: "–", label: "Checked · the original stays as it is", many: n => `${n} kept as they are`},
  failed: {glyph: "↺", label: "Last conversion did not finish · the file is untouched", many: n => `${n} not finished`},
};
function fileMark(status) {
  const info = fileMarkInfo[status?.state];
  if (!info) return null;
  const counts = status.counts && typeof status.counts === "object" ? status.counts : null;
  const note = counts ? Object.keys(fileMarkInfo).filter(key => counts[key] > 0).map(key => fileMarkInfo[key].many(counts[key])).join(" · ") : info.label;
  const mark = node("span", "", `file-mark ${status.state}`);
  mark.setAttribute("role", "img");
  mark.setAttribute("aria-label", note);
  mark.title = note;
  if (info.glyph) mark.append(node("span", info.glyph, "file-mark-glyph"));
  if (status.count > 1) mark.append(node("span", String(status.count), "file-mark-count"));
  return mark;
}
// Marks a row's name. Each mark is remembered by what it describes, so a later
// poll can change it in place instead of redrawing the list.
function markFile(key, host, status) {
  const entry = {host, mark: null, signature: ""};
  (state.fileMarks ||= new Map()).set(key, entry);
  setFileMark(entry, status);
}
function setFileMark(entry, status) {
  const signature = status ? JSON.stringify([status.state, status.count || 0, status.counts || null]) : "";
  if (signature === entry.signature) return;
  entry.signature = signature;
  entry.mark?.remove();
  entry.mark = fileMark(status);
  if (entry.mark) entry.host.append(entry.mark);
}
// Re-reads the rows already on screen, so a file added from another page or
// finished in the background shows its new state without reopening the list.
async function refreshFileMarks() {
  const targets = state.fileMarks;
  if (state.tab !== "library" || !targets?.size || state.libraryLoading || state.fileMarksRefreshing || $("workspace").hidden) return;
  const revision = state.libraryRevision, auth = state.authRevision;
  const current = () => revision === state.libraryRevision && auth === state.authRevision && state.fileMarks === targets;
  state.fileMarksRefreshing = true;
  try {
    const loaded = Math.max(1, state.libraryLoaded || 0);
    const folder = $("service").value.startsWith("folder:");
    const sort = $("library-sort").value || "name";
    const apply = (key, status) => { const entry = targets.get(key); if (entry) setFileMark(entry, status); };
    for (let offset = 0; offset < (state.libraryShowsSeasons ? 1 : loaded); offset += 100) {
      const query = folder
        ? new URLSearchParams({path: state.folder, q: $("search").value, offset, sort, listing: state.folderListing || ""})
        : new URLSearchParams({service: $("service").value, q: $("search").value, offset, limit: 100, sort});
      if (!folder && state.media) query.set("id", state.media.id);
      if (!folder && seriesSeasonSelected()) query.set("season", state.librarySeason);
      const page = await api(`${folder ? "folder" : "library"}?${query}`);
      if (!current()) return;
      if (state.libraryShowsSeasons) for (const group of page.season_groups || []) apply(`season:${group.seasonNumber ?? "unknown"}`, group.nav_status);
      else for (const item of page.items || []) apply(folder ? `folder:${item.path}` : `${state.media ? "file" : "title"}:${item.id}`, item.nav_status);
    }
    state.fileMarksCheckedAt = Date.now();
  } catch {
    // The marks are advisory; the next poll tries again.
  } finally {
    state.fileMarksRefreshing = false;
  }
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
    if (seriesSeasonSelected()) query.set("season", state.librarySeason);
    const page = await api(`library?${query}`);
    if (revision !== state.libraryRevision) return;
    const grouped = $("service").value === "sonarr" && state.media && !seriesSeasonSelected() && Array.isArray(page.season_groups);
    state.libraryShowsSeasons = Boolean(grouped);
    $("library-total").textContent = grouped ? `${page.season_groups.length} seasons · ${page.total} files` : `${page.total} ${state.media ? "files" : "titles"}`;
    if (state.media && $("service").value === "sonarr") {
      $("selection-title").textContent = (state.media.title || "Series") + (seriesSeasonSelected() ? ` · ${seriesSeasonLabel(state.librarySeason)}` : "");
      $("back").setAttribute("aria-label",seriesSeasonSelected() ? "Back to seasons" : "Back to series");
    }
    if (!more) {
      $("library-items").replaceChildren();
      state.libraryFileKeys = new Set();
      state.allSelectionKeys = null;
      state.fileMarks = new Map();
      state.fileMarksCheckedAt = Date.now();
    }
    if (grouped) renderSeriesSeasons(page.season_groups);
    for (const item of grouped ? [] : page.items) {
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
        state.libraryFileKeys.add(item.id);
      } else
        row.append(
          node(
            "span",
            $("service").value === "radarr" ? "▰" : "▤",
            "media-icon",
          ),
        );
      const title = item.title || (seriesSeasonSelected() ? (item.relativePath || item.path || "").split(/[\\/]/).pop() : item.relativePath || item.path) || `File ${item.id}`;
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
          state.librarySeason = null;
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
      const name = node("span", title, "media-title");
      markFile(`${state.media ? "file" : "title"}:${item.id}`, name, item.nav_status);
      b.append(name);
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
    if (!(grouped ? page.season_groups : page.items).length)
      $("library-items").append(
        node("p", "No matching files or titles.", "empty"),
      );
    state.libraryLoaded = grouped ? page.season_groups.length : state.libraryOffset + page.items.length;
    state.libraryHasMore = !grouped && page.has_more;
    state.libraryError = false;
    state.libraryNeedsLoad = false;
    if (!more) writeNavigation();
    $("library-more").hidden = !state.libraryHasMore;
    if (state.media && $("service").value === "sonarr") {
      const selectedSeason = state.draftSeason ?? $("season").value;
      const seasons = page.seasons || [];
      $("season").replaceChildren();
      option($("season"), "", "All seasons");
      seasons.forEach(({seasonNumber:n,fileCount:count}) => option($("season"), n, `${n === 0 ? "Specials" : `Season ${n}`} · ${count} file${count === 1 ? "" : "s"}`));
      $("season").value = seasons.some(s => String(s.seasonNumber) === selectedSeason) ? selectedSeason : "";
      if (seriesSeasonSelected() && state.librarySeason !== "unknown") $("season").value = String(state.librarySeason);
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
function seriesSeasonSelected() {
  return $("service").value === "sonarr" && state.media && state.librarySeason != null;
}
function seriesSeasonLabel(season) {
  return season == null || season === "unknown" ? "Other files" : season === 0 ? "Specials" : `Season ${season}`;
}
function fileInCurrentSeason(file) {
  if (!file) return false;
  const season = Number.isInteger(file.seasonNumber) && file.seasonNumber >= 0 ? file.seasonNumber : "unknown";
  const search = $("search").value.toLowerCase();
  return season === state.librarySeason && (!search || `${file.title || ""} ${file.path || ""} ${file.relativePath || ""}`.toLowerCase().includes(search));
}
function renderSeriesSeasons(seasons) {
  for (const season of seasons) {
    const row = node("div", "", "media-row"), icon = node("span", "", "media-icon");
    const folder = actionIcon("Folder");
    if (folder) icon.append(folder); else icon.textContent = "▤";
    const open = button("", () => navigateSeriesSeason(season.seasonNumber ?? "unknown"), "open-media");
    const name = node("span",seriesSeasonLabel(season.seasonNumber),"media-title");
    markFile(`season:${season.seasonNumber ?? "unknown"}`, name, season.nav_status);
    open.append(name,node("span",`${season.fileCount} ${season.fileCount === 1 ? "file" : "files"}`,"metadata"));
    row.append(icon,open,node("span",bytes(season.size),"media-size"));
    $("library-items").append(row);
  }
}
async function navigateSeriesSeason(season) {
  state.librarySeason = season;
  state.allSelectionKeys = null;
  state.libraryOffset = 0;
  clearFileSelection();
  setFileStep("browse");
  await loadLibrary();
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
const draftFields = ["technical","scope","path","season","recursive","selected-only","promote-batch","profile","custom","encoder","quality","preset","tune","rate-mode","bitrate","max-bitrate","audio","max-items","preserve-depth","min-savings","max-growth","media-kind","allow-hardware","size-budget-kind","size-budget-percent","size-budget-gb","size-budget-multiplier"];
function libraryDraftKey() {
  const source = $("service").value;
  return source.startsWith("folder:") ? source : `${source}:${state.media?.id || "catalog"}`;
}
function saveLibraryDraft() {
  if (!state.draftKey || typeof sessionStorage === "undefined" || $("workspace").hidden) return;
  const fields = {};
  for (const id of draftFields) if ($(id)) fields[id] = {value:id === "season" ? state.draftSeason ?? $(id).value : $(id).value,checked:$(id).checked};
  const draft = {fields,mode:processingMode(),goalExplicit:Boolean(state.goalExplicit),folderSelected:[...(state.folderSelected || [])],selected:[...state.selected],files:[...state.files].filter(([id])=>state.selected.has(id))};
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
  setProcessingMode(draft.mode || "size");
  state.goalExplicit = Boolean(draft.goalExplicit);
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
  state.librarySeason = null;
  state.libraryShowsSeasons = false;
  state.libraryFileKeys = null;
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
  $("back").setAttribute("aria-label","Parent folder");
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
  if (seriesSeasonSelected()) { safe(() => navigateSeriesSeason(null)); return; }
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
// Search is a mode: the source toolbar is swapped for [input][search][close].
// Closing is one tap or Escape; it clears the filter and restores the browse view.
function setLibrarySearchMode(open, { focus = false } = {}) {
  $("library-search-label").hidden = !open;
  $("library-tools").hidden = open;
  const toggle = $("toggle-library-search");
  toggle.setAttribute("aria-expanded", String(open));
  toggle.setAttribute("aria-label", open ? "Close search" : "Show search");
  toggle.title = open ? "Close search" : "Search";
  toggle.classList?.toggle("search-open", open);
  if (!focus) return;
  if (open) $("search").focus();
  else toggle.focus?.();
}
function closeLibrarySearch() {
  setLibrarySearchMode(false, { focus: true });
  $("search").value = "";
  state.libraryOffset = 0;
  safe(loadLibrary);
}
$("toggle-library-search").addEventListener("click", () => {
  if ($("library-search-label").hidden) setLibrarySearchMode(true, { focus: true });
  else closeLibrarySearch();
});
$("close-library-search").addEventListener("click", closeLibrarySearch);
$("search").addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    e.preventDefault();
    closeLibrarySearch();
  } else if (e.key === "Enter") {
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
      state.fileMarks = new Map();
      state.fileMarksCheckedAt = Date.now();
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
        sizeLabel.dataset.page = String(state.libraryOffset);
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
      markFile(`folder:${file.path}`, title, file.nav_status);
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
    state.folderSizesAttempt = 0;
    state.folderSizesCheckedAt = Date.now();
    scheduleFolderSizes(revision, page.path, $("search").value, state.libraryLoaded);
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
  target.textContent = size?.status === "calculating" ? (size.queued ? "En cola…" : "Calculating…")
    : size?.bytes != null ? `${size.status === "partial" ? "≥ " : ""}${bytes(size.bytes)}`
    : "Unavailable";
  target.title = size?.note || (size?.status !== "calculating" && size?.measured_at
    ? `Total file bytes, excluding symbolic links. Measured ${new Date(size.measured_at).toLocaleString()}. Saved across restarts; changes checked in the background.${size.updating ? " Updating; last measured total remains visible." : ""}`
    : size?.queued ? "Waiting for its turn; the folders on screen are measured first."
    : "Measuring total file bytes in this folder.");
}
// Sizes are only asked for while one is still being measured, and only for the
// pages that hold it. Once every size is settled nothing is requested until the
// user comes back to the tab after a while.
const folderSizeDelays = [2000, 3000, 5000, 8000, 12000, 20000];
const folderSizeStaleMs = 5 * 60 * 1000;
function pendingSizePages() {
  const pages = new Set();
  for (const labels of state.folderSizeTargets?.values() || []) {
    for (const label of [].concat(labels)) if (label.dataset.pending === "1") pages.add(Number(label.dataset.page) || 0);
  }
  return [...pages].sort((a, b) => a - b);
}
function scheduleFolderSizes(revision, path, query, loaded) {
  clearTimeout(state.folderSizesTimer);
  state.folderSizesArgs = {revision, path, query, loaded};
  if (!pendingSizePages().length || document.hidden) return;
  const delay = folderSizeDelays[Math.min(state.folderSizesAttempt || 0, folderSizeDelays.length - 1)];
  state.folderSizesAttempt = (state.folderSizesAttempt || 0) + 1;
  state.folderSizesTimer = setTimeout(() => {
    void updateFolderSizes(revision, path, query, loaded);
  }, delay);
}
// Back in view: finish what is still measuring, or look again if the sizes
// have been on screen for a while.
function resumeFolderSizes() {
  const args = state.folderSizesArgs;
  if (!args || !state.folderSizeTargets?.size || state.tab !== "library" || document.hidden) return;
  clearTimeout(state.folderSizesTimer);
  const stale = Date.now() - (state.folderSizesCheckedAt || 0) > folderSizeStaleMs;
  if (!stale && !pendingSizePages().length) return;
  state.folderSizesAttempt = 0;
  void updateFolderSizes(args.revision, args.path, args.query, args.loaded, stale);
}
async function updateFolderSizes(revision, path, query, loaded, everything = false) {
  const auth = state.authRevision;
  const current = () => revision === state.libraryRevision && auth === state.authRevision &&
    state.folder === path && state.tab === "library" && !$("workspace").hidden;
  if (!current() || !state.folderSizeTargets?.size) return;
  const offsets = everything ? Array.from({length: Math.ceil(loaded / 100)}, (_, i) => i * 100) : pendingSizePages();
  try {
    for (const offset of offsets) {
      const page = await api(`folder?${new URLSearchParams({path, q:query, offset, sizes:"1", sort:$("library-sort").value || "name", listing:state.folderListing || ""})}`);
      if (!current()) return;
      for (const item of page.items || []) {
        const target = state.folderSizeTargets.get(item.path);
        if (!target) continue;
        for (const label of Array.isArray(target) ? target : [target]) showFolderSize(label, item.folder_size);
      }
    }
    state.folderSizesCheckedAt = Date.now();
    if (current()) scheduleFolderSizes(revision, path, query, loaded);
  } catch {
    if (current()) for (const targets of state.folderSizeTargets.values()) {
      for (const target of Array.isArray(targets) ? targets : [targets]) {
        if (target.textContent === "Calculating…" || target.textContent === "En cola…") showFolderSize(target, {note:"Could not retrieve folder size. Refresh to try again."});
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
// Ticking several of the rows on screen at once, four ways: Shift+click ticks
// everything between the last row ticked and this one; the arrow keys move
// between rows and Shift+arrow grows or shrinks a range from the row the move
// started on; dragging down (or up) the checkboxes ticks, or unticks, every row
// the pointer crosses, with a finger as well as a mouse; and "Select range"
// arms the next tap as a Shift+click for screens that have no Shift key.
function fileChecks() {
  return [...$("library-items").querySelectorAll('input[type="checkbox"]')].filter(input => input.dataset.selection !== undefined && input.dataset.folder === undefined);
}
function setFileChecked(input, checked) {
  const folder = $("service").value.startsWith("folder:");
  const key = folder ? input.dataset.selection : Number(input.dataset.selection);
  const chosen = folder ? (state.folderSelected ||= new Set()) : state.selected;
  if (checked) chosen.add(key); else chosen.delete(key);
  input.checked = checked;
}
// The rows from `from` to `to`, in the order they are shown.
function rowsBetween(checks, from, to) {
  const a = checks.findIndex(input => input.dataset.selection === from), b = checks.findIndex(input => input.dataset.selection === to);
  return a < 0 || b < 0 ? [] : checks.slice(Math.min(a, b), Math.max(a, b) + 1);
}
function rowClicked(input, shift, value = input.checked) {
  const checks = fileChecks();
  const range = shift && state.selectionAnchor ? rowsBetween(checks, state.selectionAnchor, input.dataset.selection) : [];
  if (range.length) {
    for (const row of range) setFileChecked(row, value);
    fileSelectionChanged();
  }
  if (range.length && typeof window !== "undefined") window.getSelection?.()?.removeAllRanges();
  state.selectionAnchor = input.dataset.selection;
  state.selectionRange = null;
}
function rowKeyed(input, event) {
  if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
  const all = [...$("library-items").querySelectorAll('input[type="checkbox"]')].filter(box => box.dataset.selection !== undefined);
  const next = all[all.indexOf(input) + (event.key === "ArrowDown" ? 1 : -1)];
  if (!next) return;
  event.preventDefault();
  next.focus();
  if (!event.shiftKey) { state.selectionRange = null; return; }
  if (input.dataset.folder !== undefined || next.dataset.folder !== undefined) return;
  const checks = fileChecks();
  // The range starts on the row the key was pressed on, and grows from there.
  if (!state.selectionRange) state.selectionRange = {anchor: input.dataset.selection, keys: new Set()};
  const range = rowsBetween(checks, state.selectionRange.anchor, next.dataset.selection);
  const keys = new Set(range.map(row => row.dataset.selection));
  for (const row of checks) if (state.selectionRange.keys.has(row.dataset.selection) && !keys.has(row.dataset.selection)) setFileChecked(row, false);
  for (const row of range) setFileChecked(row, true);
  state.selectionRange.keys = keys;
  state.selectionAnchor = state.selectionRange.anchor;
  fileSelectionChanged();
}
// "Select range" needs a ticked row on screen to start from.
function rangeAnchorReady() {
  const anchor = state.selectionAnchor;
  return anchor !== undefined && anchor !== null && fileChecks().some(input => input.dataset.selection === anchor && input.checked);
}
// Dragging over the checkboxes. The row the pointer went down on sets what the
// drag does (tick it if it was empty, untick it if it was ticked); every file
// row between there and the pointer follows, and a row the pointer leaves goes
// back to how it was. Nothing happens until the pointer reaches another row, so
// a plain tap or click is untouched.
function fileRowAt(checks, y) {
  return checks.find(input => {
    const box = (input.closest?.(".media-row") || input).getBoundingClientRect?.();
    return box && y >= box.top && y < box.bottom;
  });
}
function dragSelectStart(event) {
  if (event.isPrimary === false || (event.pointerType === "mouse" && event.button !== 0)) return;
  const input = event.target?.closest?.(".media-check")?.querySelector?.('input[type="checkbox"]') || event.target;
  if (input?.tagName !== "INPUT" || input.type !== "checkbox" || input.dataset.selection === undefined || input.dataset.folder !== undefined || input.disabled) return;
  state.drag = {id: event.pointerId, start: input.dataset.selection, value: !input.checked, before: new Map(), last: input.dataset.selection, moved: false};
}
function dragSelectMove(event) {
  const drag = state.drag;
  if (!drag || (drag.id !== undefined && event.pointerId !== undefined && event.pointerId !== drag.id)) return;
  const checks = fileChecks();
  const over = fileRowAt(checks, event.clientY);
  if (!over || over.dataset.selection === drag.last || (!drag.moved && over.dataset.selection === drag.start)) return;
  if (!drag.moved) {
    drag.moved = true;
    // The mouse can leave the list while it is held down; the finger already holds on to its row.
    try { $("library-items").setPointerCapture?.(event.pointerId); } catch {}
  }
  event.preventDefault?.();
  drag.last = over.dataset.selection;
  const range = rowsBetween(checks, drag.start, drag.last);
  const keys = new Set(range.map(row => row.dataset.selection));
  for (const row of checks) if (drag.before.has(row.dataset.selection) && !keys.has(row.dataset.selection)) {
    setFileChecked(row, drag.before.get(row.dataset.selection));
    drag.before.delete(row.dataset.selection);
  }
  for (const row of range) {
    if (!drag.before.has(row.dataset.selection)) drag.before.set(row.dataset.selection, row.checked);
    setFileChecked(row, drag.value);
  }
  if (typeof window !== "undefined") window.getSelection?.()?.removeAllRanges();
  fileSelectionChanged();
}
function dragSelectEnd(event) {
  const drag = state.drag;
  if (!drag || (drag.id !== undefined && event.pointerId !== undefined && event.pointerId !== drag.id)) return;
  state.drag = null;
  if (!drag.moved) return;
  if (event.type === "pointercancel") {
    // The browser took the gesture over (a scroll, say): leave the rows as they were.
    for (const row of fileChecks()) if (drag.before.has(row.dataset.selection)) setFileChecked(row, drag.before.get(row.dataset.selection));
    fileSelectionChanged();
    return;
  }
  // The click that follows the release must not flip the last row back.
  state.dragEndedAt = Date.now();
  state.selectionAnchor = drag.last;
  state.selectionRange = null;
  state.rangeArmed = false;
  controls();
}
$("library-items").addEventListener("pointerdown", dragSelectStart);
$("library-items").addEventListener("pointermove", dragSelectMove);
$("library-items").addEventListener("pointerup", dragSelectEnd);
$("library-items").addEventListener("pointercancel", dragSelectEnd);
$("select-range").addEventListener("click", () => {
  state.rangeArmed = !state.rangeArmed && rangeAnchorReady();
  controls();
});
$("library-items").addEventListener("click", event => {
  const input = event.target;
  if (state.dragEndedAt && Date.now() - state.dragEndedAt < 500) {
    state.dragEndedAt = 0;
    event.preventDefault?.();
    return;
  }
  if (input?.tagName === "INPUT" && input.type === "checkbox" && input.dataset.selection !== undefined && input.dataset.folder === undefined) {
    const armed = Boolean(state.rangeArmed);
    state.rangeArmed = false;
    // An armed tap always ticks the range, even when it lands on a ticked row.
    rowClicked(input, event.shiftKey || armed, armed ? true : input.checked);
    if (armed) fileSelectionChanged();
  }
});
$("library-items").addEventListener("keydown", event => {
  const input = event.target;
  if (input?.tagName === "INPUT" && input.type === "checkbox" && input.dataset.selection !== undefined) rowKeyed(input, event);
});
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
      const query = new URLSearchParams({service:$("service").value,id:state.media.id,q:$("search").value,all:"1"});
      if (seriesSeasonSelected()) query.set("season",state.librarySeason);
      const page = await api(`library?${query}`);
      files = page.items;
      keys = files.map(file => file.id);
    }
    if (revision !== state.libraryRevision || auth !== state.authRevision) return;
    if (!keys.length) throw new Error("No videos found to select.");
    state.allSelectionKeys=new Set(keys);
    if (folder) state.folderSelected = new Set(keys);
    else {
      files.forEach(file => state.files.set(file.id,file));
      state.selected = new Set(seriesSeasonSelected() ? [...state.selected,...keys] : keys);
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
  else {
    if (seriesSeasonSelected()) {
      for (const [id,file] of state.files) {
        if (fileInCurrentSeason(file)) state.selected.delete(id);
      }
    } else { state.folderSelected?.clear(); state.selected.clear(); }
    $("library-items").querySelectorAll('input[type="checkbox"]').forEach(input=>input.checked=false);
    fileSelectionChanged();
  }
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
    state.goalExplicit = false;
    clearGoalCarry();
    resetKeepTracks();
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
$("back-to-files").addEventListener("click", () => { clearGoalCarry(); setFileStep("browse"); writeNavigation(); });
$("use-container").addEventListener("click", () => safe(async () => {
  // Freeze discovery into the same explicit selection used by checkboxes.
  await selectAllFiles();
  configureSelection();
}));
function processingMode() {
  return document.querySelector?.('input[name="processing-mode"]:checked')?.value || "size";
}
function setProcessingMode(mode) {
  document.querySelectorAll('input[name="processing-mode"]').forEach(input => { input.checked = input.value === mode; });
}
// Only a click counts as the user's choice; the form never overrides it.
document.querySelectorAll('input[name="processing-mode"]').forEach(input => input.addEventListener("change", () => { state.goalExplicit = true; controls(); }));

// The goals are not peers: a goal can skip a file outright (HDR with Save
// space, 1080p SDR with Shrink). When step 2 opens, the server reads the first
// chosen file and says what each goal would do, using the job's own gates.
const goalSkipReasons = {
  hdr_requires_compact: "HDR source",
  compact_not_needed: "already SDR at 1080p or smaller",
  already_target_codec: "already HEVC",
  unsupported_source: "unsupported video format",
  hfr_quality_unavailable: "high frame rate",
  compact_sdr_requires_bt709: "wide-gamut SDR",
  compact_requires_square_pixels: "non-square pixels",
  hdr_single_video_stream_required: "more than one video stream",
  dolby_vision_profile_5_no_compatible_base_layer: "Dolby Vision profile 5",
};
function goalSkipText(code) {
  return goalSkipReasons[code] || "not supported for this video";
}
function goalProbeTarget() {
  if ($("scope").value !== "batch") {
    const path = $("path").value.trim();
    return path ? {path, batch: false} : null;
  }
  if ($("service").value.startsWith("folder:")) {
    const first = [...(state.folderSelected || [])][0];
    return first ? {path: first, batch: state.folderSelected.size > 1} : null;
  }
  const file = state.files.get([...state.selected][0]);
  return file?.path ? {path: file.path, batch: state.selected.size > 1} : null;
}
function refreshGoalProfile() {
  const target = goalProbeTarget(), current = state.goalProfile;
  const key = target?.path || "";
  if (target && current?.key === key) { current.batch = target.batch; return; }
  if (!target && !current) return;
  // A new file, or none, makes every older response stale, and cancelling it
  // frees the server's read slot for the file now chosen.
  state.goalProfileSeq = (state.goalProfileSeq || 0) + 1;
  state.goalProfileAbort?.abort();
  state.goalProfileAbort = null;
  if (!target) {
    state.goalProfile = null;
    if (!state.applyingNavigation) state.goalExplicit = false;
    return;
  }
  // Moving from one file to another is a new visit. In a batch the key is just
  // the first selected file, so unticking it keeps the goal the user chose.
  if (current?.key && current.key !== key && !current.batch && !target.batch) state.goalExplicit = false;
  state.goalProfile = {key, status: "loading", batch: target.batch};
  clearTimeout(state.goalProfileTimer);
  state.goalProfileTimer = setTimeout(() => { if (state.goalProfile?.key === key) loadGoalProfile(key); }, 150);
}
// Reading a file takes one of the server's two source slots. Both can still be
// held by files just left, so a busy answer is asked once more.
async function readSource(url, signal, current) {
  try {
    return await api(url, undefined, {signal});
  } catch (err) {
    if (err?.status !== 429 || !current()) throw err;
    await new Promise(resolve => setTimeout(resolve, 1500));
    if (!current()) throw err;
    return api(url, undefined, {signal});
  }
}
async function loadGoalProfile(key) {
  const seq = state.goalProfileSeq, controller = new AbortController();
  state.goalProfileAbort = controller;
  const current = () => seq === state.goalProfileSeq && state.goalProfile?.key === key;
  let data = null;
  try {
    data = await readSource(`source-profile?${new URLSearchParams({path: key})}`, controller.signal, current);
  } catch {}
  if (!current()) return;
  state.goalProfileAbort = null;
  state.goalProfile = data?.profile && data.goals ? {...state.goalProfile, status: "ready", data} : {...state.goalProfile, status: "error"};
  // The file's tracks are read after its profile, never beside it.
  scheduleKeepTracks();
  const recommended = state.goalProfile.data?.recommended;
  // The recommendation fills in an unchosen goal; it never replaces a choice.
  if (recommended && goalNames[recommended] && !state.goalExplicit) setProcessingMode(recommended);
  controls();
}
function sourceProfileText(profile) {
  return [profile.resolution, profile.dynamic_range, profile.video_codec, profile.bit_depth ? `${profile.bit_depth}-bit` : "", profile.size_bytes ? bytes(profile.size_bytes) : ""].filter(Boolean).join(" · ");
}
function renderGoalProfile() {
  const p = state.goalProfile, ready = p?.status === "ready" ? p.data : null;
  const line = $("source-profile");
  line.hidden = !ready && p?.status !== "error";
  line.textContent = ready ? `${sourceProfileText(ready.profile)}${p.batch ? " · based on the first file" : ""}` : p?.status === "error" ? "Could not read this file" : "";
  for (const mode of Object.keys(goalNames)) {
    const note = $(`goal-note-${mode}`), verdict = ready?.goals?.[mode];
    const text = ready?.recommended === mode ? "Recommended for this file" : verdict?.status === "skip" ? `Skips ${p.batch ? "the first file" : "this file"}: ${goalSkipText(verdict.reason_code)}` : "";
    note.hidden = !text;
    note.textContent = text;
    note.dataset.kind = ready?.recommended === mode ? "recommended" : "skip";
  }
}
// More goals opens for a selected or recommended goal inside it. A goal
// picked only by the form's own default stays hidden while the file is read.
function syncMoreGoals() {
  const p = state.goalProfile;
  if ((!p || p.status === "loading") && !state.goalExplicit) return;
  const wanted = [processingMode(), p?.status === "ready" ? p.data.recommended : ""];
  if (wanted.some(mode => moreGoals.includes(mode))) $("more-goals").open = true;
}
// What the server says the chosen goal will do to the first chosen file.
function goalSkipWarning() {
  const p = state.goalProfile, mode = processingMode();
  const verdict = p?.status === "ready" ? p.data.goals?.[mode] : null;
  if (verdict?.status !== "skip") return "";
  return `${goalNames[mode]} will skip ${p.batch ? "the first file" : "this file"}: ${goalSkipText(verdict.reason_code)}. The original is kept and nothing is converted.${p.batch ? " The other files are checked one by one." : ""}`;
}
// Languages to keep. The Save space and Shrink goals keep the audio and
// subtitle tracks in these languages (plus the original-language audio and any
// untagged track, which the server always keeps). A language is its ISO 639-2
// terminology code, the spelling the server stores and compares by; names,
// two-letter codes and the bibliographic spellings are accepted when typing.
const languageTable = [
  // code, name, ISO 639-1, bibliographic spelling
  ["ara", "Arabic", "ar"], ["bul", "Bulgarian", "bg"], ["cat", "Catalan", "ca"], ["ces", "Czech", "cs", "cze"],
  ["cym", "Welsh", "cy", "wel"], ["dan", "Danish", "da"], ["deu", "German", "de", "ger"], ["ell", "Greek", "el", "gre"],
  ["eng", "English", "en"], ["est", "Estonian", "et"], ["eus", "Basque", "eu", "baq"], ["fas", "Persian", "fa", "per"],
  ["fin", "Finnish", "fi"], ["fra", "French", "fr", "fre"], ["gle", "Irish", "ga"], ["glg", "Galician", "gl"],
  ["heb", "Hebrew", "he"], ["hin", "Hindi", "hi"], ["hrv", "Croatian", "hr"], ["hun", "Hungarian", "hu"],
  ["hye", "Armenian", "hy", "arm"], ["ind", "Indonesian", "id"], ["isl", "Icelandic", "is", "ice"], ["ita", "Italian", "it"],
  ["jpn", "Japanese", "ja"], ["kat", "Georgian", "ka", "geo"], ["kor", "Korean", "ko"], ["lat", "Latin", "la"],
  ["lav", "Latvian", "lv"], ["lit", "Lithuanian", "lt"], ["mkd", "Macedonian", "mk", "mac"], ["msa", "Malay", "ms", "may"],
  ["nld", "Dutch", "nl", "dut"], ["nno", "Norwegian Nynorsk", "nn"], ["nob", "Norwegian Bokmål", "nb"], ["nor", "Norwegian", "no"],
  ["pol", "Polish", "pl"], ["por", "Portuguese", "pt"], ["ron", "Romanian", "ro", "rum"], ["rus", "Russian", "ru"],
  ["slk", "Slovak", "sk", "slo"], ["slv", "Slovenian", "sl"], ["spa", "Spanish", "es"], ["sqi", "Albanian", "sq", "alb"],
  ["srp", "Serbian", "sr"], ["swe", "Swedish", "sv"], ["tam", "Tamil", "ta"], ["tel", "Telugu", "te"],
  ["tgl", "Tagalog", "tl"], ["tha", "Thai", "th"], ["tur", "Turkish", "tr"], ["ukr", "Ukrainian", "uk"],
  ["vie", "Vietnamese", "vi"], ["zho", "Chinese", "zh", "chi"],
];
const languageNameOf = new Map([...languageTable.map(([code, name]) => [code, name]), ["und", "Untagged"]]);
const languageAliases = new Map([["untagged", "und"], ["unknown", "und"]]);
for (const [code, name, twoLetter, bibliographic] of languageTable)
  for (const alias of [code, name.toLowerCase(), twoLetter, bibliographic]) if (alias) languageAliases.set(alias, code);
languageAliases.set("und", "und");
// The one spelling two tags of a language share (es-419, spa and es are "spa").
function canonicalLanguage(tag) {
  const text = String(tag ?? "").trim().toLowerCase();
  if (!text) return "und";
  const base = text.split(/[-_]/)[0];
  return languageAliases.get(base) || base;
}
function languageName(tag) {
  const code = canonicalLanguage(tag);
  return languageNameOf.get(code) || code;
}
// What was typed, as a language code: "", or a three-letter code the server
// will check against ISO 639-2.
function parseLanguage(text) {
  const raw = String(text ?? "").trim().toLowerCase();
  const known = languageAliases.get(raw) || languageAliases.get(raw.replace(/\s*\([a-z]{3}\)$/, ""));
  return known || (/^[a-z]{3}$/.test(raw) ? raw : "");
}
function addLanguageTo(languages, text) {
  const code = parseLanguage(text);
  if (!code) return {message: "Enter a language such as English or eng."};
  if (code === "und") return {message: "Untagged tracks are always kept."};
  if (languages.includes(code)) return {message: `${languageName(code)} is already in the list.`};
  return {languages: [...languages, code]};
}
const sameLanguages = (a, b) => a.length === b.length && [...a].sort().every((code, i) => code === [...b].sort()[i]);
const languagesByName = languages => [...languages].sort((a, b) => languageName(a).localeCompare(languageName(b)));
const languageNames = languages => languagesByName(languages).map(languageName).join(", ");
// The suggestions offered while typing; choosing one types the name.
$("language-options").append(...languageTable.map(([code, name]) => Object.assign(document.createElement("option"), {value: name, label: code})));
function renderLanguageChips(list, languages, remove) {
  list.hidden = !languages.length;
  list.replaceChildren(...languagesByName(languages).map(code => {
    const item = node("li", "", "language-chip");
    const drop = button("×", () => remove(code), "chip-remove");
    drop.setAttribute("aria-label", `Remove ${languageName(code)}`);
    item.append(node("span", languageName(code)), node("span", code, "metadata"), drop);
    return item;
  }));
}
// Both language editors add the same way: say why an entry was refused, or take
// it. The goal form's one sits inside the job form, so Enter adds the language
// instead of submitting the job.
function wireLanguageInput({input, add, note}, current, set) {
  const submit = () => {
    const result = addLanguageTo(current(), $(input).value);
    $(note).textContent = result.message || "";
    if (!result.languages) return;
    $(input).value = "";
    set(result.languages);
  };
  $(add).addEventListener("click", submit);
  $(input).addEventListener("keydown", event => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    submit();
  });
}

// The setting, in Settings: edited as a draft and saved with Apply, like the
// node's parallel jobs. A load never replaces a draft that has been edited.
const keepLanguages = {status: "idle", saved: [], draft: [], defaults: [], saving: false, request: 0};
function applyKeepLanguages(data, adopt) {
  const k = keepLanguages, clean = adopt || sameLanguages(k.draft, k.saved);
  const changed = k.status === "ready" && !sameLanguages(k.saved, data.languages);
  k.saved = [...data.languages];
  k.defaults = [...(data.default || [])];
  k.status = "ready";
  if (clean) k.draft = [...k.saved];
  // The pre-checked tracks follow the saved list only while the form uses it;
  // a job's own languages, or every track, are not affected by it.
  if (changed && keepTracks.scope === "saved") { keepTracks.key = ""; refreshKeepTracks(); }
}
async function loadKeepLanguages() {
  const k = keepLanguages, auth = state.authRevision, request = ++k.request;
  if (k.status === "idle") { k.status = "loading"; renderKeepLanguages(); }
  try {
    const data = await api("keep-languages");
    if (auth !== state.authRevision || request !== k.request) return;
    applyKeepLanguages(data, false);
  } catch {
    if (auth !== state.authRevision || request !== k.request) return;
    if (k.status !== "ready") k.status = "error";
  }
  renderKeepLanguages();
  renderKeepTracks();
}
function keepLanguagesState(k) {
  if (k.status === "error") return "Could not load the list";
  if (k.status !== "ready") return "Loading…";
  if (!sameLanguages(k.draft, k.saved)) return "Not saved yet";
  if (!k.saved.length) return "Off: every track is kept";
  return sameLanguages(k.saved, k.defaults) ? "Default" : "Custom";
}
function stageKeepLanguages(languages) {
  keepLanguages.draft = languages;
  $("keep-languages-note").textContent = "";
  renderKeepLanguages();
}
function renderKeepLanguages() {
  const k = keepLanguages, ready = k.status === "ready";
  $("keep-languages-state").textContent = keepLanguagesState(k);
  renderLanguageChips($("keep-languages-list"), k.draft, code => {
    stageKeepLanguages(k.draft.filter(language => language !== code));
    $("keep-language-input").focus();
  });
  $("keep-language-input").disabled = !ready;
  $("keep-language-add").disabled = !ready;
  $("keep-languages-apply").disabled = !ready || k.saving || sameLanguages(k.draft, k.saved);
  $("keep-languages-reset").disabled = !ready || sameLanguages(k.draft, k.defaults);
  $("keep-languages-off").disabled = !ready || !k.draft.length;
}
async function saveKeepLanguages(languages) {
  const k = keepLanguages, note = $("keep-languages-note"), auth = state.authRevision;
  if (k.saving) return;
  k.saving = true;
  note.textContent = "Saving…";
  renderKeepLanguages();
  try {
    const data = await api("keep-languages", {languages}, {method: "PUT"});
    if (auth !== state.authRevision) return;
    k.request++;
    applyKeepLanguages(data, true);
    note.textContent = "Saved. It applies to jobs created from now on; jobs already queued keep the list they started with.";
  } catch (e) {
    note.textContent = e.message;
  } finally {
    k.saving = false;
    renderKeepLanguages();
    renderKeepTracks();
  }
}
wireLanguageInput({input: "keep-language-input", add: "keep-language-add", note: "keep-languages-note"}, () => keepLanguages.draft, stageKeepLanguages);
$("keep-languages-apply").addEventListener("click", () => void saveKeepLanguages(keepLanguages.draft));
$("keep-languages-reset").addEventListener("click", () => stageKeepLanguages([...keepLanguages.defaults]));
$("keep-languages-off").addEventListener("click", () => stageKeepLanguages([]));

// The goal form's choice for one job: the saved languages, this job's own list,
// or every track. For one file the file's tracks are listed too, checked the
// way the server's rules would keep them (the server answers, so the rules live
// in one place); changing a box sends explicit stream lists instead.
const keepGoals = ["size", "compact"];
const keepTracks = {scope: "saved", languages: [], picks: null, path: "", key: "", status: "idle", data: null, built: null, boxes: [], notice: "", error: "", seq: 0, abort: null, timer: 0};
// Which file or files the choice is for; null while the goal keeps no tracks.
function keepTrackTarget() {
  if ($("technical").checked || !keepGoals.includes(processingMode())) return null;
  const target = goalProbeTarget();
  return target ? {path: target.path, single: $("scope").value !== "batch"} : null;
}
function keepLanguageInputs() {
  const kt = keepTracks;
  return kt.scope === "saved" ? {} : {keep_languages: kt.scope === "all" ? [] : [...kt.languages]};
}
// The inputs the choice adds to a job. The saved list sends nothing; boxes that
// were changed send both lists, so what is frozen is what was on screen.
function keepTrackInputs(single, path) {
  const kt = keepTracks, inputs = keepLanguageInputs();
  if (!single || !kt.picks || kt.path !== path) return inputs;
  const order = set => [...set].sort((a, b) => a - b);
  // A file without audio has none to keep: the server takes the subtitles
  // alone then, and refuses an empty audio list.
  if (kt.data && !kt.data.audio.length) return {...inputs, keep_subtitle_streams: order(kt.picks.subtitles)};
  if (!kt.picks.audio.size) throw new Error("Keep at least one audio track.");
  return {...inputs, keep_audio_streams: order(kt.picks.audio), keep_subtitle_streams: order(kt.picks.subtitles)};
}
function resetKeepTracks() {
  Object.assign(keepTracks, {scope: "saved", languages: [], picks: null, notice: "", key: ""});
}
// Opening a file again to pick tracks starts from the languages it was made with.
function presetKeepLanguages(languages) {
  Object.assign(keepTracks, {scope: languages.length ? "job" : "all", languages: [...languages], picks: null, notice: "", key: ""});
}
function setKeepScope(scope) {
  const kt = keepTracks;
  if (scope === kt.scope) return;
  // Editing starts from the saved list rather than from nothing.
  if (scope === "job" && !kt.languages.length) kt.languages = [...keepLanguages.saved];
  kt.scope = scope;
  $("keep-job-note").textContent = "";
  controls();
}
document.querySelectorAll('input[name="keep-scope"]').forEach(radio => radio.addEventListener("change", () => { if (radio.checked) setKeepScope(radio.value); }));
wireLanguageInput({input: "keep-job-input", add: "keep-job-add", note: "keep-job-note"}, () => keepTracks.languages, languages => { keepTracks.languages = languages; controls(); });
function keepTracksQuery(path) {
  const kt = keepTracks, params = new URLSearchParams({path});
  if (kt.scope !== "saved") params.set("keep_languages", kt.scope === "all" ? "" : kt.languages.join(","));
  return params.toString();
}
// Called whenever the form changes. A new file or a new language list reads the
// file again; the same ones do nothing.
function refreshKeepTracks() {
  const kt = keepTracks, target = keepTrackTarget(), file = target?.single ? target.path : "";
  const key = file ? keepTracksQuery(file) : "";
  if (key === kt.key) return;
  kt.seq++;
  kt.abort?.abort();
  kt.abort = null;
  clearTimeout(kt.timer);
  kt.key = key;
  if (!file) {
    Object.assign(kt, {status: "idle", data: null, picks: null, path: "", notice: "", error: ""});
    return;
  }
  // A new file starts from the automatic choice. The same file with other
  // languages does too: the boxes follow the languages again.
  if (kt.path !== file) Object.assign(kt, {data: null, picks: null, path: file, notice: ""});
  else if (kt.picks) Object.assign(kt, {picks: null, notice: "The boxes follow the languages again."});
  kt.status = "loading";
  kt.error = "";
  scheduleKeepTracks();
}
// One file selection takes one of the server's two source slots at a time: the
// goal's profile read goes first, and the tracks are read once it is done (or
// at once when only the languages changed). A second tab keeps the other slot.
function scheduleKeepTracks() {
  const kt = keepTracks;
  clearTimeout(kt.timer);
  if (!kt.key || kt.status !== "loading" || kt.abort) return;
  const profile = state.goalProfile;
  if (profile?.status === "loading" && profile.key === kt.path) return;
  const key = kt.key, seq = kt.seq;
  kt.timer = setTimeout(() => loadKeepTracks(key, seq), 150);
}
async function loadKeepTracks(key, seq) {
  const kt = keepTracks, controller = new AbortController();
  kt.abort = controller;
  const current = () => seq === kt.seq;
  let data = null, error = "";
  try {
    data = await readSource(`source-tracks?${key}`, controller.signal, current);
  } catch (err) {
    error = err?.status === 400 ? err.message : "Could not read this file's tracks. The job still follows the languages.";
  }
  if (!current()) return;
  kt.abort = null;
  const ready = Boolean(data?.audio || data?.subtitles);
  Object.assign(kt, {status: ready ? "ready" : "error", data: ready ? data : null, error: ready ? "" : error || "Could not read this file's tracks. The job still follows the languages."});
  renderKeepTracks();
}
const trackCodecs = {eac3: "E-AC-3", ac3: "AC-3", truehd: "TrueHD", subrip: "SRT", ass: "ASS", ssa: "SSA", hdmv_pgs_subtitle: "PGS", dvd_subtitle: "VobSub", webvtt: "WebVTT", mov_text: "Text"};
const trackCodec = codec => trackCodecs[codec] || codecLabel(codec);
function trackChannels(count) {
  return {1: "Mono", 2: "Stereo", 6: "5.1", 8: "7.1"}[count] || (count > 0 ? `${count} channels` : "");
}
const trackKept = (kind, track) => keepTracks.picks ? keepTracks.picks[kind].has(track.index) : track.keep;
function keepTracksCount(data) {
  const kept = kind => data[kind].filter(track => trackKept(kind, track)).length;
  const part = (kind, one, many) => data[kind].length ? `${kept(kind)} of ${data[kind].length} ${data[kind].length === 1 ? one : many}` : "";
  const parts = [part("audio", "audio track", "audio tracks"), part("subtitles", "subtitle", "subtitles")].filter(Boolean);
  return parts.length ? `Keeping ${parts.join(" and ")}.` : "";
}
function toggleTrack(kind, index, on) {
  const kt = keepTracks, data = kt.data;
  if (!data) return;
  const automatic = name => new Set(data[name].filter(track => track.keep).map(track => track.index));
  const picks = kt.picks ||= {audio: automatic("audio"), subtitles: automatic("subtitles")};
  if (on) picks[kind].add(index); else picks[kind].delete(index);
  // A file keeps its audio, and boxes that match the automatic choice again are
  // that choice, so nothing explicit is sent.
  if (kind === "audio" && !picks.audio.size) picks.audio.add(index);
  const same = name => picks[name].size === automatic(name).size && [...picks[name]].every(i => automatic(name).has(i));
  if (same("audio") && same("subtitles")) kt.picks = null;
  kt.notice = "";
  renderKeepTracks();
}
function trackRow(kind, track) {
  const row = node("li", "", "track-row"), label = node("label", "", "track-label");
  const box = document.createElement("input");
  box.type = "checkbox";
  box.addEventListener("change", () => toggleTrack(kind, track.index, box.checked));
  const name = node("span", "", "track-text");
  name.append(node("strong", languageName(track.language), "track-name"));
  const facts = [trackCodec(track.codec), kind === "audio" ? trackChannels(track.channels) : "", track.title].filter(Boolean);
  if (facts.length) name.append(node("span", facts.join(" · "), "metadata track-facts"));
  label.append(box, name);
  // The badges stay on the track's own row, stacked when the row is narrow.
  const badges = node("span", "", "track-badges");
  for (const [flag, text] of [["original", "Original"], ["default", "Default"], ["forced", "Forced"]])
    if (track[flag]) badges.append(node("span", text, "badge track-badge"));
  if (badges.children.length) label.append(badges);
  row.append(label);
  return {row, box, kind, track};
}
// The boxes are built when a file is read and updated in place afterwards, so a
// keyboard user toggling one never loses their place.
function syncTrackBoxes() {
  const loading = keepTracks.status === "loading";
  const audioKept = keepTracks.boxes.filter(entry => entry.kind === "audio" && trackKept(entry.kind, entry.track)).length;
  for (const entry of keepTracks.boxes) {
    entry.box.checked = trackKept(entry.kind, entry.track);
    // Boxes wait for the file to be read again; the last audio track cannot be unticked.
    entry.box.disabled = loading || (entry.kind === "audio" && entry.box.checked && audioKept <= 1);
  }
}
function renderKeepTracks() {
  const kt = keepTracks, target = keepTrackTarget();
  $("keep-tracks").hidden = !target;
  if (!target) return;
  document.querySelectorAll('input[name="keep-scope"]').forEach(radio => { radio.checked = radio.value === kt.scope; });
  $("keep-job-languages").hidden = kt.scope !== "job";
  renderLanguageChips($("keep-job-list"), kt.languages, code => {
    keepTracks.languages = keepTracks.languages.filter(language => language !== code);
    controls();
    $("keep-job-input").focus();
  });
  const data = target.single ? kt.data : null;
  const saved = keepLanguages.status === "ready" ? keepLanguages.saved : null;
  $("keep-tracks-summary").textContent = kt.picks ? "Tracks chosen by hand" : kt.scope === "all" ? "Keep every track"
    : kt.scope === "job" ? (kt.languages.length ? `This job: ${languageNames(kt.languages)}` : "Keep every track")
    : saved ? (saved.length ? `Saved list: ${languageNames(saved)}` : "Keep every track") : "Saved list";
  $("keep-tracks-file").hidden = !target.single;
  if (!target.single) return;
  if (data && kt.built !== data) {
    const audio = data.audio.map(track => trackRow("audio", track)), subtitles = data.subtitles.map(track => trackRow("subtitles", track));
    kt.built = data;
    kt.boxes = [...audio, ...subtitles];
    $("keep-audio-list").replaceChildren(...audio.map(entry => entry.row));
    $("keep-subtitle-list").replaceChildren(...subtitles.map(entry => entry.row));
  }
  if (!data) {
    kt.built = null;
    kt.boxes = [];
    $("keep-audio-list").replaceChildren();
    $("keep-subtitle-list").replaceChildren();
  }
  $("keep-audio").hidden = !data?.audio.length;
  $("keep-subtitles").hidden = !data?.subtitles.length;
  syncTrackBoxes();
  $("keep-tracks-reset").hidden = !kt.picks;
  $("keep-tracks-status").textContent = kt.status === "error" ? kt.error : kt.status === "loading" ? "Reading the file's tracks…"
    : [data ? keepTracksCount(data) : "", kt.notice].filter(Boolean).join(" ");
}
$("keep-tracks-reset").addEventListener("click", () => {
  Object.assign(keepTracks, {picks: null, notice: ""});
  renderKeepTracks();
});

$("technical").addEventListener("change", controls);
$("allow-hardware").addEventListener("change", controls);
$("size-budget-kind").addEventListener("change", controls);

function renderProfileFacts(output, data, name, preserveDepth) {
  output.replaceChildren();
  const description = data.description || data.record?.description;
  if (description) output.append(node("p", description, "metadata"));
  const p = data.profile || {}, v = p.video || {};
  const facts = node("dl", "", "profile-facts");
  const add = (label, value) => facts.append(node("dt", label), node("dd", value));
  add("Encoder", profileEncoder(p));
  add("Quality", profileRate(p));
  add("Process", p.optimization?.enabled ? "Not available: it calibrates with samples, which was removed" : "Converts full video using this profile");
  add("Output depth", preserveDepth ? "Same as source · Overrides profile depth" : v.profile === "main10" || /10/.test(v.pixel_format || "") ? "10-bit" : v.pixel_format || v.profile ? "8-bit" : "Encoder default");
  if (v.spatial_aq != null) add("Image detail", v.spatial_aq ? "Adaptive allocation across image details" : "Standard allocation");
  add("Audio", p.audio?.mode === "copy" ? "Keep original audio tracks" : p.audio?.mode === "compact" ? "Compact · Convert lossless tracks to AAC, same layout; E-AC-3/AC-3/AAC copied" : p.audio?.mode === "compact_lossless" ? "Lossless to AAC · TrueHD, DTS-HD MA, PCM and FLAC become AAC, same layout; lossless Atmos/DTS:X lose object and height channels; lossy tracks (E-AC-3 Atmos included) copied" : p.audio?.mode === "he_aac" ? "HE-AAC · 64 kbps mono/stereo, 160 kbps surround (lossy)" : p.audio?.mode === "size_aac" ? "AAC-LC · lossless tracks, and lossy tracks at least 1.25× the target, become AAC-LC (stereo 192, 5.1 384 kbps), same layout; E-AC-3 Atmos copied" : p.audio?.mode === "size_he_aac" ? "HE-AAC · tracks at least 1.25× the target become HE-AAC (stereo 64, 5.1 160 kbps), other layouts AAC-LC; E-AC-3 Atmos copied" : p.audio?.mode === "size_aac_bed" ? "AAC-LC · lossless tracks, and lossy tracks at least 1.25× the target, become AAC-LC (stereo 192, 5.1 384 kbps), same layout; Atmos keeps its 5.1/7.1 bed, objects dropped" : p.audio?.mode === "size_he_aac_bed" ? "HE-AAC · tracks at least 1.25× the target become HE-AAC (stereo 64, 5.1 160 kbps), other layouts AAC-LC; Atmos keeps its channel bed, objects dropped" : p.audio?.mode || "Profile default");
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
  // The goal stays visible but inert: a technical profile replaces it on submit.
  if (technical && !state.technicalShown) $("advanced-profiles").open = true;
  state.technicalShown = technical;
  $("legacy-controls").hidden = !technical;
  $("processing-goal").disabled = technical;
  $("hardware-option").hidden = processingMode() !== "size";
  $("size-budget-option").hidden = processingMode() !== "size";
  updateSizeBudgetFields("size-budget");
  renderSegmented(["size-budget"]);
  refreshGoalProfile();
  renderGoalProfile();
  renderShrinkTitle();
  syncMoreGoals();
  refreshKeepTracks();
  renderKeepTracks();
  $("goal-overridden").hidden = !technical;
  $("advanced-state").textContent = technical ? "On · goal not used" : "Off";
  $("advanced-state").dataset.on = technical ? "true" : "false";
  $("processing-policy").hidden = technical;
  $("processing-policy").textContent = "Automatic encodes the whole file with fixed settings, then verifies the result. Originals stay in place; replacing them requires separate approval." + (!state.fileMedia && !state.media ? " Content type is unclassified; a conservative general-video policy applies." : "");
  $("enqueue").textContent = technical ? (batch ? "Create candidates" : "Create candidate") : "Process";
  $("source-summary").textContent = sourceSummary();
  $("batch-options").hidden = !batch;
  const folder = $("service").value.startsWith("folder:");
  const selectedCount = folder
    ? state.folderSelected?.size || 0
    : state.selected.size;
  const folderCount = folder ? new Set([...(state.folderSelected || [])].map(path=>path.slice(0,path.lastIndexOf("/")))).size : 0;
  if (!technical) {
    const files = batch ? ($("selected-only").checked ? selectedCount : 0) : $("path").value.trim() ? 1 : 0;
    $("enqueue").textContent = batch && !files ? "Process folder" : files > 1 ? `Process ${files} files` : "Process file";
  }
  $("selection-count").textContent = selectedCount ? `${selectedCount} selected${folder ? ` across ${folderCount} ${folderCount === 1 ? "folder" : "folders"}` : ""}` : "";
  $("configure-selection").hidden = !selectedCount && !$("path").value.trim();
  $("configure-selection").disabled = state.libraryLoading;
  $("use-container").hidden = folder
    ? !state.folder
    : $("service").value !== "sonarr" || !state.media;
  $("use-container").disabled = state.libraryLoading || state.selectionLoading;
  $("use-container").textContent = folder ? "Select folder" : seriesSeasonSelected() ? "Select season" : "Select series";
  $("select-all-files").hidden = !folder && (state.libraryShowsSeasons || !state.files?.size);
  $("selection-toolbar").hidden = true;
  $("use-container").title = folder ? "Choose this folder, including its subfolders" : seriesSeasonSelected() ? "Choose this season" : "Choose this series";
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
  const headerKeys = state.allSelectionKeys || (folder ? state.folderHeaderKeys : state.media && !state.libraryShowsSeasons && !state.libraryHasMore ? state.libraryFileKeys || new Set(state.files.keys()) : null);
  $("select-all-files").checked = headerKeys?.size>0 && [...headerKeys].every(key=>selectedKeys.has(key));
  const visibleSelected = seriesSeasonSelected() ? [...state.selected].some(id => fileInCurrentSeason(state.files.get(id))) : selectedCount > 0;
  $("select-all-files").indeterminate = !$("select-all-files").checked && (headerKeys ? [...headerKeys].some(key=>selectedKeys.has(key)) : visibleSelected);
  const anchored = rangeAnchorReady();
  if (!anchored) state.rangeArmed = false;
  const armed = Boolean(state.rangeArmed);
  $("range-bar").hidden = !folder && !state.media || fileChecks().length < 2;
  $("range-hint").hidden = anchored;
  $("select-range").hidden = !anchored;
  $("select-range").disabled = state.libraryLoading || state.selectionLoading;
  $("select-range").textContent = armed ? "Cancel range" : "Select range";
  $("select-range").setAttribute("aria-pressed", String(armed));
  $("range-status").textContent = armed ? "Choose the last file of the range." : anchored ? "Starts at the file you ticked last." : "";
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
  $("profile-mode-note").hidden = $("profile").value !== "auto" || $("custom").checked;
  $("profile-mode-note").textContent = "Automatic encodes the whole file with fixed settings for the objective and checks the result; originals are preserved until you approve.";
  $("custom-fields").hidden = !technical || !$("custom").checked;
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
  $("audio-note").hidden = !["compact", "he_aac"].includes($("audio").value);
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
  "profile",
  "audio",
  "rate-mode",
])
  $(id).addEventListener("change", controls);
$("encoder").addEventListener("change", () => {
  $("quality").value = $("encoder").value === "libx265" ? "23" : "65";
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
  return {
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
  };
}

async function submitJob(mode = "encode") {
  if (state.submitting || state.selectionLoading) return;
  state.submitting = true;
  controls();
  try {
    // The goal is still the user's call, but a skip the server already knows
    // about is said plainly before anything is queued.
    const warning = mode === "encode" && !$("technical").checked ? goalSkipWarning() : "";
    if (warning && !(await reviewAction({title: "This goal will skip the file", content: [node("p", warning)], confirmLabel: "Process anyway", cancelLabel: "Choose another goal"}))) return;
    notify("Submitting job…");
    return await buildAndSubmitJob(mode);
  } finally {
    state.submitting = false;
    controls();
  }
}
// Size budget (smaller files only, optional): a percent of the original, an
// absolute size in GB, or a budget matched to the sibling episodes. One at a
// time; the dropdown decides which field counts. prefix names the form's ids.
function sizeBudgetInputs(prefix) {
  const kind = $(`${prefix}-kind`).value;
  if (!kind) return {};
  const raw = name => String($(`${prefix}-${name}`).value).trim();
  const number = name => (raw(name) === "" ? NaN : Number(raw(name)));
  if (kind === "percent") {
    const percent = number("percent");
    if (!Number.isFinite(percent) || percent < 5 || percent > 85) throw new Error("Enter a size budget from 5 to 85 percent of the original.");
    return { size_budget_percent: percent };
  }
  if (kind === "gb") {
    const gb = number("gb");
    const size = Math.round(gb * 1e9);
    if (!Number.isFinite(gb) || size < 50000000) throw new Error("Enter a size budget of at least 0.05 GB.");
    return { size_budget_bytes: size };
  }
  const multiplier = raw("multiplier") === "" ? 1.5 : number("multiplier");
  if (!Number.isFinite(multiplier) || multiplier < 1 || multiplier > 4) throw new Error("Enter a sibling bitrate multiplier from 1.0 to 4.0.");
  return { size_budget_source: "siblings", size_budget_sibling_multiplier: multiplier };
}
const fixedBudgetKinds = ["gb", "percent"];
function updateSizeBudgetFields(prefix) {
  const kind = $(`${prefix}-kind`).value;
  $(`${prefix}-fixed`).hidden = !fixedBudgetKinds.includes(kind);
  $(`${prefix}-percent-label`).hidden = kind !== "percent";
  $(`${prefix}-gb-label`).hidden = kind !== "gb";
  $(`${prefix}-multiplier-label`).hidden = kind !== "siblings";
  if (fixedBudgetKinds.includes(kind)) $(`${prefix}-unit`).value = kind;
  const multiplier = Number(String($(`${prefix}-multiplier`).value).trim() || 1.5);
  $(`${prefix}-multiplier-down`).disabled = multiplier <= 1;
  $(`${prefix}-multiplier-up`).disabled = multiplier >= 4;
  const readout = sizeBudgetReadout(prefix, kind);
  $(`${prefix}-readout`).hidden = !readout;
  $(`${prefix}-readout`).textContent = readout;
}
// What a limit comes to, when the form can tell: a percent of the chosen
// file's size (the batch's first file, as the goal notes say). A gigabyte
// limit is its own number, and the sibling budget depends on bitrates only the
// server reads, so those say nothing here.
function sizeBudgetReadout(prefix, kind) {
  if (kind !== "percent") return "";
  const raw = String($(`${prefix}-percent`).value).trim(), percent = raw === "" ? NaN : Number(raw);
  const profile = prefix === "size-budget" && state.goalProfile?.status === "ready" ? state.goalProfile : null;
  const size = Number(profile?.data?.profile?.size_bytes);
  if (!(percent >= 5 && percent <= 85) || !(size > 0)) return "";
  return `≤ ${bytes(Math.round(size * percent / 100))}${profile.batch ? " for the first file" : ""}`;
}
function stepSizeBudgetMultiplier(prefix, delta) {
  const input = $(`${prefix}-multiplier`), raw = String(input.value).trim();
  const current = raw === "" || !Number.isFinite(Number(raw)) ? 1.5 : Number(raw);
  input.value = String(Math.min(4, Math.max(1, Math.round((current + delta) * 10) / 10)));
  sizeBudgetForms[prefix]();
}
// Segmented controls are radio groups over the field that stays the one
// source of truth: choosing a segment writes the field, and every render shows
// it back, so a field set in code never disagrees with what is on screen.
// Arrow keys move through a group natively because its radios share a name.
const segmentedGroups = [
  {name: "size-engine", field: "allow-hardware", form: "size-budget", read: f => f.checked ? "fast" : "small", write: (f, v) => { f.checked = v === "fast"; }},
  {name: "size-budget-limit", field: "size-budget-kind", form: "size-budget", read: f => fixedBudgetKinds.includes(f.value) ? "fixed" : f.value, write: (f, v, form) => { f.value = v === "fixed" ? $(`${form}-unit`).value || "gb" : v; }},
  {name: "reconfigure-engine", field: "reconfigure-hardware", form: "reconfigure-budget", read: f => f.checked ? "fast" : "small", write: (f, v) => { f.checked = v === "fast"; }},
  {name: "reconfigure-budget-limit", field: "reconfigure-budget-kind", form: "reconfigure-budget", read: f => fixedBudgetKinds.includes(f.value) ? "fixed" : f.value, write: (f, v, form) => { f.value = v === "fixed" ? $(`${form}-unit`).value || "gb" : v; }},
];
// Each form re-renders itself after a change: the goal form in controls(),
// the reconfigure dialog in updateReconfigureHardware().
const sizeBudgetForms = {"size-budget": () => controls(), "reconfigure-budget": () => updateReconfigureHardware()};
const segmentRadios = name => [...document.querySelectorAll(`input[name="${name}"]`)];
function renderSegmented(forms) {
  for (const group of segmentedGroups.filter(g => forms.includes(g.form))) {
    const value = group.read($(group.field));
    for (const radio of segmentRadios(group.name)) radio.checked = radio.value === value;
  }
}
for (const group of segmentedGroups)
  for (const radio of segmentRadios(group.name))
    radio.addEventListener("change", () => {
      if (!radio.checked) return;
      group.write($(group.field), radio.value, group.form);
      sizeBudgetForms[group.form]();
    });
for (const form of Object.keys(sizeBudgetForms)) {
  $(`${form}-unit`).addEventListener("change", () => {
    if (!fixedBudgetKinds.includes($(`${form}-kind`).value)) return;
    $(`${form}-kind`).value = $(`${form}-unit`).value;
    sizeBudgetForms[form]();
  });
  for (const field of ["gb", "percent", "multiplier"]) $(`${form}-${field}`).addEventListener("input", () => updateSizeBudgetFields(form));
  $(`${form}-multiplier-down`).addEventListener("click", () => stepSizeBudgetMultiplier(form, -0.1));
  $(`${form}-multiplier-up`).addEventListener("click", () => stepSizeBudgetMultiplier(form, 0.1));
}
// Shows the budget a saved goal was frozen with; files frozen with different
// budgets show none (the goal-change note says so).
function showSizeBudget(prefix, settings) {
  const set = (kind, field, value) => { $(`${prefix}-kind`).value = kind; if (field) $(`${prefix}-${field}`).value = value; };
  if (settings?.size_budget_percent > 0) set("percent", "percent", settings.size_budget_percent);
  else if (settings?.size_budget_bytes > 0) set("gb", "gb", Number((settings.size_budget_bytes / 1e9).toFixed(3)));
  else if (settings?.size_budget_source === "siblings") set("siblings", "multiplier", settings.size_budget_sibling_multiplier || 1.5);
  else set("");
  updateSizeBudgetFields(prefix);
}
async function buildAndSubmitJob(mode = "encode") {
  const batch = $("scope").value === "batch";
  const name = batch ? "transcode_batch" : "transcode_media";
  const simple = !$("technical").checked;
  const inputs = simple ? {mode:processingMode()} : { preserve_source_bit_depth: $("preserve-depth").checked };
  if (simple && inputs.mode === "size") {
    // Always explicit, so the choice on screen is the choice that is frozen.
    inputs.allow_hardware = $("allow-hardware").checked;
    Object.assign(inputs, sizeBudgetInputs("size-budget"));
  }
  if (simple && keepGoals.includes(inputs.mode)) Object.assign(inputs, keepTrackInputs(!batch, $("path").value.trim()));
  // Settings of an earlier job's goal that the form has no control for, while
  // it is still that file and that goal.
  const carry = state.goalCarry;
  if (simple && !batch && carry && carry.path === $("path").value.trim() && carry.mode === inputs.mode) Object.assign(inputs, carry.inputs);
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
    if (!simple) inputs.media_type = $("media-kind").value;
    if (state.fileMedia) {
      inputs.media_type = state.fileService === "radarr" ? "movie" : "tv";
      inputs.is_anime =
        state.fileMedia.seriesType === "anime" ||
        (state.fileMedia.genres || []).some((g) => g.toLowerCase() === "anime");
      inputs.library_context = {
        service: state.fileService,
        id: state.fileMedia.id,
      };
    }
  }
  if (!simple && $("custom").checked) {
    inputs.profile_config = profileFromControls();
  } else if (!simple) {
    inputs.profile = $("profile").value;
  }
  if (!simple && $("custom").checked) {
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
    const queued = sourceSummary();
    $("job-feedback").textContent = `Queued: ${queued}. Follow it in Activity.`;
    state.jobsLoaded = 0;
    finishSubmission(mode);
    selectTab("jobs");
    await loadJobs();
    notify(`Queued: ${queued}. It is listed first below.`);
  } finally {
    $("preview").disabled = false;
    controls();
  }
}
$("job-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(() => submitJob());
});
$("preview").addEventListener("click", () => safe(() => submitJob("preview")));

function focusedControl(container) {
  const active = document.activeElement;
  if (!active || !container.contains(active)) return null;
  if (active.dataset.jobControl)
    return { id: active.dataset.jobControl, label: active.textContent, itemKey: active.dataset.batchItemKey };
  // Job titles and recovery copy buttons carry no data-job-control: find
  // them again by their row.
  const row = active.closest?.("[data-operation-id]");
  if (!row || !active.matches?.(".job-title, .backup-actions button")) return null;
  const buttons = [...row.querySelectorAll(".backup-actions button")];
  return { row: row.dataset.operationId, title: active.matches(".job-title"), label: active.textContent, index: buttons.indexOf(active) };
}
function restoreControl(container, focus) {
  if (!focus) return;
  if (focus.row != null) {
    const row = [...container.querySelectorAll("[data-operation-id]")].find((r) => r.dataset.operationId === focus.row);
    if (!row) return;
    const buttons = [...row.querySelectorAll(".backup-actions button")];
    // A button that is gone or now disabled (Verifying…) hands focus to the row's first enabled one.
    let next = focus.title ? row.querySelector(".job-title") : buttons.find((b) => b.textContent === focus.label) || buttons[focus.index];
    if (!focus.title && (!next || next.disabled)) next = buttons.find((b) => !b.disabled);
    if (next && document.activeElement !== next) next.focus({ preventScroll: true });
    return;
  }
  [...container.querySelectorAll("[data-job-control]")]
    .find(
      (b) => b.dataset.jobControl === focus.id && b.textContent === focus.label && b.dataset.batchItemKey === focus.itemKey,
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
      `${savings.estimate_kind === "sampled_benchmark" ? (compact ? "Estimate (samples)" : "Sample estimate") : savings.estimate_kind === "audio_only_estimate" ? (compact ? "Estimate (audio)" : "Audio estimate") : savings.estimate_kind === "size_budget" ? (compact ? "Estimate (budget)" : "Budget estimate") : compact ? "Estimate (profile)" : "Profile estimate"} ${bytes(savings.estimated_saved_bytes)}`,
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
// ffmpeg reports a cumulative average speed, so each new remaining-time
// measurement is blended with the previous one projected forward; a jump of
// more than half the projection (restart, second pass) starts over.
const encodeEtas = new Map();
function encodeEta(key, worker, seconds) {
  const phase = worker?.transcode_phase || worker?.phase;
  const raw = Number(seconds);
  if (!key || phase !== "encoding" || worker.progress_is_stale || !(Number(worker.speed) > 0) || seconds == null || !Number.isFinite(raw) || raw <= 0) {
    if (key) encodeEtas.delete(key);
    return "";
  }
  const now = Date.now();
  const prior = encodeEtas.get(key);
  let value = raw;
  if (prior) {
    const projected = Math.max(0, prior.value - (now - prior.at) / 1000);
    if (prior.raw === raw) value = projected;
    else if (Math.abs(projected - raw) <= Math.max(120, projected * .5)) value = projected * .7 + raw * .3;
  }
  // Re-insert so the map stays in last-seen order, then drop entries no
  // render has refreshed for ten minutes and cap what remains.
  encodeEtas.delete(key);
  encodeEtas.set(key, {value, raw, at: now});
  for (const [k, entry] of encodeEtas) {
    if (encodeEtas.size <= 64 && now - entry.at <= 600000) break;
    encodeEtas.delete(k);
  }
  return remainingLabel(value);
}
function remainingLabel(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  if (seconds < 60) return "less than a minute left";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `~${minutes} min left`;
  const hours = Math.floor(minutes / 60), rest = minutes % 60;
  return `~${hours} h${rest ? ` ${rest} min` : ""} left`;
}
// Where a failed replacement left the original: "in_place", "recovery" (a
// recovery copy holds it) or "unclear". The server decides it from the
// promotion's own state; only an unlabelled failure falls back to the flag.
function originalStateOf(failure) {
  return failure?.original_state || (failure?.original_intact ? "in_place" : "unclear");
}
// What an approved batch replacement is doing, in words about the files. The
// server places each file in a short fixed sequence; none of this is a time
// estimate.
function replacementStepText(member) {
  const freed = member.freed_bytes != null && member.freed_bytes > 0 ? ` · freed ${bytes(member.freed_bytes)}` : "";
  switch (member.phase) {
    case "checking": return "Checking the files";
    case "backup": return `Backing up the original${member.percent != null ? ` (${member.percent}%)` : ""}`;
    case "backup_check": return "Checking the backup";
    case "swap": return "Swapping in the new file";
    case "final_check": return "Checking the new file";
    case "done": return `Done${freed}`;
    case "failed": return `Stopped${member.failure ? `: ${member.failure.reason}` : ""}${member.failure ? {in_place: " · original kept", recovery: " · original in Recovery", unclear: " · check Recovery"}[originalStateOf(member.failure)] : ""}`;
    default: return "Waiting";
  }
}
// One sentence for the batch: what is being replaced, or how it ended.
function replacementHeadline(job) {
  const r = job.replacement;
  if (!r?.approved || !r.total) return "";
  const settled = r.done + r.failed >= r.total;
  if (r.failed > 0) {
    const failures = (r.members || []).filter((m) => m.failure).map((m) => m.failure);
    const reasons = [...new Set(failures.map((f) => f.reason))];
    // "Kept" is only said when every failure left the original in place; "in
    // Recovery" only when a recovery copy holds it.
    const states = failures.map(originalStateOf);
    const kept = !failures.length ? "" : states.every((x) => x === "in_place") ? (failures.length === 1 ? " · original kept" : " · originals kept") : states.every((x) => x === "recovery") ? (failures.length === 1 ? " · original in Recovery" : " · originals in Recovery") : " · check Recovery";
    if (!settled) return `Replacing ${fileCount(r.total)} · ${r.failed} failed`;
    return `${r.done} of ${r.total} replaced · ${r.failed} failed${reasons.length === 1 ? `: ${reasons[0]}` : ""}${kept}`;
  }
  if (r.done >= r.total) return `Replaced ${fileCount(r.total)}${r.freed_bytes > 0 ? ` · freed ${bytes(r.freed_bytes)}` : ""}`;
  return `Replacing ${fileCount(r.total)}${r.frees_bytes > 0 ? ` · frees ${bytes(r.frees_bytes)}` : ""}`;
}
// The progress block of a batch whose replacement was approved and is still
// running, or stopped on a failure: a headline, a bar of finished files, and
// one line per file.
// How much of an approved replacement is done, counting each file by the step
// it is on, so the bar moves while a big file is backed up and does not sit at
// "0 of 6" until the first one finishes.
const replacementStepShare = {checking: .05, backup: .1, backup_check: .5, swap: .6, final_check: .85, done: 1};
function replacementFraction(r) {
  if (!r?.total) return 0;
  const members = r.members || [];
  if (!members.length) return Math.min(1, r.done / r.total);
  let done = 0;
  for (const member of members) {
    if (member.phase === "backup" && member.percent != null) done += .1 + .4 * Math.max(0, Math.min(100, member.percent)) / 100;
    else done += replacementStepShare[member.phase] || 0;
  }
  return Math.max(r.done / r.total, Math.min(1, done / r.total));
}
function replacementProgressModel(job) {
  const r = job.replacement;
  if (!r?.approved || !r.total) return null;
  const active = operationIsActive(job.status);
  if (!active && !r.failed && r.done >= r.total) return null;
  const stopped = !active && r.failed > 0;
  const heading = stopped ? replacementHeadline(job) : `Replacing ${fileCount(r.total)} \u00b7 ${r.done} of ${r.total} done`;
  return {
    kind: "replacement",
    className: `job-progress batch-progress replacement-progress${stopped ? " failed" : ""}`,
    parts: [
      {type: "text", tag: "strong", text: heading, cls: "replacement-headline"},
      {type: "progress", max: r.total, value: r.done, fraction: replacementFraction(r), label: `${r.done} of ${r.total} files replaced`},
      {type: "files", members: r.members || []},
    ],
  };
}
// One progress model, two looks: the queue rows draw it as small text and
// native bars; the job dialog lays the same parts out itself.
function segmentShare(batch, runningFill) {
  const count = key => Number.isInteger(batch[key]) && batch[key] > 0 ? batch[key] : 0;
  const parts = segmentParts(batch);
  const total = Math.max(batch.total, parts.reduce((sum, part) => sum + part.n, 0));
  const done = ["completed", "failed", "policy", "skip", "rejected"].reduce((sum, key) => sum + count(key), 0);
  const running = count("running") * (Number.isFinite(runningFill) ? Math.max(0, Math.min(1, runningFill)) : 0);
  return {total, share: Math.min(1, (done + running) / Math.max(1, total - count("cancelled")))};
}
const replacementActivePhases = new Set(["checking", "backup", "backup_check", "swap", "final_check", "failed"]);
const replacementRowFiles = 4;
function renderProgressPart(part) {
  if (part.type === "text") {
    const result = node(part.tag || "span", part.text, part.cls);
    // A line a row may shorten keeps its full text reachable.
    if (part.title) result.title = part.title;
    else if (!part.sentence && part.text.length > 24) result.title = part.text;
    return result;
  }
  if (part.type === "meter") {
    const meter = document.createElement("meter");
    meter.min = part.min;
    meter.max = part.max;
    meter.value = part.value;
    meter.setAttribute("aria-label", part.label);
    return meter;
  }
  if (part.type === "progress") {
    const bar = document.createElement("progress");
    bar.max = part.max;
    bar.value = part.value;
    bar.setAttribute("aria-label", part.label);
    return bar;
  }
  if (part.type === "segments") {
    const segments = batchSegments(part.batch, part.runningFill);
    segments.bar.title = part.title;
    return segments.bar;
  }
  // A row says where the batch is: the files being worked on or stopped keep a
  // line each (the headline counts the rest), and the job dialog has them all.
  const list = node("ul", "", "replacement-files");
  const shown = part.members.filter((member) => replacementActivePhases.has(member.phase));
  for (const member of shown.slice(0, replacementRowFiles)) {
    const item = node("li", "", `replacement-file ${member.phase}`);
    const name = node("span", member.name, "replacement-file-name");
    name.title = member.name;
    const step = node("span", replacementStepText(member), "replacement-file-step metadata");
    step.title = step.textContent;
    item.append(name, step);
    list.append(item);
  }
  if (shown.length > replacementRowFiles) list.append(node("li", `and ${shown.length - replacementRowFiles} more`, "replacement-file replacement-file-more metadata"));
  return list;
}
function renderProgressModel(model) {
  if (!model) return null;
  const result = node("div", "", model.className);
  for (const part of model.parts) result.append(renderProgressPart(part));
  return result;
}
// The progress block of a batch whose replacement was approved and is still
// running, or stopped on a failure: a headline, a bar of finished files, and
// one line per file.
function replacementProgressBlock(job) {
  return renderProgressModel(replacementProgressModel(job));
}
async function copyText(text, done) {
  if (typeof navigator === "undefined" || !navigator.clipboard?.writeText) return notify("Copying is not available here.");
  await navigator.clipboard.writeText(text);
  notify(done);
}
// Where a replacement put things, short by default: the new file, and the
// original's backup while one exists.
function whereBlock(where, {heading = true} = {}) {
  if (!where || !(where.new_path || where.backup_path || where.backup_removed)) return null;
  const box = node("div", "", "replacement-where");
  if (heading) box.append(node("strong", "Where"));
  const line = (label, path, note) => {
    const row = node("p", "", "metadata replacement-where-row");
    row.append(node("span", `${label}: `), node("span", path || note, "backup-path"));
    if (path) row.append(button("Copy", () => safe(() => copyText(path, `${label} path copied.`)), "text-button"));
    box.append(row);
  };
  if (where.new_path) line("New file", where.new_path);
  if (where.backup_path) {
    line("Original backup", where.backup_path);
    box.append(node("p", "Kept until the new file is verified, then removed. If a replacement stops, it stays until you remove it from Recovery.", "metadata"));
  } else if (where.backup_removed) line("Original backup", "", "removed after the new file was verified");
  return box;
}
// What each file of a batch is doing: one bar for how far along it is, and a
// legend counting finished, failed, running, needing a decision, left alone,
// stopped, and the rest waiting.
const segmentKinds = [["completed", "done", "ok"], ["failed", "failed", "err"], ["running", "running", "run"], ["waiting_decision", "need you", "rev"], ["policy", "below minimum savings", "skip"], ["skip", "skipped", "skip"], ["rejected", "rejected", "skip"], ["cancelled", "cancelled", "cancel"]];
function segmentParts(batch) {
  if (!batch || batch.dry_run || !Number.isInteger(batch.total) || batch.total < 2) return null;
  const count = key => Number.isInteger(batch[key]) && batch[key] > 0 ? batch[key] : 0;
  const parts = segmentKinds.map(([key, label, kind]) => ({kind, label, n: key === "waiting_decision" ? count("waiting_decision") || count("review") : count(key)})).filter(part => part.n > 0);
  const known = parts.reduce((sum, part) => sum + part.n, 0);
  const waiting = Math.max(0, batch.total - known);
  if (waiting) parts.push({kind: "wait", label: "waiting", n: waiting});
  return parts;
}
function batchSegments(batch, runningFill) {
  const parts = segmentParts(batch);
  if (!parts) return null;
  const {total, share} = segmentShare(batch, runningFill);
  const bar = node("div", "", "batch-segments");
  bar.setAttribute("role", "img");
  const legend = parts.map(part => `${part.n} ${part.label}`).join(" \u00b7 ");
  bar.setAttribute("aria-label", `${total} files: ${legend}`);
  // One fill, not one colour per state: the words under the bar say what each
  // file is doing. Running files count only as far as their measured steps, so
  // a file still waiting for a worker slot does not read as a finished bar.
  const fill = node("i", "", "segment-ok");
  fill.style.width = `${share * 100}%`;
  bar.append(fill);
  return {bar, legend, kinds: parts.length};
}
function batchProgressModel(job, {segments = false} = {}) {
  const replacing = replacementProgressModel(job);
  if (replacing) return replacing;
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
    const phase = w.transcode_phase || w.phase;
    let fraction = 0;
    const stage = stages[step]?.name;
    const workerStage = stage === "wait_transcode";
    if (workerStage && !w.progress_is_stale) {
      if (Number.isFinite(Number(w.progress)) && w.progress != null && !["queued","preparing"].includes(phase)) fraction = Math.max(0, Math.min(1, Number(w.progress)/100));
    }
    if (stage === "preflight" && activity.work?.total_bytes > 0) fraction = Math.min(.8, Math.max(0, Number(activity.work.bytes_read || 0)/activity.work.total_bytes*.8));
    else if (stage === "preflight" && activity.work?.phase === "probing_source") fraction = .8;
    const after = postEncodeWork({...activity, worker: w, status: "running"});
    if (after?.total > 0 && after.read != null) fraction = after.read / after.total;
    const progress = stages.length && Number.isInteger(step) ? Math.min(.99, (step + fraction)/stages.length) : 0;
    const key = activity.file || activity.id || "Active file";
    const prior = byFile.get(key);
    const observation = workerProgressObservation({...(child || activity), worker: w, status: "running"});
    const eta = workerStage && !observation.message ? encodeEta(activity.id || key, w, child?.eta_seconds ?? activity.eta_seconds) : "";
    const measurement = workerStage ? observation.message || (w.progress_is_stale ? heartbeatAlive(observation.heartbeat) ? "Worker heartbeat received; last measurement shown" : "Last measurement; stale" : eta) : "";
    if (!prior || progress > prior.progress) byFile.set(key, {progress, eta: eta ? encodeEtas.get(activity.id || key)?.value : null, label: `${key.split("/").pop() || "Active file"}: ${after ? postEncodeWorkLabel(after) : activity.work?.phase === "waiting_for_worker" ? workerSlotWaitLabel(activity.work) : workerStage && phase ? workerPhaseLabel(phase) : stage === "preflight" && activity.work?.phase === "hashing_source" ? `Verifying source SHA-256 (${bytes(activity.work.bytes_read || 0)} / ${bytes(activity.work.total_bytes)})` : stage === "preflight" && activity.work?.phase === "waiting_for_hash_slot" ? "Waiting to verify source" : stageLabel(stage || "preflight")}${measurement ? ` · ${measurement}` : ""}`});
  }
  const partial = Math.min(count("running"), byFile.size) ? [...byFile.values()].slice(0,count("running")).reduce((sum,a)=>sum+a.progress,0) : 0;
  const active = operationIsActive(job.status);
  // A cancelled file will never run, so it is not work left to do.
  const workTotal = Math.max(1, batch.total - count("cancelled"));
  let estimate = (processed + partial) / workTotal;
  if (active && job.replacement_requested) {
    const promotion = batch.promotion;
    const conversion = Math.min(1,estimate);
    const replacement = promotion?.eligible > 0 ? Math.min(1,Number(promotion.promoted || 0)/promotion.eligible) : 0;
    estimate = conversion * .85 + replacement * .15;
  }
  const percent = Math.min(active ? 99 : 100, estimate * 100);
  const presented = queuePresentation(job).status;
  const className = `job-progress batch-progress ${presented === "Failed" ? "failed" : presented === "Cancelled" ? "cancelled" : ""}`;
  const countText = `${processed} / ${batch.total} files finished`;
  const countTitle = "Processed includes completed, failed, skipped and rejected files. Cancelled files and files waiting for a decision are not counted as processed.";
  const valueText = `${active ? "~" : ""}${active ? Math.min(99,Math.round(percent)) : Math.round(percent)}%`;
  const valueTitle = active ? "Estimated overall workflow progress: finished files plus active workflow steps and measured worker work. Steps have equal weight; requested replacements reserve 15% of work. This is not a time estimate." : "Files processed";
  const runningFill = count("running") ? partial / Math.min(count("running"), Math.max(1, byFile.size)) : undefined;
  const spread = segments ? segmentParts(batch) : null;
  const parts = [];
  if (spread) {
    // The bar shows each file's state; the percent keeps the estimate. The
    // legend already counts the files, so the "N / M finished" line is left out.
    parts.push({type: "text", tag: "strong", text: valueText, cls: "batch-progress-percent", title: valueTitle}, {type: "segments", batch, runningFill, title: valueTitle, fraction: segmentShare(batch, runningFill).share},
      {type: "text", text: spread.map(part => `${part.n} ${part.label}`).join(" \u00b7 "), cls: "batch-legend metadata"});
  } else {
    parts.push({type: "text", text: countText, cls: "batch-progress-count", title: countTitle}, {type: "text", tag: "strong", text: valueText, cls: "batch-progress-percent", title: valueTitle},
      {type: "progress", max: workTotal, value: percent / 100 * workTotal, fraction: percent / 100, label: active ? `Estimated overall workflow progress ${percent.toFixed(1)}%` : `${processed} of ${workTotal} files processed`});
  }

  // A time is shown only when the worker measured one.
  const etas = active ? [...byFile.values()].map(a => a.eta).filter(eta => eta > 0) : [];
  const left = Math.max(0, batch.total - processed - count("waiting_decision") - count("cancelled"));
  const etaText = etas.length ? `Current encode ${remainingLabel(Math.max(...etas))}${left > 0 ? ` \u00b7 about ${left} ${left === 1 ? "file" : "files"} left` : ""}` : "";
  if (active) {
    const phases = [...byFile.values()].map(a=>a.label);
    // The legend of a segmented bar already counts the files needing a decision.
    const waiting = !spread && count("waiting_decision") ? `${count("waiting_decision")} ${count("waiting_decision") === 1 ? "file needs" : "files need"} your decision` : "";
    const phaseText = [...phases, waiting, spread && etas.length && left > 0 ? `about ${left} ${left === 1 ? "file" : "files"} left` : ""].filter(Boolean).join(" \u00b7 ") || (processed === batch.total ? "Finishing batch / replacement checks" : "Waiting for worker / preparing selected files");
    // One line, whatever the numbers say; the whole text is the tooltip.
    parts.push({type: "text", text: phaseText, cls: "batch-progress-phase metadata", title: phaseText});
  }
  // The line is always there while a plain bar runs, so the row keeps its height
  // when an estimate appears or goes away; a segmented bar folds it into the phase line.
  if (active && !spread) parts.push({type: "text", text: etaText || "\u00a0", cls: "batch-progress-explanation metadata", title: etas.length ? etaText : ""});
  return {kind: "batch", className, parts};
}
function batchProgress(job, options) {
  return renderProgressModel(batchProgressModel(job, options));
}
function telemetryModel(job, options) {
  const batch = batchProgressModel(job, options);
  if (batch) return batch;
  const w = job.worker;
  const model = {kind: "worker", className: `job-progress ${w ? "worker-progress" : "workflow-progress"}`, parts: []};
  const text = (value, cls = "metadata", sentence = false) => model.parts.push({type: "text", text: value, cls, sentence});
  if (job.activities?.length > 1) {
    text(`${job.activities.length} active files · See worker slots above for each file and phase`);
    return model;
  }
  const observation = workerProgressObservation(job);
  if (observation.message) {
    model.className = `job-progress ${observation.warning ? "stalled-progress" : "workflow-progress"}`;
    const measured = w?.progress ?? w?.last_known_progress?.progress;
    const last = measured != null && Number.isFinite(Number(measured))
      ? ` Last reported: ${Number(measured).toFixed(1)}%.` : "";
    text(observation.message + (observation.warning ? last : ""), "metadata", true);
    return model;
  }
  const after = postEncodeWork(job);
  if (after) {
    // What it is doing and how much sit on the first line, the bar has the
    // second to itself, and the step it belongs to is the third: the same
    // three lines at every size, whatever the numbers read.
    model.className = "job-progress worker-progress";
    text([job.activity_file ? `Current: ${job.activity_file.split("/").pop()}` : null, after.label].filter(Boolean).join(" \u00b7 "), "metadata single-line");
    model.parts.at(-1).title = model.parts.at(-1).text;
    if (after.read != null && after.total > 0) {
      model.parts.push({type: "meter", min: 0, max: after.total, value: after.read, label: `${after.measure || "Original checked"} ${(after.read / after.total * 100).toFixed(1)}%`}, {type: "text", text: `${bytes(after.read)} / ${bytes(after.total)}`, cls: "metadata", unit: true});
    }
    if (after.step) { text(after.step, "metadata progress-observation single-line"); model.parts.at(-1).title = after.step; }
    if (postEncodeWorkerPhases.includes(w?.transcode_phase) && w.last_progress_at)
      text(`Encode finished ${new Date(w.last_progress_at).toLocaleTimeString()}`, "metadata progress-observed-at");
    return model;
  }
  if (!w) {
    const stages = job.stages || [];
    const current = stages[job.current_step];
    text(current
      ? `Step ${job.current_step + 1}/${stages.length} · ${stageLabel(current.name)}`
      : job.waiting_reason || "Starting job", "metadata", Boolean(job.waiting_reason));
    return model;
  }
  const phase = w.transcode_phase || w.phase;
  const phases = {
    queued: "Queued",
    preparing: "Preparing",
    encoding: "Encoding",
    validating: "Validating",
    publishing: "Publishing",
    completed: "Completed",
  };
  const parts = [
    job.activity_file ? `Current: ${job.activity_file.split("/").pop()}` : null,
    workerPhaseLabel(phase) || phases[phase] || phase,
    w.queue_position > 0 ? `Queue #${w.queue_position}` : null,
    phase === "encoding" && !w.progress_is_stale && w.speed > 0 ? `${Number(w.speed).toFixed(2)}×` : null,
    phase === "encoding" && !w.progress_is_stale && w.fps > 0 ? `${Number(w.fps).toFixed(1)} fps` : null,
  ];
  const eta = job.savings?.eta_seconds;
  parts.push(encodeEta(job.id || "job", w, eta));
  text(parts.filter(Boolean).join(" · ") || job.progress);
  const measured = w.progress ?? w.last_known_progress?.progress;
  if (!["queued", "preparing"].includes(phase) && measured != null && Number.isFinite(Number(measured))) {
    const p = Math.max(0, Math.min(100, Number(measured)));
    model.parts.push({type: "meter", min: 0, max: 100, value: p, label: `${w.progress_is_stale ? "Last measured" : "Worker"} progress ${p.toFixed(1)}%`}, {type: "text", text: `${w.progress_is_stale ? "Last measured: " : ""}${p.toFixed(1)}%`, cls: "metadata", unit: true});
  }
  // In a queue row the heartbeat line says when the last measurement moved, so the time is left out.
  if (w.last_progress_at && !(options?.segments && observation.active))
    text(`${postEncodeWorkerPhases.includes(w.transcode_phase) ? "Encode finished" : "Measured"} ${new Date(w.last_progress_at).toLocaleTimeString()}`, "metadata progress-observed-at");
  if (observation.active) {
    const last = activityElapsed(w.last_progress_at);
    text(`${heartbeatAlive(observation.heartbeat) ? `Worker heartbeat ${activityElapsed(w.worker_heartbeat_at)} ago` : "Worker heartbeat unavailable"}${last ? ` · Last advance ${last} ago` : ""}`, "metadata progress-observation");
    if (phase === "analysing_video")
      text("The first x265 pass analyzes the video. Encoding progress starts during the second pass.", "metadata progress-observation", true);
    if (phase === "validating_quality")
      text("Final quality validation can take several minutes. The percentage reflects the last encoding measurement.", "metadata progress-observation", true);
  }
  return model;
}
function telemetry(job, options) {
  return renderProgressModel(telemetryModel(job, options));
}
// The encode a ladder run is on, from the "(step N of M)" the server writes
// into its status messages: "x265 CRF 20 (step 1 of 3)", "quality 65 (step 2 of 5)".
const encoderNames = {libx265: "x265", hevc_videotoolbox: "VideoToolbox"};
function ladderStep(job) {
  for (const text of [job.benchmark?.message, job.transcode?.message]) {
    const step = /\(step (\d+) of (\d+)\)/.exec(typeof text === "string" ? text : "");
    if (!step) continue;
    const rung = /((?:x265 |VideoToolbox )?(?:CRF|quality) \d+) \(step/.exec(text)?.[1] || "";
    const encoder = encoderNames[job.size_mode?.encoder] || (/VideoToolbox/.test(text) ? "VideoToolbox" : /x265|CRF/.test(text) ? "x265" : "");
    return {number: Number(step[1]), of: Number(step[2]), rung: rung && !/^(x265|VideoToolbox)/.test(rung) && encoder ? `${encoder} ${rung}` : rung};
  }
  return null;
}
// Short facts about a file's job: size, encoder, ladder try and size limit.
// Unknown parts are left out rather than named.
function detailFacts(job) {
  if (job.podcast || job.batch?.total > 1) return [];
  const facts = [];
  // Nothing was encoded, so there is no result size, encoder or try to report:
  // only the source and the limit it could not meet.
  if (notConverted(job)) {
    const size = Number(job.savings?.source_bytes), cap = Number(job.size_mode?.size_budget?.budget_bytes);
    if (size > 0) facts.push(bytes(size));
    if (cap > 0) facts.push(`Limit ≤ ${bytes(cap)}`);
    return facts;
  }
  const source = Number(job.savings?.source_bytes), candidate = Number(job.savings?.candidate_bytes), expected = Number(job.size_mode?.expected_bytes);
  if (source > 0) facts.push(candidate > 0 ? `${bytes(source)} → ${bytes(candidate)}` : expected > 0 ? `${bytes(source)} → ~${bytes(expected)}` : bytes(source));
  const ladder = ladderStep(job);
  const encoder = ladder?.rung || encoderNames[job.size_mode?.encoder] || "";
  if (encoder) facts.push(encoder);
  if (ladder) facts.push(`Try ${ladder.number} of ${ladder.of}`);
  const limit = Number(job.size_mode?.size_budget?.budget_bytes);
  if (limit > 0) facts.push(`Limit ≤ ${bytes(limit)}`);
  return facts;
}
// The one progress line of a running file: "42.5% · 1.35× · ~25 min left",
// with a dot for the worker's heartbeat. Batches, several active files,
// post-encode checks and worker messages keep telemetry's richer text.
function detailProgressModel(job) {
  const w = job.worker;
  const observation = workerProgressObservation(job);
  // A wait, an offline worker, the original's check or a batch says more than
  // the step line does; otherwise, without a worker, the step line already
  // names the current step.
  if (job.activities?.length > 1 || batchProgressModel(job) || postEncodeWork(job) || observation.message) return telemetryModel(job);
  if (!w) return job.stages?.[job.current_step] ? null : telemetryModel(job);
  const phase = w.transcode_phase || w.phase;
  const measured = w.progress ?? w.last_known_progress?.progress;
  const known = !["queued", "preparing"].includes(phase) && measured != null && Number.isFinite(Number(measured));
  const percent = known ? Math.max(0, Math.min(100, Number(measured))) : null;
  const live = phase === "encoding" && !w.progress_is_stale;
  const parts = percent != null
    ? [`${w.progress_is_stale ? "Last measured " : ""}${percent.toFixed(1)}%`, phase === "encoding" ? null : workerPhaseLabel(phase)]
    : [workerPhaseLabel(phase), w.queue_position > 0 ? `Queue #${w.queue_position}` : null];
  parts.push(live && w.speed > 0 ? `${Number(w.speed).toFixed(2)}×` : null, encodeEta(job.id || "job", w, job.savings?.eta_seconds));
  const model = {kind: "file", className: "job-progress worker-progress detail-progress", parts: []};
  if (percent != null) model.parts.push({type: "meter", min: 0, max: 100, value: percent, label: `${w.progress_is_stale ? "Last measured" : "Worker"} progress ${percent.toFixed(1)}%`});
  model.parts.push({type: "text", text: parts.filter(Boolean).join(" · "), cls: "detail-progress-text"});
  if (observation.active && observation.heartbeat !== "unknown")
    model.parts.push({type: "dot", state: observation.heartbeat, text: heartbeatAlive(observation.heartbeat) ? "Worker live" : "Worker silent", title: `Last heartbeat ${new Date(w.worker_heartbeat_at).toLocaleTimeString()}`});
  return model;
}
function stageLabel(name) {
  const labels = {
    plan_promotion: "Verify original and candidate",
    approve_promotion: "Review replacement",
    preserve_original: "Verify original and prepare recovery",
    import_candidate: "Import verified candidate",
    remove_old_file: "Remove original library file",
    rename_candidate: "Finalize filename",
    rescan_library: "Refresh library",
    finalize_promotion: "Verify replacement and clean up recovery",
    resolve_source: "Inspect source",
    resolve_media: "Inspect source",
    resolve_batch: "Inspect selected files",
    resolve_and_inspect: "Inspect selected files",
    preflight: "Inspect original and prepare conversion",
    submit_benchmark: "Plan the encode",
    wait_benchmark: "Choose encode settings",
    submit_transcode: "Send conversion to worker",
    wait_transcode: "Convert file",
    schedule_batch: "Process selected files",
    promote_batch: "Replace approved files",
    benchmark_transcode: "Calibrate quality",
    build_plan: "Prepare conversion",
    execute_transcode: "Convert file",
    validate_result: "Verify converted file",
    accept_result: "Review converted file",
  };
  return labels[name] || String(name || "Processing").replaceAll("_", " ");
}
const statDay = (date) => new Date(`${date}T12:00:00`).toLocaleDateString("en", { month: "short", day: "numeric" });
// "5 min ago", "3 h ago", then the date: enough to tell fresh work from old.
function agoText(iso, now = Date.now()) {
  const at = Date.parse(iso);
  if (!Number.isFinite(at)) return "";
  const minutes = Math.max(0, Math.round((now - at) / 60000));
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes} min ago`;
  if (minutes < 24 * 60) return `${Math.round(minutes / 60)} h ago`;
  return new Date(at).toLocaleDateString("en", { month: "short", day: "numeric" });
}
// The chart: one bar per local day, the last one being today.
function renderFreedPerDay(days) {
  const chart = $("freed-chart");
  chart.replaceChildren();
  const total = (n) => days.slice(-n).reduce((sum, d) => sum + (d.saved_bytes || 0), 0);
  const files = days.reduce((sum, d) => sum + (d.count || 0), 0);
  if (!files) {
    $("freed-summary").textContent = "";
    chart.append(node("p", `Nothing freed in the last ${days.length || 30} days.`, "muted"));
    return;
  }
  $("freed-summary").textContent = `Today ${bytes(total(1))} · 7 days ${bytes(total(7))} · ${days.length} days ${bytes(total(days.length))}`;
  const peak = Math.max(...days.map((d) => d.saved_bytes || 0), 1);
  const bars = node("div", "", "freed-bars");
  bars.setAttribute("role", "img");
  bars.setAttribute("aria-label", `Space freed per day over ${days.length} days: ${$("freed-summary").textContent}`);
  days.forEach((d, i) => {
    const bar = node("i", "");
    const share = (d.saved_bytes || 0) / peak;
    bar.style.height = d.saved_bytes ? `${Math.max(4, share * 100).toFixed(1)}%` : "";
    if (!d.saved_bytes) bar.dataset.empty = "";
    if (i === days.length - 1) bar.dataset.today = "";
    bar.title = `${i === days.length - 1 ? "Today" : statDay(d.date)} · ${d.saved_bytes ? `${bytes(d.saved_bytes)} from ${d.count} replacement${d.count === 1 ? "" : "s"}` : "nothing freed"}`;
    bars.append(bar);
  });
  chart.append(bars);
  const axis = node("div", "", "freed-axis metadata");
  axis.append(node("span", statDay(days[0].date)), node("span", `Busiest ${bytes(peak)}`), node("span", "Today"));
  chart.append(axis);
}
function statRow(item, detail, extra) {
  const name = item.source_path?.split("/").pop() || "File";
  const row = node("li", "", "stat-row");
  const link = jobLink(name, item.action_id);
  link.className = "stat-link";
  const facts = node("span", "", "stat-facts metadata");
  facts.append(node("strong", `−${bytes(item.saved_bytes)}`), detail ? node("span", detail) : "");
  row.append(link, facts);
  if (extra) row.append(extra);
  return row;
}
function renderSavings(data) {
  const savings = data.savings || {};
  $("saved-total").textContent = bytes(savings.realized_bytes ?? 0);
  $("saved-count").textContent =
    `${savings.completed_replacements || 0} verified replacement${savings.completed_replacements === 1 ? "" : "s"}`;
  $("candidate-total").textContent = bytes(
    savings.candidate_saved_bytes ?? savings.candidate_bytes ?? 0,
  );
  const waiting = savings.candidate_count || 0;
  $("candidate-count").textContent = waiting ? `${waiting} file${waiting === 1 ? "" : "s"} · Potential savings` : "Potential savings";
  $("estimated-total").textContent = bytes(
    savings.estimated_saved_bytes ?? savings.estimated_bytes ?? 0,
  );
  renderFreedPerDay(data.daily || []);
  const recent = $("recent-replaced");
  recent.replaceChildren();
  for (const item of (data.history || []).slice(-8).reverse()) recent.append(statRow(item, agoText(item.completed_at)));
  if (!recent.children.length) recent.append(node("li", "Nothing has been replaced yet.", "muted"));
  const ready = $("ready-list");
  ready.replaceChildren();
  for (const item of data.ready || []) ready.append(statRow(item, `${bytes(item.source_bytes)} → ${bytes(item.candidate_bytes)}`));
  if (!ready.children.length) ready.append(node("li", "No finished encodes are waiting.", "muted"));
  else if (waiting > (data.ready || []).length) ready.append(node("li", `${waiting - data.ready.length} more waiting, smaller than these.`, "muted"));
}
// Stats: how full the NAS is, what each served folder holds, and how that has
// moved. The numbers come from the server's periodic measurement.
function storageSpan(change, span) {
  if (change == null) return null;
  if (change < 0) return `${bytes(-change)} freed ${span}`;
  if (change > 0) return `${bytes(change)} more ${span}`;
  return `No change ${span}`;
}
// The changes a trend can honestly report; a window with no old enough sample
// is left out rather than guessed.
function storageChanges(trend) {
  const parts = [
    storageSpan(trend?.change_7d, "in 7 days"),
    storageSpan(trend?.change_30d, "in 30 days"),
  ].filter(Boolean);
  if (!parts.length) {
    const all = storageSpan(trend?.change_all, "since the first measurement");
    if (all) parts.push(all);
  }
  return parts;
}
function storageDate(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleDateString("en", { month: "short", day: "numeric" });
}
function storageSparkline(trend, label) {
  const points = (trend?.points || []).filter((p) => Number.isFinite(Number(p.bytes)));
  if (points.length < 2) return null;
  const values = points.map((p) => Number(p.bytes));
  const low = Math.min(...values), high = Math.max(...values);
  const span = high - low || 1;
  const wrap = node("div", "", "storage-trend");
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 300 56");
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("role", "img");
  svg.setAttribute(
    "aria-label",
    `${label} from ${bytes(values[0])} on ${storageDate(points[0].t)} to ${bytes(values.at(-1))} on ${storageDate(points.at(-1).t)}`,
  );
  const line = document.createElementNS("http://www.w3.org/2000/svg", "polyline");
  line.setAttribute(
    "points",
    values.map((v, i) => `${((i / (values.length - 1)) * 300).toFixed(1)},${(52 - ((v - low) / span) * 48).toFixed(1)}`).join(" "),
  );
  svg.append(line);
  const axis = node("div", "", "storage-trend-axis metadata");
  axis.append(node("span", storageDate(points[0].t)), node("span", storageDate(points.at(-1).t)));
  wrap.append(svg, axis);
  return wrap;
}
function storageLegendRow(name, path, size, total, detail, swatch) {
  const row = node("li", "", "storage-row");
  row.append(node("i", "", `storage-swatch ${swatch}`));
  const label = node("span", name, "storage-name");
  if (path) label.title = path;
  const figures = node("span", "", "storage-figures");
  figures.append(node("strong", bytes(size)));
  if (total > 0) figures.append(node("span", `${Math.round((size / total) * 100)}%`, "metadata"));
  row.append(label, figures);
  if (detail?.length) row.append(node("span", detail.join(" · "), "storage-detail metadata"));
  return row;
}
function storageVolumeCard(data, volume, index) {
  const card = node("article", "", "storage-volume");
  const many = data.volumes.length > 1;
  const total = Number(volume.total_bytes) || 0;
  const used = Number(volume.used_bytes) || 0;
  const head = node("div", "", "storage-volume-head");
  head.append(
    node("h4", many ? `Volume ${index + 1}` : "NAS", "storage-volume-name"),
    node("strong", `${bytes(used)} used of ${bytes(total)}`, "storage-volume-used"),
  );
  card.append(head);
  const free = Number(volume.free_bytes) || 0;
  card.append(
    node(
      "p",
      `${bytes(free)} free${total ? ` · ${Math.round((used / total) * 100)}% full` : ""}`,
      "metadata storage-volume-free",
    ),
  );
  const folders = (data.folders || []).filter((f) => f.volume === volume.id);
  const counted = folders.filter((f) => !f.inside);
  const scale = Math.max(total, counted.reduce((n, f) => n + f.bytes, 0) + (volume.other_bytes || 0), 1);
  const bar = node("div", "", "storage-bar");
  bar.dataset.bar = "";
  bar.setAttribute("role", "img");
  bar.setAttribute(
    "aria-label",
    `${bytes(used)} of ${bytes(total)} used: ${counted.map((f) => `${f.name} ${bytes(f.bytes)}`).join(", ")}${volume.other_bytes ? `, other ${bytes(volume.other_bytes)}` : ""}`,
  );
  counted.forEach((f, i) => {
    const seg = node("i", "", `storage-c${i % 6}`);
    seg.style.width = `${Math.max(0, (f.bytes / scale) * 100).toFixed(2)}%`;
    bar.append(seg);
  });
  if (volume.other_bytes > 0) {
    const seg = node("i", "", "storage-other");
    seg.style.width = `${((volume.other_bytes / scale) * 100).toFixed(2)}%`;
    bar.append(seg);
  }
  card.append(bar);
  const list = node("ul", "", "storage-legend");
  let swatch = 0;
  for (const f of folders) {
    const detail = f.inside ? [`inside ${f.inside.split("/").pop()}`] : storageChanges(f.trend);
    list.append(storageLegendRow(f.name, f.path, f.bytes, total, detail, f.inside ? "storage-nested" : `storage-c${swatch++ % 6}`));
  }
  if (volume.other_bytes > 0) list.append(storageLegendRow("Everything else", "", volume.other_bytes, total, ["Not in a folder the console serves"], "storage-other"));
  list.append(storageLegendRow("Free", "", free, total, [], "storage-free"));
  card.append(list);
  const changes = storageChanges(volume.trend);
  const spark = storageSparkline(volume.trend, "Used space");
  if (spark || changes.length) {
    const trend = node("div", "", "storage-volume-trend");
    trend.append(node("h5", "Recovery", "storage-trend-title"));
    if (changes.length) trend.append(node("p", changes.join(" · "), "storage-changes"));
    if (spark) trend.append(spark);
    card.append(trend);
  }
  return card;
}
function renderStorage(data) {
  state.storage = data;
  const measuring = Boolean(data.measuring);
  $("storage-refresh").disabled = measuring || !serverReachable;
  $("storage-refresh").textContent = measuring ? "Measuring…" : "Measure now";
  const host = $("storage-volumes");
  host.replaceChildren();
  const volumes = data.volumes || [];
  volumes.forEach((volume, index) => host.append(storageVolumeCard(data, volume, index)));
  const known = new Set(volumes.map((v) => v.id));
  const loose = (data.folders || []).filter((f) => !known.has(f.volume));
  if (loose.length) {
    const list = node("ul", "", "storage-legend");
    loose.forEach((f, i) => list.append(storageLegendRow(f.name, f.path, f.bytes, 0, storageChanges(f.trend), `storage-c${i % 6}`)));
    host.append(list);
  }
  const status = [];
  if (measuring) status.push("Measuring now. A large library can take a few minutes.");
  else if (data.measured_at) {
    const when = new Date(data.measured_at).toLocaleString("en", { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
    const sources = new Set([...volumes, ...(data.folders || [])].map((x) => x.source));
    status.push(`Last measured ${when}${sources.size === 1 && sources.has("helper") ? ", inside the NAS" : sources.has("mount") ? ", through the network share" : ""}. It repeats every 6 hours.`);
  } else if (!volumes.length) status.push("Not measured yet. It runs by itself shortly after start; use Measure now to run it immediately.");
  if (data.error) status.push(`The last measurement failed: ${data.error}`);
  for (const note of data.notes || []) status.push(note);
  $("storage-status").textContent = status.join(" ");
}
async function loadStorage() {
  const revision = (state.storageRevision = (state.storageRevision || 0) + 1);
  const data = await api("storage");
  if (revision !== state.storageRevision || state.tab !== "stats") return;
  renderStorage(data);
  clearTimeout(state.storageTimer);
  if (data.measuring) state.storageTimer = setTimeout(() => void safe(loadStorage), 3000);
}
async function measureStorage() {
  const revision = (state.storageRevision = (state.storageRevision || 0) + 1);
  $("storage-refresh").disabled = true;
  let data;
  try {
    data = await api("storage", {});
  } catch (e) {
    $("storage-refresh").disabled = !serverReachable;
    throw e;
  }
  if (revision !== state.storageRevision || state.tab !== "stats") return;
  renderStorage(data);
  clearTimeout(state.storageTimer);
  if (data.measuring) state.storageTimer = setTimeout(() => void safe(loadStorage), 3000);
}
// An action on a file or replacement changes what the open details show (the
// batch lists that file), whichever job the details are of: read them again at
// once instead of leaving the old button until the next timed refresh.
async function refreshOpenDetail() {
  if ($("job-detail").open && state.detail) await refreshDetail();
}
async function jobControl(id, name, args = {}, {onAccepted, decision} = {}) {
  if (jobIsBusy(id)) return;
  state.busyJobs.add(id);
  document.querySelectorAll("[data-job-control]").forEach((b) => {
    if (b.dataset.jobControl === id && b.dataset.navigation !== "true") b.disabled = true;
  });
  try {
    await tool(name, { id, ...args }, onAccepted, decision);
    await Promise.all([refreshOpenDetail(), loadJobs()]);
  } finally {
    state.busyJobs.delete(id);
    document.querySelectorAll("[data-job-control]").forEach((b) => {
      if (b.dataset.jobControl === id) b.disabled = jobControlDisabled(b);
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
      content.push(node("p", "Accept this candidate despite the validation result. Replacing the library file is then reviewed once more, with the exact paths and sizes, before any file changes.", "muted"));
    } else content.push(node("p", shortJobReason(job.waiting_reason) || "Review this job before continuing."));
    const candidateChoices = decision === "accept_loss" ? [
      ...(job.waiting_options.some(o=>o.decision === "reject") ? [{label:"Reject · keep original",value:"reject"}] : []),
      ...(candidatePlan ? [{label:"Transcode with other settings",value:"reconfigure"}] : []),
      ...(state.info.allow_destructive ? [{label:"Accept only · decide replacement later",value:"accept_only"}] : []),
    ] : [];
    const replaceNext = decision === "accept_loss" && state.info.allow_destructive;
    const chosen = await reviewAction({title:decision === "accept_loss" ? "Review candidate" : "Review decision", content, choices:candidateChoices, cancelLabel:decision === "accept_loss" ? "Decide later" : "Back", confirmLabel:decision === "accept_loss" ? (replaceNext ? "Accept and replace" : "Accept candidate") : "Approve"});
    if (!chosen) return;
    if (chosen === "reconfigure") { await reconfigureBatch(id,true,candidatePlan); return; }
    const latest = await read();
    if (auth !== state.authRevision || $("workspace").hidden) return;
    if (decisionStamp(latest || {}, decision) !== stamp)
      throw new Error("The candidate or decision changed. Review the job again.");
    const selected = chosen === "reject" ? "reject" : decision;
    // Accepting and replacing are two decisions, but one flow: the replacement
    // review opens by itself once the accepted candidate is ready.
    const replaceAfter = replaceNext && chosen === true;
    if (candidatePlan) {
      await commandRequest("candidate-decision",{id,candidate_id:candidatePlan.candidate_id,decision_version:candidatePlan.decision_version,decision:selected,background:true});
      await Promise.all([loadJobs(), refreshOpenDetail()]);
      notify(selected === "reject" ? "Candidate rejected. Original preserved; its candidate file is removed when nothing else uses it. Other batch files continue." : replaceAfter ? "Candidate accepted. Preparing the replacement review…" : "Candidate accepted. Replacement requires separate approval.");
      if (replaceAfter) safe(() => continueToReplacement(candidatePlan.candidate_id, id));
    } else {
      await jobControl(id, "action_resume", {decision:selected});
      if (replaceAfter) {
        notify("Candidate accepted. Preparing the replacement review…");
        safe(() => continueToReplacement(id));
      }
    }
  } finally {
    state.reviewingDecision = null;
  }
}
// After "Accept and replace" the candidate is finalized and published by the
// worker; wait for it, then go straight to the one replacement review.
async function continueToReplacement(candidateId, parentId) {
  const auth = state.authRevision;
  for (let attempt = 0; attempt < 150; attempt++) {
    await pollDelay(2000);
    if (auth !== state.authRevision || $("workspace").hidden) return;
    let job;
    try {
      job = (await api(`operations?id=${encodeURIComponent(candidateId)}`)).jobs?.[0];
    } catch {
      continue;
    }
    if (job?.candidate_ready && !job.replacement_action_id) {
      if (parentId) await replaceBatchFile(parentId, candidateId);
      else await prepareReplacement(job);
      return;
    }
    if (!job || job.replacement_action_id || ["failed", "cancelled"].includes(job.status)) break;
  }
  notify("Candidate accepted. Use Replace file when it is ready.");
}
async function prepareReplacement(job) {
  if (jobIsBusy(job.id)) return;
  if (job.replacement_action_id) {
    await openReplacement(job.replacement_action_id);
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
      await openReplacement(latest.replacement_action_id);
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
    // One tap replaces the file: the plan is approved as soon as it is read
    // back, without a second dialog or a second button.
    replaceWhenPlanned(result.id).catch((error) => {
      if (serverReachable) notify(error.message);
    });
    // The open details (this file's, or its batch's) list the file as replacing
    // right away, not on the next refresh, so Replace is not offered again.
    if ([job.id, latest.parent_action_id].includes(state.detail)) await refreshOpenDetail();
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
      [batch.queued_on_worker, "waiting for a worker"],
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
  if (["running", "Running", "waiting_external", "In progress", "Encoding", "Measuring quality", "Preparing", "Verifying", "Replacing", "Replacement in progress", "Saving candidate", "Verifying quality"].includes(status)) return "running";
  if (["failed", "Failed", "Worker offline"].includes(status)) return "failed";
  // Missing telemetry is worth attention, but nothing has failed: the worker
  // keeps the job and the original is untouched.
  if (["review", "waiting_decision", "Needs review", "Needs decision", "Needs attention", "partial", "Partial", "Cancelling", "No heartbeat", "No updates"].includes(status)) return "waiting_decision";
  if (["Preview", "Preview complete", "Eligible"].includes(status)) return "preview";
  return "neutral";
}
// A job's title is a real link to its details: it opens in the dialog, or in a
// new tab with the same job open.
function jobLink(text, id) {
  const link = node("a", text, "job-title");
  // The title is cut at two lines; the whole name is one long-press away.
  link.title = text;
  link.href = `?view=queue&job=${encodeURIComponent(id)}`;
  link.addEventListener("click", (e) => {
    if (e?.defaultPrevented || e?.button > 0 || e?.metaKey || e?.ctrlKey || e?.shiftKey || e?.altKey) return;
    e?.preventDefault?.();
    safe(() => openJob(id));
  });
  return link;
}
function statusBadge(label, status = label, onClick = null) {
  const className = `badge status-badge ${statusClass(status)}`;
  const result = onClick ? button("", onClick, `${className} status-action`) : node("span", "", className);
  if (onClick) result.setAttribute("aria-label", `${label} · View options`);
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
    const itemKey = savedPlan?.item_key;
    let plan = await api(`batch-settings?${new URLSearchParams({id,scope:itemKey ? "file" : candidateID ? "candidate" : "unfinished",...(candidateID ? {candidate_id:candidateID} : {}),...(itemKey ? {item_key:itemKey} : {})})}`);
    if (!candidateID && !itemKey && plan.selected === 0) plan = await api(`batch-settings?${new URLSearchParams({id,scope:"all"})}`);
    if (auth !== state.authRevision || revision !== state.reconfigureRevision || !$("reconfigure-batch").open) return;
    state.reconfigurePlan = plan;
    $("reconfigure-mode-label").hidden = !plan.mode;
    $("reconfigure-mode").value = plan.mode || "quality";
    // A batch that never said anything about the media engine gets the default: on, 15 %.
    $("reconfigure-hardware").checked = plan.settings.allow_hardware !== false;
    showSizeBudget("reconfigure-budget", plan.settings);
    updateReconfigureHardware();
    $("reconfigure-profile").closest("label").hidden = Boolean(plan.mode);
    $("reconfigure-limits").hidden = Boolean(plan.mode);

    $("reconfigure-profile-info").hidden = true;
    $("reconfigure-note").textContent = savedPlan?.ready_candidate ? "Same task · Original kept · Its ready candidate is removed" : "Same task · Originals kept · Replaced candidates removed";
    $("reconfigure-title").textContent = plan.mode ? "Change goal" : plan.requires_explicit_profile ? "Try another profile" : "Change settings";
    $("reconfigure-summary").textContent = batchSettingsSummary(plan);
    $("reconfigure-scope").replaceChildren();
    if (plan.candidate_id) option($("reconfigure-scope"),"candidate","This file");
    if (plan.item_key) option($("reconfigure-scope"),"file","This file");
    option($("reconfigure-scope"),"unfinished","Unfinished files");
    option($("reconfigure-scope"),"all","All unreplaced files in this batch");
    $("reconfigure-scope").value = plan.scope;
    for (const file of plan.files) $("reconfigure-files").append(node("p",file));
    if (plan.selected > plan.files.length) $("reconfigure-files").append(node("p",`+${plan.selected-plan.files.length} more files`));
    $("reconfigure-profile").replaceChildren();
    option($("reconfigure-profile"), "", "Choose a profile…");
    option($("reconfigure-profile"), "same", "Keep current settings");
    for (const profile of state.recipes) if (profile !== "auto" && !profileSearches(profile)) option($("reconfigure-profile"),profile,profileOptionLabel(profile));
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

// The request that sends one file back through the goal it already has: what
// the dialog would submit if nothing were changed.
function sameSettingsRequest(plan) {
  const s = plan.settings || {};
  const body = {id:plan.id, scope:"file", item_key:plan.item_key, selection_version:plan.selection_version, decision_version:plan.decision_version, preserve_limits:true, preserve_savings:true, preserve_growth:true};
  if (!plan.mode) return {...body, profile:"same"};
  body.mode = plan.mode;
  if (plan.mode !== "size") return body;
  body.allow_hardware = s.allow_hardware !== false;
  if (s.hardware_margin_percent > 0) body.hardware_margin_percent = s.hardware_margin_percent;
  if (s.size_budget_percent > 0) body.size_budget_percent = s.size_budget_percent;
  else if (s.size_budget_bytes > 0) body.size_budget_bytes = s.size_budget_bytes;
  else if (s.size_budget_source === "siblings") Object.assign(body, {size_budget_source:"siblings", size_budget_sibling_multiplier:s.size_budget_sibling_multiplier || 1.5});
  return body;
}
async function encodeFileAgain(id, itemKey) {
  if (jobIsBusy(id)) return;
  const auth = state.authRevision;
  state.busyJobs.add(id);
  try {
    const plan = await api(`batch-settings?${new URLSearchParams({id, scope:"file", item_key:itemKey})}`);
    if (auth !== state.authRevision) return;
    // A file that automatic testing skipped has no profile to keep.
    if (plan.requires_explicit_profile) { state.busyJobs.delete(id); await reconfigureBatch(id, false, {item_key:itemKey}); return; }
    await commandRequest("batch-settings", sameSettingsRequest(plan));
    if (auth !== state.authRevision) return;
    await Promise.all([loadJobs(), refreshOpenDetail()]);
    notify("Encoding this file again with the same settings.");
  } finally {
    state.busyJobs.delete(id);
    setConnection(serverReachable);
  }
}
function updateReconfigureHardware() {
  $("reconfigure-hardware-option").hidden = !(state.reconfigurePlan?.mode && $("reconfigure-mode").value === "size");
  $("reconfigure-budget-option").hidden = $("reconfigure-hardware-option").hidden;
  updateSizeBudgetFields("reconfigure-budget");
  renderSegmented(["reconfigure-budget"]);
}
$("reconfigure-mode").addEventListener("change", updateReconfigureHardware);
$("reconfigure-hardware").addEventListener("change", updateReconfigureHardware);
$("reconfigure-budget-kind").addEventListener("change", updateReconfigureHardware);
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
  if (plan.mode) {
    $("reconfigure-profile-note").hidden = $("reconfigure-current-profiles").hidden = $("reconfigure-profile-info").hidden = true;
    $("reconfigure-settings").textContent = `Automatic will test and verify settings for this goal. Originals stay in place.${plan.settings?.hardware_mixed ? " The selected files were frozen with different media engine choices; the choice above applies to all of them." : ""}${plan.settings?.size_budget_mixed ? " They were also frozen with different size budgets; the budget above applies to all of them." : ""}`;
    $("submit-reconfigure").disabled = !serverReachable || jobIsBusy(plan.id) || plan.selected === 0;
    $("submit-reconfigure").textContent = "Process with this goal";
    return;
  }
  $("reconfigure-current-profiles").hidden = false;
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
    const next=await api(`batch-settings?${new URLSearchParams({id:plan.id,scope:$("reconfigure-scope").value,...(plan.candidate_id ? {candidate_id:plan.candidate_id} : {}),...(plan.item_key ? {item_key:plan.item_key} : {})})}`);
    if (revision!==state.reconfigureScopeRevision || state.reconfigurePlan?.id!==plan.id || !$("reconfigure-batch").open) return;
    state.reconfigurePlan={...next,candidate_id:plan.candidate_id,item_key:plan.item_key,decision_version:next.decision_version ?? plan.decision_version};
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
  if (!plan.mode && (!$("reconfigure-profile").value || (plan.requires_explicit_profile && $("reconfigure-profile").value === "same"))) throw new Error("Choose a profile to try again.");
  const limit = id => {
    if (!$(id).value.trim()) return null;
    const value = Number($(id).value);
    if (!Number.isFinite(value)) throw new Error("Enter a valid savings or growth limit.");
    return value;
  };
  const body = {id:plan.id,scope:plan.scope,selection_version:plan.selection_version,...(plan.mode ? {mode:$("reconfigure-mode").value} : {profile:$("reconfigure-profile").value,min_savings_percent:limit("reconfigure-savings"),max_size_increase_percent:limit("reconfigure-growth")})};
  if (plan.mode && body.mode === "size") {
    body.allow_hardware = $("reconfigure-hardware").checked;
    Object.assign(body, sizeBudgetInputs("reconfigure-budget"));
  }
  body.preserve_limits = state.reconfigureOriginalLimits === batchSettingsLimits();
  let savedLimits;
  try { savedLimits = JSON.parse(state.reconfigureOriginalLimits); } catch {}
  body.preserve_savings = savedLimits?.[0] === String($("reconfigure-savings").value);
  body.preserve_growth = savedLimits?.[1] === String($("reconfigure-growth").value);
  if (plan.candidate_id) { body.candidate_id = plan.candidate_id; body.decision_version = plan.decision_version; }
  if (plan.scope === "file") { body.item_key = plan.item_key; body.decision_version = plan.decision_version; }
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
    await Promise.all([loadJobs(), refreshOpenDetail()]);
    notify("Settings updated in this batch. Active files finish first; replaced candidates are removed and their attempts stay in file history.");
  } finally {
    state.busyJobs.delete(plan.id);
    if (current()) updateBatchSettingsProfile();
    setConnection(serverReachable);
  }
}
$("reconfigure-form").addEventListener("submit", event => { event.preventDefault(); safe(submitReconfiguredBatch); });
// Temporary files an unfinished encode left beside its original. Best effort:
// a failed read must never block archiving or the notice after a cancel.
async function jobLeftovers(id) {
  try {
    const found = await api(`job-leftovers?id=${encodeURIComponent(id)}`);
    return found?.files > 0 ? found : null;
  } catch (error) {
    return null;
  }
}
function leftoverSummary(found) {
  return `${found.files} temporary ${found.files === 1 ? "file" : "files"} (${bytes(found.bytes)})`;
}
async function removeJobLeftovers(job, found) {
  const result = await api("job-leftovers", {id:job.id});
  if (!result.kept) return notify(`Removed ${result.removed} temporary ${result.removed === 1 ? "file" : "files"}, freeing ${bytes(result.freed_bytes)}.`);
  notify(`Removed ${result.removed} of ${found.files} temporary files. ${result.kept} could not be removed${result.error ? `: ${result.error}` : "."}`);
}
// After a cancel the encode's partial output stays on the share until someone
// removes it; say how much, and where it goes away.
async function noteJobLeftovers(job) {
  const found = await jobLeftovers(job.id);
  if (!found) return;
  notify(`Stopped. ${leftoverSummary(found)} from the encode ${found.files === 1 ? "was" : "were"} left beside the original. Archive the job to delete ${found.files === 1 ? "it" : "them"}.`);
}
async function archiveJob(job, archived) {
  if (jobIsBusy(job.id)) return;
  const auth = state.authRevision;
  state.busyJobs.add(job.id);
  try {
    let found = null;
    if (archived && state.info?.allow_destructive) {
      found = await jobLeftovers(job.id);
      if (auth !== state.authRevision || $("workspace").hidden) return;
    }
    if (found?.can_delete) {
      const chosen = await reviewAction({
        title: "Archive this job?",
        content: [
          node("p", queuePresentation(job).title, "review-filename"),
          node("p", `Its encode left ${leftoverSummary(found)} beside the original. Originals and candidates that can still replace them are never touched.`),
        ],
        confirmLabel: "Archive and delete temporary files",
        cancelLabel: "Back",
        choices: [{label: "Archive only · keep temporary files", value: "keep"}],
      });
      if (!chosen) return;
      if (chosen !== "keep") {
        await removeJobLeftovers(job, found);
        if (auth !== state.authRevision || $("workspace").hidden) return;
      }
    }
    await api("archive", {id:job.id,archived});
    if (auth !== state.authRevision || $("workspace").hidden) return;
    if (state.detail === job.id) $("job-detail").close();
    notify(archived ? "Job archived. Find it under Archived to restore it." : "Job restored to the queue.");
    await loadJobs();
  } finally {
    state.busyJobs.delete(job.id);
    setConnection(serverReachable);
  }
}
function confirmDecision(job, choice, label) {
  const content = [node("p", queuePresentation(job).title, "review-filename")];
  if (job.waiting_reason) content.push(node("p", shortJobReason(job.waiting_reason)));
  if (choice.description) content.push(node("p", choice.description, "muted"));
  return reviewAction({title: `${label}?`, content, confirmLabel: label, cancelLabel: "Decide later"});
}
const rerunGoals = goalNames;
// Another round on a file is the user's decision: confirming is that decision,
// and each confirmed attempt starts a new action under a fresh key.
// A skipped file whose goal cannot handle its source starts a NEW job under the
// goal the server named. The server refuses any other goal.
const remedyActions = {
  compact: {label: "Convert to SDR 1080p", title: "Convert this file to SDR 1080p?", line: "Converts to 1080p SDR and removes HDR. At least 30% smaller."},
  size: {label: "Make it smaller", title: "Make this file smaller?", line: "Keeps the look, at least 15% smaller."},
};
async function rerunJob(job, {withoutHardware = false, mode = ""} = {}) {
  const info = job.rerun || {};
  const content = [node("p", queuePresentation(job).title, "review-filename")];
  const remedy = mode ? remedyActions[mode] : null;
  const goal = rerunGoals[info.mode];
  if (remedy) content.push(node("p", remedy.line));
  else content.push(node("p", goal ? `Goal: ${goal}` : "The same settings as before."));
  if (withoutHardware) content.push(node("p", "The media engine is turned off for this job, so it encodes with x265: slower, but it usually keeps more quality at the same size."));
  if (!remedy) content.push(node("p", withoutHardware ? "This starts a new job with the same settings otherwise. The earlier job is not changed." : "This starts a new job with the same settings. The earlier job is not changed.", "muted"));
  if (!remedy && info.no_winner_rounds >= 2) content.push(node("p", `This file already had ${info.no_winner_rounds} rounds without a result. Run it again only if you want another round.`));
  if (remedy) content.push(node("p", info.replace_original ? "The original is replaced only after you approve the result." : "Originals stay in place.", "muted"));
  else content.push(node("p", info.replace_original ? "This job was set to replace the original: if it finds a result, the original is replaced only after you review and approve it." : "Originals stay in place.", "muted"));
  // One key per open confirmation: after a lost response, clicking again replays
  // the same key instead of creating a second job. Declining or success ends it.
  state.rerunKeys ||= new Map();
  const keyID = withoutHardware ? `${job.id}:x265` : mode ? `${job.id}:${mode}` : job.id;
  if (!state.rerunKeys.has(keyID)) state.rerunKeys.set(keyID, submissionID());
  if (!(await reviewAction({title: remedy ? remedy.title : withoutHardware ? "Try this file with x265?" : "Run this file again?", content, confirmLabel: remedy ? remedy.label : withoutHardware ? "Try with x265" : "Run again", cancelLabel: "Not now"}))) {
    state.rerunKeys.delete(keyID);
    return;
  }
  if (jobIsBusy(job.id)) return;
  state.busyJobs.add(job.id);
  try {
    const body = {id: job.id, key: state.rerunKeys.get(keyID)};
    if (withoutHardware) body.without_hardware = true;
    if (mode) body.mode = mode;
    const result = await api("rerun", body);
    state.rerunKeys.delete(keyID);
    state.jobsLoaded = 0;
    notify("Queued a new job. It is listed first.");
    await loadJobs();
    if (result?.id) await openJob(result.id);
  } finally {
    state.busyJobs.delete(job.id);
  }
}
// A job's own goal, repeated for the same file: the encoder and the size limit
// go into the form's controls, and what the form has no control for (the Shrink
// height, the slow preset) is carried to the submit for this file and goal.
function applyGoalSettings(path, goal) {
  $("allow-hardware").checked = goal.allow_hardware !== false;
  showSizeBudget("size-budget", goal);
  const inputs = {};
  for (const key of ["max_height", "allow_slow_preset"]) if (goal[key] != null) inputs[key] = goal[key];
  state.goalCarry = {path, mode: goal.mode, inputs};
  renderShrinkTitle();
}
// Drops what applyGoalSettings carried, so a later ordinary pick of the same
// file adds no hidden settings.
function clearGoalCarry() {
  state.goalCarry = null;
  renderShrinkTitle();
}
// The Shrink card names the height the form will send: a carried 720 is not
// "Shrink to 1080p SDR".
function renderShrinkTitle() {
  const carry = state.goalCarry;
  const height = carry?.mode === "compact" && carry.path === $("path").value.trim() ? carry.inputs.max_height : null;
  $("goal-title-compact").textContent = `Shrink to ${height || 1080}p SDR`;
}
// The series or movie the earlier job was made for, so the new job is listed
// and replaced the same way.
function applyLibraryContext(context, goal) {
  const id = context?.[context?.service === "radarr" ? "movie_id" : "series_id"];
  if (!["sonarr", "radarr"].includes(context?.service) || !Number.isInteger(id) || id <= 0) return;
  state.fileService = context.service;
  state.fileMedia = {id, seriesType: goal?.is_anime ? "anime" : "", genres: []};
}
// openGoalForm opens the library's goal form for one file, closing the job
// that asked for it. options.mode, options.goal, options.context and
// options.languages start the form on what an earlier job used (its goal and
// size limit, its library and its languages); options.pickTracks opens the
// track list.
function openGoalForm(path, options = {}) {
  if ($("job-detail").open) $("job-detail").close();
  $("path").value = path || "";
  $("scope").value = "file";
  state.fileMedia = null;
  state.fileService = null;
  clearGoalCarry();
  if (options.mode) setProcessingMode(options.mode);
  if (options.goal) applyGoalSettings(path, options.goal);
  applyLibraryContext(options.context, options.goal);
  if (options.languages) presetKeepLanguages(options.languages);
  selectTab("library");
  setFileStep("configure");
  controls();
  // Opening a new file clears an explicit goal; this one was asked for.
  if (options.mode) state.goalExplicit = true;
  if (options.pickTracks) $("keep-tracks").open = true;
}
// The Files location of a file Navigatorr was asked to convert: its media
// folder, its own folder and the file itself, ticked and in view.
// null when the file is outside every folder the Files tab can browse.
function filesRouteFor(path, {folder: isFolder = false} = {}) {
  if (typeof path !== "string" || !path.startsWith("/")) return null;
  const root = (state.info?.roots || [])
    .filter(r => path === r.replace(/\/+$/, "") || path.startsWith(`${r.replace(/\/+$/, "")}/`))
    .sort((a, b) => b.length - a.length)[0];
  if (!root) return null;
  // A folder opens as it is; a file opens its own folder, on the file.
  if (isFolder) return new URLSearchParams({view: "files", source: `folder:${root}`, folder: path}).toString();
  const folder = path.slice(0, path.lastIndexOf("/")) || root;
  // locate: show the file where it lives; its encoding form stays closed.
  return new URLSearchParams({view: "files", source: `folder:${root}`, folder, file: path, locate: "1"}).toString();
}
// The file a job or batch row is about, when it is one file.
function jobFilePath(job) {
  if (job.source_path) return job.source_path;
  if (job.promotion?.original_path) return job.promotion.original_path;
  return job.batch_files?.file_count === 1 ? job.batch_files.files?.[0]?.file_path || "" : "";
}
// The folder holding a batch's files, for a batch of more than one.
function jobFolderPath(job) {
  return job.batch_files?.file_count > 1 && typeof job.batch_files.folder === "string" ? job.batch_files.folder : "";
}
// Show the file in Files where it lives, to try again from there. Closes the
// job that asked for it; Back returns to the queue.
function showInFiles(path, options) {
  const query = filesRouteFor(path, options);
  if (!query) {
    notify(`This ${options?.folder ? "folder" : "file"} is outside the media folders Navigatorr can browse.`);
    return;
  }
  if (typeof window !== "undefined" && window.history) window.history.pushState({navigatorr: true}, "", `${window.location.pathname}?${query}`);
  return applyNavigation(navigationRoute(`?${query}`));
}
const navigationLabels = ["View replacement", "View batch", "View preview", "Review preview", "Review", "View file", "Show in Files"];
function jobControls(job, detail = false) {
  const actionRail = node("div", "", "job-actions");
  const add = (label, fn) => {
    const b = button(label, fn);
    if (label === "Start batch" || Object.values(remedyActions).some(r => r.label === label)) b.className = "primary";
    // Every job action carries an icon, so rows and the detail dialog match.
    const icon = actionIcon(label) || actionIcon("Action");
    if (icon) b.append(icon);
    b.dataset.jobControl = job.id;
    b.dataset.requiresWorker = String(
      (label === "Retry" && ["transcode_media", "transcode_batch", "benchmark_transcode"].includes(job.action_name) && !(job.action_name === "transcode_batch" && job.current_step >= 2)) ||
      label === "Run again" ||
      label === "Try again" ||
      label === "Try with x265" ||
      Object.values(remedyActions).some(r => r.label === label) ||
      label === "Resume batch" ||
      label === "Start batch" ||
      (job.action_name === "transcode_batch" && !job.batch?.promotion_plan_ready &&
        ["Review candidate", "Review replacements", "Keep originals"].includes(label))
    );
    if (navigationLabels.includes(label)) b.dataset.navigation = "true";
    b.disabled = jobControlDisabled(b);
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
    cancel: job.batch ? "Stop batch · keep originals" : "Keep original",
    recheck: "Try again",
    reconcile: "Check library again",
  };
  for (const choice of job.waiting_options || []) {
    // Candidate rejection is offered in the review with its frozen file ID and
    // decision version; the parent queue row must not reject an unseen file.
    if (choice.decision === "reject" && job.waiting_options.some(option=>option.decision === "accept_loss")) continue;
    const label = decisionLabels[choice.decision] || choice.decision.replace(/_/g, " ").replace(/^./, c => c.toUpperCase());
    add(
      label,
      async () => {
        if (choice.decision === "cancel") {
          if (await reviewAction({title:job.batch ? "Stop this batch?" : "Keep this original?",message:job.batch ? "Stop the remaining work. Originals and completed candidates are kept." : "Stop this file’s operation and keep its original.",confirmLabel:decisionLabels.cancel,cancelLabel:"Decide later"}))
            { await jobControl(job.id, "action_resume", {decision:"cancel"}); await noteJobLeftovers(job); }
          return;
        }
        if (choice.decision === "reject" && job.batch?.promotion_plan_ready) {
          await keepPreparedOriginals(job.id);
          return;
        }
        if (choice.decision === "approve" && job.action_name === "transcode_batch") {
          // A batch with a replacement plan is approved only against that
          // plan. Without one this is a forwarded per-file decision.
          if (job.batch?.promotion_plan_ready) {
            await reviewBatchReplacement(job.id);
            return;
          }
        }
        if (choice.decision === "approve" && job.promotion) {
          await reviewFilePromotion(job.id);
          return;
        }
        if (["approve", "accept_loss"].includes(choice.decision)) {
          await reviewJobDecision(job.id, choice.decision);
          return;
        }
        // Pausing, resuming and retrying are reversible; every other decision is
        // confirmed with the server's own explanation before it is sent.
        if (!["resume", "pause", "retry", "recheck"].includes(choice.decision) && !(await confirmDecision(job, choice, label)))
          return;
        await jobControl(job.id, "action_resume", {
          decision: choice.decision,
        });
      },
    );
    actionRail.children[actionRail.children.length - 1].title =
      choice.description || label;
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
    add("Review candidates", () => reviewBatchReplacement(job.id));
  const reconfigurable = job.action_name === "transcode_batch" && !job.batch?.dry_run && !job.batch?.promotion_plan_ready;
  // An encode that finished but could not be saved is tried again as it is;
  // a new goal would encode the file again.
  const savingFailed = job.waiting_options?.some(option => option.decision === "recheck");
  if (job.parent_item_key && job.parent_action_id && !job.replacement_plan && !job.replacement_action_id && !savingFailed && ["completed","failed","cancelled","waiting_decision"].includes(job.status) && !(job.mode && sourceGateReason(job.reason_code)))
    add(job.mode ? "Change goal" : "Change settings", () => reconfigureBatch(job.parent_action_id, false, {item_key:job.parent_item_key, ready_candidate:Boolean(job.candidate_ready)}));
  if (reconfigurable && !(job.mode && onlyUnsupportedSources(job))) add(job.mode ? "Change goal" : job.batch?.outcome === "no_changes" ? "Try another profile" : "Change settings", () => reconfigureBatch(job.id));
  if (job.status === "failed" && !reconfigurable && !job.parent_item_key && !declinedReplacement(job)) {
    if (rejectedCandidate(job)) {
      if (job.parent_action_id) add("Change settings", () => reconfigureBatch(job.parent_action_id, true, {candidate_id:job.id}));
    } else if (job.action_name === "benchmark_transcode")
      add("Choose a goal", () => openGoalForm(job.source_path));
    // Failed replacements are retried per file below. The batch's own Retry stays
    // when nothing is retryable per file and only the batch recorded the failure,
    // so a transient error can still be retried from here.
    else if (!(job.replacement?.failed > 0) || (!(job.replacement.retryable > 0) && job.replacement.batch_recorded_failed > 0)) add("Retry", () => jobControl(job.id, "action_retry"));
  }
  // The server marks only standalone files that were cancelled or ended with no
  // winner; batch children use Change goal and reviewed candidates are decided.
  if (job.rerun && !job.rerun.remedy_mode && !job.parent_item_key && !job.candidate_ready && !job.replacement_action_id && job.status !== "waiting_decision" && !(job.mode && sourceGateReason(job.reason_code)))
    add("Run again", () => rerunJob(job));
  // A job whose settings ran the removed sample search cannot run again as it
  // was; the server says why, and the file's goal form is offered instead.
  if (job.rerun_refused && !job.parent_item_key && !job.candidate_ready && !job.replacement_action_id && job.status !== "waiting_decision") {
    add("Choose a goal", () => openGoalForm(job.source_path));
    const choose = [...actionRail.children].at(-1);
    if (choose) choose.title = job.rerun_refused;
  }
  // A file that missed its goal's size limit has no candidate to review and no
  // one goal the server can name, but its result tells the person to choose
  // another goal: the goal form is that step, next to Run again.
  if (job.status === "completed" && job.benchmark?.outcome === "size_rejected" && job.source_path && !job.rerun_refused && !job.parent_item_key && !job.candidate_ready && !job.replacement_action_id)
    add("Choose a goal", () => openGoalForm(job.source_path));
  // A file skipped because its goal is wrong for it: one button starts a new job
  // under the goal the server named. Running it again would skip it again.
  if (job.rerun?.remedy_mode && remedyActions[job.rerun.remedy_mode] && !job.parent_item_key && !job.candidate_ready && !job.replacement_action_id && job.status !== "waiting_decision")
    add(remedyActions[job.rerun.remedy_mode].label, () => rerunJob(job, {mode: job.rerun.remedy_mode}));
  // A media engine budget encode that fit but missed the quality floor offers no
  // candidate; x265 is slower and usually keeps more quality at the same size.
  if (job.rerun && (job.rerun.without_hardware || job.size_mode?.size_budget?.retry_without_hardware) && !job.parent_item_key && !job.candidate_ready && !job.replacement_action_id && job.status !== "waiting_decision")
    add("Try with x265", () => rerunJob(job, {withoutHardware: true}));
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
    ) && !job.waiting_options?.some(option=>option.decision === "cancel")
  )
    add("Cancel", async () => {
      if (await reviewAction({title:"Cancel job?", message:`Stop ${queuePresentation(job).title} and its active work.`, confirmLabel:"Cancel job", cancelLabel:"Keep job"}))
      {
        await jobControl(job.id, "action_cancel", {
          reason: "Cancelled from maintenance UI",
        });
        await noteJobLeftovers(job);
      }
    });
  if (
    job.action_name === "transcode_media" &&
    job.status === "completed" &&
    job.candidate_ready &&
    !job.replacement_action_id &&
    !job.replacement_plan &&
    state.info.allow_destructive
  )
    add("Replace file", () => prepareReplacement(job));
  if (job.replacement_action_id)
    add("View replacement", () => openJob(job.replacement_action_id));
  // Untagged tracks are kept without being asked; the finished file may have
  // kept the wrong ones, and the tag is missing, not the track. The lists come
  // with the one job only, so the queue rows never offer it. A job that was
  // skipped or not converted made no file to choose tracks for.
  if (detail && job.action_name === "transcode_media" && job.status === "completed" && job.candidate_ready && !job.replaced && !job.replacement_action_id && job.source_path && keepGoals.includes(job.mode) && keptUntagged(job.tracks))
    add("Choose tracks", () => openGoalForm(job.source_path, {mode: job.mode, goal: job.goal || {mode: job.mode}, context: job.replacement_context, languages: Array.isArray(job.tracks.languages) ? job.tracks.languages : undefined, pickTracks: true}));
  if (job.preview_action_id && detail)
    add("View preview", () => openJob(job.preview_action_id));
  // A job that ended without a result leaves its file where it was. Files
  // shows it there, with its status and the goal form, to try again from.
  // Every job offers it, whatever its state, so the source is one click away.
  // A file's job opened on its own (from a worker slot, a link) leads back to
  // the batch it belongs to.
  // parent_batch_id is only set for a batch the server found, and a job that
  // already offers its preview does not offer the same batch twice.
  if (detail && job.parent_batch_id && job.parent_batch_id !== job.id && job.parent_batch_id !== job.preview_action_id && job.parent_batch_id !== job.preview_execution_action_id)
    add("View batch", () => openJob(job.parent_batch_id));
  const filePath = jobFilePath(job), folderPath = filePath ? "" : jobFolderPath(job);
  if (filePath && filesRouteFor(filePath))
    add("Show in Files", () => showInFiles(filePath));
  else if (folderPath && filesRouteFor(folderPath, {folder: true}))
    add("Show in Files", () => showInFiles(folderPath, {folder: true}));
  if (job.can_archive && ["completed", "failed", "cancelled"].includes(job.status))
    add("Archive", () => archiveJob(job, true));
  return actionRail;
}

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
    quality_not_met: "The encoded result did not meet the quality target; original kept",
    insufficient_savings: "The encoded result did not save enough space; original kept",
    size_growth: "The encoded result was larger than allowed; original kept",
    size_growth_exceeded: "The encoded result was larger than allowed; original kept",
    preservation_not_met: "The encoded result did not preserve the required tracks or video properties; original kept",
    already_target_codec: "Already in the target codec; original kept",
    search_budget_exhausted: "Automatic testing reached its limit without finding a reliable result; original kept",
    no_suitable_candidate: "None of the tested settings met this goal; original kept",
    audio_compaction_insufficient: "No video setting met this goal, and compacting the lossless audio alone would not save enough; original kept",
    audio_estimate_unavailable: "No video setting met this goal, and the lossless audio size could not be read to estimate audio-only savings; original kept",
    audio_only_worker_unsupported: "No video setting met this goal; compacting only the audio needs an updated worker. Original kept",
    unsupported_source: "Automatic testing does not support this source format yet; original kept",
    hdr_requires_compact: "HDR or Dolby Vision source; only the Shrink to 1080p SDR goal converts it. Original kept",
    compact_not_needed: "Already SDR at or below the Shrink to 1080p SDR target; the Save space goal can shrink it. Original kept",
    hfr_quality_unavailable: "The worker cannot measure quality at this frame rate yet; original kept",
    compact_frame_evidence_unavailable: "Could not read this video's frames to plan the conversion (it may have timed out); original kept. Use Change goal to try again",
    quality_policy_unavailable: "The worker cannot run the quality checks this goal needs; original kept",
    quality_vmaf_model_unavailable: "The worker cannot measure quality for this video (VMAF model unavailable); original kept",
    size_budget_exceeded: "The encoded result was over the size budget; original kept",
    size_budget_unreachable: "The audio, subtitles and other streams leave no room for video within the size budget; original kept",
    size_budget_estimate_unavailable: "The size budget could not be planned for this file; original kept",
  };
  if (reasons[text]) return reasons[text];
  if (text.includes("candidate promotion rejected")) return "Replacement declined by you; original and converted file untouched";
  if (isCandidateRejection(text)) return ["Candidate rejected by you; original kept", candidateNoteInText(text)].filter(Boolean).join("; ");
  if (workerBusyFailure(text)) return workerBusyFailureText;
  const growth = text.match(/Candidate file size \(\d+ bytes\) exceeds original \(\d+ bytes\) by ([\d.]+)%, which is greater than max_size_increase_percent \(([\d.]+)%\)/);
  if (growth) return `Candidate is ${growth[1]}% larger; allowed growth is ${growth[2]}%`;
  if (/no such file or directory/i.test(text)) return "File not found";
  if (/ffprobe|mediainspect failed/i.test(text)) return "Couldn't read the video file";
  if (/permission denied/i.test(text)) return "Permission denied";
  // The encode finished and only saving it failed: say so before any
  // storage-error wording, since nothing needs to be encoded again.
  if (/^Encoding finished for job .*publication requires intervention/.test(text)) {
    const note = text.match(/ (The worker (?:has not been updated|could not be asked)[^]*)$/)?.[1];
    return `Encoding finished, but the file could not be saved. The encoded copy is kept. Use Try again once storage is reachable.${note ? ` ${note}` : ""}`;
  }
  if (/smb_transport_error|connecting to SMB server/.test(text)) return "Worker cannot access media storage. Fix the connection before retrying.";
  if (/shared VMAF\/CAMBI calibration supports 8-bit SDR video below 45 fps/.test(text))
    return "Automatic testing does not support this video format";
  // Engine reasons often start with a machine code ("code: detail").
  const coded = text.match(/^([a-z0-9]+(?:_[a-z0-9]+)+)(?::\s*(.*))?$/);
  if (coded) return reasons[coded[1]] || `${coded[1].replace(/_/g, " ").replace(/^./, c => c.toUpperCase())}${coded[2] ? `: ${coded[2]}` : ""}`;
  return text.length > 140 ? `${text.slice(0, 137)}…` : text;
}
// Jobs from before slot waits failed when the worker was full. The raw error
// names the worker URL and says nothing useful: nothing was sent to run.
const workerBusyFailureText = "The worker was full and nothing ran. Retry when a slot is free.";
function workerBusyFailure(reason) {
  return /\(worker_busy\)|worker busy|maximum parallel jobs/i.test(String(reason || ""));
}
function workerUnavailable(job) {
  return job.waiting_condition === "worker_unreachable" || job.activity_waiting_condition === "worker_unreachable";
}
// The worker beats every few seconds and Navigatorr polls it about every five,
// but a busy coordinator or a page resumed from the background can delay the
// next update by a minute or two. Only prolonged silence is worth a warning.
const heartbeatFreshMs = 30000, heartbeatLostMs = 180000, heartbeatSkewMs = 60000;
function workerHeartbeatState(value, now = Date.now()) {
  const at = Date.parse(value || "");
  if (!Number.isFinite(at) || at < Date.UTC(2000, 0, 1) || at > now + heartbeatSkewMs) return "unknown";
  const age = Math.max(0, now - at);
  return age <= heartbeatFreshMs ? "fresh" : age <= heartbeatLostMs ? "delayed" : "stale";
}
function heartbeatAlive(state) {
  return state === "fresh" || state === "delayed";
}
function workerProgressObservation(job) {
  const w = job.worker || {};
  const phase = w.transcode_phase || w.phase;
  const heartbeat = workerHeartbeatState(w.worker_heartbeat_at);
  const result = {active: false, heartbeat, status: "", message: "", warning: false};
  if (!["running", "waiting_external"].includes(job.status)) return result;
  if (workerUnavailable(job)) return {...result, warning: true, status: "Worker offline", message: "Worker unreachable. Retrying connection automatically."};
  if (w.recovery_required) return {...result, warning: true, status: "Needs decision", message: job.waiting_reason || "Candidate preserved; publication requires intervention."};
  const waiting = job.activity_waiting_condition || job.waiting_condition;
  // A batch reports its own slot counts; a single job just waits its turn.
  if (waiting === "worker_busy" && !job.batch) {
    const position = job.work?.phase === "waiting_for_worker" ? Number(job.work.queue_position) : 0;
    const line = Number.isInteger(position) && position > 1 ? ` ${position - 1} ${position === 2 ? "job is" : "jobs are"} ahead of it in line.` : position === 1 ? " It is next in line." : "";
    return {...result, status: "Waiting for a free worker slot", message: `The worker is running as many jobs as it allows. This job continues automatically when a slot is free.${line}`};
  }
  if (waiting === "worker_drain") return {...result, status: "Held for worker deploy", message: "Worker drain is on, so this job has not started. It continues when the drain is resumed."};
  if (["worker_busy", "parent_busy", "queued", "transcode_retry", "worker_reconciling"].includes(waiting))
    return {...result, status: "Waiting", message: job.waiting_reason || "Waiting for worker coordination."};
  // A finished worker can leave telemetry behind while the coordinator validates
  // or requests a decision. Its old heartbeat is not evidence of a live failure.
  const status = w.transcode_status;
  if (["completed", "failed", "cancelled"].includes(status) || ["completed", "failed", "cancelled"].includes(phase)) return result;
  if (phase === "queued" || status === "queued" || !job.worker) return result;
  result.active = true;
  if (heartbeat === "stale") {
    // Say who went quiet: a worker that stopped reporting, or a coordinator
    // that has not asked it recently. Without a poll time, blame nobody.
    const poll = workerHeartbeatState(w.last_worker_poll_at);
    const quiet = activityElapsed(w.worker_heartbeat_at);
    const span = quiet ? ` for ${quiet}` : " recently";
    const who = heartbeatAlive(poll) ? `The worker has not reported${span}.` : poll === "stale" ? `Navigatorr has not checked the worker${span}.` : `No worker heartbeat${span}.`;
    return {...result, warning: true, status: "No heartbeat", message: `${who}${phase ? ` Last reported phase: ${workerPhaseLabel(phase)}.` : ""}`};
  }
  // These phases report completed units, not a continuous encoder percentage.
  // A heartbeat proves liveness only; preserve the actual last measurement.
  const discreteProgress = ["preparing", "reading_source", "probing_source", "validating", "validating_quality", "analysing_video", "publishing"].includes(phase);
  if (w.progress_is_stale && !(discreteProgress && heartbeatAlive(heartbeat)))
    return {...result, warning: true, status: "No updates", message: `No recent ${phase === "encoding" ? "encoding progress" : "progress from the worker"}. ${heartbeatAlive(heartbeat) ? `Worker heartbeat ${activityElapsed(w.worker_heartbeat_at)} ago.` : "Worker heartbeat unavailable."}`};
  return result;
}
// The server appends what happened to a retired candidate's own file to the
// rejection error or the cancellation reason.
function candidateNoteInText(text) {
  return String(text || "").match(/; ((?:candidate file (?:removed|kept)|no candidate file was left).*)$/)?.[1] || "";
}
function candidateCleanupNote(job) {
  const cleanup = job.candidate_cleanup;
  if (!cleanup) return "";
  if (cleanup.outcome === "removed") return cleanup.bytes_freed > 0 ? `Candidate file removed (${bytes(cleanup.bytes_freed)})` : "No candidate file was left";
  return `Candidate file kept: ${cleanup.detail || "its cleanup could not be confirmed"}`;
}
function isCandidateRejection(reason) {
  return String(reason || "").includes("transcode candidate rejected by user decision");
}
// Declining the replacement review is a choice, not a failure: nothing was
// touched and the converted file is still there.
function declinedReplacement(job) {
  return job.status === "failed" && String(job.error || "").includes("candidate promotion rejected");
}
function rejectedCandidate(job) {
  return job.status === "failed" && isCandidateRejection(job.error);
}
function savingsPolicyFailure(value) {
  return /benchmark winner predicts only -?\d+(?:\.\d+)?% savings, below required minimum \d+(?:\.\d+)?%|Full conversion not started; original kept/i.test(value || "");
}
// A conversion that finished and left nothing to review, replace or undo: no
// candidate file, no replacement, no retired candidate. This is a fact about
// the job, not a list of reason codes, so a reason the UI has never seen still
// reads as "Not converted" instead of a green "Completed".
function notConverted(job) {
  return job.action_name === "transcode_media" && job.status === "completed" && !job.candidate_ready &&
    !job.replaced && !job.replacement_action_id && !job.candidate_cleanup && job.savings?.realized_saved_bytes == null;
}
// Why a not-converted job left its original alone: the server's own sentence
// (the numbers are in it), else the curated line for its code, else the code
// itself in words. Never empty.
function notConvertedReason(job) {
  const message = String(job.benchmark?.message || "").replace(/\s+/g, " ").trim();
  if (message) {
    // A sentence ends at ". " before a capital, so "approx. 5 GB" stays whole.
    let sentence = (message.split(/(?<=[.!?])\s+(?=[A-Z])/)[0] || message).replace(/[.!?]+$/, "");
    const suffix = "; original kept", limit = 160;
    if (sentence.length + suffix.length > limit) {
      // Cut at a clause or word boundary, never inside a number, and keep the
      // suffix: the user must still learn that the original is untouched.
      const room = sentence.slice(0, limit - suffix.length - 1);
      const clause = Math.max(room.lastIndexOf(";"), room.lastIndexOf(","));
      const cut = clause > room.length / 2 ? clause : room.lastIndexOf(" ");
      sentence = `${room.slice(0, cut > 0 ? cut : room.length)}…`;
    }
    return /original (?:preserved|kept)/i.test(sentence) ? sentence : sentence + suffix;
  }
  return shortJobReason(job.reason_code) || "No converted file was made; original kept";
}
function visibleBatch(job) {
  if (!job.batch) return null;
  const rejected = Math.min(job.batch.failed || 0, job.batch_files?.rejected_count ?? (job.batch_files?.reasons || []).filter(reason=>isCandidateRejection(reason.reason)).reduce((count,reason)=>count+reason.count,0));
  const policy = Math.min(Math.max(0,(job.batch.failed || 0)-rejected),job.batch_files?.policy_count || 0);
  const failed = (job.batch.failed || 0)-rejected-policy;
  // Files in a worker's line are counted by the server as running; only the ones encoding are.
  const queuedOnWorker = Math.min(job.batch.queued_on_worker || 0, job.batch.running || 0);
  return {...job.batch,running:(job.batch.running || 0)-queuedOnWorker,failed,rejected,policy,outcome:rejected > 0 && rejected===job.batch.total && job.status === "completed" ? "rejected" : policy > 0 && policy===job.batch.total ? "minimum_savings" : policy > 0 && failed === 0 && job.batch.outcome === "failed" ? "no_changes" : job.batch.outcome};
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
  // A selection made across folders is called "Selected files"; the server names it from the files instead.
  const generic = !job.batch?.title || job.batch.title === "Selected files";
  const context = job.batch_files?.context || (generic && job.batch_files?.name) || job.batch?.title;
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
  const phase = job.worker?.transcode_phase || job.worker?.phase;
  // Every file handed to a worker is still in its line: nothing is encoding yet.
  const allQueued = job.activities?.length > 0 && job.activities.every(activity => (activity.worker?.transcode_phase || activity.worker?.phase) === "queued");
  const multipleActive = job.activities?.length > 1 && !allQueued;
  const offline = multipleActive
    ? job.activities.every(activity => activity.waiting_condition === "worker_unreachable")
    : workerUnavailable(job);
  const observation = workerProgressObservation(job);
  // Freed bytes appear with the first replaced file; a batch still working on
  // the rest is not "Replaced" yet.
  const replacementComplete = !operationIsActive(job.status) && (job.savings?.realized_saved_bytes != null ||
    (job.replaced && job.status === "completed"));
  // Nothing was made, so there are no savings to report.
  const leftNoFile = notConverted(job);
  const status = job.replacement?.approved && operationIsActive(job.status) ? "Replacing" : leftNoFile ? "Not converted" : job.status === "waiting_decision" && job.paused === true ? "Paused" : job.status === "waiting_decision" && job.reason_code === "search_budget_exhausted" ? "Needs attention" : job.status === "failed" && !rejectedCandidate(job) && !declinedReplacement(job) && !savingsPolicyFailure(job.error) ? "Failed" : job.replaced_files > 0 && job.remaining_candidates > 0 ? "Candidates ready" : replacementComplete
    ? "Replaced"
    : rejectedCandidate(job) ? "Rejected" : declinedReplacement(job) ? "Kept original" : savingsPolicyFailure(job.error) ? "Minimum savings not met" : outcomes[batch?.outcome] ||
      (job.candidate_ready && !job.replacement_action_id
        ? "Candidate ready"
        : ["running", "waiting_external"].includes(job.status)
          ? (offline ? "Worker offline" : multipleActive ? "In progress" : observation.status || {encoding:"Encoding",validating:"Verifying",validating_quality:"Verifying quality",analysing_video:"Analyzing video",publishing:"Saving candidate",queued:"Queued on worker",preparing:"Preparing"}[phase] || (replacement || job.promotion ? "Replacing" : "In progress"))
          : names[job.status] || job.status);
  const s = job.savings || {},
    metrics = [];
  if (s.source_bytes != null) {
    metrics.push(
      s.candidate_bytes != null && !job.replaced && !leftNoFile
        ? `${bytes(s.source_bytes)} → ${bytes(s.candidate_bytes)}`
        : `${bytes(s.source_bytes)} source`,
    );
  }
  if (s.realized_saved_bytes != null)
    metrics.push(`${bytes(s.realized_saved_bytes)} freed`);
  else if (!leftNoFile && !job.replaced && !rejectedCandidate(job) && batch?.outcome !== "rejected" && s.candidate_saved_bytes != null)
    metrics.push(
      s.candidate_saved_bytes < 0
        ? `${bytes(-s.candidate_saved_bytes)} larger`
        : `${bytes(s.candidate_saved_bytes)} potential savings`,
    );
  else if (!leftNoFile && !job.replaced && !rejectedCandidate(job) && batch?.outcome !== "rejected" && s.estimated_saved_bytes != null)
    metrics.push(s.estimated_saved_bytes < 0 ? `about ${bytes(-s.estimated_saved_bytes)} larger (estimate)` : `${bytes(s.estimated_saved_bytes)} estimated savings`);
  let result;
  const replacingHeadline = replacementHeadline(job);
  if (replacingHeadline) {
    result = replacingHeadline;
    metrics.length = 0;
  } else if (leftNoFile) result = notConvertedReason(job);
  else if (job.status === "failed")
    result = shortJobReason(job.error) || "Error details unavailable";
  else if (job.replaced && job.status === "completed") result = "Original replaced";
  else if (job.batch?.promotion_plan_ready)
    result = "Replacement approval required";
  else if (job.batch) result = batchCounts(batch, true);
  else if (job.status === "waiting_decision")
    result = job.promotion ? "Review replacement before files change" : "Your decision is needed";
  else if (job.status === "cancelled") result = "Stopped";
  if (job.workflow_actions?.length && replacement && job.status !== "failed") {
    result = `Conversion complete · ${job.savings?.realized_saved_bytes != null ? "Replacement verified" : result || "Replacement in progress"}`;
  }
  if (job.replaced_files > 0 && !replacingHeadline) result += ` · ${job.replaced_files} replaced${job.remaining_candidates > 0 ? ` · ${job.remaining_candidates} candidates available` : ""}`;
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
    kind,
    facts: metrics,
    showTelemetry:
      ["running", "waiting_external"].includes(job.status),
  };
}
// The next step a card offers, most important first. Only one is labelled.
// Shortened visible labels for main actions. Each is a prefix of the full
// label, which stays the accessible name (WCAG 2.5.3 Label in Name).
const shortActionLabels = {"Replace file":"Replace","Choose a goal":"Choose","Review candidates":"Review","Review candidate":"Review","Review replacement":"Review","Review replacements":"Review","Review preview":"Review","Validate classifications":"Validate","Check library again":"Check library","Resume batch":"Resume","Start batch":"Start","Try another profile":"Try another","Convert to SDR 1080p":"Convert to SDR"};
const mainQueueActions = ["Replace file", "Review replacements", "Review replacement", "Review candidates", "Review candidate", "Review cuts", "Validate classifications", "Review", "Resume batch", "Start batch", "Choose a goal", "Retry", "Convert to SDR 1080p", "Make it smaller", "Try again", "Check library again", "Review preview", "Try another profile"];
// Icon actions a row keeps in view when it cannot show them all, most useful first.
const keptQueueIcons = ["Pause batch", "Resume batch", "Cancel", "Archive", "Stop batch · keep originals"];
const visibleQueueIcons = 3;
const trayLabels = {"Pause batch":"Pause","Resume batch":"Resume","Stop batch · keep originals":"Stop","Show in Files":"Files","Change settings":"Settings","View batch":"Batch","View replacement":"Result","View preview":"Preview","View transcript":"Text","Choose tracks":"Tracks","Revise labels":"Labels","Ad library":"Ads","Try with x265":"x265","Run again":"Again"};
// The "..." menu: the same buttons, with their handlers and state, listed in a
// popup that closes on a choice and returns focus to its toggle.
function moreActionsMenu(items, menuFor, {toggleClass = "more-toggle", icon = null} = {}) {
  const more = node("details", "", "more-actions");
  more.dataset.menuFor = menuFor || "";
  const toggle = node("summary", icon ? "" : "⋯", toggleClass);
  toggle.setAttribute("aria-label", "More actions");
  toggle.title = "More actions";
  if (icon) toggle.append(icon);
  const menu = node("div", "", "more-menu");
  menu.append(...items);
  more.append(toggle, menu);
  // Capture runs before the item's own handler: the menu closes and focus
  // returns to its toggle, so a confirmation dialog hands focus back there.
  more.addEventListener("click", (e) => {
    if (!e.target?.closest?.(".more-menu button")) return;
    more.open = false;
    toggle.focus?.({ preventScroll: true });
  }, true);
  more.addEventListener("focusout", (e) => {
    if (e.relatedTarget && !more.contains(e.relatedTarget)) more.open = false;
  });
  return more;
}
function compactQueueActions(actions) {
  // A card offers at most one labelled main action, which the row shows as a
  // link at the end of its sentence. The other actions are icon buttons named
  // by aria-label and title: up to three stay in view, and when there are more
  // the least common go into one More actions menu (the same elements, with
  // their handlers and state, listed with icon and label).
  const buttons = [...actions.children];
  const labelOf = (b) => b.textContent.trim();
  const main = mainQueueActions.map((label) => buttons.find((b) => labelOf(b) === label)).find(Boolean);
  const icons = [];
  for (const b of buttons) {
    const label = labelOf(b);
    const icon = b.querySelector?.("svg") || null;
    b.setAttribute("aria-label", label);
    b.title ||= label;
    if (b === main) {
      b.replaceChildren(node("span", shortActionLabels[label] || label, "action-label"));
      b.className = "action-main";
      continue;
    }
    b.replaceChildren(...[icon, node("span", label, "action-label")].filter(Boolean));
    b.className = ["Cancel", "Stop batch · keep originals"].includes(label) ? "action-icon danger" : "action-icon";
    icons.push(b);
  }
  let overflow = [];
  // On a touch screen the actions wait behind a swipe, in a tray that has room
  // for all of them, so none goes into a menu.
  if (!swipeActions() && icons.length > visibleQueueIcons) {
    const rank = (b) => { const i = keptQueueIcons.indexOf(labelOf(b)); return i < 0 ? keptQueueIcons.length : i; };
    const kept = new Set([...icons].sort((a, b) => rank(a) - rank(b)).slice(0, visibleQueueIcons - 1));
    overflow = icons.filter((b) => !kept.has(b));
  }
  const shown = icons.filter((b) => !overflow.includes(b));
  // Each kind of button keeps its own place down the list: pause or resume
  // first, then what stops the job, then Files or More, and Archive always last,
  // at the right edge.
  const more = overflow.length ? moreActionsMenu(overflow, buttons[0]?.dataset.jobControl, {toggleClass: "action-icon more-toggle", icon: actionIcon("More actions")}) : null;
  const place = (b) => {
    const label = b === more ? "" : labelOf(b);
    if (["Pause batch", "Resume batch"].includes(label)) return 1;
    if (["Cancel", "Stop batch · keep originals"].includes(label)) return 2;
    return label === "Archive" ? 4 : 3;
  };
  const slotted = [...shown, ...(more ? [more] : [])].sort((x, y) => place(x) - place(y));
  let slot = 0;
  slotted.forEach((b, i) => {
    slot = Math.min(Math.max(Math.min(place(b), 3), slot + 1), 3 - (slotted.length - 1 - i));
    b.dataset.slot = String(slot);
  });
  // A tile in the swipe tray has room for one short word; the full label stays
  // its accessible name and its tooltip.
  if (swipeActions())
    for (const b of slotted) {
      const label = b.querySelector?.(".action-label"), full = b.getAttribute("aria-label");
      if (label && trayLabels[full]) label.textContent = trayLabels[full];
    }
  actions.replaceChildren(...[main, ...slotted].filter(Boolean));
}
// Touch screens keep a job's actions behind a swipe to the left, where a thumb
// reaches: the row slides over and a tray of actions waits at its right edge. A long
// swipe past most of the row archives the job without a second step.
const swipeQuery = "(hover: none) and (pointer: coarse)";
function swipeActions() {
  return typeof matchMedia === "function" && matchMedia(swipeQuery).matches;
}
const swipeSlop = 10, swipeArmed = 0.6, swipeSeenKey = "navigatorr_swipe_seen";
let swipe = null;
// Resistance past the tray, so a long drag still feels attached to the finger.
function swipeOffset(dx, tray) {
  return dx <= tray ? Math.max(dx, 0) : tray + (dx - tray) * 0.5;
}
// Where a released swipe settles: "archive" past most of the row, "open" past
// half the tray, otherwise shut. A row already open settles by the same rule.
function swipeSettle(dx, tray, width, canArchive) {
  if (canArchive && dx >= width * swipeArmed) return "archive";
  return dx >= tray / 2 ? "open" : "closed";
}
function swipeRow(target) {
  const row = target?.closest?.(".job-row.has-menu");
  return row && row.parentNode ? row : null;
}
function swipeTrayWidth(row) {
  const tray = row.querySelector?.(".job-actions");
  return tray ? tray.offsetWidth + 8 : 0;
}
function setSwipe(row, offset) {
  row.style.setProperty("--swipe", `${offset}px`);
}
function closeSwipe(except) {
  for (const row of document.querySelectorAll?.(".job-row.swipe-open, .job-row.swiping") || []) {
    if (row === except) continue;
    row.classList.remove("swipe-open", "swiping", "swipe-armed");
    row.style.removeProperty("--swipe");
  }
  if (!except || state.swipeOpenId !== except.dataset.operationId) state.swipeOpenId = null;
}
function markSwipeSeen() {
  try { localStorage.setItem(swipeSeenKey, "1"); } catch {}
  if ($("swipe-hint")) $("swipe-hint").hidden = true;
}
function swipeHintVisible() {
  if (!swipeActions()) return false;
  try { return localStorage.getItem(swipeSeenKey) !== "1"; } catch { return false; }
}
function swipeStart(e) {
  if (e.pointerType !== "touch" || !e.isPrimary) return;
  const row = swipeRow(e.target);
  if (!row) return;
  // A press on a row other than the open one shuts the open one.
  closeSwipe(row);
  swipe = {row, id: e.pointerId, x: e.clientX, y: e.clientY, open: row.classList.contains("swipe-open"), tray: swipeTrayWidth(row), width: row.offsetWidth, locked: false, dx: 0};
}
function swipeMove(e) {
  if (!swipe || e.pointerId !== swipe.id) return;
  // Leftward travel is positive: it is the way the tray opens.
  const dx = swipe.x - e.clientX, dy = e.clientY - swipe.y;
  if (!swipe.locked) {
    if (Math.abs(dy) > swipeSlop && Math.abs(dy) > Math.abs(dx)) { swipe = null; return; }
    // A shut row has nothing to the left of it to reveal.
    if (!swipe.open && dx < -swipeSlop) { swipe = null; return; }
    if (Math.abs(dx) < swipeSlop || Math.abs(dx) < Math.abs(dy) * 1.5) return;
    // The page is scrolled by the browser; once a swipe starts the finger is ours.
    swipe.locked = true;
    swipe.row.classList.add("swiping");
    try { swipe.row.setPointerCapture(e.pointerId); } catch {}
  }
  swipe.dx = (swipe.open ? swipe.tray : 0) + dx;
  setSwipe(swipe.row, swipeOffset(swipe.dx, swipe.tray));
  swipe.row.classList.toggle("swipe-armed", swipeArchive(swipe.row) !== null && swipe.dx >= swipe.width * swipeArmed);
  e.preventDefault?.();
}
function swipeArchive(row) {
  return [...row.querySelectorAll?.(".job-actions button") || []].find(b => b.getAttribute("aria-label") === "Archive") || null;
}
function swipeEnd(e) {
  if (!swipe || e.pointerId !== swipe.id) return;
  const {row, locked, dx, tray, width} = swipe;
  swipe = null;
  if (!locked) return;
  // The tap a swipe ends with must not open the job underneath.
  swipeMoved = performance.now();
  row.classList.remove("swiping", "swipe-armed");
  const archive = swipeArchive(row);
  const outcome = e.type === "pointercancel" ? (row.dataset.operationId === state.swipeOpenId ? "open" : "closed") : swipeSettle(dx, tray, width, archive !== null);
  if (outcome === "open") {
    row.classList.add("swipe-open");
    setSwipe(row, tray);
    state.swipeOpenId = row.dataset.operationId;
    markSwipeSeen();
    return;
  }
  row.classList.remove("swipe-open");
  row.style.removeProperty("--swipe");
  if (state.swipeOpenId === row.dataset.operationId) state.swipeOpenId = null;
  if (outcome === "archive") {
    markSwipeSeen();
    // The click is ours, not the tail of the swipe, so the guard lets it by.
    swipeMoved = -1000;
    archive.click();
    swipeMoved = performance.now();
  }
}
let swipeMoved = -1000;
function swipeClick(e) {
  // A swipe that ended on a link, and a tap on an open row, only shut the tray.
  const row = swipeRow(e.target);
  if (performance.now() - swipeMoved < 60) { e.preventDefault(); e.stopPropagation(); return; }
  if (!row || !row.classList.contains("swipe-open")) return;
  if (e.target.closest(".job-actions")) {
    // A choice from the tray: its own handler runs, and the tray shuts behind it.
    queueMicrotask(() => closeSwipe());
    return;
  }
  e.preventDefault();
  e.stopPropagation();
  closeSwipe();
}
// The queue redraws every few seconds; an open tray stays open across it.
function restoreSwipe(container) {
  if (!state.swipeOpenId) return;
  const row = [...container.querySelectorAll?.(".job-row.has-menu") || []].find(r => r.dataset.operationId === state.swipeOpenId);
  if (!row || !swipeActions()) { state.swipeOpenId = null; return; }
  row.classList.add("swipe-open", "swipe-restored");
  setSwipe(row, swipeTrayWidth(row));
  requestAnimationFrame?.(() => row.classList.remove("swipe-restored"));
}
if (typeof window !== "undefined" && typeof matchMedia === "function") {
  const list = $("jobs-list");
  list.addEventListener("pointerdown", swipeStart);
  list.addEventListener("pointermove", swipeMove);
  list.addEventListener("pointerup", swipeEnd);
  list.addEventListener("pointercancel", swipeEnd);
  list.addEventListener("click", swipeClick, true);
  document.addEventListener("pointerdown", e => { if (!e.target?.closest?.(".job-row.swipe-open")) closeSwipe(); });
  matchMedia(swipeQuery).addEventListener?.("change", () => { closeSwipe(); if (!state.jobsLoading) void loadJobs(); });
}
// A sheet closes with a drag down, as on any phone: from its heading, or from its
// body when that is scrolled to the top. The drag only moves the sheet; its own
// close button does the closing, so every dialog keeps its own way out.
const sheetSlop = 10;
let sheet = null;
// Where a released drag settles: "close" past a quarter of the sheet, or a short
// quick flick; otherwise it springs back.
function sheetSettle(dy, height, velocity) {
  return dy >= Math.min(140, height * 0.25) || (dy > 40 && velocity > 0.6) ? "close" : "back";
}
function sheetCloseButton(dialog) {
  return dialog.querySelector?.('.dialog-heading button[aria-label^="Close"], .jd-header button[aria-label^="Close"]') || null;
}
// Anything between the finger and the sheet that is scrolled down takes the drag as a scroll.
function sheetScrolled(target, dialog) {
  for (let el = target; el; el = el.parentElement) {
    if (el.scrollTop > 0) return true;
    if (el === dialog) break;
  }
  return false;
}
function sheetStart(e) {
  sheet = null;
  if (e.touches?.length !== 1 || !matchMedia("(pointer: coarse)").matches) return;
  const dialog = e.target?.closest?.("dialog[open]");
  if (!dialog || !sheetCloseButton(dialog) || e.target.closest("input, textarea, select")) return;
  const t = e.touches[0], header = !!e.target.closest(".dialog-heading, .jd-header");
  sheet = {dialog, x: t.clientX, y: t.clientY, at: performance.now(), dy: 0, velocity: 0, locked: false, ours: header || !sheetScrolled(e.target, dialog)};
}
function sheetMove(e) {
  if (!sheet || e.touches?.length !== 1) return;
  const t = e.touches[0], dx = t.clientX - sheet.x, dy = t.clientY - sheet.y;
  if (!sheet.locked) {
    // Up, sideways, or on a scrolled body: not ours.
    if (!sheet.ours || dy < -sheetSlop || Math.abs(dx) > sheetSlop && Math.abs(dx) > Math.abs(dy)) { sheet = null; return; }
    if (dy < sheetSlop || dy < Math.abs(dx) * 1.5) return;
    sheet.locked = true;
    sheet.dialog.style.transition = "none";
  }
  const now = performance.now(), next = Math.max(dy - sheetSlop, 0);
  if (now > sheet.at) sheet.velocity = (next - sheet.dy) / (now - sheet.at);
  sheet.at = now;
  sheet.dy = next;
  sheet.dialog.style.transform = `translateY(${next}px)`;
  e.preventDefault?.();
}
function sheetEnd(e) {
  if (!sheet) return;
  const {dialog, locked, dy, velocity} = sheet;
  sheet = null;
  if (!locked) return;
  const still = typeof matchMedia === "function" && matchMedia("(prefers-reduced-motion: reduce)").matches;
  const button = sheetCloseButton(dialog);
  const settle = () => { dialog.style.transition = still ? "none" : "transform .2s cubic-bezier(.2, .8, .2, 1)"; dialog.style.transform = ""; setTimeout(() => dialog.style.removeProperty("transition"), 250); };
  if (e.type === "touchcancel" || !button || button.disabled || sheetSettle(dy, dialog.offsetHeight, velocity) !== "close") return settle();
  dialog.style.transition = still ? "none" : "transform .2s cubic-bezier(.2, .8, .2, 1)";
  dialog.style.transform = `translateY(${innerHeight}px)`;
  // A dialog that stays open after its close button (it asked first) comes back.
  setTimeout(() => { button.click(); if (dialog.open) settle(); else { dialog.style.removeProperty("transform"); dialog.style.removeProperty("transition"); } }, still ? 0 : 200);
}
if (typeof window !== "undefined" && typeof matchMedia === "function") {
  document.addEventListener("touchstart", sheetStart, {passive: true});
  document.addEventListener("touchmove", sheetMove, {passive: false});
  document.addEventListener("touchend", sheetEnd);
  document.addEventListener("touchcancel", sheetEnd);
}
// More actions menus close on Escape and on a press anywhere outside them.
function closeActionMenus(except) {
  for (const menu of document.querySelectorAll(".more-actions[open]"))
    if (!except || !menu.contains(except)) menu.open = false;
}
function actionMenuKeydown(e) {
  if (e.key !== "Escape") return;
  const menu = document.activeElement?.closest?.(".more-actions[open]");
  closeActionMenus();
  // Inside a dialog, Escape closes only the menu, not the whole dialog.
  if (menu) {
    e.preventDefault?.();
    menu.querySelector("summary")?.focus();
  }
}
if (typeof document.addEventListener === "function") {
  document.addEventListener("pointerdown", (e) => closeActionMenus(e.target));
  document.addEventListener("keydown", actionMenuKeydown);
}
// The queue redraws every few seconds. Keep an open menu open, and keep focus
// on a More toggle whether its menu is open or closed, or on a menu item: the
// toggle and some items (View batch) carry no data-job-control, so
// restoreControl cannot find them.
function openActionMenu(container) {
  const active = document.activeElement;
  const menu = active?.closest?.(".more-actions") || container.querySelector?.(".more-actions[open]");
  if (!menu || !container.contains(menu)) return null;
  const toggle = menu.querySelector("summary") === active;
  const item = !toggle && active?.closest?.(".more-actions") === menu ? active.getAttribute("aria-label") : null;
  return { id: menu.dataset.menuFor, open: menu.open, toggle, item };
}
function restoreActionMenu(container, saved) {
  if (!saved?.id) return;
  const menu = [...container.querySelectorAll(".more-actions")].find((m) => m.dataset.menuFor === saved.id);
  if (!menu) return;
  menu.open = saved.open;
  if (saved.toggle) menu.querySelector("summary")?.focus({ preventScroll: true });
  else if (saved.item && saved.open)
    [...menu.querySelectorAll(".more-menu button")].find((b) => b.getAttribute("aria-label") === saved.item)?.focus({ preventScroll: true });
}
// Outcome labels of the encode summary the server gives under "benchmark"
// (tools/benchmark_summary.go); the key keeps the name sample tests gave it.
const resultOutcomes = {
  no_suitable_candidate: "No setting passed",
  audio_only: "Audio-only compaction",
  size_budget: "Encoded at your size budget",
  size_budget_ladder: "Encoding at your size budget",
  recipe: "Encoding with this goal's settings",
  size_rejected: "Size limit not met",
  quality_policy_unavailable: "Quality checks unavailable",
  attention: "Needs your decision",
  execution_error: "Encode could not finish",
  cancelled: "Stopped",
};
function resultHeadline(job) {
  const b = job.benchmark;
  if (b && b.outcome === "no_suitable_candidate") return "No setting passed · Original kept";
  return "";
}
// The job's one-line outcome when no result card explains it.
function jobOutcomeText(job) {
  if (notConverted(job)) return notConvertedReason(job);
  if (job.status === "completed" && !job.candidate_ready && job.reason_code) return shortJobReason(job.reason_code);
  // A size budget ladder or a goal's recipe says which full encode runs.
  if (["size_budget_ladder", "recipe"].includes(job.benchmark?.outcome) && job.benchmark.message) return job.benchmark.message;
  return shortJobReason(job.transcode?.message);
}
// Whether the encode summary has a result card: a ladder or recipe that is
// still encoding, or one not yet planned, has nothing to report.
function hasResultReport(b) {
  return Boolean(b?.outcome) && !["preparing", "size_budget_ladder", "recipe"].includes(b.outcome);
}
// Renders the server's encode summary (tools/benchmark_summary.go) for people:
// what happened and what to do next.
function resultReport(job) {
  const b = job.benchmark;
  if (!hasResultReport(b)) return null;
  const box = node("section", "", `result-report outcome-${b.outcome}`);
  box.setAttribute("aria-label", "Result");
  const head = node("div", "", "result-report-head");
  head.append(node("h3", "Result"), node("span", resultOutcomes[b.outcome] || b.outcome.replace(/_/g, " "), "result-outcome"));
  box.append(head);
  if (b.message) box.append(node("p", b.message));
  const nextStep = b.user_next_step || b.next_step;
  if (nextStep) {
    const next = node("p", "", "result-next");
    next.append(node("strong", "Next step: "), node("span", nextStep));
    box.append(next);
  }
  return box;
}
// What a Save space or Shrink job kept of the audio and subtitles: the sentence
// the server wrote, and the tracks behind it, one line each.
function keptUntagged(tracks) {
  return Boolean(tracks) && !tracks.manual && [tracks.audio, tracks.subtitles].some(split => split?.kept?.some(entry => canonicalLanguage(entry.language) === "und"));
}
function trackReport(job) {
  const report = job.tracks, message = report?.message || job.tracks_message;
  if (!message) return null;
  const box = node("section", "", "track-report");
  box.setAttribute("aria-label", "Audio and subtitles");
  const line = node("p", "", "track-report-line");
  line.append(node("strong", "Tracks: "), node("span", message));
  box.append(line);
  const groups = [["Audio", report?.audio], ["Subtitles", report?.subtitles]].filter(([, split]) => split?.total > 0);
  if (!groups.length) return box;
  const details = node("details", "", "track-report-details");
  // A refresh rebuilds the dialog; the list stays open for the job it was opened on.
  details.open = state.trackReportOpen === job.id;
  details.addEventListener("toggle", () => { state.trackReportOpen = details.open ? job.id : null; });
  details.append(node("summary", "Kept and dropped tracks"));
  for (const [title, split] of groups) {
    const entries = [...split.kept.map(entry => ({...entry, kept: true})), ...split.dropped.map(entry => ({...entry, kept: false}))].sort((a, b) => a.index - b.index);
    const list = node("ul", "", "track-report-list");
    for (const entry of entries) {
      const row = node("li", "", `track-report-row ${entry.kept ? "kept" : "dropped"}`);
      row.append(node("span", entry.kept ? "Kept" : "Dropped", "badge track-badge"), node("strong", languageName(entry.language)));
      const facts = [trackCodec(entry.codec), entry.title, entry.bytes > 0 ? bytes(entry.bytes) : ""].filter(Boolean);
      if (facts.length) row.append(node("span", facts.join(" · "), "metadata"));
      list.append(row);
    }
    details.append(node("h4", `${title} · ${split.kept.length} of ${split.total} kept`), list);
  }
  box.append(details);
  return box;
}
// plan_source/plan_note arrive only once the encode's settings are settled; a
// provisional preflight plan is never presented as a result.
function encodingPlanNote(job) {
  const b = job.benchmark;
  if (!b?.plan_source) return null;
  const budgetMet = job.size_mode?.size_budget?.outcome === "meets_quality_target_within_budget";
  const text = b.plan_source === "size_budget" ? `Encoded at your size budget (${budgetMet ? "within" : "below"} the quality target).` : b.plan_note || "The profile's fixed settings.";
  const line = node("p", "", `encoding-plan plan-${b.plan_source}`);
  line.append(node("strong", "Encoding plan: "), node("span", text));
  return line;
}
// Size mode reports what it predicted and measured for the file: the sampled
// output size, the final VMAF and which audio tracks became AAC.
function sizeModeLines(size) {
  if (!size) return [];
  const lines = [];
  if (size.expected_bytes != null) lines.push(`Expected size ${bytes(size.expected_bytes)}${size.main10 ? " · 10-bit HEVC" : ""}`);
  else if (size.main10) lines.push("10-bit HEVC");
  // The encoder tells a media engine winner apart from x265 before anything is replaced.
  if (size.encoder) lines.push(`Encoder: ${{libx265: "x265 (software)", hevc_videotoolbox: "VideoToolbox (hardware media engine)"}[size.encoder] || size.encoder}`);
  // Why the media engine result was chosen over x265, as the search recorded it.
  if (size.hardware && size.hardware_reason) lines.push(`Chosen because: ${size.hardware_reason}`);
  if (size.size_budget) lines.push(...sizeBudgetLines(size.size_budget));
  else if (size.size_budget_note) lines.push(size.size_budget_note);
  if (size.video_copy) lines.push({verified: "Video copied unchanged · verified bit-identical", failed: "Video copy check failed · original kept"}[size.video_copy] || "Video will be copied unchanged · not yet verified; only the audio is made smaller");
  // A media engine result that failed the whole-file check, and what followed;
  // the failed attempt's numbers stay here after the x265 result takes over.
  const hc = size.hardware_continuation;
  if (hc) {
    if (hc.message) lines.push(hc.message);
    const f = hc.failed_quality;
    if (f) {
      if (f.vmaf_mean != null) lines.push(`VideoToolbox final VMAF ${Number(f.vmaf_mean).toFixed(1)} mean · ${f.vmaf_p5 != null ? Number(f.vmaf_p5).toFixed(1) : "n/a"} p5${f.worst_window_mean != null ? ` · ${Number(f.worst_window_mean).toFixed(1)} worst ${Math.round(f.worst_window_seconds || 0)} s window` : ""}`);
      for (const failure of f.failures || []) lines.push(`Failed: ${failure}`);
    }
    // The failed result's windows, search against final, before the x265 result's.
    lines.push(...windowReportLines(hc.windows).map(line => `VideoToolbox ${line.charAt(0).toLowerCase()}${line.slice(1)}`));
  }
  const q = size.final_quality;
  if (q) lines.push(`Final VMAF ${Number(q.vmaf_mean).toFixed(1)} mean · ${q.vmaf_p5 != null ? Number(q.vmaf_p5).toFixed(1) : "n/a"} p5${q.worst_window_mean != null ? ` · ${Number(q.worst_window_mean).toFixed(1)} worst ${Math.round(q.worst_window_seconds || 0)} s window` : ""}`);
  if (q?.failed_checks?.length) lines.push(`Failed checks: ${q.failed_checks.map(code => String(code).replace(/^quality_/, "").replace(/_/g, " ")).join(", ")}`);
  if (q?.fresh_vmaf_mean != null) lines.push(`Fresh windows VMAF ${Number(q.fresh_vmaf_mean).toFixed(1)} mean · ${q.fresh_vmaf_p5 != null ? Number(q.fresh_vmaf_p5).toFixed(1) : "n/a"} p5 · reported only; they never decide the result`);
  lines.push(...verificationLines(size.verification), ...windowReportLines(size.windows));
  const audio = size.compacted_audio || [];
  // Size mode converts DTS only when the profile is DTS-HD MA.
  if (audio.length) lines.push(`Audio compacted: ${audio.map(a => `track ${a.stream_index + 1} ${a.from_codec === "dts" ? "DTS-HD MA" : codecLabel(a.from_codec)} → ${codecLabel(a.to_codec)}`).join(", ")}`);
  return lines;
}
// How the search checked its winner before the full encode: each attempt in
// order, and whether the search time limit ended the checking early.
function verificationLines(v) {
  if (!v) return [];
  const roles = {smallest: "smallest", next: "next", safest: "safest", bisect: "between"};
  const outcomes = {passed: "passed", quality_failed: "failed quality", rejected: "rejected"};
  const lines = [];
  const attempts = v.attempts || [];
  if (attempts.length) lines.push(`Verified on ${v.windows === "final_grid" ? "the final validation windows" : "edge windows"}: ${attempts.map(a => `${roles[a.role] || a.role} ${outcomes[a.outcome] || a.outcome}`).join(" → ")}`);
  if (v.stopped_by === "time_cap") lines.push(`Verification stopped at the search time limit${v.unverified ? `; ${v.unverified} ${v.unverified === 1 ? "setting was" : "settings were"} never checked` : ""}`);
  return lines;
}
function windowClock(seconds) {
  const total = Math.round(Number(seconds) || 0);
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, "0")}`;
}
// One line per window: what the search measured for the winner there, and
// what final validation measured on the encoded file.
function windowReportLines(windows) {
  const score = side => side ? Number(side.vmaf_mean).toFixed(1) : "—";
  return (windows || []).map(w => {
    const delta = w.search && w.final ? ` (${w.final.vmaf_mean - w.search.vmaf_mean >= 0 ? "+" : "−"}${Math.abs(w.final.vmaf_mean - w.search.vmaf_mean).toFixed(1)})` : "";
    return `Window ${windowClock(w.start_seconds)} · ${w.role} · search ${score(w.search)} → final ${score(w.final)} mean${delta}`;
  });
}
function codecLabel(codec) {
  return {truehd:"TrueHD",dts:"DTS",flac:"FLAC",alac:"ALAC",mlp:"MLP",aac:"AAC"}[codec] || (String(codec || "").startsWith("pcm_") ? "PCM" : String(codec || "").toUpperCase());
}
// Size budget: the search found nothing that meets the quality limits (or only
// something over the budget), so the file was encoded once at the budget.
const sizeBudgetOutcomes = {
  below_quality_target_within_budget: "Below quality target — within size budget",
  meets_quality_target_within_budget: "Meets quality target — within size budget",
  below_quality_floor: "Below the quality floor — no candidate offered",
  size_budget_exceeded: "Over the size budget — no candidate offered",
};
// Shown wherever a candidate is offered, so a file that missed the quality
// target to fit its budget is never approved unknowingly.
function belowTargetLabel(outcome) {
  return outcome === "below_quality_target_within_budget" ? sizeBudgetOutcomes[outcome] : "";
}
function belowTargetNotice(outcome) {
  const label = belowTargetLabel(outcome);
  if (!label) return null;
  const n = node("p", "", "size-budget-warning metadata");
  n.append(node("strong", `${label}. `), node("span", "Compare it with the original before approving the replacement."));
  return n;
}
function mbps(kbps) {
  return `${(Number(kbps) / 1000).toFixed(2)} Mbps`;
}
function sizeBudgetSiblingLines(sib) {
  if (!sib) return [];
  const lines = [`Budget from ${sib.count} sibling episodes (median ${mbps(sib.median_bitrate_kbps)} × ${sib.multiplier})${sib.scope ? ` in the ${sib.scope} folder` : ""}${sib.resolution_class ? ` · ${sib.resolution_class}` : ""}`];
  if (sib.source_bitrate_kbps > 0) lines.push(`This file averages ${mbps(sib.source_bitrate_kbps)}${sib.ratio > 0 ? `, ${Number(sib.ratio).toFixed(1)}× the siblings' median` : ""}`);
  return lines;
}
function sizeBudgetLines(b) {
  const lines = [];
  if (b.message) lines.push(b.message);
  const share = b.budget_percent > 0 ? ` (${Number(b.budget_percent)}% of the source)` : b.source_bytes > 0 ? ` (${Math.round(100 * b.budget_bytes / b.source_bytes)}% of the source)` : "";
  lines.push(`Size budget ${bytes(b.budget_bytes)}${share} · aims at ${bytes(b.target_bytes)} · video ${b.video_bitrate_kbps} kbps`);
  if (b.candidate_bytes > 0) lines.push(`Result ${bytes(b.candidate_bytes)} of the ${bytes(b.budget_bytes)} budget (${(100 * b.candidate_bytes / b.budget_bytes).toFixed(1)}%)`);
  lines.push(...sizeBudgetSiblingLines(b.siblings));
  if (b.trigger === "passing_candidate_over_budget" && b.over_budget_candidate_id) lines.push(`The setting that met the quality target (${b.over_budget_candidate_id}${b.over_budget_estimated_bytes > 0 ? `, about ${bytes(b.over_budget_estimated_bytes)}` : ""}) is larger than the budget`);
  const target = b.quality_target?.vmaf_mean_minimum, floor = b.quality_floor;
  if (target != null || floor?.vmaf_mean_minimum != null) lines.push(`Quality target VMAF mean ≥ ${target ?? "—"} · floor ${floor?.vmaf_mean_minimum ?? "—"} mean, ${floor?.vmaf_p5_minimum ?? "—"} p5, ${floor?.vmaf_worst_window_minimum ?? "—"} worst window`);
  for (const miss of b.below_target || []) lines.push(`Below target: ${miss}`);
  const choice = b.encoder_choice;
  if (choice?.video_codec) lines.push(`Encoder at the budget: ${budgetEncoderName(choice.video_codec)}${choice.message ? ` — ${choice.message}` : ""}`);
  const fix = b.bitrate_correction;
  if (fix?.bitrate_kbps > 0) lines.push(`VideoToolbox overshot at ${fix.first_bitrate_kbps} kbps (${bytes(fix.first_candidate_bytes)}); encoded again at ${fix.bitrate_kbps} kbps`);
  return lines;
}
function budgetEncoderName(codec) {
  return codec === "hevc_videotoolbox" ? "VideoToolbox" : "x265 two-pass";
}
// Point kinds: "searched" settings, the two "budget_measured" encodes of the
// encoder comparison at the budget, a "budget_overshoot" VideoToolbox encode
// re-encoded at a lower bitrate, a "budget_rung_over" VideoToolbox step that
// came out over the budget, and the "budget_fallback" encode itself.
function sizeBudgetPointLabel(p) {
  const bits = [];
  const kbps = p.bitrate_kbps ? ` ${p.bitrate_kbps} kbps` : "";
  if (p.kind === "budget_fallback") bits.push(`Budget encode · ${budgetEncoderName(p.video_codec)}${p.quality > 0 ? ` quality ${p.quality}` : kbps}`);
  else if (p.kind === "budget_measured") bits.push(`Compared at the budget · ${budgetEncoderName(p.video_codec)}${kbps}${p.chosen ? " · chosen" : ""}`);
  else if (p.kind === "budget_overshoot") bits.push(`First budget encode · ${budgetEncoderName(p.video_codec)}${kbps}`);
  else if (p.kind === "budget_rung_over") bits.push(`VideoToolbox ${p.quality > 0 ? `quality ${p.quality}` : `average bitrate${kbps}`}`);
  else bits.push(`${p.candidate_id || "Setting"}${p.video_codec === "hevc_videotoolbox" ? " · VideoToolbox" : ""}`);
  bits.push(`${p.measured ? "" : p.kind === "budget_fallback" ? "planned " : "est. "}${bytes(p.bytes)}`);
  bits.push(p.vmaf_mean != null ? `VMAF ${Number(p.vmaf_mean).toFixed(1)}${p.kind === "budget_measured" && p.vmaf_p5 != null ? ` (p5 ${Number(p.vmaf_p5).toFixed(1)})` : ""}` : "VMAF not measured yet");
  if (p.kind === "searched") bits.push(p.passed ? "met the quality target" : "missed the quality target");
  bits.push(p.within_budget ? "within budget" : "over budget");
  return bits.join(" · ");
}
function sizeBudgetPointClass(p, measuredY) {
  if (p.kind === "budget_fallback") return measuredY ? "point fallback" : "point planned";
  if (p.kind === "budget_measured") return `point compared${p.chosen ? " chosen" : ""}`;
  if (p.kind === "budget_overshoot" || p.kind === "budget_rung_over") return "point overshoot";
  return `point${p.passed ? " passed" : ""}`;
}
// A compact inline rate-quality chart: size across, VMAF mean up; searched
// settings as dots, the budget encode highlighted, a vertical line at the
// budget and horizontal lines at the quality target and floor. The point list
// beside it carries the same numbers, so the chart is optional.
function sizeBudgetChart(b) {
  if (typeof document.createElementNS !== "function") return null;
  const points = (b.curve || []).filter(p => p.bytes > 0);
  if (!points.length) return null;
  const NS = "http://www.w3.org/2000/svg";
  const el = (name, attrs = {}, text = "") => {
    const n = document.createElementNS(NS, name);
    for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, String(v));
    if (text) n.textContent = text;
    return n;
  };
  const W = 320, H = 150, L = 34, R = 8, T = 12, B = 26;
  const xs = [...points.map(p => p.bytes), b.budget_bytes], xMin = Math.min(...xs), xMax = Math.max(...xs);
  const target = b.quality_target?.vmaf_mean_minimum, floor = b.quality_floor?.vmaf_mean_minimum;
  const ys = [...points.filter(p => p.vmaf_mean != null).map(p => p.vmaf_mean), ...[target, floor].filter(v => v != null)];
  const yMin = Math.floor(Math.min(...ys, 100) - 4), yMax = Math.ceil(Math.max(...ys, 0) + 2);
  const x = v => L + (xMax === xMin ? (W - L - R) / 2 : (v - xMin) / (xMax - xMin) * (W - L - R));
  const y = v => T + (yMax - v) / Math.max(1, yMax - yMin) * (H - T - B);
  const svg = el("svg", { viewBox: `0 0 ${W} ${H}`, class: "size-budget-chart", role: "img", "aria-label": `Size against quality: ${points.length} points, budget ${bytes(b.budget_bytes)}` });
  svg.append(el("path", { class: "axis", d: `M${L} ${T}V${H - B}H${W - R}` }));
  svg.append(el("line", { class: "budget-line", x1: x(b.budget_bytes), x2: x(b.budget_bytes), y1: T, y2: H - B }), el("text", { x: x(b.budget_bytes), y: H - B + 20, "text-anchor": "middle" }, `budget ${bytes(b.budget_bytes)}`));
  if (target != null) svg.append(el("line", { class: "target-line", x1: L, x2: W - R, y1: y(target), y2: y(target) }), el("text", { x: L + 2, y: y(target) - 2 }, `target ${target}`));
  if (floor != null) svg.append(el("line", { class: "floor-line", x1: L, x2: W - R, y1: y(floor), y2: y(floor) }), el("text", { x: L + 2, y: y(floor) - 2 }, `floor ${floor}`));
  svg.append(el("text", { x: 2, y: T + 4 }, "VMAF"), el("text", { x: L, y: H - B + 10 }, bytes(xMin)), el("text", { x: W - R, y: H - B + 10, "text-anchor": "end" }, bytes(xMax)));
  for (const p of points) {
    const measuredY = p.vmaf_mean != null;
    const cls = sizeBudgetPointClass(p, measuredY);
    const dot = el("circle", { class: cls, cx: x(p.bytes), cy: measuredY ? y(p.vmaf_mean) : H - B, r: p.kind === "budget_fallback" ? 5 : p.kind === "searched" ? 3 : 4 });
    dot.append(el("title", {}, sizeBudgetPointLabel(p)));
    svg.append(dot);
  }
  return svg;
}
function sizeBudgetNote(job) {
  const b = job.size_mode?.size_budget;
  if (!b) return null;
  const box = node("section", "", "size-budget");
  box.setAttribute("aria-label", "Size budget");
  const outcome = sizeBudgetOutcomes[b.outcome];
  if (outcome) {
    const badge = node("span", outcome, "badge size-budget-badge");
    badge.dataset.outcome = b.outcome;
    box.append(badge);
  }
  const chart = sizeBudgetChart(b);
  if (chart) box.append(chart);
  if (b.curve?.length) {
    const details = node("details", "", "size-budget-points");
    details.append(node("summary", `Size and quality points (${b.curve.length})`));
    const list = node("ul", "", "result-list");
    for (const p of b.curve) list.append(node("li", sizeBudgetPointLabel(p)));
    details.append(list);
    box.append(details);
  }
  const files = b.siblings?.files || [];
  if (files.length) {
    const details = node("details", "", "size-budget-siblings");
    details.append(node("summary", `Sibling episodes (${files.length})`));
    const list = node("ul", "", "result-list");
    for (const f of files) list.append(node("li", `${f.name} · ${bytes(f.bytes)} · ${mbps(f.bitrate_kbps)}${f.width && f.height ? ` · ${f.width}×${f.height}` : ""}${f.duration_seconds > 0 ? ` · ${Math.round(f.duration_seconds / 60)} min` : ""}`));
    details.append(list);
    box.append(details);
  }
  return box;
}
function sizeModeNote(job) {
  const lines = sizeModeLines(job.size_mode);
  if (!lines.length) return null;
  const box = node("div", "", "size-mode-summary metadata");
  for (const line of lines) {
    const row = node("p", line);
    if (job.size_mode.hardware && line.startsWith("Encoder: ")) row.append(" ", node("span", "Hardware", "badge hardware-badge"));
    box.append(row);
  }
  const budget = sizeBudgetNote(job);
  if (budget) box.append(budget);
  return box;
}
function queueOutcome(job) {
  if (job.archived) return "Archived · Restore to return to the queue";
  const bench = resultHeadline(job);
  if (bench) return bench;
  const batch = visibleBatch(job);
  const cleanup = candidateCleanupNote(job);
  if (rejectedCandidate(job) || batch?.outcome === "rejected") return ["Candidate rejected · Original preserved", cleanup].filter(Boolean).join(" · ");
  if (job.candidate_cleanup?.reason === "superseded") return ["Replaced by new batch settings · Original preserved", cleanup].filter(Boolean).join(" · ");
  if (job.status === "waiting_decision") {
    if (job.paused || job.waiting_options?.some(option => option.decision === "resume")) return "Paused · Resume to continue the remaining files";
    if (job.batch?.promotion_plan_ready || job.promotion) return "Candidates verified · Waiting for replacement approval · Originals unchanged";
    return `${shortJobReason(job.waiting_reason) || "A candidate needs your decision"} · Originals unchanged`;
  }
  if (batch?.policy === batch?.total && batch?.policy > 0 || savingsPolicyFailure(job.error)) return "Sample test did not meet the savings requirement · Full conversion not started · Originals kept";
  if (job.status === "failed") return "Open job for error details";
  if (job.status === "cancelled") return ["Stopped by user", cleanup].filter(Boolean).join(" · ");
  if (job.status === "pending") return "Waiting to start";
  if (job.replaced_files > 0 && job.remaining_candidates > 0) return `${job.replaced_files} replacements verified · ${job.remaining_candidates} candidates still available; their originals remain in place`;
  if (job.savings?.realized_saved_bytes != null || job.replaced) return "Replacement verified";
  if (job.batch?.dry_run) return job.preview_execution_action_id ? "Batch started · View its progress" : job.batch.queued > 0 ? "Preview finished · Nothing starts automatically" : "Preview finished · No eligible files";
  if (job.batch?.outcome === "no_changes") return "Originals preserved";
  if (batch?.outcome === "failed") return "No candidates created · Change settings to try again";
  if (batch?.completed > 0) return `${batch.completed} ${batch.completed === 1 ? "candidate ready" : "candidates ready"} · Originals unchanged`;
  if (job.candidate_ready) return `Original preserved · Candidate available${belowTargetLabel(job.size_budget_outcome) ? ` · ${belowTargetLabel(job.size_budget_outcome)}` : ""}`;
  if (notConverted(job)) return notConvertedReason(job);
  if (job.action_name === "transcode_media" && job.reason_code && job.status === "completed") return shortJobReason(job.reason_code);
  if (job.action_name === "benchmark_transcode") return "Sample test complete · Original preserved";
  return "Finished · Open job for details";
}
// Every goal applies the same source gate, so a new goal cannot help these files.
// hfr_quality_unavailable clears only on a worker that can measure the rate.
function sourceGateReason(reason) {
  return reason === "unsupported_source" || reason === "hfr_quality_unavailable";
}
function onlyUnsupportedSources(job) {
  const reasons = job.batch_files?.reasons || [];
  return job.batch?.outcome === "no_changes" && reasons.length > 0 && reasons.every(reason => sourceGateReason(reason.reason));
}
function skippedBatchOutcome(job) {
  const result = node("div", "", "skipped-outcome");
  const reasons = job.batch_files?.reasons || [];
  const reason = reasons[0];
  const total = job.batch?.total || 0;
  const text = reason ? shortJobReason(reason.reason) : "No conversion was started";
  result.append(node("p", `${reason && reason.count < total ? `${reason.count} of ${total} files: ` : ""}${text}`, "skipped-reason"));
  const frameRateOnly = reasons.length > 0 && reasons.every(r => r.reason === "hfr_quality_unavailable");
  result.append(node("p", job.mode && onlyUnsupportedSources(job) ? frameRateOnly ? "The worker cannot measure quality at this frame rate yet, so changing the goal will not help. Your originals are unchanged." : "Automatic testing does not support this source format yet, so changing the goal will not help. Your originals are unchanged." : job.mode ? "None of the tested settings met this goal. You can change the goal; your originals are unchanged." : "Choose another profile to try again. Your originals are unchanged.", "metadata"));
  return result;
}
function renderDecisionCount(data) {
  $("job-filter-decision").textContent = data.decision_count > 0 ? `Needs decision (${data.decision_count})` : "Needs decision";
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
  markQueueFilter();
  const status = $("job-filter").value;
  const offset = more ? state.jobsLoaded || 0 : 0;
  const target = more ? 25 : Math.max(25, state.jobsLoaded || 25);
  let data,
    page = [];
  try {
    // The server bounds each page to 100. Refresh the complete visible window.
    while (page.length < target) {
      data = await api(
        `operations?${new URLSearchParams({ group: "workflow", status, limit: Math.min(100, target - page.length), offset: offset + page.length, tz_offset: String(-new Date().getTimezoneOffset()) })}`,
      );
      if (revision !== state.jobsRevision || $("workspace").hidden) return;
      if (!Array.isArray(data.jobs))
        throw new Error("Unexpected jobs response.");
      page.push(...data.jobs);
      if (!data.has_more || !data.jobs.length) break;
    }
    renderSavings(data);
    renderDecisionCount(data);
    if (!more) state.operationJobs = new Map();
    state.operationJobs ||= new Map();
    for (const job of page) state.operationJobs.set(job.workflow_id || job.id, job);
    state.jobsLoaded = offset + page.length;
    state.jobsHasMore = data.has_more;
    // A row under a finger is not rebuilt; the next refresh draws it.
    if (swipe?.locked) return;
    const focus = focusedControl($("jobs-list"));
    const menu = openActionMenu($("jobs-list"));
    const commandFocused = commandFocus();
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
      heading.append(jobLink(singleFile?.display_label || presentation.title, job.id));
      heading.append(statusBadge(presentation.status));
      // A goal batch keeps encoding while one file waits, so its own status says
      // "In progress"; the flag says a decision is waiting.
      const flagged = job.needs_decision && job.status !== "waiting_decision";
      if (flagged) {
        const decision = statusBadge("Needs decision", "waiting_decision");
        decision.classList.add("decision-badge");
        decision.title = "Needs decision";
        heading.append(decision);
      }
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
      let main = [...actions.children].find(child => /\baction-main\b/.test(child.className));
      if (!main && flagged) {
        main = button("Review", () => openJob(job.id), "action-main");
        main.title = "Review the file that needs a decision";
      }
      // One sentence says what happened or what runs now; the one thing to do
      // next is a link at its end. Everything else is in the More actions menu.
      // The generic fallback says nothing; a batch with files to review says how many.
      const fallback = queueOutcome(job);
      const reviewing = visibleBatch(job)?.waiting_decision || visibleBatch(job)?.review;
      const outcome = fallback !== "Finished · Open job for details" ? fallback : reviewing > 0 ? `${reviewing} ${reviewing === 1 ? "file needs" : "files need"} your review · Originals unchanged` : "";
      const failedLine = job.status === "failed" && outcome === "Open job for error details" ? shortJobReason(job.error) || (job.batch_files?.reasons?.[0] ? shortJobReason(job.batch_files.reasons[0].reason) : "") : "";
      const headline = replacementHeadline(job);
      const sentenceText = skipped || presentation.showTelemetry && !headline ? "" : failedLine || headline || outcome;
      if (sentenceText || main) {
        const sentence = node("p", sentenceText && main ? `${sentenceText} ` : sentenceText, `job-sentence${presentation.status === "Failed" ? " failed" : ""}`);
        if (sentenceText) sentence.title = sentenceText;
        if (main) sentence.append(main);
        meta.append(sentence);
      }
      // Facts about the job on one quiet line.
      const summaryText = skipped ? [singleFile ? presentation.context : `${job.batch.total} files`, job.batch.total === 1 ? "Original kept" : "Originals kept"].filter(Boolean).join(" · ") : presentation.facts ? (presentation.facts.length ? [presentation.kind, ...presentation.facts].join(" · ") : "") : presentation.summary;
      if (summaryText) {
        const summary = node("p", summaryText, "metadata job-summary");
        summary.title = summaryText;
        meta.append(summary);
      }
      for (const reason of (skipped ? [] : job.batch_files?.reasons || []).slice(0, 1)) {
        const text = shortJobReason(reason.reason);
        if (text === failedLine && !job.error) continue;
        const count = batchReasonLabel(job, reason);
        const line = node("p", `${count}: ${text}`, "metadata batch-reason");
        line.title = reason.reason;
        meta.append(line);
      }
      const menu = [...actions.children].filter(child => child !== main);
      row.append(heading, meta);
      const live = skipped ? null : presentation.showTelemetry ? telemetry(job, {segments: true}) : batchProgress(job, {segments: true});
      // A bar of one colour says nothing the summary line has not said (every file replaced, say), so it is left out.
      const spread = skipped || live ? null : (seg => seg?.kinds > 1 ? seg : null)(batchSegments(visibleBatch(job)));
      let body = skipped ? skippedBatchOutcome(job) : live;
      if (spread) {
        body = node("div", "", "job-progress batch-summary");
        body.append(spread.bar, node("span", spread.legend, "batch-legend metadata"));
      }
      if (body) {
        const footer = node("div", "", "job-footer");
        footer.append(body);
        row.append(footer);
      }
      if (menu.length) {
        actions.replaceChildren(...menu);
        row.className += " has-menu";
        row.append(actions);
      }
      $("jobs-list").append(row);
    }
    if (!state.operationJobs.size)
      $("jobs-list").append(node("p", "No jobs for this filter.", "empty"));
    renderCommandIndicators();
    restoreCommandFocus(commandFocused);
    restoreActionMenu($("jobs-list"), menu);
    restoreControl($("jobs-list"), focus);
    restoreSwipe($("jobs-list"));
    $("swipe-hint").hidden = !swipeHintVisible() || !document.querySelector(".job-row.has-menu");
    // The list is rebuilt on return, so the place it was left at is restored
    // once it is as long as it was.
    if (state.jobsRestore != null && state.tab === "jobs" && typeof window !== "undefined") {
      window.scrollTo({ top: state.jobsRestore });
    }

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
      // One attempt only: a failed load must not scroll the list later.
      state.jobsRestore = null;
    }
  }
}

// The worker's encode ends before its full-output checks and publication;
// its last progress measurement then marks the encode's end, not liveness.
const postEncodeWorkerPhases = ["validating", "validating_quality", "publishing", "publication_pending", "completed"];
// A finished worker still leaves the coordinator verifying the candidate and
// re-hashing the whole original, minutes on a large file. Name that work
// rather than the finished encode or calibration; null while neither applies.
function postEncodeWork(job) {
  if (!["running", "waiting_external"].includes(job?.status)) return null;
  const preserve = preserveWork(job);
  if (preserve) return preserve;
  const w = job.worker || {};
  // Only the full conversion: a finished calibration hands back to the worker.
  const converted = w.transcode_phase === "completed" || w.transcode_status === "completed";
  const stage = job.stages?.[job.current_step]?.name;
  const work = job.work || {};
  const total = Number(work.total_bytes) > 0 ? Number(work.total_bytes) : 0;
  if (work.phase === "verifying_original") {
    const read = Math.max(0, Number(work.bytes_read) || 0);
    return {label: "Checking the original is unchanged", read: total ? Math.min(read, total) : read, total};
  }
  // Preflight queues its own source hash the same way; only a later step's
  // wait is the original's re-check. A skipped conversion reaches it too.
  if (work.phase === "waiting_for_hash_slot" && (stage === "accept_result" || converted && stage !== "preflight")) return {label: "Waiting to verify original", total};
  if (!converted) return null;
  return {label: ["validate_result", "accept_result"].includes(stage) ? `Conversion finished · ${stageLabel(stage)}` : "Conversion finished"};
}
// Preparing recovery re-checks the original and the converted file: on a
// large file, many GB of storage reads. When the original cannot be moved
// aside, removing it first copies the original and verifies that copy. Name
// and measure each stage; null outside them.
const preserveWorkLabels = {waiting_for_hash_slot:"Waiting for a hash slot",verifying_original:"Re-checking the original",verifying_candidate:"Re-checking the converted file",copying_recovery:"Copying the recovery copy",verifying_recovery:"Verifying the recovery copy"};
function preserveWork(job) {
  const stages = job?.stages || [];
  const stage = stages[job?.current_step]?.name;
  const work = job?.work || {};
  const fallbackCopy = stage === "remove_old_file" && ["waiting_for_hash_slot", "verifying_original", "copying_recovery", "verifying_recovery"].includes(work.phase);
  if (stage !== "preserve_original" && !fallbackCopy) return null;
  const label = preserveWorkLabels[work.phase];
  if (!label) return null;
  const total = Number(work.total_bytes) > 0 ? Number(work.total_bytes) : 0;
  const step = `Step ${job.current_step + 1}/${stages.length} · ${stageLabel(stage)}`;
  if (work.phase === "waiting_for_hash_slot") return {label, total, step};
  const read = Math.max(0, Number(work.bytes_read) || 0);
  if (!total) return {label, read, total, step};
  const done = Math.min(read, total);
  // Floor: 100% only once the whole file has been read.
  return {label: `${label} · ${Math.floor(done / total * 100)}%`, measure: label, read: done, total, step};
}
function postEncodeWorkLabel(work) {
  return work.read != null && work.total > 0 ? `${work.label} · ${bytes(work.read)} / ${bytes(work.total)}` : work.label;
}
// The coordinator's own slot queue (work.phase waiting_for_worker): the file
// has not been sent to the worker yet. queue_position counts this file, so 1
// means it gets the next free slot.
function workerSlotWaitLabel(work) {
  const position = Number(work?.queue_position);
  if (!Number.isInteger(position) || position < 1) return "Waiting for a worker slot";
  if (position === 1) return "Waiting for a worker slot · Next in line";
  return `Waiting for a worker slot · ${position - 1} ahead in line`;
}
function workerPhaseLabel(phase) {
  return {
    queued:"Waiting for a worker slot",waiting_for_worker:"Waiting for a worker slot",preparing:"Preparing source",reading_source:"Reading source from media storage",
    probing_source:"Inspecting source",
    analysing_video:"Analyzing video · x265 pass 1 of 2",encoding:"Encoding full video",validating:"Verifying converted file",validating_quality:"Measuring final quality",publishing:"Saving converted file",
    completed:"Completed",
  }[phase] || (phase ? phase.replaceAll("_"," ") : "Preparing job");
}
function activityElapsed(value) {
  const at = Date.parse(value || "");
  if (!Number.isFinite(at) || at < Date.UTC(2000,0,1)) return null;
  const seconds = Math.max(0,Math.floor((Date.now()-at)/1000));
  return seconds >= 60 ? `${Math.floor(seconds/60)}m ${seconds%60}s` : `${seconds}s`;
}
// Where a job sits in the slot grid. Without a split every kind shares the
// whole grid; with one, video work (conversions and benchmarks) takes the first
// video.total slots and podcast work the next podcast.total.
function slotPools(activity) {
  const split = activity.slots?.split ? activity.slots : null;
  const total = activity.worker_slots_total;
  if (!split) return {split: null, total, range: () => [0, total], label: i => `Slot ${i+1}`};
  const video = split.video.total, podcast = split.podcast.total;
  return {
    split, total: Math.min(total, video + podcast),
    range: kind => kind === "podcast" ? [video, video + podcast] : [0, video],
    label: i => i < video ? `Video slot ${i+1}` : `Podcast slot ${i-video+1}`,
  };
}
function renderSlotUsage(activity) {
  const slots = activity.slots, usage = $("worker-slot-usage");
  const x265 = slots?.x265?.total > 0 ? slots.x265 : null;
  usage.hidden = !slots?.split && !x265;
  if (!usage.hidden) usage.textContent = [
    slots.split ? `Video ${slots.video.used}/${slots.video.total} · Podcast ${slots.podcast.used}/${slots.podcast.total}` : null,
    x265 ? `x265 ${x265.used}/${x265.total}` : null,
    `Ceiling ${slots.ceiling}`,
  ].filter(Boolean).join(" · ");
  $("worker-slot-split").hidden = !slots || slots.ceiling < 2;
  if (!slots) return;
  $("worker-slot-ceiling").textContent = `This node runs ${slots.ceiling} parallel jobs${slots.host_cpus ? ` on ${slots.host_cpus} CPUs` : ""}. Video and podcast slots together cannot exceed them.`;
  // Never overwrite what is being typed or has been edited and not saved.
  if (!state.slotFormDirty && !$("worker-slot-form").contains(document.activeElement)) {
    $("slot-video").value = slots.split ? slots.video.total : Math.max(1, slots.ceiling - 1);
    $("slot-podcast").value = slots.split ? slots.podcast.total : 1;
    $("slot-video").max = $("slot-podcast").max = Math.max(1, slots.ceiling - 1);
  }
}
// The slot settings are per node: with several workers, Settings edits the
// node picked there.
function workerSlotsPath() {
  return state.slotNode ? `worker-slots?node=${encodeURIComponent(state.slotNode)}` : "worker-slots";
}
// The node Settings edits. With several nodes it is always named, so a save
// goes to the node shown: the one picked, else the first that reports. A
// picked node that stops reporting stays picked, its editors hidden, rather
// than Settings moving to another node under an open edit.
function settingsNode(nodes, live) {
  if (nodes.length < 2) return live[0];
  if (!nodes.some(n => n.name === state.slotNode)) state.slotNode = live[0].name;
  return nodes.find(n => n.name === state.slotNode);
}
function renderNodePicker(nodes, selected) {
  const pick = $("settings-node-pick");
  $("settings-node-pick-label").hidden = nodes.length < 2;
  $("settings-node-title").textContent = nodes.length < 2 ? "Transcode node" : `Transcode node · ${selected.name}`;
  if (nodes.length < 2) return;
  const labels = nodes.map(n => n.available && n.activity ? n.name : `${n.name} (unavailable)`);
  if ([...pick.options].map(o => o.textContent).join() !== labels.join()) pick.replaceChildren(...nodes.map((n, i) => { const option = node("option", labels[i]); option.value = n.name; return option; }));
  pick.value = selected.name;
}
// A picked node that does not report: its settings cannot be read or saved.
function renderUnavailableNode(selected) {
  state.workerSlots = null;
  state.slotFormDirty = state.parallelFormDirty = state.x265FormDirty = false;
  $("settings-node").hidden = false;
  $("worker-parallel-summary").hidden = true;
  $("worker-parallel-form").hidden = true;
  $("worker-x265-row").hidden = true;
  $("worker-slot-split").hidden = true;
  $("worker-slot-usage").hidden = true;
  $("worker-parallel-limit").textContent = `${selected.name} is unavailable. Its settings can be changed once it reports again.`;
}
$("settings-node-pick").addEventListener("change", () => {
  state.slotNode = $("settings-node-pick").value;
  state.slotFormDirty = state.parallelFormDirty = state.x265FormDirty = false;
  $("worker-parallel-note").textContent = $("worker-slot-note").textContent = $("worker-x265-note").textContent = "";
  renderWorkerActivity(state.activityData);
});
async function saveWorkerSlots(video, podcast) {
  const note = $("worker-slot-note");
  note.textContent = "Saving…";
  try {
    const result = await api(workerSlotsPath(), {video_slots: video, podcast_slots: podcast});
    state.slotFormDirty = false;
    const parts = [result.applied ? (video ? "Applied. Takes effect as the next job starts." : "Slots are shared again.") : (result.message || "Saved. The worker has not confirmed yet.")];
    for (const warning of result.warnings || []) parts.push(warning);
    note.textContent = parts.join(" ");
    void refreshWorkerActivity(true);
  } catch (e) {
    note.textContent = e.message;
  }
}
$("worker-slot-form").addEventListener("submit", event => {
  event.preventDefault();
  void saveWorkerSlots(Number($("slot-video").value), Number($("slot-podcast").value));
});
$("worker-slot-share").addEventListener("click", () => void saveWorkerSlots(0, 0));
for (const id of ["slot-video", "slot-podcast"]) $(id).addEventListener("input", () => { state.slotFormDirty = true; });
// How many jobs the node runs at once, in the Transcode node block of
// Settings. Shown whenever the worker is connected; editable when the worker
// reports a limit (older workers only show it).
function renderParallelJobs(activity) {
  const slots = activity.slots, ceiling = slots?.ceiling || activity.worker_slots_total;
  const limit = Number(slots?.ceiling_limit) || 0;
  state.workerSlots = slots || null;
  $("settings-node").hidden = !ceiling;
  $("worker-parallel-summary").hidden = Boolean(limit);
  $("worker-parallel-summary").textContent = String(ceiling);
  $("worker-parallel-form").hidden = !limit;
  $("worker-parallel-limit").textContent = !limit ? "The video worker predates setting this here. Update it to change parallel jobs from this page."
    : `1–${limit} · ${slots.ceiling_source === "override" ? "Set here; overrides the worker config" : "From the worker config"}`;
  // Never overwrite what is being typed or has been edited and not saved.
  if (limit && !state.parallelFormDirty && !$("worker-parallel-form").contains(document.activeElement)) {
    $("parallel-jobs").value = slots.ceiling;
    $("parallel-jobs").max = limit;
  }
}
// How many x265 encodes the node runs at once, next to the parallel jobs. Only
// a worker that reports the x265 pool takes the limit; with one parallel job
// there is nothing to share, so the row stays out of the way until a limit is set.
function renderX265Limit(activity) {
  const slots = activity.slots, pool = slots?.x265;
  const row = $("worker-x265-row"), pick = $("x265-limit");
  row.hidden = !pool || (slots.ceiling < 2 && !pool.total);
  if (row.hidden) { $("worker-x265-note").textContent = ""; return; }
  $("worker-x265-limit").textContent = pool.total
    ? `At most ${pool.total} x265 ${pool.total === 1 ? "encode" : "encodes"} at once · ${pool.used} running now. VideoToolbox encodes use the other slots.`
    : "No limit. x265 encodes can fill every slot.";
  // Never overwrite what is being picked and has not been saved.
  if (state.x265FormDirty || $("worker-x265-form").contains(document.activeElement)) return;
  if (![...pick.options].some(o => o.value === String(pool.total))) {
    const extra = node("option", String(pool.total));
    extra.value = String(pool.total);
    pick.append(extra);
  }
  pick.value = String(pool.total);
}
async function saveX265Limit(limit) {
  const note = $("worker-x265-note");
  note.textContent = "Saving…";
  try {
    // The split to send back is the stored one, not what the worker runs: they
    // differ while the worker has not yet taken a saved split.
    const stored = (await api(workerSlotsPath()))?.settings;
    const result = await api(workerSlotsPath(), {video_slots: stored?.video_slots || 0, podcast_slots: stored?.podcast_slots || 0, x265_slots: limit});
    state.x265FormDirty = false;
    const parts = [result.applied ? "Applied. Takes effect as the next job starts; running jobs are never interrupted." : (result.message || "Saved. The worker has not confirmed yet.")];
    for (const warning of result.warnings || []) parts.push(warning);
    note.textContent = parts.join(" ");
    void refreshWorkerActivity(true);
  } catch (e) {
    note.textContent = e.message;
  }
}
$("worker-x265-form").addEventListener("submit", event => {
  event.preventDefault();
  void saveX265Limit(Number($("x265-limit").value));
});
$("x265-limit").addEventListener("change", () => { state.x265FormDirty = true; });
// The rule a lower ceiling applies to the split (transcode.FitSlotSplit),
// shown before saving.
function parallelSplitNote(ceiling) {
  const slots = state.workerSlots;
  if (!slots?.split || !(ceiling >= 1) || slots.video.total + slots.podcast.total <= ceiling) return "";
  return `The video and podcast split (${slots.video.total} + ${slots.podcast.total}) does not fit ${ceiling} parallel jobs, so all slots will be shared again.`;
}
async function saveParallelJobs(ceiling) {
  const note = $("worker-parallel-note"), slots = state.workerSlots;
  note.textContent = "Saving…";
  try {
    const split = slots?.split ? {video_slots: slots.video.total, podcast_slots: slots.podcast.total} : {video_slots: 0, podcast_slots: 0};
    const result = await api(workerSlotsPath(), {...split, ceiling});
    state.parallelFormDirty = false;
    const parts = [result.applied ? "Applied. Takes effect as the next job starts; running jobs are never interrupted." : (result.message || "Saved. The worker has not confirmed yet.")];
    for (const warning of result.warnings || []) parts.push(warning);
    note.textContent = parts.join(" ");
    void refreshWorkerActivity(true);
  } catch (e) {
    note.textContent = e.message;
  }
}
$("worker-parallel-form").addEventListener("submit", event => {
  event.preventDefault();
  void saveParallelJobs(Number($("parallel-jobs").value));
});
$("parallel-jobs").addEventListener("input", () => {
  state.parallelFormDirty = true;
  $("worker-parallel-note").textContent = parallelSplitNote(Number($("parallel-jobs").value));
});
$("worker-open-settings").addEventListener("click", () => {
  selectTab("advanced");
  // The page changes under the keyboard: land on the section that was asked for.
  $("settings-node-title").focus({preventScroll: false});
});
// Each worker's activity; a server from before workers had names reports one.
function activityNodes(data) {
  if (!data) return [];
  return data.workers || [{name: "", available: data.available, activity: data.activity, message: data.message}];
}
function renderWorkerActivity(data) {
  state.activityData = data;
  $("worker-slots").replaceChildren();
  const nodes = activityNodes(data), live = nodes.filter(n => n.available && n.activity);
  if (!serverReachable || !data?.available || !live.length) {
    $("worker-capacity").textContent = "Worker slots · Unknown";
    $("activity-checked").textContent = "";
    $("worker-slot-usage").hidden = true;
    $("settings-node").hidden = true;
    $("worker-slot-split").hidden = true;
    $("worker-open-settings").hidden = true;
    $("worker-slots").append(node("p",data?.message || "Worker disconnected; slot occupancy is unknown.","metadata"));
    return;
  }
  const used = live.reduce((sum, n) => sum + n.activity.worker_slots_used, 0);
  const total = live.reduce((sum, n) => sum + n.activity.worker_slots_total, 0);
  const missing = nodes.length - live.length;
  $("worker-capacity").textContent = `Worker slots · ${used}/${total} occupied${missing ? ` · ${missing} of ${nodes.length} nodes unavailable` : ""}`;
  $("activity-checked").textContent = "Updated now · Job reservations";
  const selected = settingsNode(nodes, live);
  renderNodePicker(nodes, selected);
  if (selected.available && selected.activity) {
    renderParallelJobs(selected.activity);
    renderX265Limit(selected.activity);
    renderSlotUsage(selected.activity);
  } else {
    renderUnavailableNode(selected);
  }
  // With several nodes each one's split shows under its own heading.
  if (nodes.length > 1) $("worker-slot-usage").hidden = true;
  // Activity only reports; the editors live in Settings.
  $("worker-open-settings").hidden = $("worker-parallel-form").hidden && $("worker-slot-split").hidden;
  const groups = nodes.map(n => ({n, entries: n.available && n.activity ? slotEntries(n.activity, n.name) : []}));
  // Phones get a row per node that opens onto its slots; wide screens get a
  // table of what is running. Both are in the page and the stylesheet picks one.
  const compact = node("div","","slots-compact");
  compact.append(...groups.map(({n, entries}) => slotNodeDetails(n, entries)));
  $("worker-slots").append(compact, slotTable(groups));
}
// Phases where the worker has no percentage to give: it is still getting the
// file ready, or (pass 1 of a two-pass x265) analysing it. The progress file
// written by the encode keeps its last value afterwards, so a number shown in
// these phases or after the encode would be stale, not live.
const slotUnmeasuredPhases = ["queued", "waiting_for_worker", "preparing", "reading_source", "probing_source", "analysing_video"];
const slotShortPhases = {
  queued: "Waiting", waiting_for_worker: "Waiting", preparing: "Preparing", reading_source: "Reading", probing_source: "Inspecting",
  analysing_video: "Analyzing", encoding: "Encoding", validating: "Verifying", validating_quality: "Measuring quality",
  publishing: "Saving", publication_pending: "Saving", completed: "Finished",
};
// The one line under a slot's bar. Short on purpose: every busy slot shows the
// same four rows, so cards stay the same size whatever phase they are in.
const slotPhaseNotes = {
  queued: "Waiting for a free slot", waiting_for_worker: "Waiting for a free slot", preparing: "Getting the file ready", reading_source: "Reading the source",
  probing_source: "Inspecting the file", analysing_video: "Pass 1 of 2 · no percentage yet", validating: "Encode finished · verifying",
  validating_quality: "Encode finished · measuring quality", publishing: "Encode finished · saving", publication_pending: "Encode finished · saving", completed: "Finished",
};
// The encode and its speed while encoding (x265 CRF 22 · 4.94×); what the worker
// is busy with in every other phase. ffmpeg's speed is a running average, so it
// is left out once the worker goes quiet.
function slotFacts(job, stale) {
  if (stale) return {text: "No recent worker heartbeat", title: "No recent worker heartbeat. Showing the last reported state."};
  const phase = job.phase, speed = Number(job.speed), fps = Number(job.fps), last = activityElapsed(job.last_progress_at);
  const parts = [job.encoding || null];
  if (phase === "encoding") {
    if (speed > 0) parts.push(`${speed.toFixed(2)}×`);
  } else parts.push(slotPhaseNotes[phase] || workerPhaseLabel(phase));
  const text = parts.filter(Boolean).join(" · ") || (last ? `Last advance ${last} ago` : "Waiting for the first update");
  const detail = [job.encoding, phase === "encoding" && fps > 0 ? `${fps.toFixed(1)} fps` : null, last ? `Last advance ${last} ago` : null].filter(Boolean);
  return {text, title: [text, ...detail.filter(part => !text.includes(part))].join(" · ")};
}
// The slot's progress in the same green bar the batch rows use: a measured
// percentage while encoding, an animated bar with no number while the worker
// is busy in a phase that has none, and a still, muted one when it has gone quiet.
function slotProgressParts(job, alive) {
  const phase = job.phase, value = job.progress != null ? Number(job.progress) : NaN;
  const measured = Number.isFinite(value) && !postEncodeWorkerPhases.includes(phase) && !slotUnmeasuredPhases.includes(phase);
  const label = slotShortPhases[phase] || workerPhaseLabel(phase);
  let percent = null, bar;
  if (measured) {
    percent = Math.max(0, Math.min(100, value));
    bar = document.createElement("progress");
    bar.max = 100;
    bar.value = percent;
    bar.setAttribute("aria-label", `${label} ${percent.toFixed(1)}%`);
  } else {
    bar = node("span", "", "slot-progress-bar");
    bar.setAttribute("role", "progressbar");
    bar.setAttribute("aria-label", `${label}, no percentage available`);
  }
  return {label, percent, bar};
}
function slotProgressBlock(job, alive) {
  const {label, percent, bar} = slotProgressParts(job, alive);
  const block = node("div", "", `job-progress batch-progress slot-progress${alive ? "" : " stale"}`);
  block.append(node("span", label, "batch-progress-count"));
  if (percent != null) block.append(node("strong", `${percent.toFixed(1)}%`, "batch-progress-percent"));
  block.append(bar);
  return block;
}
// The table keeps the phase and the bar in cells of their own: the bars of
// every row start and end in the same place however long a phase name is.
function slotProgressCells(job, alive) {
  const {label, percent, bar} = slotProgressParts(job, alive);
  const phase = node("td", "", `slot-phase${alive ? "" : " stale"}`);
  phase.append(node("span", label, "slot-phase-name"));
  const cell = node("td", "", `slot-bar${alive ? "" : " stale"}`);
  const track = node("div", "", "slot-bar-track");
  track.append(bar);
  cell.append(track, node("span", percent != null ? `${percent.toFixed(1)}%` : "", "slot-bar-percent"));
  return [phase, cell];
}
// One node's slots. Each node keeps its own slot positions. An entry is a free
// slot, a slot the worker says is busy without saying with what (an older
// worker), or a slot with its job.
function slotEntries(activity, name) {
  const jobs = activity.jobs || [];
  const pools = slotPools(activity);
  const shown = Math.min(16, pools.total);
  // Keep a live reservation in the same visual slot as other jobs finish.
  // Slot labels describe this node's reservations, not physical CPU cores.
  const byNode = state.activitySlotIDs ||= new Map();
  if (!byNode.has(name)) byNode.set(name, new Map());
  const assigned = byNode.get(name);
  const live = new Set(jobs.map(job => job.id));
  for (const [id,slot] of assigned) if (!live.has(id) || slot >= pools.total) assigned.delete(id);
  for (const job of jobs) if (!assigned.has(job.id)) {
    const used = new Set(assigned.values());
    const [from, to] = pools.range(job.kind);
    // A job beyond its pool (the split was lowered under it) takes any free slot.
    for (const [lo, hi] of [[from, to], [0, pools.total]]) {
      let found = false;
      for (let slot=lo;slot<Math.min(16,hi);slot++) if (!used.has(slot)) { assigned.set(job.id,slot); found = true; break; }
      if (found) break;
    }
  }
  const entries = [];
  for (let i=0;i<shown;i++) {
    const job = jobs.find(job => assigned.get(job.id) === i);
    entries.push({label: pools.label(i), job, unknown: !job && !activity.details_available && i < activity.worker_slots_used});
  }
  return entries;
}
// The encoder a full conversion runs, from the encode the action gave it
// ("x265 CRF 22", "VideoToolbox quality 65"); unknown for a job without one.
const slotEncoder = job => /x265/i.test(job.encoding || "") ? "x265" : /videotoolbox/i.test(job.encoding || "") ? "VideoToolbox" : "";
const slotKind = job => job.kind === "podcast" ? "Podcast processing" : ["Full conversion", slotEncoder(job)].filter(Boolean).join(" · ");
// Phone layout: a free slot is one line, a busy one the file, its bar and one
// line of facts. Everything about the job is on the row; nothing needs a tap.
function slotRow(entry) {
  const {label, job} = entry;
  const row = node("div","",`worker-slot${job || entry.unknown ? "" : " idle"}`);
  if (!job) {
    row.append(node("strong",`${label} · ${entry.unknown ? "Occupied" : "Available"}`));
    if (entry.unknown) row.append(node("p","File details require the updated video worker.","metadata"));
    return row;
  }
  const stale = !heartbeatAlive(workerHeartbeatState(job.heartbeat_at));
  row.append(node("strong",`${label} · ${job.file}`));
  row.children[0].title = job.file;
  row.append(slotProgressBlock(job, !stale));
  const facts = slotFacts(job, stale), elapsed = activityElapsed(job.started_at);
  const line = node("p", [facts.text, slotKind(job), elapsed ? `${elapsed} elapsed` : null].filter(Boolean).join(" · "), `metadata slot-facts${stale ? " warn" : ""}`);
  line.title = facts.title;
  row.append(line);
  return row;
}
// One node as a row that opens onto its slots. Open while something runs or is
// wrong, shut when it is idle, until the person opens or shuts it themselves.
function slotNodeDetails(n, entries) {
  const online = Boolean(n.available && n.activity);
  const busy = entries.filter(e => e.job || e.unknown).length;
  const stale = entries.some(e => e.job && !heartbeatAlive(workerHeartbeatState(e.job.heartbeat_at)));
  const key = n.name || "", chosen = state.slotNodeOpen?.get(key);
  const details = node("details","","slot-node");
  details.open = chosen ?? (!online || busy > 0);
  details.dataset.state = !online ? "down" : stale ? "stale" : busy ? "busy" : "idle";
  const summary = node("summary");
  const split = n.activity?.slots?.split ? n.activity.slots : null;
  summary.append(node("span","","slot-dot"), node("strong", n.name || "Worker"),
    node("span", !online ? n.message || "Unavailable" : `${busy}/${n.activity.worker_slots_total}${busy ? "" : " · all available"}`, "metadata"));
  if (online && split) summary.append(node("span",`Video ${split.video.used}/${split.video.total} · Podcast ${split.podcast.used}/${split.podcast.total}`,"metadata slot-split"));
  summary.append(node("span","","slot-chevron"));
  // The click runs before the details toggles: the new state is the opposite.
  summary.addEventListener("click", () => { (state.slotNodeOpen ||= new Map()).set(key, !details.open); });
  const body = node("div","","slot-node-body");
  body.append(...entries.map(slotRow));
  details.append(summary, body);
  return details;
}
// Desktop layout: the slots that are doing something as a table, the rest in a line.
function slotTable(groups) {
  const wrap = node("div","","slots-table");
  const multi = groups.length > 1, busy = [], free = [], down = [];
  for (const {n, entries} of groups) {
    if (!(n.available && n.activity)) { down.push(`${n.name} · ${n.message || "Unavailable"}`); continue; }
    const idle = entries.filter(e => !e.job && !e.unknown).length;
    if (idle) free.push([n.name, idle]);
    for (const e of entries) if (e.job || e.unknown) busy.push({n, e});
  }
  if (busy.length) {
    const table = node("table"), head = node("tr");
    for (const title of ["Slot", "File", "Phase", "Progress", "Kind", "Elapsed"]) head.append(node("th", title));
    const thead = node("thead");
    thead.append(head);
    const tbody = node("tbody");
    for (const {n, e} of busy) {
      const row = node("tr"), slot = multi ? `${n.name} · ${e.label}` : e.label;
      row.append(node("td", slot));
      if (!e.job) {
        const note = node("td", "Occupied · File details require the updated video worker.", "metadata");
        note.colSpan = 5;
        row.append(note);
      } else {
        const job = e.job, stale = !heartbeatAlive(workerHeartbeatState(job.heartbeat_at)), facts = slotFacts(job, stale);
        const file = node("td", "", "slot-file"), [phase, bar] = slotProgressCells(job, !stale);
        file.title = job.file;
        // A file whose job is known opens that job's details.
        if (job.action_id) {
          const open = node("button", job.file, "slot-file-link");
          open.type = "button";
          open.addEventListener("click", () => safe(() => openJob(job.action_id)));
          file.append(open);
        } else file.textContent = job.file;
        // What the worker is doing sits under the phase's name: encoder and speed while encoding, a short note otherwise.
        const factsLine = node("span", facts.text, stale ? "slot-facts warn" : "slot-facts");
        factsLine.title = facts.title;
        phase.append(factsLine);
        row.append(file, phase, bar, node("td", slotKind(job)), node("td", activityElapsed(job.started_at) || ""));
      }
      tbody.append(row);
    }
    table.append(thead, tbody);
    wrap.append(table);
  }
  const freeCount = free.reduce((sum, [, count]) => sum + count, 0);
  if (freeCount) wrap.append(node("p", `${freeCount} ${freeCount === 1 ? "slot" : "slots"} available${multi ? ` · ${free.map(([name, count]) => `${name} ${count}`).join(", ")}` : ""}`, "metadata slot-idle-line"));
  for (const text of down) wrap.append(node("p", text, "metadata slot-down"));
  return wrap;
}
async function refreshWorkerActivity(force) {
  if (state.activityRequest || (!force && $("workspace").hidden)) return;
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
$("storage-refresh").addEventListener("click", () => void safe(measureStorage));
// A chosen filter lights the filter button, since the select itself is only an icon.
function markQueueFilter() {
  $("job-filter-pick").classList.toggle("on", $("job-filter").value !== "all");
}
$("job-filter").addEventListener("change", () => {
  markQueueFilter();
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
  // A batch only keeps its own bookkeeping here (listing the files, saving the
  // results); each file's encode times are in that file's details.
  if (job.batch) return;
  const entries = Object.entries(job.phase_costs || {});
  if (!entries.length) return;
  const detail = node("details", "", "phase-costs detail-row");
  detail.open = Boolean(state.detailCostsOpen);
  detail.addEventListener("toggle", () => { state.detailCostsOpen = detail.open; });
  detail.append(node("summary", "Where the time went"));
  detail.append(node("p", "Time spent in each part of this file's work. Parts can overlap, so the rows are not a total. CPU time is only shown where the worker measured it.", "metadata"));
  const table = node("table");
  const header = node("tr");
  for (const label of ["Phase", "Wall time", "CPU time", "Invocations", "NAS read", "NAS written", "Cache hits / misses"]) header.append(node("th", label));
  table.append(header);
  const labels = {coordinator_inventory:"Coordinator · Inventory",coordinator_preflight:"Coordinator · Preflight",coordinator_source_hash:"Coordinator · Source hash",coordinator_probe:"Coordinator · Probe",coordinator_promotion:"Coordinator · Promotion"};
  for (const [phase, cost] of entries) {
    const row = node("tr");
    const label = labels[phase] || phase.replace(/^worker_benchmark_/, "Worker samples · ").replace(/^worker_/, "Worker · ").replaceAll("_", " ");
    row.append(node("td", label), node("td", Number.isFinite(cost.duration_ms) ? `${cost.duration_ms} ms` : "Unknown"), node("td", Number.isFinite(cost.active_compute_ms) ? `${cost.active_compute_ms} ms` : "Unknown"), node("td", Number.isFinite(cost.attempts) ? String(cost.attempts) : "Unknown"), node("td", cost.nas_read_bytes == null ? "Unknown" : bytes(cost.nas_read_bytes)), node("td", cost.nas_written_bytes == null ? "Unknown" : bytes(cost.nas_written_bytes)), node("td", `${cost.cache_hits == null ? "Unknown" : cost.cache_hits} / ${cost.cache_misses == null ? "Unknown" : cost.cache_misses}`));
    table.append(row);
  }
  detail.append(table); host.append(detail);
}
// ---- The job dialog ----
// Five sections in one fixed order: header, status, files, details and actions.
// A state changes what a section says, never which sections exist or where
// they sit; a section with nothing to say is hidden, not left as a gap.
//
// Every progress bar is the same component: bar() below. Text that stays on
// one line says so (data-single-line) and carries its full text as a tooltip.
function bar(fraction, label, kind = "hero") {
  const percent = Math.round(Math.max(0, Math.min(1, Number.isFinite(fraction) ? fraction : 0)) * 1000) / 10;
  const track = node("div", "", "bar");
  track.dataset.bar = kind;
  track.setAttribute("role", "progressbar");
  track.setAttribute("aria-label", label);
  track.setAttribute("aria-valuemin", "0");
  track.setAttribute("aria-valuemax", "100");
  track.setAttribute("aria-valuenow", String(percent));
  const fill = node("i");
  fill.dataset.fill = "";
  fill.style.width = `${percent}%`;
  track.append(fill);
  return track;
}
function oneLine(text, className = "") {
  const line = node("p", text, className);
  line.dataset.singleLine = "";
  line.title = text;
  return line;
}
function dotLine(tone, text, title = "") {
  const line = node("p", "", "jd-line jd-live");
  const dot = node("span", "", `jd-dot ${tone}`);
  dot.dataset.dot = "";
  const label = node("span", text);
  label.dataset.singleLine = "";
  if (title) line.title = title;
  line.append(dot, label);
  return line;
}
// A progress model laid out for the dialog: one headline, one bar, then lines.
function detailHero(model, status = "") {
  if (!model) return null;
  const stopped = /\b(failed|cancelled)\b/.test(model.className);
  const hero = node("div", "", `jd-hero hero-${model.kind}${stopped ? " stopped" : ""}`);
  const gauge = model.parts.find(part => ["meter", "progress", "segments"].includes(part.type));
  const fraction = !gauge ? null : gauge.fraction ?? (gauge.value - (gauge.min || 0)) / ((gauge.max - (gauge.min || 0)) || 1);
  const texts = model.parts.filter(part => part.type === "text" && part.text.trim());
  const find = name => texts.find(part => part.cls.split(" ").includes(name));
  let headline = "", rest = texts;
  if (model.kind === "batch") {
    const percent = find("batch-progress-percent"), count = find("batch-progress-count");
    headline = [percent?.text, count?.text].filter(Boolean).join(" · ");
    rest = texts.filter(part => part !== percent && part !== count);
    // A segmented bar's legend is the count of each state.
    const legend = find("batch-legend");
    if (legend) { headline = [percent?.text, legend.text].filter(Boolean).join(" · "); rest = rest.filter(part => part !== legend); }
  } else if (model.kind === "replacement" || model.kind === "file") {
    headline = texts[0]?.text || "";
    rest = texts.slice(1);
  } else {
    const unit = texts.find(part => part.unit);
    const first = unit || texts[0];
    headline = first?.text || "";
    rest = texts.filter(part => part !== first);
    // The measurement leads when there is one; otherwise the first line does,
    // unless it is a sentence that reads better as a note.
    if (!unit && first?.sentence) { headline = ""; rest = texts; }
  }
  // The chip above already names the phase; the headline keeps only what it adds.
  const lead = `${status} \u00b7 `;
  if (status && headline.toLowerCase().startsWith(lead.toLowerCase())) headline = headline.slice(lead.length);
  if (headline) hero.append(oneLine(headline, "jd-headline"));
  if (gauge) hero.append(bar(fraction, gauge.label || gauge.title || "Progress"));
  for (const part of rest) {
    const line = part.sentence ? node("p", part.text, "jd-line jd-sentence") : oneLine(part.text, "jd-line");
    hero.append(line);
  }
  for (const part of model.parts) if (part.type === "dot") hero.append(dotLine(heartbeatAlive(part.state) ? "ok" : part.state === "delayed" ? "warn" : "bad", part.text, part.title));
  return hero;
}
// Everything the status section says about a job, in one order: the progress,
// the step, the counts or facts, then the sentences that explain the outcome.
const saidText = text => text.toLowerCase().replace(/[·•:,.]/g, " ").replace(/\s+/g, " ").trim();
// What is left of a sentence once the parts the status block already says are
// taken out: a chip that reads "Candidates ready" makes "6 candidates ready" noise.
function unsaid(text, said) {
  return text.split(/\s·\s/).filter(part => {
    const n = saidText(part);
    return n && !["file", "batch"].includes(n) && !said.some(s => s === n || (n.length > 8 && s.includes(n)) || (s.length > 8 && n.includes(s)));
  }).join(" · ");
}
function detailStatusNodes(job, presentation) {
  const out = [];
  // Everything the block has said so far, so a later line never says it again.
  const said = [saidText(presentation.status)];
  const facts = detailFacts(job);
  const model = presentation.showTelemetry ? detailProgressModel(job) : batchProgressModel(job);
  const hero = detailHero(model, presentation.status);
  if (hero) {
    out.push(hero);
    for (const line of hero.children) if (line.textContent) said.push(saidText(line.textContent));
  }
  const step = stepText(job);
  if (step) {
    out.push(oneLine(step, "jd-line jd-step"));
    said.push(saidText(step));
  }
  if (job.batch?.total > 1 && !job.batch.dry_run) {
    // An approved replacement says replaced and failed in its own headline; a chip
    // of completed encodes beside "1 failed" would contradict it, so only the
    // other counts (skipped, rejected, ...) stay.
    const holder = node("div");
    renderBatchCounts(holder, visibleBatch(job), job.replacement?.approved ? ["completed", "failed"] : []);
    for (const badge of holder.children[0]?.children || []) said.push(saidText(badge.textContent));
    out.push(...holder.children);
  }
  if (facts.length) {
    const chips = node("ul", "", "detail-facts");
    for (const fact of facts) { chips.append(node("li", fact, "detail-fact")); said.push(saidText(fact)); }
    out.push(chips);
  }
  const notes = [];
  const note = (text, className = "") => {
    const rest = unsaid(text, said);
    if (!rest) return;
    said.push(saidText(rest));
    notes.push(node("p", rest, `jd-note ${className}`.trim()));
  };
  // A failed replacement's headline already is the summary.
  const headline = model?.kind === "replacement" ? model.parts[0].text : "";
  const repeated = Boolean(headline) && presentation.summary.includes(headline);
  if (!repeated && !(presentation.showTelemetry && facts.length && job.action_name === "transcode_media")) note(presentation.summary, "muted");
  const addOutcome = (text) => {
    // "Open job for details" is the queue row's pointer to this dialog.
    if (!text || /^(Open job for error details|Finished · Open job for details)$/.test(text)) return;
    note(text, "job-outcome metadata");
  };
  if (job.podcast) { const p = job.podcast; note(`${p.classified_blocks || 0}/${p.total_blocks || 0} transcript blocks classified · ${((p.removed_ms || 0)/1000).toFixed(1)} seconds removed · ${p.known_ad_units || 0} units verified by known ads`, "metadata"); if (p.output_path && job.status === "completed") note(`Validated MP3: ${p.output_path}`, "metadata"); }
  if (job.transcode) {
    // A waiting decision repeats its reason below, and a result card
    // explains the outcome itself; show it only once.
    const hasReport = hasResultReport(job.benchmark);
    if (job.status !== "waiting_decision" && !hasReport && !(presentation.showTelemetry && ladderStep(job))) addOutcome(jobOutcomeText(job));
    if (job.transcode.tested_candidates) note(`${job.transcode.tested_candidates} configurations tested${job.transcode.verified_samples ? ` · ${job.transcode.verified_samples} quality samples verified` : ""}`, "metadata");
  }
  const report = resultReport(job);
  if (report) notes.push(report);
  const tracks = trackReport(job);
  if (tracks) notes.push(tracks);
  if (!presentation.showTelemetry && !model && !job.batch?.dry_run && job.batch?.outcome !== "no_changes") addOutcome(queueOutcome(job));
  if (job.batch?.outcome === "no_changes") notes.push(skippedBatchOutcome(job));
  if (job.batch?.dry_run) note(queueOutcome(job) + (job.batch.queued > 0 && !job.preview_execution_action_id ? ". Review the eligible files, then choose Start batch to convert them." : "."), "preview-notice");
  const raw = job.status === "waiting_decision" ? job.waiting_reason : job.status === "failed" ? job.error : "";
  if (raw && shortJobReason(raw) !== raw && !workerBusyFailure(raw)) {
    const technical = node("details", "", "technical-reason");
    technical.append(node("summary", "Technical details"), node("p", raw, "metadata"));
    notes.push(technical);
  }
  if (visibleBatch(job)?.failed && job.batch_files?.reasons?.length) {
    const reason = job.batch_files.reasons[0];
    note(`${batchReasonLabel(job,reason)}: ${shortJobReason(reason.reason)}`, "muted");
  }
  out.push(...notes);
  return out;
}
// A section whose rows are all hidden is hidden too.
function syncDetailSections() {
  const rows = ["detail-related", "detail-commands", "detail-row-plan", "detail-row-steps", "detail-costs", "detail-row-path", "detail-row-log"];
  $("detail-rows").hidden = !rows.some(key => !$(key).hidden && $(key).children.length > 0);
}
async function openJob(id) {
  // The same job opened again (Back, a link to itself) keeps what it shows.
  const reopened = state.detail === id && $("job-detail").open;
  // Read before any row is cleared: removing the focused control sends focus
  // to the page. Only an already open dialog counts, so showModal keeps its
  // own initial focus.
  const hadFocus = $("job-detail").open && $("job-detail").contains?.(document.activeElement) && document.activeElement !== $("job-detail-title");
  state.detail = id;
  state.chunk = 0;
  state.stepOffset = 0;
  state.batchItemsOffset = 0;
  state.batchItemsPaging = null;
  state.batchItemsHasMore = false;
  $("batch-items-panel").hidden = true;
  state.detailRevision++;
  // A new job starts with every row closed; failures open the relevant one.
  state.detailAutoOpened = new Set();
  state.openFiles = new Set();
  state.detailCostsOpen = false;
  if (!$("job-detail").open) $("job-detail").showModal();
  writeNavigation();
  $("job-detail-title").textContent = "Job details";
  $("job-detail-title").title = "Job details";
  $("copy-detail-path").hidden = true;
  $("detail-status").replaceChildren();
  $("detail-summary").replaceChildren(node("p", "Loading job…", "muted"));
  $("detail-controls").replaceChildren();
  $("detail-controls").hidden = true;
  $("detail-related").replaceChildren();
  // Nothing of the job that was open before may show while this one loads: its
  // files, its command history, its details.
  if (!reopened) {
    $("detail-command-announcer").textContent = "";
    delete $("detail-summary").dataset.operationId;
    state.detailJob = null;
    $("batch-items-list").replaceChildren();
    $("batch-items-note").textContent = "Loading files…";
    $("batch-items-prev").hidden = $("batch-items-next").hidden = true;
    $("detail-command-list").replaceChildren();
    $("detail-commands").hidden = true;
  }
  resetDetailRows();
  syncDetailSections();
  // The control that opened this job was cleared above: focus goes to the
  // title instead of falling to the page.
  if (hadFocus) $("job-detail-title").focus?.();
  // The list already holds this job: show it at once and let the server's
  // answer correct it, instead of a blank "Loading job…".
  const known = [...(state.operationJobs?.values() || [])].find(job => job.id === id);
  if (known) paintDetail(known);
  await refreshDetail();
}
// Paints a job into the open dialog. The same blocks, in the same order, whether
// the job came from the list a moment ago or from the server just now.
function paintDetail(job) {
  state.detailJob = job;
  const focus = focusedControl($("detail-controls"));
  const menu = openActionMenu($("detail-controls"));
  const presentation = queuePresentation(job);
  const summary = $("detail-summary");
  const path = detailPath(job);
  const title = job.batch || job.source_path || job.promotion ? presentation.title : job.id;
  $("job-detail-title").textContent = title;
  $("job-detail-title").title = path || title;
  $("copy-detail-path").hidden = !path;
  $("detail-status").replaceChildren(statusBadge(presentation.status));
  summary.replaceChildren(...detailStatusNodes(job, presentation));
  summary.dataset.operationId = job.id;
  renderCommandIndicators();
  renderDetailRows(job);
  const related = $("detail-related");
  related.replaceChildren();
  for (const action of job.workflow_actions || []) {
    if (action.id === job.id) continue;
    related.append(button(`${workflows[action.action_name] || action.action_name} · ${names[action.status] || action.status}`, () => openJob(action.id), "quiet"));
  }
  syncDetailSections();
  const controls = $("detail-controls");
  controls.replaceChildren(...detailActionBar(job));
  controls.hidden = !controls.children.length;
  $("batch-items-panel").hidden = !job.batch;
  return {focus, menu};
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
    const {focus, menu} = paintDetail(job);
    const controls = $("detail-controls");
    if (job.batch) await loadBatchItems(id, revision, job.batch.total);
    restoreControl(controls, focus);
    restoreActionMenu(controls, menu);
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
      $("detail-controls").hidden = true;
      $("detail-related").replaceChildren();
      $("batch-items-panel").hidden = true;
      resetDetailRows();
      syncDetailSections();
    }
  }
}
// The action bar: at most one filled main action, one quiet second action and
// a More menu for the rest, so a state never shows a wall of buttons.
const detailSecondaryActions = ["Keep originals", "Keep original", "Pause batch", "Cancel", "Stop batch · keep originals", "View batch", "Change settings", "Change goal", "Show in Files", "View replacement"];
const destructiveActions = ["Cancel", "Stop batch · keep originals"];
function detailActionBar(job) {
  const buttons = [...jobControls(job, true).children];
  if (!buttons.length) return [];
  const labelOf = b => b.textContent.trim();
  const primary = mainQueueActions.map(label => buttons.find(b => labelOf(b) === label)).find(Boolean) || null;
  const rank = b => { const i = detailSecondaryActions.indexOf(labelOf(b)); return i < 0 ? detailSecondaryActions.length : i; };
  const rest = buttons.filter(b => b !== primary).sort((a, b) => rank(a) - rank(b));
  const ordered = [primary, ...rest].filter(Boolean);
  const shown = ordered.length <= 2 ? ordered : ordered.slice(0, 2);
  const overflow = ordered.slice(shown.length);
  for (const b of buttons) {
    b.setAttribute("aria-label", labelOf(b));
    b.title ||= labelOf(b);
  }
  for (const b of shown) {
    b.querySelector?.("svg")?.remove?.();
    b.className = `${b === primary ? "jd-primary" : "jd-secondary"}${destructiveActions.includes(labelOf(b)) ? " jd-danger" : ""}`;
  }
  for (const b of overflow) {
    b.querySelector?.("svg")?.remove?.();
    b.className = `jd-menu-item${destructiveActions.includes(labelOf(b)) ? " jd-danger" : ""}`;
  }
  return [...shown, ...(overflow.length ? [moreActionsMenu(overflow, job.id)] : [])];
}
function batchFileActivity(item, job) {
  if (job?.batch?.dry_run) return "";
  if (item.retry_pending) return "New settings saved · Current attempt finishes before the next attempt starts";
  if (item.status === "queued" && !item.child_action_id) {
    if (job?.paused) return "Batch paused · Resume the batch to start this file";
    if (job?.status === "waiting_decision") return "Waiting for the batch decision before starting this file";
    if (["failed", "cancelled"].includes(job?.status)) return "Batch stopped · This file has not started";
    return "";
  }
  const activity = job?.activities?.find(activity => activity.id === item.child_action_id);
  if (activity?.waiting_condition === "worker_unreachable") return "Worker unreachable · Waiting for connection";
  const worker = activity?.worker;
  const phase = worker?.transcode_phase || worker?.phase;
  // Cleared once the worker accepts the job, so it never hides worker progress.
  if (activity?.work?.phase === "waiting_for_worker") return workerSlotWaitLabel(activity.work);
  const after = activity && postEncodeWork({...activity, status: item.status});
  if (after) {
    const observation = workerProgressObservation({...activity, status: item.status});
    return postEncodeWorkLabel(after) + (observation.message ? ` · ${observation.message}` : "");
  }
  if (phase) {
    const observation = workerProgressObservation({...activity, status: item.status});
    const eta = observation.message ? "" : encodeEta(activity.id, worker, activity.eta_seconds);
    return workerPhaseLabel(phase) + (observation.message ? ` · ${observation.message}` : eta ? ` · ${eta}` : "");
  }
  if (activity?.work?.phase === "waiting_for_hash_slot") return "Waiting to verify source";
  if (activity?.work?.phase === "hashing_source") return `Verifying source${activity.work.total_bytes > 0 ? ` · ${bytes(activity.work.bytes_read || 0)} / ${bytes(activity.work.total_bytes)}` : ""}`;
  if (activity?.work?.phase) return workerPhaseLabel(activity.work.phase);
  if (item.status === "waiting_for_slot") return "Waiting for an available worker slot";
  if (item.status === "queued" && item.child_action_id) return "File task created · Waiting to start";
  return "";
}

async function replaceBatchFile(batchID, childID) {
  const auth = state.authRevision;
  const job = (await api(`operations?id=${encodeURIComponent(childID)}`)).jobs?.[0];
  if (auth !== state.authRevision || $("workspace").hidden) return;
  if (!job || job.parent_action_id !== batchID || (!job.candidate_ready && !job.replacement_action_id)) throw new Error("This candidate changed. Refresh the batch before reviewing replacement.");
  await prepareReplacement(job);
}
// What a replacement Retry does. Retrying through the batch re-runs every failed
// file that stopped before approval, so the button says how many; a file's own
// retry re-runs just that file.
function retryReplacementLabel(item, batchID, name) {
  const count = item.retry_action_id === batchID ? state.detailJob?.replacement?.batch_retry_count || 0 : 0;
  return count > 1 ? {button: `Retry ${count} failed replacements`, confirm: `Retry ${count} failed replacements`} : {button: "Retry replacement", confirm: `Retry replacement of ${name}`};
}
// Tries a failed file's replacement again. The server re-checks it against the
// plan that was approved and does not encode again.
async function retryFileReplacement(item, batchID) {
  const name = item.display_label || (item.file_path || "").split("/").pop();
  await jobControl(item.retry_action_id, "action_retry", {}, {decision: {label: retryReplacementLabel(item, batchID, name).confirm, verb: "Retrying replacement", failure: "Retry failed"}});
}
// One row per file. Collapsed it is a single control showing the name, what
// the file saves and where it stands; opening it shows the one-sentence story
// and every action for that file, so a long batch is a list, not a wall of
// buttons. Which rows are open survives each refresh (state.openFiles).
const fileStateLabel = (item, job) => {
  const queued = item.replacement_plan === "queued";
  return item.replaced ? "Replaced" : queued ? "Replacement queued (approved)" : item.replacement_plan === "review" ? "Replacement awaiting approval" : item.replacement_action_id ? ({waiting_decision:"Replacement needs decision",failed:"Replacement failed",cancelled:"Replacement cancelled"}[item.replacement_status] || "Replacement in progress") : item.candidate_ready ? "Candidate ready" : item.status === "waiting_decision" && item.reason_code === "search_budget_exhausted" ? "Needs attention" : savingsPolicyFailure(item.error) ? "Below minimum savings" : rejectedCandidate(item) ? "Rejected" : job?.batch?.dry_run && item.status === "queued" ? "Eligible" : names[item.status] || item.status || item.decision;
};
function batchFileRow(item, id) {
  const job = state.detailJob;
  const member = job?.replacement?.members?.find(m => m.item_key === item.item_key);
  const queued = item.replacement_plan === "queued";
  const failure = item.replacement_failure;
  const label = fileStateLabel(item, job);
  const fileName = item.display_label || (item.file_path || "").split("/").pop();
  // A row that needs the person opens itself once per job, so "Try again" is
  // never hidden behind a tap; closing it afterwards sticks.
  const needsYou = Boolean(failure || item.retry_action_id || ["failed", "waiting_decision"].includes(item.status));
  const autoKey = `file:${item.item_key}`;
  if (needsYou && !state.detailAutoOpened.has(autoKey)) {
    state.detailAutoOpened.add(autoKey);
    state.openFiles.add(item.item_key);
  }
  const open = state.openFiles.has(item.item_key);
  const row = node("div", "", "jd-file");
  row.setAttribute("role", "listitem");
  row.dataset.fileRow = "";
  row.dataset.open = String(open);
  row.dataset.itemKey = item.item_key;
  const bodyId = `file-body-${String(item.item_key).replace(/[^A-Za-z0-9-]/g, c => `_${c.charCodeAt(0).toString(16)}_`)}`;
  const head = node("button", "", "jd-file-head");
  head.type = "button";
  head.setAttribute("aria-expanded", String(open));
  head.setAttribute("aria-controls", bodyId);
  head.dataset.fileToggle = item.item_key;
  head.title = fileName;
  const name = node("span", fileName, "jd-file-name");
  name.dataset.singleLine = "";
  const meta = node("span", "", "jd-file-meta");
  meta.dataset.singleLine = "";
  const badge = statusBadge(label, item.replacement_status || label);
  meta.append(badge);
  if (item.saved_bytes > 0) meta.append(node("span", `−${bytes(item.saved_bytes)}`, "jd-file-saved"));
  meta.title = [label, item.saved_bytes > 0 ? `Smaller than the original by ${bytes(item.saved_bytes)}` : ""].filter(Boolean).join(" · ");
  const chevron = node("span", "", "jd-chevron");
  chevron.setAttribute("aria-hidden", "true");
  head.append(name, meta, chevron);
  const body = node("div", "", "jd-file-body");
  body.id = bodyId;
  body.dataset.rowBody = "";
  body.hidden = !open;
  head.addEventListener("click", () => {
    const next = body.hidden;
    body.hidden = !next;
    row.dataset.open = String(next);
    head.setAttribute("aria-expanded", String(next));
    if (next) state.openFiles.add(item.item_key); else state.openFiles.delete(item.item_key);
  });
  row.append(head, body);

  const review = item.replacement_action_id ? () => openJob(item.replacement_action_id) : item.status === "waiting_decision" && item.child_action_id ? () => openJob(item.child_action_id) : ["skip","review"].includes(item.status) && item.can_reconfigure ? () => reconfigureBatch(id, false, {item_key:item.item_key}) : null;
  const activity = failure || queued ? "" : item.replacement_action_id && member ? replacementStepText(member) : batchFileActivity(item, job);
  if (activity) body.append(node("p", activity, "jd-note batch-file-activity"));

  const actions = node("div", "", "jd-file-actions");
  // Buttons that only open something stay usable while the batch is busy.
  const add = (text, fn, {navigation = false, target = id, primary = text === "Replace file", requiresWorker = false, into = actions} = {}) => {
    const control = button(text, fn, primary ? "jd-primary" : "jd-secondary");
    control.dataset.jobControl = target;
    if (requiresWorker) control.dataset.requiresWorker = "true";
    control.dataset.batchItemKey = item.item_key;
    if (navigation) control.dataset.navigation = "true";
    control.disabled = jobControlDisabled(control);
    into.append(control);
  };
  if (item.candidate_ready && !item.replacement_plan && state.info.allow_destructive) add("Replace file", () => replaceBatchFile(id, item.child_action_id));
  // The encode finished and only saving it failed: one button tries the same
  // file again, on the worker that holds it. No settings change, no re-encode.
  if (item.status === "waiting_decision" && item.child_action_id && item.waiting_decisions?.includes("recheck")) {
    add("Try again", () => jobControl(item.child_action_id, "action_resume", {decision: "recheck"}, {decision: {label: "Try again", verb: "Trying to save the file again", failure: "Try again failed"}}), {target: item.child_action_id, primary: true, requiresWorker: true});
  }
  if (failure) {
    const card = node("div", "", "replacement-failure jd-note");
    card.setAttribute("role", "group");
    card.append(node("strong", `Replacement stopped: ${failure.reason}`), node("p", {in_place: "Your original file is still in place.", recovery: "Your original is safe in its recovery copy. Check Recovery before trying again.", unclear: "Check Recovery to see where your original is before trying again."}[originalStateOf(failure)], "metadata"));
    body.append(card);
    if (item.retry_action_id) add(retryReplacementLabel(item, id, "").button, () => retryFileReplacement(item, id), {target: item.retry_action_id, primary: true});
    if (item.replacement_action_id) add("View details", () => openJob(item.replacement_action_id), {navigation: true, primary: false});
    if (failure.detail) {
      const technical = node("details", "", "technical-reason");
      technical.append(node("summary", "Technical details"), node("p", failure.detail, "metadata"));
      body.append(technical);
    }
  } else if (item.replacement_action_id) add("View replacement", () => openJob(item.replacement_action_id), {navigation: true});
  const unsupportedSource = item.mode && (sourceGateReason(item.reason_code) || item.reasons?.some(sourceGateReason));
  // A ready candidate can also be encoded again under a new goal; the dialog
  // says that candidate is removed, and Replace file stays the primary action.
  if (item.can_reconfigure && !unsupportedSource && (["skip","review","failed","cancelled","waiting_decision"].includes(item.status) || item.candidate_ready && !item.replacement_plan)) add(item.mode ? "Change goal" : "Change settings", () => reconfigureBatch(id, false, {item_key:item.item_key, ready_candidate:Boolean(item.candidate_ready)}));
  // A cancelled or failed file can be encoded again as it was set up, without
  // reopening the goal dialog.
  if (item.can_reconfigure && !unsupportedSource && ["failed","cancelled"].includes(item.status))
    add("Encode again", () => encodeFileAgain(id, item.item_key), {primary: true});
  if (filesRouteFor(item.file_path))
    add("Show in Files", () => showInFiles(item.file_path), {navigation: true, target: item.child_action_id || id});
  if (item.child_action_id && !item.replacement_action_id) add("Open job", () => openJob(item.child_action_id), {navigation: true, target: item.child_action_id, primary: false});
  if (review && !item.replacement_action_id && !item.child_action_id) add("Review", review, {navigation: true, primary: false});

  const where = whereBlock(item.where, {heading: false});
  if (where) {
    const details = node("details", "", "batch-where");
    details.append(node("summary", "Where"), where);
    body.append(details);
  }
  if (item.tracks_message) body.append(node("p", item.tracks_message, "muted batch-file-tracks"));
  // A finished candidate repeats its batch's goal and its own status, so only
  // files that still need a look say which goal they follow.
  const settled = item.candidate_ready || item.replaced || item.replacement_action_id;
  if (settled) {
    // Nothing to add.
  } else if (item.mode && item.mode !== job?.mode) body.append(node("p", `Goal: ${item.mode === "compact" ? `Shrink to ${item.max_height || 1080}p SDR` : goalNames[item.mode] || item.mode}`, "metadata batch-file-profile"));
  else if (item.requested_profile || item.profile) body.append(node("p", `${item.retry_pending ? "Next attempt" : "Profile"}: ${profileOptionLabel(item.requested_profile || item.profile)}`, "metadata batch-file-profile"));
  if (item.previous_attempts?.length) {
    const history = node("details", "", "batch-attempt-history");
    history.append(node("summary", `${item.previous_attempts.length} previous ${item.previous_attempts.length === 1 ? "attempt" : "attempts"}`));
    item.previous_attempts.forEach((attempt, index) => {
      const open = button(`Attempt ${index + 1} · ${names[attempt.status] || attempt.status}`, () => openJob(attempt.child_action_id), "jd-secondary");
      open.dataset.jobControl = attempt.child_action_id;
      open.dataset.navigation = "true";
      open.dataset.batchItemKey = item.item_key;
      open.disabled = jobControlDisabled(open);
      history.append(open);
    });
    body.append(history);
  }
  const reasons = item.waiting_reason ? [item.waiting_reason] : item.reason_code ? [item.reason_code] : item.error ? [item.error] : item.reasons || [];
  // The recipe's reason says why a file was or was not converted; once it is
  // being replaced or has a candidate it explains nothing more.
  const visibleReasons = item.replacement_action_id || item.replacement_plan || item.replaced || item.candidate_ready ? [] : [...new Set(reasons.filter(reason => reason && !/^(explicit profile requested|eligible for transcode|per-source bounded samples required before full encode|New settings requested(; previous attempt retained in history)?)$/i.test(reason)).map(shortJobReason))];
  if (!visibleReasons.length && item.status === "skip") visibleReasons.push("This file was omitted by the processing plan; original kept. View the file details or change settings to try again.");
  for (const reason of visibleReasons) body.append(node("p", reason, "muted jd-note"));
  if (item.candidate_ready && belowTargetLabel(item.size_budget_outcome)) body.append(node("p", belowTargetLabel(item.size_budget_outcome), "badge size-budget-badge"));
  if (item.candidate_ready && !state.info.allow_destructive) body.append(node("p", "Replacement is disabled in server settings", "muted jd-note"));
  if (actions.children.length) body.append(actions);
  return row;
}
// The most the server returns at once; a batch of 148 files is two pages.
const batchItemsPage = 100;
async function loadBatchItems(id, revision) {
  const offset = state.batchItemsOffset;
  const response = await api(
    `batch-items?${new URLSearchParams({ id, offset, limit: batchItemsPage })}`,
  );
  if (
    id !== state.detail ||
    revision !== state.detailRevision ||
    offset !== state.batchItemsOffset ||
    !$("job-detail").open
  )
    return;
  const list = $("batch-items-list");
  const focus = focusedControl(list);
  const toggled = document.activeElement?.dataset?.fileToggle || null;
  list.replaceChildren(...(response.items || []).map(item => batchFileRow(item, id)));
  restoreControl(list, focus);
  if (toggled) [...list.querySelectorAll("[data-file-toggle]")].find(t => t.dataset.fileToggle === toggled)?.focus({preventScroll: true});
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
  safe(() => changeBatchPage(-batchItemsPage)),
);
$("batch-items-next").addEventListener("click", () =>
  safe(() => changeBatchPage(batchItemsPage)),
);
// The collapsible rows under the job summary are static markup, so a user's
// open or closed row survives each refresh. A row opens itself once per job
// when it explains a failure; closing it afterwards sticks.
const detailRowKeys = ["plan", "steps", "path", "log"];
function showDetailRow(key, visible, autoOpen = false) {
  const row = $(`detail-row-${key}`);
  row.hidden = !visible;
  if (!visible || !autoOpen || state.detailAutoOpened.has(key)) return false;
  state.detailAutoOpened.add(key);
  row.open = true;
  return true;
}
function resetDetailRows() {
  for (const key of detailRowKeys) {
    $(`detail-row-${key}`).hidden = true;
    $(`detail-row-${key}`).open = false;
  }
  $("detail-plan").replaceChildren();
  $("detail-path").replaceChildren();
  $("detail-stages").replaceChildren();
  $("detail-costs").replaceChildren();
  $("detail-content").textContent = "";
}
function detailPath(job) {
  return job.source_path || job.promotion?.original_path || "";
}
function renderDetailRows(job) {
  const plan = $("detail-plan");
  plan.replaceChildren();
  const planNote = encodingPlanNote(job), sizeNote = sizeModeNote(job);
  if (planNote) plan.append(planNote);
  if (sizeNote) plan.append(sizeNote);
  showDetailRow("plan", Boolean(planNote || sizeNote));
  renderStages(job);
  const stages = effectiveStages(job);
  showDetailRow("steps", stages.length > 0, stages.some(stage => ["failed", "rejected"].includes(stage.status)));
  $("detail-costs").replaceChildren();
  renderPhaseCosts($("detail-costs"), job);
  const paths = $("detail-path");
  paths.replaceChildren();
  if (job.source_path) paths.append(node("p", job.source_path, "metadata"));
  const location = job.promotion?.new_path || job.promotion?.original_path;
  if (location) paths.append(node("p", `Library: ${location.slice(0, location.lastIndexOf("/"))}`, "metadata"));
  // With several workers, say which one runs (or ran) the job.
  const workerName = job.worker_name || (typeof job.worker === "string" ? job.worker : "");
  if (workerName && (state.workerInfo?.nodes?.length || 0) > 1) paths.append(node("p", `Worker: ${workerName}`, "metadata"));
  showDetailRow("path", paths.children.length > 0);
  // The log opens itself for a failure no step explains; opening loads it.
  const explained = stages.some(stage => ["failed", "rejected"].includes(stage.status));
  showDetailRow("log", Boolean(job.logs_available), job.status === "failed" && !explained);
}
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

function renderBatchCounts(container, batch, omit = []) {
  const counts = node("div", "", "batch-counts");
  if (batch.policy > 0) counts.append(statusBadge(`${batch.policy} Below minimum savings`, "neutral"));
  if (batch.rejected > 0) counts.append(statusBadge(`${batch.rejected} Rejected`, "rejected"));
  for (const [key, label, status] of [["completed","Completed","completed"],["running","Active","running"],["queued","Queued","queued"],["waiting_for_slot","Waiting for slot","queued"],["waiting_decision","Needs decision","waiting_decision"],["review","Needs review","review"],["failed","Failed","failed"],["skip","Skipped","skip"],["cancelled","Cancelled","cancelled"]]) {
    if (batch[key] > 0 && !omit.includes(key)) counts.append(statusBadge(`${batch[key]} ${label}`, status));
  }
  if (counts.children.length) container.append(counts);
}
// The steps as the job dialog shows them, with batch and rejected-candidate
// outcomes folded into the stored stage that reports them.
function effectiveStages(job) {
  if (job.batch?.dry_run) return [];
  return (job.stages || []).map((storedStage, index) => {
    let stage = storedStage;
    const batch = visibleBatch(job);
    if (stage.name === "schedule_batch" && ["failed","partial"].includes(stage.status) && batch?.policy && !batch.failed) {
      stage = {...stage,status:batch.completed > 0 ? "partial" : "skip",note:`${batch.completed || 0} candidates · ${batch.policy} below minimum savings · Originals kept`};
    } else if (savingsPolicyFailure(job.error) && index === job.current_step) stage = {...stage,status:"skip",note:"Minimum savings not met; full conversion not started."};
    else if (stage.name === "schedule_batch" && ["failed","partial"].includes(stage.status) && batch?.rejected) {
      stage = {...stage,status:batch.failed > 0 || batch.completed > 0 ? "partial" : "rejected",note:[`${batch.completed || 0} candidates`,`${batch.rejected} rejected`,batch.failed > 0 ? `${batch.failed} failed` : ""].filter(Boolean).join(" · ")};
    } else if (rejectedCandidate(job) && index === job.current_step) stage = {...stage,status:"rejected",note:["Candidate rejected; original preserved.", candidateCleanupNote(job)].filter(Boolean).join(" ")};
    return stage;
  });
}
function renderStages(job) {
  const list = $("detail-stages");
  list.replaceChildren();
  for (const [index, stage] of effectiveStages(job).entries()) {
    const row = node("li", "", `stage ${stage.status}`);
    row.append(node("span", String(index + 1), "stage-number"), node("span", stageLabel(stage.name)), statusBadge(stage.status === "pending" ? "Next" : names[stage.status] || stage.status, stage.status));
    if (stage.note) row.append(node("span", stage.note, "stage-note"));
    if (index === job.current_step && ["running", "waiting_external", "waiting_decision"].includes(job.status)) row.setAttribute("aria-current", "step");
    list.append(row);
  }
}

// Where the job is in its steps, in one line: "Step 6 of 7 · Convert file".
// Done jobs say nothing here; the All steps row lists every step.
function stepText(job) {
  const stages = effectiveStages(job);
  if (!stages.length) return "";
  const active = ["running", "waiting_external", "waiting_decision"].includes(job.status);
  const stateOf = (stage, index) => ["failed", "rejected"].includes(stage.status) ? "failed"
    : active && (index === job.current_step || stage.status === "running") ? "current"
    : ["completed", "skip", "partial"].includes(stage.status) ? "done" : "pending";
  const states = stages.map(stateOf);
  const at = states.includes("failed") ? states.indexOf("failed") : states.indexOf("current");
  if (at >= 0) return `${states[at] === "failed" ? "Failed at step" : "Step"} ${at + 1} of ${stages.length} \u00b7 ${stageLabel(stages[at].name)}`;
  // "Next" only means something while the job can still move on; a finished,
  // stopped or cancelled one has no next step to name.
  if (states.every(value => value === "done") || ["completed", "cancelled", "failed", "rejected", "skip", "archived"].includes(job.status)) return "";
  return `Next: ${stageLabel(stages[Math.max(0, states.indexOf("pending"))].name)}`;
}
// Compact replacements can be re-grabbed by Radarr/Sonarr. The tracking
// shown is read-only; Navigatorr never changes monitoring or profiles.
function regrabWarning(notice) {
  if (!notice?.regrab_warning) return null;
  const box = node("div", "", "regrab-warning");
  box.setAttribute("role", "alert");
  box.append(node("strong", "Radarr or Sonarr may download the original again"), node("p", notice.regrab_warning));
  const tracking = [];
  if (typeof notice.arr_monitored === "boolean") tracking.push(notice.arr_monitored ? "Monitored" : "Not monitored");
  if (notice.arr_quality_profile) tracking.push(`Quality profile: ${notice.arr_quality_profile}`);
  if (tracking.length) box.append(node("p", `Library tracking now: ${tracking.join(" · ")}`, "metadata"));
  return box;
}
// Only a notice the server never stored (non-compact batch) means "no
// warning". Any other failure, or a notice computed for another plan, stops
// the review rather than letting a replacement be approved unwarned.
async function readBatchPromotionNotice(id, digest) {
  let notice;
  try {
    notice = (await tool("action_detail", { id, section: "state", key: "batch_promotion_notice", chunk: 0 })).data;
  } catch (error) {
    if (/not found in section/.test(error?.message || "")) return null;
    throw new Error("Cannot verify the re-grab warning. Reopen the replacement review.");
  }
  if (!notice || typeof notice !== "object") return null;
  if (notice.plan_digest !== undefined && notice.plan_digest !== digest)
    throw new Error("The re-grab warning belongs to an earlier plan. Reopen the replacement review.");
  return notice;
}
function replacementReady(job) {
  const p = job?.promotion;
  return !!(p?.original_path && p.candidate_path && p.original_sha256 && p.candidate_sha256 && p.plan_digest && job.status === "waiting_decision" && job.waiting_options?.some(option => option.decision === "approve"));
}
// An existing replacement that is waiting for approval opens its review;
// anything else opens the job.
async function openReplacement(id) {
  const job = (await api(`operations?id=${encodeURIComponent(id)}`)).jobs?.[0];
  if (replacementReady(job)) await reviewFilePromotion(id);
  else await openJob(id);
}
const replacementMethods = {filesystem: "Replace the file in place", sonarr: "Import through Sonarr", radarr: "Import through Radarr"};
function showFileReview(job, approval, {unprompted = false} = {}) {
  const p = job.promotion;
  Object.assign(approval, { digest: p.plan_digest, original: p.original_sha256, candidate: p.candidate_sha256 });
  const facts = job.replacement_facts || {source_bytes: p.original_bytes, candidate_bytes: p.candidate_bytes, size_budget_outcome: job.size_budget_outcome};
  const name = p.original_path.split("/").pop().replace(/\.[^.]+$/, "");
  const data = $("file-review-data");
  data.replaceChildren(replacementCard({name, facts, originalPath: p.original_path, newPath: p.candidate_path}), node("p", `Method: ${replacementMethods[p.service] || p.service || "Replace the file in place"}`, "metadata"));
  const warning = regrabWarning(p);
  if (warning) data.prepend(warning);
  const frees = replacementTotalLine([p], () => facts);
  $("file-review-total").textContent = frees && frees.replace(/ across 1 file$/, "");
  $("reject-file-review").hidden = !job.waiting_options?.some(option => option.decision === "reject");
  const settle = () => {
    $("reject-file-review").disabled = !serverReachable || state.approvingFile;
    $("approve-file-review").disabled = !serverReachable || state.approvingFile;
  };
  if (!unprompted) return settle();
  // A review that opens by itself, while a finger may already be on its way
  // to something else, keeps its buttons off until the layout has settled: a
  // stray tap must not land on Replace file or Keep original.
  $("reject-file-review").disabled = true;
  $("approve-file-review").disabled = true;
  setTimeout(() => { if (state.fileApproval === approval) settle(); }, 800);
}
function pollDelay(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}
// Tapping Replace file is the decision, so nothing is asked twice: the plan is
// read back once it is ready and approved with the digest just read. Only what
// a person has to see first (a Radarr or Sonarr re-grab warning) or a plan that
// cannot be approved opens the review or the job instead.
async function replaceWhenPlanned(id) {
  const auth = state.authRevision;
  notify("Preparing the replacement. It starts by itself.");
  for (let attempt = 0; attempt < 200; attempt++) {
    await pollDelay(1500);
    if (auth !== state.authRevision || $("workspace").hidden) return;
    let job;
    try {
      job = (await api(`operations?id=${encodeURIComponent(id)}`)).jobs?.[0];
    } catch {
      continue;
    }
    if (replacementReady(job)) {
      if (job.promotion.regrab_warning) return reviewFilePromotion(id, {unprompted: true});
      return jobControl(id, "action_resume", { decision: "approve", plan_digest: job.promotion.plan_digest }, {decision: {label: "Replace file", verb: "Replacing file", failure: "Replacement failed"}});
    }
    if (!job || ["failed", "cancelled", "completed"].includes(job.status)) return notify(shortJobReason(job?.error) || "This replacement can no longer be approved. Nothing was changed.");
    if (job.status === "waiting_decision") return openJob(id);
  }
  notify("The replacement is still being prepared. Open the job to follow it.");
}
async function reviewFilePromotion(id, {unprompted = false} = {}) {
  const auth = state.authRevision;
  const job = (await api(`operations?id=${encodeURIComponent(id)}`)).jobs?.[0];
  if (auth !== state.authRevision || $("workspace").hidden) return;
  if (!replacementReady(job)) throw new Error("This replacement is no longer waiting for approval. Refresh the queue.");
  const approval = state.fileApproval = { id, digest: "", original: "", candidate: "" };
  showFileReview(job, approval, {unprompted});
  $("file-review").showModal();
}
$("dismiss-file-review").addEventListener("click", () => $("file-review").close());
$("dismiss-file-review-action").addEventListener("click", () => $("file-review").close());
// Keeping the original needs no reviewed digest: it changes no file.
$("reject-file-review").addEventListener("click", () => safe(async () => {
  const approval = state.fileApproval;
  if (!approval || state.approvingFile || !serverReachable) return;
  await jobControl(approval.id, "action_resume", { decision: "reject" }, {onAccepted: () => $("file-review").close(), decision: {label: "Keep original", verb: "Keeping the original", failure: "Decision failed"}});
}));
$("file-review").addEventListener("close", () => { state.fileApproval = null; });
$("approve-file-review").addEventListener("click", () => safe(async () => {
  const approval = state.fileApproval;
  if (!approval || !approval.digest || state.approvingFile || !serverReachable) return;
  state.approvingFile = true;
  $("approve-file-review").disabled = true;
  try {
    const current = (await api(`operations?id=${encodeURIComponent(approval.id)}`)).jobs?.[0];
    if (state.fileApproval !== approval || !$("file-review").open) return;
    if (current?.status !== "waiting_decision" || !current.waiting_options?.some(option => option.decision === "approve") || current.promotion?.original_sha256 !== approval.original || current.promotion?.candidate_sha256 !== approval.candidate || current.promotion?.plan_digest !== approval.digest)
      throw new Error("The replacement plan changed. Close this review and open it again.");
    // The server refuses an approval whose digest is not the reviewed plan's.
    // Once it accepts, the review is done: progress shows on the job's row.
    await jobControl(approval.id, "action_resume", { decision: "approve", plan_digest: approval.digest }, {onAccepted: () => $("file-review").close(), decision: {label: "Replace file", verb: "Replacing file", failure: "Replacement failed"}});
  } finally {
    state.approvingFile = false;
    $("approve-file-review").disabled = !serverReachable;
  }
}));

// One dialog decides a batch replacement: the files with what each saves and
// how good it measured, then one button. Everything shown here is display;
// what an approval is checked against is the plan's member list and digest,
// which the server holds and this page reads back before it approves.
const fileCount = (n) => `${n} ${n === 1 ? "file" : "files"}`;
function replacementFileName(member) {
  return member.episode || (member.original_path || "").split("/").pop().replace(/\.[^.]+$/, "") || "File";
}
function replacementSizes(facts) {
  if (!(facts?.source_bytes > 0 && facts?.candidate_bytes > 0)) return null;
  const saved = facts.source_bytes - facts.candidate_bytes;
  const result = saved >= 0 ? `Saves ${bytes(saved)} (${Math.round(saved / facts.source_bytes * 100)}%)` : `Grows by ${bytes(-saved)}`;
  return node("p", `${bytes(facts.source_bytes)} → ${bytes(facts.candidate_bytes)} · ${result}`, "replacement-sizes");
}
function replacementQuality(facts) {
  if (facts?.vmaf_mean == null) return null;
  const parts = [`Quality VMAF ${Math.round(facts.vmaf_mean)}`];
  if (facts.vmaf_worst_window != null) parts.push(`worst ${Math.round(facts.vmaf_worst_window)}`);
  return node("p", parts.join(" · "), "metadata replacement-quality");
}
// One file as a person decides on it. The paths stay behind a disclosure.
function replacementCard({name, facts, originalPath, newPath, check, onView}) {
  const card = node("div", "", `candidate-review-row replacement-row${check ? " selectable" : ""}`);
  // The name is cut at two lines in a narrow list; its full text is the title.
  // Inside a label it is a plain span: a <strong> there would render bolder
  // than the label's weight, i.e. the font's heaviest face.
  const nameLine = node(check ? "span" : "strong", name, check ? "" : "replacement-name");
  nameLine.title = name;
  if (check) {
    const label = node("label", "", "check replacement-name");
    label.append(check, nameLine);
    card.append(label);
  } else card.append(nameLine);
  for (const line of [replacementSizes(facts), replacementQuality(facts), belowTargetNotice(facts?.size_budget_outcome)]) if (line) card.append(line);
  const tracks = facts?.tracks_message || facts?.audio_summary;
  if (tracks) card.append(node("p", tracks, "metadata replacement-tracks"));
  if (originalPath || newPath) {
    const paths = node("details", "", "replacement-paths");
    paths.append(node("summary", "Show paths"));
    for (const [label, path] of [["Original", originalPath], ["New file", newPath]]) {
      if (!path) continue;
      const line = node("p", "", "metadata backup-path");
      line.append(node("span", `${label}: `), node("span", path));
      paths.append(line);
    }
    if (onView) paths.append(button("View file", onView, "text-button"));
    card.append(paths);
  }
  return card;
}
// The total a decision frees, only when every file in it reports its sizes.
function replacementTotalLine(members, factsOf) {
  const known = members.filter((m) => factsOf(m)?.source_bytes > 0 && factsOf(m)?.candidate_bytes > 0);
  if (!members.length || known.length !== members.length) return "";
  const saved = known.reduce((sum, m) => sum + factsOf(m).source_bytes - factsOf(m).candidate_bytes, 0);
  return saved >= 0 ? `Frees ${bytes(saved)} across ${fileCount(members.length)}` : `Grows by ${bytes(-saved)} across ${fileCount(members.length)}`;
}
const replaceDecision = (n) => ({label: `Replace ${fileCount(n)}`, verb: `Replacing ${fileCount(n)}`, failure: "Replacement failed"});
const prepareDecision = (n) => ({label: `Prepare replacement of ${fileCount(n)}`, verb: `Preparing replacement of ${fileCount(n)}`, failure: "Preparation failed"});
const keepDecision = (n) => ({label: `Keep ${n} ${n === 1 ? "original" : "originals"}`, verb: `Keeping ${n} ${n === 1 ? "original" : "originals"}`, failure: "Keeping originals failed"});

// The plan exactly as the server holds it. prepared is false when none exists.
async function readBatchPlan(id) {
  const plan = await api(`batch-candidates?${new URLSearchParams({id, plan: 1})}`);
  if (plan.prepared && (plan.id !== id || !plan.digest || !Array.isArray(plan.members) || !plan.members.length))
    throw new Error("Cannot verify the batch replacement list.");
  return plan;
}
// How the server's prepared plan differs from the files the person saw.
function planDifferences(shown, members) {
  const planned = new Map(members.map((m) => [m.item_key, m]));
  const seen = new Map(shown.map((m) => [m.item_key, m]));
  const out = [];
  for (const m of shown) {
    const p = planned.get(m.item_key);
    if (!p) out.push(`Not in the prepared list: ${replacementFileName(m)}`);
    else if (p.original_path !== m.original_path || p.candidate_path !== m.candidate_path || p.transcode_action_id !== m.transcode_action_id || (m.candidate_sha256 && p.candidate_sha256 !== m.candidate_sha256))
      out.push(`Changed since you reviewed it: ${replacementFileName(m)}`);
  }
  for (const m of members) if (!seen.has(m.item_key)) out.push(`Added to the prepared list: ${replacementFileName(m)}`);
  return out;
}
const reviewMembers = (r) => r.mode === "plan" ? r.members : r.members.filter((m) => r.selected.has(m.item_key));
function syncBatchReview() {
  const r = state.batchReview;
  const chosen = r ? reviewMembers(r) : [];
  const approve = $("approve-batch-review"), reject = $("reject-batch-review");
  approve.textContent = chosen.length ? `Replace ${fileCount(chosen.length)}` : "Replace files";
  $("batch-review-total").textContent = r ? replacementTotalLine(chosen, (m) => r.facts[m.transcode_action_id]) : "";
  approve.disabled = !serverReachable || !r || r.busy || !chosen.length || !state.info?.allow_destructive;
  reject.hidden = !r || r.mode !== "plan";
  reject.disabled = !serverReachable || !r || r.busy || !state.info?.allow_destructive;
}
function renderBatchReview() {
  const r = state.batchReview;
  const notice = $("batch-review-notice"), host = $("batch-review-data");
  const notes = [];
  if (r.differences?.length) {
    const box = node("div", "", "review-difference");
    box.setAttribute("role", "alert");
    box.append(node("strong", "The prepared list is not the one you selected. Nothing was replaced."), node("p", "Review this list, then confirm again."));
    const list = node("ul", "", "metadata");
    for (const line of r.differences) list.append(node("li", line));
    box.append(list);
    notes.push(box);
  } else if (r.changed) {
    notes.push(node("p", "The list changed while this review was open. Nothing was replaced. Review it, then confirm again.", "review-difference"));
  }
  const warning = regrabWarning(r.notice);
  if (warning) notes.push(warning);
  if (!state.info?.allow_destructive) notes.push(node("p", "Replacement is disabled in the server settings.", "metadata"));
  notice.replaceChildren(...notes);
  const rows = [];
  const view = (m) => () => { $("batch-review").close(); return openJob(m.transcode_action_id); };
  if (r.mode === "select") {
    const all = node("input");
    all.type = "checkbox";
    all.checked = r.selected.size === r.members.length;
    all.indeterminate = r.selected.size > 0 && !all.checked;
    all.setAttribute("aria-label", "Select all files");
    const allLabel = node("label", "", "check");
    allLabel.append(all, node("span", "Select all files"));
    const checks = [];
    all.addEventListener("change", () => {
      r.selected = new Set(all.checked ? r.members.map((m) => m.item_key) : []);
      checks.forEach((c) => { c.checked = all.checked; });
      all.indeterminate = false;
      syncBatchReview();
    });
    rows.push(allLabel);
    for (const m of r.members) {
      const check = node("input");
      check.type = "checkbox";
      check.checked = r.selected.has(m.item_key);
      checks.push(check);
      check.addEventListener("change", () => {
        if (check.checked) r.selected.add(m.item_key); else r.selected.delete(m.item_key);
        all.checked = r.selected.size === r.members.length;
        all.indeterminate = r.selected.size > 0 && !all.checked;
        syncBatchReview();
      });
      rows.push(replacementCard({name: replacementFileName(m), facts: r.facts[m.transcode_action_id], originalPath: m.original_path, newPath: m.candidate_path, check, onView: view(m)}));
    }
    if (r.excluded) rows.push(node("p", `${r.excluded} other files are unfinished, have no candidate, or already have a replacement job. They stay unchanged.`, "metadata"));
  } else {
    for (const m of r.members)
      rows.push(replacementCard({name: replacementFileName(m), facts: r.facts[m.transcode_action_id], originalPath: m.original_path, newPath: m.candidate_path, onView: view(m)}));
  }
  host.replaceChildren(...rows);
  syncBatchReview();
}
function openBatchReview(review) {
  state.batchReview = {busy: false, facts: {}, ...review};
  renderBatchReview();
  if (!$("batch-review").open) $("batch-review").showModal();
}
// The batch's replacement review. A batch with a prepared plan shows exactly
// that plan; otherwise it shows the candidates to choose from.
async function reviewBatchReplacement(id) {
  const auth = state.authRevision;
  const plan = await readBatchPlan(id);
  if (auth !== state.authRevision || $("workspace").hidden) return;
  if (plan.prepared) {
    if (plan.approved) throw new Error("This replacement is already approved. Its progress shows on the batch.");
    if (!plan.waiting) throw new Error("This replacement is not waiting for approval. Refresh the queue.");
    const notice = await readBatchPromotionNotice(id, plan.digest);
    if (auth !== state.authRevision || $("workspace").hidden) return;
    openBatchReview({id, mode: "plan", members: plan.members, facts: plan.member_facts || {}, digest: plan.digest, notice, noticeSeen: true});
    return;
  }
  const review = await api(`batch-candidates?id=${encodeURIComponent(id)}`);
  if (auth !== state.authRevision || $("workspace").hidden) return;
  if (!review.members.length) {
    notify("No unreserved candidates are available. Open the batch files to inspect their outcomes.");
    return;
  }
  openBatchReview({id, mode: "select", members: review.members, facts: review.member_facts || {}, version: review.version, excluded: review.excluded, selected: new Set(review.members.map((m) => m.item_key))});
}
// Shows the plan the server holds, with why it needs another look.
function showPreparedPlan(r, plan, notice, differences, changed) {
  Object.assign(r, {mode: "plan", members: plan.members, facts: plan.member_facts || {}, digest: plan.digest, selected: null, notice, noticeSeen: true, differences, changed});
  renderBatchReview();
}
// One click: prepare the plan for exactly the checked files if none exists,
// read it back, and approve only when it is the list that was shown, with the
// digest the server returned for it.
async function replaceBatchFiles() {
  const r = state.batchReview;
  if (!r || r.busy || !serverReachable || !state.info?.allow_destructive) return;
  const auth = state.authRevision;
  const live = () => state.batchReview === r && $("batch-review").open && auth === state.authRevision;
  r.busy = true;
  syncBatchReview();
  try {
    const shown = reviewMembers(r);
    let plan = await readBatchPlan(r.id);
    if (!live()) return;
    if (!plan.prepared) {
      if (r.mode !== "select") throw new Error("This replacement is no longer prepared. Close this review and open it again.");
      await commandRequest("batch-candidates", {id: r.id, version: r.version, item_keys: shown.map((m) => m.item_key)}, undefined, prepareDecision(shown.length));
      if (!live()) return;
      plan = await readBatchPlan(r.id);
      if (!live()) return;
    }
    if (!plan.prepared || plan.approved || !plan.waiting) throw new Error("This replacement is no longer waiting for approval. Refresh the queue.");
    const notice = await readBatchPromotionNotice(r.id, plan.digest);
    if (!live()) return;
    const differences = planDifferences(shown, plan.members);
    const changed = r.mode === "plan" && plan.digest !== r.digest;
    if (differences.length || changed || (notice?.regrab_warning && !r.noticeSeen)) {
      showPreparedPlan(r, plan, notice, differences, changed);
      return;
    }
    await commandRequest("batch-candidates", {id: r.id, digest: plan.digest, decision: "approve"}, () => $("batch-review").close(), replaceDecision(shown.length));
  } finally {
    r.busy = false;
    syncBatchReview();
  }
}
// Keeps every original of a prepared plan from the queue row, without opening
// the review: dropping a plan changes no file.
async function keepPreparedOriginals(id) {
  const plan = await readBatchPlan(id);
  if (!plan.prepared || !plan.waiting) throw new Error("Reopen the replacement review before deciding.");
  await commandRequest("batch-candidates", {id, digest: plan.digest, decision: "reject"}, undefined, keepDecision(plan.members.length));
}
// Keeps every original: the plan is dropped and nothing is replaced.
async function keepBatchOriginals() {
  const r = state.batchReview;
  if (!r || r.mode !== "plan" || r.busy || !serverReachable) return;
  const auth = state.authRevision;
  const live = () => state.batchReview === r && $("batch-review").open && auth === state.authRevision;
  r.busy = true;
  syncBatchReview();
  try {
    const plan = await readBatchPlan(r.id);
    if (!live()) return;
    if (!plan.prepared || !plan.waiting) throw new Error("This replacement is no longer waiting for a decision. Refresh the queue.");
    if (plan.digest !== r.digest) {
      showPreparedPlan(r, plan, r.notice, planDifferences(r.members, plan.members), true);
      return;
    }
    await commandRequest("batch-candidates", {id: r.id, digest: plan.digest, decision: "reject"}, () => $("batch-review").close(), keepDecision(plan.members.length));
  } finally {
    r.busy = false;
    syncBatchReview();
  }
}
$("dismiss-batch-review").addEventListener("click", () => $("batch-review").close());
$("close-batch-review").addEventListener("click", () => $("batch-review").close());
$("batch-review").addEventListener("close", () => { state.batchReview = null; syncBatchReview(); });
$("approve-batch-review").addEventListener("click", () => safe(replaceBatchFiles));
$("reject-batch-review").addEventListener("click", () => safe(keepBatchOriginals));
$("close-detail").addEventListener("click", () => $("job-detail").close());
$("job-detail").addEventListener("close", () => { state.detailRevision++; state.detail=null; writeNavigation(); });
$("detail-row-log").addEventListener("toggle", () => { if ($("detail-row-log").open) safe(loadDetail); });
$("copy-detail-path").addEventListener("click", () => safe(async () => {
  const path = state.detailJob ? detailPath(state.detailJob) : "";
  if (!path) return;
  if (typeof navigator === "undefined" || !navigator.clipboard?.writeText) return notify("Copying is not available here.");
  await navigator.clipboard.writeText(path);
  notify("Path copied.");
}));

function profileEncoder(p) {
  return p?.video?.codec === "hevc_videotoolbox" ? "Hardware HEVC" : p?.video?.codec === "libx265" ? "Software HEVC · CPU" : p?.video?.codec || "Encoder unavailable";
}
function profileRate(p) {
  const v = p?.video || {};
  if (v.x265) return `${v.x265.bitrate_kbps} kbps target bitrate · Two-pass x265${v.preset ? ` · ${v.preset} preset` : ""}`;
  if (v.average_bitrate_kbps > 0) return `${v.average_bitrate_kbps} kbps target bitrate`;
  if (v.quality == null) return "Profile default";
  if (v.codec === "libx265") return `CRF ${v.quality} · Lower means higher quality${v.preset ? ` · ${v.preset} preset` : ""}`;
  if (v.codec === "hevc_videotoolbox") return `Quality ${v.quality} · Higher means higher quality`;
  return `Quality ${v.quality}`;
}
// Profiles that calibrate with samples are rejected by the server.
function profileSearches(name) {
  return Boolean(state.recipeDetails[name]?.profile?.optimization?.enabled);
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
  const effective = p.video?.codec === "copy" ? "Keep video" : p.video?.x265 ? `${p.video.x265.bitrate_kbps} kbps · Two passes` : p.video?.quality != null ? `${p.video.codec === "libx265" ? "CRF" : "Quality"} ${p.video.quality}` : "Custom settings";
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
  const runnable = state.recipes.filter((n) => !profileSearches(n));
  runnable.forEach((n) => option($("profile"), n, profileOptionLabel(n)));
  $("profile").value = runnable.includes(selected) ? selected : "auto";
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
    const bitrate = (args.profile.video.codec === "hevc_videotoolbox" && args.profile.video.average_bitrate_kbps > 0) || !!args.profile.video.x265;
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
    (!software && state.recipeDraft?.video?.average_bitrate_kbps > 0) || (software && !!state.recipeDraft?.video?.x265);
  $("recipe-preset").disabled = !software;
  const presetLabel = $("recipe-preset").closest?.("label");
  if (presetLabel) presetLabel.hidden = !software;
  $("recipe-quality").disabled = bitrate;
  $("recipe-quality").max = software ? "51" : "100";
  $("recipe-quality").min = bitrate ? "0" : "1";
  $("recipe-quality-label").textContent = bitrate ? "Quality (bitrate mode)" : software ? "CRF · lower preserves more detail" : "VideoToolbox Q · higher preserves more detail";
  $("recipe-scale-note").textContent = bitrate ? "Bitrate is measured in kbps." : software ? "x265 CRF: 1–51, lower favors quality. This is not a quality percentage." : "VideoToolbox Q: 1–100, higher favors quality. Q 70 is not 70% quality and cannot be compared to x265 CRF.";
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
      delete p.video.x265;
      p.video.quality = 65;
    }
    setRecipeControls(p);
    renderRecipeDirty();
    notify(
      "Encoder changed. Incompatible settings were reset; review before saving.",
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
    (p.video.codec === "hevc_videotoolbox" && p.video.average_bitrate_kbps > 0) || p.video.x265
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
  // A stored optimization block (the removed sample search's settings) stays in
  // the profile as it is; nothing edits it here.
  const {optimization: _stored, ...shown} = profile;
  const view = mergeAdvancedDefaults(defaults, shown);
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
        const x265Bounds = {bitrate_kbps:[64,50000],lookahead:[5,250],b_frames:[0,16],ref_frames:[1,16],aq_mode:[0,4],aq_strength:[0,3],psy_rd:[0,3],psy_rdoq:[0,3]};
        const fieldBounds = current.startsWith("video.x265.") ? x265Bounds[key] : bounds[key];
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
                current.includes(".quality.") || (current.startsWith("video.x265.") && ["aq_strength","psy_rd","psy_rdoq"].includes(key))
                  ? "any"
                  : "1";
              if (fieldBounds) [input.min, input.max] = fieldBounds.map(String);
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
                  (fieldBounds &&
                    (next < fieldBounds[0] || next > fieldBounds[1])) ||
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
  if (copy.restore_available) return "This is the original file. The replacement never took its library path, which is empty. Move it back to restore the library file; nothing is deleted.";
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
  const commandFocused = commandFocus();
  const focus = focusedControl($("backup-list"));
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
    if (copy.restore_available) {
      const restore = button("Move original back", () => cleanBackup(copy,"restore"));
      restore.disabled = Boolean(state.backupCleaning) || !serverReachable;
      actions.append(restore);
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
  // Rebuilt rows get their command chips back now, not on the next poll.
  renderCommandIndicators();
  restoreCommandFocus(commandFocused);
  restoreControl($("backup-list"), focus);
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
  if (["discard","restore"].includes(task.mode) && !currentObservation && !task.error) { renderBackupCleanup(); return; }
  if (operationIsActive(job.status) || currentObservation && !["completed","stopped"].includes(job.cleanup?.phase)) { renderBackupCleanup(); return; }
  state.backupCleaning = null;
  $("backup-progress").hidden = !state.backupsVisible;
  $("backup-progress").replaceChildren();
  $("backup-progress").classList.toggle("cleanup-stopped", job.status !== "completed" && job.cleanup?.phase !== "completed");
  $("backup-progress").textContent = (["discard","restore"].includes(task.mode) ? currentObservation && job.cleanup?.phase === "completed" : job.status === "completed" || job.cleanup?.phase === "completed") ? (task.mode === "restore" ? `Moved the original of ${task.name} back to its library path. Job history unchanged.` : `Removed recovery data for ${task.name}. ${task.mode === "discard" ? "Backup removed by your choice; library files and job history unchanged." : task.mode === "discard_duplicate" ? "Original preserved; failed job unchanged." : "Replacement verified."}`) : `Cleanup stopped for ${task.name}. ${cleanupError(job.cleanup?.error || task.error || job.error)} Remaining recovery data is listed below.`;
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
  const content = [node("p", backupName(copy), "cleanup-file"), node("p", `${bytes(Number(copy.bytes || 0)+Number(copy.partial_bytes || 0))} ${mode === "restore" ? "to move back, verified first" : mode === "discard" ? "will be permanently removed" : "to remove if verification passes"}`, "cleanup-space"), node("p", mode === "restore" ? "Checks the original against its recorded SHA-256 (a full read unless its file identity is unchanged since it was verified), then renames it back to its library path. Only runs while that path is empty; nothing is deleted or overwritten." : mode === "discard" ? "Deletes this backup only, without verifying the replacement or repairing the old job. This cannot be undone. Library files are untouched." : mode === "discard_duplicate" ? "Checks that the original and recovery copy match the recorded SHA-256. Removes only redundant recovery data. The original and failed job stay unchanged." : "Checks the active library file and both SHA-256 hashes. If any check fails, the recovery copy stays.")];
  const locations = node("details", "", "cleanup-locations"); locations.append(node("summary", "File locations"), node("p", `Library file: ${copy.original_path || "See job details"}`, "backup-path"), node("p", `${mode === "restore" ? "Original to move back" : "Copy to remove"}: ${copy.path || "See job details"}`, "backup-path")); content.push(locations);
  if (!await reviewAction({title:mode === "restore" ? "Move original back?" : mode === "discard" ? "Remove recovery copy?" : "Verify and remove extra copy", content, confirmLabel:mode === "restore" ? "Move back" : mode === "discard" ? "Remove copy" : "Verify & remove copy"}) || auth !== state.authRevision) return;
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
      state.tab === "library" && Date.now() - (state.fileMarksCheckedAt || 0) > 10000 ? refreshFileMarks() : Promise.resolve(),
    ]);
  } finally {
    polling = false;
  }
}, 5000);
if (typeof window !== "undefined") setConnection(serverReachable);

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
