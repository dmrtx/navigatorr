package transcode

import "math"

// PlanFinalValidationWindows spreads final quality windows over a source:
// one windowSeconds window per everySeconds of duration, clamped to
// [minimum, maximum], centred in equal slices so they never overlap. It
// returns nil when the source is too short for minimum full windows with a
// gap between them; callers then keep the benchmark windows.
func PlanFinalValidationWindows(duration, windowSeconds, everySeconds float64, minimum, maximum int) []BenchmarkSampleWindow {
	if math.IsNaN(duration) || math.IsInf(duration, 0) || windowSeconds <= 0 || everySeconds <= 0 || minimum < 1 || maximum < minimum || maximum > MaxBenchmarkSamples {
		return nil
	}
	if duration < 2*windowSeconds*float64(minimum) {
		return nil
	}
	count := int(math.Ceil(duration / everySeconds))
	count = max(minimum, min(maximum, count))
	// Each window needs its own slice at least twice its length.
	for count > minimum && duration < 2*windowSeconds*float64(count) {
		count--
	}
	slice := duration / float64(count)
	windows := make([]BenchmarkSampleWindow, count)
	for i := range windows {
		center := slice * (float64(i) + 0.5)
		windows[i] = BenchmarkSampleWindow{Index: i, StartSeconds: center - windowSeconds/2, DurationSeconds: windowSeconds, CenterSeconds: center}
	}
	return windows
}

// GridVerificationWindows lays out the sample windows of a size search that
// verifies on the final validation grid: the grid itself (see
// PlanFinalValidationWindows), reordered so that the first three are the
// discovery windows and the rest are the verification windows. A candidate that
// passes verification was therefore measured on exactly the windows final
// validation re-measures on the full encode.
//
// The discovery windows are the interior grid windows closest to 20%, 50% and
// 80% of the source; the first and last windows are left to verification
// because they often sit on titles or credits. Window indexes are positions in
// the returned slice, so the discovery windows are 0-2. It returns nil when the
// source is too short for a grid.
func GridVerificationWindows(duration, windowSeconds, everySeconds float64, minimum, maximum int) []BenchmarkSampleWindow {
	grid := PlanFinalValidationWindows(duration, windowSeconds, everySeconds, minimum, maximum)
	if len(grid) < 5 {
		return nil
	}
	chosen := make([]int, 0, 3)
	taken := map[int]bool{}
	for _, target := range []float64{0.2, 0.5, 0.8} {
		best, bestDist := -1, math.MaxFloat64
		for i := 1; i < len(grid)-1; i++ {
			if taken[i] {
				continue
			}
			if d := math.Abs(grid[i].CenterSeconds - target*duration); d < bestDist-1e-9 {
				best, bestDist = i, d
			}
		}
		taken[best] = true
		chosen = append(chosen, best)
	}
	out := make([]BenchmarkSampleWindow, 0, len(grid))
	for _, i := range chosen {
		out = append(out, grid[i])
	}
	for i, w := range grid {
		if !taken[i] {
			out = append(out, w)
		}
	}
	for i := range out {
		out[i].Index = i
	}
	return out
}

// FreshValidationWindows places up to count windows that no search window
// covers, at the boundaries between grid windows nearest one third and two
// thirds of the source. Final validation measures them only on the full encode,
// so they are an out-of-sample check on what verification could not see. The
// grid must be the final validation grid in time order. It returns nil when the
// grid has no room for them.
func FreshValidationWindows(grid []BenchmarkSampleWindow, duration, windowSeconds float64, count int) []BenchmarkSampleWindow {
	if count < 1 || len(grid) < 3 || windowSeconds <= 0 {
		return nil
	}
	var boundaries []float64
	for i := 0; i+1 < len(grid); i++ {
		boundaries = append(boundaries, (grid[i].CenterSeconds+grid[i+1].CenterSeconds)/2)
	}
	var out []BenchmarkSampleWindow
	used := map[int]bool{}
	for n := 1; n <= count; n++ {
		target := duration * float64(n) / float64(count+1)
		best, bestDist := -1, math.MaxFloat64
		for i, b := range boundaries {
			if used[i] {
				continue
			}
			if d := math.Abs(b - target); d < bestDist-1e-9 {
				best, bestDist = i, d
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		b := boundaries[best]
		start := b - windowSeconds/2
		if start < 0 || start+windowSeconds > duration {
			continue
		}
		out = append(out, BenchmarkSampleWindow{Index: len(grid) + len(out), StartSeconds: start, DurationSeconds: windowSeconds, CenterSeconds: b})
	}
	return out
}
