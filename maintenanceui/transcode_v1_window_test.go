package maintenanceui

import (
	"encoding/json"
	"testing"
)

func TestTranscodeV1WindowRoutes(t *testing.T) {
	_, h := transcodeUI(t)
	const p = "/transcode/v1/workers/video-worker/window"

	if c, e := get(t, h, p, keyHdr()); c != 404 || e["code"] != "no_window" {
		t.Fatal("no window yet", c, e)
	}
	if c, e := get(t, h, "/transcode/v1/workers/nodeZ/window", keyHdr()); c != 404 || e["code"] != "unknown_worker" {
		t.Fatal(c, e)
	}
	if c, k := code(t, write(h, "PUT", "/transcode/v1/workers/nodeZ/window", `{}`)); c != 404 || k != "unknown_worker" {
		t.Fatal(c, k)
	}
	if c, k := code(t, write(h, "DELETE", "/transcode/v1/workers/nodeZ/window", ``)); c != 404 || k != "unknown_worker" {
		t.Fatal(c, k)
	}
	for name, body := range map[string]string{
		"bad type":    `{"media_types":["music"]}`,
		"bad margin":  `{"margin_minutes":1441}`,
		"neg margin":  `{"margin_minutes":-1}`,
		"bad until":   `{"until":"tomorrow"}`,
		"neg rev":     `{"revision":-1}`,
		"unknown key": `{"since":"x"}`,
		"empty":       ``,
	} {
		if c, k := code(t, write(h, "PUT", p, body)); c != 400 || k != "invalid_request" {
			t.Fatal(name, c, k)
		}
	}
	for _, q := range []string{"?revision=x", "?revision=", "?revision"} {
		if c, k := code(t, write(h, "DELETE", p+q, ``)); c != 400 || k != "invalid_request" {
			t.Fatal(q, c, k)
		}
	}
	if c, k := code(t, write(h, "PUT", p, `{"media_types":["movie",""]}`)); c != 400 || k != "invalid_request" {
		t.Fatal("blank media type", c, k)
	}
	if c, k := code(t, write(h, "PUT", p, `{"media_types":["  "]}`)); c != 400 || k != "invalid_request" {
		t.Fatal("blank media type", c, k)
	}

	w := write(h, "PUT", p, `{"until":"2099-01-01T06:00:00+02:00","margin_minutes":30,"media_types":["Movie","tv"]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "open" || got["margin_minutes"] != float64(30) || got["revision"] != float64(1) ||
		got["closes_at"] != "2099-01-01T03:30:00Z" || got["until"] != "2099-01-01T04:00:00Z" {
		t.Fatal(got)
	}
	if mt, _ := json.Marshal(got["media_types"]); string(mt) != `["tv","movie"]` {
		t.Fatal(string(mt))
	}

	if c, e := get(t, h, "/transcode/v1/windows", keyHdr()); c != 200 {
		t.Fatal(c, e)
	} else if ws, _ := e["windows"].([]any); len(ws) != 1 {
		t.Fatal(e)
	}

	// stale revision: 409, and nothing changes
	if c, k := code(t, write(h, "PUT", p, `{"margin_minutes":5,"revision":7}`)); c != 409 || k != "stale_window_revision" {
		t.Fatal(c, k)
	}
	if c, e := get(t, h, p, keyHdr()); c != 200 || e["margin_minutes"] != float64(30) {
		t.Fatal(c, e)
	}
	// the right revision writes
	if w := write(h, "PUT", p, `{"until":"2000-01-01T00:00:00Z","revision":1}`); w.Code != 200 || !json.Valid(w.Body.Bytes()) {
		t.Fatal(w.Code, w.Body.String())
	} else {
		var e map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if e["state"] != "closed" || e["revision"] != float64(2) {
			t.Fatal(e)
		}
	}
	if c, k := code(t, write(h, "DELETE", p+"?revision=1", ``)); c != 409 || k != "stale_window_revision" {
		t.Fatal(c, k)
	}
	if w := write(h, "DELETE", p+"?revision=2", ``); w.Code != 200 || !json.Valid(w.Body.Bytes()) {
		t.Fatal(w.Code, w.Body.String())
	}
	// Revisions keep counting past the deletion: the missing window says where.
	if c, e := get(t, h, p, keyHdr()); c != 404 || e["code"] != "no_window" || e["revision"] != float64(3) {
		t.Fatal(c, e)
	}
	// A writer holding the old revision cannot create over the deletion; the
	// counter is the expected revision of "no window".
	if c, k := code(t, write(h, "PUT", p, `{"margin_minutes":1,"revision":2}`)); c != 409 || k != "stale_window_revision" {
		t.Fatal(c, k)
	}
	if w := write(h, "PUT", p, `{"margin_minutes":1,"revision":3}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	} else {
		var e map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if e["revision"] != float64(4) || e["state"] != "open" {
			t.Fatal(e)
		}
	}
	if w := write(h, "DELETE", p, ``); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := write(h, "DELETE", p, ``); w.Code != 200 {
		t.Fatal("deleting nothing is a no-op", w.Code)
	}
}
