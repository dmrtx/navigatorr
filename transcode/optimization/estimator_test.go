package optimization

import (
	"math"
	"testing"
)

func TestEstimateOutput_SampleVideoBytes(t *testing.T) {
	// 60s sample encoded video is 10 MB (10,000,000 bytes).
	// Total video duration is 3600s (60x multiplier for video).
	// Audio stream is 200 MB copied (does NOT scale with sample duration!).
	// Subtitle stream is 2 MB copied.
	// Attachments are 1 MB copied.
	in := OutputEstimateInput{
		SourceSizeBytes:       10 * 1024 * 1024 * 1024, // 10 GB source
		TotalDurationSeconds:  3600.0,
		SampleDurationSeconds: 60.0,
		SampleVideoBytes:      10000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "aac", Channels: 2, SizeBytes: 200 * 1024 * 1024, Copied: true},
		},
		SubtitleStreams: []SubtitleStreamEstimate{
			{Index: 2, Codec: "subrip", SizeBytes: 2 * 1024 * 1024},
		},
		AttachmentBytes:       1 * 1024 * 1024,
		ContainerOverheadRate: 0.005, // 0.5%
	}

	res := EstimateOutput(in)

	// Video should scale 60x: 10,000,000 * 60 = 600,000,000 bytes
	expectedVideo := int64(600000000)
	if res.EstimatedVideoBytes != expectedVideo {
		t.Errorf("EstimatedVideoBytes = %d, want %d", res.EstimatedVideoBytes, expectedVideo)
	}

	// Audio must NOT scale: 200 MB
	expectedAudio := int64(200 * 1024 * 1024)
	if res.EstimatedAudioBytes != expectedAudio {
		t.Errorf("EstimatedAudioBytes = %d, want %d", res.EstimatedAudioBytes, expectedAudio)
	}

	// Subtitles must NOT scale: 2 MB
	expectedSub := int64(2 * 1024 * 1024)
	if res.EstimatedSubtitleBytes != expectedSub {
		t.Errorf("EstimatedSubtitleBytes = %d, want %d", res.EstimatedSubtitleBytes, expectedSub)
	}

	// Attachments: 1 MB
	expectedAtt := int64(1 * 1024 * 1024)
	if res.EstimatedAttachmentBytes != expectedAtt {
		t.Errorf("EstimatedAttachmentBytes = %d, want %d", res.EstimatedAttachmentBytes, expectedAtt)
	}

	payload := expectedVideo + expectedAudio + expectedSub + expectedAtt
	expectedMux := int64(math.Round(float64(payload) * 0.005))
	if res.EstimatedMuxOverheadBytes != expectedMux {
		t.Errorf("EstimatedMuxOverheadBytes = %d, want %d", res.EstimatedMuxOverheadBytes, expectedMux)
	}

	expectedTotal := payload + expectedMux
	if res.EstimatedTotalBytes != expectedTotal {
		t.Errorf("EstimatedTotalBytes = %d, want %d", res.EstimatedTotalBytes, expectedTotal)
	}

	if res.SavingsBytes != in.SourceSizeBytes-expectedTotal {
		t.Errorf("SavingsBytes = %d, want %d", res.SavingsBytes, in.SourceSizeBytes-expectedTotal)
	}

	if res.SavingsPercent <= 0 {
		t.Errorf("expected positive savings percent, got %f", res.SavingsPercent)
	}

	if len(res.Uncertainties) > 0 {
		t.Errorf("expected 0 uncertainties with exact inputs, got: %v", res.Uncertainties)
	}
}

func TestEstimateOutput_DeclaredBitrateFallback(t *testing.T) {
	// Sample bytes missing, but declared bitrate = 2,000,000 bps (2 Mbps).
	// Total duration = 1000s.
	// Expected video = 2,000,000 * 1000 / 8 = 250,000,000 bytes.
	in := OutputEstimateInput{
		SourceSizeBytes:         500000000,
		TotalDurationSeconds:    1000.0,
		DeclaredVideoBitrateBps: 2000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "ac3", Channels: 6, BitrateBps: 384000, Copied: true},
		},
	}

	res := EstimateOutput(in)

	expectedVideo := int64(250000000)
	if res.EstimatedVideoBytes != expectedVideo {
		t.Errorf("EstimatedVideoBytes = %d, want %d", res.EstimatedVideoBytes, expectedVideo)
	}

	// Audio: 384,000 * 1000 / 8 = 48,000,000 bytes
	expectedAudio := int64(48000000)
	if res.EstimatedAudioBytes != expectedAudio {
		t.Errorf("EstimatedAudioBytes = %d, want %d", res.EstimatedAudioBytes, expectedAudio)
	}

	hasVideoFallback := false
	hasAudioFallback := false
	for _, u := range res.Uncertainties {
		if u == ReasonVideoBitrateFallback {
			hasVideoFallback = true
		}
		if u == ReasonAudioBitrateFallback+":stream_1" {
			hasAudioFallback = true
		}
	}
	if !hasVideoFallback {
		t.Errorf("expected uncertainty %q in %v", ReasonVideoBitrateFallback, res.Uncertainties)
	}
	if !hasAudioFallback {
		t.Errorf("expected audio bitrate uncertainty in %v", res.Uncertainties)
	}
}

func TestEstimateOutput_HeuristicFallbacks(t *testing.T) {
	in := OutputEstimateInput{
		SourceSizeBytes:       100000000,
		TotalDurationSeconds:  100.0,
		SampleVideoBytes:      1000000,
		SampleDurationSeconds: 10.0,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "aac", Channels: 2, Copied: true}, // stereo AAC heuristic: 192kbps
		},
		SubtitleStreams: []SubtitleStreamEstimate{
			{Index: 2, Codec: "hdmv_pgs_subtitle"}, // bitmap heuristic: 20MB
			{Index: 3, Codec: "subrip"},            // text heuristic: 100KB
		},
	}

	res := EstimateOutput(in)

	// Stereo AAC 192kbps * 100s / 8 = 2,400,000 bytes
	expectedAudio := int64(192000 * 100 / 8)
	if res.EstimatedAudioBytes != expectedAudio {
		t.Errorf("EstimatedAudioBytes = %d, want %d", res.EstimatedAudioBytes, expectedAudio)
	}

	// PGS (20MB) + Subrip (100KB)
	expectedSub := int64(20*1024*1024 + 100*1024)
	if res.EstimatedSubtitleBytes != expectedSub {
		t.Errorf("EstimatedSubtitleBytes = %d, want %d", res.EstimatedSubtitleBytes, expectedSub)
	}

	foundAudioHeuristic := false
	foundPGSHeuristic := false
	foundSRTHeuristic := false
	for _, u := range res.Uncertainties {
		if u == ReasonAudioHeuristicFallback+":stream_1" {
			foundAudioHeuristic = true
		}
		if u == ReasonSubtitleSizeEstimated+":stream_2" {
			foundPGSHeuristic = true
		}
		if u == ReasonSubtitleSizeEstimated+":stream_3" {
			foundSRTHeuristic = true
		}
	}
	if !foundAudioHeuristic || !foundPGSHeuristic || !foundSRTHeuristic {
		t.Errorf("missing expected heuristic uncertainties in %v", res.Uncertainties)
	}
}

func TestEstimateOutput_MissingSourceSize(t *testing.T) {
	in := OutputEstimateInput{
		SourceSizeBytes:       0,
		TotalDurationSeconds:  60.0,
		SampleDurationSeconds: 60.0,
		SampleVideoBytes:      5000000,
	}

	res := EstimateOutput(in)
	foundMissingSource := false
	for _, u := range res.Uncertainties {
		if u == ReasonMissingSourceSize {
			foundMissingSource = true
		}
	}
	if !foundMissingSource {
		t.Errorf("expected %q uncertainty in %v", ReasonMissingSourceSize, res.Uncertainties)
	}
}
