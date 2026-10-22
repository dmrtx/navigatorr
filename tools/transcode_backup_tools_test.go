package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func TestTranscodeBackupsToolModes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "backups.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine := action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}})
	s := server.NewMCPServer("test", "0.0.0")
	registerActionTools(s, engine)
	res := callTool(t, s, "transcode_backups", map[string]any{})
	if text := resultText(t, res); !strings.Contains(text, `"items": []`) || !strings.Contains(text, `"page_bytes": 0`) {
		t.Fatalf("default inventory: %s", text)
	}
	res = callTool(t, s, "transcode_backups", map[string]any{"mode": "candidates"})
	if text := resultText(t, res); res.IsError || !strings.Contains(text, `"items": []`) || !strings.Contains(text, `"page_bytes": 0`) {
		t.Fatalf("candidate inventory: %s", text)
	}
	for _, args := range []map[string]any{
		{"mode": "clean"},
		{"mode": "clean", "action_id": "anything"},
		{"mode": "delete_all"},
		{"mode": "list", "offset": -1},
		{"mode": "candidates", "offset": -1},
		{"mode": "discard_candidate"},
		{"mode": "discard_candidate", "action_id": "anything"},
		{"mode": "discard_candidate", "action_id": "anything", "confirm": "false"},
		{"mode": "discard_candidate", "action_id": "anything", "confirm": true},
	} {
		res := callTool(t, s, "transcode_backups", args)
		if res.IsError != true {
			t.Fatalf("expected tool error for %v", args)
		}
	}
	res = callTool(t, s, "transcode_backups", map[string]any{"mode": "discard_candidate", "action_id": "anything"})
	if text := resultText(t, res); !strings.Contains(text, "confirm=true") {
		t.Fatalf("missing confirmation not explained: %s", text)
	}
	res = callTool(t, s, "transcode_backups", map[string]any{"mode": "discard_candidate", "action_id": "anything", "confirm": "true"})
	if text := resultText(t, res); !strings.Contains(text, "allow_destructive") {
		t.Fatalf("disabled destructive changes not explained: %s", text)
	}
	description := s.GetTool("transcode_backups").Tool.Description
	if strings.Contains(description, "fs_safe_delete") || !strings.Contains(description, "mode=discard_candidate") || !strings.Contains(description, "worker_check") {
		t.Fatalf("candidate listing does not name its removal mode: %s", description)
	}
}
