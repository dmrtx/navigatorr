package maintenanceui

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/tools"
	"github.com/mark3labs/mcp-go/server"
)

// A job whose node froze its encode says so on the job itself, with what froze
// it, even when its worker is only a name (a standalone file): the page then
// shows it as paused, not as encoding. One that is not frozen carries neither.
func TestJobFrozenForABuildCarriesWhatFroze(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	by := map[string]any{"kind": "build", "id": "bld_1", "title": "verify", "source": "dev-laptop"}
	state := map[string]any{"worker": "m1", "original": map[string]any{"size_bytes": 1000}, "transcode_phase": "encoding"}
	seedOperation(t, st, "frozen", "transcode_media", "waiting_external", map[string]any{"path": "/media/a.mkv"}, map[string]any{"worker_suspended": true, "suspended_by": by}, state)
	seedOperation(t, st, "running", "transcode_media", "waiting_external", map[string]any{"path": "/media/b.mkv"}, map[string]any{"worker_suspended": false}, state)

	cfg := &config.Config{Web: config.WebConfig{Token: testToken}}
	reg := arrservice.NewRegistry(cfg)
	m := server.NewMCPServer("test", "1")
	e := tools.RegisterMaintenance(m, cfg, reg, nil, st)
	s, err := New(cfg, reg, e, m)
	if err != nil {
		t.Fatal(err)
	}
	w := request(s.Handler(), "GET", "/api/maintenance/operations?limit=25", "", true)
	var page struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	seen := map[string]map[string]any{}
	for _, j := range page.Jobs {
		if id, _ := j["id"].(string); id != "" {
			seen[id] = j
		}
	}
	frozen, running := seen["frozen"], seen["running"]
	if frozen == nil || running == nil {
		t.Fatalf("both jobs are listed: %s", w.Body.String())
	}
	cause, _ := frozen["suspended_by"].(map[string]any)
	if frozen["worker_suspended"] != true || cause["kind"] != "build" || cause["title"] != "verify" {
		t.Fatalf("the frozen job says what froze it: %v", frozen)
	}
	if _, ok := running["worker_suspended"]; ok {
		t.Fatalf("a job that is not frozen says nothing: %v", running)
	}
	if _, ok := running["suspended_by"]; ok {
		t.Fatalf("a job that is not frozen has no cause: %v", running)
	}
}
