# Typed VideoToolbox transcode controls

Navigatorr's transcode recipe schema deliberately does not accept arbitrary FFmpeg argument arrays. Video encoder behavior is expressed through explicit typed fields, resolved into the immutable transcode plan, included in `plan_digest`, persisted with the job, and translated to a deterministic FFmpeg argv by the worker.

## Supported `video` fields

For `codec: hevc_videotoolbox` the schema supports:

| Field | Type | Accepted values | Default when omitted | FFmpeg mapping |
| --- | --- | --- | --- | --- |
| `codec` | string | `hevc_videotoolbox` | required | `-c:v hevc_videotoolbox` |
| `quality` | integer | `1..100` | existing profile value | `-q:v N` |
| `average_bitrate_kbps` | integer | `1..1000000` | not forced | `-b:v Nk` (AverageBitRate) |
| `max_bitrate_kbps` | integer | `>= average`, `<= 1000000` | not forced | `-maxrate Nk` (DataRateLimits) |
| `constant_bitrate` | boolean | `true`, `false` | not forced (off) | `-constant_bit_rate 1/0` |
| `profile` | string | `main`, `main10` | not forced | `-profile:v VALUE` |
| `pixel_format` | string | `yuv420p`, `p010le` | not forced | `-pix_fmt VALUE` |
| `prioritize_speed` | boolean | `true`, `false` | not forced | `-prio_speed 1/0` |
| `spatial_aq` | boolean | `true`, `false` | not forced | `-spatial_aq 1/0` |
| `realtime` | boolean | `true`, `false` | not forced | `-realtime 1/0` |
| `qmin` / `qmax` | integer | `0..69`, `qmin <= qmax` | not forced | `-qmin N` / `-qmax N` (allowed frame QP) |
| `gop_size` | integer | `1..100000` | not forced | `-g N` (max keyframe interval) |
| `b_frames` | integer | `0` or `1` | not forced | `-bf 0` disables frame reordering/B-frames, `-bf 1` enables it (VideoToolbox chooses the actual reorder/B-frame depth internally — upstream reports 2 for HEVC — so this is an on/off switch, never a tunable depth) |
| `closed_gop` | boolean | `true`, `false` | not forced | `-flags +cgop` / `-flags -cgop` |
| `power_efficient` | boolean | `true`, `false` | not forced | `-power_efficient 1/0` |
| `max_ref_frames` | integer | `1..16` | not forced | `-max_ref_frames N` |

## Rate-control modes

Exactly one mode per recipe; `quality` and `average_bitrate_kbps` together
fail recipe validation, as does a profile whose rate mode disagrees with its
benchmark search dimension (`quality_values` vs `bitrate_values`):

- **Quality mode** (`quality: 65`): constant-quality `-q:v`. This is the
  default and the only mode the legacy profiles use.
- **Average-bitrate mode** (`average_bitrate_kbps: 3500` with `quality`
  unset): `-b:v 3500k` (AverageBitRate). Optional `constant_bitrate: true`
  adds `-constant_bit_rate 1` (never the default), and optional
  `max_bitrate_kbps` adds generic `-maxrate` capping at or above the average.
  Benchmark sweeps use `bitrate_values: [3200, 3500, 3800]` (see
  `live-action-hevc-vt`).

Integer knobs use pointer semantics like the booleans below: omitted means
"emit nothing", while an explicit value — including `b_frames: 0` — is
emitted verbatim and validated against the ranges above. Values above a
knob's documented maximum (e.g. `b_frames: 2`) fail recipe validation
instead of implying encoder granularity FFmpeg does not implement.

`max_ref_frames` only has an effect below the maximum allowed by the
profile/level (upstream encoder semantics); larger values are accepted by
the range check but do not force references past that ceiling.

Omitted fields stay absent from the immutable plan. This is intentional: legacy is `FFmpeg-argv compatible; no new encoder defaults are injected`, not fully behavior-compatible because capability probing occurs.

### Pointer-boolean semantics

The optional boolean switches (`prioritize_speed`, `spatial_aq`, `realtime`) use pointer-boolean semantics:
- **Omitted / `nil`**: omitted/nil means Navigatorr injects nothing into the FFmpeg command line and does not require that option to be supported by the worker's FFmpeg binary.
- **Explicit `false`**: explicit false pointer-bools are configured and require the corresponding FFmpeg option; emitting `0` in FFmpeg arguments (e.g. `-prio_speed 0`, `-spatial_aq 0`, `-realtime 0`).
- **Explicit `true`**: Configured typed value emitting `1` in FFmpeg arguments (e.g. `-prio_speed 1`, `-spatial_aq 1`, `-realtime 1`), and likewise requires that corresponding FFmpeg option to exist in the worker's FFmpeg encoder capabilities.

`ffmpeg_args`, `extra_args`, shell snippets, and similar escape hatches are not part of the schema and are rejected by the strict YAML decoder.

## Validation rules

The recipe loader rejects unknown values and invalid types before a job is submitted. `main10` requires `pixel_format: p010le`; conversely, `p010le` requires `profile: main10`. `main` with `p010le` is invalid. Main10 resolves to `expected_bit_depth: 10` in the immutable plan.

The worker's `ValidatePlan` performs early structural validation before persisting state or spawning a job process, ensuring that typed VideoToolbox settings (profile, pixel format, and expected bit depth consistency) are validated before external FFmpeg execution.

The remote worker adds a second, runtime capability boundary. Before starting FFmpeg it executes the equivalent of:

```text
ffmpeg -hide_banner -h encoder=hevc_videotoolbox
```

and parses the installed binary's reported profiles, pixel formats, and encoder options. A requested capability that is not reported fails with the classifiable prefix `encoder_capability_unsupported`; FFmpeg is not started and the original is never touched.

The worker CLI also exposes the parsed result directly:

```text
navigatorr-transcode capabilities
```

The worker CLI is process-per-command rather than a long-running daemon, so capability discovery is intentionally performed immediately before an encode instead of maintaining a stale cross-process cache. The probe is tiny compared with an encode and makes each job validate the exact FFmpeg binary it is about to execute.

## Main10 post-validation

A Main10 recipe does not merely request `main10` and `p010le`. The resolved plan carries `expected_bit_depth: 10`, and Navigatorr's candidate validation checks the first output video stream with ffprobe. An 8-bit result fails closed. Resolution is also compared with the original, in addition to the existing checks for codec, duration, audio streams, subtitle streams, attachments, chapters, and the original SHA-256 verification performed before acceptance.

Using Main10 on an 8-bit source does not create missing source detail and does not guarantee better quality or compression. It is an experiment worth measuring, not an automatic upgrade.

## Experimental comparison profiles

The current `anime-hevc` profile is intentionally unchanged. The additional profiles are isolated comparison presets:

- `anime-hevc-balanced`: quality 65, Main/yuv420p, quality-over-speed, AQ off, realtime off.
- `anime-hevc-balanced-aq`: same as balanced with spatial AQ on.
- `anime-hevc-quality`: quality 75, Main/yuv420p, spatial AQ on.
- `anime-hevc-space`: quality 55, Main/yuv420p, AQ off.
- `anime-hevc-main10`: quality 65, Main10/p010le, AQ off.
- `anime-hevc-main10-aq`: quality 65, Main10/p010le, AQ on.

Example:

```yaml
profiles:
  anime-hevc-balanced:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
      profile: main
      pixel_format: yuv420p
      prioritize_speed: false
      spatial_aq: false
      realtime: false
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}

  anime-hevc-main10-aq:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
      profile: main10
      pixel_format: p010le
      prioritize_speed: false
      spatial_aq: true
      realtime: false
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
```

For these presets the deterministic video portions of argv are:

```text
anime-hevc-balanced
-c:v hevc_videotoolbox -q:v 65 -profile:v main -pix_fmt yuv420p -prio_speed 0 -spatial_aq 0 -realtime 0

anime-hevc-balanced-aq
-c:v hevc_videotoolbox -q:v 65 -profile:v main -pix_fmt yuv420p -prio_speed 0 -spatial_aq 1 -realtime 0

anime-hevc-quality
-c:v hevc_videotoolbox -q:v 75 -profile:v main -pix_fmt yuv420p -prio_speed 0 -spatial_aq 1 -realtime 0

anime-hevc-space
-c:v hevc_videotoolbox -q:v 55 -profile:v main -pix_fmt yuv420p -prio_speed 0 -spatial_aq 0 -realtime 0

anime-hevc-main10
-c:v hevc_videotoolbox -q:v 65 -profile:v main10 -pix_fmt p010le -prio_speed 0 -spatial_aq 0 -realtime 0

anime-hevc-main10-aq
-c:v hevc_videotoolbox -q:v 65 -profile:v main10 -pix_fmt p010le -prio_speed 0 -spatial_aq 1 -realtime 0

live-action-hevc-vt (average-bitrate mode)
-c:v hevc_videotoolbox -b:v 3500k -profile:v main -pix_fmt yuv420p -prio_speed 0 -spatial_aq 1 -realtime 0
```

New options always append after the legacy `-realtime` slot in fixed order
(`-qmin`, `-qmax`, `-g`, `-bf`, `-flags`, `-power_efficient`,
`-max_ref_frames`, `-constant_bit_rate`, `-maxrate`), so legacy argv stays
byte-identical.

The rest of the generated command continues to use Navigatorr's explicit stream maps, copied audio/attachments, recipe-resolved subtitle codecs, metadata/chapter preservation, progress output, and candidate path.

## Bitrate controls

Rate control is typed, never passthrough: `average_bitrate_kbps` (`-b:v`),
`max_bitrate_kbps` (`-maxrate`), and `constant_bitrate`
(`-constant_bit_rate`) are the only accepted spellings. The bare legacy
names `bitrate`, `max_bitrate`, and especially `bufsize` stay rejected by the
strict recipe loader: the current FFmpeg VideoToolbox encoder does not
consume `bufsize` meaningfully, so Navigatorr refuses to promise semantics
for it. CBR is opt-in per recipe and never the default.

## Encoder-family boundaries

Recipes select exactly one encoder family, and each family accepts only its
own typed knobs — validated fail-closed at recipe parse, plan build, and
worker capability negotiation:

- `hevc_videotoolbox` accepts everything in the table above and rejects
  `preset` (libx265-only).
- `libx265` accepts `quality` (CRF 1..51, lower = better) plus `preset` from
  the fixed safe enum, and rejects every VideoToolbox-only knob
  (`average_bitrate_kbps`, `max_bitrate_kbps`, `constant_bitrate`,
  `qmin`/`qmax`, `gop_size`, `b_frames`, `closed_gop`, `power_efficient`,
  `max_ref_frames`, `prioritize_speed`, `spatial_aq`, `realtime`).

Deliberately absent from both families (rejected by strict decoding):
`require_sw` / `allow_sw` software fallback, `alpha_quality`,
`frames_before` / `frames_after`, and `low_delay`. Hardware acceleration
remains required for `hevc_videotoolbox`.

## VideoToolbox versus x265

`hevc_videotoolbox` is a hardware encoder optimized for throughput and power efficiency. Software x265, especially 10-bit presets used by compact anime release groups, can spend far more CPU time per frame and expose substantially more psychovisual and rate-control tuning. These experimental profiles are therefore intended to find the best VideoToolbox speed/quality/size trade-off, not to reproduce an x265/Judas encode exactly.

## Batch compatibility

The profile is still propagated by name through `transcode_batch`; no batch selection semantics are changed here. The Sonarr batch API continues to use `season`, not `season_number`.

## Required real-hardware verification

Schema acceptance and unit tests are not proof that a particular Mac/FFmpeg build actually supports AQ or 10-bit VideoToolbox. Before promoting these profiles, run `navigatorr-transcode capabilities` on the configured Apple Silicon worker and perform candidate-only encodes with `replace_original=false`.

- **Main10**: Main10 is considered verified only when post-validation reports 10-bit output from `main10 + p010le`.
- **Spatial AQ**: Spatial AQ is physically validated only when encode completes without VideoToolbox unsupported/ignored AQ warning; the worker fails such warnings as `encoder_capability_unsupported`.
- **New typed options** (`constant_bit_rate`, `power_efficient`, `max_ref_frames`): requested options must appear in the worker's `ffmpeg -h encoder=hevc_videotoolbox` probe or the job fails closed before any encode starts. Generic controls (`-b:v`, `-maxrate`, `-qmin`/`-qmax`, `-g`, `-bf`, `-flags`) are core FFmpeg options validated structurally instead.

The M1 physical matrix has not been run yet; real-hardware matrix verification across Apple Silicon generations remains pending.
