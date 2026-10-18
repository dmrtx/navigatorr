# Perceptual quality validation

Navigatorr can measure a selected VMAF v1 model and an independent CAMBI
full-reference banding metric during benchmarking. The final encoded candidate
is checked again on the same deterministic source windows before publication.
Native SDR Main10 can use VMAF/CAMBI when the worker advertises
`quality.native_10_bit`: both metrics must execute on a native 10-bit synthetic
pattern with automatic pixel conversion disabled. Older/unverified workers
retain the VMAF/CAMBI gate and the existing SSIM fallback for `metric: auto`.
This applies to both libx265 and VideoToolbox; libx265 receives yuv420p10le.
Main10 benchmarks also run this capability check when `vmaf.model` is omitted
(legacy/default VMAF, including combined VMAF/SSIM). Omitting the model does not
change the scoring policy or bypass native-depth verification. Main10 benchmark
VMAF commands disable automatic pixel conversion, so incompatible inputs fail
instead of silently measuring an 8-bit conversion.

## Opt in

Set these fields under an optimization profile's `quality` policy:

```yaml
quality:
  preferred_metric: vmaf
  vmaf:
    model: v1_1080p_3h
    target: 96
    minimum: 95
    marginal_tolerance: 0.5
    guardrail_enforcement: observe
    p5_minimum: 93
    worst_window_minimum: 92
    worst_window_seconds: 5
    frame_threshold: 95
    max_frames_below_threshold: 50
  banding:
    enabled: true
    metric: cambi
    mode: full_ref
    enforcement: observe
  final_validation:
    mode: sampled
```

`v1_1080p_3h` selects `vmaf_v1.0.16_3d0h`, with a 0–100 score range and
10-bit *measurement* precision. The candidate's encoded bit depth does not
change. The worker executes a synthetic probe before claiming support for the
model or CAMBI. A worker that exposes `libvmaf` but cannot initialize the model
is reported as unsupported. Explicit VMAF requests fail closed; an explicit
`metric: auto` may choose SSIM when the model is unavailable or the source is
unverified native 10-bit or HFR on a worker without the decimation capability.

### Frame pairing

Every VMAF and CAMBI graph restamps both inputs by frame index
(`settb=AVTB,setpts=N`) before libvmaf. libvmaf pairs frames by timestamp, and
lossless sample extraction rounds both samples to a 1 ms timebase; at 59.94 FPS
a source and candidate frame can land 1 ms apart and would otherwise be paired
with a neighbour. The final check still requires equal frame counts and
relative timestamps within a quarter frame before it measures.

### High-frame-rate sources (45–61 FPS)

The VMAF models were not trained on 50/59.94/60 FPS motion. Sources from 45 up
to 61 FPS are therefore measured on a frame-exact 2:1 temporal decimation: both
reference and candidate keep frame indices 0, 2, 4, ... (`select=not(mod(n\,2))`)
and are then restamped by index, so every pair stays aligned and the model sees
25/29.97/30 FPS motion. Sources above 61 FPS, with missing timing, or
interlaced (`field_order` tt, bb, tb or bt) in the HFR class still fail
closed: an interlaced stream may report its field rate, and dropping every
second picture would keep one field parity. VMAF evidence records
`temporal_decimation` and `measured_fps`; window lengths keep their configured
duration in seconds, and frame locations map back to the source timeline. A
`max_frames_below_threshold` limit is compared with the measured count
multiplied by the decimation factor, so it keeps its meaning in source time.
CAMBI is a purely spatial per-frame metric and keeps every frame: decimating
it would only skip half of the frames its peak guardrail inspects.

A worker advertises `quality.hfr_temporal_decimation: 2` only after running
the production decimated model graph (including `cambi.enc_bitdepth`) on a
synthetic, temporally varying 48 FPS pair: the aligned pair must score near
the top and a candidate one source frame late must score clearly lower, so the
probe proves index alignment and not only the frame count. The probe has its
own timeout; a slow probe withholds HFR only, never native-rate VMAF/CAMBI.
Without the capability the worker reports `quality.hfr_probe_reason`, and a
timed-out probe is retried on the next capability lookup.

Coordinators treat HFR as unavailable on workers without the capability, so a
coordinator never dispatches HFR work a worker would reject. Automatic modes
skip such sources with `hfr_quality_unavailable` (distinct from
`unsupported_source`: a capable worker clears it, a new goal does not). A
transient capability lookup failure waits for the worker instead of skipping.
Explicit benchmarks and recipe searches with a VMAF model follow the same
capability gate. Deploy the worker before relying on HFR support. Shared
batch calibration still requires sources below 45 FPS.

Decimation reuses the non-HFR model's calibration; it is not an HFR-trained
model. Scores for HFR sources are comparable with 25/30 FPS content, not with
an HFR model, and thresholds were not separately calibrated for HFR libraries.

`guardrail_enforcement: observe` records p5, the worst sample-local window,
and frame counts without changing candidate selection. `reject` makes a
candidate ineligible and adaptive search tries the next allowed candidate.
CAMBI uses a separate bounded pass because VMAF v1 already registers its own
CAMBI feature. Its full-reference output measures additional banding relative
to the source; a lower value is better. CAMBI has no default rejection limit.
Set `max_mean` and/or `max_peak` only after calibrating with real library
encodes, then set `enforcement: reject` if desired.

## Calibrate before enforcing

1. Run representative 8-bit SDR titles with both guardrails in `observe`.
   Include flat gradients, dark scenes, animation, grain, and clean live action.
2. Collect `benchmark_quality` and the final job's `quality_evidence` with
   the source title, candidate setting, and a human review of the sampled
   scenes. Retain the model, libvmaf version, worker fingerprint, and window
   locations alongside every measurement.
3. Compare p5, worst-window mean, frame counts, CAMBI mean/peak, and the
   existing mean VMAF decision for accepted and visibly degraded encodes.
   Choose limits from these measurements; a universal CAMBI cutoff is not
   assumed.
4. Enable `reject` on a small canary profile. Inspect rejected candidates and
   final-validation failures before widening it. A failed final check leaves
   the original untouched and never publishes the local candidate.

No real-library threshold is embedded in the software. A worker only reports
the model as available after its executable probe passes.

The final check extracts each planned source and candidate window losslessly,
checks frame counts and relative timestamps, and stores bounded evidence
with the candidate SHA-256, size, plan digest, model, metric version, worker
capability fingerprint, and verdict. Window locations use the source's nominal
FPS and are marked `nominal_fps`; they are not claimed to be frame-accurate
timestamps for variable-frame-rate material. Raw per-frame logs remain local
to worker scratch and are cleaned up after validation.

The worker benchmark status exposes per-candidate `quality`, and the final job
status exposes `quality_evidence`. Action results carry these as
`benchmark_quality` and `quality_evidence`; large candidate sets remain
available through action detail when the compact MCP response omits them.

The current implementation supports the standard 1080p 3H VMAF v1 model.
Other VMAF v1 models, including HFR-trained and 4K ones, require registry entries and
their own executable capability probes. Full-file validation is intentionally
not offered because per-frame JSON logs are bounded.

## Finish a search instead of chaining rounds

`action_status` and `action_run` include a compact `benchmark`
summary with its outcome, next step, rejected checks, configured limits where
available, and a source timestamp to review. Progress refers to sample testing;
`completed` means the benchmark finished, not that a candidate passed. A VMAF
pass means the configured sampled checks passed, not guaranteed perceptual
transparency. Full-reference CAMBI measures additional banding relative to the
source, so existing source banding alone does not explain a rejection.

Two completed no-winner rounds for the same source SHA-256 trigger a review
before any further new benchmark admission. This uses durable action history
across recipes, paths, restarts and new action IDs. Technical execution failures
and unfinished jobs do not count; already accepted jobs continue to reconcile.
The limit does not retroactively cancel concurrently admitted jobs. History is
retained until the source bytes change; an explicit review authorizes one action,
not a reset of future searches. No database migration or tuning setting is added.

The pending action shows the proposed candidates, samples and quality policy.
Use `action_resume` with `cancel` to stop, or `run_once_after_review` only after
an explicit user decision to run that displayed round. Agents must not select
that decision automatically or start successive CRF ladders after no-winner
results. A user can still choose direct candidate encoding with a non-optimizing
profile or `profile_config.optimization.enabled=false`; this preserves structural
checks and the original, but does not claim the failed perceptual checks passed.

`planned_audio` describes the future encoding policy, never a completed audio
conversion: the benchmark measures video and estimates audio size. In compact
mode, an EAC3 track is copied and has no invented AAC bitrate target. Rejected
benchmark size estimates are not achieved savings.

`action_list` keeps its small listing shape; use `action_status` for the
benchmark explanation, planned audio and concrete review proposal.
