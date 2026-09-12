package selector

import (
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// Decision constants for selector outcomes.
const (
	DecisionTranscode = "transcode"
	DecisionSkip      = "skip"
	DecisionReview    = "review"
)

// Deterministic explanation reason constants.
const (
	ReasonAlreadyHEVC          = "already_hevc"
	Reason10Bit                = "10bit"
	ReasonReasonableSize       = "reasonable_size"
	ReasonOversized            = "oversized"
	ReasonH2641080p            = "h264_1080p"
	ReasonAnime                = "anime"
	ReasonBelowMinSavings      = "below_min_savings"
	ReasonNotOversized         = "not_oversized"
	ReasonNoAutomaticDownscale = "no_automatic_downscale"
	ReasonUnsupportedSubtitle  = "unsupported_subtitle_stream"
	ReasonUnsupportedCodec     = "unsupported_source_codec"
	ReasonUnknownVideoCodec    = "unknown_video_codec"
	ReasonNoVideoStream        = "no_video_stream"
	ReasonMultipleVideoStreams = "multiple_video_streams"
	ReasonUnprobedMedia        = "unprobed_media"
	ReasonNon1080p             = "non_1080p"
)

// Allowed subtitle codecs for Matroska container preservation.
// Direct stream copy allowlist from recipe capability boundary:
var allowedCopySubtitles = map[string]bool{
	"subrip":            true,
	"srt":               true,
	"ass":               true,
	"ssa":               true,
	"hdmv_pgs_subtitle": true,
	"pgs":               true,
	"dvd_subtitle":      true,
	"vobsub":            true,
	"dvb_subtitle":      true,
	"dvb_teletext":      true,
	"webvtt":            true,
	"text":              true,
}

// Subtitle conversion allowlist:
var allowedConvertSubtitles = map[string]bool{
	"mov_text": true, // converts to subrip
}

// Input encapsulates media inspection data and contextual signals for profile selection.
type Input struct {
	Report             mediainspect.DetailedReport `json:"report"`
	MediaType          string                      `json:"media_type,omitempty"`
	IsAnime            bool                        `json:"is_anime"`
	MinSavingsPercent  float64                     `json:"min_savings_percent"`
	OversizedThreshold int64                       `json:"oversized_threshold_bytes,omitempty"`
	ExpectedSavings    float64                     `json:"expected_savings_percent,omitempty"`
}

// Result is the serializable, explainable decision produced by the selector.
type Result struct {
	Decision               string   `json:"decision"`
	Profile                string   `json:"profile,omitempty"`
	Reasons                []string `json:"reasons"`
	ExpectedSavingsPercent float64  `json:"expected_savings_percent,omitempty"`
}

// Select determines whether media should be transcoded, skipped, or reviewed.
func Select(input Input) Result {
	// (d) Fail-closed stream preservation checks:
	if !input.Report.Probed {
		return Result{
			Decision: DecisionReview,
			Reasons:  []string{ReasonUnprobedMedia},
		}
	}
	if len(input.Report.Video) == 0 {
		return Result{
			Decision: DecisionReview,
			Reasons:  []string{ReasonNoVideoStream},
		}
	}
	if len(input.Report.Video) > 1 {
		return Result{
			Decision: DecisionReview,
			Reasons:  []string{ReasonMultipleVideoStreams},
		}
	}

	// Verify all subtitle streams can be safely preserved or converted in Matroska
	for _, s := range input.Report.Subtitles {
		c := strings.ToLower(strings.TrimSpace(s.Codec))
		if !allowedCopySubtitles[c] && !allowedConvertSubtitles[c] {
			return Result{
				Decision: DecisionReview,
				Reasons:  []string{ReasonUnsupportedSubtitle},
			}
		}
	}

	v := input.Report.Video[0]
	codec := strings.ToLower(strings.TrimSpace(v.Codec))
	if codec == "" {
		return Result{
			Decision: DecisionReview,
			Reasons:  []string{ReasonUnknownVideoCodec},
		}
	}

	// Evaluate oversized status
	isOversized, isReasonable := evaluateSize(input.Report, input.OversizedThreshold)

	// (a) HEVC / x265 check:
	if isHEVC(codec) {
		reasons := []string{ReasonAlreadyHEVC}
		if v.BitDepth == 10 {
			reasons = append(reasons, Reason10Bit)
		}
		if isReasonable {
			reasons = append(reasons, ReasonReasonableSize)
		}
		return Result{
			Decision: DecisionSkip,
			Reasons:  reasons,
		}
	}

	// (c) Resolution and downscaling evaluation:
	is1080p, exceeds1080p := evaluateResolution(v.Width, v.Height)
	if exceeds1080p {
		return Result{
			Decision: DecisionSkip,
			Reasons:  []string{ReasonNoAutomaticDownscale},
		}
	}
	if !is1080p {
		return Result{
			Decision: DecisionSkip,
			Reasons:  []string{ReasonNon1080p},
		}
	}

	// (b) H264 / AVC check:
	if !isH264(codec) {
		return Result{
			Decision: DecisionSkip,
			Reasons:  []string{ReasonUnsupportedCodec},
		}
	}

	if !isOversized {
		return Result{
			Decision: DecisionSkip,
			Reasons:  []string{ReasonNotOversized, ReasonReasonableSize},
		}
	}

	// (e) Expected savings calculation and threshold enforcement:
	expectedSavings := input.ExpectedSavings
	if expectedSavings <= 0 {
		expectedSavings = calculateExpectedSavings(input.Report, input.IsAnime)
	}

	if input.MinSavingsPercent > 0 && expectedSavings < input.MinSavingsPercent {
		return Result{
			Decision:               DecisionSkip,
			Reasons:                []string{ReasonBelowMinSavings},
			ExpectedSavingsPercent: expectedSavings,
		}
	}

	targetProfile := "general-hevc"
	reasons := []string{ReasonH2641080p, ReasonOversized}
	if input.IsAnime {
		targetProfile = "anime-hevc"
		reasons = append(reasons, ReasonAnime)
	}

	return Result{
		Decision:               DecisionTranscode,
		Profile:                targetProfile,
		Reasons:                reasons,
		ExpectedSavingsPercent: expectedSavings,
	}
}

func isHEVC(codec string) bool {
	switch codec {
	case "hevc", "h265", "x265", "hev1", "hvc1":
		return true
	default:
		return false
	}
}

func isH264(codec string) bool {
	switch codec {
	case "h264", "avc", "avc1", "x264":
		return true
	default:
		return false
	}
}

func evaluateResolution(width, height int) (is1080p bool, exceeds1080p bool) {
	if width > 1920 || height > 1080 {
		return false, true
	}
	// 1080p encompasses standard 1920x1080, cropped aspect ratios (e.g. 1920x800, 1920x1040),
	// and 4:3 1080p (e.g. 1440x1080).
	if (height >= 720 && height <= 1080 && width >= 1280 && width <= 1920) || width == 1920 || height == 1080 {
		return true, false
	}
	return false, false
}

func evaluateSize(rep mediainspect.DetailedReport, explicitThreshold int64) (isOversized bool, isReasonable bool) {
	if explicitThreshold > 0 {
		isOversized = rep.SizeBytes > explicitThreshold
		return isOversized, !isOversized
	}

	// Conservative defaults for 1080p:
	// A standard 24-min anime episode at 900 MB is ~5.2 Mbps bitrate.
	// We mark content as oversized if bitrate > 5.0 Mbps or total size > 900 MB.
	if rep.DurationSec > 0 {
		bitrateBps := float64(rep.SizeBytes*8) / rep.DurationSec
		isOversized = bitrateBps > 5_000_000 || rep.SizeBytes > (900*1024*1024)
	} else {
		isOversized = rep.SizeBytes > (900 * 1024 * 1024)
	}
	return isOversized, !isOversized
}

func calculateExpectedSavings(rep mediainspect.DetailedReport, isAnime bool) float64 {
	// Conservative expected target bitrates for VideoToolbox HEVC:
	// - anime-hevc (quality 80): ~3.5 Mbps target
	// - general-hevc (quality 70): ~3.0 Mbps target
	targetBitrateBps := 3_000_000.0
	if isAnime {
		targetBitrateBps = 3_500_000.0
	}

	if rep.DurationSec > 0 && rep.SizeBytes > 0 {
		sourceBitrateBps := float64(rep.SizeBytes*8) / rep.DurationSec
		if sourceBitrateBps > targetBitrateBps {
			return ((sourceBitrateBps - targetBitrateBps) / sourceBitrateBps) * 100.0
		}
		return 0.0
	}

	// Conservative nominal savings when duration is unprobed:
	if isAnime {
		return 35.0
	}
	return 45.0
}
