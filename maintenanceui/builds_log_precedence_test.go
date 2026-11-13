package maintenanceui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// buildsUIWithClock is the builds server over an engine whose clock the test
// moves, to see retention without waiting for it.
func buildsUIWithClock(t *testing.T) (http.Handler, *testClock) {
	t.Helper()
	base, _ := testUI(t)
	path := filepath.Join(t.TempDir(), "builds-token")
	if err := os.WriteFile(path, []byte(testBuildsToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	deps := base.engine.Deps()
	deps.Now = clock.now
	cfg := *base.cfg
	cfg.Web.BuildsTokenFile = path
	s, err := New(&cfg, base.registry, action.NewEngine(deps), base.mcp)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler(), clock
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newBuild(t *testing.T, h http.Handler, title string) store.Build {
	t.Helper()
	return decodeBuild(t, withBuildsToken(h, "POST", "/api/maintenance/builds", fmt.Sprintf(`{"target":"laptop","source":"ci","title":%q}`, title)))
}

// A log of the wrong type is judged after the build: unknown is 404, over is
// 409, only a live build says 400, and a refused heartbeat changes nothing.
func TestBuildHeartbeatWrongLogTypeFollowsTheBuildStatus(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	live := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"laptop","source":"ci","title":"unit run","progress":"step 1","ttl_seconds":120}`))
	over := newBuild(t, h, "done")
	withBuildsToken(h, "POST", path+"/"+over.ID+"/finish", `{"status":"succeeded"}`)
	longProgress := strings.Repeat("p", 201)
	bodies := []string{
		`{"log":[1]}`, `{"log":"x"}`, `{"log":{"a":1}}`, `{"log":[["a"]]}`, `{"log":true}`, `{"log":[null]}`,
		// progress: wrong type, too long, not a single line
		`{"progress":42}`, `{"progress":["a"]}`, `{"progress":{"a":1}}`, `{"progress":"` + longProgress + `"}`, `{"progress":"a\nb"}`,
		// ttl_seconds: wrong type, not whole, negative
		`{"ttl_seconds":-1}`, `{"ttl_seconds":"x"}`, `{"ttl_seconds":1.5}`, `{"ttl_seconds":true}`, `{"ttl_seconds":[1]}`,
		// a good field beside a bad one
		`{"progress":"fine","ttl_seconds":-5}`, `{"progress":42,"log":["fine"]}`,
	}
	for _, body := range bodies {
		if w := withBuildsToken(h, "POST", path+"/bld_missing/heartbeat", body); w.Code != 404 {
			t.Errorf("%s on an unknown build: %d", body, w.Code)
		}
		if w := withBuildsToken(h, "POST", path+"/"+over.ID+"/heartbeat", body); w.Code != 409 {
			t.Errorf("%s on an ended build: %d", body, w.Code)
		}
		// A new progress and a long ttl in the same body must not be applied.
		// (a field the body already has is not sent twice)
		with := body
		if !strings.Contains(body, `"progress"`) {
			with = strings.Replace(with, "{", `{"progress":"changed",`, 1)
		}
		if !strings.Contains(body, `"ttl_seconds"`) {
			with = strings.Replace(with, "{", `{"ttl_seconds":3600,`, 1)
		}
		if w := withBuildsToken(h, "POST", path+"/"+live.ID+"/heartbeat", with); w.Code != 400 {
			t.Errorf("%s on a live build: %d %s", with, w.Code, w.Body.String())
		}
	}
	got := decodeBuild(t, withBuildsToken(h, "GET", path+"/"+live.ID, ""))
	if lines := decodeBuildLog(t, withBuildsToken(h, "GET", path+"/"+live.ID+"/log", "").Body.String()).Lines; len(lines) != 0 {
		t.Fatal("a refused heartbeat kept lines", lines)
	}
	if got.Progress != "step 1" || got.ExpiresAt != live.ExpiresAt || got.LastHeartbeatAt != live.LastHeartbeatAt {
		t.Fatalf("a refused heartbeat changed the build: %+v then %+v", live, got)
	}
}

// The 409 of a heartbeat is the same record whichever way the log was found
// wrong, and the one the build endpoint serves.
func TestBuildHeartbeat409HasOneShape(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	b := newBuild(t, h, "done")
	withBuildsToken(h, "POST", path+"/"+b.ID+"/finish", `{"status":"failed","exit_code":2}`)
	body := func(req string) map[string]json.RawMessage {
		w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", req)
		if w.Code != 409 {
			t.Fatalf("%s: %d", req, w.Code)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	plain, nulled, typed := body(`{}`), body(`{"log":[null]}`), body(`{"log":"x"}`)
	for name, got := range map[string]map[string]json.RawMessage{"null": nulled, "type": typed} {
		if string(got["build"]) != string(plain["build"]) || string(got["error"]) != string(plain["error"]) {
			t.Fatalf("%s path answers %s, the plain path %s", name, got["build"], plain["build"])
		}
	}
	if !strings.Contains(string(plain["build"]), `"holds_transcode_node"`) {
		t.Fatalf("the marked record is expected: %s", plain["build"])
	}
	one := withBuildsToken(h, "GET", path+"/"+b.ID, "").Body.String()
	var rec map[string]json.RawMessage
	_ = json.Unmarshal([]byte(one), &rec)
	if string(rec["status"]) != `"failed"` || string(plain["build"]) != strings.TrimSpace(one) {
		t.Fatalf("409 build %s vs GET %s", plain["build"], one)
	}
}

// The real worst case: 64 lines of 300 astral characters, each escaped as a
// surrogate pair (12 bytes), plus a progress line, is accepted under the limit.
func TestBuildHeartbeatWorstCaseBodyIsAccepted(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	b := newBuild(t, h, "big")
	const bs = "\\"
	line := `"` + strings.Repeat(bs+"ud83d"+bs+"ude00", 300) + `"`
	if n := len(line); n < 3600 || n > 3700 {
		t.Fatalf("a line is %d bytes", n)
	}
	lines := make([]string, 64)
	for i := range lines {
		lines[i] = line
	}
	body := `{"progress":"` + strings.Repeat("p", 200) + `","ttl_seconds":600,"log":[` + strings.Join(lines, ",") + `]}`
	if len(body) < 200<<10 || len(body) >= 256<<10 {
		t.Fatalf("the body is %d bytes, meant to be near the limit", len(body))
	}
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", body); w.Code != 200 {
		t.Fatalf("%d %.200s", w.Code, w.Body.String())
	}
	got := decodeBuildLog(t, withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "").Body.String())
	if len(got.Lines) != 64 || strings.Count(got.Lines[0], "\U0001F600") != 300 {
		t.Fatal("the lines were not kept whole", len(got.Lines))
	}
}

// Past 64 buffers the ended build's output goes before any running build's.
func TestBuildLogEvictionOverHTTP(t *testing.T) {
	h, clock := buildsUIWithClock(t)
	const path = "/api/maintenance/builds"
	ids := make([]string, 0, 65)
	for i := 0; i < 64; i++ {
		clock.advance(time.Second)
		b := newBuild(t, h, fmt.Sprintf("run %d", i))
		ids = append(ids, b.ID)
		if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"log":["line"]}`); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// The newest buffer ends: it is the one to go although it is not the oldest.
	withBuildsToken(h, "POST", path+"/"+ids[63]+"/finish", `{"status":"succeeded"}`)
	clock.advance(time.Second)
	extra := newBuild(t, h, "one more")
	if w := withBuildsToken(h, "POST", path+"/"+extra.ID+"/heartbeat", `{"log":["new"]}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	lines := func(id string) int {
		w := withBuildsToken(h, "GET", path+"/"+id+"/log", "")
		if w.Code != 200 {
			t.Fatalf("%s: %d", id, w.Code)
		}
		return len(decodeBuildLog(t, w.Body.String()).Lines)
	}
	if lines(ids[63]) != 0 {
		t.Fatal("the ended build kept its output")
	}
	if !strings.Contains(withBuildsToken(h, "GET", path+"/"+ids[63]+"/log", "").Body.String(), `"expired":true`) {
		t.Fatal("an evicted buffer is expired")
	}
	if strings.Contains(withBuildsToken(h, "GET", path+"/"+ids[0]+"/log", "").Body.String(), `"expired":true`) {
		t.Fatal("a running build is not expired")
	}
	for _, id := range append(ids[:63:63], extra.ID) {
		if lines(id) != 1 {
			t.Fatalf("running build %s lost its output", id)
		}
	}
}

// Thirty minutes after the end the output is forgotten; the build is still
// there, with its end time, so a reader can tell the output is no longer kept.
func TestBuildLogExpiryOverHTTP(t *testing.T) {
	h, clock := buildsUIWithClock(t)
	const path = "/api/maintenance/builds"
	b := newBuild(t, h, "short")
	withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"log":["kept for now"]}`)
	withBuildsToken(h, "POST", path+"/"+b.ID+"/finish", `{"status":"succeeded"}`)
	clock.advance(action.BuildLogRetention - time.Second)
	if got := decodeBuildLog(t, withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "").Body.String()); len(got.Lines) != 1 {
		t.Fatal("the output goes only after the retention", got)
	}
	clock.advance(time.Second)
	w := withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "")
	got := decodeBuildLog(t, w.Body.String())
	if w.Code != 200 || len(got.Lines) != 0 || got.Status != "succeeded" {
		t.Fatalf("after the retention: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ended_at":"`) || !strings.Contains(w.Body.String(), `"expired":true`) {
		t.Fatalf("ended_at and expired tell the output is gone: %s", w.Body.String())
	}
	// An ended build that never sent anything is not expired: there was nothing to keep.
	quiet := newBuild(t, h, "quiet")
	withBuildsToken(h, "POST", path+"/"+quiet.ID+"/finish", `{"status":"succeeded"}`)
	if body := withBuildsToken(h, "GET", path+"/"+quiet.ID+"/log", "").Body.String(); !strings.Contains(body, `"expired":false`) {
		t.Fatalf("a build without output is not expired: %s", body)
	}
}

// The output answers on GET /log only: none of the other endpoints that show
// builds or nodes carry it.
func TestBuildOutputIsOnlyOnTheLogEndpoint(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	node := "m1"
	b := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"ci","title":"unit run"}`))
	if w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", `{"log":["token=synthetic-secret-77"]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !strings.Contains(withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "").Body.String(), "synthetic-secret-77") {
		t.Fatal("the log endpoint is meant to serve it")
	}
	reads := map[string]string{
		"build":    path + "/" + b.ID,
		"builds":   path,
		"workers":  "/api/maintenance/workers",
		"activity": "/api/maintenance/worker-activity",
		"pause":    "/api/maintenance/worker-pause",
		"slots":    "/api/maintenance/worker-slots",
	}
	for name, p := range reads {
		w := withBuildsToken(h, "GET", p, "")
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			w = request(h, "GET", p, "", true)
		}
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "synthetic-secret") {
			t.Fatalf("the output reached %s: %s", name, w.Body.String())
		}
	}
}
