package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// The deployed server freezes the capability fingerprint into snapshots and
// compares it later, so the digest of an unchanged machine must not move when
// capabilities are added, removed or reworded in code. The search-only fields
// stay in the payload with their values until the fingerprint is versioned.
const (
	pinnedFixtureFingerprint = "sha256:304b937263ab7913e7f79e56d4e1236cdbdd88de7eacf5f52d2a5667d1606b33"
	pinnedProbeFingerprint   = "sha256:a5579bcc3ddd707f18e30ec75bdbe562dcb813a2e6a1a5facab4dae20c0d8f87"
)

func fingerprintFixture() transcode.WorkerCapabilities {
	return transcode.WorkerCapabilities{
		X265TwoPass:               true,
		HEAACAudio:                true,
		AutomaticModesV2:          true,
		AutomaticQualityPruning:   true,
		SavingsPrecheck:           true,
		ExplicitMain10Benchmarks:  true,
		CompactAudio:              true,
		CoverArt:                  true,
		SizeModeV3:                true,
		SizeAudioOnly:             true,
		SearchBudgetExtension:     true,
		AutomaticSearchRules:      true,
		AutomaticGridVerification: true,
		AutomaticQualityWalk:      true,
		HardwareSearch:            true,
		HardwareFallbackSearch:    true,
		HardwareSelectionMargin:   true,
		SearchContinuation:        true,
		CompactTranscode:          true,
		ProtocolVersion:           transcode.WorkerProtocolVersion,
		WorkerVersion:             "1.0.0",
		BuildGitCommit:            "abcdef0",
		FFmpegVersion:             "7.1",
		FFmpegPath:                "/path/to/ffmpeg",
		Encoders:                  map[string]bool{"hevc_videotoolbox": true, "libx265": true, "aac": true},
		EncoderDetails: map[string]transcode.EncoderCapabilities{
			"hevc_videotoolbox": {
				Encoder:      "hevc_videotoolbox",
				Available:    true,
				Profiles:     []string{"main10", "main"},
				PixelFormats: []string{"p010le", "nv12"},
				Options:      []string{"spatial_aq", "profile"},
			},
		},
		Filters: map[string]bool{"scale": true, "libvmaf": true},
		Quality: &transcode.QualityCapabilities{
			Native10Bit:           true,
			LibvmafVersion:        "3.0.0",
			Models:                map[string]transcode.QualityModelCapability{"vmaf_v0.6.1": {Available: true, MeasurementBitDepth: 10, ScoreMin: 0, ScoreMax: 100}},
			CAMBIFullRef:          true,
			HFRTemporalDecimation: 2,
		},
	}
}

func TestCapabilityFingerprintPinnedForFixture(t *testing.T) {
	fp, err := transcode.ComputeCapabilityFingerprint(fingerprintFixture())
	if err != nil {
		t.Fatal(err)
	}
	if fp != pinnedFixtureFingerprint {
		t.Fatalf("fixture fingerprint moved: got %s, want %s", fp, pinnedFixtureFingerprint)
	}
}

func TestProbedCapabilitiesKeepFingerprintAndSearchFlags(t *testing.T) {
	dir := t.TempDir()
	fakeFFmpeg := filepath.Join(dir, "fake_ffmpeg.sh")
	script := `#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers"
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF2'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
EOF2
    ;;
  *"-encoders"*)
    cat << 'EOF2'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 A..... aac                  AAC
EOF2
    ;;
  *"-filters"*)
    cat << 'EOF2'
Filters:
 ... scale             V->V       Scale the input video
 ... format            V->V       Convert pixel format
EOF2
    ;;
  *)
    exit 1
    ;;
esac
`
	if err := os.WriteFile(fakeFFmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	SetBuildMetadata("1.0.0", "abcdef0")
	defer SetBuildMetadata("", "")

	caps, err := ProbeWorkerCapabilities(context.Background(), fakeFFmpeg)
	if err != nil {
		t.Fatal(err)
	}
	if caps.CapabilityFingerprint != pinnedProbeFingerprint {
		t.Fatalf("probed fingerprint moved: got %s, want %s", caps.CapabilityFingerprint, pinnedProbeFingerprint)
	}
	// The payload keeps the search-only fields at their old values.
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"explicit_main10_benchmarks", "automatic_modes_v2", "automatic_quality_pruning", "savings_precheck",
		"search_budget_extension", "automatic_search_rules", "automatic_grid_verification", "automatic_quality_walk",
		"hardware_search", "hardware_fallback_search", "hardware_selection_margin", "search_continuation",
	} {
		if !strings.Contains(string(raw), `"`+key+`":true`) {
			t.Errorf("capabilities payload lost %s", key)
		}
	}
}
