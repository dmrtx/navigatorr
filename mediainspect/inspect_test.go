package mediainspect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Heuristic mode (no ffprobe binary) still reports container, size,
// sidecars and dangerous names.
func TestHeuristicInspection(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Show.S01E01.1080p.mkv")
	os.WriteFile(media, []byte("fake-video-bytes"), 0o644)
	os.WriteFile(filepath.Join(dir, "Show.S01E01.1080p.eng.srt"), []byte("subs"), 0o644)

	rep, err := InspectFile(context.Background(), "/nonexistent/ffprobe", media)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if rep.Container != "mkv" {
		t.Errorf("container %q, want mkv", rep.Container)
	}
	if rep.SizeBytes == 0 {
		t.Error("size missing")
	}
	if len(rep.ExternalSubtitles) != 1 {
		t.Errorf("sidecars: %v", rep.ExternalSubtitles)
	}
	if rep.Probed {
		t.Error("probed should be false without ffprobe")
	}
}

func TestDangerousNameFlagged(t *testing.T) {
	dir := t.TempDir()
	evil := filepath.Join(dir, "Episode.1080p.mkv.exe")
	os.WriteFile(evil, []byte("x"), 0o644)
	rep, err := InspectFile(context.Background(), "/nonexistent/ffprobe", evil)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(rep.DangerousFiles) != 1 {
		t.Errorf("dangerous file not flagged: %+v", rep)
	}
}

func TestMissingFileErrors(t *testing.T) {
	if _, err := InspectFile(context.Background(), "/nonexistent/ffprobe",
		filepath.Join(t.TempDir(), "nope.mkv")); err == nil {
		t.Error("missing file should error")
	}
}

func TestParseBitDepth(t *testing.T) {
	tests := []struct {
		name     string
		rawBits  any
		pixFmt   string
		profile  string
		expected int
	}{
		{
			name:     "HEVC 10-bit with yuv420p10le without bits_per_raw_sample",
			rawBits:  nil,
			pixFmt:   "yuv420p10le",
			profile:  "Main 10",
			expected: 10,
		},
		{
			name:     "HEVC 10-bit with p010le without bits_per_raw_sample",
			rawBits:  nil,
			pixFmt:   "p010le",
			profile:  "Main 10",
			expected: 10,
		},
		{
			name:     "video 8-bit with raw string 8",
			rawBits:  "8",
			pixFmt:   "yuv420p",
			profile:  "High",
			expected: 8,
		},
		{
			name:     "video 8-bit without raw bits",
			rawBits:  nil,
			pixFmt:   "yuv420p",
			profile:  "High",
			expected: 8,
		},
		{
			name:     "video 10-bit with raw integer 10",
			rawBits:  10,
			pixFmt:   "",
			profile:  "",
			expected: 10,
		},
		{
			name:     "video 10-bit with raw string 10",
			rawBits:  "10",
			pixFmt:   "",
			profile:  "",
			expected: 10,
		},
		{
			name:     "video 12-bit with yuv420p12le",
			rawBits:  nil,
			pixFmt:   "yuv420p12le",
			profile:  "Main 12",
			expected: 12,
		},
		{
			name:     "unknown metadata returns 0",
			rawBits:  nil,
			pixFmt:   "",
			profile:  "",
			expected: 0,
		},
		{
			name:     "unknown format returns 0",
			rawBits:  nil,
			pixFmt:   "some_obscure_fmt",
			profile:  "unknown_profile",
			expected: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseBitDepth(tc.rawBits, tc.pixFmt, tc.profile)
			if got != tc.expected {
				t.Errorf("ParseBitDepth(%v, %q, %q) = %d; want %d",
					tc.rawBits, tc.pixFmt, tc.profile, got, tc.expected)
			}
		})
	}
}

func TestInspectFileRealWorldBitDepth(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "Fate.Strange.Fake.HEVC.10bit.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-video-payload"), 0o644)

	// Mock ffprobe returning Fate/Strange Fake HEVC 10-bit JSON
	mockFFprobe10Bit := filepath.Join(dir, "mock_ffprobe_10bit.sh")
	probe10BitJSON := `{
		"streams": [
			{
				"codec_type": "video",
				"codec_name": "hevc",
				"profile": "Main 10",
				"pix_fmt": "yuv420p10le",
				"width": 1920,
				"height": 1080
			}
		],
		"format": {
			"format_name": "matroska,webm",
			"duration": "1420.50"
		}
	}`
	script10Bit := "#!/bin/sh\ncat << 'EOF'\n" + probe10BitJSON + "\nEOF\n"
	if err := os.WriteFile(mockFFprobe10Bit, []byte(script10Bit), 0o755); err != nil {
		t.Fatal(err)
	}

	rep10, err := InspectFile(context.Background(), mockFFprobe10Bit, mediaFile)
	if err != nil {
		t.Fatalf("InspectFile failed: %v", err)
	}
	if !rep10.Probed {
		t.Fatalf("expected probed=true")
	}
	if rep10.VideoCodec != "hevc" {
		t.Errorf("expected video_codec hevc, got %s", rep10.VideoCodec)
	}
	if rep10.Resolution != "1080p" {
		t.Errorf("expected resolution 1080p, got %s", rep10.Resolution)
	}
	if rep10.BitDepth != 10 {
		t.Errorf("expected bit_depth 10 for HEVC 10-bit, got %d", rep10.BitDepth)
	}

	// Mock ffprobe returning 8-bit x264 JSON with string bits_per_raw_sample: "8"
	mockFFprobe8Bit := filepath.Join(dir, "mock_ffprobe_8bit.sh")
	probe8BitJSON := `{
		"streams": [
			{
				"codec_type": "video",
				"codec_name": "h264",
				"profile": "High",
				"pix_fmt": "yuv420p",
				"bits_per_raw_sample": "8",
				"width": 1920,
				"height": 1080
			}
		],
		"format": {
			"format_name": "matroska,webm",
			"duration": "1420.50"
		}
	}`
	script8Bit := "#!/bin/sh\ncat << 'EOF'\n" + probe8BitJSON + "\nEOF\n"
	if err := os.WriteFile(mockFFprobe8Bit, []byte(script8Bit), 0o755); err != nil {
		t.Fatal(err)
	}

	rep8, err := InspectFile(context.Background(), mockFFprobe8Bit, mediaFile)
	if err != nil {
		t.Fatalf("InspectFile failed: %v", err)
	}
	if rep8.BitDepth != 8 {
		t.Errorf("expected bit_depth 8 for H264 8-bit, got %d", rep8.BitDepth)
	}

	// Mock ffprobe with unknown metadata
	mockFFprobeUnknown := filepath.Join(dir, "mock_ffprobe_unknown.sh")
	probeUnknownJSON := `{
		"streams": [
			{
				"codec_type": "video",
				"codec_name": "custom_codec",
				"width": 1920,
				"height": 1080
			}
		],
		"format": {
			"format_name": "avi",
			"duration": "100.0"
		}
	}`
	scriptUnknown := "#!/bin/sh\ncat << 'EOF'\n" + probeUnknownJSON + "\nEOF\n"
	if err := os.WriteFile(mockFFprobeUnknown, []byte(scriptUnknown), 0o755); err != nil {
		t.Fatal(err)
	}

	repUnk, err := InspectFile(context.Background(), mockFFprobeUnknown, mediaFile)
	if err != nil {
		t.Fatalf("InspectFile failed: %v", err)
	}
	if repUnk.BitDepth != 0 {
		t.Errorf("expected bit_depth 0 for unknown metadata, got %d", repUnk.BitDepth)
	}
}
