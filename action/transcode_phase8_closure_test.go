package action

// Phase 8 closure tests for the transcode optimization milestone.
//
// These tests lock the final integration guarantees required before the
// single PR towards main: the absence of worker-internal secrets/paths in
// persisted coordinator outputs, the worker API surface carrying no secret
// fields, and a fixed profile encoding without any sample job.
//
// All external boundaries are stubbed with the same clearly-separated test
// doubles used throughout this file's suite: a fake ffprobe shell script
// (createSmartFakeFFprobeScript) and mockTranscodeExecutor. Nothing here
// exercises real Apple Silicon hardware; live-hardware evidence is recorded
// by the acceptance suite.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// TestPhase8_PersistedOutputsLeakNoWorkerInternals runs a fixed-profile
// transcode_media action, then serializes its persisted outputs and asserts
// that no worker-internal secrets or scratch paths leak into
// coordinator-visible state: no run tokens, no file-lock names, no
// samples/scratch directories, and no worker persistence filenames.
// Legitimate identifiers (plan digests, media and candidate paths) must
// remain present.
func TestPhase8_PersistedOutputsLeakNoWorkerInternals(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, Progress: 100}, nil
		},
	}
	engine, _, media, _ := setupProbeTestEnv(t, mock, standard8BitProbeJSON)
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    media,
		"profile": "hevc-vt",
	})
	if err != nil {
		t.Fatalf("transcode run error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected transcode StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}

	forbidden := []string{
		"run_token", "runToken", "RunToken", "RUN_TOKEN",
		".lock", "/samples/", "samples\\",
		"benchmark.json", "job.json", ".tmp",
		"process_start_time", "ProcessStartTime",
	}
	raw, err := json.Marshal(res.Outputs)
	if err != nil {
		t.Fatalf("marshaling outputs: %v", err)
	}
	lowered := strings.ToLower(string(raw))
	for _, needle := range forbidden {
		if strings.Contains(lowered, strings.ToLower(needle)) {
			t.Errorf("persisted outputs leak worker-internal %q: %s", needle, string(raw))
		}
	}
	if getString(res.Outputs, "plan_digest") == "" {
		t.Errorf("transcode_media outputs missing plan_digest")
	}
}

// TestPhase8_BenchmarkAPITypesExposeNoSecrets guards the public coordinator/
// worker contract against secret leakage at the type level: none of the
// benchmark status, decision, winner, or per-candidate evaluation structs
// may carry a token/secret field (name or JSON tag). Worker run tokens must
// stay inside benchmark.json on the worker and never cross the protocol.
func TestPhase8_BenchmarkAPITypesExposeNoSecrets(t *testing.T) {
	types := []any{
		transcode.BenchmarkStatus{},
		transcode.BenchmarkDecision{},
		transcode.BenchmarkWinner{},
		transcode.BenchmarkCandidateEvaluation{},
		transcode.BenchmarkJob{},
		transcode.BenchmarkSubmitResponse{},
		transcode.BenchmarkCancelResponse{},
	}
	for _, v := range types {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
			if strings.Contains(name, "token") || strings.Contains(name, "secret") {
				t.Errorf("%s field %s looks like a leaked secret carrier (json tag %q)",
					rt.Name(), f.Name, f.Tag.Get("json"))
			}
		}
	}
}

const liveValidation8BitProbeJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "profile": "High", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bit_rate": "5000000"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "750000000", "bit_rate": "5000000"},
  "chapters": []
}`

// TestPhase8_BuiltinFixedProfile_SubmitsStaticPlan tests that a
// builtin profile without an optimization block and without metric input skips benchmark
// and proceeds with legacy static transcode unchanged.
func TestPhase8_BuiltinFixedProfile_SubmitsStaticPlan(t *testing.T) {
	var fullTranscodeReq transcode.Request
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			fullTranscodeReq = req
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output-static"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupProbeTestEnv(t, mock, liveValidation8BitProbeJSON)

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "anime-hevc-quality",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}

	if getBool(res.Outputs, "optimization_enabled") {
		t.Errorf("outputs unexpectedly reported optimization_enabled=true when no metric requested")
	}
	if atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Errorf("expected 1 full transcode submit call, got %d", atomic.LoadInt32(&mock.submitCalls))
	}
	const expectedStaticBaseDigest = "sha256:83bdf39d0e2b5f51ab7c5ccf4ab9d8bd564307455b144d1ec86ab5fd96e0c55b"
	if fullTranscodeReq.Plan == nil || fullTranscodeReq.Plan.PlanDigest != expectedStaticBaseDigest {
		t.Errorf("expected full submit to use static base digest %s, got %v", expectedStaticBaseDigest, fullTranscodeReq.Plan)
	}
}
