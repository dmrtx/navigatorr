package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestBuildMetadata_DynamicAndDefault(t *testing.T) {
	// 1. Explicitly test setting dynamic build metadata
	SetBuildMetadata("2026.10.1", "deadbeef")
	ver, commit := GetBuildMetadata()
	if ver != "2026.10.1" {
		t.Errorf("expected version 2026.10.1, got %q", ver)
	}
	if commit != "deadbeef" {
		t.Errorf("expected commit deadbeef, got %q", commit)
	}

	// 2. Clear metadata; should fall back to debug info or "unknown"
	SetBuildMetadata("", "")
	ver2, commit2 := GetBuildMetadata()
	if ver2 == "" {
		t.Errorf("version should not be empty")
	}
	if commit2 == "" {
		t.Errorf("commit should not be empty")
	}
}

func TestCapabilityFingerprint_PathExcludedAndEquivalence(t *testing.T) {
	caps1 := transcode.WorkerCapabilities{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		WorkerVersion:   "1.0.0",
		BuildGitCommit:  "abcdef0",
		FFmpegVersion:   "7.1",
		FFmpegPath:      "/usr/bin/ffmpeg",
		Encoders:        map[string]bool{"hevc_videotoolbox": true, "libx264": true},
		Filters:         map[string]bool{"scale": true, "ssim": true},
		VideoToolbox: transcode.VideoToolboxCapabilities{
			Encoder:      "hevc_videotoolbox",
			Available:    true,
			Profiles:     []string{"main", "main10"},
			PixelFormats: []string{"nv12", "p010le"},
			Options:      []string{"profile", "spatial_aq"},
		},
	}

	caps2 := caps1
	caps2.FFmpegPath = "/opt/homebrew/bin/ffmpeg" // Different machine path

	fp1, err := transcode.ComputeCapabilityFingerprint(caps1)
	if err != nil {
		t.Fatalf("failed computing fp1: %v", err)
	}
	fp2, err := transcode.ComputeCapabilityFingerprint(caps2)
	if err != nil {
		t.Fatalf("failed computing fp2: %v", err)
	}

	if fp1 != fp2 {
		t.Errorf("capability fingerprint must exclude machine-specific FFmpegPath: %s != %s", fp1, fp2)
	}

	// Modifying actual capabilities must change fingerprint
	caps3 := caps1
	caps3.Encoders["libx265"] = true
	fp3, err := transcode.ComputeCapabilityFingerprint(caps3)
	if err != nil {
		t.Fatalf("failed computing fp3: %v", err)
	}
	if fp3 == fp1 {
		t.Errorf("expected different fingerprint when capabilities change")
	}
}

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

func TestProbeWorkerCapabilities_DeterministicFingerprintAndPartialAbsence(t *testing.T) {
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

	SetBuildMetadata("1.0.0", "abcdef0")
	defer SetBuildMetadata("", "")

	caps, err := ProbeWorkerCapabilities(context.Background(), fakeFFmpeg)
	if err != nil {
		t.Fatalf("ProbeWorkerCapabilities failed: %v", err)
	}

	if caps.ProtocolVersion != transcode.WorkerProtocolVersion {
		t.Errorf("expected ProtocolVersion=%d, got %d", transcode.WorkerProtocolVersion, caps.ProtocolVersion)
	}
	if caps.WorkerVersion != "1.0.0" {
		t.Errorf("expected WorkerVersion=1.0.0, got %s", caps.WorkerVersion)
	}
	if caps.BuildGitCommit != "abcdef0" {
		t.Errorf("expected BuildGitCommit=abcdef0, got %s", caps.BuildGitCommit)
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
	if !strings.HasPrefix(caps.CapabilityFingerprint, "sha256:") {
		t.Errorf("expected sha256 fingerprint, got %s", caps.CapabilityFingerprint)
	}
	if caps.HasProbeErrors() {
		t.Errorf("unexpected probe errors: %+v", caps.ProbeErrors)
	}

	// Verify deterministic fingerprint verification passes
	if err := transcode.VerifyCapabilityFingerprint(caps); err != nil {
		t.Errorf("VerifyCapabilityFingerprint failed: %v", err)
	}

	// Second probe produces identical deterministic fingerprint
	caps2, err := ProbeWorkerCapabilities(context.Background(), fakeFFmpeg)
	if err != nil {
		t.Fatalf("second probe failed: %v", err)
	}
	if caps.CapabilityFingerprint != caps2.CapabilityFingerprint {
		t.Errorf("fingerprints did not match: %s vs %s", caps.CapabilityFingerprint, caps2.CapabilityFingerprint)
	}
}

func TestProbeWorkerCapabilities_ProbeErrorsRecordedOnFailure(t *testing.T) {
	dir := t.TempDir()
	fakeFFmpeg := filepath.Join(dir, "fake_ffmpeg_err.sh")

	// Mock script where -encoders and -filters fail with non-zero exit and error text
	script := `#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1"
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: p010le
hevc_videotoolbox AVOptions:
  -profile <int>
     main10 2
EOF
    ;;
  *"-encoders"*)
    echo "probe encoders failed: internal driver fault" >&2
    exit 1
    ;;
  *"-filters"*)
    echo "probe filters failed: cannot load filter graph" >&2
    exit 2
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
		t.Fatalf("ProbeWorkerCapabilities should preserve partial results and not fail completely, got: %v", err)
	}

	if !caps.HasProbeErrors() {
		t.Errorf("expected probe errors to be recorded")
	}
	if !caps.HasComponentError("encoders") {
		t.Errorf("expected component error for encoders")
	}
	if !caps.HasComponentError("filters") {
		t.Errorf("expected component error for filters")
	}

	// Check that error messages are bounded
	for _, pe := range caps.ProbeErrors {
		if len(pe.Message) > 300 {
			t.Errorf("probe error message exceeded bounded length: %d chars", len(pe.Message))
		}
	}
}

func TestProbeWorkerCapabilities_CleanAbsenceOnNonApple(t *testing.T) {
	dir := t.TempDir()
	fakeFFmpeg := filepath.Join(dir, "fake_ffmpeg_linux.sh")

	// Standard non-Apple FFmpeg build output where hevc_videotoolbox is not recognized
	script := `#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1-static (Linux)"
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    echo "Codec 'hevc_videotoolbox' is not recognized by FFmpeg." >&2
    exit 1
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... libx264              libx264 H.264
 V..... libx265              libx265 H.265
 A..... aac                  AAC
EOF
    ;;
  *"-filters"*)
    cat << 'EOF'
Filters:
 ... scale             V->V       Scale
 ... ssim              VV->V      SSIM
EOF
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
		t.Fatalf("ProbeWorkerCapabilities failed on clean absence: %v", err)
	}

	// Clean absence must NOT be treated as a ProbeError
	if caps.HasProbeErrors() {
		t.Errorf("expected no probe errors for clean absence, got: %+v", caps.ProbeErrors)
	}
	if caps.VideoToolbox.Available {
		t.Errorf("expected VideoToolbox.Available=false")
	}
	if caps.EncoderDetails["hevc_videotoolbox"].Available {
		t.Errorf("expected EncoderDetails[hevc_videotoolbox].Available=false")
	}
	if caps.Encoders["hevc_videotoolbox"] {
		t.Errorf("expected Encoders[hevc_videotoolbox]=false")
	}
	if !caps.Encoders["libx264"] || !caps.Encoders["libx265"] {
		t.Errorf("expected standard software encoders to be available")
	}
	if !strings.HasPrefix(caps.CapabilityFingerprint, "sha256:") {
		t.Errorf("expected valid fingerprint digest, got %q", caps.CapabilityFingerprint)
	}
}
