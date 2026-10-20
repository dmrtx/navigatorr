package action

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode/recipe"
)

const (
	hdrProbeJSON  = `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","pix_fmt":"yuv420p10le","width":3840,"height":2160,"avg_frame_rate":"24/1","r_frame_rate":"24/1","color_transfer":"smpte2084","color_primaries":"bt2020","color_space":"bt2020nc","bits_per_raw_sample":"10"}],"format":{"format_name":"matroska","duration":"100","size":"100"},"chapters":[]}`
	sdrProbeJSON  = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"100"},"chapters":[]}`
	deepProbeJSON = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p12le","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"12"}],"format":{"format_name":"matroska","duration":"100","size":"100"},"chapters":[]}`
)

func writeProbeScript(t *testing.T, dir, json string) string {
	t.Helper()
	probe := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\ncat <<'JSON'\n"+json+"\nJSON\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return probe
}

// preflightWithHashSlotsHeld runs preflight with every hash slot taken, so a
// source hash can never start: it would wait on a slot until the context ends
// and fail the step.
func preflightWithHashSlotsHeld(t *testing.T, probeJSON, mode string, content []byte) (StepResult, *ExecutionContext) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{capabilitiesFunc: nil}, writeProbeScript(t, dir, probeJSON), []string{dir}, []string{dir}, false)
	defer st.Close()
	for i := 0; i < mediaHashConcurrency; i++ {
		e.hashSlots <- struct{}{}
	}
	ec := &ExecutionContext{InstanceID: "no-hash-" + mode, ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": mode}, State: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := e.stepTranscodePreflight(ctx, ec)
	if err != nil {
		t.Fatal(err)
	}
	return result, ec
}

func TestPreflightProbeExitsSkipSourceHash(t *testing.T) {
	for _, tc := range []struct {
		name, probe, mode, reason string
	}{
		{"HDR source in size mode", hdrProbeJSON, "size", "hdr_requires_compact"},
		{"HDR source in quality mode", hdrProbeJSON, "quality", "hdr_requires_compact"},
		{"unsupported bit depth", deepProbeJSON, "size", "unsupported_source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, ec := preflightWithHashSlotsHeld(t, tc.probe, tc.mode, make([]byte, 100))
			if result.Status != StepCompleted || getString(result.Outputs, "reason_code") != tc.reason || !getBool(ec.State, "skip_transcode") {
				t.Fatalf("probe exit %+v state=%+v", result, ec.State)
			}
			if _, ok := ec.State["original_sha256"]; ok {
				t.Fatal("early exit recorded a source hash")
			}
			if _, ok := result.Outputs["original_sha256"]; ok {
				t.Fatal("early exit reported a source hash")
			}
			if _, hashed := phaseCostMap(ec)["coordinator_source_hash"]; hashed {
				t.Fatal("early exit recorded a source hash phase")
			}
			if getString(ec.State, "resolved_path") == "" || ec.State["original"] == nil {
				t.Fatalf("early exit lost the source identity: %+v", ec.State)
			}
		})
	}
}

func TestPreflightStillHashesSourcesThatSubmitWork(t *testing.T) {
	content := []byte("source bytes that will be encoded")
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	defer st.Close()
	ec := &ExecutionContext{InstanceID: "hash-normal", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size"}, State: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	result, err := e.stepTranscodePreflight(context.Background(), ec)
	if err != nil || result.Status != StepCompleted || getBool(ec.State, "skip_transcode") {
		t.Fatalf("preflight %+v %v", result, err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(content))
	if got := getString(ec.State, "original_sha256"); got != want {
		t.Fatalf("baseline hash %q, want %q", got, want)
	}
	if getString(result.Outputs, "original_sha256") != want {
		t.Fatalf("output hash %q", getString(result.Outputs, "original_sha256"))
	}
	if orig, _ := ec.State["original"].(map[string]any); orig["sha256"] != want {
		t.Fatalf("original summary lost its hash: %+v", orig)
	}
	if _, hashed := phaseCostMap(ec)["coordinator_source_hash"]; !hashed {
		t.Fatal("source hash phase not recorded")
	}
}

func TestPreflightHashFailsWhenSourceChangesAfterProbe(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	defer st.Close()
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("replaced after the probe"), 0600); err != nil {
		t.Fatal(err)
	}
	ec := &ExecutionContext{InstanceID: "hash-changed", ActionName: "transcode_media", State: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", map[string]any{}, ec.State)
	if sha, res := e.hashPreflightSource(context.Background(), ec, file, fi); res == nil || res.Status != StepFailed || sha != "" {
		t.Fatalf("source replaced after the probe was hashed: %q %+v", sha, res)
	}
}

func TestAcceptSkipsOriginalVerificationOnlyWithoutWork(t *testing.T) {
	content := []byte("original bytes")
	sum := fmt.Sprintf("%x", sha256.Sum256(content))
	e := NewEngine(EngineDeps{})
	for _, tc := range []struct {
		name     string
		state    map[string]any
		sha      string
		verified bool
	}{
		{"probe-only skip", map[string]any{"skip_transcode": true, "reason_code": "hdr_requires_compact"}, "", false},
		{"probe-only skip that kept a baseline", map[string]any{"skip_transcode": true, "reason_code": "unsupported_source"}, sum, false},
		{"skip after a worker job", map[string]any{"skip_transcode": true, "job_id": "job-1"}, sum, true},
		{"skip after a benchmark job", map[string]any{"skip_transcode": true, "benchmark_job_id": "bench-1"}, sum, true},
		{"skip with a candidate", map[string]any{"skip_transcode": true, "candidate_path": "/c.mkv"}, sum, true},
		{"skip with an external reference", map[string]any{"skip_transcode": true, "external_reference": "job-1"}, sum, true},
		{"skip after a transcode job", map[string]any{"skip_transcode": true, "transcode_job_id": "job-1"}, sum, true},
		{"skip after a submission", map[string]any{"skip_transcode": true, "transcode_submitted": true}, sum, true},
		{"skip with an output path", map[string]any{"skip_transcode": true, "output_path": "/c.mkv"}, sum, true},
		{"produced candidate", map[string]any{"job_id": "job-1", "candidate_path": "/c.mkv"}, sum, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, changed := range []bool{false, true} {
				p := filepath.Join(t.TempDir(), "original.mkv")
				if err := os.WriteFile(p, content, 0600); err != nil {
					t.Fatal(err)
				}
				if changed {
					if err := os.WriteFile(p, []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				ec := testReconcileEC("")
				ec.State["resolved_path"] = p
				ec.State["original_sha256"] = tc.sha
				if tc.sha == "" {
					delete(ec.State, "original_sha256")
				}
				for k, v := range tc.state {
					ec.State[k] = v
				}
				r, err := e.stepTranscodeAccept(context.Background(), ec)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case !tc.verified:
					// Untouched original: completes with or without a baseline,
					// and never claims a verification that did not happen.
					if r.Status != StepCompleted || r.Outputs["original_integrity"] != "not_applicable" || ec.State["original_integrity"] == "verified" {
						t.Fatalf("changed=%v: %+v", changed, r)
					}
					if _, claimed := ec.State["validation"]; claimed {
						t.Fatalf("unverified skip recorded a validation: %+v", ec.State["validation"])
					}
				case changed:
					if r.Status != StepFailed {
						t.Fatalf("changed original accepted: %+v", r)
					}
				default:
					if r.Status != StepCompleted || ec.State["original_integrity"] != "verified" {
						t.Fatalf("unchanged original not verified: %+v", r)
					}
				}
			}
		})
	}
}

func TestAcceptMissingBaselineStillFailsClosedWithoutSkip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "original.mkv")
	if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{})
	ec := testReconcileEC("")
	ec.State["resolved_path"] = p
	delete(ec.State, "original_sha256")
	if r, err := e.stepTranscodeAccept(context.Background(), ec); err != nil || r.Status != StepFailed {
		t.Fatalf("missing baseline accepted: %+v %v", r, err)
	}
	rejected := testReconcileEC("")
	rejected.Decision = "reject"
	rejected.State["resolved_path"], rejected.State["skip_transcode"] = p, true
	if r, err := e.stepTranscodeAccept(context.Background(), rejected); err != nil || r.Status != StepFailed {
		t.Fatalf("rejected decision completed: %+v %v", r, err)
	}
}

func TestWorkIsNeverSubmittedWithoutSourceBaseline(t *testing.T) {
	e := NewEngine(EngineDeps{Transcode: &mockTranscodeExecutor{}})
	ec := testReconcileEC("")
	delete(ec.State, "original_sha256")
	if r, err := e.stepTranscodeSubmit(context.Background(), ec); err != nil || r.Status != StepFailed {
		t.Fatalf("encode submitted without a baseline: %+v %v", r, err)
	}
	bench := &ExecutionContext{InstanceID: "bench", ActionName: "transcode_media", Inputs: map[string]any{}, State: map[string]any{"optimization_enabled": true, "resolved_path": "/media/ep.mkv"}}
	if r, err := e.stepBenchmarkSubmit(context.Background(), bench); err != nil || r.Status != StepFailed {
		t.Fatalf("benchmark submitted without a baseline: %+v %v", r, err)
	}
	if err := e.applySharedBatchCalibration(&ExecutionContext{ActionName: "transcode_media"}, nil, nil, recipe.Profile{}, "", "digest", "/media/ep.mkv"); err == nil {
		t.Fatal("shared calibration accepted an empty source hash")
	}
}
