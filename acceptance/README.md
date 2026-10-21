# Transcode acceptance suite

An end-to-end check that every transcode goal produces what the UI promises.
It drives the real action engine in-process and the real
`navigatorr-transcode serve` worker binary (built from this tree, started from a
YAML config on a loopback port, spawning `_internal_run` children
as in production) over synthetic ffmpeg-lavfi sources. Nothing outside
a temporary directory is read or written, and no network service is used.

It is test-only: no production code is changed or mocked. Run it on the
transcode worker host before a deploy.

## Run

```
NAVIGATORR_ACCEPTANCE=1 go test ./acceptance -v -timeout 60m
```

Without `NAVIGATORR_ACCEPTANCE=1` the package skips in well under a second, so
`go test ./...` is unaffected.

| Variable | Meaning |
| --- | --- |
| `NAVIGATORR_ACCEPTANCE` | `1` to run. |
| `NAVIGATORR_ACCEPTANCE_FFMPEG`, `NAVIGATORR_ACCEPTANCE_FFPROBE` | ffmpeg and ffprobe to use; default is `PATH`. |
| `NAVIGATORR_ACCEPTANCE_ALLOW_MISSING` | `1` turns "the worker lacks a needed capability" from a failure into a skip (development machines). |
| `NAVIGATORR_ACCEPTANCE_REPORT` | Path for a JSON report of the table. |
| `NAVIGATORR_ACCEPTANCE_KEEP` | `1` keeps the temporary tree (media, worker state) for inspection. |

When the suite is enabled and the worker lacks something a case needs (an
encoder, the VMAF model, the `lut3d` filter, VideoToolbox) that case fails with
the capability named. It never passes by doing less.

## Cases

Each case is a subtest, run one after another against one engine and one worker.

| Case | Source | Goal | Must produce |
| --- | --- | --- | --- |
| 1_size_hardware | 1080p SDR H.264 + FLAC | size, hardware allowed | candidate within the size budget, at least 15% smaller, AAC audio, HEVC video, passing quality evidence, a VideoToolbox rung in the budget, original hash unchanged |
| 1b_size_hardware_no_budget | same | size, hardware allowed, no budget | passing candidate at least 15% smaller, AAC audio, from a rung of the VideoToolbox quality ladder (`plan_source` `quality_ladder`, encoder `hevc_videotoolbox`, no x265 fallback), no sample test |
| 2_size_x265 | same | size, hardware off | passing candidate from the one two-pass x265 encode at the budget (`budget_first`, encoder choice `hardware_not_allowed`), no sample test |
| 3_compact_hdr | 4K HDR10 HEVC 10-bit, tagged BT.2020 / PQ with mastering display and light level | compact | at most 1080p SDR BT.709, at least 30% smaller, passing quality from a rung of the x265 CRF ladder, no sample test |
| 4_quality | 1080p SDR | quality | passing candidate against the strict thresholds from a rung of the x265 CRF ladder, no sample test |
| 5_x265_preserve | 1080p SDR H.264 | x265_preserve | passing HEVC candidate from a rung of the x265 CRF ladder, no sample test |
| 6a/6b/6c | HDR + size, SDR + compact, HEVC + x265_preserve | skips | a completed action with the documented skip reason, and the source profile (`SourceGoals`) predicting the same verdict |
| 7_timestamp_glitch | variable frame rate with a burst of frames closer than one frame period | size | alignment passes, a candidate with the same number of frames |
| 8_no_size_tags | remux without per-stream Matroska statistics tags | size | a planned budget (never `size_budget_estimate_unavailable`) and a candidate |
| 8b_no_size_tags_flac | the same, with FLAC audio that states no bitrate either | size | the same: the VideoToolbox ladder is used, not the x265 fallback |
| 9_promotion | short SDR file | x265_preserve, then `promote_transcode_candidate` (filesystem) | the reviewed plan digest approved, the library file replaced by the candidate (valid, decodes, hash equal), candidate directory and recovery copy cleaned |

Candidate cases also check: the candidate sits under `.navigatorr-candidates`,
decodes end to end, keeps the audio track count and duration, the original's
SHA-256 is unchanged and equals the hash the action recorded, and a VMAF mean
is present and sane.

## Output

A table per case (goal, outcome and reason code, source and candidate size, share
saved, VMAF mean / p5 / worst window, encoder with its VideoToolbox rung or ladder step, wall time) is
logged at the end of the run, and written as JSON to
`NAVIGATORR_ACCEPTANCE_REPORT` when set.

## Runtime

Expect 25 to 35 minutes on an Apple M-series host. Cutting the source length
barely changes it: the full-file encodes and their final validation dominate,
and quality ladders that need a second or third rung repeat both. The skip cases take seconds. A single case is
limited to 40 minutes, the whole run needs `-timeout 60m` or more.

## A failing case

A failing case is a finding about the pipeline, not about the suite: do not
deploy that commit to the worker. Case 8b, for example, caught size budgets
that could not be planned for FLAC sources without per-stream statistics, which
silently traded the VideoToolbox ladder for a much slower x265 encode.

## Development machines

The quality gate needs the VMAF model the worker reports as `v1_1080p_3h`. A
libvmaf build that cannot initialise it makes every candidate case fail with the
`vmaf` capability named; setting `NAVIGATORR_ACCEPTANCE_ALLOW_MISSING=1` turns
those into skips.
