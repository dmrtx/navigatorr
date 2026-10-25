# Transcode reconciliation and operational status

Navigatorr owns the lifecycle of submitted HTTP transcode jobs.
The server starts the action reconciler with its maintenance engine and stops
it before closing the state database. MCP clients can disconnect without
stopping polling or result acceptance.

MCP `action_run` admits worker workflows through the same durable queue as the
web interface. It returns the persisted action ID before source inspection or
hashing; `pending` means accepted, not successfully encoded. Preflight belongs
to the service reconciler, so a client timeout or disconnect cannot cancel it
and force another source hash. Use `action_status` for subsequent progress and
real preflight errors. Reusing the same `idempotency_key` and immutable inputs
returns the same action, including failed/completed results and older actions
created before durable receipts. A changed request under that key is rejected.
Omitting the key creates an independent submission and returns its generated
key; supply your own key to recover a response lost before its ID is received.
New admissions still require worker readiness. Recovering an existing receipt
does not require contacting the worker. Process shutdown remains a checkpoint
boundary; this does not promise to resume a partially read hash after a crash.

## Reconciliation

Opted-in workflows resume external waits automatically. A normal poll is due
after five seconds; uncertain worker transport uses bounded backoff up to one
minute. The next poll and known remote identity survive restarts. Execution
leases prevent two continuations sharing the database from accepting the same
result concurrently.

The submitted job identity is saved before the request. A lost response means
the submission is uncertain: the same identity is queried before any retry.
This is separate from retrying an encode that the worker has definitively
reported as failed. User decision waits are never approved by the reconciler.

The workflow's terminal output carries the final worker status. Original
integrity is marked verified only after comparing the original with its baseline:
the SHA-256 for actions that were hashed by the coordinator, or the recorded
size and mtime (`original_integrity_check: metadata`) for actions whose digest
the worker learned while staging; the resolved `original_sha256_pending` field is removed.

### Batch parent/child coordination

A `transcode_batch` parent fans out one `transcode_media` child per item and
links them durably through the batch item (`child_action_id`) and the child's
exact idempotency key (`batch-<batch_id>-<item_key>`). Every remote admission
(the child's transcode submit) re-reads the parent's
cancel/pause policy under the parent's execution lease before contacting the
worker, so a confirmed batch cancel cannot interleave a new submit. Ordinary
status polls and hashes never take that lease, so sibling work is not
serialized; a busy parent defers the admission instead of waiting on it.

Cancelling or pausing the batch stops *new* submissions and retries of pending
children, including after a restart and under the autonomous reconciler. It does
not kill already accepted remote jobs: their identity stays durable and they
continue to be tracked to completion. An uncertain submit is always resolved by
querying the same job identity, and it is never resubmitted after the cancel. A
paused parent defers admissions reversibly; resuming the batch persists the
unpause before fan-out resumes.

A child whose parent link is known but whose parent row cannot be read is failed
closed. A parent that is itself waiting for a user decision blocks new child
admissions reversibly (`parent_waiting_decision`) while accepted jobs keep being
tracked. User decision waits (`waiting_decision`) are never approved, rejected or
cancelled by the reconciler, and an explicit batch cancel preserves that pending
decision together with its identity and candidate.

## Availability and diagnostics

Normal `diagnostics` calls use `/v1/health` for process health and `/v1/ready`
for admission readiness. To request environmental checks too, use:

```text
diagnostics(check_connectivity=true, check_deep=true)
```

The `doctor` result appears separately. A deep diagnostic failure or timeout
does not override healthy readiness. This also prevents a slow deep check from
being required for ordinary status queries or submission.

## Progress and timing

`action_status` keeps its compact response and adds bounded worker telemetry
and reconciliation timestamps. Full reports remain available through
`action_detail`. A workflow step count and encoder progress measure different
things; both remain visible.

- `next_poll_at`, `last_worker_poll_at`, `worker_completed_at`, and `reconciled_at`
  describe coordination.
- `wall_duration_ms` measures the complete action lifetime. The legacy
  `duration_ms` remains an alias for compatibility.
- `queue_duration_ms`, `encode_duration_ms`, `validation_duration_ms`, and
  `reconcile_lag_ms` separate the components when timestamps are available.
  Older workers may omit these values; missing measurements are not zero.
- `last_progress_at`, `worker_heartbeat_at`, `progress_is_stale`, and
  `last_known_progress` distinguish a live worker from a fresh encoder sample.
  A stale percentage alone does not mean the worker stopped. During sample
  metrics (VMAF/CAMBI), validation or publication, the queue shows the phase,
  last measured percentage and heartbeat separately. Sample metrics also show
  the latest reported sample/candidate and resolved units; work can overlap.
  Heartbeats never advance the percentage or refresh `last_progress_at`.
  Missing heartbeat evidence remains unknown, and a heartbeat older than 30
  seconds is warned about even if the percentage is fresh. Full-video encoding
  still warns when progress is stale despite a live heartbeat. External waits,
  recovery decisions and terminal outcomes take precedence over telemetry.

The stable `queued`, `running`, and terminal job statuses are retained. The
additional phase describes acceptance, preparation, reading, encoding,
validation, publication, or completion. Worker slot counts measure occupied runner slots, which include
preparation and publication as well as encoding. Queue position is reported
separately.

### Copied-track duration metadata

A changed Matroska `DURATION` tag alone does not establish audio/subtitle
desynchronization. Remuxing can rewrite that statistic using the final packet's
end timestamp, while the source tag describes the interval from the first
packet to the last packet's end. For example, a subtitle starting at 29.363 s
and ending at 2801.009 s can have tags of either 2771.646 s or 2801.009 s even
when every packet timestamp is unchanged. Check packet timestamps, durations,
counts and sizes alongside copied payload hashes before reporting a timing
regression. Identical payload hashes alone do not verify synchronization.

## Storage failures

Direct SMB errors retain their storage origin: `smb_signing_required`,
`smb_session_invalid`, `smb_auth_failed`, `smb_transport_error`,
`storage_permission_denied`, `storage_io_error`, or `source_unreachable`.
They are not classified as unknown FFmpeg errors.

Recoverable session/signing/transport failures receive one bounded retry with
a newly negotiated and authenticated session. Signing remains required. The
recovery happens at the storage operation before replaying any complete encode;
authentication and permission failures do not trigger repeated attempts.

After encoding has finished, the worker scheduler can retry publication of the
existing local candidate without encoding again. It makes at most three
recovery attempts with backoff. Permanent failures or an exhausted budget expose
`publication_pending` and `recovery_required`; the action waits for a decision
and preserves the local candidate. Resolving a permanent storage problem still
requires operator intervention. A `reconcile` decision checks the existing job
and does not submit a new encode. A `recheck` decision (**Try again** in the UI)
asks the worker for one more publication attempt of the same candidate
(`POST /v1/jobs/{id}/retry-publication`); the worker records the request as
`operator_retry_requested` and restores the budget of automatic attempts; the
next sweep consumes the request and publishes the same candidate, and if that
fails the worker again retries on its own up to the usual three attempts before
asking for a decision once more.

Jobs identify `storage_backend` and distinguish the Navigatorr path, worker
path, SMB share and relative path. Selecting direct SMB does not enable an
implicit local mount fallback. Automatic execution remains HTTP-only.

## Size estimates and progress

Unknown sizes for identified text subtitle codecs use a conservative estimate
of 2 MiB per stream when neither a measured size nor an explicit fallback is
available. Supported codecs are SRT/subrip, `mov_text`, ASS/SSA and WebVTT.
The estimate adds `subtitle_size_estimated:stream_N` uncertainty without
disqualifying an otherwise valid candidate. Bitmap subtitles, unknown codecs,
and malformed byte-count metadata retain their validation requirements.

Worker progress for a goal is the full encode of the current ladder rung and
then its final quality check; it includes the last started window, metric and
stage. Concurrent metrics can overlap; this detail is an activity indicator, not
an exclusive slot assignment. Heartbeats are saved every five
seconds independently of polling, and progress becomes stale after thirty
seconds without a new stage or unit. A heartbeat does not invent progress.

### Failure classes

`ffmpeg_unknown` means a real ffmpeg process ran and failed in a way no more
specific class recognizes. Failure text that nothing recognizes is
`unclassified`, never `ffmpeg_unknown`. A failed job (or worker benchmark) also carries the
worker's own `failure_classification` when the worker knows the cause, and the
coordinator uses that class instead of reading the message:

- `benchmark_evidence_incomplete`: the pipeline finished without a result for a
  candidate window and nothing else explains it. The message names the
  candidate and the window.
- `internal_invariant`: a state the worker's own earlier steps should have made
  impossible, such as an extracted reference sample that is gone.

- `media_probe_failed`: an `ffprobe` inspection gave no usable result (see
  "Source probe"). The class comes from the typed probe error, never from the
  message: the message holds the file path and `ffprobe`'s stderr, and a word in
  a path must not change the class.

- `candidate_invalid`: the encoded candidate failed an integrity check of its
  own (missing, empty, not a regular file, size or digest not what was accepted,
  changed while it was hashed). These errors still wrap `ErrSourceInvalid`, so
  callers that test for it keep working, but they say nothing about the source.

`unclassified`, `benchmark_evidence_incomplete`, `internal_invariant`,
`media_probe_failed` and `candidate_invalid` are not retried automatically. A
manual retry stays available.

`source_changed` is assigned only where the code detects that the source differs
from the one the plan was built on, and each of those sites is a typed error
(`FailureClass()`), so the class reaches the job record and the coordinator
whatever the message says:

- the staged source's digest does not match the plan's (`compareSourceDigest`,
  including the streamed digest) or the file changed while it was hashed for the
  source check;
- the SMB download's size moved while it was copied;
- the source was modified during a benchmark (`verifySourceUnchanged`);
- the resolved plan no longer matches the source's streams (subtitle index,
  codec or a missing stream);
- the accept step that compares hashes or, for learned-digest actions, the
  original's size and mtime, or a status whose `source_sha256` differs from the
  pinned digest.

An unchanged manual retry after any of these is refused, because it would copy
and hash the same source again. The generic file hasher is role-neutral: it
reports "changed while hashing" without a class, and the caller assigns
`source_changed` when it was hashing the source and `candidate_invalid` when it
was hashing the candidate. A message that merely mentions "source" and "sha",
such as a path, does not match.

A recipe's `fallbacks[].when` may name `ffmpeg_unknown` but not `unclassified`,
`benchmark_evidence_incomplete`, `internal_invariant`, `media_probe_failed` or
`candidate_invalid`.
Those classes are never retried automatically, so a fallback on them could never
fire, and validation rejects it with a message saying so.

Audio-only (size mode) video-copy validation probes the source and the candidate
before it compares packets. A probe that fails there is a terminal job failure of
class `media_probe_failed`, not a `preservation_not_met` policy rejection: a
file that could not be inspected has not been shown to differ from the source,
so it is neither preserved nor rejected as altered. The original is untouched;
a manual retry re-runs the probe.

## Source probe

Preflight inspects the source with `ffprobe` before it plans the encode. A
probe that fails returns a typed error with its kind: `timeout`, `unavailable`,
`tool_exit` (the exit status and up to 2 KB of `ffprobe` stderr), or
`bad_output`. `ffprobe` runs with `-v error` and a bounded wait, so a probe
stuck on a stalled read is abandoned instead of hanging the action.

Preflight tries again once, after a short backoff, when the failure is a
timeout or `ffprobe` reported an I/O-looking error such as a stale handle or a
reset connection. Deterministic failures (a corrupt container, a missing
binary, unparsable output) are not retried. When the retry fails too, the
action fails closed and the message carries the kind and the cause. A source
that cannot be inspected is never treated as probed.

## Promotion

`promote_transcode_candidate` is a separate, explicitly approved action.
`transcode_media` continues to produce a candidate. Promotion validates the
original and candidate, preserves a verified recovery copy, reconciles Sonarr
imports, verifies episode/file associations and physical content, removes the
old file, renames and rescans, and then verifies the final library before
removing the recovery copy. A successful result leaves the active file outside
`.navigatorr-candidates` and records the saved bytes.

An ambiguous external mutation is reconciled from durable state; it is never
blindly repeated. Recovery copies remain available when a promotion cannot
finish safely. The existing destructive-operation configuration guard still
applies, in addition to approval of the concrete promotion.
