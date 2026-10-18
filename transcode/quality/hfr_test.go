package quality

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestTemporalDecimationClasses(t *testing.T) {
	for _, tc := range []struct {
		fps    float64
		factor int
		ok     bool
	}{
		{0, 0, false}, {-25, 0, false}, {math.NaN(), 0, false}, {math.Inf(1), 0, false},
		{23.976, 1, true}, {25, 1, true}, {29.97, 1, true}, {30, 1, true}, {44.99, 1, true},
		{45, 2, true}, {48, 2, true}, {50, 2, true}, {59.94, 2, true}, {60, 2, true}, {61, 2, true},
		{61.01, 0, false}, {100, 0, false}, {119.88, 0, false}, {120, 0, false},
	} {
		factor, err := TemporalDecimation(tc.fps)
		if (err == nil) != tc.ok || factor != tc.factor {
			t.Fatalf("%.3f fps: factor %d err %v, want %d ok=%v", tc.fps, factor, err, tc.factor, tc.ok)
		}
		if IsHFR(tc.fps) != (tc.factor == 2) {
			t.Fatalf("%.3f fps: IsHFR mismatch", tc.fps)
		}
	}
}

func TestDecimationFilterConstruction(t *testing.T) {
	if got, err := DecimationFilter(1); err != nil || got != "" {
		t.Fatalf("factor 1 must leave the graph unchanged: %q %v", got, err)
	}
	got, err := DecimationFilter(2)
	if err != nil || got != `,select=not(mod(n\,2))` {
		t.Fatalf("factor 2 filter %q %v", got, err)
	}
	for _, bad := range []int{0, -1, 3, 4} {
		if _, err := DecimationFilter(bad); err == nil {
			t.Fatalf("factor %d accepted", bad)
		}
	}
	if got, err := MetricPairInputs(1); err != nil || got != "[0:v]format=yuv420p10le,settb=AVTB,setpts=N[d];[1:v]format=yuv420p10le,settb=AVTB,setpts=N[r];[d][r]" {
		t.Fatalf("native pair inputs %q %v", got, err)
	}
	if got, err := MetricPairInputs(2); err != nil || got != `[0:v]format=yuv420p10le,select=not(mod(n\,2)),settb=AVTB,setpts=N[d];[1:v]format=yuv420p10le,select=not(mod(n\,2)),settb=AVTB,setpts=N[r];[d][r]` {
		t.Fatalf("decimated pair inputs %q %v", got, err)
	}
	if got := IndexPairInputs(); got != "[0:v]settb=AVTB,setpts=N[d];[1:v]settb=AVTB,setpts=N[r];[d][r]" {
		t.Fatalf("index pair inputs %q", got)
	}
	if _, err := MetricPairInputs(3); err == nil {
		t.Fatal("factor 3 pair inputs accepted")
	}
	for _, tc := range [][3]int{{0, 2, 0}, {1, 2, 1}, {2, 2, 1}, {99, 2, 50}, {100, 2, 50}, {100, 1, 100}, {5, 0, 0}} {
		if got := DecimatedFrameCount(tc[0], tc[1]); got != tc[2] {
			t.Fatalf("DecimatedFrameCount(%d,%d)=%d want %d", tc[0], tc[1], got, tc[2])
		}
	}
}

func vmafLog(scores []float64) []byte {
	type frame struct {
		FrameNum int                `json:"frameNum"`
		Metrics  map[string]float64 `json:"metrics"`
	}
	frames := make([]frame, len(scores))
	for i, s := range scores {
		frames[i] = frame{FrameNum: i, Metrics: map[string]float64{"vmaf": s}}
	}
	data, _ := json.Marshal(map[string]any{"version": "test", "frames": frames})
	return data
}

// A decimated log has one entry per kept source frame. Times must still land
// on the source timeline and windows keep their length in seconds.
func TestParseVMAFDecimatedTimingMapsToSource(t *testing.T) {
	model, _ := Model("v1_1080p_3h")
	scores := make([]float64, 50) // 2 s of 50 fps source, decimated 2:1
	for i := range scores {
		scores[i] = 95
	}
	for i := 20; i < 25; i++ { // measured frames 20-24 = source frames 40-48
		scores[i] = 60
	}
	threshold := 80.0
	got, err := ParseVMAF(vmafLog(scores), 1<<20, model, 3, 100, 50, 2, 0.2, &threshold)
	if err != nil {
		t.Fatal(err)
	}
	if got.TemporalDecimation != 2 || got.MeasuredFPS != 25 || got.FramesTotal != 50 {
		t.Fatalf("decimation evidence missing: %+v", got)
	}
	if got.WorstWindowSeconds != 0.2 || math.Abs(got.WorstWindowStartSec-0.8) > 1e-9 || math.Abs(got.WorstWindowSourceSec-100.8) > 1e-9 || got.WorstWindowMean != 60 {
		t.Fatalf("window not mapped to source time: %+v", got)
	}
	if got.FramesBelowThreshold != 5 || got.SourceEquivalentFramesBelowThreshold() != 10 {
		t.Fatalf("frame limit must be compared in source frames: %+v", got)
	}
	data, _ := json.Marshal(got)
	if !strings.Contains(string(data), `"temporal_decimation":2`) || !strings.Contains(string(data), `"measured_fps":25`) {
		t.Fatalf("evidence JSON lacks decimation: %s", data)
	}

	native, err := ParseVMAF(vmafLog(scores), 1<<20, model, 3, 100, 25, 1, 0.2, &threshold)
	if err != nil || native.TemporalDecimation != 1 || native.SourceEquivalentFramesBelowThreshold() != 5 || native.WorstWindowSourceSec != got.WorstWindowSourceSec {
		t.Fatalf("native 25 fps must match decimated 50 fps timing: %+v %v", native, err)
	}
	for _, bad := range []int{0, 3, -2} {
		if _, err := ParseVMAF(vmafLog(scores), 1<<20, model, 0, 0, 50, bad, 0.2, nil); err == nil {
			t.Fatalf("decimation %d accepted", bad)
		}
	}
}

func TestCombineVMAFRejectsMixedDecimation(t *testing.T) {
	model, _ := Model("v1_1080p_3h")
	a, err := ParseVMAF(vmafLog([]float64{90, 91}), 4096, model, 0, 0, 50, 2, 0.04, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseVMAF(vmafLog([]float64{90, 91}), 4096, model, 1, 10, 25, 1, 0.04, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CombineVMAF([]VMAFStats{a, b}); err == nil {
		t.Fatal("samples measured at different decimations combined")
	}
	combined, err := CombineVMAF([]VMAFStats{a, a})
	if err != nil || combined.TemporalDecimation != 2 || combined.FramesTotal != 4 {
		t.Fatalf("decimation lost in combination: %+v %v", combined, err)
	}
}
