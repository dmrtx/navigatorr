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
		SourceSizeBytes:       10 * 1000 * 1000 * 1000, // 10 GB source (decimal)
		TotalDurationSeconds:  3600.0,
		SampleDurationSeconds: 60.0,
		SampleVideoBytes:      10000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "aac", Channels: 2, SizeBytes: 200 * 1000 * 1000, Copied: true},
		},
		SubtitleStreams: []SubtitleStreamEstimate{
			{Index: 2, Codec: "subrip", SizeBytes: 2 * 1000 * 1000},
		},
		AttachmentBytes:       1 * 1000 * 1000,
		ContainerOverheadRate: 0.005, // 0.5%
	}

	res, err := EstimateOutput(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.SuitableForSelection {
		t.Fatalf("expected SuitableForSelection = true, got unusable: %s", res.UnusableReason)
	}

	// Video should scale 60x: 10,000,000 * 60 = 600,000,000 bytes
	expectedVideo := int64(600000000)
	if res.EstimatedVideoBytes != expectedVideo {
		t.Errorf("EstimatedVideoBytes = %d, want %d", res.EstimatedVideoBytes, expectedVideo)
	}

	// Audio must NOT scale: 200 MB
	expectedAudio := int64(200 * 1000 * 1000)
	if res.EstimatedAudioBytes != expectedAudio {
		t.Errorf("EstimatedAudioBytes = %d, want %d", res.EstimatedAudioBytes, expectedAudio)
	}

	// Subtitles must NOT scale: 2 MB
	expectedSub := int64(2 * 1000 * 1000)
	if res.EstimatedSubtitleBytes != expectedSub {
		t.Errorf("EstimatedSubtitleBytes = %d, want %d", res.EstimatedSubtitleBytes, expectedSub)
	}

	// Attachments: 1 MB
	expectedAtt := int64(1 * 1000 * 1000)
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

	// Decimal MB check (1,000,000 bytes per MB)
	expectedMB := round2(float64(expectedTotal) / 1000000.0)
	if res.EstimatedTotalMB != expectedMB {
		t.Errorf("EstimatedTotalMB = %f, want %f (decimal MB)", res.EstimatedTotalMB, expectedMB)
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

func TestEstimateOutput_HonorCopiedAudioOnly(t *testing.T) {
	// Audio stream 1 is Copied: false (should NOT be included in copied audio)
	// Audio stream 2 is Copied: true (should be included)
	in := OutputEstimateInput{
		SourceSizeBytes:       1000000000,
		TotalDurationSeconds:  1000.0,
		SampleDurationSeconds: 10.0,
		SampleVideoBytes:      1000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "dts", SizeBytes: 150000000, Copied: false}, // NOT copied
			{Index: 2, Codec: "aac", SizeBytes: 50000000, Copied: true},   // copied
		},
	}

	res, err := EstimateOutput(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only Stream 2 (50MB) should be counted; Stream 1 (150MB) must be ignored
	expectedAudio := int64(50000000)
	if res.EstimatedAudioBytes != expectedAudio {
		t.Errorf("EstimatedAudioBytes = %d, want %d (non-copied audio must be excluded)", res.EstimatedAudioBytes, expectedAudio)
	}
}

func TestEstimateOutput_CallerSuppliedFallbacks(t *testing.T) {
	// Fallbacks supplied explicitly by caller; no hidden magic heuristics!
	in := OutputEstimateInput{
		SourceSizeBytes:         500000000,
		TotalDurationSeconds:    1000.0,
		DeclaredVideoBitrateBps: 2000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "ac3", Channels: 6, FallbackBitrateBps: 384000, Copied: true},
		},
		SubtitleStreams: []SubtitleStreamEstimate{
			{Index: 2, Codec: "pgs", FallbackSizeBytes: 15000000},
		},
	}

	res, err := EstimateOutput(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.SuitableForSelection {
		t.Errorf("expected SuitableForSelection = true with caller fallbacks")
	}

	// Video: 2,000,000 * 1000 / 8 = 250,000,000 bytes
	expectedVideo := int64(250000000)
	if res.EstimatedVideoBytes != expectedVideo {
		t.Errorf("EstimatedVideoBytes = %d, want %d", res.EstimatedVideoBytes, expectedVideo)
	}

	// Audio: 384,000 * 1000 / 8 = 48,000,000 bytes
	expectedAudio := int64(48000000)
	if res.EstimatedAudioBytes != expectedAudio {
		t.Errorf("EstimatedAudioBytes = %d, want %d", res.EstimatedAudioBytes, expectedAudio)
	}

	// Subtitles: 15,000,000 bytes
	expectedSub := int64(15000000)
	if res.EstimatedSubtitleBytes != expectedSub {
		t.Errorf("EstimatedSubtitleBytes = %d, want %d", res.EstimatedSubtitleBytes, expectedSub)
	}

	hasVideoFallback := false
	hasAudioFallback := false
	hasSubFallback := false
	for _, u := range res.Uncertainties {
		if u == ReasonVideoBitrateFallback {
			hasVideoFallback = true
		}
		if u == ReasonAudioBitrateFallback+":stream_1" {
			hasAudioFallback = true
		}
		if u == ReasonSubtitleSizeEstimated+":stream_2" {
			hasSubFallback = true
		}
	}
	if !hasVideoFallback || !hasAudioFallback || !hasSubFallback {
		t.Errorf("missing expected fallback uncertainties in %v", res.Uncertainties)
	}
}

func TestEstimateOutput_MissingFallbacksUnsuitable(t *testing.T) {
	// Copied audio stream has no size, no bitrate, and NO caller fallback.
	// System must NOT invent size; it must surface uncertainty and mark unsuitable for selection!
	in := OutputEstimateInput{
		SourceSizeBytes:       500000000,
		TotalDurationSeconds:  100.0,
		SampleDurationSeconds: 10.0,
		SampleVideoBytes:      1000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "aac", Copied: true}, // No bitrate or fallback!
		},
	}

	res, err := EstimateOutput(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.SuitableForSelection {
		t.Errorf("expected SuitableForSelection = false when stream size cannot be determined")
	}
	if res.UnusableReason != ReasonMissingStreamBitrate {
		t.Errorf("UnusableReason = %q, want %q", res.UnusableReason, ReasonMissingStreamBitrate)
	}

	// Subtitle without size or fallback
	inSub := OutputEstimateInput{
		SourceSizeBytes:       500000000,
		TotalDurationSeconds:  100.0,
		SampleDurationSeconds: 10.0,
		SampleVideoBytes:      1000000,
		SubtitleStreams: []SubtitleStreamEstimate{
			{Index: 2, Codec: "subrip"}, // No size or fallback!
		},
	}

	resSub, err := EstimateOutput(inSub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resSub.SuitableForSelection {
		t.Errorf("expected SuitableForSelection = false for missing subtitle size")
	}
	if resSub.UnusableReason != ReasonMissingSubtitleSize {
		t.Errorf("UnusableReason = %q, want %q", resSub.UnusableReason, ReasonMissingSubtitleSize)
	}
}

func TestEstimateOutput_UnusableVideoEstimate(t *testing.T) {
	// Zero sample video bytes and no declared bitrate
	in := OutputEstimateInput{
		TotalDurationSeconds: 100.0,
	}

	res, err := EstimateOutput(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.SuitableForSelection {
		t.Errorf("expected SuitableForSelection = false when video bytes are 0")
	}
	if res.EstimatedVideoBytes != 0 {
		t.Errorf("EstimatedVideoBytes = %d, want 0", res.EstimatedVideoBytes)
	}
	if res.UnusableReason != ReasonInvalidVideoEstimate && res.UnusableReason != ReasonUnusableEstimate {
		t.Errorf("expected unusable reason, got %q", res.UnusableReason)
	}
}

func TestEstimateOutput_InputValidation(t *testing.T) {
	tests := []struct {
		name string
		in   OutputEstimateInput
	}{
		{
			name: "zero total duration",
			in:   OutputEstimateInput{TotalDurationSeconds: 0},
		},
		{
			name: "negative total duration",
			in:   OutputEstimateInput{TotalDurationSeconds: -10},
		},
		{
			name: "NaN total duration",
			in:   OutputEstimateInput{TotalDurationSeconds: math.NaN()},
		},
		{
			name: "negative sample duration",
			in:   OutputEstimateInput{TotalDurationSeconds: 100, SampleDurationSeconds: -5},
		},
		{
			name: "negative video bytes",
			in:   OutputEstimateInput{TotalDurationSeconds: 100, SampleVideoBytes: -100},
		},
		{
			name: "overhead rate >= 1.0",
			in:   OutputEstimateInput{TotalDurationSeconds: 100, ContainerOverheadRate: 1.05},
		},
		{
			name: "negative overhead rate",
			in:   OutputEstimateInput{TotalDurationSeconds: 100, ContainerOverheadRate: -0.1},
		},
		{
			name: "negative audio stream bitrate",
			in: OutputEstimateInput{
				TotalDurationSeconds: 100,
				AudioStreams: []AudioStreamEstimate{
					{BitrateBps: -100, Copied: true},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := EstimateOutput(tt.in)
			if err == nil {
				t.Errorf("expected error for %s", tt.name)
			}
			if res.SuitableForSelection {
				t.Errorf("expected SuitableForSelection = false on invalid input")
			}
		})
	}
}
