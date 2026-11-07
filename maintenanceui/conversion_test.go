package maintenanceui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func conversionRecord(id, action, status, created string, inputs, state map[string]any, candidate *int64) operationRecord {
	if inputs == nil {
		inputs = map[string]any{}
	}
	if state == nil {
		state = map[string]any{}
	}
	return operationRecord{inst: store.ActionInstance{ID: id, ActionName: action, Status: status, CreatedAt: created, UpdatedAt: created}, inputs: inputs, outputs: map[string]any{}, state: state, savings: operationSavings{CandidateBytes: candidate}}
}

func conversionFactsOf(records ...operationRecord) conversionFacts {
	byID := map[string]operationRecord{}
	for _, r := range records {
		byID[r.inst.ID] = r
	}
	replacements := queueReplacementLinks(records)
	roots, numbers, _ := queueWorkflows(records, byID, replacements)
	replacedByAct := map[string]bool{}
	for _, r := range records {
		if r.inst.ActionName == "promote_transcode_candidate" && r.savings.RealizedSavedBytes != nil {
			replacedByAct[operationString(r.inputs["transcode_action_id"])] = true
		}
	}
	return newConversionFacts(records, byID, roots, replacements, map[string]bool{}, replacedByAct, map[string]string{}, map[string]bool{}, numbers)
}

// A file's transcode and its replacement are one conversion with one state,
// and a later conversion that got a result resolves an earlier failure.
func TestConversionIsOneJobWithOneState(t *testing.T) {
	size := int64(100)
	saved := int64(50)
	ready := map[string]any{"original_intact": true}
	failed := conversionRecord("t0", "transcode_media", store.ActionStatusFailed, "2026-11-01T10:00:00Z", map[string]any{"path": "/media/a.mkv"}, nil, nil)
	done := conversionRecord("t1", "transcode_media", store.ActionStatusCompleted, "2026-11-01T11:00:00Z", map[string]any{"path": "/media/a.mkv"}, ready, &size)
	promote := conversionRecord("p1", "promote_transcode_candidate", store.ActionStatusCompleted, "2026-11-01T12:00:00Z", map[string]any{"transcode_action_id": "t1"}, nil, nil)
	promote.savings.RealizedSavedBytes = &saved
	waiting := conversionRecord("t2", "transcode_media", store.ActionStatusCompleted, "2026-11-01T13:00:00Z", map[string]any{"path": "/media/b.mkv"}, ready, &size)
	running := conversionRecord("t3", "transcode_media", store.ActionStatusRunning, "2026-11-01T14:00:00Z", map[string]any{"path": "/media/c.mkv"}, nil, nil)
	f := conversionFactsOf(failed, done, promote, waiting, running)

	if got := f.conversionID(promote); got != "t1" {
		t.Fatalf("replacement opens its conversion, got %q", got)
	}
	for r, want := range map[*operationRecord]string{&done: "replaced", &waiting: "ready_to_replace", &running: "encoding", &failed: "failed"} {
		if got := f.conversionStatus(*r); got != want {
			t.Fatalf("%s: status %q want %q", r.inst.ID, got, want)
		}
	}
	if run := f.conversionReplacement(done); run == nil || run["id"] != "p1" || run["status"] != store.ActionStatusCompleted {
		t.Fatalf("replacement run %v", run)
	}
	if by := f.resolvedBy(failed); by == nil || by["id"] != "t1" || by["conversion_status"] != "replaced" {
		t.Fatalf("resolved by %v", by)
	}
	if by := f.resolvedBy(done); by != nil {
		t.Fatalf("a finished job is not resolved by anything: %v", by)
	}
}

// The outcome is the setting that passed, which of the frozen settings it
// was, and the quality against its limits: data, not a sentence.
func TestConversionOutcomeIsTheResultNotThePlan(t *testing.T) {
	size, source := int64(68), int64(111)
	r := conversionRecord("t", "transcode_media", store.ActionStatusCompleted, "", nil, map[string]any{
		"size_main10": true,
		"quality_ladder": map[string]any{"video_codec": "libx265", "preset": "medium", "quality_values": []any{22, 19, 16}, "rung": 0, "outcome": "passed",
			"final": map[string]any{"verdict": "pass", "measured": map[string]any{"vmaf_mean": 93.4, "vmaf_worst_window": 91.9}, "limits": map[string]any{"vmaf_mean_minimum": 93.0, "vmaf_worst_window_minimum": 85.0}}},
	}, &size)
	r.savings.SourceBytes = &source
	got := conversionOutcome(r)
	if got["encoder"] != "x265 10-bit" || got["setting"] != "CRF 22 · medium" || got["attempt"] != 1 || got["attempts"] != 3 {
		t.Fatalf("outcome %v", got)
	}
	if got["vmaf_mean"] != 93.4 || got["vmaf_worst"] != 91.9 || got["vmaf_target"] != 93.0 || got["vmaf_floor"] != 85.0 {
		t.Fatalf("quality %v", got)
	}
	if conversionOutcome(conversionRecord("x", "transcode_media", store.ActionStatusRunning, "", nil, nil, nil)) != nil {
		t.Fatal("a running job has no outcome")
	}
}

// A later run of the same file that made no new file resolves nothing, even
// after the file was replaced by another run.
func TestNoResultRunDoesNotResolveAFailure(t *testing.T) {
	failed := conversionRecord("t0", "transcode_media", store.ActionStatusFailed, "2026-11-01T10:00:00Z", map[string]any{"path": "/media/a.mkv"}, nil, nil)
	nothing := conversionRecord("t1", "transcode_media", store.ActionStatusCompleted, "2026-11-01T11:00:00Z", map[string]any{"path": "/media/a.mkv"}, map[string]any{"skip_transcode": true}, nil)
	f := conversionFactsOf(failed, nothing)
	f.replaced[operationSourceKey(nothing)] = true
	if got := f.conversionStatus(nothing); got != "no_result" {
		t.Fatalf("status %q", got)
	}
	if by := f.resolvedBy(failed); by != nil {
		t.Fatalf("resolved by %v", by)
	}
}

// The list snapshot keeps only some inputs, so this reads through the endpoint.
func TestWaitingJobIsQueuedAndKeepsItsRunNext(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seed := func(id, condition string, inputs string) {
		t.Helper()
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_media", Status: store.ActionStatusWaitingExternal, WaitingCondition: condition, InputsJSON: inputs, OutputsJSON: "{}", StateJSON: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	priority := `{"path":"/media/a.mkv","queue_priority":true}`
	seed("first", "worker_busy", priority)
	seed("started", "transcode_complete", priority)
	seed("plain", "worker_busy", `{"path":"/media/b.mkv"}`)
	seed("held", "worker_drain", `{"path":"/media/c.mkv"}`)
	if err := st.CreateActionInstance(store.ActionInstance{ID: "first-batch", ActionName: "transcode_batch", Status: store.ActionStatusWaitingExternal, InputsJSON: `{"queue_priority":true}`, OutputsJSON: "{}", StateJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	seed("batch-child", "worker_busy", `{"path":"/media/d.mkv","parent_action_id":"first-batch"}`)
	if err := st.CreateActionInstance(store.ActionInstance{ID: "no-room", ActionName: "transcode_media", Status: store.ActionStatusWaitingExternal, WaitingCondition: "transcode_complete", WaitingReason: "Transcoding media (queued); waiting for room on the worker to keep the candidate (free_disk)", InputsJSON: priority, OutputsJSON: "{}", StateJSON: `{"transcode_phase":"queued"}`}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]struct {
		status   string
		priority any
	}{
		"first":       {"queued", true},
		"started":     {"encoding", nil},
		"plain":       {"queued", nil},
		"held":        {"queued", nil},
		"no-room":     {"queued", true},
		"batch-child": {"queued", true},
	} {
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body)
		}
		var body struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Jobs) != 1 {
			t.Fatalf("%s: %v %s", id, err, w.Body)
		}
		if job := body.Jobs[0]; job["conversion_status"] != want.status || job["queue_priority"] != want.priority {
			t.Fatalf("%s: status %v priority %v", id, job["conversion_status"], job["queue_priority"])
		}
	}
	// The page reads why it waits from these two.
	w := request(h, "GET", "/api/maintenance/operations?id=no-room", "", true)
	var held struct {
		Jobs []struct {
			WaitingReason string `json:"waiting_reason"`
			Worker        struct {
				Phase string `json:"transcode_phase"`
			} `json:"worker"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &held); err != nil || len(held.Jobs) != 1 || held.Jobs[0].Worker.Phase != "queued" || !strings.Contains(held.Jobs[0].WaitingReason, "(free_disk)") {
		t.Fatalf("%v %s", err, w.Body)
	}
	// The list reads the trimmed snapshot, not the full record.
	w = request(h, "GET", "/api/maintenance/operations", "", true)
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, job := range list.Jobs {
		if (job["id"] == "first" || job["id"] == "batch-child") && job["queue_priority"] == true {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("the list lost the Run next flag: %s", w.Body)
	}
}
