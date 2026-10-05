package maintenanceui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/tools"
	"github.com/mark3labs/mcp-go/server"
)

func seedOperation(t *testing.T, st *store.Store, id, name, status string, inputs, outputs, state map[string]any) {
	t.Helper()
	encode := func(v map[string]any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: name, Status: status, InputsJSON: encode(inputs), OutputsJSON: encode(outputs), StateJSON: encode(state)}); err != nil {
		t.Fatal(err)
	}
}

func TestOperationsAccountingPaginationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	projection := func(candidate int64, cleaned bool, originalSHA string) map[string]any {
		return map[string]any{"original_path": "/media/a.mkv", "original_sha256": originalSHA, "original_bytes": int64(1000), "candidate_bytes": candidate, "transcode_action_id": "original-child", "recovery_cleanup_completed": cleaned, "recovery_verified_before_replacement": true}
	}
	seedOperation(t, st, "promoted", "promote_transcode_candidate", "completed", nil, map[string]any{"promoted": true, "recovery_retained": false, "promotion": projection(300, true, "before")}, nil)
	seedOperation(t, st, "duplicate", "promote_transcode_candidate", "completed", nil, map[string]any{"promoted": true, "recovery_retained": false, "promotion": projection(300, true, "before")}, nil)
	seedOperation(t, st, "retained", "promote_transcode_candidate", "failed", nil, map[string]any{"promoted": true, "recovery_retained": true, "promotion": projection(300, false, "other")}, nil)
	seedOperation(t, st, "unverified", "promote_transcode_candidate", "completed", nil, map[string]any{"promoted": true, "recovery_retained": false, "promotion": projection(300, false, "unverified")}, nil)
	seedOperation(t, st, "growth", "promote_transcode_candidate", "completed", nil, map[string]any{"promoted": true, "recovery_retained": false, "promotion": projection(1100, true, "after")}, nil)
	media := map[string]any{"original": map[string]any{"size_bytes": 1000}, "result": map[string]any{"size_bytes": 300}, "original_intact": true, "original_sha256": "before"}
	seedOperation(t, st, "original-child", "transcode_media", "completed", map[string]any{"path": "/media/a.mkv"}, media, nil)
	seedOperation(t, st, "candidate", "transcode_media", "completed", map[string]any{"path": "/media/b.mkv"}, media, nil)
	seedOperation(t, st, "candidate-duplicate", "transcode_media", "completed", map[string]any{"path": "/media/b.mkv"}, media, nil)
	seedOperation(t, st, "batch", "transcode_batch", "completed", nil, map[string]any{"size_saved_bytes": 999999}, nil)
	seedOperation(t, st, "active", "transcode_media", "waiting_external", map[string]any{"path": "/media/c.mkv"}, nil, map[string]any{"original": map[string]any{"size_bytes": 1000}, "benchmark_decision": map[string]any{"winner": map[string]any{"estimated_bytes": 400}}})
	seedOperation(t, st, "benchmark", "benchmark_transcode", "completed", map[string]any{"path": "/media/c.mkv"}, nil, map[string]any{"original": map[string]any{"size_bytes": 1000}, "benchmark_decision": map[string]any{"winner": map[string]any{"estimated_bytes": 400}}})
	seedOperation(t, st, "unrelated", "validate_torrent", "running", nil, nil, nil)
	check := func() {
		cfg := &config.Config{Web: config.WebConfig{Token: testToken}}
		reg := arrservice.NewRegistry(cfg)
		m := server.NewMCPServer("test", "1")
		e := tools.RegisterMaintenance(m, cfg, reg, nil, st)
		s, err := New(cfg, reg, e, m)
		if err != nil {
			t.Fatal(err)
		}
		h := s.Handler()
		w := request(h, "GET", "/api/maintenance/operations?status=completed&limit=2&offset=1", "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		if page.Total != 9 || len(page.Jobs) != 2 || !page.HasMore || page.ActiveCount != 1 {
			t.Fatalf("wrong page/counts: %+v", page)
		}
		if page.Savings.RealizedBytes != 600 || page.Savings.CompletedReplacements != 2 || page.Savings.CandidateBytes != 700 || page.Savings.EstimatedBytes != 600 || len(page.History) != 2 {
			t.Fatalf("double-counted or premature accounting: %+v", page)
		}
		if page.History[1].CumulativeBytes != 600 {
			t.Fatal(page.History)
		}
		for _, job := range page.Jobs {
			if job["worker"] == nil && job["id"] == "active" {
				t.Fatal("missing telemetry")
			}
		}
		w = request(h, "GET", "/api/maintenance/operations?status=active", "", true)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 1 || len(page.Jobs) != 1 || page.Jobs[0]["id"] != "active" {
			t.Fatal(w.Body.String())
		}
		for _, query := range []string{"?offset=-1", "?limit=101", "?status=typo"} {
			if w := request(h, "GET", "/api/maintenance/operations"+query, "", true); w.Code != 400 {
				t.Fatal(query, w.Code)
			}
		}
		if w := request(h, "GET", "/api/maintenance/operations", "", false); w.Code != 401 {
			t.Fatal("accounting not protected")
		}
		w = request(h, "GET", "/api/maintenance/operations?id=active", "", true)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 1 || len(page.Jobs) != 1 || page.Jobs[0]["id"] != "active" || page.Savings.RealizedBytes != 600 {
			t.Fatal("individual job lost global history", w.Body.String())
		}
		w = request(h, "GET", "/api/maintenance/operations?offset=9223372036854775807", "", true)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 0 || page.HasMore {
			t.Fatal("offset overflow", w.Body.String())
		}
	}
	check()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	check()
}

func TestBatchSavingsProjectChildrenWithoutDoubleCounting(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "waiting_external", nil, map[string]any{"counts": map[string]any{"total": 3, "transcode": 3}}, nil)
	for _, id := range []string{"child", "child-copy"} {
		seedOperation(t, st, id, "transcode_media", "completed", map[string]any{"path": "/media/a", "parent_action_id": "batch"}, map[string]any{"original": map[string]any{"size_bytes": 1000}, "result": map[string]any{"size_bytes": 300}, "original_sha256": "before", "original_intact": true}, nil)
	}
	seedOperation(t, st, "child-pending", "transcode_media", "waiting_external", map[string]any{"path": "/media/b", "parent_action_id": "batch"}, nil, map[string]any{"original": map[string]any{"size_bytes": 2000}, "benchmark_decision": map[string]any{"winner": map[string]any{"estimated_bytes": 1000}}})
	seedOperation(t, st, "promoted", "promote_transcode_candidate", "completed", nil, map[string]any{"promoted": true, "recovery_retained": false, "promotion": map[string]any{"original_path": "/media/a", "original_sha256": "before", "original_bytes": 1000, "candidate_bytes": 300, "transcode_action_id": "child", "recovery_cleanup_completed": true, "recovery_verified_before_replacement": true}}, nil)
	w := request(h, "GET", "/api/maintenance/operations?id=batch", "", true)
	var page operationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	encoded, _ := json.Marshal(page.Jobs[0]["savings"])
	var savings operationSavings
	if json.Unmarshal(encoded, &savings) != nil || savings.SourceBytes == nil || *savings.SourceBytes != 3000 || savings.RealizedSavedBytes == nil || *savings.RealizedSavedBytes != 700 || savings.CandidateSavedBytes == nil || *savings.CandidateSavedBytes != 700 || savings.EstimatedSavedBytes == nil || *savings.EstimatedSavedBytes != 1000 || savings.MeasuredFiles != 1 || !savings.Partial {
		t.Fatal("wrong partial batch projection", string(encoded))
	}
	if page.Savings.RealizedBytes != 700 || page.Savings.CandidateBytes != 0 || page.Savings.EstimatedBytes != 1000 {
		t.Fatal("batch projection entered global totals twice", w.Body.String())
	}
}

func TestOperationSavingsEstimatesAndETA(t *testing.T) {
	now := time.Now().UTC()
	r := operationRecord{inst: store.ActionInstance{ActionName: "transcode_media", Status: "waiting_external"}, inputs: map[string]any{}, outputs: map[string]any{}, state: map[string]any{"original": map[string]any{"size_bytes": float64(1000), "duration_sec": float64(1200)}, "expected_savings_percent": float64(20), "transcode_phase": "encoding", "last_progress_at": now.Add(-5 * time.Second).Format(time.RFC3339Nano), "progress": float64(50), "speed": float64(2)}}
	s := projectOperationSavings(r, now)
	if s.EstimateKind != "profile_heuristic" || s.EstimatedSavedBytes == nil || *s.EstimatedSavedBytes != 200 || s.ETASeconds == nil || *s.ETASeconds != 300 {
		t.Fatalf("wrong estimate: %+v", s)
	}
	r.state["benchmark_decision"] = map[string]any{"winner": map[string]any{"estimated_bytes": float64(350)}}
	s = projectOperationSavings(r, now)
	if s.EstimateKind != "sampled_benchmark" || *s.EstimatedSavedBytes != 650 {
		t.Fatalf("heuristic replaced measured estimate: %+v", s)
	}
	for _, change := range []map[string]any{{"progress_is_stale": true}, {"last_progress_at": now.Add(-time.Minute).Format(time.RFC3339Nano)}, {"transcode_phase": "validating"}, {"speed": float64(0)}} {
		modified := r
		modified.state = map[string]any{}
		for k, v := range r.state {
			modified.state[k] = v
		}
		for k, v := range change {
			modified.state[k] = v
		}
		if got := projectOperationSavings(modified, now); got.ETASeconds != nil {
			t.Fatalf("ETA fabricated for %+v: %+v", change, got)
		}
	}
}

func TestOperationsHistoryIsBoundedButTotalsAreLifetime(t *testing.T) {
	s, h := testUI(t)
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("promotion-%02d", i)
		seedOperation(t, s.engine.Deps().Store, id, "promote_transcode_candidate", "completed", nil, map[string]any{"promoted": true, "recovery_retained": false, "promotion": map[string]any{"original_path": "/media/" + id, "original_sha256": "source", "original_bytes": 1000, "candidate_bytes": 500, "recovery_cleanup_completed": true, "recovery_verified_before_replacement": true}}, nil)
	}
	w := request(h, "GET", "/api/maintenance/operations?limit=1", "", true)
	var page operationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Savings.RealizedBytes != 20000 || page.Savings.CompletedReplacements != 40 || len(page.History) != 25 || page.History[len(page.History)-1].CumulativeBytes != 20000 {
		t.Fatal(w.Code, w.Body.String())
	}
}
