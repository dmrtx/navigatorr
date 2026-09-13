# Typed VideoToolbox transcode controls

Navigatorr's transcode recipe schema deliberately does not accept arbitrary FFmpeg argument arrays. Video encoder behavior is expressed through explicit typed fields, resolved into the immutable transcode plan, included in `plan_digest`, persisted with the job, and translated to a deterministic FFmpeg argv by the worker.

## Supported `video` fields

For `codec: hevc_videotoolbox` the schema supports:

| Field | Type | Accepted values | Default when omitted | FFmpeg mapping |
| --- | --- | --- | --- | --- |
| `codec` | string | `hevc_videotoolbox` | required | `-c:v hevc_videotoolbox` |
| `quality` | integer | `1..100` | existing profile value | `-q:v N` |
| `profile` | string | `main`, `main10` | not forced | `-profile:v VALUE` |
| `pixel_format` | string | `yuv420p`, `p010le` | not forced | `-pix_fmt VALUE` |
| `prioritize_speed` | boolean | `true`, `false` | not forced | `-prio_speed 1/0` |
| `spatial_aq` | boolean | `true`, `false` | not forced | `-spatial_aq 1/0` |
| `realtime` | boolean | `true`, `false` | not forced | `-realtime 1/0` |

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
```

The rest of the generated command continues to use Navigatorr's explicit stream maps, copied audio/attachments, recipe-resolved subtitle codecs, metadata/chapter preservation, progress output, and candidate path.

## Bitrate controls

`bitrate`, `max_bitrate`, and `bufsize` are intentionally **not** accepted yet. FFmpeg exposes generic rate-control switches, but their exact interaction with the installed Apple VideoToolbox implementation must be verified before Navigatorr promises stable semantics. Until that verification exists, the strict recipe loader rejects these fields instead of silently translating them into an untested mode or accidentally forcing CBR.

## VideoToolbox versus x265

`hevc_videotoolbox` is a hardware encoder optimized for throughput and power efficiency. Software x265, especially 10-bit presets used by compact anime release groups, can spend far more CPU time per frame and expose substantially more psychovisual and rate-control tuning. These experimental profiles are therefore intended to find the best VideoToolbox speed/quality/size trade-off, not to reproduce an x265/Judas encode exactly.

## Batch compatibility

The profile is still propagated by name through `transcode_batch`; no batch selection semantics are changed here. The Sonarr batch API continues to use `season`, not `season_number`.

## Required real-hardware verification

Schema acceptance and unit tests are not proof that a particular Mac/FFmpeg build actually supports AQ or 10-bit VideoToolbox. Before promoting these profiles, run `navigatorr-transcode capabilities` on the configured Apple Silicon worker and perform candidate-only encodes with `replace_original=false`.

- **Main10**: Main10 is considered verified only when post-validation reports 10-bit output from `main10 + p010le`.
- **Spatial AQ**: Spatial AQ is physically validated only when encode completes without VideoToolbox unsupported/ignored AQ warning; the worker fails such warnings as `encoder_capability_unsupported`.

The M1 physical matrix has not been run yet; real-hardware matrix verification across Apple Silicon generations remains pending.
