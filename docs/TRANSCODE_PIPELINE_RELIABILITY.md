# Transcode reconciliation and operational status

Navigatorr owns the lifecycle of submitted HTTP transcode and benchmark jobs.
The server starts the action reconciler with its maintenance engine and stops
it before closing the state database. MCP clients can disconnect without
stopping polling or result acceptance.

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
integrity is marked verified only after comparing the SHA-256 with the original
baseline; the resolved `original_sha256_pending` field is removed.

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

The stable `queued`, `running`, and terminal job statuses are retained. The
additional phase describes acceptance, preparation, reading, encoding,
validation, publication, or completion. Worker slot counts measure occupied runner slots, which include
preparation and publication as well as encoding. Queue position is reported
separately.

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
and does not submit a new encode.

Jobs identify `storage_backend` and distinguish the Navigatorr path, worker
path, SMB share and relative path. Selecting direct SMB does not enable an
implicit local mount fallback. Automatic execution remains HTTP-only.

## Benchmarks

Unknown sizes for identified text subtitle codecs use a conservative estimate
of 2 MiB per stream when neither a measured size nor an explicit fallback is
available. Supported codecs are SRT/subrip, `mov_text`, ASS/SSA and WebVTT.
The estimate adds `subtitle_size_estimated:stream_N` uncertainty without
disqualifying an otherwise valid candidate. Bitmap subtitles, unknown codecs,
and malformed byte-count metadata retain their validation requirements.

Benchmark progress includes the last started sample, candidate, metric and
stage. Concurrent metrics or encodes can overlap; this detail is an activity
indicator, not an exclusive slot assignment. Heartbeats are saved every five
seconds independently of polling, and progress becomes stale after thirty
seconds without a new stage or unit. A heartbeat does not invent progress.

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
