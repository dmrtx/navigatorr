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
    // Keeping originals is a decision, not filing the job away: a shield, not a box.
    "Keep original": "M12 3l7 3v6c0 4-3 7-7 9-4-2-7-5-7-9V6ZM9 12l2 2 4-4",
    "Keep originals": "M12 3l7 3v6c0 4-3 7-7 9-4-2-7-5-7-9V6ZM9 12l2 2 4-4",
    "Stop batch · keep originals": "M6 6h12v12H6Z",
    "Compare frames": "M4 5h16v14H4ZM12 5v14",
    "Run one more test": "M9 3h6M10 3v6l-5 9a2 2 0 0 0 2 3h10a2 2 0 0 0 2-3l-5-9V3",
    "Check worker again": "M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14ZM20 20l-4-4",
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
async function api(path, body, { signal } = {}) {
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
async function tool(name, args = {}, onAccepted) {
  const mutates = ["action_run","action_resume","action_retry","action_cancel"].includes(name) || name === "transcode_backups" && ["clean","discard_duplicate","discard","restore"].includes(args.mode);
  const result = mutates
    ? await commandRequest("tool", {name,arguments:args,background:true}, onAccepted)
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
    if (button.dataset.jobControl === info.id) button.disabled = !serverReachable || jobIsBusy(info.id) || (button.dataset.requiresWorker === "true" && !state.workerInfo?.ready);
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
function commandLabel(command) { return commandLabels[command.kind] || command.label || "Update"; }
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
    return {kind:"failed", command:newest, text:commandFailures[label] || "Action failed", title:`${label} · Failed`};
  const active = commands.find((c) => !c.status || commandActiveStatuses.includes(c.status));
  if (active) {
    const activeLabel = commandLabel(active);
    const text = active.status === "waiting_external" ? "Waiting to apply" : commandVerbs[activeLabel] || "Applying";
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
    copies:q.get("copies") === "1", configure:q.get("configure") === "1",
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
        if (file) {
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
    open.append(node("span",seriesSeasonLabel(season.seasonNumber),"media-title"),node("span",`${season.fileCount} ${season.fileCount === 1 ? "file" : "files"}`,"metadata"));
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
async function loadGoalProfile(key) {
  const seq = state.goalProfileSeq, controller = new AbortController();
  state.goalProfileAbort = controller;
  const current = () => seq === state.goalProfileSeq && state.goalProfile?.key === key;
  const read = () => api(`source-profile?${new URLSearchParams({path: key})}`, undefined, {signal: controller.signal});
  let data = null;
  try {
    data = await read();
  } catch (err) {
    // Both read slots can still be held by files just left; ask once more.
    if (err?.status === 429 && current()) {
      await new Promise(resolve => setTimeout(resolve, 1500));
      if (current()) try { data = await read(); } catch {}
    }
  }
  if (!current()) return;
  state.goalProfileAbort = null;
  state.goalProfile = data?.profile && data.goals ? {...state.goalProfile, status: "ready", data} : {...state.goalProfile, status: "error"};
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
  add("Audio", p.audio?.mode === "copy" ? "Keep original audio tracks" : p.audio?.mode === "compact" ? "Compact · Convert lossless tracks to AAC, same layout; E-AC-3/AC-3/AAC copied" : p.audio?.mode === "compact_lossless" ? "Lossless to AAC · TrueHD, DTS-HD MA, PCM and FLAC become AAC, same layout; lossless Atmos/DTS:X lose object and height channels; lossy tracks (E-AC-3 Atmos included) copied" : p.audio?.mode === "he_aac" ? "HE-AAC · 64 kbps mono/stereo, 160 kbps surround (lossy)" : p.audio?.mode || "Profile default");
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
  refreshGoalProfile();
  renderGoalProfile();
  syncMoreGoals();
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
function updateSizeBudgetFields(prefix) {
  const kind = $(`${prefix}-kind`).value;
  $(`${prefix}-percent-label`).hidden = kind !== "percent";
  $(`${prefix}-gb-label`).hidden = kind !== "gb";
  $(`${prefix}-multiplier-label`).hidden = kind !== "siblings";
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
  const etas = active ? [...byFile.values()].map(a => a.eta).filter(eta => eta > 0) : [];
  const left = Math.max(0, batch.total - processed - count("waiting_decision") - count("cancelled"));
  result.append(node("span", etas.length
    ? `Current encode ${remainingLabel(Math.max(...etas))}${left > 0 ? ` · about ${left} ${left === 1 ? "file" : "files"} left` : ""}`
    : "Estimated workflow progress · Not time remaining", "batch-progress-explanation metadata"));
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
  const observation = workerProgressObservation(job);
  if (observation.message) {
    result.className = `job-progress ${observation.warning ? "stalled-progress" : "workflow-progress"}`;
    const measured = w?.progress ?? w?.last_known_progress?.progress;
    const last = measured != null && Number.isFinite(Number(measured))
      ? ` Last reported: ${Number(measured).toFixed(1)}%.` : "";
    result.append(node("span", observation.message + (observation.warning ? last : ""), "metadata"));
    return result;
  }
  const after = postEncodeWork(job);
  if (after) {
    result.append(node("span", [job.activity_file ? `Current: ${job.activity_file.split("/").pop()}` : null, after.step, after.label].filter(Boolean).join(" · "), "metadata"));
    if (after.read != null && after.total > 0) {
      const meter = document.createElement("meter");
      meter.min = 0;
      meter.max = after.total;
      meter.value = after.read;
      meter.setAttribute("aria-label", `${after.measure || "Original checked"} ${(after.read / after.total * 100).toFixed(1)}%`);
      result.append(meter, node("span", `${bytes(after.read)} / ${bytes(after.total)}`, "metadata"));
    }
    if (postEncodeWorkerPhases.includes(w?.transcode_phase) && w.last_progress_at)
      result.append(node("span", `Encode finished ${new Date(w.last_progress_at).toLocaleTimeString()}`, "metadata progress-observed-at"));
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
    meter.setAttribute("aria-label", `${w.progress_is_stale ? "Last measured" : "Worker"} progress ${p.toFixed(1)}%`);
    result.append(meter, node("span", `${w.progress_is_stale ? "Last measured: " : ""}${p.toFixed(1)}%`, "metadata"));
  }
  if (w.last_progress_at)
    result.append(
      node(
        "span",
        `${postEncodeWorkerPhases.includes(w.transcode_phase) ? "Encode finished" : "Measured"} ${new Date(w.last_progress_at).toLocaleTimeString()}`,
        "metadata progress-observed-at",
      ),
    );
  if (observation.active) {
    const last = activityElapsed(w.last_progress_at);
    result.append(node("span", `${heartbeatAlive(observation.heartbeat) ? `Worker heartbeat ${activityElapsed(w.worker_heartbeat_at)} ago` : "Worker heartbeat unavailable"}${last ? ` · Last advance ${last} ago` : ""}`, "metadata progress-observation"));
    if (phase === "analysing_video")
      result.append(node("span", "The first x265 pass analyzes the video. Encoding progress starts during the second pass.", "metadata progress-observation"));
    if (phase === "validating_quality")
      result.append(node("span", "Final quality validation can take several minutes. The percentage reflects the last encoding measurement.", "metadata progress-observation"));
  }
  return result;
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
async function jobControl(id, name, args = {}, {onAccepted} = {}) {
  if (jobIsBusy(id)) return;
  state.busyJobs.add(id);
  document.querySelectorAll("[data-job-control]").forEach((b) => {
    if (b.dataset.jobControl === id) b.disabled = true;
  });
  try {
    await tool(name, { id, ...args }, onAccepted);
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
      await Promise.all([loadJobs(),state.detail === id && $("job-detail").open ? refreshDetail() : Promise.resolve()]);
      notify(selected === "reject" ? "Candidate rejected. Original preserved; other batch files continue." : replaceAfter ? "Candidate accepted. Preparing the replacement review…" : "Candidate accepted. Replacement requires separate approval.");
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
    // One review follows the plan: it opens now and fills in as soon as the
    // files are verified, so approving needs no second button.
    await reviewFilePromotion(result.id, { wait: true });
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
  if (["running", "Running", "waiting_external", "In progress", "Encoding", "Measuring quality", "Preparing", "Verifying", "Replacing", "Replacement in progress", "Saving candidate", "Verifying quality"].includes(status)) return "running";
  if (["failed", "Failed", "Worker offline"].includes(status)) return "failed";
  // Missing telemetry is worth attention, but nothing has failed: the worker
  // keeps the job and the original is untouched.
  if (["review", "waiting_decision", "Needs review", "Needs decision", "Needs attention", "partial", "Partial", "Cancelling", "No heartbeat", "No updates"].includes(status)) return "waiting_decision";
  if (["Preview", "Preview complete", "Eligible"].includes(status)) return "preview";
  return "neutral";
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
    $("reconfigure-note").textContent = "Same task · Originals kept · Previous attempts saved";
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

function updateReconfigureHardware() {
  $("reconfigure-hardware-option").hidden = !(state.reconfigurePlan?.mode && $("reconfigure-mode").value === "size");
  $("reconfigure-budget-option").hidden = $("reconfigure-hardware-option").hidden;
  updateSizeBudgetFields("reconfigure-budget");
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
// openGoalForm opens the library's goal form for one file.
function openGoalForm(path) {
  $("path").value = path || "";
  $("scope").value = "file";
  state.fileMedia = null;
  state.fileService = null;
  selectTab("library");
  setFileStep("configure");
  controls();
}
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
      label === "Try with x265" ||
      Object.values(remedyActions).some(r => r.label === label) ||
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
    cancel: job.batch ? "Stop batch · keep originals" : "Keep original",
    recheck: "Check worker again",
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
            await jobControl(job.id, "action_resume", {decision:"cancel"});
          return;
        }
        if (choice.decision === "reject" && job.batch?.promotion_plan_ready) {
          const plan = await readBatchPromotionPlan(job.id);
          if (!plan?.digest) throw new Error("Reopen the replacement review before deciding.");
          await commandRequest("batch-candidates",{id:job.id,digest:plan.digest,decision:"reject"});
          return;
        }
        if (choice.decision === "approve" && job.action_name === "transcode_batch") {
          // A batch with a replacement plan is approved only against that
          // plan. Without one this is a forwarded per-file decision.
          if (job.batch?.promotion_plan_ready) {
            await reviewBatchPromotion(job.id);
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
        if (!["resume", "pause", "retry"].includes(choice.decision) && !(await confirmDecision(job, choice, label)))
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
    add("Review candidates", () => reviewBatchCandidates(job.id));
  const reconfigurable = job.action_name === "transcode_batch" && !job.batch?.dry_run && !job.batch?.promotion_plan_ready;
  if (job.parent_item_key && job.parent_action_id && !job.candidate_ready && !job.replacement_action_id && ["completed","failed","cancelled","waiting_decision"].includes(job.status) && !(job.mode && sourceGateReason(job.reason_code)))
    add(job.mode ? "Change goal" : "Change settings", () => reconfigureBatch(job.parent_action_id, false, {item_key:job.parent_item_key}));
  if (reconfigurable && !(job.mode && onlyUnsupportedSources(job))) add(job.mode ? "Change goal" : job.batch?.outcome === "no_changes" ? "Try another profile" : "Change settings", () => reconfigureBatch(job.id));
  if (job.status === "failed" && !reconfigurable && !job.parent_item_key) {
    if (rejectedCandidate(job)) {
      if (job.parent_action_id) add("Change settings", () => reconfigureBatch(job.parent_action_id, true, {candidate_id:job.id}));
    } else if (job.action_name === "benchmark_transcode")
      add("Choose a goal", () => openGoalForm(job.source_path));
    else add("Retry", () => jobControl(job.id, "action_retry"));
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
  if (job.preview_action_id && detail)
    add("View preview", () => openJob(job.preview_action_id));
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
  };
  if (reasons[text]) return reasons[text];
  if (isCandidateRejection(text)) return "Candidate rejected by you; original kept";
  if (workerBusyFailure(text)) return workerBusyFailureText;
  const growth = text.match(/Candidate file size \(\d+ bytes\) exceeds original \(\d+ bytes\) by ([\d.]+)%, which is greater than max_size_increase_percent \(([\d.]+)%\)/);
  if (growth) return `Candidate is ${growth[1]}% larger; allowed growth is ${growth[2]}%`;
  if (/no such file or directory/i.test(text)) return "File not found";
  if (/permission denied/i.test(text)) return "Permission denied";
  if (/smb_transport_error|connecting to SMB server/.test(text)) return "Worker cannot access media storage. Fix the connection before retrying.";
  if (/shared VMAF\/CAMBI calibration supports 8-bit SDR video below 45 fps/.test(text))
    return "Automatic testing does not support this video format";
  if (/^Encoding finished for job .*publication requires intervention/.test(text)) return "Encoding finished, but saving the candidate failed. The candidate is kept; check storage, then ask the worker again";
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
  const phase = job.worker?.transcode_phase || job.worker?.phase;
  const multipleActive = job.activities?.length > 1;
  const offline = multipleActive
    ? job.activities.every(activity => activity.waiting_condition === "worker_unreachable")
    : workerUnavailable(job);
  const observation = workerProgressObservation(job);
  const replacementComplete = job.savings?.realized_saved_bytes != null ||
    (job.replaced && job.status === "completed");
  const skippedFile = job.action_name === "transcode_media" && job.status === "completed" && !job.candidate_ready && ["quality_not_met","insufficient_savings","size_growth","size_growth_exceeded","preservation_not_met","already_target_codec","no_suitable_candidate","audio_compaction_insufficient","audio_estimate_unavailable","audio_only_worker_unsupported","unsupported_source","hdr_requires_compact","compact_not_needed"].includes(job.reason_code);
  const status = skippedFile ? "Skipped" : job.status === "waiting_decision" && job.reason_code === "search_budget_exhausted" ? "Needs attention" : job.status === "failed" && !rejectedCandidate(job) && !savingsPolicyFailure(job.error) ? "Failed" : job.replaced_files > 0 && job.remaining_candidates > 0 ? "Candidates ready" : replacementComplete
    ? "Replaced"
    : rejectedCandidate(job) ? "Rejected" : savingsPolicyFailure(job.error) ? "Minimum savings not met" : outcomes[batch?.outcome] ||
      (job.candidate_ready && !job.replacement_action_id
        ? "Candidate ready"
        : ["running", "waiting_external"].includes(job.status)
          ? (offline ? "Worker offline" : multipleActive ? "In progress" : observation.status || {encoding:"Encoding",validating:"Verifying",validating_quality:"Verifying quality",analysing_video:"Analyzing video",publishing:"Saving candidate",queued:"Queued on worker",preparing:"Preparing"}[phase] || (replacement || job.promotion ? "Replacing" : "In progress"))
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
    metrics.push(s.estimated_saved_bytes < 0 ? `about ${bytes(-s.estimated_saved_bytes)} larger (estimate)` : `${bytes(s.estimated_saved_bytes)} estimated savings`);
  let result;
  if (skippedFile) result = shortJobReason(job.reason_code);
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
// The next step a card offers, most important first. Only one is labelled.
// Shortened visible labels for main actions. Each is a prefix of the full
// label, which stays the accessible name (WCAG 2.5.3 Label in Name).
const shortActionLabels = {"Replace file":"Replace","Choose a goal":"Choose","Review candidates":"Review","Review candidate":"Review","Review replacement":"Review","Review replacements":"Review","Review preview":"Review","Validate classifications":"Validate","Check worker again":"Check worker","Check library again":"Check library","Resume batch":"Resume","Start batch":"Start","Try another profile":"Try another","Convert to SDR 1080p":"Convert to SDR"};
const mainQueueActions = ["Replace file", "Review replacements", "Review replacement", "Review candidates", "Review candidate", "Review cuts", "Validate classifications", "Review", "Resume batch", "Start batch", "Choose a goal", "Retry", "Convert to SDR 1080p", "Make it smaller", "Check worker again", "Check library again", "Review preview", "Try another profile"];
const visibleQueueIcons = 3;
function compactQueueActions(actions) {
  // A card shows at most one labelled main action, then square icon buttons
  // whose full name lives in aria-label and title. Up to four icons show; with
  // five or more, three stay and the rest move, as the same elements with
  // their handlers and state, into a More actions menu that lists them with
  // icon and label.
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
      b.replaceChildren(...[icon, node("span", shortActionLabels[label] || label, "action-label")].filter(Boolean));
      b.className = "action-main primary";
      continue;
    }
    // The label stays in the DOM for the More menu; the row shows the icon.
    b.replaceChildren(...[icon, node("span", label, "action-label")].filter(Boolean));
    b.className = ["Cancel", "Stop batch · keep originals"].includes(label) ? "action-icon danger" : "action-icon";
    icons.push(b);
  }
  // A menu never holds a single item: four icons fit where three and More do.
  const shown = icons.length > visibleQueueIcons + 1 ? visibleQueueIcons : icons.length;
  const overflow = icons.slice(shown);
  const rail = [main, ...icons.slice(0, shown)].filter(Boolean);
  if (overflow.length) {
    const more = node("details", "", "more-actions");
    more.dataset.menuFor = buttons[0]?.dataset.jobControl || "";
    const toggle = node("summary", "", "action-icon more-toggle");
    toggle.setAttribute("aria-label", "More actions");
    toggle.title = "More actions";
    const dots = actionIcon("More actions");
    if (dots) toggle.append(dots);
    const menu = node("div", "", "more-menu");
    menu.append(...overflow);
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
    rail.push(more);
  }
  actions.replaceChildren(...rail);
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
  if (menu) menu.querySelector("summary")?.focus();
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
  if (size.video_copy) lines.push({verified: "Video copied unchanged · verified bit-identical", failed: "Video copy check failed · original kept"}[size.video_copy] || "Video will be copied unchanged · not yet verified; only lossless audio is compacted");
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
  if (job.candidate_ready) return `Original preserved · Candidate available${belowTargetLabel(job.size_budget_outcome) ? ` · ${belowTargetLabel(job.size_budget_outcome)}` : ""}`;
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
    renderDecisionCount(data);
    if (!more) state.operationJobs = new Map();
    state.operationJobs ||= new Map();
    for (const job of page) state.operationJobs.set(job.workflow_id || job.id, job);
    state.jobsLoaded = offset + page.length;
    state.jobsHasMore = data.has_more;
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
      heading.append(
        button(singleFile?.display_label || presentation.title, () => openJob(job.id), "job-title"),
      );
      heading.append(statusBadge(presentation.status, presentation.status, job.status === "waiting_decision" || job.batch?.outcome === "no_changes" ? () => openJob(job.id) : null));
      // A goal batch keeps encoding while one file waits, so its own status says
      // "In progress"; the decision still needs its own way in.
      if (job.needs_decision && job.status !== "waiting_decision") heading.append(statusBadge("Needs decision", "waiting_decision", () => openJob(job.id)));
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
    restoreCommandFocus(commandFocused);
    restoreActionMenu($("jobs-list"), menu);
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
  usage.hidden = !slots?.split;
  if (slots?.split) usage.textContent = `Video ${slots.video.used}/${slots.video.total} · Podcast ${slots.podcast.used}/${slots.podcast.total} · Ceiling ${slots.ceiling}`;
  $("worker-slot-split").hidden = !slots || slots.ceiling < 2;
  if (!slots) return;
  $("worker-slot-ceiling").textContent = `The worker's ceiling is ${slots.ceiling} slots${slots.host_cpus ? ` on ${slots.host_cpus} CPUs` : ""}. Video and podcast slots together cannot exceed it.`;
  // Never overwrite what is being typed or has been edited and not saved.
  if (!state.slotFormDirty && !$("worker-slot-form").contains(document.activeElement)) {
    $("slot-video").value = slots.split ? slots.video.total : Math.max(1, slots.ceiling - 1);
    $("slot-podcast").value = slots.split ? slots.podcast.total : 1;
    $("slot-video").max = $("slot-podcast").max = Math.max(1, slots.ceiling - 1);
  }
}
async function saveWorkerSlots(video, podcast) {
  const note = $("worker-slot-note");
  note.textContent = "Saving…";
  try {
    const result = await api("worker-slots", {video_slots: video, podcast_slots: podcast});
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
function renderWorkerActivity(data) {
  $("worker-slots").replaceChildren();
  if (!serverReachable || !data?.available) {
    $("worker-capacity").textContent = "Worker slots · Unknown";
    $("activity-checked").textContent = "";
    $("worker-slot-usage").hidden = true;
    $("worker-slot-split").hidden = true;
    $("worker-slots").append(node("p",data?.message || "Worker disconnected; slot occupancy is unknown.","metadata"));
    return;
  }
  const activity = data.activity;
  $("worker-capacity").textContent = `Worker slots · ${activity.worker_slots_used}/${activity.worker_slots_total} occupied`;
  $("activity-checked").textContent = "Updated now · Job reservations";
  renderSlotUsage(activity);
  const jobs = activity.jobs || [];
  const pools = slotPools(activity);
  const shown = Math.min(16, pools.total);
  // Keep a live reservation in the same visual slot as other jobs finish.
  // Slot labels describe this node's reservations, not physical CPU cores.
  const assigned = state.activitySlotIDs ||= new Map();
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
  for (let i=0;i<shown;i++) {
    const row = node("div","","worker-slot"), job = jobs.find(job => assigned.get(job.id) === i);
    if (!job) {
      const unknownOccupied = !activity.details_available && i < activity.worker_slots_used;
      row.append(node("strong",`${pools.label(i)} · ${unknownOccupied ? "Occupied" : "Available"}`));
      row.append(node("p",unknownOccupied ? "File details require the updated video worker." : "Ready for the next job.","metadata"));
    } else {
      const stale = !heartbeatAlive(workerHeartbeatState(job.heartbeat_at));
      row.append(node("strong",`${pools.label(i)} · ${job.file}`));
      row.children[0].title = job.file;
      const phase = workerPhaseLabel(job.phase);
      const parts = [job.kind === "podcast" ? "Podcast processing" : "Full conversion",phase,activityElapsed(job.started_at) ? `${activityElapsed(job.started_at)} elapsed` : null];
      if (job.progress != null) parts.push(`${Number(job.progress).toFixed(1)}%`);
      row.append(node("p",parts.filter(Boolean).join(" · "),"metadata"));
      const last = activityElapsed(job.last_progress_at);
      row.append(node("p",stale ? "No recent worker heartbeat. Last reported state." : last ? `Last advance ${last} ago` : "Worker connected · Waiting for the next measurement", "metadata"));
    }
    $("worker-slots").append(row);
  }
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
  if (!entries.length) return;
  const detail = node("details", "", "phase-costs");
  detail.append(node("summary", "Measured phase costs"));
  detail.append(node("p", "Elapsed wall measurements include synchronous I/O. Coordinator measurements count returned invocations and exclude waits between them. CPU time is the user + system time of the worker's ffmpeg/ffprobe processes where the worker measured it; otherwise CPU time is unknown; phases may overlap and are not added into a total.", "metadata"));
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
    // The summary line already says why a file was skipped. An outcome line that
    // only repeats text already shown is left out, so the reason appears once.
    const shownOutcomes = [queuePresentation(job).summary];
    const addOutcome = (text) => {
      if (!text || shownOutcomes.some(shown => shown.includes(text))) return;
      shownOutcomes.push(text);
      summary.append(node("p", text, "job-outcome metadata"));
    };
    if (job.podcast) { const p=job.podcast; summary.append(node("p",`${p.classified_blocks || 0}/${p.total_blocks || 0} transcript blocks classified · ${((p.removed_ms || 0)/1000).toFixed(1)} seconds removed · ${p.known_ad_units || 0} units verified by known ads`,"metadata")); if (p.output_path && job.status === "completed") summary.append(node("p",`Validated MP3: ${p.output_path}`,"metadata")); }
    if (job.transcode) {
      // A waiting decision repeats its reason below, and a result card
      // explains the outcome itself; show it only once.
      const hasReport = hasResultReport(job.benchmark);
      if (job.status !== "waiting_decision" && !hasReport) addOutcome(jobOutcomeText(job));
      if (job.transcode.tested_candidates) summary.append(node("p", `${job.transcode.tested_candidates} configurations tested${job.transcode.verified_samples ? ` · ${job.transcode.verified_samples} quality samples verified` : ""}`, "metadata"));
    }
 renderPhaseCosts(summary, job);
    const report = resultReport(job);
    if (report) summary.append(report);
    const plan = encodingPlanNote(job);
    if (plan) summary.append(plan);
    const size = sizeModeNote(job);
    if (size) summary.append(size);
    if (queuePresentation(job).showTelemetry) summary.append(telemetry(job));
    else if (batchProgress(job)) summary.append(batchProgress(job));
    else if (!job.batch?.dry_run && job.batch?.outcome !== "no_changes") addOutcome(queueOutcome(job));
    if (job.batch?.outcome === "no_changes") summary.append(skippedBatchOutcome(job));
    if (job.batch?.dry_run) summary.append(node("p", queueOutcome(job) + (job.batch.queued > 0 && !job.preview_execution_action_id ? ". Review the eligible files, then choose Start batch to convert them." : "."), "preview-notice"));
    const raw = job.status === "waiting_decision" ? job.waiting_reason : job.status === "failed" ? job.error : "";
    if (raw && shortJobReason(raw) !== raw && !workerBusyFailure(raw)) {
      const technical = node("details", "", "technical-reason");
      technical.append(node("summary", "Technical details"), node("p", raw, "metadata"));
      summary.append(technical);
    }
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
    return workerPhaseLabel(phase) + (observation.message ? ` · ${observation.message}` : worker.progress_is_stale && heartbeatAlive(observation.heartbeat) ? " · Worker heartbeat received; last measurement shown" : eta ? ` · ${eta}` : "");
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
  if (!job || job.parent_action_id !== batchID || !job.candidate_ready || job.replacement_action_id) throw new Error("This candidate changed. Refresh the batch before reviewing replacement.");
  await prepareReplacement(job);
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
  const focus = focusedControl($("batch-items-list"));
  $("batch-items-list").replaceChildren();
  for (const item of response.items || []) {
    const row = node("div", "", "batch-item-row");
    const label = item.replaced ? "Replaced" : item.replacement_action_id ? ({waiting_decision:"Replacement needs decision",failed:"Replacement failed",cancelled:"Replacement cancelled"}[item.replacement_status] || "Replacement in progress") : item.candidate_ready ? "Candidate ready" : item.status === "waiting_decision" && item.reason_code === "search_budget_exhausted" ? "Needs attention" : savingsPolicyFailure(item.error) ? "Below minimum savings" : rejectedCandidate(item) ? "Rejected" : state.detailJob?.batch?.dry_run && item.status === "queued" ? "Eligible" : names[item.status] || item.status || item.decision;
    const review = item.replacement_action_id ? () => openJob(item.replacement_action_id) : item.status === "waiting_decision" && item.child_action_id ? () => openJob(item.child_action_id) : ["skip","review"].includes(item.status) && item.can_reconfigure ? () => reconfigureBatch(id, false, {item_key:item.item_key}) : item.candidate_ready ? () => openJob(item.child_action_id) : null;
    row.append(node("span", item.display_label || item.file_path, "metadata"), statusBadge(label, item.replacement_status || label, review));
    if (item.child_action_id)
      row.append(
        button("View file", () => openJob(item.child_action_id), "quiet"),
      );

    const actions = node("div", "", "batch-file-actions");
    const add = (label, fn) => {
      const control = button(label, fn, label === "Replace file" ? "primary" : "");
      control.dataset.jobControl = id;
      control.dataset.batchItemKey = item.item_key;
      control.disabled = !serverReachable || jobIsBusy(id);
      actions.append(control);
    };
    if (item.candidate_ready && state.info.allow_destructive) add("Replace file", () => replaceBatchFile(id, item.child_action_id));
    if (item.replacement_action_id) add("View replacement", () => openJob(item.replacement_action_id));
    const unsupportedSource = item.mode && (sourceGateReason(item.reason_code) || item.reasons?.some(sourceGateReason));
    if (item.can_reconfigure && !unsupportedSource && ["skip","review","failed","cancelled","waiting_decision"].includes(item.status)) add(item.mode ? "Change goal" : "Change settings", () => reconfigureBatch(id, false, {item_key:item.item_key}));
    if (actions.children.length) row.append(actions);
    const activity = batchFileActivity(item, state.detailJob);
    if (activity) row.append(node("span", activity, "batch-file-activity"));
    if (item.mode) row.append(node("span", `Goal: ${item.mode === "compact" ? `Shrink to ${item.max_height || 1080}p SDR` : goalNames[item.mode] || item.mode}`, "metadata batch-file-profile"));
    else if (item.requested_profile || item.profile) row.append(node("span", `${item.retry_pending ? "Next attempt" : "Profile"}: ${profileOptionLabel(item.requested_profile || item.profile)}`, "metadata batch-file-profile"));
    if (item.previous_attempts?.length) {
      const history=node("details","","batch-attempt-history");
      history.append(node("summary",`${item.previous_attempts.length} previous ${item.previous_attempts.length===1 ? "attempt" : "attempts"}`));
      item.previous_attempts.forEach((attempt,index)=>history.append(button(`Attempt ${index+1} · ${names[attempt.status] || attempt.status}`,()=>openJob(attempt.child_action_id),"quiet")));
      row.append(history);
    }
    const reasons = item.waiting_reason ? [item.waiting_reason] : item.reason_code ? [item.reason_code] : item.error ? [item.error] : item.reasons || [];
    const visibleReasons = [...new Set(reasons.filter(reason => reason && !/^(explicit profile requested|eligible for transcode|per-source bounded samples required before full encode|New settings requested; previous attempt retained in history)$/i.test(reason)).map(shortJobReason))];
    if (!visibleReasons.length && item.status === "skip") visibleReasons.push("This file was omitted by the processing plan; original kept. View the file details or change settings to try again.");
    for (const reason of visibleReasons) row.append(node("span", reason, "muted"));
    if (item.candidate_ready && belowTargetLabel(item.size_budget_outcome)) row.append(node("span", belowTargetLabel(item.size_budget_outcome), "badge size-budget-badge"));
    if (item.candidate_ready) row.append(node("span", state.info.allow_destructive ? "Candidate verified · Original kept until replacement is approved" : "Candidate verified · Replacement is disabled in server settings", "muted"));
    for (const control of row.querySelectorAll("button")) {
      control.dataset.batchItemKey = item.item_key;
      control.dataset.jobControl ||= item.child_action_id || id;
    }
    $("batch-items-list").append(row);
  }
  restoreControl($("batch-items-list"), focus);
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
async function readBatchPromotionNotice(id) {
  try {
    const notice = (await tool("action_detail", { id, section: "state", key: "batch_promotion_notice", chunk: 0 })).data;
    return notice && typeof notice === "object" ? notice : null;
  } catch {
    return null;
  }
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
function showFileReview(job, approval) {
  const p = job.promotion;
  Object.assign(approval, { digest: p.plan_digest, original: p.original_sha256, candidate: p.candidate_sha256, pending: false });
  const data = $("file-review-data");
  data.replaceChildren(node("p", p.original_path.split("/").pop(), "review-filename"));
  for (const [label, path, size] of [["Original", p.original_path, p.original_bytes], ["Verified candidate", p.candidate_path, p.candidate_bytes]]) {
    const row = node("div", "", "review-file");
    row.append(node("span", label, "muted"), node("strong", bytes(size)), node("span", path, "metadata review-path"));
    data.append(row);
  }
  const savings = savingsLine(job.savings);
  if (savings) data.append(savings);
  data.append(node("p", `Method: ${replacementMethods[p.service] || p.service || "Replace the file in place"}`, "metadata"));
  const warning = regrabWarning(p);
  if (warning) data.prepend(warning);
  const below = belowTargetNotice(job.size_budget_outcome);
  if (below) data.prepend(below);
  $("approve-file-review").disabled = !serverReachable || state.approvingFile;
}
function showFileReviewPending(text) {
  $("file-review-data").replaceChildren(node("p", text, "muted"));
  $("approve-file-review").disabled = true;
}
function pollDelay(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}
// Keeps the open review current until the plan is ready or stops being
// possible; closing the dialog ends it and leaves the job waiting.
async function awaitFileReview(approval, auth) {
  const live = () => state.fileApproval === approval && $("file-review").open && auth === state.authRevision;
  while (live()) {
    await pollDelay(1500);
    if (!live()) return;
    let job;
    try {
      job = (await api(`operations?id=${encodeURIComponent(approval.id)}`)).jobs?.[0];
    } catch {
      continue;
    }
    if (!live()) return;
    if (replacementReady(job)) return showFileReview(job, approval);
    if (!job || ["failed", "cancelled", "completed"].includes(job.status)) return showFileReviewPending(shortJobReason(job?.error) || "This replacement can no longer be approved. Nothing was changed.");
    if (job.status === "waiting_decision") return showFileReviewPending("This replacement is waiting for a decision that is not an approval. Open the job to continue.");
    showFileReviewPending("Verifying the original and the candidate. Nothing changes until you approve; this can take a few minutes for large files.");
  }
}
async function reviewFilePromotion(id, { wait = false } = {}) {
  const auth = state.authRevision;
  const job = (await api(`operations?id=${encodeURIComponent(id)}`)).jobs?.[0];
  if (auth !== state.authRevision || $("workspace").hidden) return;
  const ready = replacementReady(job);
  if (!ready && !(wait && job && ["pending", "running", "waiting_external"].includes(job.status)))
    throw new Error("This replacement is no longer waiting for approval. Refresh the queue.");
  const approval = state.fileApproval = { id, digest: "", original: "", candidate: "", pending: !ready };
  if (ready) showFileReview(job, approval);
  else {
    showFileReviewPending("Verifying the original and the candidate. Nothing changes until you approve; this can take a few minutes for large files.");
    awaitFileReview(approval, auth);
  }
  $("file-review").showModal();
}
$("dismiss-file-review").addEventListener("click", () => $("file-review").close());
$("file-review").addEventListener("close", () => { state.fileApproval = null; });
$("approve-file-review").addEventListener("click", () => safe(async () => {
  const approval = state.fileApproval;
  if (!approval || approval.pending || !approval.digest || state.approvingFile || !serverReachable) return;
  state.approvingFile = true;
  $("approve-file-review").disabled = true;
  try {
    const current = (await api(`operations?id=${encodeURIComponent(approval.id)}`)).jobs?.[0];
    if (state.fileApproval !== approval || !$("file-review").open) return;
    if (current?.status !== "waiting_decision" || !current.waiting_options?.some(option => option.decision === "approve") || current.promotion?.original_sha256 !== approval.original || current.promotion?.candidate_sha256 !== approval.candidate || current.promotion?.plan_digest !== approval.digest)
      throw new Error("The replacement plan changed. Close this review and open it again.");
    // The server refuses an approval whose digest is not the reviewed plan's.
    // Once it accepts, the review is done: progress shows on the job's row.
    await jobControl(approval.id, "action_resume", { decision: "approve", plan_digest: approval.digest }, {onAccepted: () => $("file-review").close()});
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
    const belowTarget=belowTargetNotice(review.size_budget_outcomes?.[member.transcode_action_id]);if(belowTarget)row.append(belowTarget);
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
  const notice = await readBatchPromotionNotice(id);
  if (authRevision !== state.authRevision || $("workspace").hidden) return;
  state.batchApproval = {id,digest:plan.digest};
  const host=$("batch-review-data");
  host.replaceChildren(node("p",`${plan.members.length} ${plan.members.length === 1 ? "candidate selected" : "candidates selected"}`,"metadata"));
  const warning = regrabWarning(notice);
  if (warning) host.prepend(warning);
  for (const member of plan.members) {
    const row=node("div","","candidate-review-row");
    row.append(node("strong",member.episode || member.original_path.split("/").pop()));
    for (const [label,path] of [["Original",member.original_path],["Candidate",member.candidate_path]]) {
      const line=node("p","","metadata backup-path");line.append(node("span",`${label}: `),node("span",path));row.append(line);
    }
    host.append(row);
    // The plan does not carry how a candidate was made; look up the few that
    // came from a size budget and label them without delaying the review.
    if (member.transcode_action_id && plan.members.length <= 50) api(`operations?id=${encodeURIComponent(member.transcode_action_id)}`).then(found => {
      const notice = belowTargetNotice(found.jobs?.[0]?.size_budget_outcome);
      if (notice) row.append(notice);
    }).catch(() => {});
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
    ]);
  } finally {
    polling = false;
  }
}, 5000);
if (typeof window !== "undefined") setConnection(serverReachable);

$("close-podcast").addEventListener("click", () => $("podcast-dialog").close());
$("podcast-new").addEventListener("click", () => { $("podcast-dialog").showModal(); safe(async () => { const data=await api("podcasts"); const select=$("podcast-id"); select.replaceChildren(); for (const id of data.podcasts || []) option(select,id,id); $("podcast-create-status").textContent=data.enabled && data.podcasts?.length ? "" : "Enable a podcast in the server configuration first."; }); });
$("podcast-form").addEventListener("submit", event => { event.preventDefault(); safe(async () => { const result=await tool("action_run",{action:"clean_podcast_ads",inputs:JSON.stringify({podcast_id:$("podcast-id").value,path:$("podcast-source").value,output_path:$("podcast-output").value})}); $("podcast-dialog").close(); await loadJobs(); await openJob(result.id || result.action?.id); }); });
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
