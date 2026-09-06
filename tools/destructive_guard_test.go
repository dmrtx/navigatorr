package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/transmission"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// callTool drives a registered tool by name, the way the MCP server would.
func callTool(t *testing.T, s *server.MCPServer, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	tool := s.GetTool(name)
	if tool == nil {
		t.Fatalf("tool %q was not registered", name)
	}

	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}}
	res, err := tool.Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("%s returned a transport error: %v", name, err)
	}
	return res
}

// Deletes in both torrent clients travel as POSTs rather than as the DELETE
// verb, so the guard in call_api never sees them. The tools have to refuse on
// their own, and they have to refuse before a request goes out — a guard that
// only suppresses the response has already deleted the torrent.
func TestTorrentDeletesRespectAllowDestructive(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		args   map[string]any
		reject string
	}{
		{"qbit delete", "qbit_manage_torrent", map[string]any{"action": "delete", "hashes": "abc"}, "Deleting is disabled"},
		{"qbit delete_files", "qbit_manage_torrent", map[string]any{"action": "delete_files", "hashes": "abc"}, "Deleting is disabled"},
		{"transmission remove", "transmission_manage_torrent", map[string]any{"action": "remove", "ids": "1"}, "Removing is disabled"},
		{"transmission remove_data", "transmission_manage_torrent", map[string]any{"action": "remove_data", "ids": "1"}, "Removing is disabled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)

			s := server.NewMCPServer("test", "0.0.0")
			registerQbitTools(s, qbit.NewClient(srv.URL, "u", "p"), false)
			registerTransmissionTools(s, transmission.NewClient(srv.URL, "u", "p"), false)

			res := callTool(t, s, tt.tool, tt.args)

			if !strings.Contains(resultText(t, res), tt.reject) {
				t.Errorf("expected a refusal mentioning %q, got: %s", tt.reject, resultText(t, res))
			}
			if hits != 0 {
				t.Errorf("guard let %d request(s) reach the server; it must refuse before any request goes out", hits)
			}
		})
	}
}

// The guard must not touch the non-destructive actions.
func TestNonDestructiveActionsAreNotGated(t *testing.T) {
	for _, tt := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"qbit pause", "qbit_manage_torrent", map[string]any{"action": "pause", "hashes": "abc"}},
		{"transmission stop", "transmission_manage_torrent", map[string]any{"action": "stop", "ids": "1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := server.NewMCPServer("test", "0.0.0")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)

			registerQbitTools(s, qbit.NewClient(srv.URL, "u", "p"), false)
			registerTransmissionTools(s, transmission.NewClient(srv.URL, "u", "p"), false)

			text := resultText(t, callTool(t, s, tt.tool, tt.args))
			for _, refusal := range []string{"Deleting is disabled", "Removing is disabled"} {
				if strings.Contains(text, refusal) {
					t.Errorf("%s was gated but should not be: %s", tt.args["action"], text)
				}
			}
		})
	}
}

// With the setting on, the destructive actions reach the client.
func TestDeletesGoThroughWhenAllowed(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	s := server.NewMCPServer("test", "0.0.0")
	registerQbitTools(s, qbit.NewClient(srv.URL, "u", "p"), true)

	text := resultText(t, callTool(t, s, "qbit_manage_torrent", map[string]any{"action": "delete", "hashes": "abc"}))
	if strings.Contains(text, "Deleting is disabled") {
		t.Fatalf("delete was refused with allow_destructive on: %s", text)
	}
	if hits == 0 {
		t.Error("delete was allowed but no request reached the server")
	}
}

func TestAPICallDestructiveSafetyClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "test-key"},
		},
	}
	reg := arrservice.NewRegistry(cfg)

	t.Run("destructive operations blocked when allow_destructive=false", func(t *testing.T) {
		s := server.NewMCPServer("test", "0.0.0")
		registerAPICallTool(s, reg, nil, 100, false)

		cases := []struct {
			name   string
			method string
			path   string
			body   string
		}{
			{"HTTP DELETE verb", "DELETE", "/movie/1", ""},
			{"POST /command CleanUpRecycleBin", "POST", "/command", `{"name":"CleanUpRecycleBin"}`},
			{"POST /command DeleteLogFiles", "POST", "/command", `{"name":"DeleteLogFiles"}`},
			{"POST /command PurgeQueue", "POST", "/command", `{"name":"PurgeQueue"}`},
			{"POST to /delete endpoint", "POST", "/movie/delete", `{"ids":[1,2]}`},
			{"POST to /purge endpoint", "POST", "/cache/purge", `{}`},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				args := map[string]any{
					"service": "radarr",
					"method":  tc.method,
					"path":    tc.path,
				}
				if tc.body != "" {
					args["body"] = tc.body
				}
				res := callTool(t, s, "call_api", args)
				txt := resultText(t, res)
				if !res.IsError && !strings.Contains(txt, "destructive operations are disabled") {
					t.Errorf("expected destructive operation to be refused, got: %s", txt)
				}
				if !strings.Contains(txt, "allow_destructive: true") {
					t.Errorf("expected refusal to mention allow_destructive, got: %s", txt)
				}
			})
		}
	})

	t.Run("non-destructive operations allowed when allow_destructive=false", func(t *testing.T) {
		s := server.NewMCPServer("test", "0.0.0")
		registerAPICallTool(s, reg, nil, 100, false)

		// Regular GET
		resGet := callTool(t, s, "call_api", map[string]any{"service": "radarr", "method": "GET", "path": "/movie"})
		if resGet.IsError {
			t.Errorf("expected GET to succeed, got error: %s", resultText(t, resGet))
		}

		// Non-destructive POST command
		resPost := callTool(t, s, "call_api", map[string]any{
			"service": "radarr",
			"method":  "POST",
			"path":    "/command",
			"body":    `{"name":"RescanMovie","movieId":1}`,
		})
		if resPost.IsError {
			t.Errorf("expected RescanMovie POST to succeed, got error: %s", resultText(t, resPost))
		}
	})

	t.Run("all operations allowed when allow_destructive=true", func(t *testing.T) {
		s := server.NewMCPServer("test", "0.0.0")
		registerAPICallTool(s, reg, nil, 100, true)

		resDel := callTool(t, s, "call_api", map[string]any{"service": "radarr", "method": "DELETE", "path": "/movie/1"})
		if resDel.IsError && strings.Contains(resultText(t, resDel), "destructive operations are disabled") {
			t.Errorf("expected DELETE to be permitted when allow_destructive=true")
		}

		resCmd := callTool(t, s, "call_api", map[string]any{
			"service": "radarr",
			"method":  "POST",
			"path":    "/command",
			"body":    `{"name":"CleanUpRecycleBin"}`,
		})
		if resCmd.IsError && strings.Contains(resultText(t, resCmd), "destructive operations are disabled") {
			t.Errorf("expected CleanUpRecycleBin to be permitted when allow_destructive=true")
		}
	})
}
