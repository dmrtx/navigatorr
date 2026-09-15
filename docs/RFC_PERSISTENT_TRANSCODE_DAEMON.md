# RFC: Persistent M1 Transcode Daemon

Status: PR1 proposal (daemon mode + versioned `/v1` API only).
See “Explicit non-goals (NOT part of PR1)” — those futures are described here
so PR1 does not foreclose them, but they are NOT implemented in PR1.

## 1. Current-state map (grounded in this repo)

Binary: `cmd/navigatorr-transcode/main.go`

- Today the worker binary is a short-lived CLI invoked over SSH:
  `doctor | capabilities | submit (stdin JSON) | status <id> | cancel <id> |
  _internal_run <id> | benchmark_* | _internal_benchmark`.
- `submit` reads `transcodeworker.SubmitRequest` from stdin, calls
  `Worker.Submit(ctx, req, selfExe, configPath)`, and prints
  `SubmitResponse` JSON. Exit code signals success/failure.
- Adding `serve` in PR1 keeps every existing subcommand byte-for-byte
  compatible; `serve` is purely additive.

Worker core: `internal/transcodeworker/`

- `worker.go` — `WorkerConfig` (`ffmpeg/ffprobe/state_dir/allowed_roots/
  max_parallel_jobs/quality` + PR1 `http_listen/http_token/http_token_file`),
  `Worker.Submit/Status/Cancel/InternalRun`, `IsPathWithinAllowedRoots`,
  capacity accounting via `countActiveJobs`, per-job file lock
  (`acquireCapacityLock`/`acquireJobLock`), detached runner spawn
  (`exec.Command(selfExe, _internal_run <id>)` with `Setsid:true`).
- `state.go` — `JobRecord` (`job.json`: id/status/source/candidate/profile/
  plan/conversions/pid/process_start_time/created/started/finished/exit/error/
  duration), atomic `SaveJobAtomic` (temp + rename), `IsProcessAlive`,
  `GetProcessIdentity/Tokens`, `IsJobProcessAlive` (PID + start-time +
  `_internal_run <job-id>` match guards against PID recycling),
  `ParseProgress` (`progress.txt` fps/speed/out_time_us).
- `plan.go` — `ProbeSourceStreams` (ffprobe), `BuildExecutionPlan`
  (fail-closed subtitle index/codec matching, missing/extra action rejection).
- `profile.go` — capability boundary: `ValidatePlan` (mkv-only,
  `hevc_videotoolbox`-only, quality 1–100, copy-audio, preserve-subtitles,
  metadata/chapters/attachments required, recipe identity
  `RecipeVersion+RecipeDigest(sha256:…)`, `PlanDigest` must equal
  `transcode.DigestPlan`, subtitle allowlist `copy` vs single
  `mov_text->subrip` conversion, resilience bounds, applied-fallback allowlist,
  `retry_on` allowlist), `ResolveWorkerPlan` (supplied resolved plan validated
  verbatim; narrow legacy `hevc-vt` shim only), `Capabilities()`.
- `builder.go` — `BuildVideoEncoderArgs` (typed profile/pixel/bit-depth
  consistency: `main10<->p010le<->10-bit`), `BuildFFmpegArgs` (explicit
  `-map 0:v?/a?/s?/t?`, `-c:v hevc_videotoolbox -q:v`, `-c:a copy`,
  per-subtitle `-c:s:<n>`, `-c:t copy`; no global `-c copy`, no raw args).
- `ffmpeg.go` — `ResolveToolPath`, `CheckVideoToolboxEncoder`,
  `RunFFmpeg` (worker-side capability re-probe +
  `ValidateVideoToolboxCapabilities` before argv build, `ffmpeg.log` +
  bounded `SummarizeFFmpegError`, spatial-AQ unsupported detection).
- `capabilities*.go` — `ProbeWorkerCapabilities` (ffmpeg version, encoders,
  filters, per-encoder profiles/pixel-formats/options, structured
  `ProbeErrors`, deterministic `CapabilityFingerprint`), protocol version
  `transcode.WorkerProtocolVersion = 1`.
- `http.go` (PR1) — versioned routes over the same `Worker`; `http_test.go`
  covers them with `httptest` without real ffmpeg.
- Benchmark subsystem (`benchmark*.go`, `capabilities_worker.go`) shares
  capacity accounting (`countActiveJobs` counts both `job.json` and
  `benchmark.json`) and the `bench-` ID namespace. PR1 HTTP serves transcode
  jobs only; benchmark HTTP is a future, not PR1.

Coordinator: Navigatorr `main.go` + `transcode/` + `action/`

- `transcode/executor.go` — `Executor` interface
  (`Doctor/Capabilities/Submit/Status/Cancel` + benchmark trio). Worker reuse
  target: a future HTTP executor must implement this same interface.
- `transcode/ssh.go` — current production executor: `SSHExecutor`
  (BatchMode, StrictHostKeyChecking, connect timeout, identity/known-hosts,
  longest-prefix `TranslateLocalToRemote/RemoteToLocal`, job-ID regex
  `^[a-zA-Z0-9_.-]+$`, JSON-over-stdin/stdout for submit, `status/cancel
  <job-id>` argv). Production `main.go` selects the executor with
  `if cfg.Transcode.Enabled && cfg.Transcode.Executor == "ssh"` — PR1 leaves
  this SSH-only.
- `transcode/types.go` — `Plan` (immutable media policy: container, video
  codec/quality/profile/pixel/knobs/bit-depth, audio/subtitle modes,
  `SubtitleActions`, `RecipeVersion/Digest`, `PlanDigest`, `Resilience`,
  `AppliedFallbacks`), `DigestPlan` (sha256 over plan minus `PlanDigest`),
  `Request/Job/JobStatus`, `SSHConfig/PathMapping`.
- `transcode/capabilities.go` — versioned `WorkerCapabilities` + fingerprint
  (`Compute/VerifyCapabilityFingerprint`; `FFmpegPath` excluded so identical
  capability sets match across nodes).
- `action/transcode_submit.go` (+ batch/poll/validate/accept steps) — the
  Navigatorr action state machine owns submission, polling, retry/backoff
  budgets, validation, and acceptance. PR1 does NOT touch it.
- `config.yaml.example` — `transcode.executor: "ssh"`, recipes
  (`builtin/file/https/github`), `transcode.ssh` (host/user/key/known-hosts/
  timeouts/path_mappings). No HTTP executor config in PR1.
- `queue/http.go` — precedent for bearer auth (`Bearer` scheme
  case-insensitive, `subtle.ConstantTimeCompare`, JSON error envelope,
  `MaxBytesReader`, content-type discipline). PR1 follows the same pattern.

Docs: `docs/TRANSCODING.md` (policy-as-data, recipe bundle schema v1/v2,
plan resolution + worker boundary, LKG/rollback, candidate-only invariant,
SHA-256 source integrity, benchmark optimization).

## 2. Navigatorr vs worker responsibilities

- Navigatorr owns immutable media policy and result acceptance:
  - Probe source, resolve recipe profile into an exact `resolved_plan`
    (per-stream subtitle actions, resilience bounds, applied fallbacks) +
    `plan_digest` (`transcode.DigestPlan`).
  - Submit `Request{id, source_path, candidate_path, profile, plan}`.
  - Poll `Status`, enforce retry/backoff budgets, run post-transcode
    validation (duration, video/audio/subtitle/attachment/chapter checks),
    hash source before/after, accept or reject the candidate. Originals are
    never overwritten (`replace_original: true` rejected; candidate-only).
  - Never reinterprets worker operational state as media truth.
- Worker owns operational execution state:
  - Validate but never reinterpret recipe/codec/quality: `ValidatePlan` +
    `PlanDigest` check + `BuildExecutionPlan` source re-probe (index/codec
    match, missing/extra subtitle actions fail closed). Legacy `hevc-vt`
    shim is rolling-upgrade-only, not the policy path.
  - Own `job.json` lifecycle (`queued/running/completed/failed/cancelled`),
    `progress.txt`/`ffmpeg.log`, PID + start-time identity, crash detection
    (dead/recycled PID → `failed: process terminated unexpectedly`),
    candidate cleanup on cancel (never touching source).
  - Build the exact FFmpeg argv itself (`BuildFFmpegArgs`); accept no
    shell/raw ffmpeg args over any transport.
- Transport failure is not encode failure (invariant). No automatic SSH
  execution fallback after HTTP uncertainty (invariant). SSH stays
  bootstrap/diagnostics/manual recovery only (invariant).

## 3. Reuse of `transcode.Executor`

- PR1 reuses `internal/transcodeworker.Worker` directly inside the
  `navigatorr-transcode serve` daemon (`Submit/Status/Cancel` verbatim).
- A future Navigatorr-side `HTTPExecutor` must implement
  `transcode.Executor` (`Doctor/Capabilities/Submit/Status/Cancel`) with the
  same fail-closed semantics as `SSHExecutor` (job-ID regex, path mappings,
  protocol-version + fingerprint checks, candidate-path translation).
- HTTPExecutor integration is NOT part of PR1. Production `main.go` executor
  selection remains SSH-only in PR1.

## 4. Versioned `/v1` HTTP API (PR1)

Base: `http://<listen>/v1`. Structured JSON only; `Content-Type:
application/json` on responses; error envelope `{"error":"…"}`.

- `GET /v1/health` — liveness. No disk/ffmpeg/job access. Returns
  `{"ok":true,"service":"navigatorr-transcode","version":…,"commit":…}`.
  Method guard: non-GET → 405.
- `GET /v1/ready` — readiness. Minimal/deterministic: `StateDir` must be
  creatable and genuinely writable, verified by a bounded
  create/write/close/remove probe (`MkdirAll` alone could pass on an
  existing read-only directory); returns `{"ready":true,"state_dir":…,
  "max_parallel_jobs":…}` or 503 `{"ready":false,"error":…}`. Deliberately
  does NOT gate on darwin/arm64, ffmpeg presence, encoder probes, or
  allowed-root contents (keeps probes usable in CI/pre-provisioning).
- `POST /v1/jobs` — body is `SubmitRequest`
  `{id, source_path, candidate_path, profile?, plan?}` (unknown fields
  rejected via `DisallowUnknownFields`; body capped at 1 MiB). Validates job
  ID (`ValidateTranscodeJobID`), then delegates to `Worker.Submit`.
  Codes: 201 `SubmitResponse` on new `queued`; 200 on idempotent
  `running/completed` re-submit; 400 validation/plan/path failures; 409
  `worker busy` (temporary PR1 behavior); 500 spawn/persist/lock failures.
- `GET /v1/jobs/{id}` — validates `{id}`, delegates to `Worker.Status`.
  200 `JobStatusResponse`; 400 invalid ID; 404 unknown/missing job
  (detected via “job not found”/“reading job file”/ENOENT).
- `POST /v1/jobs/{id}/cancel` — validates `{id}`, delegates to
  `Worker.Cancel`. 200 cancelled record; 400 invalid ID; 404 unknown job.
  Deeper paths (`/v1/jobs/a/b`) → 404; encoded separators (`%2F/%5C`) → 400.
- No shell/raw ffmpeg args anywhere. All existing guards are reused:
  job-ID validation, `bench-` rejection, path traversal fail-closed,
  `AllowedRoots`, `candidate != source`, source exists/not-dir, supplied
  resolved-plan validation + `PlanDigest` checks.

## 5. Durable worker queue future (NOT PR1)

- Problem: PR1 keeps `worker busy` (count of alive `job.json`/`benchmark.json`
  vs `MaxParallelJobs` → 409/`worker busy` error). Callers must retry/backoff.
- Future: authoritative persistent queue in `StateDir` (e.g. `queue.json`
  + per-job records, atomic rename, file locks) so `POST /v1/jobs` enqueues
  durably and workers drain FIFO/subject to `MaxParallelJobs`; `GET`
  reports `queued` position; busy becomes queue depth, not rejection.
- PR1 deferral: no queue files, no queue endpoints, no queue-depth API.

## 6. Strong idempotency future (NOT PR1)

- Today: weak idempotency by `id` only (existing `running/queued`+alive →
  return same; `completed` → return completed; dead PID → mark failed).
- Future: `idempotency_key + execution_spec_digest` (digest over normalized
  source/candidate/profile/plan bytes) so same key + same spec → same job,
  same key + different spec → deterministic conflict (e.g. 409 with
  `idempotency_conflict`), preventing silent reuse across changed media
  policy. Requires persisted spec digest per job.
- PR1 deferral: no new headers/fields, no spec digest, no conflict codes.

## 7. Transport uncertainty / reconciliation future (NOT PR1)

- Invariant (already): transport failure ≠ encode failure; no auto SSH
  fallback after HTTP uncertainty.
- Future: reconciliation hardening — Navigatorr treats timeouts/disconnects
  as “unknown”, then reconciles via idempotent `GET /v1/jobs/{id}` (and later
  completion markers) before deciding retry/cancel; retry budgets distinguish
  “never accepted” (safe to retry) from “possibly running” (must reconcile
  first). May add `Retry-After`/`409 busy` + `waiting_for_slot` surfacing
  without burning transient budgets (as batch already does for SSH busy).
- PR1 deferral: no reconciliation protocol changes, no new status fields.

## 8. Detached runner + completion marker future (NOT PR1)

- Today: `Submit` spawns `selfExe _internal_run <id>` with `Setsid:true`
  (survives SSH disconnect); `_internal_run` drives
  probe → `BuildExecutionPlan` → `RunFFmpeg` → final `job.json` write.
- Future: explicit completion marker (e.g. `done.json`/marker file or
  atomic status + fsync discipline) so crash-vs-complete is unambiguous even
  if the runner dies between ffmpeg exit and `job.json` persist; daemon
  startup scans and heals orphans deterministically.
- PR1 deferral: no marker files, no runner protocol change; daemon reuses the
  existing detached spawn verbatim.

## 9. SMB lease manager future (NOT PR1)

- NAS-backed sources/candidates over SMB need coordinated leases (break
  management, stale-handle recovery, write-exclusion) so concurrent
  readers/encoders do not observe torn files.
- PR1 deferral: no SMB code, no lease API, no mount management. `AllowedRoots`
  + local filesystem semantics only.

## 10. Local-output-first and final NAS `.partial.<job_id>` future (NOT PR1)

- Future: encode to local staging (fast, crash-atomic), then finalize to NAS
  via `.partial.<job_id>` temp + atomic rename + verification, so partial
  NAS files are never mistaken for complete candidates and interrupted
  finalizations resume/clean deterministically.
- PR1 deferral: no staging dir, no `.partial.<job_id>` finalize, no
  copy/rename pipeline. `CandidatePath` is written directly as today.

## 11. Retry / failure ownership

CURRENT PR1: existing Navigatorr retry behavior is unchanged. Navigatorr
(coordinator) keeps its current retry budgets and failure classification
(`ResiliencePlan`: `max_attempts/transient_retries/backoff/max_fallbacks/
retry_on/applied_fallbacks`; batch `waiting_for_slot` without burning
budgets). The worker surfaces typed failures (`worker_busy`,
`encoder_capability_unsupported`, probe/plan digest mismatches, ffmpeg
tail summaries) but takes on no new retry role in PR1.

TARGET architecture (NOT PR1, no implementation here): the worker owns
bounded operational retries — `storage_io_transient`, mount/staging/
final-copy and other explicitly retryable execution failures retried
inside the worker within immutable limits sent by Navigatorr
(`max_attempts/transient_retries/backoff`, allowlisted `retry_on` classes
only; media policy itself never changes across attempts). Navigatorr owns
media policy and result acceptance (resolve `resolved_plan` + `plan_digest`,
validate the candidate, hash source before/after) and mirrors worker
attempt/retry metadata (`attempt/retry_count/applied_fallbacks`) rather
than re-deciding operational retries. Transport failure, unreachable
worker, and reconciling-unknown consume zero encode retry budget and never
cause a duplicate submit: uncertainty reconciles via idempotent
`GET /v1/jobs/{id}` first.

PR1 change: `worker busy` maps to HTTP 409 (retryable) instead of SSH
stderr; taxonomy itself unchanged. Retry taxonomy changes are NOT PR1.

## 12. Security

- Structured JSON only; no shell, no raw ffmpeg args, no command injection
  surface. `DisallowUnknownFields` + 1 MiB body cap.
- Job-ID fail-closed: regex `^[a-zA-Z0-9_.-]+$`, length ≤128, reject
  `.`/`..`/`..` substrings/`/\:\x00`/`%2F`/`%5C`/`%2E`, reserved device names,
  `bench-` namespace. An outer guard rejects raw dot/empty segments and
  encoded traversal before ServeMux normalization (never redirects); auth
  runs before that guard so a configured token protects traversal paths too
  (401 unauthenticated / 400 authenticated).
- Error envelopes are bounded (2 KiB cap on user-visible strings).
- Path guards: `filepath.Clean` + `IsPathWithinAllowedRoots` (strict
  prefix), `candidate != source`, source must exist and not be a dir,
  job-dir traversal check (`Rel == id`, no `/`,`\`,`..`), benchmark/transcode
  collision rejection. Cancel only removes `candidate` when
  `candidate != source`.
- Bind/auth: default `127.0.0.1:8097` (loopback). Non-loopback
  (`0.0.0.0`/`:`/LAN/DNS) without bearer token fails closed at startup
  (`ValidateServeAddr`). Token via `http_token` or `http_token_file`
  (trimmed); comparison with `subtle.ConstantTimeCompare`; `Bearer` scheme
  case-insensitive (same as `queue/http.go`). When a token is configured it
  is required on ALL `/v1/*` routes including health/ready (documented;
  LaunchDaemon probes should use loopback-no-auth or send the token).
- Timeouts: `ReadHeader 5s / Read+Write 15s / Idle 60s`; explicit `net.Listen`
  so bind failures exit loudly instead of looking healthy.
- No secrets in logs/errors; ffmpeg errors are bounded/sanitized
  (`SummarizeFFmpegError` tail, 8 KiB/300-char caps).

## 13. LaunchDaemon / deployment split

- M1 host runs `navigatorr-transcode serve` as a LaunchDaemon (loopback by
  default; token file with `0600` when bound wider). Owns `state_dir`,
  `allowed_roots` (`/Volumes/media`), ffmpeg/ffprobe paths.
- Navigatorr container keeps SSH executor in PR1 (no network change, no new
  env/config). Future cutover swaps/adds an HTTP executor pointing at the
  daemon (with path mappings local↔remote) while SSH remains for bootstrap/
  diagnostics/manual recovery. No production cutover in PR1.
- Health: LaunchDaemon `KeepAlive` + `GET /v1/health` (loopback or bearer);
  readiness gate `GET /v1/ready` before routing jobs.

## 14. Phased migration

1. PR1 (this change): `serve` + `/v1` + tests; all CLI unchanged; Navigatorr
   stays SSH-only. Verify daemon locally, no cutover.
2. HTTPExecutor (future): implement `transcode.Executor` over `/v1`
   (protocol-version + fingerprint checks, path translation, reconciliation
   on uncertainty); shadow/dual-run behind config; SSH remains fallback for
   bootstrap only, never auto-fallback after HTTP uncertainty.
3. Durability (future, in order): authoritative queue → strong idempotency
   (`idempotency_key + execution_spec_digest`) → completion markers +
   reconciliation hardening → local-staging/`.partial.<job_id>` finalize →
   SMB leases. Each lands with its own RFC/tests/cutover.

## 15. Required tests

PR1 (implemented in `internal/transcodeworker/http_test.go`, no real ffmpeg):

- Health + ready success (200 JSON, `application/json`).
- Malformed JSON → 400 `{"error":…}`.
- Invalid job IDs / path traversal fail closed (POST body + URL params +
  encoded `%2F/%5C/%2E` + `bench-` + overlong + reserved) → 400. Fixed
  invariant: traversal paths never redirect and are rejected with 400 by an
  outer guard before ServeMux path normalization; when a token is
  configured, unauthenticated traversal yields 401 (auth first) and
  authenticated traversal yields 400 — never 3xx, never a `Location` header.
- Auth-before-traversal ordering: unauthenticated traversal → 401,
  authenticated traversal → 400, both with no redirect.
- Error envelopes bounded (2 KiB cap): oversized invalid fields yield a
  truncated 400 envelope, never an unbounded body.
- Ready verifies genuine writability via a bounded create/write/close/remove
  probe (no leftover files); file-as-`StateDir` → 503.
- Source outside `AllowedRoots` → 400 with “outside allowed roots”.
- Unknown job `GET`/`POST cancel` → 404.
- Bearer auth: missing/wrong/malformed-scheme → 401 on every route when a
  token is set; correct `Bearer` → 200; empty-token server allows no-auth.
- Submit/status/cancel wiring without ffmpeg: seeded `running` job → `GET`
  200; idempotent `POST` same ID → 200 (no spawn); invalid plan → 400 and no
  job dir created; busy slot → 409; `POST …/cancel` → 200 `cancelled`,
  follow-up `GET` stays `cancelled`, partial candidate removed.
- Safe bind: `IsLoopbackBind` table (127.0.0.1/::1/localhost true;
  0.0.0.0/bare-port/LAN/DNS false); `ValidateServeAddr` rejects
  non-loopback without token; token-file load/trim/precedence.

Existing suites that must stay green: `worker_test.go` (path validation,
progress, atomic state, crash/busy/idempotent/cancel/recycled-PID/plan
guards), `plan_test.go` (digest/mutation, subtitle boundary, legacy shim),
`ffmpeg/capabilities/benchmark` tests, `transcode/ssh_test.go`.

Future phases add: queue durability/crash-recovery tests, idempotency-key /
spec-digest conflict tests, reconciliation (uncertainty → GET → decide)
tests, completion-marker healing tests, staging/`.partial.<job_id>`
finalize tests, SMB lease tests, `HTTPExecutor` conformance tests against
`httptest` daemons.

## Explicit non-goals (NOT part of PR1)

- Authoritative persistent queue — NOT PR1 (busy stays 409).
- Strong idempotency (`idempotency_key + execution_spec_digest`) — NOT PR1.
- SMB lease manager — NOT PR1.
- Local staging / final NAS `.partial.<job_id>` — NOT PR1.
- Completion markers — NOT PR1.
- Reconciliation hardening (beyond today’s `GET` + crash detection) — NOT PR1.
- Retry taxonomy changes — NOT PR1.
- HTTPExecutor integration / Navigatorr `main.go` executor selection change /
  production cutover — NOT PR1 (SSH-only stays).
- SMB management, batch concurrency, action state machine, benchmark HTTP,
  deployment/LaunchDaemon plist changes — NOT PR1.
- Unrelated refactors — NOT PR1.
