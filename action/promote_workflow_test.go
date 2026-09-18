package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

type promotionHarness struct {
	t                                  *testing.T
	mu                                 sync.Mutex
	engine                             *Engine
	st                                 *store.Store
	root, original, candidate, final   string
	files                              map[int]promotionFile
	episodes                           []promotionEpisode
	commands                           map[int]promotionCommandResponse
	nextCommand                        int
	imports, deletes, renames, rescans int
	mutationOrder                      []string
	dropImportResponse                 bool
	acceptDroppedImport                bool
	importDeletesOriginal              bool
	renameFailures                     int
	poisonOldPath                      bool
	poisonCandidateOnImport            bool
	backupSeen                         bool
}

func newPromotionHarness(t *testing.T) *promotionHarness {
	t.Helper()
	// t.TempDir returns the /var/folders alias on darwin; canonicalize only the
	// fixture root so the production symlink guard still sees real paths.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &promotionHarness{t: t, st: setupTestStore(t), root: root, files: map[int]promotionFile{}, commands: map[int]promotionCommandResponse{}, nextCommand: 1, acceptDroppedImport: true}
	season := filepath.Join(h.root, "Series", "Season 1")
	h.original = filepath.Join(season, "Series S01E01-E02.mp4")
	h.candidate = filepath.Join(season, ".navigatorr-candidates", "candidate.mkv")
	h.final = filepath.Join(season, "Series S01E01-E02.mkv")
	if err := os.MkdirAll(filepath.Dir(h.candidate), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.original, []byte(strings.Repeat("original-", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.candidate, []byte(strings.Repeat("candidate-", 20)), 0600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(h.root, "ffprobe")
	probeJSON := `{"format":{"duration":"60","format_name":"matroska"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","width":1920,"height":1080,"pix_fmt":"yuv420p","bits_per_raw_sample":"8"}],"chapters":[]}`
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s' '"+probeJSON+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	old := promotionFile{ID: 101, SeriesID: 1, Path: h.original, Quality: json.RawMessage(`{"quality":{"id":3,"name":"WEBDL-1080p"},"revision":{"version":1,"real":0,"isRepack":false}}`), Languages: json.RawMessage(`[{"id":1,"name":"English"}]`)}
	old.Size = int64(len(strings.Repeat("original-", 100)))
	old.MediaInfo.VideoCodec = "AVC"
	h.files[101] = old
	h.episodes = []promotionEpisode{{ID: 11, SeriesID: 1, EpisodeFileID: 101}, {ID: 12, SeriesID: 1, EpisodeFileID: 101}}
	server := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(server.Close)
	cfg := &config.Config{AllowDestructive: true, Services: map[string]config.ServiceConfig{"sonarr": {URL: server.URL, APIVersion: "/api/v3"}}}
	resolver, err := fsop.NewResolver([]string{h.root}, []string{h.root})
	if err != nil {
		t.Fatal(err)
	}
	h.engine = NewEngine(EngineDeps{Store: h.st, Config: cfg, Registry: arrservice.NewRegistry(cfg), Fs: resolver, Ffprobe: probe})
	h.engine.registerPromoteTranscodeTemplate()
	originalBytes, err := os.ReadFile(h.original)
	if err != nil {
		t.Fatal(err)
	}
	originalSHA := sha256.Sum256(originalBytes)
	sourceState := map[string]any{
		"resolved_path": h.original, "candidate_path": h.candidate, "original_sha256": hex.EncodeToString(originalSHA[:]), "original_intact": true,
		"plan":     &transcode.Plan{VideoCodec: "hevc_videotoolbox", ExpectedBitDepth: 8, AudioMode: "copy"},
		"original": map[string]any{"size_bytes": len(originalBytes), "duration_sec": 60, "video": []mediainspect.DetailedStream{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}}, "audio": []any{}, "subtitles": []any{}, "attachments": []any{}, "chapters": 0},
	}
	if err := h.st.CreateActionInstance(store.ActionInstance{ID: "source-transcode", ActionName: "transcode_media", Status: StatusCompleted, StateJSON: toJSON(sourceState), InputsJSON: `{"expected_video_codec":"hevc"}`}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *promotionHarness) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(value any) {
		if err := json.NewEncoder(w).Encode(value); err != nil {
			h.t.Error(err)
		}
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v3/series/1":
		write(map[string]any{"id": 1, "path": filepath.Join(h.root, "Series")})
	case r.Method == "GET" && r.URL.Path == "/api/v3/episode":
		write(h.episodes)
	case r.Method == "GET" && r.URL.Path == "/api/v3/episodefile":
		files := []promotionFile{}
		for _, f := range h.files {
			files = append(files, f)
		}
		write(files)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/episodefile/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/episodefile/"))
		f, ok := h.files[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if h.poisonOldPath && id == 101 {
			f.Path = h.candidate
		}
		write(f)
	case r.Method == "DELETE" && r.URL.Path == "/api/v3/episodefile/101":
		for _, ep := range h.episodes {
			if ep.EpisodeFileID != 202 {
				h.t.Errorf("DELETE before episode %d adopted candidate", ep.ID)
			}
		}
		if !h.backupSeen {
			h.t.Error("DELETE before verified recovery copy")
		}
		h.deletes++
		h.mutationOrder = append(h.mutationOrder, "delete")
		delete(h.files, 101)
		_ = os.Remove(h.original)
		write(map[string]any{})
	case r.Method == "GET" && r.URL.Path == "/api/v3/command":
		commands := []promotionCommandResponse{}
		for _, c := range h.commands {
			commands = append(commands, c)
		}
		write(commands)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/command/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/command/"))
		c, ok := h.commands[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		write(c)
	case r.Method == "POST" && r.URL.Path == "/api/v3/command":
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			h.t.Error(err)
			w.WriteHeader(400)
			return
		}
		name := getString(payload, "name")
		cmd := promotionCommandResponse{ID: h.nextCommand, Name: name, Status: "completed", Body: payload}
		h.nextCommand++
		switch name {
		case "ManualImport":
			h.imports++
			h.mutationOrder = append(h.mutationOrder, "import")
			instances, err := h.st.ListActionInstances("", 100)
			if err != nil {
				h.t.Error(err)
			}
			for _, inst := range instances {
				if inst.ActionName != "promote_transcode_candidate" || inst.Status != StatusRunning {
					continue
				}
				p, err := loadPromotion(parseExecutionContext(&inst, h.engine))
				if err != nil {
					continue
				}
				if p.Commands["import"] == nil || p.Commands["import"].SentAt == "" {
					h.t.Error("import sent before durable intent")
				}
				if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
					h.t.Errorf("import before recovery verification: %v", err)
				} else {
					h.backupSeen = true
				}
			}
			files, _ := payload["files"].([]any)
			if len(files) != 1 {
				h.t.Errorf("invalid import files: %v", payload)
			}
			if len(files) == 1 {
				f, _ := files[0].(map[string]any)
				ids, _ := f["episodeIds"].([]any)
				if len(ids) != 2 {
					h.t.Errorf("multi-episode import lost IDs: %v", f)
				}
			}
			if !h.dropImportResponse || h.acceptDroppedImport {
				candidate, err := os.Stat(h.candidate)
				if err != nil {
					h.t.Error(err)
					return
				}
				file := promotionFile{ID: 202, SeriesID: 1, Path: h.candidate, Size: candidate.Size()}
				file.MediaInfo.VideoCodec = "HEVC"
				h.files[202] = file
				for i := range h.episodes {
					h.episodes[i].EpisodeFileID = 202
				}
				if h.importDeletesOriginal {
					delete(h.files, 101)
					_ = os.Remove(h.original)
				}
				if h.poisonCandidateOnImport {
					_ = os.WriteFile(h.candidate, []byte("damaged"), 0600)
				}
				h.commands[cmd.ID] = cmd
			}
			if h.dropImportResponse {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					h.t.Error(err)
				} else {
					_ = conn.Close()
				}
				return
			}
		case "RenameFiles":
			h.renames++
			h.mutationOrder = append(h.mutationOrder, "rename")
			if h.renameFailures > 0 {
				h.renameFailures--
				cmd.Status = "failed"
			} else {
				if err := os.Rename(h.candidate, h.final); err != nil {
					h.t.Error(err)
					cmd.Status = "failed"
				} else {
					f := h.files[202]
					f.Path = h.final
					h.files[202] = f
				}
			}
		case "RescanSeries":
			h.rescans++
			h.mutationOrder = append(h.mutationOrder, "rescan")
		default:
			h.t.Errorf("unexpected command %q", name)
			w.WriteHeader(400)
			return
		}
		h.commands[cmd.ID] = cmd
		write(cmd)
	default:
		h.t.Errorf("unexpected Sonarr request %s %s", r.Method, r.URL)
		w.WriteHeader(404)
	}
}

func (h *promotionHarness) run() *ActionResult {
	h.t.Helper()
	result, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-transcode", "series_id": 1})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *promotionHarness) resume(id, decision string) *ActionResult {
	h.t.Helper()
	result, err := h.engine.Resume(context.Background(), id, decision, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *promotionHarness) finish(r *ActionResult) *ActionResult {
	h.t.Helper()
	for i := 0; i < 12 && r.Status == StatusWaitingExternal; i++ {
		r = h.resume(r.ID, "")
	}
	return r
}

func (h *promotionHarness) restart() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.engine = NewEngine(h.engine.Deps())
	h.engine.registerPromoteTranscodeTemplate()
}

func TestPromotionRequiresApprovalAndDeletesOnlyAfterAdoption(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	if r.Status != StatusWaitingDecision || h.imports != 0 {
		t.Fatalf("before approval: %+v imports=%d", r, h.imports)
	}
	if r = h.resume(r.ID, ""); r.Status != StatusWaitingDecision || h.imports != 0 {
		t.Fatal("empty resume bypassed approval")
	}
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: status=%s error=%s waiting=%s", r.Status, r.Error, r.WaitingReason)
	}
	if strings.Join(h.mutationOrder, ",") != "import,delete,rename,rescan" {
		t.Fatalf("order %v", h.mutationOrder)
	}
	if !getBool(r.Outputs, "promoted") || getBool(r.Outputs, "recovery_retained") || getInt64(r.Outputs, "size_saved_bytes") != 700 {
		t.Fatalf("outputs: %v", r.Outputs)
	}
	if _, err := os.Stat(h.original); !os.IsNotExist(err) {
		t.Fatalf("original remains: %v", err)
	}
	if _, err := os.Stat(h.candidate); !os.IsNotExist(err) {
		t.Fatalf("temporary candidate remains: %v", err)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.BackupPath); !os.IsNotExist(err) {
		t.Fatalf("backup remains after success: %v", err)
	}
	// Completed resume and a new request for the same source both remain
	// idempotent, including after a durable-store reload.
	h.restart()
	if r2 := h.resume(r.ID, "approve"); r2.ID != r.ID || r2.Status != StatusCompleted {
		t.Fatalf("duplicate resume: %+v", r2)
	}
	if r2 := h.run(); r2.ID != r.ID {
		t.Fatalf("duplicate Run created %s instead of %s", r2.ID, r.ID)
	}
	if h.imports != 1 || h.deletes != 1 || h.renames != 1 {
		t.Fatalf("duplicate mutation: import=%d delete=%d rename=%d", h.imports, h.deletes, h.renames)
	}
}

func TestPromotionRejectsChangedOriginalAfterApproval(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	if err := os.WriteFile(h.original, []byte("changed-original"), 0600); err != nil {
		t.Fatal(err)
	}
	r = h.resume(r.ID, "approve")
	if r.Status != StatusFailed || h.imports != 0 || !strings.Contains(r.Error, "SHA-256") {
		t.Fatalf("changed original: %+v imports=%d", r, h.imports)
	}
}

func TestPromotionDoesNotCaptureModifiedCandidateAsValidatedBaseline(t *testing.T) {
	h := newPromotionHarness(t)
	probe := h.engine.deps.Ffprobe
	script, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	// Return a valid probe result, but modify the candidate before the
	// validator returns. A post-validation-only hash would accept those
	// different bytes as if the probe had validated them.
	script = append(script, []byte("for promotion_test_path in \"$@\"; do :; done\nprintf damage >> \"$promotion_test_path\"\n")...)
	if err := os.WriteFile(probe, script, 0700); err != nil {
		t.Fatal(err)
	}
	r := h.run()
	if r.Status != StatusFailed || !strings.Contains(r.Error, "SHA-256") || h.imports != 0 {
		t.Fatalf("modified candidate baseline accepted: %s %s imports=%d", r.Status, r.Error, h.imports)
	}
}

func TestPromotionResumesAcceptedImportAfterLostResponse(t *testing.T) {
	h := newPromotionHarness(t)
	h.dropImportResponse = true
	r := h.run()
	r = h.resume(r.ID, "approve")
	if r.Status != StatusWaitingExternal || h.imports != 1 {
		t.Fatalf("lost response: %s %s imports=%d", r.Status, r.Error, h.imports)
	}
	h.restart()
	r = h.finish(r)
	if r.Status != StatusCompleted || h.imports != 1 {
		t.Fatalf("reconcile import: %s %s imports=%d", r.Status, r.Error, h.imports)
	}
}

func TestPromotionNeverResubmitsAnUncertainUnobservedImport(t *testing.T) {
	h := newPromotionHarness(t)
	h.dropImportResponse = true
	h.acceptDroppedImport = false
	r := h.run()
	r = h.resume(r.ID, "approve")
	for i := 0; i < 2; i++ {
		r = h.resume(r.ID, "")
	}
	if h.imports != 1 || h.deletes != 0 {
		t.Fatalf("blind retry: import=%d delete=%d", h.imports, h.deletes)
	}
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, h.engine)
	p, err := loadPromotion(ec)
	if err != nil {
		t.Fatal(err)
	}
	p.Commands["import"].SentAt = time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano)
	ec.State["promotion"] = p
	inst.StateJSON = toJSON(ec.State)
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	r = h.resume(r.ID, "")
	if r.Status != StatusWaitingDecision || h.imports != 1 {
		t.Fatalf("uncertain import did not stop: %s %s", r.Status, r.WaitingReason)
	}
	r = h.resume(r.ID, "reconcile")
	if r.Status != StatusWaitingDecision || h.imports != 1 {
		t.Fatal("reconcile decision repeated the import")
	}
	if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatalf("recovery missing: %v", err)
	}
}

func TestPromotionRetainsRecoveryAndRetriesConfirmedRenameFailure(t *testing.T) {
	h := newPromotionHarness(t)
	h.renameFailures = 1
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusFailed || h.imports != 1 || h.renames != 1 {
		t.Fatalf("rename failure: %s %s", r.Status, r.Error)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
		t.Fatal(err)
	}
	r, err = h.engine.Retry(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = h.finish(r)
	if r.Status != StatusCompleted || h.imports != 1 || h.deletes != 1 || h.renames != 2 {
		t.Fatalf("rename retry: %s %s imports=%d deletes=%d renames=%d", r.Status, r.Error, h.imports, h.deletes, h.renames)
	}
}

func TestPromotionRecoversWhenSonarrDeletesOriginalDuringImport(t *testing.T) {
	h := newPromotionHarness(t)
	h.importDeletesOriginal = true
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted || !h.backupSeen || h.deletes != 0 {
		t.Fatalf("Sonarr internal deletion: %s %s", r.Status, r.Error)
	}
}

func TestPromotionBlocksOldIDPathDriftAndDamagedImport(t *testing.T) {
	for _, mode := range []string{"old-id-path", "candidate-content"} {
		t.Run(mode, func(t *testing.T) {
			h := newPromotionHarness(t)
			h.poisonOldPath = mode == "old-id-path"
			h.poisonCandidateOnImport = mode == "candidate-content"
			r := h.run()
			r = h.finish(h.resume(r.ID, "approve"))
			if r.Status != StatusFailed || h.deletes != 0 {
				t.Fatalf("unsafe deletion after %s: %s %s", mode, r.Status, r.Error)
			}
			p, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.engine.verifyPromotionHash(context.Background(), p.BackupPath, p.OriginalSHA); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPromotionRejectsSymlinkRecoveryAndDestructiveDisabled(t *testing.T) {
	t.Run("destructive-disabled", func(t *testing.T) {
		h := newPromotionHarness(t)
		h.engine.deps.Config.AllowDestructive = false
		r := h.run()
		if r.Status != StatusFailed || h.imports != 0 {
			t.Fatalf("destructive guard: %+v", r)
		}
	})
	t.Run("symlink-recovery", func(t *testing.T) {
		h := newPromotionHarness(t)
		r := h.run()
		recoveryDir := filepath.Join(filepath.Dir(h.candidate), ".promotion-recovery")
		other := filepath.Join(h.root, "other")
		if err := os.Mkdir(other, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, recoveryDir); err != nil {
			t.Fatal(err)
		}
		r = h.resume(r.ID, "approve")
		if r.Status != StatusFailed || h.imports != 0 {
			t.Fatalf("symlink recovery: %s %s", r.Status, r.Error)
		}
	})
}

func TestPromotionRejectedDecisionKeepsBothFiles(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	r = h.resume(r.ID, "reject")
	if r.Status != StatusFailed || h.imports != 0 {
		t.Fatalf("reject: %s %s", r.Status, r.Error)
	}
	for _, path := range []string{h.original, h.candidate} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(fmt.Sprintf("file removed after rejection: %s %v", path, err))
		}
	}
}

func TestPromotionMatchesJournaledCommandMetadata(t *testing.T) {
	want := map[string]any{"name": "ManualImport", "importMode": "copy", "files": []any{map[string]any{"path": "/series/.navigatorr-candidates/a.mkv", "episodeIds": []int{11, 12}, "quality": json.RawMessage(`{"revision":{"version":1,"real":0},"quality":{"name":"HD","id":3}}`)}}}
	var got map[string]any
	if err := json.Unmarshal([]byte(`{"name":"manualimport","importMode":"Copy","files":[{"path":"/series/.navigatorr-candidates/a.mkv","episodeIds":[11,12],"quality":{"quality":{"id":3,"name":"HD","source":"web"},"revision":{"real":0,"version":1}},"downloadId":null}],"sendUpdatesToClient":true}`), &got); err != nil {
		t.Fatal(err)
	}
	if !promotionPayloadMatches(want, got) {
		t.Fatal("matching command with reordered/extra metadata was not recognized")
	}
	got["files"].([]any)[0].(map[string]any)["episodeIds"] = []any{11, 99}
	if promotionPayloadMatches(want, got) {
		t.Fatal("command for another episode matched")
	}
}

func TestPromotionCleansResidualVerifiedPartialBeforeSuccess(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	r = h.resume(r.ID, "approve")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("expected deletion checkpoint: %s %s", r.Status, r.Error)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulates a previously published independent backup plus the known
	// partial left behind by an interrupted older publication implementation.
	if err := os.WriteFile(p.BackupPath+".partial", backup, 0600); err != nil {
		t.Fatal(err)
	}
	r = h.finish(r)
	if r.Status != StatusCompleted {
		t.Fatalf("partial recovery: %s %s", r.Status, r.Error)
	}
	if _, err := os.Stat(p.BackupPath + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial retained despite success: %v", err)
	}
}

func TestPromotionDifferentCandidatesCannotClaimSameOriginal(t *testing.T) {
	h := newPromotionHarness(t)
	first := h.run()
	source, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	source.ID = "another-source-transcode"
	if err := h.st.CreateActionInstance(*source); err != nil {
		t.Fatal(err)
	}
	second, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": source.ID, "series_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != StatusWaitingDecision {
		t.Fatalf("second preflight: %s %s", second.Status, second.Error)
	}
	// The first submit is unresolved and the old file remains active, so an
	// episode snapshot alone cannot prevent the second promotion's submit.
	h.dropImportResponse = true
	h.acceptDroppedImport = false
	first = h.resume(first.ID, "approve")
	if first.Status != StatusWaitingExternal || h.imports != 1 {
		t.Fatalf("first submit: %s %s", first.Status, first.Error)
	}
	second = h.resume(second.ID, "approve")
	if second.Status != StatusFailed || !strings.Contains(second.Error, "reserved") || h.imports != 1 {
		t.Fatalf("original reservation bypassed: %s %s imports=%d", second.Status, second.Error, h.imports)
	}
}
