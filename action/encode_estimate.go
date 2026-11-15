package action

import (
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// An encode estimate is built from how fast this worker encoded this kind of
// file before, never from a guess: without enough history there is no number.

// encodeEstimateWindow is how many of the newest matching samples count.
const encodeEstimateWindow = 20

// encodeEstimateMinSamples is the fewest samples an estimate rests on.
const encodeEstimateMinSamples = 2

// EncodeSample is one finished encode: how much video it covered and how long
// the worker spent encoding it.
type EncodeSample struct {
	Worker      string
	HeightClass string
	Family      string
	DurationSec float64
	EncodeSec   float64
}

// Encoder families. Speeds differ by an order of magnitude between them, so a
// sample only speaks for its own family.
const (
	EncodeFamilyCopy     = "copy"
	EncodeFamilyX265     = "x265"
	EncodeFamilyHardware = "hardware"
)

// HeightClass buckets a video height: uhd (4K), hd (1080p and 720p), sd.
func HeightClass(h int) string {
	switch {
	case h >= 1800:
		return "uhd"
	case h >= 900:
		return "hd"
	case h > 0:
		return "sd"
	}
	return ""
}

// EncodeFamily names the encoder family from the video codec of the plan a
// job ran ("copy" for a copied video or audio-only job).
func EncodeFamily(videoCodec string) string {
	switch {
	case videoCodec == transcode.VideoCodecCopy:
		return EncodeFamilyCopy
	case videoCodec == transcode.VideoCodecLibX265:
		return EncodeFamilyX265
	case strings.HasSuffix(videoCodec, "_videotoolbox"):
		return EncodeFamilyHardware
	}
	return ""
}

// EstimateEncodeSeconds estimates how long durationSec of video takes to
// encode: durationSec over the median realtime ratio of the newest samples
// (callers pass them newest first) of the same worker, class and family, or of
// any worker when this one has too few. It reports false rather than guess.
func EstimateEncodeSeconds(samples []EncodeSample, worker, class, family string, durationSec float64) (float64, bool) {
	if durationSec <= 0 || class == "" || family == "" {
		return 0, false
	}
	for _, anyWorker := range []bool{false, true} {
		var ratios []float64
		for _, s := range samples {
			if len(ratios) == encodeEstimateWindow {
				break
			}
			if s.DurationSec <= 0 || s.EncodeSec <= 0 || s.HeightClass != class || s.Family != family || (!anyWorker && s.Worker != worker) {
				continue
			}
			ratios = append(ratios, s.DurationSec/s.EncodeSec)
		}
		if len(ratios) < encodeEstimateMinSamples {
			continue
		}
		sort.Float64s(ratios)
		mid := len(ratios) / 2
		median := ratios[mid]
		if len(ratios)%2 == 0 {
			median = (ratios[mid-1] + ratios[mid]) / 2
		}
		return durationSec / median, true
	}
	return 0, false
}
