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

func TestEstimateOutput_DiscardedAudioExcluded(t *testing.T) {
	// Audio stream 1 is Discarded: true (should NOT be included in output)
	// Audio stream 2 is Copied: true (should be included)
	in := OutputEstimateInput{
		SourceSizeBytes:       1000000000,
		TotalDurationSeconds:  1000.0,
		SampleDurationSeconds: 10.0,
		SampleVideoBytes:      1000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "dts", SizeBytes: 150000000, Discarded: true}, // Discarded
			{Index: 2, Codec: "aac", SizeBytes: 50000000, Copied: true},     // Kept
		},
	}

	res, err := EstimateOutput(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedAudio := int64(50000000)
	if res.EstimatedAudioBytes != expectedAudio {
		t.Errorf("EstimatedAudioBytes = %d, want %d (discarded audio must be excluded)", res.EstimatedAudioBytes, expectedAudio)
	}
}

func TestEstimateOutput_NonCopiedAudioExplicitPayload(t *testing.T) {
	// Re-encoded audio stream (Copied: false) with explicit target bitrate
	in := OutputEstimateInput{
		SourceSizeBytes:       1000000000,
		TotalDurationSeconds:  100.0,
		SampleDurationSeconds: 10.0,
		SampleVideoBytes:      1000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "aac", Channels: 2, BitrateBps: 192000, Copied: false}, // 192 kbps re-encode
		},
	}

	res, err := EstimateOutput(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.SuitableForSelection {
		t.Errorf("expected SuitableForSelection = true when non-copied audio has explicit bitrate")
	}

	// 192,000 * 100 / 8 = 2,400,000 bytes
	expectedAudio := int64(2400000)
	if res.EstimatedAudioBytes != expectedAudio {
		t.Errorf("EstimatedAudioBytes = %d, want %d", res.EstimatedAudioBytes, expectedAudio)
	}
}

func TestEstimateOutput_NonCopiedAudioLackingEstimateUnsuitable(t *testing.T) {
	// Re-encoded audio stream (Copied: false) lacking size, bitrate, and fallback.
	// System MUST NOT silently ignore non-copied audio (which would underestimate size);
	// it must mark the estimate unsuitable!
	in := OutputEstimateInput{
		SourceSizeBytes:       1000000000,
		TotalDurationSeconds:  100.0,
		SampleDurationSeconds: 10.0,
		SampleVideoBytes:      1000000,
		AudioStreams: []AudioStreamEstimate{
			{Index: 1, Codec: "aac", Channels: 2, Copied: false}, // No bitrate or fallback!
		},
	}

	res, err := EstimateOutput(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.SuitableForSelection {
		t.Errorf("expected SuitableForSelection = false when non-copied stream has unknown size")
	}
	if res.UnusableReason != ReasonMissingStreamBitrate {
		t.Errorf("UnusableReason = %q, want %q", res.UnusableReason, ReasonMissingStreamBitrate)
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

	// Bitmap subtitle without size or fallback still fails closed.
	inSub := OutputEstimateInput{
		SourceSizeBytes:       500000000,
		TotalDurationSeconds:  100.0,
		SampleDurationSeconds: 10.0,
		SampleVideoBytes:      1000000,
		SubtitleStreams: []SubtitleStreamEstimate{
			{Index: 2, Codec: "hdmv_pgs_subtitle"}, // No size or fallback!
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

func TestEstimateOutput_TextSubtitleAllowance(t *testing.T) {
	for _, codec := range []string{"srt", "subrip", "mov_text", "ass", "ssa", "webvtt", " ASS "} {
		t.Run(codec, func(t *testing.T) {
			res, err := EstimateOutput(OutputEstimateInput{
				SourceSizeBytes: 1786755584, TotalDurationSeconds: 3600,
				SampleDurationSeconds: 60, SampleVideoBytes: 7304172,
				AudioStreams:    []AudioStreamEstimate{{Index: 1, Codec: "aac", BitrateBps: 192000, Copied: true}},
				SubtitleStreams: []SubtitleStreamEstimate{{Index: 2, Codec: codec}},
			})
			if err != nil || !res.SuitableForSelection || res.UnusableReason != "" {
				t.Fatalf("unknown text subtitle size should remain selectable: %+v, %v", res, err)
			}
			if res.EstimatedSubtitleBytes != DefaultTextSubtitleSizeBytes {
				t.Errorf("subtitle estimate = %d, want conservative %d", res.EstimatedSubtitleBytes, DefaultTextSubtitleSizeBytes)
			}
			if len(res.Uncertainties) != 1 || res.Uncertainties[0] != "subtitle_size_estimated:stream_2" {
				t.Errorf("missing visible subtitle uncertainty: %v", res.Uncertainties)
			}
			if math.Abs(res.SavingsPercent-70.36) > 0.15 {
				t.Errorf("small text allowance materially changed ~70%% savings: %.2f", res.SavingsPercent)
			}
		})
	}
	for _, codec := range []string{"hdmv_pgs_subtitle", "dvd_subtitle", "unknown", ""} {
		t.Run("no_default_"+codec, func(t *testing.T) {
			res, err := EstimateOutput(OutputEstimateInput{
				TotalDurationSeconds: 100, SampleDurationSeconds: 10, SampleVideoBytes: 1000000,
				SubtitleStreams: []SubtitleStreamEstimate{{Index: 2, Codec: codec}},
			})
			if err != nil || res.SuitableForSelection || res.UnusableReason != ReasonMissingSubtitleSize {
				t.Fatalf("unknown/bitmap subtitle must still require size: %+v, %v", res, err)
			}
		})
	}
}

func TestEstimateOutput_OverflowProtection(t *testing.T) {
	// Float multiplication scaling that would exceed int64
	in := OutputEstimateInput{
		TotalDurationSeconds:  1000000.0,
		SampleDurationSeconds: 0.001,
		SampleVideoBytes:      10000000000000000, // 1e16 bytes * 1e9 scale = 1e25 bytes
	}

	res, err := EstimateOutput(in)
	if err == nil {
		t.Errorf("expected error on float overflow scaling")
	}
	if res.SuitableForSelection {
		t.Errorf("expected SuitableForSelection = false on overflow")
	}
	if res.UnusableReason != ReasonIntegerOverflow {
		t.Errorf("UnusableReason = %q, want %q", res.UnusableReason, ReasonIntegerOverflow)
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
