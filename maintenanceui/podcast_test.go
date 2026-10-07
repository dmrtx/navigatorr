package maintenanceui

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func TestPodcastAdmissionPreservesBrowserAndHookReceipts(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Web: config.WebConfig{Enabled: true, Token: testToken}, Podcasts: config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"genwhy": {Enabled: true}}}}
	// Background admission needs no worker connection: the existing reconciler
	// will perform preflight after the durable HTTP receipt has been returned.
	e := action.NewEngine(action.EngineDeps{Store: st, Config: cfg})
	s, err := New(cfg, arrservice.NewRegistry(cfg), e, server.NewMCPServer("test", "0.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	if w := request(h, http.MethodGet, "/api/maintenance/podcasts", "", false); w.Code != http.StatusUnauthorized {
		t.Fatal("podcast configuration bypassed authentication", w.Code)
	}
	encode := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	inputs := map[string]any{"podcast_id": "genwhy", "path": "/media/episode.mp3", "output_path": "/media/clean.mp3"}
	body := func(key, explicit string) string {
		a := map[string]any{"action": "clean_podcast_ads", "inputs": encode(inputs)}
		if explicit != "" {
			a["idempotency_key"] = explicit
		}
		return encode(map[string]any{"name": "action_run", "background": true, "key": key, "arguments": a})
	}
	admit := func(body string) string {
		t.Helper()
		w := request(h, http.MethodPost, "/api/maintenance/tool", body, true)
		if w.Code != http.StatusAccepted {
			t.Fatalf("admission HTTP %d: %s", w.Code, w.Body.String())
		}
		var receipt struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.ID == "" {
			t.Fatalf("invalid admission receipt: %s", w.Body.String())
		}
		return receipt.ID
	}
	browserRequest := body("browser-lost-ack", "")
	id := admit(browserRequest)
	// Simulate a client that loses the first response after durable admission
	// and retries with commandRequest's saved top-level submission key.
	if again := admit(browserRequest); again != id {
		t.Fatal("lost acknowledgement duplicated the podcast action", id, again)
	}
	inputs["output_path"] = "/media/different.mp3"
	if w := request(h, http.MethodPost, "/api/maintenance/tool", body("browser-lost-ack", ""), true); w.Code != http.StatusConflict {
		t.Fatal("same receipt accepted changed immutable inputs", w.Code, w.Body.String())
	}
	inputs["output_path"] = "/media/clean.mp3"
	hookID := admit(body("browser-key-ignored", "moonstation-download"))
	if hookID == id || admit(body("another-browser-key", "moonstation-download")) != hookID {
		t.Fatal("explicit hook idempotency key lost precedence")
	}
	instances, err := st.ListActionInstances("all", 10)
	if err != nil || len(instances) != 2 {
		t.Fatalf("unexpected admitted actions: %+v %v", instances, err)
	}
}
