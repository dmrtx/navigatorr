// Synthetic jobs for the layout matrix. Nothing here comes from a real library.
//
// A scenario is one state a job can be in, built from small parts so a new state
// is a few lines. `variant` changes the content around the state (long names,
// unbroken names, big or tiny sizes) so the same state is checked against text
// that stretches it. The shapes follow what the console reads from
// /api/maintenance/operations and /batch-items.

const GB = 1e9, MB = 1e6;

export const nameVariants = {
  short: { show: "Silo", season: "Season 2", file: n => `Silo - S02E0${n} - Pilot.mkv`, batchTitle: "Silo · Season 2" },
  long: {
    show: "The Extraordinarily Long Running Documentary Series About Everything",
    season: "Season 12 — The Complete Remastered Extended Edition",
    file: n => `The Extraordinarily Long Running Documentary Series About Everything - S12E0${n} - The One Where Nothing Fits On A Phone Screen WEBDL-2160p HDR10 Proper.mkv`,
    batchTitle: "The Extraordinarily Long Running Documentary Series About Everything · Season 12 — The Complete Remastered Extended Edition",
  },
  // No spaces at all: the browser cannot wrap it, so the layout must.
  unbroken: {
    show: "Monogatari",
    season: "Season3",
    file: n => `Monogatari.S03E0${n}.Tsubasa.Tiger.Part.${n}.BluRay.1080p.x264.DUAL.AAC2.0.H.264-GROUP[rarbg]${"x".repeat(60)}.mkv`,
    batchTitle: `Monogatari.S03.Complete.BluRay.1080p.${"x".repeat(70)}`,
  },
  cjk: { show: "物語シリーズ", season: "第三期", file: n => `物語シリーズ - S03E0${n} - 翼タイガー 前編 BD-1080p.mkv`, batchTitle: "物語シリーズ · 第三期" },
};

// Sizes: a normal file, a huge one, a tiny one, and a candidate that grew.
export const sizeVariants = {
  normal: { source: 4.2 * GB, candidate: 1.1 * GB },
  huge: { source: 1.8e12, candidate: 9.9e11 },
  tiny: { source: 3 * MB, candidate: 1 * MB },
  larger: { source: 1 * GB, candidate: 1.3 * GB },
};

let counter = 1000;
const id = prefix => `${prefix}-${++counter}`;
const path = (v, n = 1) => `/media/TV/${v.show}/${v.season}/${v.file(n)}`;
const iso = ago => new Date(Date.UTC(2026, 9, 25, 12, 0, 0) - ago * 1000).toISOString();
const worker = (phase, extra = {}) => ({ phase, transcode_phase: phase, worker_heartbeat_at: iso(2), last_progress_at: iso(3), ...extra });

const encodeStages = [{ name: "preflight", status: "completed" }, { name: "submit_transcode", status: "completed" }, { name: "wait_transcode", status: "running" }, { name: "validate_result", status: "pending" }];
const batchStages = (at, final = "promote_batch") => [
  { name: "resolve_batch", status: "completed" },
  { name: "schedule_batch", status: at > 1 ? "completed" : "running" },
  ...(final ? [{ name: final, status: at > 2 ? "completed" : at === 2 ? "running" : "pending" }] : []),
];

function file(v, v2, n, extra = {}) {
  const sizes = sizeVariants[v2] || sizeVariants.normal;
  const childId = id("child");
  return {
    item_key: `item-${n}`, child_action_id: childId, file_path: path(v, n), display_label: v.file(n), status: "completed",
    candidate_ready: true, saved_bytes: sizes.source - sizes.candidate, can_reconfigure: true,
    tracks_message: "Kept all 2 audio tracks and 2 of 3 subtitles; dropping the rest saves about 0 MB", ...extra,
  };
}

// ---- single files ----------------------------------------------------------

const replaceStages = ["plan_promotion", "approve_promotion", "preserve_original", "import_candidate", "remove_old_file", "rename_candidate", "rescan_library", "finalize_promotion"];
function single(v, s, over) {
  const sizes = sizeVariants[s] || sizeVariants.normal;
  return {
    id: id("job"), workflow_id: undefined, number: 1213, status: "running", action_name: "transcode_media", source_path: path(v),
    current_step: 2, stages: encodeStages, created_at: iso(900), updated_at: iso(2), can_archive: true, logs_available: true,
    savings: { source_bytes: sizes.source, candidate_bytes: undefined }, size_mode: { expected_bytes: sizes.candidate },
    ...over,
  };
}

// ---- batches ---------------------------------------------------------------

function batch(v, s, over, items = []) {
  const job = {
    id: id("batch"), number: 1219, status: "running", action_name: "transcode_batch", current_step: 1, stages: batchStages(1),
    created_at: iso(3600), updated_at: iso(4), can_archive: true, logs_available: true,
    batch: { total: items.length || 8, completed: 0, running: 0, queued: 0, title: v.batchTitle, ...over.batch },
    batch_files: { file_count: items.length || 8, context: v.batchTitle, name: v.batchTitle },
    savings: { source_bytes: 22.07 * GB, candidate_bytes: 11 * GB },
    ...over,
  };
  job.batch = { total: items.length || 8, completed: 0, running: 0, queued: 0, title: v.batchTitle, ...over.batch };
  job.workflow_id = job.id;
  return { job, items };
}

const itemsOf = (v, s, n, make) => Array.from({ length: n }, (_, i) => file(v, s, i + 1, make(i)));
const member = (item, over = {}) => ({ item_key: item.item_key, name: item.display_label, phase: "waiting", percent: null, ...over });

export const scenarios = [
  // -- one file, working
  { id: "file-queued", build: (v, s) => ({ job: single(v, s, { status: "pending", current_step: 0, worker: worker("queued", { queue_position: 3 }) }) }) },
  { id: "file-analysing", build: (v, s) => ({ job: single(v, s, { worker: worker("analysing_video") }) }) },
  { id: "file-encoding", build: (v, s) => ({ job: single(v, s, { worker: worker("encoding", { progress: 42.5, speed: 1.35, fps: 61.2 }), savings: { source_bytes: sizeVariants[s].source, eta_seconds: 1500 } }) }) },
  { id: "file-encoding-zero", build: (v, s) => ({ job: single(v, s, { worker: worker("encoding", { progress: 0, speed: 0.4 }) }) }) },
  { id: "file-encoding-almost", build: (v, s) => ({ job: single(v, s, { worker: worker("encoding", { progress: 99.9, speed: 2.1 }) }) }) },
  { id: "file-encoding-stale", build: (v, s) => ({ job: single(v, s, { worker: worker("encoding", { progress: 61.2, progress_is_stale: true, worker_heartbeat_at: iso(400), last_progress_at: iso(900) }) }) }) },
  { id: "file-ladder", build: (v, s) => ({ job: single(v, s, { worker: worker("encoding", { progress: 12.3 }), transcode: { message: "x265 CRF 22 (step 2 of 5)" }, size_mode: { encoder: "libx265", size_budget: { budget_bytes: 1.5 * GB } } }) }) },
  { id: "file-validating", build: (v, s) => ({ job: single(v, s, { current_step: 3, worker: worker("validating_quality", { progress: 100 }) }) }) },
  { id: "file-worker-offline", build: (v, s) => ({ job: single(v, s, { status: "waiting_external", waiting_condition: "worker_unreachable", worker: worker("encoding", { progress: 33, progress_is_stale: true, worker_heartbeat_at: iso(7200) }) }) }) },
  // -- one file, waiting for you
  { id: "file-decision", build: (v, s) => ({ job: single(v, s, { status: "waiting_decision", waiting_reason: "Candidate is 31% smaller; the audio language could not be identified for 1 track. Choose what to do.", waiting_options: [{ decision: "approve", description: "Review the candidate" }, { decision: "cancel", description: "Keep the original" }] }) }) },
  { id: "file-decision-budget", build: (v, s) => ({ job: single(v, s, { status: "waiting_decision", reason_code: "search_budget_exhausted", waiting_reason: "search_budget_exhausted", waiting_options: [{ decision: "accept_loss" }, { decision: "retry" }, { decision: "abort" }] }) }) },
  // -- one file, done
  { id: "file-candidate", build: (v, s) => ({ job: single(v, s, { status: "completed", candidate_ready: true, current_step: 3, stages: encodeStages.map(x => ({ ...x, status: "completed" })), savings: { source_bytes: sizeVariants[s].source, candidate_bytes: sizeVariants[s].candidate, candidate_saved_bytes: sizeVariants[s].source - sizeVariants[s].candidate }, mode: "size", replacement_context: { service: "filesystem" }, tracks: { languages: ["spa", "eng"], kept: 2, dropped: 1 } }) }) },
  { id: "file-candidate-larger", build: (v) => ({ job: single(v, "larger", { status: "completed", candidate_ready: true, stages: encodeStages.map(x => ({ ...x, status: "completed" })), savings: { source_bytes: 1 * GB, candidate_bytes: 1.3 * GB, candidate_saved_bytes: -0.3 * GB } }) }) },
  { id: "file-not-converted", build: (v, s) => ({ job: single(v, s, { status: "completed", reason_code: "minimum_savings", error: "Minimum savings not met: estimated 3% < 10%", stages: encodeStages.map(x => ({ ...x, status: "completed" })) }) }) },
  { id: "file-replacing", build: (v, s) => ({ job: single(v, s, { status: "running", action_name: "promote_transcode_candidate", promotion: { original_path: path(v), new_path: path(v) }, stages: [{ name: "plan_promotion", status: "completed" }, { name: "approve_promotion", status: "completed" }, { name: "preserve_original", status: "running" }, { name: "import_candidate", status: "pending" }, { name: "finalize_promotion", status: "pending" }], current_step: 2 }) }) },
  // The coordinator's own reads while a replacement is prepared: the row shows how far it is.
  ...[["copy", "copying_recovery", 1.48 * GB], ["hash-wait", "waiting_for_hash_slot", 0], ["verify", "verifying_candidate", 0.2 * GB], ["verify-done", "verifying_recovery", 3.37 * GB - 1]].map(([name, phase, read]) => ({ id: `file-replacing-${name}`, build: (v, s) => ({ job: single(v, s, { status: "running", action_name: "promote_transcode_candidate", promotion: { original_path: path(v), new_path: path(v) }, stages: replaceStages.map((x, i) => ({ name: x, status: i < 2 ? "completed" : i === 2 ? "running" : "pending" })), current_step: 2, work: { phase, bytes_read: read, total_bytes: 3.37 * GB }, savings: { source_bytes: 3.37 * GB, candidate_bytes: 0.589 * GB } }) }) })),
  { id: "file-replaced", build: (v, s) => ({ job: single(v, s, { status: "completed", replaced: true, savings: { source_bytes: sizeVariants[s].source, candidate_bytes: sizeVariants[s].candidate, realized_saved_bytes: sizeVariants[s].source - sizeVariants[s].candidate }, stages: encodeStages.map(x => ({ ...x, status: "completed" })) }) }) },
  { id: "file-failed", build: (v, s) => ({ job: single(v, s, { status: "failed", error: "ffmpeg exited with status 1: Error while decoding stream #0:1: Invalid data found when processing input\n  at libavcodec/hevc_ps.c:1204", current_step: 2, stages: encodeStages.map((x, i) => ({ ...x, status: i < 2 ? "completed" : i === 2 ? "failed" : "pending", note: i === 2 ? "Worker reported exit 1" : undefined })) }) }) },
  { id: "file-failed-short", build: (v, s) => ({ job: single(v, s, { status: "failed", error: "Source file is missing", stages: encodeStages.map((x, i) => ({ ...x, status: i === 0 ? "failed" : "pending" })), current_step: 0 }) }) },
  { id: "file-cancelled", build: (v, s) => ({ job: single(v, s, { status: "cancelled", stages: encodeStages.map((x, i) => ({ ...x, status: i < 2 ? "completed" : "cancelled" })) }) }) },
  { id: "file-archived", build: (v, s) => ({ job: single(v, s, { status: "completed", archived: true }) }) },

  // -- batches
  ...batchScenarios(),

  // -- podcast
  { id: "podcast-classifying", build: (v) => ({ job: { id: id("pod"), number: 77, status: "waiting_decision", action_name: "podcast_clean", waiting_condition: "podcast_classification", source_path: `/media/Podcasts/${v.show}/episode-${"9".repeat(3)}.mp3`, podcast: { podcast_id: v.show, classified_blocks: 40, total_blocks: 120, removed_ms: 183000, known_ad_units: 2 }, stages: [{ name: "resolve_source", status: "completed" }], current_step: 0 } }) },
];

function batchScenarios() {
  const out = [];
  const add = (id, make) => out.push({ id, build: (v, s) => make(v, s) });
  add("batch-queued", (v, s) => batch(v, s, { status: "pending", batch: { total: 8, queued: 8 } }));
  add("batch-running", (v, s) => batch(v, s, { batch: { total: 8, completed: 1, running: 2, queued: 5 }, activities: [] , activity_file: path(v, 2) }));
  add("batch-running-decision", (v, s) => batch(v, s, { needs_decision: true, batch: { total: 8, completed: 3, running: 1, queued: 3, waiting_decision: 1 } }));
  add("batch-running-failures", (v, s) => batch(v, s, { batch: { total: 8, completed: 3, failed: 2, running: 1, queued: 2 }, batch_files: { file_count: 8, context: v.batchTitle, reasons: [{ reason: "Source file is missing", count: 2 }] } }));
  add("batch-preview", (v, s) => batch(v, s, { status: "completed", stages: batchStages(2, null), current_step: 1, batch: { total: 12, queued: 10, skip: 2, dry_run: true, outcome: "preview" } }));
  add("batch-nochanges", (v, s) => batch(v, s, { status: "completed", stages: batchStages(2, null), batch: { total: 3, skip: 3, outcome: "no_changes" } }));
  add("batch-failed", (v, s) => batch(v, s, { status: "failed", error: "Worker lost connection", batch: { total: 8, completed: 2, failed: 6, outcome: "failed" } }));
  add("batch-cancelled", (v, s) => batch(v, s, { status: "cancelled", batch: { total: 8, completed: 2, cancelled: 6, outcome: "cancelled" } }));

  const ready = (v, s, n, extra = {}) => itemsOf(v, s, n, () => ({}));
  add("batch-candidates", (v, s) => { const items = ready(v, s, 6); return batch(v, s, { status: "completed", stages: batchStages(2, null), remaining_candidates: 6, batch: { total: 6, completed: 6, outcome: "candidates_ready" }, savings: { source_bytes: 22.07 * GB, candidate_bytes: 3.67 * GB, candidate_saved_bytes: 18 * GB } }, items); });
  add("batch-plan-ready", (v, s) => { const items = ready(v, s, 6); return batch(v, s, { status: "waiting_decision", waiting_options: [{ decision: "approve" }, { decision: "reject" }], batch: { total: 6, completed: 6, promotion_plan_ready: true, outcome: "candidates_ready" } }, items); });
  add("batch-mixed-candidates", (v, s) => { const items = [...ready(v, s, 3), ...itemsOf(v, s, 2, () => ({ status: "failed", candidate_ready: false, error: "Encode failed", saved_bytes: 0, can_reconfigure: true })), ...itemsOf(v, s, 2, () => ({ status: "skip", candidate_ready: false, saved_bytes: 0, reason_code: "already_efficient" }))].map((it, i) => ({ ...it, item_key: `item-${i + 1}`, display_label: v.file(i + 1) })); return batch(v, s, { status: "completed", stages: batchStages(2, null), remaining_candidates: 3, batch: { total: 7, completed: 3, failed: 2, skip: 2, outcome: "partial" } }, items); });

  // Replacing: the state that looked worst on a phone.
  const replacing = (phases, over = {}) => (v, s) => {
    const items = itemsOf(v, s, phases.length, i => ({ replacement_plan: "queued", replacement_action_id: id("repl"), replacement_status: "running", previous_attempts: i < 2 ? [{ child_action_id: id("old"), status: "failed" }] : undefined, where: i === 0 ? { new_path: path(v, 1), backup_path: path(v, 1) + ".navigatorr-original" } : undefined }));
    const members = items.map((it, i) => member(it, phases[i]));
    const done = members.filter(m => m.phase === "done").length, failed = members.filter(m => m.phase === "failed").length;
    return batch(v, s, {
      current_step: 2, stages: batchStages(2),
      replacement_requested: true,
      replacement: { approved: true, total: phases.length, done, failed, retryable: failed, frees_bytes: 10.05 * GB, freed_bytes: done * 1.4 * GB, members, ...over },
      batch: { total: phases.length, completed: phases.length, outcome: "candidates_ready", promotion: { eligible: phases.length, promoted: done } },
    }, items);
  };
  add("batch-replacing-start", replacing([{ phase: "checking" }, { phase: "backup", percent: 2 }, { phase: "waiting" }, { phase: "waiting" }, { phase: "waiting" }, { phase: "waiting" }]));
  add("batch-replacing-mid", replacing([{ phase: "done", freed_bytes: 1.45 * GB }, { phase: "done", freed_bytes: 2.27 * GB }, { phase: "backup", percent: 64 }, { phase: "swap" }, { phase: "waiting" }, { phase: "waiting" }]));
  add("batch-replacing-failure", replacing([{ phase: "done", freed_bytes: 1.4 * GB }, { phase: "failed", failure: { reason: "Not enough free space on the library volume", detail: "write /media/TV/x: no space left on device (errno 28)", original_state: "in_place" } }, { phase: "backup", percent: 30 }, { phase: "waiting" }]));
  add("batch-replacing-done", (v, s) => { const make = replacing(Array.from({ length: 6 }, () => ({ phase: "done", freed_bytes: 1.7 * GB }))); const r = make(v, s); r.job.status = "completed"; r.job.replaced = true; r.job.replaced_files = 6; r.job.savings = { source_bytes: 22 * GB, realized_saved_bytes: 10.05 * GB }; return r; });
  add("batch-replacing-stopped", (v, s) => { const make = replacing([{ phase: "done", freed_bytes: 1.4 * GB }, { phase: "failed", failure: { reason: "Original changed since it was checked", original_state: "recovery" } }, { phase: "failed", failure: { reason: "Original changed since it was checked", original_state: "recovery" } }, { phase: "waiting" }]); const r = make(v, s); r.job.status = "failed"; r.job.error = "2 replacements stopped"; return r; });
  add("batch-partially-replaced", (v, s) => { const items = [...itemsOf(v, s, 3, () => ({ replaced: true, candidate_ready: false })), ...itemsOf(v, s, 4, () => ({}))].map((it, i) => ({ ...it, item_key: `item-${i + 1}`, display_label: v.file(i + 1) })); return batch(v, s, { status: "completed", replaced_files: 3, remaining_candidates: 4, savings: { realized_saved_bytes: 4 * GB, source_bytes: 30 * GB }, batch: { total: 7, completed: 7, outcome: "promoted", promotion: { eligible: 7, promoted: 3 } } }, items); });

  // A long batch: two pages, to check paging controls and a tall list.
  add("batch-large", (v, s) => { const items = itemsOf(v, s, 148, i => (i % 9 === 4 ? { status: "failed", candidate_ready: false, error: "Encode failed", saved_bytes: 0 } : i % 7 === 3 ? { status: "skip", candidate_ready: false, saved_bytes: 0, reason_code: "already_efficient" } : {})).map((it, i) => ({ ...it, item_key: `item-${i + 1}`, display_label: v.file(1).replace(/E0\d/, `E${String(i + 1).padStart(3, "0")}`) })); return batch(v, s, { status: "completed", stages: batchStages(2, null), remaining_candidates: 120, batch: { total: 148, completed: 120, failed: 16, skip: 12, outcome: "partial" } }, items); });
  // One file only, still a batch.
  add("batch-single", (v, s) => { const items = ready(v, s, 1); return batch(v, s, { status: "completed", stages: batchStages(2, null), remaining_candidates: 1, batch: { total: 1, completed: 1, outcome: "candidates_ready" }, batch_files: { file_count: 1, files: [{ file_path: items[0].file_path, display_label: items[0].display_label }] } }, items); });
  return out;
}

// What a check needs to put a scenario on the server: the job, its batch items,
// and the replacement it links to so "View replacement" has somewhere to go.
export function world(scenario, nameVariant = "short", sizeVariant = "normal") {
  const v = nameVariants[nameVariant], s = sizeVariants[sizeVariant] ? sizeVariant : "normal";
  const built = scenario.build(v, s);
  const job = built.job;
  job.workflow_id ||= job.id;
  return { jobs: [job], items: built.items ? { [job.id]: built.items } : {}, id: job.id };
}
