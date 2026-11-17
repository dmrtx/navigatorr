package maintenanceui

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

// A create that names an id is idempotent: the worker retries a registration
// whose answer it lost (2026-11-17, m1: one lease became ~30 running builds),
// and a retry must give back the same build, never a new one.
func TestBuildCreateWithAClientIDIsIdempotent(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	const body = `{"id":"bld_lease0001","target":"laptop","source":"mdm-build","title":"up android server"}`

	// Retries race the first create.
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := withBuildsToken(h, "POST", path, body)
			if w.Code != 200 {
				t.Errorf("create %d: %d %s", i, w.Code, w.Body.String())
				return
			}
			var b store.Build
			_ = json.Unmarshal(w.Body.Bytes(), &b)
			ids[i] = b.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != "bld_lease0001" {
			t.Fatalf("every answer is the named build: %v", ids)
		}
	}
	var list struct {
		Builds []store.Build `json:"builds"`
	}
	if err := json.Unmarshal(withBuildsToken(h, "GET", path, "").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Builds) != 1 {
		t.Fatalf("one build on record, got %d: %+v", len(list.Builds), list.Builds)
	}

	// After its end the id still answers that build, ended: nothing new runs.
	if w := withBuildsToken(h, "POST", path+"/bld_lease0001/finish", `{"status":"succeeded","exit_code":0}`); w.Code != 200 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	if b := decodeBuild(t, withBuildsToken(h, "POST", path, body)); b.ID != "bld_lease0001" || b.Status != store.BuildSucceeded {
		t.Fatalf("retry after the end: %+v", b)
	}

	// The id of another target or source is refused with a stable code.
	w := withBuildsToken(h, "POST", path, `{"id":"bld_lease0001","target":"laptop","source":"other","title":"x"}`)
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 409 || e.Code != "build_id_taken" {
		t.Fatalf("taken id: %d %s", w.Code, w.Body.String())
	}

	// A malformed id is the caller's to fix.
	for _, id := range []string{"lease0001", "bld_short", "bld_has space1"} {
		w := withBuildsToken(h, "POST", path, `{"id":"`+id+`","target":"laptop","source":"s","title":"x"}`)
		if w.Code != 400 {
			t.Fatalf("id %q: %d %s", id, w.Code, w.Body.String())
		}
	}

	// Without an id every create is a new build, as before.
	a := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"laptop","source":"s","title":"x"}`))
	b := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"laptop","source":"s","title":"x"}`))
	if a.ID == b.ID || a.ID == "" {
		t.Fatalf("ids without a client id: %q %q", a.ID, b.ID)
	}
}
