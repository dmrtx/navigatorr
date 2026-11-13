package maintenanceui

import (
	"encoding/json"
	"strings"
	"testing"
)

type buildLogBody struct {
	Lines     []string `json:"lines"`
	UpdatedAt *string  `json:"updated_at"`
	Status    string   `json:"status"`
}

func decodeBuildLog(t *testing.T, body string) buildLogBody {
	t.Helper()
	var out buildLogBody
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err, body)
	}
	return out
}

func TestBuildLogEndpoint(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	b := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"laptop","source":"ci","title":"unit run"}`))

	// No output yet: an empty array (not null) and no update time.
	w := withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"lines":[]`) || !strings.Contains(w.Body.String(), `"updated_at":null`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := decodeBuildLog(t, w.Body.String()); got.Status != "running" {
		t.Fatal(got)
	}

	// The builds token appends lines; the web token reads them back.
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"progress":"one line","log":["first","second"]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"log":["third"]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for name, w := range map[string]string{
		"builds token": withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "").Body.String(),
		"web token":    request(h, "GET", path+"/"+b.ID+"/log", "", true).Body.String(),
	} {
		got := decodeBuildLog(t, w)
		if strings.Join(got.Lines, "|") != "first|second|third" || got.UpdatedAt == nil || got.Status != "running" {
			t.Fatalf("%s: %s", name, w)
		}
	}
	// The log is not part of the build record or its list.
	if body := withBuildsToken(h, "GET", path+"/"+b.ID, "").Body.String(); strings.Contains(body, "first") || strings.Contains(body, `"log"`) {
		t.Fatal("the build record carries the log:", body)
	}
	if body := withBuildsToken(h, "GET", path, "").Body.String(); strings.Contains(body, "first") {
		t.Fatal("the build list carries the log:", body)
	}

	// An ended build keeps its lines for a while, with its final status.
	withBuildsToken(h, "POST", path+"/"+b.ID+"/finish", `{"status":"failed","exit_code":1}`)
	if got := decodeBuildLog(t, withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "").Body.String()); len(got.Lines) != 3 || got.Status != "failed" {
		t.Fatal(got)
	}

	// Unknown build: 404, for both tokens. Without a token: 401.
	if w := withBuildsToken(h, "GET", path+"/bld_missing/log", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request(h, "GET", path+"/bld_missing/log", "", true); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request(h, "GET", path+"/"+b.ID+"/log", "", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	// Only a GET: the route takes no write.
	if w := request(h, "POST", path+"/"+b.ID+"/log", `{}`, true); w.Code == 200 {
		t.Fatal("POST on the log route")
	}
}

func TestBuildHeartbeatRejectsBadLog(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	b := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"laptop","source":"ci","title":"unit run"}`))
	many := `"` + strings.Repeat(`x","`, 64) + `x"`
	cases := map[string]string{
		"too many lines": `{"log":[` + many + `]}`,
		"too long":       `{"log":["` + strings.Repeat("x", 301) + `"]}`,
		"newline":        `{"log":["a\nb"]}`,
		"carriage":       `{"log":["a\rb"]}`,
		"nul":            `{"log":["a\u0000b"]}`,
		"not strings":    `{"log":[1,2]}`,
		"not an array":   `{"log":"line"}`,
	}
	for name, body := range cases {
		w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "error") {
			t.Errorf("%s: want 400 with an error, got %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"log":[]}`); w.Code != 200 {
		t.Fatal("an empty array is fine", w.Code, w.Body.String())
	}
	got := decodeBuildLog(t, withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "").Body.String())
	if len(got.Lines) != 0 {
		t.Fatal("a rejected heartbeat kept lines", got)
	}
	// Exactly 64 lines of 300 characters pass.
	ok := `{"log":["` + strings.Repeat(strings.Repeat("y", 300)+`","`, 63) + strings.Repeat("y", 300) + `"]}`
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", ok); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestBuildLogRouteIsOnlyForBuildIDs(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/maintenance/builds/abc/log":       true,
		"/api/maintenance/builds/abc/log/more":  false,
		"/api/maintenance/builds/abc/logs":      false,
		"/api/maintenance/builds/a b/log":       false,
		"/api/maintenance/builds/../worker/log": false,
	} {
		if got := isBuildsPath(p); got != want {
			t.Errorf("isBuildsPath(%q) = %v", p, got)
		}
	}
}
