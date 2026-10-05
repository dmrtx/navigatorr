package maintenanceui

import (
	"encoding/json"
	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComparisonProxyRequiresKnownActionAndKeepsWorkerTokenServerSide(t *testing.T) {
	s, h := testUI(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer server-only-secret" {
			t.Error("missing server worker authentication")
		}
		if r.URL.Path != "/v1/benchmarks/bench-known/comparison" {
			t.Error("unexpected worker path", r.URL.Path)
		}
		if r.URL.Query().Get("image") != "" {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\nfixture"))
			return
		}
		json.NewEncoder(w).Encode(transcode.BenchmarkComparison{ProtocolVersion: transcode.WorkerProtocolVersion, CandidateID: "winner", Frames: []transcode.BenchmarkComparisonFrame{{SampleIndex: 0, Width: 128, Height: 72}}})
	}))
	defer upstream.Close()
	deps := s.engine.Deps()
	executor, err := transcode.NewHTTPExecutor(transcode.HTTPConfig{BaseURL: upstream.URL, Token: "server-only-secret"})
	if err != nil {
		t.Fatal(err)
	}
	deps.Transcode = executor
	s.engine = action.NewEngine(deps)
	seedOperation(t, deps.Store, "known", "benchmark_transcode", "completed", nil, nil, map[string]any{"benchmark_job_id": "bench-known"})
	for _, tc := range []struct {
		path string
		auth bool
		want int
	}{{"?id=known", false, 401}, {"?id=unknown", true, 404}, {"?id=bench-known", true, 404}, {"?id=known&image=0&side=../original", true, 400}, {"?id=known", true, 200}, {"?id=known&image=0&side=original", true, 200}} {
		response := request(h, "GET", "/api/maintenance/benchmark-comparison"+tc.path, "", tc.auth)
		if response.Code != tc.want || strings.Contains(response.Body.String(), "server-only-secret") {
			t.Fatal(tc, response.Code, response.Body.String())
		}
	}
	if calls != 2 {
		t.Fatal("invalid requests reached worker", calls)
	}
}
