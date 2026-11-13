package maintenanceui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildHeartbeatLogEdgeCases(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/builds"
	b := decodeBuild(t, withBuildsToken(h, "POST", path, `{"target":"laptop","source":"ci","title":"unit run"}`))
	hb := path + "/" + b.ID + "/heartbeat"
	readLog := func() (lines []string, status string, ended any) {
		var out struct {
			Lines   []string `json:"lines"`
			Status  string   `json:"status"`
			EndedAt any      `json:"ended_at"`
		}
		if err := json.Unmarshal(withBuildsToken(h, "GET", path+"/"+b.ID+"/log", "").Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Lines, out.Status, out.EndedAt
	}
	// null elements are no strings; a null array is no log.
	for _, body := range []string{`{"log":[null]}`, `{"log":["a",null]}`} {
		if w := withBuildsToken(h, "POST", hb, body); w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := withBuildsToken(h, "POST", hb, `{"log":null}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if lines, _, _ := readLog(); len(lines) != 0 {
		t.Fatal(lines)
	}
	// The biggest legal heartbeat: 64 lines of 300 characters, every one escaped
	// (a 12 byte surrogate pair for the astral ones), well past 100 KB.
	const bs = "\\"
	var big []string
	for i := 0; i < 64; i++ {
		line := strings.Repeat(bs+"u00e9", 300)
		if i%2 == 0 {
			line = strings.Repeat(bs+"ud83d"+bs+"ude00", 150) + strings.Repeat(bs+"u00e9", 150)
		}
		big = append(big, `"`+line+`"`)
	}
	body := `{"log":[` + strings.Join(big, ",") + `]}`
	if len(body) < 100<<10 {
		t.Fatal("the body is meant to be big", len(body))
	}
	if w := withBuildsToken(h, "POST", hb, body); w.Code != 200 {
		t.Fatalf("a legal 64x300 heartbeat: %d %.200s", w.Code, w.Body.String())
	}
	if lines, _, ended := readLog(); len(lines) != 64 || ended != nil {
		t.Fatal(len(lines), ended)
	}

	// Ended: 409 whatever the log, nothing stored, and ended_at is served.
	withBuildsToken(h, "POST", path+"/"+b.ID+"/finish", `{"status":"succeeded"}`)
	for _, body := range []string{`{"log":["fine"]}`, `{"log":["a\nb"]}`, `{"log":[null]}`, `{"log":["` + strings.Repeat("x", 301) + `"]}`} {
		if w := withBuildsToken(h, "POST", hb, body); w.Code != 409 {
			t.Fatalf("%.40s on an ended build: %d %s", body, w.Code, w.Body.String())
		}
	}
	if lines, status, ended := readLog(); len(lines) != 64 || ended == nil || status != "succeeded" {
		t.Fatal("an ended build stored lines or hides ended_at", len(lines), ended, status)
	}
	// Unknown: 404 whatever the log.
	for _, body := range []string{`{"log":["fine"]}`, `{"log":["a\nb"]}`, `{"log":[null]}`} {
		if w := withBuildsToken(h, "POST", path+"/bld_missing/heartbeat", body); w.Code != 404 {
			t.Fatalf("unknown build: %d", w.Code)
		}
	}
}
