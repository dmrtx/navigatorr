package selector

import (
	"reflect"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func TestSelector_AnimeH264_1080pOversized_SelectsAnimeHEVC(t *testing.T) {
	rep := mediainspect.DetailedReport{
		Probed:      true,
		Container:   "mkv",
		DurationSec: 1440,               // 24 min
		SizeBytes:   1600 * 1024 * 1024, // 1.6 GB (~8.9 Mbps) -> oversized
		Video:       []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		Audio:       []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "aac", Channels: 2}},
		Subtitles:   []mediainspect.DetailedStream{{Index: 2, Kind: "subtitle", Codec: "ass"}},
	}

	res := Select(Input{
		Report:            rep,
		MediaType:         "episode",
		IsAnime:           true,
		MinSavingsPercent: 15.0,
	})

	if res.Decision != DecisionTranscode {
		t.Fatalf("expected decision %q, got %q (reasons: %v)", DecisionTranscode, res.Decision, res.Reasons)
	}
	if res.Profile != "anime-hevc" {
		t.Fatalf("expected profile %q, got %q", "anime-hevc", res.Profile)
	}
	expectedReasons := []string{ReasonH2641080p, ReasonOversized, ReasonAnime}
	if !reflect.DeepEqual(res.Reasons, expectedReasons) {
		t.Fatalf("expected reasons %v, got %v", expectedReasons, res.Reasons)
	}
	if res.ExpectedSavingsPercent <= 15.0 {
		t.Fatalf("expected savings > 15%%, got %.2f%%", res.ExpectedSavingsPercent)
	}
}

func TestSelector_NonAnimeH264_1080pOversized_SelectsGeneralHEVC(t *testing.T) {
	rep := mediainspect.DetailedReport{
		Probed:      true,
		Container:   "mkv",
		DurationSec: 1440,
		SizeBytes:   1600 * 1024 * 1024,
		Video:       []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		Audio:       []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "ac3", Channels: 6}},
		Subtitles:   []mediainspect.DetailedStream{{Index: 2, Kind: "subtitle", Codec: "subrip"}},
	}

	res := Select(Input{
		Report:            rep,
		MediaType:         "episode",
		IsAnime:           false,
		MinSavingsPercent: 15.0,
	})

	if res.Decision != DecisionTranscode {
		t.Fatalf("expected decision %q, got %q (reasons: %v)", DecisionTranscode, res.Decision, res.Reasons)
	}
	if res.Profile != "general-hevc" {
		t.Fatalf("expected profile %q, got %q", "general-hevc", res.Profile)
	}
	expectedReasons := []string{ReasonH2641080p, ReasonOversized}
	if !reflect.DeepEqual(res.Reasons, expectedReasons) {
		t.Fatalf("expected reasons %v, got %v", expectedReasons, res.Reasons)
	}
}

func TestSelector_HEVC10BitReasonable_SkipsWithStableReasons(t *testing.T) {
	rep := mediainspect.DetailedReport{
		Probed:      true,
		Container:   "mkv",
		DurationSec: 1440,
		SizeBytes:   450 * 1024 * 1024, // 450 MB (~2.5 Mbps) -> reasonable size
		Video:       []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "hevc", Width: 1920, Height: 1080, BitDepth: 10}},
		Audio:       []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "aac", Channels: 2}},
	}

	res := Select(Input{
		Report:            rep,
		IsAnime:           true,
		MinSavingsPercent: 15.0,
	})

	if res.Decision != DecisionSkip {
		t.Fatalf("expected decision %q, got %q", DecisionSkip, res.Decision)
	}
	if res.Profile != "" {
		t.Fatalf("expected empty profile on skip, got %q", res.Profile)
	}

	expectedReasons := []string{ReasonAlreadyHEVC, Reason10Bit, ReasonReasonableSize}
	if !reflect.DeepEqual(res.Reasons, expectedReasons) {
		t.Fatalf("expected stable reasons %v, got %v", expectedReasons, res.Reasons)
	}
}

func TestSelector_BelowMinSavings_Skips(t *testing.T) {
	// Source bitrate is ~3.88 Mbps (700 MB / 1440s), anime target is 3.5 Mbps -> expected savings ~9.8%
	rep := mediainspect.DetailedReport{
		Probed:      true,
		Container:   "mkv",
		DurationSec: 1440,
		SizeBytes:   700 * 1024 * 1024,
		Video:       []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
	}

	res := Select(Input{
		Report:             rep,
		IsAnime:            true,
		MinSavingsPercent:  20.0, // higher than expected ~9.8% savings
		OversizedThreshold: 500 * 1024 * 1024,
	})

	if res.Decision != DecisionSkip {
		t.Fatalf("expected decision %q, got %q", DecisionSkip, res.Decision)
	}
	if len(res.Reasons) != 1 || res.Reasons[0] != ReasonBelowMinSavings {
		t.Fatalf("expected reason %q, got %v", ReasonBelowMinSavings, res.Reasons)
	}
}

func TestSelector_NoAutomaticDownscale_4K_Skips(t *testing.T) {
	rep := mediainspect.DetailedReport{
		Probed:      true,
		Container:   "mkv",
		DurationSec: 1440,
		SizeBytes:   4000 * 1024 * 1024, // 4GB
		Video:       []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 3840, Height: 2160, BitDepth: 8}},
	}

	res := Select(Input{
		Report:            rep,
		IsAnime:           false,
		MinSavingsPercent: 15.0,
	})

	if res.Decision != DecisionSkip {
		t.Fatalf("expected decision %q, got %q", DecisionSkip, res.Decision)
	}
	if len(res.Reasons) != 1 || res.Reasons[0] != ReasonNoAutomaticDownscale {
		t.Fatalf("expected reason %q, got %v", ReasonNoAutomaticDownscale, res.Reasons)
	}
}

func TestSelector_FailClosed_UnsupportedSubtitleStream_Review(t *testing.T) {
	rep := mediainspect.DetailedReport{
		Probed:      true,
		Container:   "mkv",
		DurationSec: 1440,
		SizeBytes:   1500 * 1024 * 1024,
		Video:       []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		Subtitles:   []mediainspect.DetailedStream{{Index: 1, Kind: "subtitle", Codec: "made_up_subtitle"}},
	}

	res := Select(Input{
		Report:  rep,
		IsAnime: true,
	})

	if res.Decision != DecisionReview {
		t.Fatalf("expected decision %q, got %q", DecisionReview, res.Decision)
	}
	if len(res.Reasons) != 1 || res.Reasons[0] != ReasonUnsupportedSubtitle {
		t.Fatalf("expected reason %q, got %v", ReasonUnsupportedSubtitle, res.Reasons)
	}
}

func TestSelector_FailClosed_UnprobedOrNoVideoStream(t *testing.T) {
	unprobed := Select(Input{
		Report: mediainspect.DetailedReport{Probed: false},
	})
	if unprobed.Decision != DecisionReview || unprobed.Reasons[0] != ReasonUnprobedMedia {
		t.Fatalf("expected unprobed review, got %+v", unprobed)
	}

	noVideo := Select(Input{
		Report: mediainspect.DetailedReport{Probed: true, Video: nil},
	})
	if noVideo.Decision != DecisionReview || noVideo.Reasons[0] != ReasonNoVideoStream {
		t.Fatalf("expected no video review, got %+v", noVideo)
	}
}

func TestSelector_SupportedSubtitleConversions_movTextAllowed(t *testing.T) {
	rep := mediainspect.DetailedReport{
		Probed:      true,
		Container:   "mkv",
		DurationSec: 1440,
		SizeBytes:   1500 * 1024 * 1024,
		Video:       []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		Subtitles:   []mediainspect.DetailedStream{{Index: 1, Kind: "subtitle", Codec: "mov_text"}},
	}

	res := Select(Input{
		Report:            rep,
		IsAnime:           false,
		MinSavingsPercent: 15.0,
	})

	if res.Decision != DecisionTranscode {
		t.Fatalf("mov_text should be allowed for conversion to subrip, got decision %s: %v", res.Decision, res.Reasons)
	}
}
