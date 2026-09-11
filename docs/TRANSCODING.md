# SSH transcoding

Navigatorr coordinates a remote Apple Silicon worker over system OpenSSH using the dedicated `navigatorr-transcode` worker binary. Keep deployment-specific hostnames, usernames, keys, and storage paths outside the public repository.

## Configuration

```yaml
transcode:
  enabled: true
  executor: ssh
  default_action: manual_approval
  default_profile: hevc-vt-balanced
  min_savings_percent: 15.0
  max_parallel_jobs: 1

  # Declarative, strictly validated transcode profiles.
  # Arbitrary FFmpeg arguments, shell snippets, or raw flags are prohibited.
  profiles:
    hevc-vt-balanced:
      container: mkv
      video:
        codec: hevc_videotoolbox
        quality: 65
      audio:
        mode: copy
      subtitles:
        mode: preserve
        convert_incompatible: true
      preserve:
        metadata: true
        chapters: true
        attachments: true

    hevc-vt-quality:
      container: mkv
      video:
        codec: hevc_videotoolbox
        quality: 55
      audio:
        mode: copy
      subtitles:
        mode: preserve
        convert_incompatible: true
      preserve:
        metadata: true
        chapters: true
        attachments: true

    hevc-vt-space:
      container: mkv
      video:
        codec: hevc_videotoolbox
        quality: 75
      audio:
        mode: copy
      subtitles:
        mode: preserve
        convert_incompatible: true
      preserve:
        metadata: true
        chapters: true
        attachments: true

  ssh:
    host: "192.0.2.10"
    port: 22
    user: "transcoder"
    ssh_key_path: "/run/secrets/navigatorr_transcode_ssh"
    known_hosts_path: "/run/secrets/navigatorr_known_hosts"
    remote_binary: "/opt/homebrew/bin/navigatorr-transcode"
    connect_timeout_sec: 5
    command_timeout_sec: 60
    path_mappings:
      - local_prefix: "/media"
        remote_prefix: "/Volumes/media"
```

> [!NOTE]
> `192.0.2.0/24` is the documentation-only TEST-NET-1 range (RFC 5737). Replace all example values in your private deployment configuration.

## Built-in Profiles & Backward Compatibility

If `profiles` is omitted from `config.yaml`, Navigatorr automatically provides built-in profiles:
- `hevc-vt` (legacy default, alias to `hevc-vt-balanced` with VideoToolbox quality `65`)
- `hevc-vt-balanced` (VideoToolbox quality `65`)
- `hevc-vt-quality` (VideoToolbox quality `55`)
- `hevc-vt-space` (VideoToolbox quality `75`)

Actions specifying `profile: "hevc-vt"` remain fully compatible without requiring changes to existing configuration.

## Safe Stream Planning & Container Compatibility

FFmpeg arguments are generated deterministically by the worker's stream planner from structured parameters:

- **No arbitrary arguments**: Custom FFmpeg flags or raw strings are rejected at configuration load and at the worker boundary (fail-closed model).
- **Subtitles in Matroska (MKV)**:
  - **Direct copy**: Supported Matroska subtitle formats (`subrip`/`srt`, `ass`, `ssa`, `hdmv_pgs_subtitle`, `dvd_subtitle`, `webvtt`) are copied directly without re-encoding.
  - **ASS/SSA preservation**: Stylized subtitles (`ass`/`ssa`) and embedded fonts/attachments are strictly preserved and never degraded to plain text SRT.
  - **Incompatible text subtitles**: Codecs incompatible with Matroska muxing (such as MP4 `mov_text`) are safely converted to `subrip` per stream index (e.g. `-c:s:0 subrip`), leaving adjacent stylized tracks untouched.
  - **Unrecognized codecs**: If a subtitle codec is neither supported nor safely convertible, transcode fails closed before execution to prevent data loss.
- **Audio**: Streams are copied directly (`-c:a copy`), preserving all channels, bitrates, and language metadata.
- **Candidate-only execution**: Transcoding runs exclusively in candidate mode (`<source-dir>/.navigatorr-candidates/<stem>.<job-id>.<ext>`). Original media is verified cryptographically via SHA-256 before and after and is never deleted or overwritten in this version.

