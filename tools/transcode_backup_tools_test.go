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
	for _, args := range []map[string]any{
		{"mode": "clean"},
		{"mode": "clean", "action_id": "anything"},
		{"mode": "delete_all"},
		{"mode": "list", "offset": -1},
	} {
		res := callTool(t, s, "transcode_backups", args)
		if res.IsError != true {
			t.Fatalf("expected tool error for %v", args)
		}
	}
}
