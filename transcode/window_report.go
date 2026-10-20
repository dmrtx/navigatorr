package transcode

import (
	"math"
	"sort"
)

// Roles of a window in a side-by-side report. "discovery" and "verification"
// come from the search samples, "fresh" from the out-of-sample final windows,
// and "unsearched" marks a final window no search sample covered.
const (
	WindowRoleDiscovery    = "discovery"
	WindowRoleVerification = "verification"
	WindowRoleFresh        = "fresh"
	WindowRoleUnsearched   = "unsearched"
)

// windowMatchSeconds is how far apart two windows' start times may be and
// still be the same window. Search and final windows come from the same grid
// planner, so real matches are exact; the tolerance absorbs rounding only.
const windowMatchSeconds = 0.01

// WindowMeasure is one window's VMAF.
type WindowMeasure struct {
	VMAFMean float64 `json:"vmaf_mean"`
	VMAFP5   float64 `json:"vmaf_p5"`
	VMAFMin  float64 `json:"vmaf_min"`
}

// WindowComparison puts what the search measured for the winner on a window
// next to what final validation measured on the encoded file. Either side is
// nil when that stage never saw the window.
type WindowComparison struct {
	StartSeconds    float64        `json:"start_seconds"`
	DurationSeconds float64        `json:"duration_seconds"`
	Role            string         `json:"role"`
	Search          *WindowMeasure `json:"search,omitempty"`
	Final           *WindowMeasure `json:"final,omitempty"`
}

// CompareWindows joins the winner's per-window sample scores to the final
// validation's per-window scores by start time, in time order.
func CompareWindows(search []BenchmarkWindowScore, final []FinalWindowScore) []WindowComparison {
	out := make([]WindowComparison, 0, len(search)+len(final))
	for _, s := range search {
		out = append(out, WindowComparison{
			StartSeconds: s.StartSeconds, DurationSeconds: s.DurationSeconds, Role: s.Role,
			Search: &WindowMeasure{VMAFMean: s.VMAFMean, VMAFP5: s.VMAFP5, VMAFMin: s.VMAFMin},
		})
	}
	for _, f := range final {
		measure := &WindowMeasure{VMAFMean: f.VMAFMean, VMAFP5: f.VMAFP5, VMAFMin: f.VMAFMin}
		matched := false
		for i := range out {
			if out[i].Final == nil && out[i].Search != nil && math.Abs(out[i].StartSeconds-f.StartSeconds) <= windowMatchSeconds {
				out[i].Final = measure
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		role := WindowRoleUnsearched
		if f.Role == WindowRoleFresh {
			role = WindowRoleFresh
		}
		out = append(out, WindowComparison{StartSeconds: f.StartSeconds, DurationSeconds: f.DurationSeconds, Role: role, Final: measure})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartSeconds < out[j].StartSeconds })
	if len(out) == 0 {
		return nil
	}
	return out
}
