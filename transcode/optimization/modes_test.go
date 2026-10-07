package optimization

import (
	"math"
	"testing"
)

func modeCandidate(id, codec string, bytes int64, score float64, seconds *float64) EvaluatedCandidate {
	return EvaluatedCandidate{CandidateID: id, VideoCodec: codec, EstimatedBytes: bytes, Score: score, Eligible: true, MinimumMet: true, TargetReached: true, MeasuredSeconds: seconds}
}
func TestModeSavingsBoundaryAndEncoder(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		bytes int64
		codec string
		pass  bool
	}{{"size", 85, "libx265", true}, {"quality", 85, "libx265", true}, {"size", 95, "libx265", false}, {"quality", 95, "libx265", false}, {"x265_preserve", 95, "libx265", true}, {"x265_preserve", 95, "hevc_videotoolbox", false}, {"x265_preserve", 110, "libx265", false}, {"size", 110, "libx265", false}} {
		r := SelectMeasuredCandidates([]EvaluatedCandidate{modeCandidate("a", tc.codec, tc.bytes, 97, nil)}, tc.mode, 2, 100)
		if (r.Winner != nil) != tc.pass {
			t.Errorf("%+v -> %+v", tc, r)
		}
	}
}
func TestModesMeasuredBytesQualityTimeStable(t *testing.T) {
	slow, fast := 10.0, 1.0
	candidates := []EvaluatedCandidate{modeCandidate("best", "libx265", 80, 99, &slow), modeCandidate("small", "libx265", 50, 95, &fast), modeCandidate("near", "libx265", 81, 98, &fast)}
	for _, tc := range []struct{ mode, winner string }{{"size", "small"}, {"quality", "near"}, {"x265_preserve", "near"}} {
		r := SelectMeasuredCandidates(candidates, tc.mode, 2, 100)
		if r.Winner == nil || r.Winner.CandidateID != tc.winner {
			t.Fatalf("%s: %+v", tc.mode, r)
		}
	}
	candidates = []EvaluatedCandidate{modeCandidate("unknown", "libx265", 50, 99, nil), modeCandidate("measured", "libx265", 51, 99, &slow)}
	if r := SelectMeasuredCandidates(candidates, "size", 2, 100); r.Winner.CandidateID != "measured" {
		t.Fatal("unknown duration treated as fast")
	}
	nan := math.NaN()
	candidates[1].MeasuredSeconds = &nan
	if r := SelectMeasuredCandidates(candidates, "size", 2, 100); r.Winner.CandidateID != "measured" {
		t.Fatal("stable ID tie-break missing")
	}
}

func TestModeHugeByteBoundaryAndInvalidTolerance(t *testing.T) {
	source := int64(9007199254740999)
	maximum := (source/100)*85 + (source%100)*85/100
	if !ModeSizeAllowed("quality", source, maximum) || ModeSizeAllowed("quality", source, maximum+1) {
		t.Fatal("unrounded byte boundary lost")
	}
	for _, tol := range []float64{math.NaN(), math.Inf(1), -1} {
		r := SelectMeasuredCandidates([]EvaluatedCandidate{modeCandidate("a", "libx265", 80, 99, nil)}, "quality", tol, 100)
		if r.Winner != nil || r.DecisionReason != "invalid_tolerance" {
			t.Fatalf("invalid tolerance accepted: %+v", r)
		}
	}
}
