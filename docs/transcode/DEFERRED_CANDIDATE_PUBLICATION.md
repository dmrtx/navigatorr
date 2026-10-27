# Deferred candidate publication

Status: design, revision 4 (after two Opus reviews against the code). Step 1
(the worker, see "Worker implementation notes") is implemented behind the
`deferred_publication` worker setting, off by default. Step 2 (the server side,
see "Server implementation notes") is implemented behind the per-worker
`publish_policy` setting, off by default; since revision 4 it covers the
children of a batch too (see "Batches"). The UI and storage view (step 4) and
making `on_approval` the default (step 5) are not. Each review found blockers the
previous revision hid.

A finished encode stays on the worker's local disk until the owner approves its
replacement, and is copied to the media share only then. Today every validated
candidate is copied to the share at once, whether or not it is ever used.

## Why

The worker encodes on local disk, validates there, and then publishes the
finished file to `<source dir>/.navigatorr-candidates/` on the share. The
candidate then sits beside its original until the owner approves "Replace file"
or the batch settles, which can be days or weeks.

Observed on one production share, in round numbers: about 80 candidate files,
about 100 GB, spread over about 40 folders, while dozens of jobs ran and over a
hundred more were queued. Free space fell by about 90 GB while the owner was
approving candidates, because each encode adds a file to the share and each
approval frees one only later. About half of the candidates belonged to attempts
that could never be promoted any more (see "Candidates already on the share").

Copying a candidate that is rejected, superseded or never approved is wasted
network and storage work, and it takes space from the share the owner is trying
to free.

## Goals

- A candidate takes no space on the share until its replacement is approved.
- Approval, rejection and cancellation stay as safe as they are today: the
  original is never touched before the promotion's own checks pass on the bytes
  that were published.
- Workers and the server can be upgraded, and rolled back, independently. The
  current behaviour stays available and is the default.

## Non-goals

- Changing the encode, the quality ladder or the size policy.
- Changing the promotion's recovery and integrity checks
  (`TRANSCODE_CANDIDATE_PROMOTION.md`).
- Keeping candidates across a loss of the worker's local disk. A lost candidate is
  encoded again.

## Current flow, as the code does it

1. `submit_transcode` sends the job. The worker stages the source locally, encodes,
   and validates the local candidate. A candidate the size policy or quality
   limits refuse is removed and never published.
2. In `executeOperational` (`internal/transcodeworker/operational_execute.go`
   around lines 357-371) the worker checkpoints `EncodeComplete`,
   `LocalCandidatePath`, `CandidateSizeBytes` and `CandidateSHA256`. It then runs
   `finalizeOperational`, which publishes the file and calls
   `completeOperationalJob`, which removes the local artifacts and the source
   cache pins.
3. What publishes is decided by `FinalizationState` and `EncodeComplete`, not by
   `Phase`. The four finalization states are in `operational.go:28-31` and are
   checked by `ValidateFinalizationState`. `automaticFinalizationAllowed`
   (`scheduler.go:43`) lets a pending encoded job publish by itself.
4. Publication is a hash-verified exclusive copy for direct-SMB jobs that carry a
   size and SHA (`PublishVerified`). Other paths exist: a plain `Publish`, a guarded
   one for podcasts, and for filesystem destinations a partial plus rename with a
   non-atomic copy fallback and a size-only check afterwards.
5. The coordinator's `wait_transcode` completes only on `StatusCompleted`.
   `validate_result` does stat-level checks (none for `smb_direct`,
   `verifyPublishedCandidateLight`). `accept_result` runs once, when the encode
   ends, and checks the original is unchanged.
6. The promotion plans from the share. `plan_promotion` hashes the candidate
   (`action/promote_files.go` ~354), inspects its streams (~371) and hashes it
   again (~382). `plan_digest` itself reads no file
   (`action/promote_plan_digest.go`), and `approve_promotion` does not read the
   candidate either; only the plan step does.
7. `retry-publication` does not publish; it records `OperatorRetryRequested` and the
   periodic `ResumePostEncode` sweep publishes later, needing a slot.

## What the code already allows

- `fsop.ResolveWrite` accepts a path that does not exist yet
  (`fsop/fsop.go:75-85`), so a plan can name the planned candidate path.
- Candidate paths are unique per job (`transcode_submit.go:73`), so a late
  publication cannot collide with another attempt.
- A retained job with state `running` and PID 0 occupies no worker slot
  (`slots.go:353`).

## What the code does not allow

- **The engine has no promotion template versions.** Templates are found by name
  (`action/engine.go:105-117`); an instance stores only the action name and the
  current step (`store/actions.go:27`); `Version: 2` appears only in the catalog.
  A second template means a new name and changes to 29 literal references in 18
  files. So no step is added to `promote_transcode_candidate`.
- **Publication cannot live in `approve_promotion`.** `engine.go:478` and `:526`
  treat every promotion at or before `approve_promotion` as "not approved", which
  exposes a running upload to batch revisions and to the retry guard.
- **`Engine.cancel` refuses a completed action** (`engine.go:735-737`), and a
  retained `transcode_media` is completed. Cancelling a batch cancels its children
  one by one (`engine.go:776-800`) and fails on the finished ones. Release needs its
  own code.
- **`candidateWorkerStopped` returns at once when `transcode_done` is true**
  (`candidate_cleanup.go:249-266`), so cleanup never asks the worker. For a retained
  candidate every cleanup path (retire, `retireReplacedSiblings`,
  `DiscardLeftoverCandidate`) would record "removed" with 0 bytes and leave the file
  on the worker until it expires.
- Nothing polls completed actions, so nothing marks an action when the worker drops
  a candidate.

## Proposed flow

New per-job field `publish_policy`: `on_encode` (today) or `on_approval`. The
coordinator sends it only for `on_approval`, and only to a worker that advertises
`deferred_publication`; for `on_encode` the field is omitted, because the worker
rejects unknown fields (`http.go:500`).

With `on_approval`:

1. The worker encodes and validates as today and writes the same checkpoint.
2. It then **retains** the job. ("Retained", because "hold" already means a queue
   frozen by drain: `hold.go`, `/v1/hold`, `worker-hold.json`.) The staged source
   copy and the source cache pins are released; the local candidate and the
   checkpoint stay.
3. The coordinator records the encode as finished with `candidate_location:
   "worker"`, the attested size and SHA-256, and the planned share path.
4. `plan_promotion` plans from the attested size and SHA and the validation evidence
   from encode time. It still checks the original and the series/episode
   associations. The owner approves that plan.
5. The upload happens at the start of the existing step `preserve_original`, at the
   same step index, so promotions in flight are unaffected:
   - the step creates the candidates directory if needed (the recovery copy lives
     beside the candidate, `promote_files.go:49-51`, so the directory must exist
     before the original is preserved);
   - it asks the worker to publish and records `Published` in the promotion state;
   - it verifies the published file: size and SHA-256 equal to the attested values,
     the same full stream inspection the plan step does today, and a fresh
     `promotionPath` check on the now existing path so a symlink created after
     planning is caught. It records `PublishVerified`;
   - it records `candidate_location: "share"` on the `transcode_media` state (under
     that action's lease), so later cleanup treats the file as a share file;
   - only then does it preserve or move aside the original, import and remove the
     old file. The order is a safety requirement.
   While uploading the promotion sets `work_progress.phase = "uploading"`;
   `replacementPhase` already reads that before the step
   (`maintenanceui/replacement_progress.go:81-93`).
6. On rejection, supersession, failed validation or expiry the coordinator releases
   the retained job. Nothing was written to the share.

## Worker changes

- **Two predicates, not one.** `IsPostEncodeFinalizationPending` today answers both
  "never mark this `runner_killed`" and "must be published". It feeds
  `isPostEncodeResume` (`operational_execute.go:1234`), `ResumePostEncode`,
  `RetryPublication` (`publication_retry.go:46`) and `RecoveryRequired`
  (`telemetry.go:149`). Adding the retained state to it makes the sweep publish it;
  leaving it out makes a restart mark it failed (`reconcileJob` `worker.go:1840`,
  strong idempotent submit `:889`, weak `:1007`) and orphan the file. Split it into
  "encode finished" (includes retained) and "pending publication" (excludes it).
  `reconcileStoppedRunner` does not act on PID 0 (`telemetry.go:34`).
- **Where retained jobs live, for rollback.** An older worker that finds an unknown
  finalization state in a `running` job fails `ValidateFinalizationState`
  (`worker.go:1810`); `ReconcileStartup` stops at the first error (`:1764`) and the
  worker refuses to start its scheduler (`cmd/navigatorr-transcode/main.go:279,
  306`). A flag instead of a state is worse: an older worker would see an encoded
  `pending` job and publish it (`scheduler.go:43-52`). So retained job directories
  are kept under a name starting with a dot; reconcile, the slot count and the
  activity scan ignore such entries (`worker.go:1753`, `slots.go:347`). An older
  worker answers not-found and the coordinator re-encodes. Alternatively, release all
  retained candidates before any worker rollback.
- Capability `deferred_publication` in both capability structures
  (`transcode/capabilities.go`) and in the fingerprint. Precedents that gate on a
  capability: `SizeAudioOnly`, `CoverArt`.
- Publish route `POST /v1/jobs/{id}/publish`, valid only for a retained job. It
  moves the job from retained to finalizing under the job lock and persists that
  before the runner starts, so a restart cannot return it to retained. It is
  idempotent and verifies the local file against the checkpoint before touching the
  share. An older worker answers 404 with the default code, which the coordinator
  already reads as an unknown job (`candidate_cleanup.go:290-297`); the coordinator
  must check the capability before calling and never read that 404 as "candidate
  lost". The route is refused while the drain freeze is on (`hold.go:11-19` says
  post-encode work still publishes, so this is an explicit new rule), so an approval
  does not start an upload during a deploy.
- Release: reuse `Cancel`, which already stops the runner and removes
  `LocalCandidatePath` (`worker.go:2180-2184`), but only for jobs with
  `publish_policy=on_approval` make it refuse while a publish is running or a rename
  may have happened, as `RetryPublication` does with `jobAlive`. Cancelling an
  `on_encode` job must not change. `Cancel` on a completed job does nothing
  (`worker.go:2057-2065`), which is why the file must be known to be on the share
  once published (step 5).
- Disk: retained candidates count against `retained_candidates_max_bytes` and a
  minimum free-disk margin. Past either limit the worker answers new submits with the
  existing "busy" code. The worker has no free-disk check at admission and no
  collector for stale job directories (only `legacy_cleanup.go`); both are new.
- Expiry: a retained job expires after `retained_ttl`. The worker keeps an explicit
  `expired` state for the job, because `wait_transcode` turns a 404 into
  `StepFailed` "disappeared" (`transcode_submit.go:648`) and a missing job cannot be
  told from an expired one. The coordinator maps `expired` to "encode again".
- Specification digest: `DigestTranscodeExecutionSpecWithSourceSHA`
  (`worker.go:782`) includes `publish_policy` only when it is `on_approval`. Including
  it always would change the digest of running `on_encode` jobs and fail their
  reconciliation as a mismatch.
- Podcast jobs and jobs whose finalization is `NotRequired` are excluded.

## Coordinator changes

Everything below assumed the candidate was on the share. Each place must read
`candidate_location`.

- `wait_transcode`: treat a retained job as encode-finished and free the worker slot
  (`noteWorkerSlotFreed` reacts only to terminal states, `transcode_submit.go:659`).
- `validate_result`: use the attested size and skip the stat for a retained
  candidate. The defensive skips (`transcode_validate.go:146-181`) say "whoever let
  it through", so "refused candidates never reach retained" is an assumption.
- One hook, not a list. Add a single release when a `transcode_media` with a
  retained candidate becomes not promotable, and call it from every path that ends
  that outcome: failed `validate_result` (`:128`, `:160`), the reject after the size
  wait (`:199`), the defensive skips, a failed accept, a rejected plan, batch
  revision or cancellation, sibling replacement, and leftover discard. A batch
  cancellation needs explicit code for its already-completed children because
  `Engine.cancel` refuses them.
- Cleanup: `removeOwnCandidate` branches on `candidate_location`. For `worker` it
  asks the worker to release and records the outcome; changing
  `candidateWorkerStopped` does not help since it returns early. A worker that cannot
  be reached leaves the candidate recorded as kept.
- Leftover listing: `leftoverOf` measures bytes with `os.Lstat`
  (`candidate_cleanup.go:506`); a retained candidate needs its size from the
  attestation.
- Drain: readiness is computed from database rows (`classifyDrainRow` returns ""
  for a done row, `worker_drain.go:254`). Retained candidates must not block a
  deploy; an upload inside `preserve_original` should, which needs a new row class.
- Batches: decided, see "Batches". A batch child defers like any attempt; its
  promotion needs no new plan step, and a worker with no room left to retain
  makes the child publish on encode instead of waiting.
- Rollback of the server: an older server cannot plan a retained candidate ("not
  accessible"). That fails closed but strands candidates until expiry, so release
  first or accept re-encoding.

## Maintenance UI

- A retained candidate shows "Ready on <worker>, not on the share yet" and the size
  it will take. `fileConverted` is derived from `CandidatePath`
  (`file_status.go`) and must not read a planned path as an existing file.
- The "uploading" phase is set through `work_progress` (above).
- The storage view knows only share volumes (`storage.go`). It gains retained bytes
  per worker next to the share's free space.

## Failure modes

| Event | Result |
|---|---|
| Worker loses its local disk, or the job directory is deleted | The candidate is gone. The coordinator marks the file as needing a new encode; no original is touched. |
| Worker unreachable at approval | The promotion waits in `preserve_original` before touching the original; nothing changes. |
| Worker restarts while the job is retained | The job stays retained; startup and the sweep do not publish it. |
| Server restarts during an upload | `preserve_original` resumes; the publish is idempotent and reports completion. |
| Share full or unreachable at publication | The local candidate is kept and publication is retried within the attempt limit; the original is untouched. |
| Original changed while retained | Refused at the promotion's checks as today; the job is released through the single hook. |
| Two attempts retained for one file | Promoting one releases the others, retained-aware. |
| Promotion fails or is rejected after the upload | `candidate_location` is `share`, so ordinary share cleanup removes the published file. |
| Local disk full | The worker stops admitting jobs; a running encode that cannot write fails as any out-of-space encode does. |
| Release or expiry while an upload runs | Refused while the runner is alive or a rename may have happened. |
| Worker rolled back with retained jobs | Hidden directories are ignored; the coordinator re-encodes. |

## Compatibility and rollout

0. **Independent of this design:** list stale leftovers (next section) and clean
   them. It gives the benefit now.
1. Worker (implemented): the two predicates, hidden retained directories,
   `expired` state, capability, publish and release routes, disk limits. The field
   is optional and never sent, so this deploys alone. Deploy order for the
   capability: the server first, then the worker (a server that has not seen the
   capability must not receive a fingerprint it cannot parse), then enable
   `deferred_publication` on one worker.
2. Server: `candidate_location` handling in cleanup, listing, validation and wait;
   the single release hook and the batch cancellation code; the upload inside
   `preserve_original` with the `candidate_location` write; the batch admission
   rule. All release paths are in this step, not a later one, or the first trial
   leaks candidates. It changes no template and stays inert while `publish_policy`
   is `on_encode`.
3. Enable `on_approval` for one worker and one test batch; compare share growth and
   approval time.
4. UI and storage view.
5. Make `on_approval` the default only after measured use. `on_encode` stays for
   workers with small disks.

Once `on_approval` is on, moving a job to a different or older worker is unsafe.

## Worker implementation notes

What the worker PR does where this document left a choice, or differs from it.

- **Settings** (worker config, all optional, `deferred_publication` defaults to
  off and the others to "no limit" except the TTL): `deferred_publication`,
  `retained_ttl_hours` (default 720, so 30 days), `retained_candidates_max_bytes`,
  `min_free_disk_bytes`. The capability is advertised only when
  `deferred_publication` is on, and a worker that does not advertise it also
  refuses `publish_policy: on_approval`.
- **State.** `FinalizationState` gains `retained` and `expired`, which only ever
  appear in a hidden directory. The record carries `publish_policy`,
  `retain_reserve_bytes` (the admission reservation while encoding), `retained_at`
  and `expired_at`. An encoded job that is not yet hidden is `pending` with
  `publish_policy: on_approval`; nothing publishes that state and nothing marks it
  `runner_killed`, so either crash window (before or after the rename) is finished
  by the retained sweep.
- **Predicates.** `IsPostEncodeFinished` ("never `runner_killed`") includes
  retained and deferred-pending jobs; `IsPostEncodeFinalizationPending` ("must be
  published") excludes them.
- **Publish.** `POST /v1/jobs/{id}/publish` verifies the local file against the
  checkpoint (size and SHA-256), saves `finalizing` while the directory is still
  hidden, moves it back and returns. The existing post-encode sweep starts the
  runner, so a publish **uses a video slot** and waits when every slot is busy
  (the open question below stays open: a lane of its own was not built). The
  retry budget is reset, so the usual automatic retries and `retry-publication`
  apply afterwards. Answers: `not_retained` (409), `candidate_changed` (409),
  `worker_held` (503, retryable) while the drain freeze is on.
- **Release.** `Cancel` on a retained job removes the candidate and returns the job
  to its regular directory as an ordinary cancelled job. For an `on_approval` job
  it answers `release_refused` (409) from the moment a publish is requested until
  the job completes, because a rename may have happened. A stuck publish is
  therefore retried (`retry-publication`), not released.
- **Expiry.** The retained sweep (piggybacking on the existing `post_encode`
  scheduler sweep, at most once a minute, and once at startup) removes the
  candidate after `retained_ttl_hours` and leaves an `expired` tombstone, which
  `GET /v1/jobs/{id}` answers with `status: "expired"`. The tombstone is removed
  after 30 days. A job whose publish was requested is never expired.
- **Admission.** An `on_approval` submit is answered with the existing `worker_busy`
  (429) when retained bytes plus the new job's source size would pass
  `retained_candidates_max_bytes`, or when free space on the local work disk would
  fall below `min_free_disk_bytes`. Encoding jobs are counted by their source size
  until their candidate exists, so a burst of submits cannot overshoot. A failed
  measurement refuses the job. `on_encode` submits are never subject to either.
  The free-disk floor also subtracts the reservations of encodes that have not
  written their candidate yet; it budgets the source size only, not a staged copy
  on top of it.
- **Stale directories.** The sweep also removes a job directory older than an hour
  that holds nothing but its lock file. That is the whole collector: it never
  touches `_work`, scratch directories (names starting with `_`, or without a
  lock file) or candidates.
- **Encoding again after expiry.** The coordinator reuses the job id, so a new
  encode can finish while the old tombstone is still on disk. Retaining it
  replaces the tombstone. `Cancel` on a tombstone leaves it expired.
- **Interrupted release.** A release saves `cancelled`, moves the directory back
  and cleans up. If it stops in between, the sweep or a repeated `Cancel` finishes
  the move and the cleanup.
- **Verification errors.** A read error or a cancelled request while the candidate
  is hashed for a publish is answered 500 (retryable), not `candidate_changed`,
  which is final. The hash runs inside the request while the job lock is held.
- **Rollback limit.** The "older worker ignores it" guarantee covers hidden
  directories. Between the encode checkpoint and the directory move a deferred job
  is `pending` with a checkpoint under its regular name, which an older worker
  would publish; the window is milliseconds, or longer only if the worker crashed
  there and nobody has run the sweep. Drain the worker (no job running, sweep
  once) before rolling back.
- **Duplicate scan race.** Moves between the hidden and regular directory take the
  job lock but not the capacity lock, so a submit that scans both at the instant
  of a publish or release can miss the job. The coordinator submits a job id once
  and only after reading its status, so this is not reachable in its flow.
- **Logs.** `GET /v1/jobs/{id}/logs` still reads the regular directory, so it
  answers not found for a retained job.

## Server implementation notes

Step 2 changes no template and is inert while a worker's `publish_policy` is
`on_encode` (the default): no `publish_policy` is sent, no `candidate_location` is
written, and nothing is asked of the worker's retained-candidate routes.

**Deciding to defer.** `publishPolicyFor` decides once per attempt and stores the
decision (`publish_policy` in the attempt's state), so a resubmit after an
uncertain submit sends what the first submit sent. An attempt defers only when all
of these hold: the assigned worker is configured `publish_policy: on_approval`; its
executor can publish and release retained candidates; it advertises
`deferred_publication`; the worker has not refused the attempt for lack of room to
retain (`publish_policy_fallback: retention_full`, batch children only). When a
worker is configured for deferral but cannot do it, the attempt publishes on encode and
records why in `publish_policy_fallback`; a capability lookup that fails holds the
submit rather than silently publishing on encode.

**Batch children defer too** (revision 4; revisions 1 to 3 refused them at
admission). `publishPolicyFor` no longer looks at `parent_action_id`. "Batches"
below gives the reasoning and the one rule that keeps a batch from stalling.

**Where the candidate is.** `candidate_location` is `share` (or absent) or
`worker`. `wait_transcode` completes at retention (`transcode_done` true, the slot
freed) and copies the worker's attested size and SHA-256 into the attempt. A
retained job that attests neither fails closed. `validate_result` trusts the
attested size and does not stat the share. The detailed ffprobe validation moves to
after the upload (below), because it needs the file.

**The release hook.** `retireCandidate` is the single hook; `removeOwnCandidate`
branches on location and asks the worker to release. It is called from: the reject
after the size wait, an attempt that ends skipped by a size or saving guard, a
rejected, cancelled or failed promotion (`releaseRetainedForPromotion`), batch
revision, batch cancellation (finished children go through `releaseRetainedChild`),
sibling replacement and leftover discard. A worker that cannot be reached, refuses
(a publication is running) or cannot release leaves the candidate recorded as
kept and listed. A worker that already published it makes the candidate a share
file again, which is removed.

**A failure alone releases nothing.** An attempt that fails (a share that dropped
out during `accept_result`, a validation that can be repeated) keeps its retained
candidate, as a failed `on_encode` attempt keeps its file: Retry resumes it on that
candidate. It is listed as a leftover from the worker's attestation, Discard
releases it, and the worker drops it at the end of its retention otherwise.
Releasing on failure made Retry unable to recover hours of encoding.

**Releases that do not go through.** The release of a promotion's candidate waits
up to ten seconds for the attempt's lease. A promotion that ended for good
(cancelled, or failed with the release flag: rejected, or the candidate is gone)
while its worker could not be reached or was copying leaves the candidate on the
worker: the attempt lists it with the reason, and Discard is allowed although the
promotion still references it. Discard releases the candidate and removes anything
a failed or finished copy left on the share, so a copy that landed after a
cancellation does not stay as an unlisted file.

**Promotion.** The plan asks the worker whether the candidate can still be
uploaded and carries its attestation instead of hashing a file. `preserve_original`
first asks the worker to publish (`POST /v1/jobs/{id}/publish`), with the original
untouched; while the copy is going the step returns a wait and the scheduler asks
again (the request is idempotent), so neither an executor nor the promotion's lease
is held for as long as the copy sits behind encodes. The wait keeps
`work_progress.phase = "uploading"`, so a deploy keeps seeing the upload. When the
copy is done it writes `candidate_location: share` on the source attempt (a source
that stays busy is another wait), hashes the file that arrived against the
attestation, runs the detailed validation, and only then continues as for a
candidate that was always on the share. A run that finds the location already
`share` does not ask the worker again. A worker that is draining or unreachable
makes the promotion wait; a candidate the worker no longer holds (expired,
released, changed, unknown) is a final failure that releases what is left. A job
the worker reports as failed after a copy was requested is not a verdict: it is a
transient failure, the candidate stays, and the owner retries or discards it.

**Expiry is lazy.** A retained attempt is a completed action, so nothing polls it.
The worker's `expired` answer is found when `wait_transcode` or the reconcile path
reads the job, or when a promotion plans or uploads; the attempt then encodes the
same job again (bounded at two re-encodes per attempt, then it fails and says to
check the worker's retention).

**Drain.** Readiness reads database rows. A retained candidate is a done
`transcode_media`, which classifies as idle, so it never blocks a deploy. An upload
sets the promotion's `work_progress.phase` to `uploading` and records
`upload_worker`; `classifyDrainRow` treats that as a running job on that worker, so
a deploy waits for it. The promotion deliberately does not record the source's
`job_id`, so cancelling a promotion never cancels (releases) the source's job.

## Batches

A batch (`transcode_batch`) fans out `transcode_media` children, and when it was
started with `promote_candidates` (or is reviewed afterwards) it asks for one
approval of a replacement plan and then promotes the members. Revisions 1 to 3
kept batch children out of deferral because they assumed that plan hashed the
candidates on the share, and that a pool of retained candidates would starve the
batch's admission. The first assumption was wrong and the second needs one rule.

**Promotion needs no new step.** The batch plan (`buildBatchPromotionPlan`,
`BatchCandidates`) is built from the evidence the encode left on each child:
`candidate_path` (the planned path) and the worker's attested `candidate_sha256`.
It reads no file on the share. The approval is one decision on the plan's digest.
Each member is then promoted by its own `promote_transcode_candidate` child, at
most `promotion_parallelism` (1 or 2) at a time, and that is the promotion this
document already describes: its plan asks the worker whether the candidate can
still be uploaded (`planRetainedCandidate`), and `preserve_original` uploads it
before the original is touched, holds the arrived file to the attestation
(`verifyBatchPromotionApproval` compares the member's SHA with the promotion's),
and records `candidate_location: share`. So a batch approval uploads at most two
candidates at a time, which also bounds the worker's upload bandwidth, and a
member that waits for its upload (the worker is busy, draining or restarting)
leaves the batch waiting on `batch_promotion` with the original untouched.

**A retained job holds no slot.** The child's `wait_transcode` completes at
retention and its batch item turns `completed`; `batchAvailableSlots` counts only
`running` items, and the worker counts a retained job as no running job. The next
child is admitted as soon as the encode ends. Slots are therefore never the
limit; the worker's disk is.

**Admission rule: a batch spills, it does not wait.** The worker's limits
(`retained_candidates_max_bytes`, `min_free_disk_bytes`) answer a deferred submit
that does not fit with `worker_busy` (nothing created), counting the candidates
it holds and the encodes that have not written theirs. For a standalone attempt
the answer is a wait: the owner approves or discards the candidates that fill the
worker one by one, and room appears. A batch cannot wait. Nothing in it is
approved before every child has encoded, so the candidates of its first children
would stay on the worker, the others would wait for room that cannot appear, and
when the first candidates reached `retained_ttl_hours` they would expire and be
encoded again, in a loop. The alternatives considered:

- *Wait for room* (the standalone rule): deadlocks, as above.
- *A cap per batch or per worker on the coordinator*: it stops the batch earlier
  but the remaining children still wait for approvals that cannot come.
- *Approve a batch in parts while it encodes*: a different feature (the plan, the
  digest and the review are made for a finished batch) and not needed to be safe.
- *Refuse deferral for batches* (revisions 1 to 3): safe, and a no-op for the
  workload that matters, since nearly every file in production is a batch child.

So the rule is: **a batch child the worker has no room to retain publishes on
encode.** `spillRetentionRefusal` (`action/candidate_location.go`) recognises the
refusal by its message (`transcode.IsRetentionRefusal`: `worker busy` plus the
budget or the free-disk marker the worker writes), clears the attempt's
`publish_policy`, records `publish_policy_fallback: retention_full` and sends the
same job again without a policy. That is safe because a busy answer created
nothing, the job id and idempotency key are unchanged, and the digest of a job
without a policy is the one it always had. Later submits of the attempt (a
reconcile that finds no job) keep publishing on encode, and a manual retry decides
again. A refusal for slots (`max parallel jobs`, from an older worker) is not
touched. The cost is bounded and visible: a batch retains what the worker's
budget allows, and the rest publishes up front, exactly as every batch did before.
A worker with no limits configured never refuses, so it retains the whole batch
and its disk is the only bound; set `retained_candidates_max_bytes` before using
deferral for large batches. A free-disk refusal also spills, so a burst of
`on_encode` jobs can use the disk the floor was protecting; keep the floor below
what one encode needs and the budget below what the disk can hold.

The worker's message is the contract, not a new code: the strings are constants in
`transcode/worker_errors.go`, the worker builds its messages from them, and a test
on the worker side fails if either stops matching. Workers and servers of any
age keep reading the answer as the busy it always was, and the server needs no
new capability.

While a batch holds the budget, standalone files on that worker wait for room,
since their candidates are not the batch's to release. This is the same
trade-off as before the change (a batch occupying the worker's slots) and is a
reason to give a very large batch its own worker.

**Every end of a retained child goes through the one hook.**

| Event | Path |
|---|---|
| Batch cancelled (`Engine.Cancel`) | `releaseRetainedChild` for each finished child |
| Batch cancelled by its schedule step's `cancel` decision | the same, added in revision 4: the step used to leave `completed` children alone |
| Settings revision of a file | `retireSupersededCandidates` |
| Sibling replaced | `retireReplacedSiblings` |
| Member promotion rejected, cancelled or failed for good | `releaseRetainedForPromotion` |
| Leftover discarded by the owner | `DiscardLeftoverCandidate` |
| A child skipped by a size or saving guard | `retireCandidate` from the child itself |

The batch plan being rejected is deliberately not on the list: as for candidates
on the share, a rejected plan keeps the candidates and a later review can replace
them (`BatchCandidates` offers every finished member that has no promotion yet).

**Expiry in a batch.** A batch may wait for approval for weeks, as the
retention (default 30 days) may end first. The expiry is found when a member's
promotion plans or uploads, as for a standalone file: that member's promotion
fails for good ("encode the file again"), releases what is left, and the batch
reports `N batch promotion(s) need attention` naming the file, while the other
members promote normally and the original of the expired one is untouched. The
member is then re-encoded with the batch's file revision (the same action that
changes a file's settings), which supersedes the finished attempt. Choose
`retained_ttl_hours` longer than the longest wait expected for a batch's approval.

**Worker restarts.** A retained job lives in a hidden directory and survives a
restart. An upload that was under way answers uncertain; the member's promotion
waits and asks again (the request is idempotent), so the same batch approval
finishes the upload once the worker is back. A restart does not hand a retained
child to the sweep that publishes, so nothing reaches the share without a
promotion asking for it.

**Rollout for batches.** Nothing changes until a worker is configured
`publish_policy: on_approval` and advertises `deferred_publication`, so this
deploys inert. Roll the server out first, then the worker (its messages are
unchanged, the new constants only name them). Enabling a worker for batches is a
separate step: start with one worker, set `retained_candidates_max_bytes`, run one
batch larger than that budget and check that the overflow publishes on encode
(`publish_policy_fallback: retention_full` on those children) and that one
approval promotes both the retained and the published members.

## Candidates already on the share

A finished attempt whose original was replaced by another attempt cannot be
promoted, yet its file stays on the share. `replacedOriginalReason` lists attempts
whose sibling cleanup was recorded as "kept", but attempts finished before sibling
retirement was deployed are not listed.

The criterion to list one must be at least as strict as sibling retirement, which
requires the same `original_sha256` (`candidate_cleanup.go:416`): list a completed
attempt whose recorded original no longer matches the file now at its path, and
only when the library directory is readable and holds other files, so an unmounted
share is not read as "gone". Then `discard_candidate` removes it with its usual
checks.

## Open questions

- How much free disk do the worker Macs have for retained candidates? Not measured.
  The job directory also holds the staged source, released at retention.
- `retained_ttl`: batches have waited weeks. Resolved: expiry is lazy (see "Server
  implementation notes"); nothing polls completed actions.
- The batch admission rule: resolved, see "Batches": batch children defer, and a child the worker has no room to retain publishes on encode.
- Should publishing use an encode slot or a lane of its own? With every slot
  encoding, an approval can wait hours.
- Support for workers without direct SMB (a local mount): the filesystem path
  publishes by copy and should work but is untested.
- Publish concurrency for approving a whole series, given one worker's upload
  bandwidth. For a batch it is bounded by `promotion_parallelism` (at most 2);
  a standalone series approved one file at a time is not bounded.
