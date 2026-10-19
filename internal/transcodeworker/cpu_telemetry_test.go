package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestCPUMeterChargesChildrenRunUnderItsContext(t *testing.T) {
	meter := &cpuMeter{}
	if meter.activeComputeMs() != nil {
		t.Fatal("a phase that ran nothing must report unknown CPU, not 0")
	}
	ctx := withCPUMeter(context.Background(), meter)
	for range 2 {
		if err := runCharged(ctx, exec.Command("/bin/sh", "-c", "i=0; while [ $i -lt 20000 ]; do i=$((i+1)); done")); err != nil {
			t.Fatal(err)
		}
	}
	// A failing child still spent CPU and is charged.
	_ = runCharged(ctx, exec.Command("/bin/sh", "-c", "exit 3"))
	got := meter.activeComputeMs()
	if got == nil || *got < 0 {
		t.Fatalf("measured children not charged: %v", got)
	}
	// A child outside the metered context charges nothing.
	other := &cpuMeter{}
	_ = runCharged(context.Background(), exec.Command("/bin/sh", "-c", "true"))
	if other.activeComputeMs() != nil {
		t.Fatal("an unmetered child was charged")
	}
}

func TestProbeSourceDetailsChargesProbePhase(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "ffprobe")
	script := "#!/bin/sh\necho '{\"streams\":[{\"index\":0,\"codec_type\":\"video\",\"codec_name\":\"h264\"}],\"format\":{\"duration\":\"10.0\"}}'\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	meter := &cpuMeter{}
	if _, err := ProbeSourceDetails(withCPUMeter(context.Background(), meter), probe, filepath.Join(dir, "in.mkv")); err != nil {
		t.Fatal(err)
	}
	if meter.activeComputeMs() == nil {
		t.Fatal("ffprobe child not charged to the probe phase")
	}
}

func TestTelemetryKeepsMeasuredCPUWhenTimestampsOwnDuration(t *testing.T) {
	start := time.Now().Add(-time.Minute).UTC()
	cpu := int64(4200)
	job := &JobRecord{
		Status:     "completed",
		CreatedAt:  start,
		StartedAt:  start,
		FinishedAt: start.Add(40 * time.Second),
	}
	job.EncodeStartedAt, job.EncodeFinishedAt = start, start.Add(30*time.Second)
	job.PhaseCosts = map[string]transcode.PhaseCost{
		"encode": {DurationMs: 1, Attempts: 1, ActiveComputeMs: &cpu},
		"probe":  {DurationMs: 5, Attempts: 1},
	}
	got := telemetryFor(job, time.Now())
	encode := got.PhaseCosts["encode"]
	if encode.DurationMs != 30000 || encode.ActiveComputeMs == nil || *encode.ActiveComputeMs != cpu {
		t.Fatalf("encode cost lost its CPU time: %+v", encode)
	}
	raw, _ := json.Marshal(got.PhaseCosts["probe"])
	var probe map[string]any
	_ = json.Unmarshal(raw, &probe)
	if _, ok := probe["active_compute_ms"]; ok {
		t.Fatalf("an unmeasured phase must omit active_compute_ms: %s", raw)
	}
}
