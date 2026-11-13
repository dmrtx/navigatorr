package maintenanceui

import (
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// The build is looked up from the path before the body is read: an unknown one
// is 404 and an ended or expired one 409, whatever is sent and however large;
// only a live build judges the body.
func TestBuildHeartbeatLooksTheBuildUpBeforeTheBody(t *testing.T) {
	h, clock := buildsUIWithClock(t)
	const path = "/api/maintenance/builds"
	huge := `{"log":["` + strings.Repeat("x", 300<<10) + `"]}`
	nonsense := "this is not json"
	ended := newBuild(t, h, "done")
	withBuildsToken(h, "POST", path+"/"+ended.ID+"/finish", `{"status":"succeeded"}`)
	live := newBuild(t, h, "live")

	for _, body := range []string{huge, nonsense, `{"progress":"x"}`, `{"log":"x"}`} {
		if w := withBuildsToken(h, "POST", path+"/bld_missing/heartbeat", body); w.Code != 404 {
			t.Errorf("unknown build, %.20s: %d", body, w.Code)
		}
		if w := withBuildsToken(h, "POST", path+"/"+ended.ID+"/heartbeat", body); w.Code != 409 {
			t.Errorf("ended build, %.20s: %d", body, w.Code)
		}
	}
	for _, body := range []string{huge, nonsense, `{"log":"x"}`} {
		if w := withBuildsToken(h, "POST", path+"/"+live.ID+"/heartbeat", body); w.Code != 400 {
			t.Errorf("live build, %.20s: %d", body, w.Code)
		}
	}

	// A running build past its expiry that the tick has not marked yet is over
	// on every path, valid body or not, and the heartbeat does not revive it.
	for _, body := range []string{`{"progress":"revived"}`, `{"progress":42}`, `{"log":"x"}`, huge, nonsense, ``} {
		b := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"laptop","source":"ci","title":"stale","progress":"before","ttl_seconds":60}`))
		clock.advance(61 * time.Second)
		w := withBuildsToken(h, "POST", path+"/"+b.ID+"/heartbeat", body)
		if w.Code != 409 {
			t.Errorf("expired build, %.20q: %d %s", body, w.Code, w.Body.String())
		}
		got := decodeBuild(t, withBuildsToken(h, "GET", path+"/"+b.ID, ""))
		if got.Status != store.BuildLost || got.Progress != "before" {
			t.Errorf("expired build, %.20q: %+v", body, got)
		}
		if lines := decodeBuildLog(t, withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "").Body.String()).Lines; len(lines) != 0 {
			t.Errorf("expired build kept lines: %v", lines)
		}
	}
}
