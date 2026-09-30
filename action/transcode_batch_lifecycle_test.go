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
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/transcode"
)

// The coordinator uses an isolated Sonarr and deterministic worker evidence.
// Real encoders/metrics are covered separately by the worker's native tests.
func TestBatchLifecycleTwoCandidatesOneApprovalAndRestart(t *testing.T) {
	for _, dropRescanResponse := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost_rescan_response=%t", dropRescanResponse), func(t *testing.T) {
			testBatchLifecycleTwoCandidatesOneApprovalAndRestart(t, dropRescanResponse)
		})
	}
}

func testBatchLifecycleTwoCandidatesOneApprovalAndRestart(t *testing.T, dropRescanResponse bool) {
	h := newPromotionHarness(t)
	originals := []string{h.original, filepath.Join(filepath.Dir(h.original), "Series S01E03.mp4")}
	if err := os.WriteFile(originals[1], []byte(strings.Repeat("second-original-", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	files := map[int]promotionFile{}
	episodes := []promotionEpisode{}
	for i, path := range originals {
		f := h.files[101]
		f.ID, f.Path = 101+i, path
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		f.Size = info.Size()
		files[f.ID] = f
		episodes = append(episodes, promotionEpisode{ID: 11 + i, SeriesID: 1, EpisodeFileID: f.ID})
	}
	probe := strings.ReplaceAll(fakeMultiProbeJSON, `"bits_per_raw_sample": "10"`, `"bits_per_raw_sample": "8"`)
	probe = strings.ReplaceAll(probe, `"pix_fmt": "yuv420p"`, `"pix_fmt": "yuv420p", "r_frame_rate": "24/1", "avg_frame_rate": "24/1"`)
	// Renamed final files must continue to probe as the encoded HEVC output.
	probe = strings.Replace(probe, "*candidate*)", "*candidate*|*final*)", 1)
	if err := os.WriteFile(h.engine.deps.Ffprobe, []byte(probe), 0700); err != nil {
		t.Fatal(err)
	}
	commands := map[int]promotionCommandResponse{}
	imports, deletes, rescans := 0, 0, 0
	var sonarrMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sonarrMu.Lock()
		defer sonarrMu.Unlock()
		write := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/series/1":
			write(map[string]any{"id": 1, "path": filepath.Join(h.root, "Series")})
		case r.Method == "GET" && r.URL.Path == "/api/v3/episode":
			all := []map[string]any{}
			for _, ep := range episodes {
				all = append(all, map[string]any{"id": ep.ID, "seriesId": ep.SeriesID, "episodeFileId": ep.EpisodeFileID, "hasFile": true, "seasonNumber": 1, "episodeNumber": ep.ID - 10})
			}
			write(all)
		case r.Method == "GET" && r.URL.Path == "/api/v3/episodefile":
			all := []promotionFile{}
			for _, f := range files {
				all = append(all, f)
			}
			write(all)
		case strings.HasPrefix(r.URL.Path, "/api/v3/episodefile/"):
			id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/episodefile/"))
			f, ok := files[id]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if r.Method == "DELETE" {
				for _, ep := range episodes {
					if ep.EpisodeFileID == id {
						t.Error("delete before adoption")
					}
				}
				if err := os.Remove(f.Path); err != nil {
					t.Error(err)
				}
				delete(files, id)
				deletes++
				write(map[string]any{})
			} else {
				write(f)
			}
		case r.Method == "GET" && r.URL.Path == "/api/v3/command":
			all := []promotionCommandResponse{}
			for _, command := range commands {
				all = append(all, command)
			}
			write(all)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/command/"):
			id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/command/"))
			write(commands[id])
		case r.Method == "POST" && r.URL.Path == "/api/v3/command":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			name := getString(payload, "name")
			switch name {
			case "ManualImport":
				f := payload["files"].([]any)[0].(map[string]any)
				path := getString(f, "path")
				id := 201 + imports
				imports++
				info, err := os.Stat(path)
				if err != nil {
					t.Error(err)
					return
				}
				file := promotionFile{ID: id, SeriesID: 1, Path: path, Size: info.Size()}
				file.MediaInfo.VideoCodec = "HEVC"
				files[id] = file
				for _, raw := range f["episodeIds"].([]any) {
					for i := range episodes {
						if episodes[i].ID == int(raw.(float64)) {
							episodes[i].EpisodeFileID = id
						}
					}
				}
			case "RenameFiles":
				for _, raw := range payload["files"].([]any) {
					id := int(raw.(float64))
					f := files[id]
					final := filepath.Join(filepath.Dir(h.original), fmt.Sprintf("final-%d.mkv", id))
					if err := os.Rename(f.Path, final); err != nil {
						t.Error(err)
						return
					}
					f.Path = final
					files[id] = f
				}
			case "RescanSeries":
				rescans++
				// Real Sonarr can rediscover an original during a sibling's
				// pending import. A batch-wide scan is safe only after every
				// original is removed and both candidates have final paths.
				for _, path := range originals {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Errorf("series scanned while an original remains: %s (%v)", path, err)
					}
				}
				for _, file := range files {
					if !strings.HasPrefix(filepath.Base(file.Path), "final-") {
						t.Errorf("series scanned before candidate rename: %s", file.Path)
					}
				}
			}
			cmd := promotionCommandResponse{ID: len(commands) + 1, Name: name, Status: "completed", Body: payload}
			commands[cmd.ID] = cmd
			if name == "RescanSeries" && dropRescanResponse {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
				} else {
					_ = conn.Close()
				}
				return
			}
			write(cmd)
		default:
			t.Errorf("unexpected Sonarr call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg := h.engine.deps.Config
	svc := cfg.Services["sonarr"]
	svc.URL = server.URL
	cfg.Services["sonarr"] = svc
	requests := map[string]transcode.Request{}
	mock := &mockTranscodeExecutor{
		capabilitiesFunc: func(ctx context.Context) (transcode.WorkerCapabilities, error) {
			caps, _ := (&mockTranscodeExecutor{}).Capabilities(ctx)
			caps.Encoders["libx265"] = true
			caps.Quality = &transcode.QualityCapabilities{Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}, CAMBIFullRef: true}
			caps.CapabilityFingerprint, _ = transcode.ComputeCapabilityFingerprint(caps)
			return caps, nil
		},
		benchmarkStatusFunc: func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{ID: id, Status: transcode.StatusCompleted, Decision: &transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{{Quality: 20, VideoCodec: "libx265", MetricType: "vmaf", Eligible: true, MinimumMet: true, TargetReached: true, Score: 96, EstimatedBytes: 100, SavingsPercent: 30}}}}, nil
		},
		submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
			requests[req.ID] = req
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
			r := requests[id]
			if r.Plan == nil || r.Plan.QualityValidation == nil {
				t.Fatal("missing final quality validation")
			}
			data, err := os.ReadFile(r.CandidatePath)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			hash := hex.EncodeToString(sum[:])
			size := int64(len(data))
			return transcode.JobStatus{ID: id, Status: transcode.StatusCompleted, CandidatePath: r.CandidatePath, CandidateSHA256: hash, CandidateSizeBytes: size, QualityEvidence: &transcode.FinalQualityEvidence{Verdict: "pass", CandidateSHA256: hash, CandidateSizeBytes: size, PlanDigest: r.Plan.PlanDigest, BenchmarkRequestDigest: r.Plan.QualityValidation.BenchmarkRequestDigest}}, nil
		},
	}
	deps := h.engine.deps
	deps.Registry = arrservice.NewRegistry(cfg)
	deps.Transcode = mock
	h.engine = NewEngine(deps)
	r, err := h.engine.Run(context.Background(), "transcode_batch", map[string]any{"service": "sonarr", "series_id": 1, "profile_config": calibratedTestProfileConfig(), "shared_calibration": true, "calibration_items": 1, "max_items": 2, "promote_candidates": true})
	if err != nil {
		t.Fatal(err)
	}
	r = h.finish(r)
	if r.Status != StatusWaitingDecision || imports != 0 || atomic.LoadInt32(&mock.submitCalls) != 2 || atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 1 {
		t.Fatalf("expected two validated candidates and one approval: %+v requests=%d", r, len(requests))
	}
	for _, path := range originals {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("source changed before approval")
		}
	}
	// Reconstruct the engine from persisted state both before and after approval.
	h.engine = NewEngine(deps)
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted || imports != 2 || deletes != 2 || rescans != 1 {
		t.Fatalf("batch did not complete: %+v imports=%d deletes=%d rescans=%d", r, imports, deletes, rescans)
	}
	items, err := h.st.ListTranscodeBatchItems(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		child, err := h.st.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:sonarr:"+item.ChildActionID)
		if err != nil || child == nil || child.Status != StatusCompleted {
			t.Fatalf("promotion missing: %+v %v", child, err)
		}
		p, err := loadPromotion(parseExecutionContext(child, h.engine))
		if err != nil {
			t.Fatal(err)
		}
		if !p.RecoveryVerifiedBeforeReplacement || !p.RecoveryCleanupCompleted {
			t.Fatal("missing recovery lifecycle evidence")
		}
		if _, err := os.Stat(p.BackupPath); !os.IsNotExist(err) {
			t.Fatalf("backup not cleaned: %v", err)
		}
		if err := h.engine.verifyPromotionHash(context.Background(), p.NewPath, p.CandidateSHA); err != nil {
			t.Fatal(err)
		}
	}
	h.engine = NewEngine(deps)
	r = h.resume(r.ID, "")
	if r.Status != StatusCompleted || imports != 2 || deletes != 2 || rescans != 1 || atomic.LoadInt32(&mock.benchmarkSubmitCalls) != 1 {
		t.Fatal("restart repeated work")
	}
}
