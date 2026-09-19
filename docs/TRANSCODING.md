# Transcoding architecture

Navigatorr treats transcoding policy as data, not as worker code.

The stable image/worker exposes a small hard-coded **capability boundary**: supported containers, encoders, stream operations, preservation features, and safe codec conversions. A versioned **recipe bundle** decides which of those capabilities to use for a profile and how to handle container compatibility. Recipes cannot add worker capabilities, raw FFmpeg arguments, shell commands, or arbitrary binaries.

This separation is intentional: a compatibility or tuning problem should normally be fixed by publishing/editing a recipe and reloading it, not by rebuilding the Navigatorr or worker image.

## Normal failure workflow

When a transcode fails because the current policy does not describe a safe way to handle the media:

1. Inspect the source streams and the typed failure classification.
2. Change or add a recipe/profile using only supported declarative fields.
3. Increment `bundle_version` when publishing a changed version.
4. Run `recipe_reload` for a file source or `recipe_update` for a remote source.
5. Confirm `recipe_status` reports the expected version and SHA-256 digest.
6. Retry `transcode_media`.

The existing image is unchanged. If the new bundle is malformed, unsafe, incompatible with the worker capability boundary, has the wrong digest, or cannot be fetched, Navigatorr keeps the last-known-good bundle active.

A new image is required only when the engine itself needs a **new capability** (for example, a new encoder or conversion implementation), not when an existing capability merely needs different policy.

## Recipe sources

`transcode.recipes.source` accepts:

- `builtin` — embedded safe default bundle; useful for zero-config startup.
- `file` — local YAML or JSON bundle, suitable for a bind mount or configuration management.
- `https` — strict HTTPS manifest that points to a same-host versioned bundle and includes its SHA-256 digest.
- `github` — versioned manifest/bundle hosted on `raw.githubusercontent.com`, selected by `stable`, `beta`, or a pinned revision.

Example configuration:

```yaml
transcode:
  enabled: true
  executor: ssh
  default_profile: hevc-vt-balanced

  recipes:
    source: file
    path: /config/transcode-recipes.yaml
    cache_dir: /config/cache/transcode-recipes

  ssh:
    host: 192.0.2.10
    user: transcoder
    remote_binary: /opt/homebrew/bin/navigatorr-transcode
```

GitHub channel example:

```yaml
transcode:
  recipes:
    source: github
    repository: example/transcode-recipes
    channel: stable
    refresh_interval: 15m
    cache_dir: /config/cache/transcode-recipes
```

A pinned `revision` may be used instead of `channel`. `main` and `master` are deliberately rejected as pinned revisions so a supposedly immutable deployment cannot silently change underneath a running installation.

## Custom file recipes

To load custom recipes from a local file, configure `source: file`:

```yaml
transcode:
  recipes:
    source: file
    path: /root/.config/navigatorr/transcode-recipes.yaml
    cache_dir: /root/.cache/navigatorr/transcode-recipes
```

When running in Docker, mount the recipe file into the container:

```text
/home/navigatorr/navigatorr/transcode-recipes.yaml:/root/.config/navigatorr/transcode-recipes.yaml:ro
```

> [!NOTE]
> When using `source: file`, ensure your custom bundle includes the profile specified by `transcode.default_profile` (which defaults to `hevc-vt-balanced`), or configure `default_profile` to match one of the profiles in your file (such as `general-hevc` or `auto`).

> [!WARNING]
> When configuring `source: builtin`, you must not specify `path`, `manifest_url`, or `repository`. Specifying any of these fields with `builtin` will fail configuration validation.

## Bundle schema

A bundle is strictly decoded. Unknown fields fail validation.

```yaml
schema_version: 1
bundle_version: "2026.09.2"

containers:
  mkv:
    subtitle_copy:
      - subrip
      - ass
      - ssa
      - hdmv_pgs_subtitle
    subtitle_conversions:
      mov_text:
        target_codec: subrip
        reason: matroska_compatibility

profiles:
  hevc-vt-balanced:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
    audio:
      mode: copy
    subtitles:
      mode: preserve
      convert_incompatible: true
    preserve:
      metadata: true
      chapters: true
      attachments: true
    resilience:
      max_attempts: 3
      transient_retries: 2
      retry_backoff_seconds: [5, 20]
      max_fallbacks: 1
      fallbacks:
        - when: container_subtitle_incompatible
          action: apply_container_conversion
        - when: worker_busy
          action: retry
        - when: ssh_transient
          action: retry
```

JSON is also accepted because the strict YAML decoder supports JSON syntax. The schema intentionally contains no raw argument field.

Local `transcode.profiles` entries are retained as deterministic whole-profile overrides for backwards compatibility. They do not extend worker capabilities and are validated through the same recipe rules.

## Plan resolution and worker boundary

Before SSH submission, Navigatorr probes the source and resolves every subtitle stream into an exact action. For the built-in Matroska policy, for example:

- `mov_text` -> `subrip` when the profile authorizes the container conversion fallback.
- `ass`, `ssa`, `subrip`, supported bitmap subtitles, and other allowlisted Matroska-compatible codecs -> `copy`.
- an unknown codec with no recipe-authorized safe conversion -> fail closed before submission.

The resulting immutable plan contains the recipe version, recipe SHA-256 digest, plan SHA-256 digest, exact per-stream actions, bounded retry policy, and applied fallbacks. The worker probes the source again and verifies stream index/codec plus the plan digest before constructing FFmpeg argv. A recipe therefore cannot smuggle in unsupported operations.

The worker still has one narrowly scoped legacy `hevc-vt` profile-only compatibility shim for rolling upgrades. Legacy profile submissions are `FFmpeg-argv compatible; no new encoder defaults are injected`, but are not fully behavior-compatible because runtime capability probing occurs on the worker before encode execution. New Navigatorr submissions always send a fully resolved plan; the shim is not the normal policy path and should be removed after old clients are retired.

## Hot reload, LKG, and rollback

Recipe activation is atomic. A candidate bundle is fetched, integrity-checked, parsed, semantically validated, cached, and persisted before the active pointer is swapped. Jobs that already resolved a plan continue with that exact plan even if a newer bundle becomes active.

Versioned bundles are cached as `<cache>/<bundle_version>/bundle.yaml`. Reusing the same `bundle_version` with different bytes/digest is rejected as an immutable-version collision.

The MCP tools are:

- `recipe_status` — active source/version/digest, LKG and last sanitized error.
- `recipe_reload` — re-read and atomically activate the configured source when valid.
- `recipe_update` — fetch the configured remote/versioned source now.
- `recipe_rollback` — atomically activate the previous validated cached bundle.

Bad updates never replace the active LKG.

## VideoToolbox quality semantics

For `hevc_videotoolbox`, FFmpeg maps `-q:v` to VideoToolbox's quality property as a value from 0 to 1. Higher numeric `-q:v` values therefore mean **higher quality**, not more compression.

The built-in profiles use:

- `hevc-vt-balanced`: 65
- `hevc-vt-quality`: 75
- `hevc-vt-space`: 55

## Safety and validation

`transcode_media` is candidate-only. `replace_original: true` is rejected. The original is SHA-256 hashed before work begins and re-hashed at acceptance.

Post-transcode validation checks duration, video codec, audio stream count/codecs/languages/channels, subtitle count and expected copy/conversion codecs, subtitle languages and `forced`/`default` dispositions, attachments, and chapters. A discrepancy requires rejection or an explicit user decision; the original is not deleted or overwritten.

### Transcode I/O optimization (worker-local validation + shared source cache)

Old data flow (per optimized `transcode_media`):

```text
NAS source --(benchmark seeks)--> benchmark samples/metrics
NAS source --(full staging copy)--> full encode --> publish candidate to NAS
NAS candidate --(full coordinator ffprobe validation)--> accept
```

New data flow:

```text
NAS source --(one local cache copy)--> shared source cache (<local_work_dir>/_source-cache)
shared cache --(local reads)--> benchmark samples/metrics
shared cache --(local copy to per-job staged input)--> full encode locally
local candidate --(full structural/media validation locally, fail closed)--> publish once to NAS
published NAS object --(lightweight stat/size/identity verification worker + coordinator)--> accept
coordinator full stream/policy checks remain as defense-in-depth
```

Safety invariants (unchanged):

- Full candidate validation runs worker-local BEFORE publish; a bad local
  candidate fails the job and its local file is removed without ever publishing.
- Post-publish verification is lightweight and independent (regular-file
  existence + exact size match for filesystem destinations; hash-verified
  exclusive publish for SMB-direct). Mismatches fail closed and stay resumable.
- Original-preservation, no-clobber (`O_EXCL` / exclusive rename / hard-link,
  never overwriting an existing destination), cancellation-wins,
  crash/resume (EncodeComplete checkpoint never re-encodes), idempotency
  (execution-spec digest + idempotency key), and promotion separation are
  preserved.
- `benchmark_transcode` stays non-destructive and never creates a permanent
  `.navigatorr-candidates` entry. It may populate/read the shared cache but
  only cleans its own `samples/` workspace and legacy per-job downloads, never
  shared cache entries.

Source cache identity and cleanup:

- Key: `sha256(clean_source_path | size | mtime_nanos | mode)` with a JSON
  sidecar (`source`, `size`, `mod_time_unix_nano`). Any size/mtime change is a
  miss; stale content is never reused.
- Location: `<local_work_dir>/_source-cache/<key><ext>` plus `<key>.meta.json`,
  always on worker-local storage, never on NAS roots.
- Concurrency: atomic temp + no-clobber publish; concurrent populators race
  safely (one wins, losers revalidate the winner). Per-job staged inputs are
  fulfilled by local cache-to-staged copies, never by sharing mutable files.
- Bounds/cleanup: `source_cache_max_bytes` (default 20 GiB) and
  `source_cache_ttl_hours` (default 72h). Sweeps are best-effort, never fail a
  job, never delete temp/sidecar-less files, and skip recently-written entries
  that may belong to active jobs. Per-job completion/cancel cleanup never
  deletes cache paths.
- `staging_policy` (`auto`/`never`/`always`) and SMB-direct/path-mapping
  semantics are preserved. The cache is consulted only when the source would
  otherwise require a NAS read (external path per policy, or SMB-direct
  mapping). `disable_source_cache: true` restores legacy per-job staging
  exactly.

Worker config (`~/.config/navigatorr-transcode/config.yaml`):

```yaml
staging_policy: auto
external_roots: ["/Volumes/media"]
local_work_dir: "/tmp/navigatorr-work"
# All three below are optional; shown with conservative defaults.
disable_source_cache: false
source_cache_max_bytes: 21474836480
source_cache_ttl_hours: 72
```

Ansible: no configuration changes are required. Existing worker configs keep
working; the new keys are optional with safe defaults. To opt out, set
`disable_source_cache: true`.

## Benchmark-driven optimization (recipes v2)

Profiles without `optimization` follow the legacy flow with no observable change. A profile with `optimization.enabled: true` (schema_version 2) instead probes a small set of encoder configurations over deterministic source samples, scores them objectively, and transcodes the full file only with the concrete winning parameters.

```text
preflight + InspectDetailed
→ capability handshake
→ resolve optimization policy
→ SamplePlanner (deterministic windows)
→ remote sequential benchmark
→ QualityEvaluator (VMAF and/or SSIM)
→ OutputEstimator
→ CandidateSelector (explainable decision)
→ benchmark-only: finish with report
→ optimization enabled: materialize immutable winning plan + PlanDigest
→ normal full transcode → existing validation → candidate-only acceptance
```

### Actions

- `benchmark_transcode` is candidate-only: it shares preflight and models with `transcode_media`, runs the benchmark, persists the report, and never executes the full file nor creates a permanent candidate.
- `transcode_media` with an optimized profile adds `submit_benchmark` / `wait_benchmark` steps before the existing `submit_transcode` / `wait_transcode` / `validate_result` / `accept_result` steps. It uses the winner's concrete knobs (quality, profile, pixel format, bit depth) for the full transcode and refuses to continue when there is no valid winner.

### Optimization policy reference

```yaml
optimization:
  enabled: true
  sampling:
    strategy: distributed        # distributed | uniform | relative_positions
    sample_count: 3              # 1-20, must match len(positions)
    sample_seconds: 20           # (0, 120]
    positions: [0.2, 0.5, 0.8]   # strictly increasing, in (0, 1)
  quality:
    preferred_metric: vmaf       # vmaf | ssim
    vmaf: {target: 96.0, minimum: 95.0, marginal_tolerance: 0.5}
    ssim: {target: 0.99, minimum: 0.98, marginal_tolerance: 0.005}
  search:
    max_candidates: 5            # 1-20, >= len(quality_values)
    quality_values: [55, 60, 65, 70, 75]  # strictly increasing -q:v list
  size:
    preferred_total_bitrate_kbps: {min: 2100, max: 3650}
    soft_max_total_bitrate_kbps: 4250      # soft guidance, not a hard target
```

Omitted blocks fall back to the defaults above; an explicitly requested metric block must carry its own thresholds. Quality always wins over hitting a size number: size guidance is soft.

A complete, loader-validated example lives in `docs/examples/transcode-recipes-optimization.yaml`. The embedded builtin bundle stays schema_version 1; optimization is adopted via `file`/`https`/`github` sources, never by editing worker code.

### Sampling

Positions mark the desired center of each sample (`start = position * duration - sample_seconds / 2`), clamped to the valid range with overlap avoided when duration allows. Short videos use fewer samples or the whole segment without exceeding duration. Reference and candidate stay frame-aligned (lossless FFV1 reference, PTS normalized) so VMAF/SSIM compare identical frames. Temporary outputs live only under `state_dir/<job-id>/samples/` and only known job files are ever cleaned. For quick debugging, `sample_count: 1` with `sample_seconds: 60` covers the simple single-sample case.

### Metrics

Every candidate is scored against the same original sample. The aggregate must clear the configured minimum and no single sample may fall below the per-sample gate. VMAF (0-100) and SSIM (0-1) have independent thresholds and marginal tolerances; scores are never invented and thresholds are never converted between metrics. If the preferred metric is unavailable but the other has configured thresholds, the other may decide; with no valid metric the benchmark reports without an automatic winner.

### Selection

Invalid, incomplete, or below-minimum candidates are rejected. Among valid candidates the smallest that reaches target wins; if none reaches target but some clear minimum, the highest quality wins with size as tie-break; tiny gains above target do not justify large bitrate increases (metric-specific marginal tolerance). Otherwise there is no automatic winner, and `transcode_media` stops before any full submit. Every outcome carries stable reason codes plus a human-readable explanation, and the full candidate list with exact parameters is persisted in order.

### Bit depth

- 8-bit source: only 8-bit candidates (`main` + `yuv420p`).
- 10-bit source: only 10-bit candidates (`main10` + `p010le`).
- There is no "convert 8-bit to 10-bit" optimization; existing Main10 is never marketed as an optimization.
- If the worker cannot preserve the required depth, the job returns review or an explicit failure, never a silent downgrade; the final candidate is re-validated for bit depth.
- VMAF on 10-bit sources fails closed unless the worker reports verified 10-bit VMAF capability; SSIM handles 10-bit natively. Prefer `ssim` for 10-bit SDR policies.

### HDR and Dolby Vision

Automatic selection supports SDR only. HDR/Dolby Vision sources (transfer, primaries, color space, tags, or DOVI side data) are preserved in the report but fail closed: review or an explicitly informational benchmark with no automatic winner. No silent SDR thresholds, no implicit tone mapping.

### Speed priority

There is no separate "fast" benchmark mode. Speed is expressed per profile through the typed VideoToolbox switches documented in `docs/VIDEOTOOLBOX_PROFILES.md` (`prioritize_speed`, `spatial_aq`, `realtime` with pointer-boolean semantics: omitted means "emit nothing", explicit values are required capabilities and fail closed when unsupported).

### Capabilities

Before any benchmark the coordinator fetches versioned `WorkerCapabilities` (protocol version, Navigatorr/worker build, FFmpeg version, encoders, `libvmaf`/`ssim` filters, per-encoder profiles/pixel formats/options, structured probe errors, deterministic `CapabilityFingerprint`). Missing capabilities fail the job with a classifiable error before any encode starts; an old worker safely rejects the explicit benchmark protocol command.

### Troubleshooting

| Symptom | Meaning | Action |
|---|---|---|
| `worker_busy` / `waiting_for_slot` | All worker slots occupied | Wait; retries use bounded backoff and do not burn the transient budget |
| No winner, reason `no_candidate_met_minimum_quality` | Nothing cleared the quality floor | Loosen thresholds deliberately, or keep the original; the engine will not guess |
| HDR/DV review | Fail-closed on HDR metadata | Expected; do not force SDR thresholds onto HDR |
| `encoder_capability_unsupported` | Recipe asks for an option the worker FFmpeg lacks | Change the recipe (typed fields only), bump `bundle_version`, `recipe_reload` |
| `replace_original: true` rejected | Candidate-only invariant | Keep `false`; originals are hashed before/after and never overwritten |
| Benchmark retry backed off | `benchmark_retry_not_before` set | Automatic; resume after the reported time |

### Validation status

Automated suites cover the full orchestration matrix with stubbed executors and fixture probes (8-bit/10-bit SDR, HDR/DV fail-closed, vmaf/ssim/both, no-winner, busy/retry/backoff, cancellation on both sides of the benchmark boundary, reload/resume, deterministic digests, candidate-only acceptance, source immutability, no secret/path leakage). Live Apple Silicon evidence (real capability handshake, real inspection, real VMAF/SSIM scoring, real recipe loading) versus blocked items (hardware VideoToolbox encodes unavailable in the validation sandbox) is recorded per item in `docs/TRANSCODE_OPTIMIZATION_IMPLEMENTATION_PLAN.md` phase 8.

## Rollout

A safe rollout is:

1. Deploy the new Navigatorr and worker once so the capability engine understands versioned plans.
2. Start with `source: builtin` and verify `recipe_status`.
3. Move policy to `source: file` or a versioned GitHub/HTTPS source.
4. For future policy/compatibility changes, publish a new recipe bundle version and call `recipe_update`/`recipe_reload` instead of rebuilding the image.
5. If a recipe causes trouble, use `recipe_rollback`; fetch/validation failures automatically keep LKG active.

## Batch transcoding (`transcode_batch`)

The `transcode_batch` action coordinates persistent batch transcoding across library series (supporting Sonarr). It resolves media files, inspects media streams, applies deterministic auto-profile selection, limits concurrency to `config.Transcode.MaxParallelJobs`, and tracks execution per item in a dedicated SQLite table (`transcode_batch_items`).

### Action inputs & defaults

| Input | Type | Required | Default | Description |
|---|---|---|---|---|
| `service` | string | Yes | — | Media service name (must be `sonarr`). |
| `series_id` | string/int | Yes | — | Sonarr series ID to transcode. |
| `season` | int | No | `nil` (all) | Optional season number filter. Omit to transcode the entire series. |
| `profile` | string | No | `auto` | Recipe profile name or `auto` for deterministic stream-based selection. If omitted in `transcode_media`, honors configured `DefaultProfile` (including `auto`), else falls back to legacy `hevc-vt`. |
| `replace_original` | bool | No | `false` | Must remain `false`. Setting `true` is rejected fail-closed; original files are never overwritten. |
| `dry_run` | bool | No | `false` | If `true`, inspects and selects profiles without queuing or running transcode jobs. |
| `media_type` | string | No | derived | Media type override (e.g. `anime`, `tv`). Defaults to Sonarr metadata classification. |
| `is_anime` | bool | No | derived | Explicit anime flag. If omitted, detected automatically from Sonarr series metadata (`seriesType` or genres). |
| `min_savings_percent` | float | No | `config` | Minimum projected file size savings threshold (default from `config.Transcode.MinSavingsPercent`). |
| `max_size_increase_percent` | float | No | `0.0` | Allowed candidate size increase percentage (default `0.0`). Any candidate exceeding this triggers `waiting_decision`. |
| `surface_worker_busy` | bool | No | `true` | When `true`, worker capacity saturation surfaces `waiting_for_slot` without failing or burning retry budgets. |
| `max_items` / `max_output_items` | int | No | `25` | Deterministic bound on returned item summaries in outputs (default 25, capped at max 100). |

> [!IMPORTANT]
> `idempotency_key` is a **top-level** MCP argument to `action_run`, NOT nested within the `inputs` JSON object.
> Furthermore, `inputs` must always be supplied as a serialized JSON object string (e.g. `"{\"service\":\"sonarr\",...}"`).

### Action invocation via `action_run`

`transcode_batch` is executed via the `action_run` MCP tool:

#### 1. Season dry-run example

Probes all episode files in Season 1, evaluates each stream against deterministic auto-profile rules, and returns aggregate counts and bounded summaries without running transcodes or mutating files:

```json
{
  "action": "transcode_batch",
  "inputs": "{\"service\":\"sonarr\",\"series_id\":10,\"season\":1,\"profile\":\"auto\",\"dry_run\":true}",
  "idempotency_key": "batch-sonarr-10-s1"
}
```

#### 2. Real candidate-only batch example

Processes Season 2 files, skipping items that are already HEVC or do not meet savings thresholds, and generates standalone `.candidate.<ext>` files for qualifying items with serial concurrency and worker slot management:

```json
{
  "action": "transcode_batch",
  "inputs": "{\"service\":\"sonarr\",\"series_id\":10,\"season\":2,\"profile\":\"auto\",\"dry_run\":false,\"replace_original\":false,\"max_size_increase_percent\":0.0}",
  "idempotency_key": "batch-sonarr-10-s2"
}
```

#### 3. Single-item transcode example (`transcode_media`)

```json
{
  "action": "transcode_media",
  "inputs": "{\"path\":\"/media/tv/Series/S01E01.mkv\",\"profile\":\"auto\"}",
  "idempotency_key": "transcode-s01e01"
}
```
If `profile` is omitted, the engine honors `config.Transcode.DefaultProfile` (which can be set to `auto` or a specific profile), falling back to `hevc-vt` if unset.

> [!NOTE]
> `replace_original` defaults to `false` and must remain `false`. Any request specifying `replace_original: true` is rejected fail-closed to guarantee original library files are never touched or overwritten.

### Bounded summaries and truncation metadata

To prevent unbounded JSON responses when batching entire series or large seasons, action outputs provide aggregate counts plus a bounded list of per-item summaries (default 25, capped at max 100):

```json
{
  "batch_id": "act-transcode_batch-123456",
  "series_title": "Kaguya-sama: Love Is War",
  "is_anime": true,
  "dry_run": true,
  "counts": {
    "total": 24,
    "queued": 0,
    "transcode": 18,
    "skip": 6,
    "review": 0,
    "waiting_for_slot": 0,
    "waiting_decision": 0,
    "running": 0,
    "completed": 0,
    "failed": 0
  },
  "total_items": 24,
  "returned_items": 24,
  "truncated": false,
  "items_limit": 25,
  "items": [
    {
      "item_key": "epfile-101",
      "file_path": "/media/tv/Kaguya S01E01.mkv",
      "display_label": "Kaguya-sama: Love Is War - S01E01",
      "episode_info": "S01E01",
      "decision": "transcode",
      "profile": "anime-hevc",
      "reasons": ["video: h264 1080p, anime content, high bitrate (8.5 Mbps)"],
      "status": "queued",
      "attempts": 0,
      "child_action_id": "",
      "job_id": ""
    }
  ]
}
```

- **Deterministic bounding**: Returns up to `items_limit` (default: 25, configurable via `max_items` or `max_output_items`, strictly capped at 100).
- **Truncation metadata**: `total_items`, `returned_items`, `truncated` (`true` when more items exist), and `items_limit`.
- **Full persistence**: All items are persistently recorded in the SQLite `transcode_batch_items` table regardless of output truncation.
- **Traceability**: Active items record both the `child_action_id` and the worker `job_id` returned by the transcode executor.
- **Per-item diagnostics**: Explanatory `reasons`, runtime `error`, and retry `attempts` remain available on each item summary.

### Persistence, recovery, and resume behavior

`transcode_batch` tracks every unique media file in the SQLite `transcode_batch_items` table:
- **Crash and daemon restart resilience**: If the Navigatorr daemon stops or restarts mid-batch, calling `Resume(ctx, instanceID, "", nil)` re-attaches to the batch. Items are recovered via stable child idempotency lookup (`batch-<batch_id>-<item_key>`) across all statuses, preventing duplicate job submissions.
- **Concurrency & slot occupancy**: Active in-flight items occupy slots up to `config.Transcode.MaxParallelJobs` across resumes.
- **Decision forwarding**: When a child item requires user decision (e.g. `max_size_increase_percent` exceeded), the batch surfaces `waiting_decision`. Calling `action_resume` with `decision: "accept_loss"` or `"reject"` automatically forwards the decision to the child action.
- **Safe cancellation semantics**: Calling `action_resume` with `decision: "cancel"` marks queued and waiting items as cancelled. Active remote transcode jobs already in flight on workers are **not** stopped and remain running on workers. The batch surfaces this explicitly rather than falsely claiming remote jobs were terminated.
- **Pause/resume semantics**: Passing `paused: true` or resuming with `decision: "pause"` transitions the batch to `waiting_decision`. Resuming with `decision: "resume"` cleanly continues remaining items.

### Concurrency and `worker_busy` behavior

- **Parallelism**: Respects `config.Transcode.MaxParallelJobs` (default: 1). When `max_parallel_jobs: 1`, media files are transcoded serially one after another.
- **Worker busy handling**: If the remote transcode worker returns `worker_busy` (or reaches max parallel slots), the current item transitions to `waiting_for_slot`. Normal background transcodes remain in `running`.
- **Retry budget safety**: Encountering `worker_busy` does **not** increment item `attempts` or consume the transient retry budget. The batch transitions to `waiting_external` with `waiting_condition: "worker_busy"` and resumes cleanly once worker capacity becomes available.


