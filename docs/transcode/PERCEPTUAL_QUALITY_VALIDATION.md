# Perceptual quality validation

Navigatorr measures a selected VMAF v1 model and an independent CAMBI
full-reference banding metric on the final encoded candidate, on deterministic
source windows, before publication. Each goal's ladder (see
[TRANSCODE_AUTOMATIC.md](TRANSCODE_AUTOMATIC.md)) is judged by this check.
Native SDR Main10 can use VMAF/CAMBI when the worker advertises
`quality.native_10_bit`: both metrics must execute on a native 10-bit synthetic
pattern with automatic pixel conversion disabled. Older/unverified workers
retain the VMAF/CAMBI gate and the existing SSIM fallback for `metric: auto`.
This applies to both libx265 and VideoToolbox; libx265 receives yuv420p10le.
Main10 measurements also run this capability check when `vmaf.model` is omitted
(legacy/default VMAF, including combined VMAF/SSIM). Omitting the model does not
change the scoring policy or bypass native-depth verification. Main10 VMAF
commands disable automatic pixel conversion, so incompatible inputs fail
instead of silently measuring an 8-bit conversion.

## The quality policy

Each goal freezes its quality policy in its policy snapshot, and the final
check request carries it to the worker. It is not configured per request: the
`quality` goal freezes this shape (the other goals freeze their own limits; see
[TRANSCODE_MODES_REVIEW.md](TRANSCODE_MODES_REVIEW.md)):

```yaml
quality:
  preferred_metric: vmaf
  vmaf:
    model: v1_1080p_3h
    target: 96
    minimum: 95
    marginal_tolerance: 2
    guardrail_enforcement: reject
    p5_minimum: 93
    worst_window_minimum: 92
    worst_window_seconds: 5
  banding:
    enabled: true
    metric: cambi
    mode: full_ref
    enforcement: reject
    max_mean: 6
    max_peak: 16
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

Every VMAF, CAMBI and SSIM graph restamps both inputs by frame index
(`settb=AVTB,setpts=N`) before the metric (SSIM keeps the inputs' own pixel
format; VMAF/CAMBI convert to 10-bit first). The metrics pair frames by timestamp, and
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
own 5 s budget, well under the coordinator's 15 s capabilities request
timeout, and normally takes well under a second; a slow probe withholds HFR
only, never native-rate VMAF/CAMBI. Without the capability the worker reports
`quality.hfr_probe_reason`. A timed-out probe is remembered for one minute and
then retried on its own, on top of the cached native-rate results.

Coordinators treat HFR as unavailable on workers without the capability, so a
coordinator never dispatches HFR work a worker would reject. Automatic modes
skip such sources with `hfr_quality_unavailable` (distinct from
`unsupported_source`: a capable worker clears it, a new goal does not). A
transient capability lookup failure waits for the worker instead of skipping.
The worker's benchmark endpoints with a VMAF model follow the same
capability gate. Deploy the worker before relying on HFR support.

Decimation reuses the non-HFR model's calibration; it is not an HFR-trained
model. Scores for HFR sources are comparable with 25/30 FPS content, not with
an HFR model, and thresholds were not separately calibrated for HFR libraries.

`guardrail_enforcement: observe` records p5, the worst window and frame
counts without changing the verdict. `reject` makes a missed limit fail the final
check, which is what the goals use. CAMBI uses a separate bounded pass because
VMAF v1 already registers its own CAMBI feature. Its full-reference output
measures additional banding relative to the source; a lower value is better.
CAMBI has no limit unless the policy sets one; the goals set mean 6 and peak 16.

## Limits are frozen per goal

The limits are not user-tunable and no goal relaxes them. They were chosen
from the measurements in [TRANSCODE_MODES_REVIEW.md](TRANSCODE_MODES_REVIEW.md);
the CAMBI 6/16 limits are inherited guardrails that those fixtures do not
calibrate exactly. A failed final check leaves the original untouched and never
publishes the local candidate. A worker only reports the model as available after
its executable probe passes.

The final check extracts each planned source and candidate window losslessly,
checks frame counts and relative timestamps, and stores bounded evidence
with the candidate SHA-256, size, plan digest, model, metric version, worker
capability fingerprint, and verdict. Window locations use the source's nominal
FPS and are marked `nominal_fps`; they are not claimed to be frame-accurate
timestamps for variable-frame-rate material. Raw per-frame logs remain local
to worker scratch and are cleaned up after validation.

Windows are measured up to three at a time, as many as the job's share of the
host CPUs (CPUs ÷ `max_parallel_jobs`) can give two libvmaf threads each, and
libvmaf threads are split across them (a 10-CPU worker with two parallel jobs
measures two windows at once with two threads each). Each window's lossless extracts are deleted as soon
as it is measured, so scratch holds at most that many windows at once. The
evidence is identical to measuring the windows one by one: statistics are
combined in window order, and a failure reports the first failing window in
that order (later windows are cancelled).

The final job status exposes `quality_evidence`, which action results carry
under the same name; large evidence remains available through action detail when
the compact MCP response omits it.

The current implementation supports the standard 1080p 3H VMAF v1 model.
Other VMAF v1 models, including HFR-trained and 4K ones, require registry entries and
their own executable capability probes. Full-file validation is intentionally
not offered because per-frame JSON logs are bounded.

## Reading the result

`action_status` and `action_run` include a compact `benchmark` summary (the key
keeps its historical name) with the outcome, next step, failed checks, configured
limits where available, and a source timestamp to review. Progress refers to the
full encode of the current ladder rung. A VMAF pass means the configured sampled
checks passed, not guaranteed perceptual transparency. Full-reference CAMBI
measures additional banding relative to the source, so existing source banding
alone does not explain a rejection.

`planned_audio` describes the future encoding policy, never a completed audio
conversion. In compact mode, an EAC3 track is copied and has no invented AAC
bitrate target. Estimated sizes are not achieved savings.

`action_list` keeps its small listing shape; use `action_status` for the
explanation and planned audio.

## Low VMAF p5 on dark, grainy sources

A production case where every encoder kept a 3.4–4.5 point gap between VMAF
mean and p5 on a dark, grainy 1920×960 source was investigated. The candidate
is encoded from the FFV1 reference cut, both inputs are re-timed by frame index
(`setpts=N`) and converted to the same pixel format without scaling, and p5 is
pooled over all sampled frames. A local reproduction with synthetic temporal
grain showed:

- a start off a keyframe scores the same as one on a keyframe;
- a one-frame shift drops the score to about 54 (lowest frame about 45), far
  below anything observed, so misalignment is ruled out;
- 8-bit versus main10 output differs by about 0.2 points, and no scaling or
  range conversion is involved;
- grain alone costs 1.5 (CRF 16) to 2.7 (CRF 20) points of mean and pulls the
  lowest frames down; the finest VIF scale collapses because x265 smooths
  frame-random grain in dark, flat areas.

This is content behaviour, not a measurement bug. The quality goal's p5 floor
(93) is strict for such sources; the size goal's floor (84) is met by the same
settings. Policy options, none enabled here:

1. Keep the floors: under the quality goal a grainy source goes down the ladder
   to CRF 14 or keeps its original.
2. Add grain-preserving x265 rungs (`tune=grain`, or no SAO with stronger
   psy-rd/psy-rdoq) for such sources. In the reproduction `tune=grain` raised
   CRF 20's p5 by about 2 points but nearly doubled the sample size, so it
   should be judged on size per quality, not quality alone.
3. Detect grain from the source and add option 2's rungs automatically.
