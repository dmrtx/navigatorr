# Transcoding architecture

Navigatorr treats transcoding policy as data, not as worker code.

## Reading results and recovery

Use `action_status` for the operational outcome and next step, including old
actions. `action_detail` exposes the persisted record without backfilling newer
derived summaries. For a goal (`mode`), `transcode.quality_ladder` (or
`size_budget`) lists the rungs, the one encoding or kept, and what each earlier
rung measured; see [TRANSCODE_AUTOMATIC.md](TRANSCODE_AUTOMATIC.md). Actions
that ran the removed sample search stay readable; their records keep their
original keys (`benchmark`, `benchmark_request`) and are not backfilled.

`recipe_get` exposes `compact_audio_policy` and managed `shadowed_profiles`.
Managed profiles are complete overrides: updates to a same-named bundle or
static profile do not flow into them. `fallback_audio_bitrate_bps` affects size
estimation only; it does not select an encoding bitrate. The reviewed Main10
profile is in [the example JSON](examples/anime-x265-medium-main10.json); updating
an existing managed recipe requires its current generation/digest via `recipe_save`.

Before publishing compact audio, the worker checks codecs, track count,
languages, channels and dispositions. It also streams audio packet metadata from
the staged source and candidate to check each track's duration and original A/V
offset (250 ms tolerance for priming/frame rounding). This reads both files once
more with bounded memory; it does not trust copied Matroska duration/bitrate tags.
Converted tracks longer than ten seconds also reject gross bitrate overshoot
(above twice the nominal target plus 16 kb/s). AAC may legitimately use fewer
bits for silence, so no lower bitrate floor is imposed. These checks detect
structural/timing failures, not audible transparency or every packet discontinuity.

Promotion summaries distinguish `recovery_verified_before_replacement` from
`recovery_cleanup_completed`. `recovery_verified` remains the legacy retained-copy
flag and becomes false after successful cleanup. For current physical backup
presence, use `transcode_backups`; workflow history is not a filesystem inventory.

`diagnostics.database` reports exact `actions_by_status` and
`maintenance_by_status`, plus `running_actions` and `non_terminal_actions`.
The legacy `active_actions` alias still means running only. Database failures
return unavailable counts and degraded health instead of reporting zero jobs.

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
  executor: http
  default_profile: hevc-vt-balanced

  recipes:
    source: file
    path: /config/transcode-recipes.yaml
    cache_dir: /config/cache/transcode-recipes

  http:
    base_url: http://192.0.2.10:8097
    token_file: /run/secrets/navigatorr_transcode_token
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

All structural/media/policy validation (duration, video codec, expected bit depth and pixel format, audio stream count/codecs/languages/channels, subtitle count and expected copy/conversion codecs, subtitle languages and `forced`/`default` dispositions, attachments, and chapters) runs worker-local on the candidate BEFORE it is published. A bad candidate fails the job closed and is never published; the original is never deleted or overwritten. Promotion of an already-published candidate intentionally performs a full re-inspection.

### Input validation before anything is persisted

`transcode_media`, `transcode_batch` and `clean_podcast_ads` check what the caller sent before an action row is written, before the idempotency key is looked up, and before the worker is asked anything. The checks are the same ones preflight runs (unknown or missing inputs, input combinations such as `profile` with `profile_config` or `max_height` without `mode: "compact"`, a strict `profile_config` decode including the x265 settings, inputs that only tuned the removed sample search (`metric`, `priority`, `shared_calibration`, `calibration_items`, `batch_fixed_quality`, optimization profiles, `benchmark_transcode`), a podcast `output_path`, batch path shapes, mode and size-budget rules), so a mistake is reported in milliseconds instead of after a multi-gigabyte source hash.

A rejected request:

- returns an `invalid_params` error from `action_run` (a JSON body with `error`, `field` and `reason`), or HTTP 400 with `code: "invalid_params"` and the same `field` from the maintenance UI;
- writes no action row, takes no hash slot and makes no worker call, including the admission check;
- does not reserve the idempotency key, so the corrected request reuses it.

Replays are not judged again: a key that already holds an action with identical inputs returns that stored action even if the configuration has changed since (for example the read roots were narrowed or `allow_destructive` was turned off), so a lost response stays recoverable. Only a submission that would create a new action, or that differs from the stored one, is validated. Batch children already persisted are resumed or returned as they are.

The expected output bit depth is computed during validation and carried into preflight, where checking it against the probed source is a single comparison. Preflight still re-runs the same validators defensively for rows created by older versions. The catalog's required and optional inputs are the single list the unknown-key and required-input checks read, so the two cannot drift.

### Transcode I/O optimization (worker-local validation + shared source cache)

Data flow:

```text
NAS source --(one local cache copy, local bytes hashed vs expected SHA-256)--> shared source cache
shared cache --(job-local hard link / lease)--> full encode and final quality check locally
local candidate --(full structural/media/policy validation locally, fail closed)--> publish once to NAS
published object --(lightweight identity: regular/non-empty + exact size; SMB uses its verified publish)--> accept
```

Coordinator post-publish validation is deliberately lightweight and performs **no** ffprobe/mediainspect and **no** NAS candidate read. The worker computes the accepted candidate's size and SHA-256 during local validation and attests them in `job.json`/status; the coordinator compares the published object's size against that attestation (stat only), enforces the cheap size guardrail, and relies on the worker's hash-verified exclusive publish for SMB-direct logical paths (which are never `stat`ed). Full promotion-time inspection of a published candidate still uses the detailed validator.

Safety invariants (unchanged):

- Full candidate validation runs worker-local BEFORE publish; a bad local
  candidate fails the job and its local file is removed without ever publishing.
- Post-publish verification is lightweight and independent: regular/non-empty
  existence and exact size match for filesystem destinations, plus the
  worker's own stat/size check; SMB-direct uses its existing hash-verified
  exclusive publish. Mismatches fail closed and stay resumable.
- Original-preservation, no-clobber (`O_EXCL` / exclusive rename / hard-link,
  never overwriting an existing destination), cancellation-wins,
  crash/resume (EncodeComplete checkpoint never re-encodes), idempotency
  (execution-spec digest + idempotency key), and promotion separation are
  preserved.

Source cache identity and cleanup:

- Key: `sha256(clean_source_path | size | mtime_nanos | mode | expected_sha256)`
  with a JSON sidecar (`source`, `size`, `mod_time_unix_nano`, `sha256`). The
  coordinator preflight SHA-256 of the source is forwarded to the
  transcode. A size, mtime, or **content** change is a
  miss, so a same path/size/mtime source with changed content can never be
  reused. Before an entry is published the worker hashes only the LOCAL
  copied/downloaded bytes and compares them to the expected digest; a mismatch
  is never published. No additional NAS read/hash is ever performed.
- Location: `<local_work_dir>/_source-cache/<key><ext>` plus `<key>.meta.json`,
  always on worker-local storage, never on NAS roots. Sweeps remove the correct
  `<key>.meta.json` sidecar (not `<key><ext>.meta.json`).
- Concurrency: atomic temp + no-clobber publish; concurrent populators race
  safely (one wins, losers revalidate the winner).
- Active-job safety: an in-flight transcode never reads the
  evictable shared filename directly. It acquires a JOB-LOCAL immutable hard
  link (safe atomic local-copy fallback if linking is unsupported) and runs
  against that. Eviction may unlink the shared name without breaking an active
  job; per-job cleanup removes only its own link/copy. This yields zero second
  NAS transfer for an encode and its final quality check of the same source.
- Bounds/cleanup: `source_cache_max_bytes` (default 20 GiB) and
  `source_cache_ttl_hours` (default 72h). Sweeps are best-effort, never fail a
  job, and skip recently-written entries. Per-job completion/cancel cleanup
  never deletes shared cache paths.
- `staging_policy` (`auto`/`never`/`always`) and SMB-direct/path-mapping
  semantics are preserved. The cache is consulted only when the source would
  otherwise require a NAS read (external path per policy, or SMB-direct
  mapping). Local sources are read in place and never cached.
  `disable_source_cache: true` restores legacy per-job staging exactly.

Source content identity is also part of **strong full-transcode idempotency**:
the canonical execution-spec digest includes the normalized preflight SHA-256
when available (the field is omitted when absent, so historical digests are
byte-for-byte unchanged). The HTTP executor forwards `source_sha256`
and compute the same hash-aware digest the worker recomputes from the persisted
record, so a changed source at the same path with the same candidate/profile/
plan is a deterministic idempotency conflict rather than a silent reuse.

> **Protocol v2 — server and worker must be upgraded together.** Adding
> `source_sha256` to the normal transcode submit and benchmark payloads is an
> incompatible wire-schema change: worker HTTP decoders use
> `DisallowUnknownFields`, so a v1 worker rejects the new payloads. The worker
> protocol version is therefore bumped to **2**, and a new coordinator rejects a
> stale v1 worker during the capability handshake (before any submit) instead of
> deferring the failure. There is no compatibility fallback and `source_sha256`
> is never silently omitted. Upgrade the Navigatorr server and every transcode
> worker in the same rollout.

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

Ansible: the worker binary does not require config changes (the new keys are
optional with safe defaults), but the cache settings are explicitly managed by
the deployment's configuration management (for example, an Ansible role)
rather than left to defaults. To opt out there, set
`disable_source_cache: true`.

## Sample evaluation cache (removed)

The worker used to keep a durable cache of per-(candidate, window) sample
evaluations under `<local_work_dir>/_sample-cache/` for its benchmark
endpoints, which are gone. Startup removes that directory and any `bench-*`
sample test job directories an older build left in the state directory, and
logs one line when it does. The `disable_sample_cache`, `sample_cache_max_bytes`
and `sample_cache_ttl_hours` keys are ignored if a config still carries them.

The final quality check is never cached: it validates the artifact produced in
that run.

## Optimization profiles (removed)

A profile with `optimization.enabled: true` (schema_version 2) used to probe a
set of encoder configurations on source samples before the full encode. That
sample search was removed: new requests with such a profile (including
`profile_config.optimization.enabled`), `benchmark_transcode` and the inputs that
only tuned it are refused with an input error, and goals (`mode`) judge each
whole-file encode with the worker's final quality gate instead (see
[TRANSCODE_AUTOMATIC.md](TRANSCODE_AUTOMATIC.md)). Profiles without
`optimization` follow the fixed-profile flow unchanged: the recipe resolves to
one concrete plan and the full file is encoded with it. Recipes select exactly
one encoder family (`hevc_videotoolbox` or `libx265`) plus only typed knobs valid
for that family; cross-family knobs (VideoToolbox rate/offline switches on
`libx265`, `preset`/CRF semantics on VideoToolbox) fail recipe validation.

### Speed priority

Speed is expressed per profile through the typed VideoToolbox switches
documented in `docs/transcode/VIDEOTOOLBOX_PROFILES.md` (`prioritize_speed`,
`spatial_aq`, `realtime` with pointer-boolean semantics: omitted means "emit
nothing", explicit values are required capabilities and fail closed when
unsupported).

### Capabilities

Before submitting, the coordinator fetches versioned `WorkerCapabilities`
(protocol version, Navigatorr/worker build, FFmpeg version, encoders,
`libvmaf`/`ssim` filters, per-encoder profiles/pixel formats/options, structured
probe errors, deterministic `CapabilityFingerprint`). Missing capabilities fail
the job with a classifiable error before any encode starts.

### Troubleshooting

| Symptom | Meaning | Action |
|---|---|---|
| `worker_busy` / `waiting_for_slot` | All worker slots occupied; nothing was sent to run | Wait, or cancel; the job resubmits by itself after the worker's `Retry-After` (plus one second), for as long as it waits, without using its retry budget |
| Goal ended with reason `quality_not_met` | Every rung missed the goal's quality limits | Keep the original, or choose a goal with lower limits; the engine will not guess |
| `hdr_requires_compact` | HDR/Dolby Vision source under a goal other than compact | Choose compact, or keep the original; do not force SDR limits onto HDR |
| `encoder_capability_unsupported` | Recipe asks for an option the worker FFmpeg lacks | Change the recipe (typed fields only), bump `bundle_version`, `recipe_reload` |
| `replace_original: true` rejected | Candidate-only invariant | Keep `false`; originals are hashed before/after and never overwritten |

## Rollout

A safe rollout is:

1. Deploy the new Navigatorr and worker once so the capability engine understands versioned plans.
2. Start with `source: builtin` and verify `recipe_status`.
3. Move policy to `source: file` or a versioned GitHub/HTTPS source.
4. For future policy/compatibility changes, publish a new recipe bundle version and call `recipe_update`/`recipe_reload` instead of rebuilding the image.
5. If a recipe causes trouble, use `recipe_rollback`; fetch/validation failures automatically keep LKG active.

## Batch transcoding (`transcode_batch`)

The `transcode_batch` action coordinates persistent batch transcoding across library series (supporting Sonarr). It resolves media files, inspects media streams, applies deterministic auto-profile selection, limits concurrency to the worker's video pool (capped by `transcode.max_parallel_jobs` when that is positive), and tracks execution per item in a dedicated SQLite table (`transcode_batch_items`).

### Simple series workflow

Supply `service: "sonarr"`, `series_id` and a goal: `mode` is `size`, `quality`,
`x265_preserve` or `compact` (see [TRANSCODE_AUTOMATIC.md](TRANSCODE_AUTOMATIC.md)).
The batch freezes one policy and each episode is encoded and judged on its own
with the goal's fixed ladder of whole-file encodes; there is no calibration pass
and no quality shared between episodes. A file that passes no rung keeps its
original and does not block the others. Each final candidate is still validated
by the worker's quality gate and actual size savings.

`dry_run: true` previews eligibility without encoding. `batch.selection` explains
whether each file is eligible, skipped or unsuitable. `batch.next_step` gives
status-specific continuation guidance. A completed preview requires a new
`action_run` with `dry_run: false`; resuming it cannot change its immutable
inputs.

Sources the goal cannot judge keep their originals with a skip/review reason
(for example `hdr_requires_compact`, `hfr_quality_unavailable`,
`already_target_codec`). Use `promote_candidates: true` to request one approval
after candidates are ready; without it, the action only creates candidates. No
worker protocol change or recipe catalog update is required.

For direct encoding, choose a non-optimizing profile or supply a `profile_config`
with optimization disabled, and omit `mode`. This uses ordinary file/stream
validation without a VMAF/CAMBI check. A profile with optimization enabled is
refused. For example:

```json
{"action":"transcode_batch","inputs":"{\"service\":\"sonarr\",\"series_id\":10,\"profile\":\"general-hevc\"}"}
```

For a goal:

```json
{"action":"transcode_batch","inputs":"{\"service\":\"sonarr\",\"series_id\":10,\"mode\":\"size\"}"}
```

### Action inputs & defaults

| Input | Type | Required | Default | Description |
|---|---|---|---|---|
| `service` | string | Yes | — | Media service name (must be `sonarr`). |
| `series_id` | string/int | Yes | — | Sonarr series ID to transcode. |
| `season` | int | No | `nil` (all) | Optional season number filter. Omit to transcode the entire series. |
| `episode_file_ids` | array of ints | No | all eligible files | Select up to 1000 positive episode-file IDs belonging to this series. Intersects with `season`; duplicate/shared episode files are encoded once. Empty arrays and foreign IDs are rejected. |
| `mode` | string | No | — | Goal: `size`, `quality`, `x265_preserve` or `compact`. With a goal, omit `profile`; each file runs the goal's ladder. Goal-specific inputs (`max_height`, `allow_hardware`, `allow_slow_preset`, `size_budget_*`) are in [TRANSCODE_AUTOMATIC.md](TRANSCODE_AUTOMATIC.md). |
| `profile` | string | No | `auto` | Recipe profile name for a fixed encode, without `mode`. A profile with optimization enabled (and `auto` when it can pick one) is refused: choose `mode` instead. If omitted in `transcode_media`, honors configured `DefaultProfile`, else falls back to legacy `hevc-vt`. |
| `replace_original` | bool | No | `false` | Must remain `false`. Setting `true` is rejected fail-closed; original files are never overwritten. |
| `dry_run` | bool | No | `false` | If `true`, inspects and selects profiles without queuing or running transcode jobs. |
| `media_type` | string | No | derived | Media type override (e.g. `anime`, `tv`). Defaults to Sonarr metadata classification. |
| `is_anime` | bool | No | derived | Explicit anime flag. If omitted, detected automatically from Sonarr series metadata (`seriesType` or genres). |
| `min_savings_percent` | float | No | `config` | Minimum projected file size savings threshold (default from `config.Transcode.MinSavingsPercent`). |
| `max_size_increase_percent` | float | No | `0.0` | Allowed candidate size increase percentage (default `0.0`). Any candidate exceeding this triggers `waiting_decision`. |
| `max_items` | int | No | unlimited | Deterministic bound on episode files prepared, persisted, and scheduled by the batch. |
| `max_output_items` | int | No | `25` | Bound on returned item summaries in outputs (default 25, capped at max 100). This does not affect scheduling. |
| `promote_candidates` | bool | No | `false` | After the batch finishes, offer one approval for every completed, verified candidate. Promotion uses Sonarr and recovery copies. |
| `promotion_parallelism` | int | No | `2` | Concurrent Sonarr promotions after batch approval, from 1 to 2. |

The inputs `metric`, `priority`, `shared_calibration`, `calibration_items` and
`batch_fixed_quality` only tuned the removed sample search and are refused with
an input error.

> [!IMPORTANT]
> `idempotency_key` is a **top-level** MCP argument to `action_run`, NOT nested within the `inputs` JSON object.
> Furthermore, `inputs` must always be supplied as a serialized JSON object string (e.g. `"{\"service\":\"sonarr\",...}"`).

### Action invocation via `action_run`

`transcode_batch` is executed via the `action_run` MCP tool:

#### 1. Season dry-run example

Probes all episode files in Season 1, evaluates each stream against deterministic eligibility rules, and returns aggregate counts and bounded summaries without running transcodes or mutating files:

```json
{
  "action": "transcode_batch",
  "inputs": "{\"service\":\"sonarr\",\"series_id\":10,\"season\":1,\"dry_run\":true}",
  "idempotency_key": "batch-sonarr-10-s1"
}
```

#### 2. Real candidate-only batch example

Processes Season 2 files, skipping items that are already HEVC or do not meet savings thresholds, and generates standalone `.candidate.<ext>` files for qualifying items with serial concurrency and worker slot management:

```json
{
  "action": "transcode_batch",
  "inputs": "{\"service\":\"sonarr\",\"series_id\":10,\"season\":2,\"mode\":\"size\"}",
  "idempotency_key": "batch-sonarr-10-s2"
}
```

#### 3. Single-item transcode example (`transcode_media`)

```json
{
  "action": "transcode_media",
  "inputs": "{\"path\":\"/media/tv/Series/S01E01.mkv\",\"mode\":\"size\"}",
  "idempotency_key": "transcode-s01e01"
}
```
Without `mode`, if `profile` is omitted the engine honors `config.Transcode.DefaultProfile` (a fixed profile; one with optimization enabled is refused), falling back to `hevc-vt` if unset.

#### Series with one promotion decision

Use one `transcode_batch` action for the series. Each episode runs the goal's
ladder on its own; an episode that passes no rung keeps its original and does not
block the other episodes, and if none passes the batch keeps all originals and
completes without candidates. A rejected candidate leaves its original intact
while other episodes continue. Technical failures remain errors, rather than
being reported as quality skips. Previously created batches keep the policy they
were frozen with.

```json
{
  "action": "transcode_batch",
  "inputs": "{\"service\":\"sonarr\",\"series_id\":10,\"mode\":\"size\",\"promote_candidates\":true}",
  "idempotency_key": "series-10-size"
}
```

The action runs asynchronously and survives restarts. After all candidate jobs finish, `promote_candidates: true` presents the exact list once as `waiting_decision`. Resume the **batch** with `decision: "approve"` to promote those candidates; no per-episode approvals are needed. `reject` leaves originals and candidates in place. Promotion still verifies each physical file, keeps a recovery copy until Sonarr confirms its replacement, and runs at most two files concurrently. Omit `promote_candidates` to keep every original and only create validated candidates.

> [!NOTE]
> `replace_original` defaults to `false` and must remain `false`. Any request specifying `replace_original: true` is rejected fail-closed to guarantee original library files are never touched or overwritten.

### Bounded summaries and truncation metadata

To prevent unbounded JSON responses when batching entire series or large seasons, action outputs provide aggregate counts plus a bounded list of per-item summaries (default 25, capped at max 100):

```json
{
  "batch_id": "act-transcode_batch-123456",
  "series_title": "Moriko-sama: Tide Is Rising",
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
      "file_path": "/media/tv/Moriko S01E01.mkv",
      "display_label": "Moriko-sama: Tide Is Rising - S01E01",
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

- **Deterministic batch limit**: `max_items` limits the sorted episode-file set before inspection, persistence, or scheduling.
- **Deterministic output bounding**: Returns up to `items_limit` summaries (default: 25, configurable via `max_output_items`, strictly capped at 100).
- **Truncation metadata**: `total_items`, `returned_items`, `truncated` (`true` when more items exist), and `items_limit`.
- **Full persistence**: All items are persistently recorded in the SQLite `transcode_batch_items` table regardless of output truncation.
- **Traceability**: Active items record both the `child_action_id` and the worker `job_id` returned by the transcode executor.
- **Per-item diagnostics**: Explanatory `reasons`, runtime `error`, and retry `attempts` remain available on each item summary.

### Persistence, recovery, and resume behavior

`transcode_batch` tracks every unique media file in the SQLite `transcode_batch_items` table:
- **Crash and daemon restart resilience**: If the Navigatorr daemon stops or restarts mid-batch, calling `Resume(ctx, instanceID, "", nil)` re-attaches to the batch. Items are recovered via stable child idempotency lookup (`batch-<batch_id>-<item_key>`) across all statuses, preventing duplicate job submissions.
- **Concurrency & slot occupancy**: Active in-flight items occupy slots up to the batch fan-out (see Parallelism) across resumes.
- **Decision forwarding**: When a child item requires user decision (e.g. `max_size_increase_percent` exceeded), the batch surfaces `waiting_decision`. Calling `action_resume` with `decision: "accept_loss"` or `"reject"` automatically forwards the decision to the child action.
- **Safe cancellation semantics**: Calling `action_resume` with `decision: "cancel"` records cancellation before new work can start, cancels active child actions and their worker jobs, and records any cancellation errors in the batch result.
- **Pause/resume semantics**: Passing `paused: true` or resuming with `decision: "pause"` transitions the batch to `waiting_decision`. Resuming with `decision: "resume"` cleanly continues remaining items.

### Concurrency and `worker_busy` behavior

- **Parallelism**: A batch keeps as many children in flight as the worker's video pool: its [parallel jobs](#parallel-jobs-on-a-node), or its video slots when the pool is split. The server reads that from `GET /v1/slots` (cached for 30 seconds; an unreachable worker keeps the last value it reported). The server's `transcode.max_parallel_jobs` is an optional cap on top: `0` follows the worker, a positive value caps each batch at that many. A worker that cannot report its pool falls back to the cap, or one at a time without one. When the result is 1, media files are transcoded serially one after another.
- **Metric threads**: The worker's `metric_threads` sets libvmaf `n_threads` for each VMAF and CAMBI pass. The default `0` divides the host CPUs among simultaneous metric passes (`metric_concurrency` × the video jobs running at that moment; a lone video job gets every CPU, and with a [video/podcast split](#video-and-podcast-slots) a busy podcast slot does not count). With an explicit value, final validation runs as many sample windows at once as fit that many threads each into the job's share of the CPUs (host CPUs ÷ the video jobs running), and one at a time when the value is below 2. Thread count changes wall time only; per-frame scores are identical.
- **x265 thread pools**: The worker's `x265_pools` sets the libx265 thread-pool size of each software encode (both passes of a two-pass encode and the full encode). The default `0` splits the host CPUs evenly across the video jobs running together, sized when each encode starts (a video job alone keeps x265's default pool and so uses every CPU; a second one that starts later gets half the host while the first keeps the pool it began with); `-1` turns partitioning off; a positive value sets the pool size, but one below 4 or not below the host CPU count is ignored (encodes run unpartitioned) and config load logs a warning saying so. x265 derives its frame-thread count from the pool size, and frame threads change the bitstream, so the worker also pins `frame-threads` to the count x265 picks by itself for the whole host. Pools below 4 threads are never used (they also change the output), nor is partitioning applied on hosts with 32 or more CPUs, where x265's own choice depends on the source height. The encoded frames are identical to an unpartitioned encode; only the informational SEI that records the encoder options differs. The worker proves this on every `serve` start: in the background (readiness never waits) it encodes a two-second synthetic 1080p Main10 clip, CRF and two-pass ABR, with and without every partition the current slot limits can produce (one per count of video jobs running together, so a ceiling of 3 on a 12-CPU host proves pools of 6 and 4) and compares the decoded frames (`-f framemd5`). Saving a new slot split proves its pools in the background when no stored verdict covers them. A difference turns the partition off and logs why; the outcome is reported as `x265_partition` in `/v1/health` (`status`, `active`, `detail`) and stored in the state directory so spawned job processes honour it. A check that cannot run (no libx265, cancelled, out of time) is `inconclusive` and changes nothing; `x265_pools: -1` skips it.
- <a id="parallel-jobs-on-a-node"></a>**Parallel jobs on a node**: the worker's ceiling is how many jobs it runs at once. Its config `max_parallel_jobs` is the boot default; the maintenance UI (Workers, Worker activity, *Parallel jobs on this node*) overrides it at runtime, from 1 to the worker's limit: `max_parallel_jobs_limit` in the worker config, else the host CPU count (never below the config value). The override is pushed with the split (`PUT /v1/slots` with `"ceiling":N`; absent or `0` keeps the current ceiling, and a value equal to the config clears the override) and kept in the same `worker-slots.json`, so it survives restarts. `GET /v1/slots` reports `ceiling` (in force), `ceiling_limit` and `ceiling_source` (`config` or `override`). Lowering it never interrupts running jobs: nothing new is admitted until use drains below the new value. Raising it takes effect at the next admission. x265 pools, `/v1/ready` `max_parallel_jobs`, `/v1/activity` `slots_total` and batch fan-out all follow the value in force. If a lower ceiling no longer fits the video + podcast split, saving it returns the split to one shared pool and says so (a split saved alone that does not fit is still refused). x265 jobs share the CPUs, so two at once are not faster in total; VideoToolbox encodes and quality checks overlap well. A worker that predates the override reports no `ceiling_limit`: the UI shows its value read-only and asks for a worker update, and the server never sends it a `ceiling` (it would refuse the unknown field).
- <a id="video-and-podcast-slots"></a>**Video and podcast slots**: the worker's [parallel jobs](#parallel-jobs-on-a-node) are its hard ceiling: every job kind shares those slots. From the maintenance UI (Workers, Worker activity, *Slot split*) the operator can divide them into `video_slots` (transcode and final quality check) and `podcast_slots` so the two stop competing; a lone video job still gets the free CPU because encode and metric threads follow the video jobs actually running, not the ceiling. The split is stored on the server per worker node and pushed to the worker (`PUT /v1/slots`, bearer-authenticated, `{"video_slots":N,"podcast_slots":M}`; `GET /v1/slots` reads it; `0`/`0` clears it). It needs no redeploy or restart: the worker keeps it in its state directory (`worker-slots.json`) and applies it at its next admission, so running jobs are never interrupted and a pool that now exceeds its share simply drains. Rules: both values at least 1, their sum at most the worker's ceiling (so the ceiling stays the cap), video slots at most the host CPU count; the UI warns when `CPUs ÷ video slots` is below 4, because x265 then cannot be partitioned frame-identically and encodes run unpartitioned (oversubscribing the host). Admission is enforced on the worker for each kind against its own pool (queue drain and post-encode resume); the server's slot queue only ever holds video jobs, because podcast jobs queue on the worker and never take a server ticket. A job refused for a full pool reports the kind ("all N video slots are in use"). The server re-pushes the stored split when it sees a worker report something else (restart with a wiped state directory, or a save made while it was unreachable), so a save is never lost (the stored ceiling is re-pushed the same way, only to workers that report `ceiling_limit`). Raise the parallel jobs first if the sum of the two pools should exceed them. `/v1/ready`, `/v1/activity` and the Worker activity panel report each pool's total and in-use count.
- **Worker busy handling**: When every worker slot is in use, the worker refuses a new job with `worker_busy` and creates nothing. The refused job waits in `waiting_external` with `waiting_condition: "worker_busy"`; in a batch, its item shows as `waiting_for_slot` while the files that already have a slot keep `running`.
- **Retry budget safety**: Waiting for a slot does **not** increment item `attempts` or consume the transient retry budget, and it has no deadline: it lasts until a slot frees up or the action is cancelled. The next submit is sent no earlier than the worker's `Retry-After` plus one second, with the same job identity and idempotency key. A long wait shows as one `waiting_external` row in the step log, not one per poll; each refused resend is counted on the job's single `worker_busy` entry in `failure_history` (`count`, `last_at`) instead.
- **Single files wait too**: A standalone `transcode_media` waits the same way ("Waiting for a free worker slot") instead of failing, whatever its retry budget. `surface_worker_busy` is still accepted on child inputs but no longer changes this behaviour.
- **Recipe settings**: A recipe's `fallbacks` entry `when: worker_busy` and a `worker_busy` entry in `retry_on` no longer change how busy submit refusals are handled: those always wait for a slot. Both are still accepted, so existing recipes keep validating.

## Compact audio without another optimizer

Existing recipes and direct encoding preserve audio (`audio: {mode: copy}`).
An opt-in recipe can set `audio: {mode: compact}` while using either existing
video encoder. This does not expand automatic profile selection to HEVC.
For a HEVC Main10 source, use an explicit recipe or `profile_config`.

Compact mode keeps all tracks, languages and channel counts. DTS, TrueHD/MLP,
FLAC, ALAC and PCM become AAC: 128 kbps mono, 192 kbps stereo, 384 kbps up to
5.1, 512 kbps up to 7.1. AAC/AC3/EAC3/Opus and other codecs are copied, as are
tracks with unknown channel count or more than eight channels. This is lossy
audio conversion; lossless quality and object-audio metadata are not preserved.
Use copy when those are required. There is no audio parameter search.

Size estimates include the selected audio policy (AAC estimates are
nominal, not measured guarantees). The final encode checks audio codecs, track
counts, languages and channels. VMAF/CAMBI validate video, not perceived audio
quality. The original remains protected by the existing candidate/promotion
workflow. Deploy the worker and server together before opting in.
