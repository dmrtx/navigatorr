package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// twoPassProfileConfig is a complete ephemeral profile with a two-pass x265
// ABR block; bitrate lets a test make it valid or not.
func twoPassProfileConfig(bitrateKbps int) map[string]any {
	profile := testEphemeralBatchProfileConfig()
	profile["video"] = map[string]any{
		"codec":        "libx265",
		"quality":      0,
		"preset":       "slow",
		"profile":      "main",
		"pixel_format": "yuv420p",
		"x265": map[string]any{
			"bitrate_kbps": bitrateKbps, "two_pass": true, "lookahead": 40, "b_frames": 4, "ref_frames": 4,
			"aq_mode": 2, "aq_strength": 1.0, "psy_rd": 2.0, "psy_rdoq": 1.0,
		},
	}
	return profile
}

func actionRows(t *testing.T, e *Engine) int {
	t.Helper()
	rows, err := e.deps.Store.ListActionInstances("", 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func requireInvalid(t *testing.T, err error, field string) *InvalidParamsError {
	t.Helper()
	invalid, ok := AsInvalidParams(err)
	if !ok {
		t.Fatalf("want InvalidParamsError for %q, got %v", field, err)
	}
	if invalid.Field != field {
		t.Fatalf("invalid field = %q (%s), want %q", invalid.Field, invalid.Reason, field)
	}
	return invalid
}

// A request the caller got wrong is refused before the source is read, the
// worker is asked anything, a row is written or the idempotency key is bound.
func TestInvalidProfileConfigIsRefusedBeforeAnyHashWorkerCallOrRow(t *testing.T) {
	mediaDir := t.TempDir()
	source := filepath.Join(mediaDir, "ep01.mkv")
	if err := os.WriteFile(source, []byte("synthetic source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	var capabilityCalls atomic.Int32
	mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		capabilityCalls.Add(1)
		return transcode.WorkerCapabilities{}, nil
	}}
	e, st := setupTranscodeEngine(t, mock, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()

	// Every hash slot is held: a hash attempt would block for good.
	for i := 0; i < mediaHashConcurrency; i++ {
		e.hashSlots <- struct{}{}
	}
	bad := map[string]any{"path": source, "profile_config": twoPassProfileConfig(10)}
	const key = "submission-one"
	ctx := context.Background()

	if _, err := e.EnqueueWorker(ctx, "transcode_media", bad, key); err == nil {
		t.Fatal("invalid two-pass profile_config admitted by EnqueueWorker")
	} else if invalid := requireInvalid(t, err, "profile_config"); !strings.Contains(invalid.Reason, "two-pass x265") {
		t.Fatalf("reason does not name the x265 settings: %s", invalid.Reason)
	}
	_, err := e.Enqueue(ctx, "transcode_media", bad, key)
	requireInvalid(t, err, "profile_config")
	_, err = e.Run(ctx, "transcode_media", bad, key)
	requireInvalid(t, err, "profile_config")

	if n := actionRows(t, e); n != 0 {
		t.Fatalf("%d action rows written for an invalid request", n)
	}
	if old, err := st.FindActionByIdempotencyKey("transcode_media", key); err != nil || old != nil {
		t.Fatalf("idempotency key reserved: %+v %v", old, err)
	}
	if capabilityCalls.Load() != 0 || atomic.LoadInt32(&mock.doctorCalls) != 0 {
		t.Fatalf("worker contacted: capabilities=%d doctor=%d", capabilityCalls.Load(), mock.doctorCalls)
	}
	// Nothing was queued, so the reconciler has nothing to hash even though
	// every slot is held.
	reconcileCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := e.ReconcileOnce(reconcileCtx); err != nil {
		t.Fatal(err)
	}

	// The same key then succeeds with the corrected request.
	good := map[string]any{"path": source, "profile_config": twoPassProfileConfig(4000)}
	res, err := e.EnqueueWorker(ctx, "transcode_media", good, key)
	if err != nil || res.IdempotencyKey != key {
		t.Fatalf("corrected request refused: %+v %v", res, err)
	}
	if n := actionRows(t, e); n != 1 {
		t.Fatalf("%d action rows after the corrected request, want 1", n)
	}
}

func TestEnqueueRefusesInvalidParamsWithoutPersisting(t *testing.T) {
	mediaDir := t.TempDir()
	source := filepath.Join(mediaDir, "ep01.mkv")
	if err := os.WriteFile(source, []byte("synthetic source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "ep01.mkv")
	mp3 := filepath.Join(mediaDir, "ep01-clean.mp3")
	digest := strings.Repeat("ab", 32)
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()

	podcast := func(extra map[string]any) map[string]any {
		inputs := map[string]any{"path": source, "podcast_id": "synthetic-show", "output_path": mp3}
		for k, v := range extra {
			inputs[k] = v
		}
		return inputs
	}
	tests := []struct {
		name   string
		action string
		inputs map[string]any
		field  string
		reason string
	}{
		{"benchmark with media_type", "benchmark_transcode", map[string]any{"path": source, "media_type": "movie"}, "media_type", "unsupported input"},
		{"unknown key", "transcode_media", map[string]any{"path": source, "bogus": 1}, "bogus", "unsupported input"},
		{"batch unknown key", "transcode_batch", map[string]any{"paths": []string{source}, "bogus": 1}, "bogus", "unsupported input"},
		{"missing path", "transcode_media", map[string]any{}, "path", "path is required"},
		{"blank path", "benchmark_transcode", map[string]any{"path": "  "}, "path", "path is required"},
		{"path outside roots", "transcode_media", map[string]any{"path": outside}, "path", "outside allowed read roots"},
		{"metric psnr", "benchmark_transcode", map[string]any{"path": source, "metric": "psnr"}, "metric", "invalid metric"},
		{"profile with profile_config", "transcode_media", map[string]any{"path": source, "profile": "general-hevc", "profile_config": twoPassProfileConfig(4000)}, "profile_config", "mutually exclusive"},
		{"undecodable profile_config", "benchmark_transcode", map[string]any{"path": source, "profile_config": map[string]any{"container": "mkv", "surprise": true}}, "profile_config", "invalid profile_config"},
		{"profile_config for a batch", "transcode_batch", map[string]any{"paths": []string{source}, "profile_config": twoPassProfileConfig(10)}, "profile_config", "two-pass x265"},
		{"preserve flag type", "transcode_media", map[string]any{"path": source, "preserve_source_bit_depth": "no"}, "preserve_source_bit_depth", "must be a boolean"},
		{"replace_original", "transcode_media", map[string]any{"path": source, "replace_original": true}, "replace_original", "candidate-only"},
		{"compact with allow_hardware", "transcode_media", map[string]any{"path": source, "mode": "compact", "allow_hardware": true}, "allow_hardware", "only supported with mode=size"},
		{"allow_hardware without mode", "transcode_media", map[string]any{"path": source, "allow_hardware": true}, "allow_hardware", "only supported with mode=size"},
		{"hardware margin outside size", "transcode_media", map[string]any{"path": source, "mode": "quality", "hardware_margin_percent": 10}, "hardware_margin_percent", "only supported with mode=size"},
		{"slow preset outside size", "transcode_media", map[string]any{"path": source, "mode": "compact", "allow_slow_preset": true}, "allow_slow_preset", "only supported with mode=size"},
		{"size budget outside size", "transcode_media", map[string]any{"path": source, "mode": "quality", "size_budget_percent": 40}, "size_budget_percent", "only supported with mode=size"},
		{"size budget out of range", "transcode_media", map[string]any{"path": source, "mode": "size", "size_budget_percent": 1}, "size_budget_percent", "must be between"},
		{"max_height without compact", "transcode_media", map[string]any{"path": source, "max_height": 720}, "max_height", "only supported with mode=compact"},
		{"max_height with size", "transcode_media", map[string]any{"path": source, "mode": "size", "max_height": 720}, "max_height", "only supported with mode=compact"},
		{"mode with profile", "transcode_media", map[string]any{"path": source, "mode": "size", "profile": "general-hevc"}, "profile", "cannot be combined"},
		{"batch compact with allow_hardware", "transcode_batch", map[string]any{"paths": []string{source}, "mode": "compact", "allow_hardware": true}, "allow_hardware", "only supported with mode=size"},
		{"batch paths mixed with a series", "transcode_batch", map[string]any{"paths": []string{source}, "service": "sonarr"}, "paths", "cannot be combined"},
		{"batch path outside roots", "transcode_batch", map[string]any{"paths": []string{outside}}, "paths", "outside allowed read roots"},
		{"batch promotion parallelism", "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10, "promotion_parallelism": 7}, "promotion_parallelism", "must be 1 or 2"},
		{"podcast without output_path", "clean_podcast_ads", map[string]any{"path": source, "podcast_id": "synthetic-show"}, "output_path", "output_path is required"},
		{"podcast without path", "clean_podcast_ads", map[string]any{"podcast_id": "synthetic-show", "output_path": mp3}, "path", "path is required"},
		{"podcast without podcast_id", "clean_podcast_ads", map[string]any{"path": source, "output_path": mp3}, "podcast_id", "podcast_id is required"},
		{"podcast unknown key", "clean_podcast_ads", podcast(map[string]any{"bogus": true}), "bogus", "unsupported input"},
		{"podcast output not mp3", "clean_podcast_ads", podcast(map[string]any{"output_path": filepath.Join(mediaDir, "ep01-clean.wav")}), "output_path", "separate MP3"},
		{"podcast output is the source", "clean_podcast_ads", podcast(map[string]any{"output_path": source}), "output_path", "separate MP3"},
		{"podcast output outside roots", "clean_podcast_ads", podcast(map[string]any{"output_path": outside + ".mp3"}), "output_path", "allowed"},
		{"podcast bad digest", "clean_podcast_ads", podcast(map[string]any{"source_sha256": "sha256:abc"}), "source_sha256", "64 hex"},
		{"podcast unconfigured show", "clean_podcast_ads", podcast(map[string]any{"source_sha256": "sha256:" + digest}), "podcast_id", "not enabled"},
	}
	ctx := context.Background()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := "key-" + strings.ReplaceAll(tc.name, " ", "-")
			for surface, call := range map[string]func() (*ActionResult, error){
				"Enqueue":       func() (*ActionResult, error) { return e.Enqueue(ctx, tc.action, tc.inputs, key) },
				"EnqueueWorker": func() (*ActionResult, error) { return e.EnqueueWorker(ctx, tc.action, tc.inputs, key) },
				"Run":           func() (*ActionResult, error) { return e.Run(ctx, tc.action, tc.inputs, key) },
			} {
				res, err := call()
				if res != nil {
					t.Fatalf("%s admitted an invalid request: %+v", surface, res)
				}
				invalid := requireInvalid(t, err, tc.field)
				if !strings.Contains(invalid.Reason, tc.reason) {
					t.Fatalf("%s reason %q does not contain %q", surface, invalid.Reason, tc.reason)
				}
			}
			if n := actionRows(t, e); n != 0 {
				t.Fatalf("%d rows written", n)
			}
			if old, err := st.FindActionByIdempotencyKey(tc.action, key); err != nil || old != nil {
				t.Fatalf("idempotency key reserved: %+v %v", old, err)
			}
		})
	}
}

// The podcast message names the input that is missing, not the resolver's.
func TestPodcastMissingOutputPathMessage(t *testing.T) {
	mediaDir := t.TempDir()
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()
	_, err := e.Enqueue(context.Background(), "clean_podcast_ads", map[string]any{"path": filepath.Join(mediaDir, "ep01.mp3"), "podcast_id": "synthetic-show"}, "k")
	invalid := requireInvalid(t, err, "output_path")
	if invalid.Reason != "output_path is required" {
		t.Fatalf("reason = %q", invalid.Reason)
	}
}

// Valid requests of every validated action still pass and are stored as given.
func TestValidRequestsAreAdmitted(t *testing.T) {
	mediaDir := t.TempDir()
	source := filepath.Join(mediaDir, "ep01.mkv")
	if err := os.WriteFile(source, []byte("synthetic source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()
	for i, tc := range []struct {
		action string
		inputs map[string]any
	}{
		{"transcode_media", map[string]any{"path": source, "mode": "size", "allow_hardware": false, "size_budget_percent": 50}},
		{"transcode_media", map[string]any{"path": source, "mode": "compact", "max_height": 720}},
		{"transcode_media", map[string]any{"path": source, "profile": "general-hevc", "metric": "SSIM"}},
		{"transcode_media", map[string]any{"path": source, "profile_config": twoPassProfileConfig(4000), "preserve_source_bit_depth": false}},
		{"benchmark_transcode", map[string]any{"path": source, "profile": "general-hevc", "metric": "vmaf+ssim"}},
		{"transcode_batch", map[string]any{"paths": []string{source}, "mode": "quality", "dry_run": true}},
	} {
		if _, err := e.Enqueue(context.Background(), tc.action, tc.inputs, "valid-"+string(rune('a'+i))); err != nil {
			t.Fatalf("%s %v: %v", tc.action, tc.inputs, err)
		}
	}
}

// Preflight judges the pinned bit depth with one comparison, so validation has
// to say up front what the request pins.
func TestValidationComputesPinnedBitDepth(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ten := twoPassProfileConfig(4000)
	ten["video"].(map[string]any)["profile"], ten["video"].(map[string]any)["pixel_format"] = "main10", "p010le"
	for name, tc := range map[string]struct {
		inputs map[string]any
		want   int
	}{
		"ten bit":              {map[string]any{"path": "/media/show/ep01.mkv", "profile_config": ten}, 10},
		"eight bit":            {map[string]any{"path": "/media/show/ep01.mkv", "profile_config": twoPassProfileConfig(4000)}, 8},
		"preserve disabled":    {map[string]any{"path": "/media/show/ep01.mkv", "profile_config": ten, "preserve_source_bit_depth": false}, 0},
		"named profile":        {map[string]any{"path": "/media/show/ep01.mkv", "profile": "general-hevc"}, 0},
		"mode owns the policy": {map[string]any{"path": "/media/show/ep01.mkv", "mode": "size"}, 0},
	} {
		req, err := e.validateTranscodeRequest("transcode_media", tc.inputs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if req.PinnedBitDepth != tc.want {
			t.Fatalf("%s: pinned bit depth %d, want %d", name, req.PinnedBitDepth, tc.want)
		}
	}
}

// The catalog and the validator read the same declarations: every declared
// input is accepted and every required one is enforced.
func TestCatalogDeclarationsDriveValidation(t *testing.T) {
	e := NewEngine(EngineDeps{})
	for _, name := range []string{"transcode_media", "benchmark_transcode", "clean_podcast_ads", "transcode_batch"} {
		tmpl, ok := e.GetTemplate(name)
		if !ok || tmpl.Validate == nil {
			t.Fatalf("%s has no validation hook", name)
		}
		declared := map[string]any{}
		for _, in := range append(append([]string{}, tmpl.RequiredInputs...), tmpl.OptionalInputs...) {
			declared[in] = nil
		}
		if err := unsupportedInput(tmpl, declared); err != nil {
			t.Fatalf("%s rejects a declared input: %v", name, err)
		}
		for _, required := range tmpl.RequiredInputs {
			inputs := map[string]any{}
			for _, other := range tmpl.RequiredInputs {
				inputs[other] = "x"
			}
			delete(inputs, required)
			requireInvalid(t, requiredInputs(tmpl, inputs), required)
		}
	}
}
