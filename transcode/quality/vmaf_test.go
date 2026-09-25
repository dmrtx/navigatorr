package quality

import (
	"math"
	"strings"
	"testing"
)

func TestParseVMAFStrictFramesAndSampleLocalWindow(t *testing.T) {
	model, err := Model("v1_1080p_3h")
	if err != nil {
		t.Fatal(err)
	}
	threshold := 95.0
	data := []byte(`{"version":"3.2.0","pooled_metrics":{"vmaf":{"mean":94.5}},"frames":[{"frameNum":0,"metrics":{"vmaf":90}},{"frameNum":1,"metrics":{"vmaf":92}},{"frameNum":2,"metrics":{"vmaf":97}},{"frameNum":3,"metrics":{"vmaf":99}}]}`)
	got, err := ParseVMAF(data, 4096, model, 2, 1062.5, 2, 1, &threshold)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mean != 94.5 || got.P5 != 90 || got.Min != 90 || got.Max != 99 || got.FramesBelowThreshold != 2 || got.FramesTotal != 4 {
		t.Fatalf("unexpected stats: %+v", got)
	}
	if got.WorstWindowMean != 91 || got.WorstWindowSampleIndex != 2 || got.WorstWindowSourceSec != 1062.5 || got.WindowTiming != "nominal_fps" {
		t.Fatalf("unexpected worst window: %+v", got)
	}
}

func TestParseVMAFRejectsInconsistentOrIncompleteEvidence(t *testing.T) {
	model, _ := Model("v1_1080p_3h")
	cases := []string{
		`{"pooled_metrics":{"vmaf":{"mean":95}}}`,
		`{"pooled_metrics":{"vmaf":{"mean":95}},"frames":[{"frameNum":0,"metrics":{"vmaf":90}}]}`,
		`{"frames":[{"frameNum":0,"metrics":{"vmaf":90}},{"frameNum":0,"metrics":{"vmaf":91}}]}`,
		`{"frames":[{"frameNum":0,"metrics":{"vmaf":90}},{"frameNum":2,"metrics":{"vmaf":91}}]}`,
		`{"frames":[{"frameNum":0,"metrics":{"vmaf":90}},{"frameNum":1,"metrics":{}}]}`,
		`{"frames":[{"frameNum":0,"metrics":{"vmaf":101}}]}`,
		`{"frames":[{"frameNum":0,"metrics":{"vmaf":90}}]`,
	}
	for _, data := range cases {
		if _, err := ParseVMAF([]byte(data), 4096, model, 0, 0, 24, 5, nil); err == nil {
			t.Errorf("accepted invalid evidence %s", data)
		}
	}
	if _, err := ParseVMAF([]byte(strings.Repeat("x", 4097)), 4096, model, 0, 0, 24, 5, nil); err == nil {
		t.Error("accepted oversized log")
	}
	if _, err := ParseVMAF([]byte(`{"frames":[{"frameNum":0,"metrics":{"vmaf":90}}]}`), 4096, model, 0, 0, math.NaN(), 5, nil); err == nil {
		t.Error("accepted NaN fps")
	}
	if _, err := Model("unknown"); err == nil {
		t.Error("accepted unknown model")
	}
}

func TestCombineVMAFWindowNeverBridgesSamples(t *testing.T) {
	model, _ := Model("v1_1080p_3h")
	first, err := ParseVMAF([]byte(`{"frames":[{"frameNum":0,"metrics":{"vmaf":100}},{"frameNum":1,"metrics":{"vmaf":100}}]}`), 4096, model, 0, 10, 1, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseVMAF([]byte(`{"frames":[{"frameNum":0,"metrics":{"vmaf":40}},{"frameNum":1,"metrics":{"vmaf":40}}]}`), 4096, model, 1, 100, 1, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CombineVMAF([]VMAFStats{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mean != 70 || got.P5 != 40 || got.WorstWindowMean != 40 || got.WorstWindowSampleIndex != 1 || got.WorstWindowSourceSec != 100 || got.FramesTotal != 4 {
		t.Fatalf("unexpected combined evidence: %+v", got)
	}
}

func TestParseVMAFUsesModelSpecificBounds(t *testing.T) {
	model := VMAFModel{ID: "test_4k", LibvmafModel: "test_4k", MinScore: 0, MaxScore: 110, MeasurementBitDepth: 10}
	got, err := ParseVMAF([]byte(`{"frames":[{"frameNum":0,"metrics":{"vmaf":105}}]}`), 4096, model, 0, 0, 24, 5, nil)
	if err != nil || got.Max != 105 {
		t.Fatalf("model range 0..110 rejected score 105: %+v %v", got, err)
	}
}
