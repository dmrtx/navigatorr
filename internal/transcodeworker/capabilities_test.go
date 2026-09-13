package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestParseFFmpegVersion(t *testing.T) {
	raw := `ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers
built with Apple clang version 16.0.0 (clang-1600.0.26.4)
configuration: --prefix=/opt/homebrew/Cellar/ffmpeg/7.1`
	ver := ParseFFmpegVersion(raw)
	if ver != "7.1" {
		t.Errorf("expected 7.1, got %q", ver)
	}

	fallback := ParseFFmpegVersion("custom build line")
	if fallback != "unknown" {
		t.Errorf("expected unknown for malformed output, got %q", fallback)
	}
}

func TestParseAvailableEncoders_PartialAbsence(t *testing.T) {
	rawEncoders := `Encoders:
 V..... = Video
 A..... = Audio
 S..... = Subtitle
 ------
 V..... hevc_videotoolbox    VideoToolbox H.265 Encoder (codec hevc)
 V..... h264_videotoolbox    VideoToolbox H.264 Encoder (codec h264)
 V..... libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 A..... aac                  AAC (Advanced Audio Coding)
`
	m := ParseAvailableEncoders(rawEncoders)
	if !m["hevc_videotoolbox"] {
		t.Errorf("expected hevc_videotoolbox=true")
	}
	if !m["h264_videotoolbox"] {
		t.Errorf("expected h264_videotoolbox=true")
	}
	if !m["libx264"] {
		t.Errorf("expected libx264=true")
	}
	if m["libx265"] {
		t.Errorf("expected libx265=false for partial absence")
	}
	if m["prores_videotoolbox"] {
		t.Errorf("expected prores_videotoolbox=false for partial absence")
	}
}

func TestParseAvailableFilters_PartialAbsence(t *testing.T) {
	rawFilters := `Filters:
  ... = Source flag
  .T. = Timeline support
  .S. = Slice threading
  ... = Command support
  ---
 ... scale             V->V       Scale the input video size and/or convert the image format.
 ... format            V->V       Convert the input video to one of the specified pixel formats.
 ... ssim              VV->V      Calculate the SSIM between two video streams.
 ... null              N->N       Pass the source unchanged to the output.
`
	f := ParseAvailableFilters(rawFilters)
	if !f["scale"] {
		t.Errorf("expected scale=true")
	}
	if !f["format"] {
		t.Errorf("expected format=true")
	}
	if !f["ssim"] {
		t.Errorf("expected ssim=true")
	}
	if f["libvmaf"] {
		t.Errorf("expected libvmaf=false when missing from build (partial absence represented)")
	}
}

func TestProbeWorkerCapabilities_DeterministicSignatureAndPartialAbsence(t *testing.T) {
	dir := t.TempDir()
	fakeFFmpeg := filepath.Join(dir, "fake_ffmpeg.sh")

	script := `#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers"
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... h264_videotoolbox    VideoToolbox H.264
 A..... aac                  AAC
EOF
    ;;
  *"-filters"*)
    cat << 'EOF'
Filters:
 ... scale             V->V       Scale the input video
 ... format            V->V       Convert pixel format
 ... ssim              VV->V      SSIM score
EOF
    ;;
  *)
    echo "unknown args: $*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(fakeFFmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("failed writing fake ffmpeg: %v", err)
	}

	caps, err := ProbeWorkerCapabilities(context.Background(), fakeFFmpeg)
	if err != nil {
		t.Fatalf("ProbeWorkerCapabilities failed: %v", err)
	}

	if caps.ProtocolVersion != transcode.WorkerProtocolVersion {
		t.Errorf("expected ProtocolVersion=%d, got %d", transcode.WorkerProtocolVersion, caps.ProtocolVersion)
	}
	if caps.FFmpegVersion != "7.1" {
		t.Errorf("expected FFmpegVersion=7.1, got %s", caps.FFmpegVersion)
	}
	if !caps.VideoToolbox.Available {
		t.Errorf("expected VideoToolbox.Available=true")
	}
	if !caps.Encoders["hevc_videotoolbox"] {
		t.Errorf("expected Encoders[hevc_videotoolbox]=true")
	}
	if caps.Encoders["libx265"] {
		t.Errorf("expected libx265=false")
	}
	if !caps.Filters["scale"] || !caps.Filters["ssim"] {
		t.Errorf("expected scale and ssim to be true")
	}
	if caps.Filters["libvmaf"] {
		t.Errorf("expected libvmaf=false (partial absence represented)")
	}
	if !strings.HasPrefix(caps.Signature, "sha256:") {
		t.Errorf("expected sha256 signature, got %s", caps.Signature)
	}

	// Verify deterministic signature verification passes
	if err := transcode.VerifyCapabilitySignature(caps); err != nil {
		t.Errorf("VerifyCapabilitySignature failed: %v", err)
	}

	// Second probe produces identical deterministic signature
	caps2, err := ProbeWorkerCapabilities(context.Background(), fakeFFmpeg)
	if err != nil {
		t.Fatalf("second probe failed: %v", err)
	}
	if caps.Signature != caps2.Signature {
		t.Errorf("signatures did not match: %s vs %s", caps.Signature, caps2.Signature)
	}
}

func TestProbeWorkerCapabilities_VideoToolboxMissingDoesNotFailEntireReport(t *testing.T) {
	dir := t.TempDir()
	fakeFFmpeg := filepath.Join(dir, "fake_ffmpeg_no_vt.sh")

	script := `#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 6.1"
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    echo "Codec 'hevc_videotoolbox' is not recognized by FFmpeg." >&2
    exit 1
    ;;
  *"-encoders"*)
    echo " V..... libx264    libx264"
    ;;
  *"-filters"*)
    echo " ... scale        V->V    Scale video"
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeFFmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("failed writing fake ffmpeg: %v", err)
	}

	caps, err := ProbeWorkerCapabilities(context.Background(), fakeFFmpeg)
	if err != nil {
		t.Fatalf("expected ProbeWorkerCapabilities to succeed despite missing VideoToolbox, got: %v", err)
	}
	if caps.VideoToolbox.Available {
		t.Errorf("expected VideoToolbox.Available=false")
	}
	if caps.Encoders["hevc_videotoolbox"] {
		t.Errorf("expected Encoders[hevc_videotoolbox]=false")
	}
	if !caps.Encoders["libx264"] {
		t.Errorf("expected Encoders[libx264]=true")
	}
	if err := transcode.VerifyCapabilitySignature(caps); err != nil {
		t.Errorf("signature verification failed: %v", err)
	}
}
