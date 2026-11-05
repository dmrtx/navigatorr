package maintenanceui

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

const workerScanDamage = `{"step":"wait_transcode","error":"ffmpeg_input_corrupt: the source file is damaged: reading its video the demuxer reported 3 errors, the first 41% into the file ([matroska,webm] EBML header parsing failed); download or remux the file again, a retry fails the same way"}`

// recordedFile is the state of a job that recorded the file it started with.
func recordedFile() map[string]any {
	return map[string]any{
		"job_id":        "job-1",
		"resolved_path": "/media/a.mkv",
		"original": map[string]any{
			"path": "/media/a.mkv", "size_bytes": 64.0, "video_seconds": 60.0,
			"video": []any{map[string]any{"index": 0.0, "kind": "video"}},
		},
	}
}

// A worker's scan report that Navigatorr has not read for itself leaves Retry
// open, because Retry reads the file here before it decides.
func TestWorkerScanReportLeavesRetryOpenUntilTheFileIsFoundDamaged(t *testing.T) {
	for name, tc := range map[string]struct {
		state  func() map[string]any
		inputs map[string]any
		open   bool
	}{
		"never read here": {func() map[string]any { return recordedFile() }, nil, true},
		"read, could not judge": {func() map[string]any {
			s := recordedFile()
			s["source_recheck"] = map[string]any{"job_id": "job-1", "verdict": "unchecked"}
			return s
		}, nil, true},
		"read here and damaged": {func() map[string]any {
			s := recordedFile()
			s["source_recheck"] = map[string]any{"job_id": "job-1", "verdict": "damaged"}
			return s
		}, nil, false},
		"nothing recorded to read": {func() map[string]any { return map[string]any{"job_id": "job-1"} }, nil, false},
		// A batch runs a file blamed this way again by skipping it, and its page
		// has no Retry of the file's own to read it with.
		"a file of a batch": {func() map[string]any { return recordedFile() }, map[string]any{"parent_action_id": "batch-x"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := failedRecord(workerScanDamage, tc.state())
			for k, v := range tc.inputs {
				r.inputs[k] = v
			}
			e, blocked, steps := explainFailure(r)
			// The cause is what the page's main button follows: a source to replace
			// first, or a worker's claim that Retry reads.
			wantCause := "file_damaged"
			if tc.open {
				wantCause = "worker_reported_damage"
			}
			if cause := failureCause(r); cause.Kind != wantCause {
				t.Errorf("cause = %+v, want %s", cause, wantCause)
			}
			if tc.open {
				if blocked != "" || steps != nil || !strings.Contains(e.Happened, "reported the file as damaged") || len(e.Learned) != 1 || !strings.Contains(e.Learned[0], "Retry reads the file here first") {
					t.Fatalf("blocked %q steps %q explanation %+v", blocked, stepIDs(steps), e)
				}
				return
			}
			if blocked == "" || !strings.Contains(e.Happened, "The file itself is damaged") {
				t.Fatalf("blocked %q explanation %+v", blocked, e)
			}
		})
	}
}

// A failure Navigatorr already overturned keeps the worker's words but is no
// claim about the file: it is explained as the worker's read fault, with the
// state of the job that cleared it.
func TestOverturnedScanReportIsNotOfferedAnotherRead(t *testing.T) {
	const overturned = `{"step":"wait_transcode","error":"Transcode failed (worker_source_read_fault): the worker reported the source file as damaged, but the coordinator read the same file end to end (2 video packets, 60 of 60 seconds) without a single demuxer error and the file did not change, so the file is intact and the worker's own read of it was at fault. Running it again is safe. The worker said: [worker read error] the source file is damaged: reading its video the demuxer reported 3 errors"}`
	s := recordedFile()
	s["source_recheck"] = map[string]any{"job_id": "job-1", "verdict": "intact"}
	e, blocked, steps := explainFailure(failedRecord(overturned, s))
	if blocked != "" || steps != nil || !strings.Contains(e.Happened, "read the same file end to end") || strings.Contains(e.Happened, "Navigatorr has not confirmed it") {
		t.Fatalf("blocked %q steps %q explanation %+v", blocked, stepIDs(steps), e)
	}
}

// The job's view offers no block for a worker's unconfirmed report, and keeps
// it for a file Navigatorr read as damaged.
func TestFailedJobViewOffersRetryForAnUnconfirmedScanReport(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	confirmed := recordedFile()
	confirmed["source_recheck"] = map[string]any{"job_id": "job-1", "verdict": "damaged"}
	for id, state := range map[string]map[string]any{"unconfirmed": recordedFile(), "confirmed": confirmed} {
		inputs := map[string]any{"path": "/media/a.mkv", "mode": "size"}
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_media", Status: "failed", InputsJSON: mustJSON(t, inputs), OutputsJSON: "{}", StateJSON: mustJSON(t, state), ErrorJSON: workerScanDamage}); err != nil {
			t.Fatal(err)
		}
	}
	unconfirmed := rerunJobView(t, h, "unconfirmed")
	if operationMap(unconfirmed["explanation"])["happened"] == nil || unconfirmed["retry_blocked"] != nil || unconfirmed["next_steps"] != nil {
		t.Fatalf("unconfirmed view %v", unconfirmed)
	}
	// The page's main button follows the cause: a file to replace first would
	// disable the very Retry that reads it.
	if got := operationMap(unconfirmed["cause"])["kind"]; got != "worker_reported_damage" {
		t.Fatalf("unconfirmed cause = %v", got)
	}
	confirmedView := rerunJobView(t, h, "confirmed")
	if confirmedView["retry_blocked"] == nil || confirmedView["next_steps"] == nil || operationMap(confirmedView["cause"])["kind"] != "file_damaged" {
		t.Fatalf("confirmed view %v", confirmedView)
	}
}
