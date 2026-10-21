# Automatic processing (policy v4)

The existing Process button and MCP `action_run` use `mode=size|quality|x265_preserve|compact`.
There are no new configuration steps. Every new operation freezes `transcode-modes-v4`.

Under v4 every goal is a short, fixed ladder of whole-file encodes. Nothing is
sampled or searched: the file is encoded in full at the goal's first rung and the
worker's final quality gate judges the result (see "Final validation" below).
Rungs are ordered most compressed first. The first rung that passes is kept; a
rung that fails on quality is followed by the next, which compresses less; a
rung refused for its size (it does not save enough, or grew the file) ends the
ladder, because a better rung only makes the file larger. When the last rung
fails too, no candidate is offered and the original is preserved.

* size: x265 ladder (below) or, with the media engine, a VideoToolbox ladder; at
  least 15% actual savings. Optional size budget.
* quality: strict quality limits, no output growth.
* x265_preserve: strict quality limits, libx265 only, no growth; existing HEVC omitted.
* compact: a much smaller SDR file (see below); size-mode quality limits and at
  least 30% actual savings.

| Goal | Encoder | Rungs (most compressed first) | VMAF minimums: mean / p5 / worst 5 s window |
|---|---|---|---|
| size | VideoToolbox, then x265 on a worker without it | VideoToolbox `-q:v` 60, 65, 70; x265 CRF 22, 19, 16 | 88 / 84 / 85 |
| size, `allow_hardware=false` | x265 | CRF 22, 19, 16 | 88 / 84 / 85 |
| size with a budget | see "Size budget" | VideoToolbox `-q:v` 65, 58, 50, then average bitrate; or one two-pass x265 encode | floor 70 / 60 / 65 |
| quality | x265 | CRF 20, 17, 14 | 95 / 93 / 92 |
| x265_preserve | x265 | CRF 24, 21, 18 | 95 / 93 / 92 |
| compact | x265 | CRF 18, 16, 14 | 88 / 84 / 85 |

x265 rungs use the medium preset. `allow_slow_preset=true` (size only) makes the
size x265 ladder use slow, whose full encode of a long 1080p source can take hours.
VideoToolbox rungs use constant quality (`-q:v`, higher is better) with a
240-frame GOP. The rungs are frozen in the policy snapshot (`recipe`) with its
savings and growth limits, so they cannot change after admission. A rung is a
function of the frozen policy, the source and the rung number kept in the record
(`quality_ladder`), so validation rebuilds it and accepts only a plan that matches
it exactly (a quality ladder's plan carries no byte ceiling; a size budget's does).

Before publication the worker's final check, the actual final bytes, streams and
source/candidate integrity still must pass. A negative outcome preserves the
original; it does not imply every possible encode is bad. No profile is created,
no quality limit is relaxed, and there is no search time limit: a goal costs at
most one full encode and one final check per rung.

`action_status.transcode` and the web detail show the same explanation: the
ladder (`quality_ladder`: encoder, rungs, the rung encoding or kept, and what
each earlier rung measured) or the size budget record. An accepted full
conversion is a candidate, never a replacement: replacement approval is separate
and is never authorized by processing or changing a goal.
`transcode_goal_change` provides a read-only selection preview and a versioned
command for the same goal recovery available in the web UI. Parent inputs and
previous attempts remain immutable; active attempts finish before another attempt
is admitted.

The metrics describe the actual sampled source class and model, not a universal
quality percentage. No claim of full-motion human review or lossless video is made.

## Final validation

Every rung's output is judged by the worker's final gate, not by an estimate:
VMAF (model `v1_1080p_3h`) mean, p5 and worst 5-second window, and full-reference
CAMBI banding (mean at most 6, peak at most 16), measured on
deterministic source windows of at most 10 seconds against the original (see
[PERCEPTUAL_QUALITY_VALIDATION.md](PERCEPTUAL_QUALITY_VALIDATION.md)). Size
goals measure one window per 600 s of source (5 to 12 windows) plus up to two
fresh windows that are reported but do not decide the verdict. Other goals, and
sources too short for that grid, measure five windows (the 20%, 50% and 80%
positions plus 35% and 65%); a source under 50 seconds keeps its three planned
windows. A quality miss moves the ladder to the next rung and a pass keeps the
file; the verdict names the failed metric and its limit.

When a size goal's ladder ends with no candidate and the policy allows it
(`audio_only_fallback`, on for size), navigatorr copies the video unchanged and
compacts only the lossless audio, which is accepted when it still meets the 15%
minimum saving (and the budget, if any).

If every VideoToolbox rung misses the quality limits the record sets
`retry_without_hardware`: x265 at the same goal usually keeps more quality, so the
file may be run again with `allow_hardware=false` (the job says so and the web UI
offers **Try with x265**).

## Earlier policies

Operations frozen under `transcode-modes-v1`, `-v2` (quality, x265_preserve,
compact) and `-v3` (size) chose their encode settings with a CRF sample search over
short windows, and could continue with x265 after a failed VideoToolbox winner.
That search no longer exists. Their actions and any `benchmark_transcode` action
stay readable in history, but an old action that is not finished when the engine
next meets it ends as failed with "This operation used the removed sample search;
start it again to use the current recipe. Original preserved." New requests for
`benchmark_transcode`, optimization profiles (a profile with optimization
enabled, `profile_config.optimization.enabled`) or the inputs `metric`,
`priority`, `shared_calibration`, `calibration_items` and `batch_fixed_quality`
are refused with an input error: choose a goal (`mode`) instead.

## Media engine option (size mode)

`mode=size` accepts `allow_hardware` (default **`true`**). With it on, a worker
that lists the `hevc_videotoolbox` encoder runs the VideoToolbox ladder above
first: it encodes a whole episode in minutes and uses much less CPU than x265,
and the final gate decides whether the result is good enough. A worker without
VideoToolbox runs the x265 ladder instead and the job says so. With
`allow_hardware=false` only x265 is used. `hardware_margin_percent` (0 to 50,
default 15) is still accepted and frozen, but no longer changes the result:
nothing compares a VideoToolbox result with an x265 one any more.

It is on by default for size and always visible and switchable: the Library and
batch setup show an *Allow the media engine (faster)* checkbox under Save space,
*Change goal* shows the same control, and the MCP input `allow_hardware=false`
changes it per job or batch. quality, x265_preserve and compact reject
`allow_hardware=true`.

A result from the media engine is labeled before anything is replaced: the job
shows *Encoder: VideoToolbox (hardware media engine)* with a *Hardware* badge
(x265 shows *Encoder: x265 (software)*), the candidate lists name
`VideoToolbox (hardware)`, and the MCP job and action detail carry `encoder` and
`hardware` on the size result. The MCP `transcode_goal_change` preview (no
`mode`) returns the `allow_hardware` / `hardware_margin_percent` the selected
smaller-files attempts were frozen with, and `hardware_mixed` when they differ;
leaving `allow_hardware` out of a change to size still takes the default (on), so
pass the shown value to keep it. See [TRANSCODE_MODES_REVIEW.md](TRANSCODE_MODES_REVIEW.md)
and the [experiment](reviews/2026-10-19-hardware-media-engine-experiment.md).

## Size budget (size mode)

`mode=size` accepts one optional size budget, a ceiling for the finished file.
It is for a source where the quality target cannot be met inside the size you can
afford. Without a budget nothing changes: the size ladder above runs.

**Inputs.** Exactly one of:

* `size_budget_percent`, 5 to 85: the budget as a percentage of the source file.
  The upper bound exists because size mode's worker-enforced minimum saving is
  15%, so a budget above 85% of the source could never be what limits the file
  and is refused up front.
* `size_budget_bytes`, at least 50,000,000: an absolute ceiling.
* `size_budget_source=siblings` with an optional `size_budget_sibling_multiplier`
  (1.0 to 4.0, default 1.5): the budget follows the other episodes. It is the
  median total bitrate of the other video files in the same folder, times the
  multiplier, times the source's duration. When that folder has too few and is
  named like a season (`Season 1`, `S01`, `Specials`, …), the series folder
  above it is scanned too; a movie's own folder or a flat series never widens
  to the library. Files are compared within a resolution class (SD, 720p,
  1080p, 1440p, 2160p, from the width and the height scaled to 16:9, so a
  letterboxed 1920-wide film is 1080p). At least 3 such files are needed;
  without them the file runs as if it had no budget, and the job says why. The
  scan probes at most 60 files and stops after 3 minutes. The multiplier is
  refused without the source.

The three are mutually exclusive. The effective budget is the smaller of the
budget and 85% of the source, so the 15% minimum saving that size mode enforces
always holds. The budget, the inputs and the result are
frozen into the batch and per-file goal-change policy (`size.size_budget`).

**What runs.** The budget replaces the size ladder with one of two recipes.
Audio-only compaction still wins when it alone brings the file inside the
budget: the video is copied and nothing is re-encoded. When no budget encode can
be planned (the source's or its video stream's size or duration is unknown, the
other streams already take the budget, or, without the media engine, the worker
cannot encode two-pass x265), the action ends with no candidate and the reason
(`size_budget_estimate_unavailable`, `size_budget_unreachable`,
`size_budget_worker_unsupported`, or `size_budget_unavailable` for a sibling
budget with no evidence); the original is preserved.

**VideoToolbox ladder.** When the goal allows the media engine
(`allow_hardware`, on by default) and the worker lists `hevc_videotoolbox`, the
file is encoded in full at a short ladder, best picture first: constant quality
(`-q:v`) 65, then 58, then 50, then VideoToolbox's average bitrate at the
budget's video bitrate (the x265 budget bitrate below). The values are frozen
in the policy (`size.size_budget.vt_quality_ladder`). Each step's plan carries
the budget as an absolute ceiling (`size_policy.max_candidate_bytes`), so the
worker refuses an over-budget file right after the encode, before it spends
minutes scoring it, and the next step is encoded. The first step that fits is
scored by the normal final validation against the floor:

* within the budget and meeting the goal's quality target: `meets_quality_target_within_budget`;
* within the budget, below the target but above the floor: `below_quality_target_within_budget`, offered for review;
* below the floor: no candidate is offered. The job says *Try again with x265
  (allow_hardware off): slower, but it usually keeps more quality at the same
  size*, and the web UI shows a **Try with x265** button that starts a new
  `transcode_media` action with the same inputs and `allow_hardware=false`;
* the last step (the average bitrate) still over the budget: `size_budget_exceeded`, original preserved.

The over-budget steps stay in the job's size and quality points. Validation
rebuilds the step from the frozen policy, the source and the step number kept
in the record and accepts only a plan that matches it and the ceiling exactly.

**The x265 budget encode.** With the media engine off (`allow_hardware=false`),
or on a worker without it, one two-pass x265 average-bitrate encode (preset
medium, lookahead 40, 4 B-frames, 3 reference frames, aq-mode 3, aq-strength 1,
psy-rd 2, psy-rdoq 1; main10 when the plan is main10) aims at 97% of the budget.
The video bitrate is `(target - non-video bytes) * 8 / duration`, where
non-video bytes are the source minus its video stream, minus what lossless audio
compaction saves. A budget roomier than two-pass x265's 50 Mbps ceiling is met at
the ceiling, and the file comes out smaller. The size is therefore known before
the encode starts and shown as the estimate (*Budget estimate*). Validation
rebuilds these settings from the frozen policy and the source and accepts only a
plan that matches them exactly. It faces the same floor and outcomes as below.

**Validation.** The result is judged against the same final windows as any size
result, but against a lower, frozen floor: VMAF mean 70, p5 60 and worst
5-second window 65. A VMAF difference of about 20 spans one ACR category and
about 6 is one just-noticeable difference, so this floor is roughly four
just-noticeable differences below the normal 88/84/85 gates. It is a floor for
a file that is smaller on purpose, not a promise of transparency. The banding
check (CAMBI) is not lowered, because banding is the typical low-bitrate
artefact on dark content and the floor would not hide it. The floor is part of
the frozen policy and is not user-tunable.

**Outcomes.** The result carries an `outcome`:

* `meets_quality_target_within_budget`: the budget encode also met the normal
  quality target.
* `below_quality_target_within_budget`: it is within the budget and above the
  floor but below the normal target. The candidate is labelled *Below quality
  target — within size budget* in the job, the queue, the batch candidate list
  and the replacement review, with the failed target checks listed.
* a quality rejection when the floor itself is not met, and a size rejection
  (`size_budget_exceeded`) when the finished file is still over the budget.
  Reason codes name the failed metric and its limit.

A budget candidate is never promoted automatically: like every candidate it
waits for review, and the label travels with it into the replacement review.
The batch approval itself also says how many of its candidates are below the
quality target, wherever it is answered.

**Seeing it.** Job detail shows the budget against the result, the encoder the
budget used and why, an inline rate-quality chart (size across, VMAF mean up, the
budget as a vertical line, the quality target and floor as horizontal lines, the
over-budget steps and the result as points) and a collapsed list with the same
numbers for browsers without SVG support. `action_status` carries the same data
as `size_budget`, and the compact summary reports `plan_source=size_budget` with
outcome `size_budget`. For a sibling-matched budget the job reads "Budget from N
sibling episodes (median X Mbps × M)", shows how the source's bitrate compares
with the median, and lists the files used under a collapsed *Sibling episodes*
section. *Change goal*, a batch setup and *Run again* carry the budget: the UI
offers *None*, *Percent of source*, *Gigabytes* and *Match sibling episodes*, and
shows the budget a file was frozen with. The `transcode_goal_change` preview
shows it too (`size_budget`, `size_budget_mixed`); a change to size without
`size_budget_*` inputs drops it.

**Not done.** The floor is frozen, not user-tunable. A VideoToolbox budget
encode that misses the floor is reported below the floor; it is not re-encoded
with x265 unless you run it again with `allow_hardware=false`.

## Compact mode

`mode=compact` (optional `max_height` 720, 1080 (default), 1440 or 2160) is the
goal for HDR and Dolby Vision sources, and encodes the converted picture with
the x265 ladder in the table above (CRF 18, then 16, then 14). It accepts
HDR10, HDR10+, PQ, HLG, Dolby Vision profile 7/8 and SDR sources taller or wider
than the limit. It deliberately drops HDR, Dolby Vision and HDR10+ dynamic
metadata and lossless audio fidelity, so it is a separate goal rather than
a relaxed size mode. Other goals skip HDR and Dolby Vision sources as
`hdr_requires_compact` (not `unsupported_source`), so a per-file goal change
to compact picks them up:

* Video: a frozen `compact-sdr-v1` chain. PQ sources use BT.2390 EETF on
  maxRGB from the mastering/MaxCLL peak to 100 cd/m²; HLG sources use the
  BT.2100 OOTF for a 1000 cd/m² display first. Both go through a generated 33³
  LUT (tetrahedral), BT.2020→BT.709 gamut clipping and BT.1886 encoding. The
  picture is scaled (lanczos, never upscaled) to fit a 16:9 box of
  `max_height`, tagged BT.709 limited range, and every HDR/DOVI side data type
  is deleted. Dolby Vision 7/8 is decoded from its base layer; profile 5 has no
  compatible base layer and is rejected. SDR sources at or below the limit are
  skipped as `compact_not_needed`; wide-gamut SDR and anamorphic sources are
  rejected. High-frame-rate sources use the same decimated metric pairing as
  the other goals.
* Dolby Vision is detected only from structural evidence: the DOVI
  configuration record, a `dvh1`/`dvhe` codec tag or an RPU on decoded
  frames. Stream titles are never trusted. The base layer must match the
  profile's signal compatibility (7.6 and 8.1 PQ/HDR10, 8.4 HLG, 8.2 SDR);
  anything else is refused as `dolby_vision_base_layer_unrecognized`.
* libx265 compact encodes always pass `-dolbyvision 0`: the encoder's default
  follows the stream-level configuration record, which side-data deletion
  cannot remove. FFmpeg builds without the option (before 7.1) are refused
  for libx265 compact work; VideoToolbox does not emit Dolby Vision.
* The video stream title is cleared and the stale statistics tags (`BPS`,
  `NUMBER_OF_BYTES` and the rest of the mkvmerge set) are removed from the
  video and every converted audio track.
* Audio: the existing compact audio policy. DTS/DTS-HD MA, TrueHD, PCM, FLAC
  and ALAC become AAC with the channel layout kept (stereo at 192 kb/s, 5.1 at
  384 kb/s, 7.1 at 512 kb/s). Layouts the AAC encoder would remap (6.1,
  7.1 wide-side, height layouts) are copied rather than altered. E-AC-3, AC-3,
  AAC and Opus tracks are copied, so Atmos carried as E-AC-3 JOC survives;
  TrueHD Atmos becomes plain AAC. Converted tracks are retitled from their
  layout ("AAC 5.1"); other titles stay. Every track, language, subtitle,
  chapter and attachment is kept.
* Quality: the metric reference is the source through the identical chain, so
  VMAF/CAMBI measure only encode loss at the target size. The chain, LUT digest
  and measurement space are recorded in the final evidence, and the final
  output must pass an SDR signalling check (stream and decoded frames).
* Workers advertise `compact_transcode` only after executing the chain on a
  synthetic frame; the server refuses compact work without it. Replacement still
  needs normal approval.
* Re-grab risk: Sonarr or Radarr may see a compact file as below the quality
  profile's cutoff and download the original release again. Every compact
  approval (single file and batch) carries a warning and, when the promotion
  knows the library item, a read-only snapshot of its monitored state and
  quality profile. Unmonitor the item or move it to a suitable profile before
  approving; Navigatorr never changes these settings.
