# Transcode mode policy v1

`transcode_media` and `transcode_batch` accept `mode=size|quality|x265_preserve` through the same action engine used by UI and MCP. Requests without `mode` retain their legacy behavior. Mode requests reject profiles, priority, quality/size controls, custom metrics and preservation overrides. Replacement authorization remains separate; filesystem operations create candidates and never replace originals.

Each operation freezes `transcode-modes-v1`, the complete sampled policy and its SHA-256 digest. Reload verifies the persisted content against that digest. A batch child verifies its parent, manifest membership, file path and mode before inheriting the frozen policy. Plans include mode, policy digest and final size policy in their own digest.

| Mode | Mean target/minimum | P5 minimum | Worst 5-second window | Final savings | Encoder |
|---|---|---|---|---|---|
| size | 92 / 88 | 84 | 85 | at least 15%, no growth | measured supported x265 / VideoToolbox |
| quality | 96 / 95 | 93 | 92 | at least 15%, no growth | measured supported x265 / VideoToolbox |
| x265_preserve | 96 / 95 | 93 | 92 | at least 0%, no growth | libx265 only |

CAMBI 6/16 limits are inherited initial guardrails: the fixtures (peak at most 3.283) do not calibrate those exact boundaries.

All modes use VMAF model alias `v1_1080p_3h` (`vmaf_v1.0.16_3d0h`), enforced full-reference CAMBI mean <=6 and peak <=16, and sampled final-candidate validation. These are conservative sampled policy gates, not a guarantee of visual transparency or pixel identity. Sources already HEVC are omitted only in `x265_preserve`. Resolution, source depth/color and existing stream preservation checks remain mandatory. Supported source classes are one 8-bit or 10-bit SDR video stream below 45 fps. Native Main10 additionally needs the worker's execution-tested native quality capability; HDR/Dolby Vision and unsupported classes preserve the original before encoding.

The common optimization selector filters quality eligibility and exact unrounded byte gates first. `size` contests all passing plans. The strict modes restrict to the allowed score margin of the best measured score. Within a fixed 2% band of the smallest qualifying byte estimate, it prefers measured lower sample-encode duration, then stable candidate ID. Unknown time never means faster. Numeric CRF/quality values are not compared between encoders. Estimates remain estimates; worker validation before publication and independent coordinator acceptance enforce actual size and evidence identity.

Mode batches measure each source rather than inheriting another source's sample quality. There is no representative veto of an unmeasured season. This deliberately trades representative reuse for source-specific evidence. The search is bounded to three 10-second windows and at most six configurations: x265 CRF 20/23/26, plus supported VideoToolbox quality 65/70/75 for the first two modes. A hardware-only worker can use its three measured plans. `x265_preserve` never substitutes hardware. Mixed requests and Main10 are tested with fake capabilities and typed candidate validation; this is not a hardware performance claim.

A source search receives 600 seconds of runner wall time, beginning after worker staging/queue admission; its start is persisted and restart cannot reset it. The operation has 3600 seconds of search allowance. The parent persists a maximum reservation before starting each source, retains reservations for uncertain accepted work or interrupted search invocations with unknown prior cost, and settles authoritative measured search time to release unused allowance. A restarted search with unknown cumulative cost omits that measurement and keeps its full reservation; the reservation is a conservative allowance, not reported measured CPU or runtime. This measurement includes synchronous sample I/O, probes and quality evaluation, and is not CPU utilization. Queue and human decision waits do not consume that search allowance. Existing same-source unsuccessful-search history also prevents changing action IDs or recipes from silently restarting repeated negative searches. Explicit retries remain reviewed operations. Exhaustion gives attention with no conclusion for unmeasured files. Search timing is not full-file encode timing.

## Calibration evidence and limits

Nine real FFmpeg encodes cover three deterministic two-second 1080p/24fps synthetic sources (motion/checker detail, SDR gradient/box, noisy native Main10), at x265 medium CRF 20/26/40. Raw measurements are in [fixtures/transcode-modes-v1.json](reviews/fixtures/transcode-modes-v1.json). The strict minimum 95 rejects the native grain q26 measurement (mean 94.3357, P5 93.0816), which static inspection showed smoothed fine noise. The size policy admits that measured compromise. Motion q26 measured mean 96.4376/P5 95.1632 and retained the checker detail in the inspected crop. Motion q40 measured mean 77.9165 and lost detail; gradient q40 measured mean 86.3582 and showed ringing, both rejected by the size minimum. Gradient q20/q26 remain below the strict minimum, so the strict policy conservatively has no winner among those tested settings.

The reviewer inspected selected native-resolution crops at t=1s: motion source/q26/q40, gradient source/q26/q40 and grain source/q20/q26. This was AI static-image inspection, not human full-motion review or validation against a real library. Small synthetic samples cannot establish universal perceptual thresholds. FFV1 reference-byte savings are not representative of H264/HEVC library savings. No production source was encoded or replaced. These measurements substantiate the bounded starting policy and its conservative grain rejection; they do not demonstrate an operational speed improvement.

Local system FFmpeg 9.0.1/libvmaf could not initialize the requested model (`Speed_chroma_feature_speed_chroma_uv_score`). Measurement used a temporary libvmaf build pinned to `f85a853692a8c730d0270cd733c8bb30b5b93b7c`; no system library was replaced. Workers independently execution-probe model availability and fail closed when unavailable. To reproduce with a compatible installed libvmaf or a separately built pinned library:

```sh
python3 scripts/calibrate_transcode_modes.py --output-dir /tmp/navigatorr-mode-calibration --ffmpeg /path/to/ffmpeg --libvmaf-dir /path/to/vmaf-build/src
```

The optional library directory only affects subprocesses launched by the reproducer. Omitting it uses the normal FFmpeg installation. The script emits the references, candidates, per-frame metric logs, screenshots and JSON measurements in the requested output directory; timings are host-specific.
