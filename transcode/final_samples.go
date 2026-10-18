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
