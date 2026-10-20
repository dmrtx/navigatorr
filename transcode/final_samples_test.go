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

func TestGridVerificationWindowsAreTheFinalGrid(t *testing.T) {
	for _, duration := range []float64{101, 2700, 3700, 6300, 10800} {
		grid := PlanFinalValidationWindows(duration, 10, 600, 5, 12)
		windows := GridVerificationWindows(duration, 10, 600, 5, 12)
		if len(windows) != len(grid) {
			t.Fatalf("%.0f s: %d windows for a %d window grid", duration, len(windows), len(grid))
		}
		byStart := map[float64]bool{}
		for i, w := range windows {
			if w.Index != i {
				t.Fatalf("%.0f s: window %d has index %d", duration, i, w.Index)
			}
			byStart[w.StartSeconds] = true
		}
		for _, g := range grid {
			if !byStart[g.StartSeconds] {
				t.Fatalf("%.0f s: final window at %.1f s is not searched", duration, g.StartSeconds)
			}
		}
		// Discovery windows are interior, near 20/50/80%, and distinct.
		seen := map[float64]bool{}
		for i, target := range []float64{0.2, 0.5, 0.8} {
			c := windows[i].CenterSeconds
			if c == grid[0].CenterSeconds || c == grid[len(grid)-1].CenterSeconds || seen[c] {
				t.Fatalf("%.0f s: discovery window %d at %.1f is an edge or repeated", duration, i, c)
			}
			seen[c] = true
			if d := c - target*duration; d > duration/float64(len(grid)) || -d > duration/float64(len(grid)) {
				t.Fatalf("%.0f s: discovery window %d at %.1f is far from %.0f%%", duration, i, c, target*100)
			}
		}
	}
	if GridVerificationWindows(99, 10, 600, 5, 12) != nil {
		t.Fatal("a source too short for the grid got windows")
	}
}

func TestFreshValidationWindowsAvoidTheGrid(t *testing.T) {
	grid := PlanFinalValidationWindows(3700, 10, 600, 5, 12)
	fresh := FreshValidationWindows(grid, 3700, 10, 2)
	if len(fresh) != 2 {
		t.Fatalf("%d fresh windows", len(fresh))
	}
	for i, f := range fresh {
		if f.Index != len(grid)+i || f.DurationSeconds != 10 || f.StartSeconds < 0 || f.StartSeconds+f.DurationSeconds > 3700 {
			t.Fatalf("fresh window %+v", f)
		}
		for _, g := range grid {
			if f.StartSeconds < g.StartSeconds+g.DurationSeconds && g.StartSeconds < f.StartSeconds+f.DurationSeconds {
				t.Fatalf("fresh window %+v overlaps %+v", f, g)
			}
		}
	}
	if FreshValidationWindows(grid, 3700, 10, 0) != nil || FreshValidationWindows(nil, 3700, 10, 2) != nil {
		t.Fatal("fresh windows without a grid or a count")
	}
}
