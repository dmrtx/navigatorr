package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode/quality"
)

// Exercise the actual filter parser and model, not a string assertion. The
// files stay 8-bit in both measurements; only the model's encoding metadata
// changes. Promoting samples to 10-bit must not make CAMBI assume a 10-bit encode.
func TestVMAFRealEncodedDepthIsIndependentOfMeasurementDepth(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	caps := probeQualityCapabilities(ctx, ffmpeg, dir)
	if !caps.Models["v1_1080p_3h"].Available {
		t.Skip("VMAF v1 model unavailable")
	}
	run := func(args []string) {
		t.Helper()
		if output, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, output)
		}
	}
	ref := filepath.Join(dir, "reference.mkv")
	cand := filepath.Join(dir, "candidate.mkv")
	run([]string{"-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "nullsrc=size=640x360:rate=24:duration=0.25,format=yuv420p,geq=lum='16+X*200/W':cb=128:cr=128", "-c:v", "ffv1", ref})
	run([]string{"-y", "-nostdin", "-v", "error", "-i", ref, "-vf", "lut=y='floor(val/4)*4'", "-c:v", "ffv1", cand})
	model, _ := quality.Model("v1_1080p_3h")
	scores := map[int]float64{}
	for _, depth := range []int{8, 10} {
		log := filepath.Join(dir, "metric.json")
		args, err := BuildVMAFModelArgs(cand, ref, log, model, depth, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		run(args)
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, 0, 0, 24, 1, 0.2, nil)
		if err != nil || stats.FramesTotal != 6 || stats.MeasurementBitDepth != 10 {
			t.Fatalf("invalid measurement: %+v, %v", stats, err)
		}
		var shape struct {
			Pooled map[string]json.RawMessage `json:"pooled_metrics"`
		}
		if err := json.Unmarshal(data, &shape); err != nil {
			t.Fatal(err)
		}
		encoded8 := false
		for name := range shape.Pooled {
			if strings.HasPrefix(name, "cambi_") && strings.Contains(name, "encbd_8") {
				encoded8 = true
			}
		}
		if encoded8 != (depth == 8) {
			t.Fatal("libvmaf received incorrect encoding metadata")
		}
		scores[depth] = stats.Mean
		run(BuildCAMBIArgs(cand, ref, log, depth, 0))
		data, err = os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		shape.Pooled = nil
		cambi, err := quality.ParseCAMBIFullReference(data, MaxMetricLogSizeBytes, "cambi_full_reference", 0, 0, 24)
		if err != nil || cambi.FramesTotal != 6 {
			t.Fatalf("invalid CAMBI measurement: %+v, %v", cambi, err)
		}
		if err := json.Unmarshal(data, &shape); err != nil {
			t.Fatal(err)
		}
		if depth == 8 {
			if _, ok := shape.Pooled["cambi_encbd_8"]; !ok {
				t.Fatal("full-reference CAMBI lost encoded bit depth")
			}
		}
	}
	t.Logf("same 8-bit samples, measurement precision 10: encoded8=%.3f encoded10=%.3f", scores[8], scores[10])
}

func TestVMAFRejectsUnknownEncodedDepth(t *testing.T) {
	model, _ := quality.Model("v1_1080p_3h")
	for _, depth := range []int{0, 9, 12} {
		if _, err := BuildVMAFModelArgs("candidate", "reference", "metric.json", model, depth, 1, 0); err == nil {
			t.Fatalf("accepted unverified encoded bit depth %d", depth)
		}
	}
}
