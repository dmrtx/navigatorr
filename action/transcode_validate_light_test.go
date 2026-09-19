package action

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// The coordinator's post-publish validation is deliberately lightweight: it
// performs only stat-level identity checks and never runs ffprobe/mediainspect.
// These tests use a deliberately-broken ffprobe path to prove no heavy
// inspection happens, and cover the direct-SMB logical-path case where the
// semantic destination is not mounted.

func lightValidateEngine(t *testing.T) *Engine {
	t.Helper()
	return NewEngine(EngineDeps{Ffprobe: filepath.Join(t.TempDir(), "ffprobe-that-must-never-run")})
}

func lightValidateEC(candidatePath string, candidateSize int64, storageBackend string) *ExecutionContext {
	state := map[string]any{
		"candidate_path":       candidatePath,
		"plan":                 &transcode.Plan{Container: "mkv", VideoCodec: "hevc_videotoolbox", ExpectedBitDepth: 8},
		"candidate_size_bytes": candidateSize,
		"original":             map[string]any{"size_bytes": int64(100000), "duration_sec": 60.0, "resolution": "1920x1080", "bit_depth": 8},
		"profile":              "hevc-vt",
		"recipe_version":       "v1",
	}
	if storageBackend != "" {
		state["storage_backend"] = storageBackend
	}
	return &ExecutionContext{
		State:   state,
		Inputs:  map[string]any{},
		Outputs: map[string]any{},
	}
}

func TestTranscodeValidatePostPublishIsLightweight(t *testing.T) {
	dir := t.TempDir()
	cand := filepath.Join(dir, "cand.mkv")
	content := []byte("candidate-bytes")
	if err := os.WriteFile(cand, content, 0o644); err != nil {
		t.Fatal(err)
	}
	e := lightValidateEngine(t)
	ec := lightValidateEC(cand, int64(len(content)), "local")
	res, err := e.stepTranscodeValidate(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepCompleted {
		t.Fatalf("expected lightweight validation to complete without ffprobe, got %s (%s)", res.Status, res.Error)
	}
	if res.Outputs["candidate_path"] != cand {
		t.Fatalf("candidate_path output = %v", res.Outputs["candidate_path"])
	}
	if got := res.Outputs["size_saved_bytes"]; got != int64(100000-len(content)) {
		t.Fatalf("size_saved_bytes = %v", got)
	}
}

func TestTranscodeValidateRejectsSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	cand := filepath.Join(dir, "cand.mkv")
	if err := os.WriteFile(cand, []byte("candidate-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := lightValidateEngine(t)
	ec := lightValidateEC(cand, 99999, "local")
	res, err := e.stepTranscodeValidate(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepFailed {
		t.Fatalf("expected size mismatch failure, got %s", res.Status)
	}
}

func TestTranscodeValidateRejectsMissingCandidate(t *testing.T) {
	e := lightValidateEngine(t)
	ec := lightValidateEC(filepath.Join(t.TempDir(), "missing.mkv"), 0, "local")
	res, err := e.stepTranscodeValidate(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepFailed {
		t.Fatalf("expected missing candidate failure, got %s", res.Status)
	}
}

func TestTranscodeValidateSMBDirectNeverStatsLogicalPath(t *testing.T) {
	// The semantic destination is a logical SMB name that is NOT mounted; a
	// stat would fail. The worker's hash-verified publish is the verification.
	e := lightValidateEngine(t)
	ec := lightValidateEC("//server/media/movie/candidate.mkv", 1234, "smb_direct")
	res, err := e.stepTranscodeValidate(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepCompleted {
		t.Fatalf("expected smb_direct lightweight validation to complete, got %s (%s)", res.Status, res.Error)
	}
	if res.Outputs["candidate_path"] != "//server/media/movie/candidate.mkv" {
		t.Fatalf("candidate_path output = %v", res.Outputs["candidate_path"])
	}
}
