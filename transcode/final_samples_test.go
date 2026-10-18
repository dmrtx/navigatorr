package transcode

import "testing"

func TestPlanFinalValidationWindows(t *testing.T) {
	for _, tc := range []struct {
		duration float64
		count    int
	}{
		{99, 0}, {100, 5}, {600, 5}, {45 * 60, 5}, {55 * 60, 6}, {105 * 60, 11}, {120 * 60, 12}, {300 * 60, 12},
	} {
		windows := PlanFinalValidationWindows(tc.duration, 10, 600, 5, 12)
		if len(windows) != tc.count {
			t.Fatalf("duration %.0f: %d windows, want %d", tc.duration, len(windows), tc.count)
		}
		for i, w := range windows {
			if w.Index != i || w.DurationSeconds != 10 || w.StartSeconds < 0 || w.StartSeconds+w.DurationSeconds > tc.duration {
				t.Fatalf("duration %.0f: window %+v out of range", tc.duration, w)
			}
			if i > 0 && windows[i-1].StartSeconds+windows[i-1].DurationSeconds >= w.StartSeconds {
				t.Fatalf("duration %.0f: windows %d and %d overlap or touch", tc.duration, i-1, i)
			}
		}
	}
	for _, bad := range [][]float64{{-1}, {0}} {
		if PlanFinalValidationWindows(bad[0], 10, 600, 5, 12) != nil {
			t.Fatalf("duration %v planned windows", bad[0])
		}
	}
	if PlanFinalValidationWindows(3600, 10, 600, 5, MaxBenchmarkSamples+1) != nil {
		t.Fatal("maximum above the worker sample limit was accepted")
	}
}
