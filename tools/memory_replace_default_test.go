package tools

import (
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

// An agent cannot switch automatic replacement on for every new job: the
// default is not a preference memory_set can write.
func TestMemorySetCannotChangeTheReplaceWhenDoneDefault(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := server.NewMCPServer("test", "1")
	registerMemoryTools(s, &Deps{Store: st})
	for _, scope := range []string{"maintenance", "global"} {
		result := callTool(t, s, "memory_set", map[string]any{"scope": scope, "key": store.ReplaceWhenDoneDefaultKey, "value": "true"})
		if !result.IsError {
			t.Fatalf("memory_set wrote the default in %s", scope)
		}
	}
	// Even a preference written some other way is not the default.
	if _, err := st.SetPreference("maintenance", store.ReplaceWhenDoneDefaultKey, "true", "agent", 0); err != nil {
		t.Fatal(err)
	}
	if on, err := st.ReplaceWhenDoneDefault(); err != nil || on {
		t.Fatal("a preference turned automatic replacement on", on, err)
	}
}
