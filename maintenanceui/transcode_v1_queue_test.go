package maintenanceui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

// seedWaiter adds a conversion waiting for a worker, in the place key says.
func seedWaiter(t *testing.T, st *store.Store, id, key, path string, state map[string]any) {
	t.Helper()
	s := map[string]any{"transcode_queue_order": key}
	for k, v := range state {
		s[k] = v
	}
	seedOperation(t, st, id, "transcode_media", "waiting_external", map[string]any{"path": path, "media_type": "episode"}, nil, s)
}

func postQ(t *testing.T, h http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	hd := keyHdr()
	hd["Content-Type"] = "application/json"
	w := v1(h, "POST", path, body, hd)
	var out map[string]any
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("POST %s: not a JSON object: %q", path, w.Body.String())
	}
	return w.Code, out
}

func queueIDsOf(t *testing.T, h http.Handler) ([]string, map[string]any) {
	t.Helper()
	code, q := get(t, h, "/transcode/v1/queue", keyHdr())
	if code != 200 {
		t.Fatal(code, q)
	}
	var ids []string
	for i, e := range q["entries"].([]any) {
		en := e.(map[string]any)
		if int(en["position"].(float64)) != i+1 {
			t.Fatalf("entry %d has position %v", i, en["position"])
		}
		ids = append(ids, en["ref"].(map[string]any)["id"].(string))
	}
	return ids, q
}

func TestTranscodeV1QueueReadsInOrderAndHoldsMovesAndReleases(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	seedWaiter(t, st, "c-a", "1|2026-01-01T00:00:01Z|g|00000000000000000001", "/media/A.mkv", nil)
	seedWaiter(t, st, "c-b", "1|2026-01-01T00:00:02Z|g|00000000000000000002", "/media/B.mkv", nil)
	seedWaiter(t, st, "c-c", "1|2026-01-01T00:00:03Z|g|00000000000000000003", "/media/C.mkv", nil)

	ids, q := queueIDsOf(t, h)
	if strings.Join(ids, ",") != "c-a,c-b,c-c" {
		t.Fatal(ids)
	}
	first := q["entries"].([]any)[0].(map[string]any)
	if first["path"] != "/media/A.mkv" || first["media_type"] != "episode" || first["held"] != false || first["ref"].(map[string]any)["kind"] != "action" {
		t.Fatalf("entry: %v", first)
	}
	rev0 := q["queue_revision"].(float64)

	// Hold: it keeps its place, is flagged, and says why it waits.
	code, r := postQ(t, h, "/transcode/v1/queue/actions/c-b/hold", ``)
	if code != 200 {
		t.Fatal(code, r)
	}
	en := r["entry"].(map[string]any)
	if en["held"] != true || en["held_by"] != "conversion" || en["waiting_condition"] != "queue_held" || r["queue_revision"].(float64) <= rev0 {
		t.Fatalf("hold: %v", r)
	}
	ids, _ = queueIDsOf(t, h)
	if strings.Join(ids, ",") != "c-a,c-b,c-c" {
		t.Fatal("a hold keeps the place", ids)
	}

	// Move first, then after, then before.
	for body, want := range map[string]string{
		`{"first":true}`: "c-c,c-a,c-b",
	} {
		if code, r := postQ(t, h, "/transcode/v1/queue/actions/c-c/move", body); code != 200 || r["entry"].(map[string]any)["position"].(float64) != 1 {
			t.Fatal(code, r)
		}
		if ids, _ = queueIDsOf(t, h); strings.Join(ids, ",") != want {
			t.Fatalf("%s: %v", body, ids)
		}
	}
	if code, r := postQ(t, h, "/transcode/v1/queue/actions/c-c/move", `{"after":{"kind":"action","id":"c-b"}}`); code != 200 {
		t.Fatal(code, r)
	}
	if ids, _ = queueIDsOf(t, h); strings.Join(ids, ",") != "c-a,c-b,c-c" {
		t.Fatal(ids)
	}
	if code, r := postQ(t, h, "/transcode/v1/queue/actions/c-c/move", `{"before":{"kind":"action","id":"c-b"}}`); code != 200 {
		t.Fatal(code, r)
	}
	if ids, _ = queueIDsOf(t, h); strings.Join(ids, ",") != "c-a,c-c,c-b" {
		t.Fatal(ids)
	}

	// Release.
	code, r = postQ(t, h, "/transcode/v1/queue/actions/c-b/release", `{}`)
	if code != 200 || r["entry"].(map[string]any)["held"] != false {
		t.Fatal(code, r)
	}
}

func TestTranscodeV1QueueRefusals(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	seedWaiter(t, st, "w-1", "1|2026-01-01T00:00:01Z|g|00000000000000000001", "/media/A.mkv", nil)
	seedWaiter(t, st, "w-2", "1|2026-01-01T00:00:02Z|g|00000000000000000002", "/media/B.mkv", nil)
	seedOperation(t, st, "busy-1", "transcode_media", "waiting_external", map[string]any{"path": "/media/X.mkv"}, nil,
		map[string]any{"transcode_queue_order": "1|2026-01-01T00:00:00Z|g|00000000000000000000", "worker": "m1"})
	_, q := queueIDsOf(t, h)
	rev := q["queue_revision"].(float64)

	type tc struct {
		name, path, body string
		status           int
		code             string
		msg              string
	}
	for _, c := range []tc{
		{"hold with a worker", "/transcode/v1/queue/actions/busy-1/hold", ``, 409, "queue_conflict", "worker"},
		{"move with a worker", "/transcode/v1/queue/actions/busy-1/move", `{"first":true}`, 409, "queue_conflict", "worker"},
		{"unknown action", "/transcode/v1/queue/actions/nope/hold", ``, 404, "queue_entry_not_found", ""},
		{"unknown item", "/transcode/v1/queue/items/999/hold", ``, 404, "queue_entry_not_found", ""},
		{"unknown batch", "/transcode/v1/queue/batches/nope/hold", ``, 404, "queue_entry_not_found", ""},
		{"item id not a number", "/transcode/v1/queue/items/abc/hold", ``, 400, "invalid_request", ""},
		{"move to itself", "/transcode/v1/queue/actions/w-1/move", `{"before":{"kind":"action","id":"w-1"}}`, 400, "invalid_request", ""},
		{"move relative to a missing ref", "/transcode/v1/queue/actions/w-1/move", `{"after":{"kind":"action","id":"nope"}}`, 404, "queue_entry_not_found", ""},
		{"move relative to a running one", "/transcode/v1/queue/actions/w-1/move", `{"after":{"kind":"action","id":"busy-1"}}`, 409, "queue_conflict", ""},
		{"no place", "/transcode/v1/queue/actions/w-1/move", `{}`, 400, "invalid_request", ""},
		{"two places", "/transcode/v1/queue/actions/w-1/move", `{"first":true,"after":{"kind":"action","id":"w-2"}}`, 400, "invalid_request", ""},
		{"bad kind", "/transcode/v1/queue/actions/w-1/move", `{"after":{"kind":"thing","id":"w-2"}}`, 400, "invalid_request", ""},
		{"unknown field", "/transcode/v1/queue/actions/w-1/move", `{"first":true,"nope":1}`, 400, "invalid_request", ""},
	} {
		code, r := postQ(t, h, c.path, c.body)
		if code != c.status || r["code"] != c.code || r["error"] == "" || (c.msg != "" && !strings.Contains(r["error"].(string), c.msg)) {
			t.Errorf("%s: %d %v, want %d %s", c.name, code, r, c.status, c.code)
		}
	}
	// A stale revision changes nothing.
	for _, p := range []string{"hold", "release", "move"} {
		body := `{"queue_revision": 99999}`
		if p == "move" {
			body = `{"first":true,"queue_revision": 99999}`
		}
		code, r := postQ(t, h, "/transcode/v1/queue/actions/w-2/"+p, body)
		if code != 409 || r["code"] != "stale_queue_revision" {
			t.Errorf("stale %s: %d %v", p, code, r)
		}
	}
	ids, q := queueIDsOf(t, h)
	if strings.Join(ids, ",") != "w-1,w-2" || q["queue_revision"].(float64) != rev {
		t.Fatalf("refusals changed the queue: %v at %v (was %v)", ids, q["queue_revision"], rev)
	}
	// The current revision is accepted.
	if code, r := postQ(t, h, "/transcode/v1/queue/actions/w-2/move", `{"first":true,"queue_revision":`+jsonNum(rev)+`}`); code != 200 {
		t.Fatal(code, r)
	}
}

func jsonNum(f float64) string { b, _ := json.Marshal(f); return string(b) }

func TestTranscodeV1QueueItemsAndBatches(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	seedWaiter(t, st, "solo", "1|2026-01-01T00:00:01Z|g|00000000000000000001", "/media/S.mkv", nil)
	seedOperation(t, st, "batch-q", "transcode_batch", "waiting_external", nil, nil, map[string]any{})
	for _, k := range []string{"k1", "k2"} {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-q", ItemKey: k, FilePath: "/media/" + k + ".mkv", Status: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	code, q := get(t, h, "/transcode/v1/queue", keyHdr())
	if code != 200 || len(q["entries"].([]any)) != 3 {
		t.Fatal(code, q)
	}
	var itemRef map[string]any
	for _, e := range q["entries"].([]any) {
		en := e.(map[string]any)
		if en["batch_id"] == "batch-q" {
			itemRef = en["ref"].(map[string]any)
			break
		}
	}
	if itemRef["kind"] != "item" {
		t.Fatal(itemRef)
	}
	id := jsonNum(itemRef["id"].(float64))
	// Hold the item, then move it to the front by number.
	if code, r := postQ(t, h, "/transcode/v1/queue/items/"+id+"/hold", ``); code != 200 || r["entry"].(map[string]any)["held"] != true {
		t.Fatal(code, r)
	}
	if code, r := postQ(t, h, "/transcode/v1/queue/items/"+id+"/move", `{"after":{"kind":"action","id":"solo"}}`); code != 200 {
		t.Fatal(code, r)
	}
	_, q = get(t, h, "/transcode/v1/queue", keyHdr())
	second := q["entries"].([]any)[1].(map[string]any)
	if second["ref"].(map[string]any)["id"] != itemRef["id"] || second["held"] != true {
		t.Fatalf("the item should be second and held: %v", q["entries"])
	}
	// The batch moves as a block, ahead of the solo conversion.
	if code, r := postQ(t, h, "/transcode/v1/queue/batches/batch-q/move", `{"before":{"kind":"action","id":"solo"}}`); code != 200 || r["batch"] == nil || r["batch"].(map[string]any)["waiting"].(float64) != 2 {
		t.Fatal(code, r)
	}
	_, q = get(t, h, "/transcode/v1/queue", keyHdr())
	if last := q["entries"].([]any)[2].(map[string]any); last["ref"].(map[string]any)["id"] != "solo" {
		t.Fatalf("solo should be last: %v", q["entries"])
	}
	// A refused batch hold leaves no mark: an unknown batch and a plain action.
	if code, _ := postQ(t, h, "/transcode/v1/queue/batches/solo/hold", ``); code != 404 {
		t.Fatal("an action is not a batch", code)
	}
}

func TestTranscodeV1QueueRoutesNeedTheKeyAndAreInTheSpec(t *testing.T) {
	_, h := transcodeUI(t)
	for _, p := range []string{"actions/x", "items/1", "batches/x"} {
		for _, a := range []string{"hold", "release", "move"} {
			if w := v1(h, "POST", "/transcode/v1/queue/"+p+"/"+a, `{}`, map[string]string{"Content-Type": "application/json"}); w.Code != 401 {
				t.Errorf("%s/%s without a key: %d", p, a, w.Code)
			}
		}
	}
	if w := v1(h, "GET", "/transcode/v1/queue", "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	spec := string(v1(h, "GET", "/transcode/v1/openapi.json", "", nil).Body.String())
	for _, want := range []string{`"/transcode/v1/queue"`, `/queue/batches/{id}/move`, `queue_held`} {
		if !strings.Contains(spec, want) {
			t.Errorf("the served spec lacks %s", want)
		}
	}
}

// "01" and "+1" are the item 1; an id that is not a string is refused for an
// action or a batch rather than read as one.
func TestTranscodeV1QueueIdsAreNormalisedAndTyped(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	seedWaiter(t, st, "solo", "1|2026-01-01T00:00:01Z|g|00000000000000000001", "/media/S.mkv", nil)
	seedOperation(t, st, "batch-q", "transcode_batch", "waiting_external", nil, nil, map[string]any{})
	for _, k := range []string{"k1", "k2"} {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-q", ItemKey: k, FilePath: "/media/" + k + ".mkv", Status: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := st.GetTranscodeBatchItem("batch-q", "k1")
	second, _ := st.GetTranscodeBatchItem("batch-q", "k2")
	n1, n2 := strconv.FormatInt(first.ID, 10), strconv.FormatInt(second.ID, 10)

	for _, id := range []string{"0" + n1, "+" + n1, "00" + n1} {
		code, r := postQ(t, h, "/transcode/v1/queue/items/"+id+"/hold", ``)
		if code != 200 || r["entry"].(map[string]any)["held"] != true {
			t.Fatalf("hold on item %q: %d %v", id, code, r)
		}
		if code, r := postQ(t, h, "/transcode/v1/queue/items/"+n1+"/release", ``); code != 200 {
			t.Fatal(code, r)
		}
	}
	// In a body reference, as a string or a number.
	for _, ref := range []string{`{"kind":"item","id":"+` + n2 + `"}`, `{"kind":"item","id":"0` + n2 + `"}`, `{"kind":"item","id":` + n2 + `}`} {
		if code, r := postQ(t, h, "/transcode/v1/queue/items/"+n1+"/move", `{"after":`+ref+`}`); code != 200 {
			t.Fatalf("move after %s: %d %v", ref, code, r)
		}
	}
	for _, body := range []string{
		`{"after":{"kind":"action","id":5}}`,
		`{"before":{"kind":"batch","id":7}}`,
		`{"after":{"kind":"action","id":true}}`,
		`{"after":{"kind":"batch","id":{"a":1}}}`,
	} {
		if code, r := postQ(t, h, "/transcode/v1/queue/actions/solo/move", body); code != 400 || r["code"] != "invalid_request" {
			t.Errorf("%s: %d %v, want 400 invalid_request", body, code, r)
		}
	}
}
