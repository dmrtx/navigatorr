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
settings. If measurements fail, it tests one appropriate x265 slow family:
16/18/20 for quality rejection, or 23/26/28 when measured quality can pass but
size cannot. The alternative also applies after an initial verification failure.
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
