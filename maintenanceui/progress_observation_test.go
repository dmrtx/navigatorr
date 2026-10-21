package maintenanceui

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A stale percentage and a fresh heartbeat are independent facts. The queue
// must receive both, including the unit being measured, without refreshing the
// underlying action just because the user opened or polled the page.
func TestQueuePreservesStaleEncodeObservationWithoutAdvancingAction(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	telemetry := map[string]any{
		"phase":    "encoding",
		"progress": 73.3, "progress_is_stale": true,
		"last_progress_at":    "2026-10-16T12:22:18.667341Z",
		"worker_heartbeat_at": "2026-10-16T12:22:54.153942Z",
	}
	seedOperation(t, st, "stale-encode", "transcode_media", "waiting_external", map[string]any{"path": "/media/episode.mkv"}, nil, telemetry)
	before, err := st.GetActionInstance("stale-encode")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		w := request(h, "GET", "/api/maintenance/operations?id=stale-encode", "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		worker := operationMap(page.Jobs[0]["worker"])
		for key, want := range telemetry {
			if !reflect.DeepEqual(worker[key], want) {
				t.Errorf("queue worker %s = %#v, want %#v", key, worker[key], want)
			}
		}
	}
	after, err := st.GetActionInstance("stale-encode")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read-only queue polling changed the action", err)
	}
}
