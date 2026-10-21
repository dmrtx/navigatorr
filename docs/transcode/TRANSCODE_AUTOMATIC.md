# Automatic processing v2

The existing Process button and MCP `action_run` use `mode=size|quality|x265_preserve|compact`.
There are no new configuration steps. New operations freeze `transcode-modes-v2`;
existing v1 snapshots retain their original thresholds and 15% quality saving gate.

* size: existing sampled quality gates and at least 15% actual savings.
* quality: existing strict sampled quality gates, with no output growth.
* x265_preserve: strict sampled quality, libx265 only, no growth; existing HEVC omitted.
* compact: a much smaller SDR file (see below); size-mode quality gates and at
  least 30% actual savings.

For sources at least 50 seconds long, one immutable benchmark contains three
discovery windows and nonoverlapping verification windows. Size mode's new
snapshots place all of them on the final validation grid (see Grid verification
in [TRANSCODE_MODES_REVIEW.md](TRANSCODE_MODES_REVIEW.md)); other goals keep two
verification windows. The worker first
tests the existing x265 medium 20/23/26 and supported VideoToolbox 65/70/75
settings. If measurements fail, it tests one appropriate fallback family: for
quality rejection, x265 medium CRF 14 to 21 (the highest quality first, then from
CRF 21 down until one meets quality; the slow preset only when the operation sets
`allow_slow_preset=true`), or the slow savings family 24/27/30 when measured
quality can pass but size cannot. Rungs between the initial ladder's steps
(CRF 23/25/27/29) are measured only next to a quality failure, and banding is
measured only for candidates VMAF has not already rejected (see "Quality walk and
refinement" in [TRANSCODE_MODES_REVIEW.md](TRANSCODE_MODES_REVIEW.md)). Operations frozen
before that change keep the earlier 16/18/20 medium-then-slow search. The alternative also applies after an initial verification failure.
Goals other than size verify at most two candidates; size mode's new snapshots
keep verifying until a candidate passes or the search time limit ends. Only a
candidate passing every window can start a full conversion. Actual final bytes, sampled final quality,
streams and source/candidate integrity still must pass before publication.

Workers that advertise `automatic_quality_pruning` receive `quality_pruning`
frozen in the request. Each stage then measures every family's best setting first
(lowest x265 CRF, highest VideoToolbox `-q:v`, all other options equal). When that
setting fails a quality check (VMAF minimum, p5, worst window, frames below
threshold, CAMBI mean or peak), the family's lower-quality settings are not
measured; they appear as `skipped_quality_dominated` and in the stage's `skipped`
list. Incomplete evidence, encode errors and size rejections never skip anything.

All choices and limits are frozen before admission. No profile is created by
this search, no quality threshold is relaxed, and no new action restarts its
600-second deadline. Each file's 600-second search reservation and the
same-source unsuccessful-search review remain enforced. A negative measured
outcome preserves the original; it does not imply every possible encode is bad.

`action_status.transcode` and the web detail use the same explanation, including
tested candidates, stages and verified sample count. A benchmark winner and an
accepted full conversion are distinct outcomes. `transcode_goal_change` provides
a read-only selection preview and a versioned command for the same goal recovery
available in the web UI. Parent inputs and previous attempts remain immutable;
active attempts finish before another attempt is admitted. Replacement approval
is separate and is never authorized by processing or changing a goal.

The metrics describe the actual sampled source class and model, not a universal
quality percentage. No claim of full-motion human review or lossless video is made.

## Media engine option (size mode)

`mode=size` accepts `allow_hardware` (default **`true`**) and, while it is on,
`hardware_margin_percent` (0 to 50, default 15). With the option on, the size
search also tries four VideoToolbox candidates (`-q:v` 45, 50, 55, 60, a
240-frame GOP) beside the x265 ones. They face the same quality gates and
holdout windows. A VideoToolbox result wins when it is at most the margin larger
than the smallest x265 result, or when no x265 candidate qualifies; otherwise
the smallest result wins as before. VideoToolbox encodes several times faster
and uses much less CPU, at the price of up to the margin in size.

**Higher-quality fallback.** On hard content the ladder's best rung can fail
quality (a hard 1080p episode missed the VMAF p5 limit at `-q:v` 60), which used
to leave VideoToolbox out of the search entirely and send it to the slow x265
fallback. When every initial VideoToolbox rung fails quality (measured, or
skipped as dominated by the best rung's failure), or the best rung fails its
holdout on quality, three more rungs (`-q:v` 65, 70, 75, same GOP and B-frames,
IDs `fallback-hardware-cand_q…`) are measured. They run in the same stage as the
first (medium) x265 quality fallback, so the margin has an x265 result to be
compared with, and before the slow stage: a rung that qualifies within the
margin wins and the slow stage is never encoded. When no medium x265 setting
qualifies either, a qualifying rung wins unconditionally, as an initial rung
would (the savings minimum, quality gates and holdouts still apply). If the new
rungs fail too (the highest fails, so the others are skipped), the slow stage
runs as before. They are tried only for this reason, and only when the search
reaches the quality fallback: a ladder that passes, or fails for another reason
such as size, never pays for them, and neither does a search that still has an
eligible x265 setting to verify after a ladder holdout failure.

**Stricter hardware selection.** *(Applies to snapshots frozen with the extra margin, and to a search that cannot verify on the final grid. New grid snapshots freeze no selection margin at all: they verify the candidate on every final-validation window instead; see "No selection margin when the search verifies on the grid" in TRANSCODE_MODES_REVIEW.md.)* A VideoToolbox candidate must clear the quality
gates by a wider margin than an x265 one: its VMAF p5 and worst-window means need
2 VMAF more than the 1.5 every candidate needs, 3.5 in total (p5 at least 87.5
and worst window at least 88.5 against the 84/85 limits). The sample search sees
five short windows while the final check sees every frame, and VideoToolbox's
quality swings more between windows than x265's, so a hardware result that only
just clears the gates on the sample is the one most likely to fail the whole
file. A hard 1080p episode picked `-q:v` 70 on a p5 of 87.1 and a worst window
of 89.2 and then failed the whole-file check; under the new margin that result is
rejected in the sample search (p5 87.1 is below 87.5) and the search moves on to
the next setting, or to x265. x265 candidates keep the 1.5 margin. There is no
worst-frame floor: the single worst frame (78.7 there) is already covered by the
frame-count and p5 checks of the whole-file validation, and a floor tuned on one
episode would reject results that validate fine on others.

**Same-action x265 continuation.** When a VideoToolbox winner fails the final
whole-file quality check, the action no longer ends there. It goes back to the
size search in the same action and round, with hardware excluded, so the x265
families that were never measured (including the slow quality fallback) get their
turn; it then encodes and validates as usual. This happens at most once per
action: if the x265 result fails validation too, the action ends as a quality
rejection, as it always did. The original is never touched and nothing is
promoted before the x265 result is validated.

- *Reuse.* The worker keeps the earlier search's measurements. The continued
  request names the earlier search (`continues_benchmark`, id and request
  digest); the worker reuses an x265 candidate's discovery-window measurements,
  and only for candidates the earlier search rejected on a quality gate at
  discovery (a candidate that passed is measured again, because its holdout check
  needs per-frame scores the saved record does not keep), and only when its own
  saved record matches exactly (same source hash, quality
  policy, windows, rules and capability fingerprint, identical candidate
  settings, one clean row per discovery window). Anything else is measured again,
  never an error. Under grid verification both searches are planned on the same
  final-grid windows, so the rule holds; reuse covers the discovery stage only and
  the continued search still verifies on every grid window.
- *Budget.* The frozen search ceiling (base budget + extension + verification)
  covers both searches together. The continuation starts only if the ceiling
  leaves at least the base search budget after the hardware search's active time;
  its extension is cut to fit what is left. If there is not enough left, the
  action ends as a quality rejection and says why. The batch's reservation stays
  held while a hardware result awaits its final check, and the recorded search
  time is the sum of both searches.
- *Visibility.* The result, the MCP `action_status` `transcode` block and the web
  job detail say that the VideoToolbox result failed the final quality check and
  the action continued with x265, and keep the failed attempt's whole-file VMAF
  mean, p5, worst window and the failed checks next to the final result. A
  quality rejection names the failed metric and its limit.
- *Compatibility.* The continuation reference and the extra margin (when the
  snapshot or the search needs one) are sent only
  to a worker that advertises `search_continuation` / `hardware_selection_margin`;
  a worker without `hardware_selection_margin` gets no hardware candidates (x265
  only), and one without `search_continuation` simply measures everything again
  if it ever sees a continued search. Both flags are in the capability
  fingerprint, so **upgrade the server first**, then the worker (an older server
  fails closed on a newer worker's fingerprint). Workers reject unknown request
  fields, so a new server sends the new fields only after the worker advertises
  the flags. Rolling the server back while an action is mid-continuation is
  unsafe: an older server ignores `hardware_continuation` and may repeat a
  VideoToolbox attempt, so wait for such actions to finish first.

It is on by default for the new policy only, and always visible and switchable:
the Library and batch setup show an *Allow the media engine (faster)* checkbox
under Smaller files (with the margin), *Change goal* shows the same two
controls, and the MCP inputs `allow_hardware=false` or a different
`hardware_margin_percent` change it per job or batch. A snapshot frozen before
this option existed is never re-derived, so existing batches and their digests
stay x265-only; `allow_hardware=false` freezes a snapshot identical to the
earlier size policy. If the worker lacks the capability or the encoder, the
search silently stays x265-only. A worker without `hardware_fallback_search`
gets the ladder alone, without the fallback rungs. Upgrade the server before the
worker: the capability is part of the worker's fingerprint, which an older server
fails closed on.

The fallback rungs are frozen in the policy (`hardware_search.fallback_quality_values`),
so new default-on snapshots have a new digest. Snapshots frozen with the media
engine before this change carry no such field, still re-hash to their digest and
stay on the ladder alone.

A winner from the media engine is labeled before anything is replaced: the job
shows *Encoder: VideoToolbox (hardware media engine)* with a *Hardware* badge
(x265 shows *Encoder: x265 (software)*), the candidate lists name
`VideoToolbox (hardware)`, and the MCP job and action detail carry `encoder` and
`hardware` on the size result. The job also says why the media engine result was
chosen (*Chosen because: ...* under the encoder line, `hardware_reason` in the
MCP benchmark summary): its size against the smallest x265 result and the
margin, that no x265 candidate qualified, or, after a failed holdout, that a
safer setting inside the margin was verified instead. The MCP `transcode_goal_change`
preview (no `mode`) returns the `allow_hardware` / `hardware_margin_percent` the
selected smaller-files attempts were frozen with, and `hardware_mixed` when they
differ; leaving `allow_hardware` out of a change to size still takes the
default (on, 15), so pass the shown values to keep them. See
[TRANSCODE_MODES_REVIEW.md](TRANSCODE_MODES_REVIEW.md) and the
[experiment](reviews/2026-10-19-hardware-media-engine-experiment.md).

## Size budget (size mode)

`mode=size` accepts one optional size budget, a ceiling for the finished file
that the quality search may not exceed. It is for a source where the quality
target cannot be met inside the size you can afford. Without a budget nothing
changes: the policy, its digest and the search are exactly as before.

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

**When it applies.** The normal search runs first. The budget is consulted when
the best passing candidate is larger than the budget (the estimate, or the
measured size), or when no candidate qualifies and a budget encode could still
fit. Audio-only compaction still wins when it alone brings the file inside the
budget: nothing is re-encoded then. When no budget encode can be planned (the
source's video stream size is unknown, the other streams already take the
budget, or the worker cannot encode two-pass x265), the search's own outcome
stands: a passing winner over the budget is kept, otherwise the usual
audio-only or no-candidate result follows, and the job says why the budget was
not applied.

**The budget encode.** A single x265 two-pass average-bitrate encode (preset
medium, lookahead 40, 4 B-frames, 3 reference frames, aq-mode 3, aq-strength 1,
psy-rd 2, psy-rdoq 1; main10 when the plan is main10). It aims at 97% of the
budget. The video bitrate is `(target - non-video bytes) * 8 / duration`, where
non-video bytes are the source minus its video stream, minus what lossless audio
compaction saves. A budget roomier than two-pass x265's 50 Mbps ceiling is met at
the ceiling, and the file comes out smaller. The size is therefore known before
the encode starts and shown as the estimate (*Budget estimate*), not the
over-budget winner. Validation rebuilds these settings from the frozen policy
and the source and accepts only a plan that matches them exactly.

**VideoToolbox ladder.** When the goal allows the media engine
(`allow_hardware`, on by default) and the worker lists `hevc_videotoolbox`, a
size budget does not search at all. VideoToolbox encodes a whole episode in
minutes and, at a budget, measured above the quality floor every time, so the
file is encoded in full at a short ladder, best picture first: constant quality
(`-q:v`) 65, then 58, then 50, then VideoToolbox's average bitrate at the
budget's video bitrate (the x265 budget bitrate above). The values are frozen
in the policy (`size.size_budget.vt_quality_ladder`). Each step's plan carries
the budget as an absolute ceiling (`size_policy.max_candidate_bytes`), so the
worker refuses an over-budget file right after the encode, before it spends
minutes scoring it, and the next step is encoded. The first step that fits is
scored by the normal final validation against the floor:

* within the budget and meeting the goal's quality target: `meets_quality_target_within_budget`;
* within the budget, below the target but above the floor: `below_quality_target_within_budget`, offered for review as before;
* below the floor: no candidate is offered. The job says *Try again with x265
  (allow_hardware off): slower, but it usually keeps more quality at the same
  size*, and the web UI shows a **Try with x265** button that starts a new
  `transcode_media` action with the same inputs and `allow_hardware=false`;
* the last step (the average bitrate) still over the budget: `size_budget_exceeded`, original preserved.

The over-budget steps stay in the job's size and quality points. Validation
rebuilds the step from the frozen policy, the source and the step number kept
in the record and accepts only a plan that matches it and the ceiling exactly.
With the media engine off, or on a worker without it, the search and the
two-pass x265 budget encode run as described above.

**Legacy encoder comparison.** Snapshots frozen before the ladder carry an
encoder comparison instead (`size.size_budget.encoder_comparison`) and keep
running it. When the goal allows the media engine (`allow_hardware`, on by
default), the budget is frozen with an encoder
comparison (`size.size_budget.encoder_comparison`) and the fallback measures
before it encodes. At the budget the bitrate is fixed, so the encoders can be
compared on quality alone: one bounded benchmark round (20 minutes) encodes the
search's own sample windows twice at the budget's video bitrate, once with
VideoToolbox in average-bitrate mode (the frozen hardware search's keyframe
interval and B-frames, Main10 for a 10-bit output) and once with the two-pass
x265 settings above, and measures both against the floor. VideoToolbox is used
when its sampled VMAF mean and p5 are each no more than 1 point below x265's;
otherwise x265 is. One point is a sixth of a just-noticeable difference (about
6 VMAF) and about what two encodes of the same windows differ by when they are
measured apart (about half a point on the mean, a point on the p5), so
VideoToolbox is taken only when nobody could see what it costs. It encodes a
film in minutes where two-pass x265 medium takes hours.

Both encodes get the same bitrate because that is what each budget encode
would get, so the comparison is between the files the budget would actually
produce. Their sample size estimates can still differ. VideoToolbox's rate
control often lands under its average on short windows of hard material and
still holds it over a whole film. In one such case the samples estimated about
four-fifths of x265's size, and the full VideoToolbox encode then came out at
x265's size. VideoToolbox's sampled numbers are then a lower bound, so the
difference only counts against it. Raising its bitrate from the sample estimate
would overshoot the full file, so the comparison does not do that. The
worker's own ranking of the two encodes (it may name the smaller one a
"winner") plays no part; only the tolerance rule picks.

The comparison is optional. With `allow_hardware=false` no comparison is
frozen and the budget encode is x265, as it is for a budget frozen before the
comparison existed (its digest is unchanged). A worker without VideoToolbox, a
round the worker refuses, fails, cancels or runs out of time, and a round
missing either measurement all leave the budget encode x265, and the job says
which. The round runs on the worker's existing explicit benchmark path, so it
needs no new worker capability.

VideoToolbox has no second pass, so it can miss its average over a whole file.
A VideoToolbox budget encode that comes out over the budget is encoded once
more, at its bitrate scaled by how far the video overshot
(`floor(kbps * (target - non-video) / (result - non-video))`), under a new job
identity. A second overshoot is reported over the budget like any other.
Validation rebuilds the VideoToolbox plan from the frozen policy, the source and
the comparison's recorded numbers (recomputing the choice) and, after a
correction, the bitrate the recorded first size gives; it accepts only an exact
match.

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
  when the finished file is still over the budget. Reason codes name the failed
  metric and its limit.

A budget candidate is never promoted automatically: like every candidate it
waits for review, and the label travels with it into the replacement review.
The batch approval itself also says how many of its candidates are below the
quality target, wherever it is answered.

**Seeing it.** Job detail shows the budget against the result and the trigger,
an inline rate-quality chart (size across, VMAF mean up, the budget as a
vertical line, the quality target and floor as horizontal lines, each searched
setting as a dot, the two encodes compared at the budget as rings, the chosen
one filled, a VideoToolbox encode that overshot as a dashed ring, and the budget
encode highlighted), the encoder the budget used and why, and a collapsed list
with the same numbers for browsers without SVG support. `action_status` carries the
same data as `size_budget`, and the compact benchmark summary reports
`plan_source=size_budget` with outcome `size_budget`. For a sibling-matched
budget the job reads "Budget from N sibling episodes (median X Mbps × M)", shows
how the source's bitrate compares with the median, and lists the files used
under a collapsed *Sibling episodes* section. *Change goal*, a batch setup and
*Run again* carry the budget: the UI offers *None*, *Percent of source*,
*Gigabytes* and *Match sibling episodes*, and shows the budget a file was frozen
with. The `transcode_goal_change` preview shows it too (`size_budget`,
`size_budget_mixed`); a change to size without `size_budget_*` inputs drops
it.

**Not done.** A search that ended because it hit the time cap does not trigger
the fallback. A CRF winner judged within budget on its estimate is not
re-checked after the full encode. The floor is frozen, not user-tunable. The
first, over-budget VideoToolbox candidate stays in the candidates folder beside
the corrected one until it is cleaned up. A VideoToolbox budget encode that
misses the floor is reported below the floor; it is not re-encoded with x265.
The comparison measures sample windows, which may not show how VideoToolbox's
rate control spends bits across a whole film; the correction covers the size
side of that, the final validation the quality side.

## Compact mode

`mode=compact` (optional `max_height` 720, 1080 (default), 1440 or 2160) accepts
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
  and measurement space are recorded in benchmark and final evidence, and the
  final output must pass an SDR signalling check (stream and decoded frames).
* Workers advertise `compact_transcode` only after executing the chain on a
  synthetic frame; the server refuses compact work without it. Replacement still
  needs normal approval.
* Re-grab risk: Sonarr or Radarr may see a compact file as below the quality
  profile's cutoff and download the original release again. Every compact
  approval (single file and batch) carries a warning and, when the promotion
  knows the library item, a read-only snapshot of its monitored state and
  quality profile. Unmonitor the item or move it to a suitable profile before
  approving; Navigatorr never changes these settings.
