# Perceptual quality validation

Navigatorr can measure a selected VMAF v1 model and an independent CAMBI
full-reference banding metric during benchmarking. The final encoded candidate
is checked again on the same deterministic source windows before publication.
The existing 10-bit source restriction remains: native Main10 material uses
SSIM when `metric: auto` is requested. CAMBI on native Main10 remains gated
until its source-to-worker path is verified end to end.

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
native 10-bit/HFR. Non-HFR VMAF v1 is rejected for sources at 45 FPS or above.

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
Other VMAF v1 models, including HFR and 4K, require registry entries and
their own executable capability probes. Full-file validation is intentionally
not offered because per-frame JSON logs are bounded.
