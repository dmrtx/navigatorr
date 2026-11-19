package nodes

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func idsOf(t *testing.T, page map[string]any) []string {
	t.Helper()
	bs, _ := page["builds"].([]any)
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.(map[string]any)["id"].(string))
	}
	return out
}

func makeBuilds(t *testing.T, h http.Handler, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bld_page%04d", i)
		if code, e := call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"`+id+`","target":"remote-box","source":"ci","title":"t"}`); code != 200 {
			t.Fatal(code, e)
		}
		ids = append(ids, id)
	}
	return ids
}

// Paging walks every build exactly once, newest created first, even when a
// build changes status between two pages (finishing moves nothing).
func TestAPIBuildsPagingIsStableWhileStatusesChange(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	created := makeBuilds(t, h, 9)
	want := make([]string, 0, len(created))
	for i := len(created) - 1; i >= 0; i-- {
		want = append(want, created[i])
	}
	var got []string
	cursor, pages := "", 0
	for {
		url := "/nodes/v1/builds?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		code, page := call(t, h, "GET", url, "")
		if code != 200 {
			t.Fatal(code, page)
		}
		got = append(got, idsOf(t, page)...)
		pages++
		// Between pages: one build already listed finishes, one not yet listed
		// finishes, one not yet listed is cancelled. None may move or vanish.
		switch pages {
		case 1:
			finishBuild(t, s, got[0], "succeeded")
			finishBuild(t, s, created[2], "failed")
		case 2:
			if code, e := call(t, h, "POST", "/nodes/v1/builds/"+created[0]+"/cancel", ""); code != 200 {
				t.Fatal(code, e)
			}
		}
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("every build once, newest first:\n got %v\nwant %v", got, want)
	}
	if pages != 5 {
		t.Fatalf("9 builds at 2 per page is 5 pages, got %d", pages)
	}
}

// A cursor stays valid after the build it came from is pruned.
func TestAPIBuildsCursorSurvivesPruning(t *testing.T) {
	s, clock := buildSvc(t)
	h := s.Handler()
	created := makeBuilds(t, h, 5) // newest first: 4 3 2 1 0
	_, page := call(t, h, "GET", "/nodes/v1/builds?limit=2", "")
	next, _ := page["next_cursor"].(string)
	if got := idsOf(t, page); len(got) != 2 || got[1] != created[3] || next == "" {
		t.Fatal(got, next)
	}
	// The build the cursor names ends and its history is pruned.
	finishBuild(t, s, created[3], "succeeded")
	clock.advance(DefaultRetention + time.Hour)
	tick(s) // the others expire (ended now) and stay; the finished one is pruned
	if _, err := s.GetBuild(created[3]); err == nil {
		t.Fatal("the cursor's build must be pruned for this test")
	}
	code, page2 := call(t, h, "GET", "/nodes/v1/builds?limit=2&cursor="+next, "")
	if code != 200 {
		t.Fatal("a pruned cursor build is not an error", code, page2)
	}
	if got := idsOf(t, page2); len(got) != 2 || got[0] != created[2] || got[1] != created[1] {
		t.Fatalf("it continues after it: %v", got)
	}
}

func TestAPIBuildsLimitBoundsAndLastPage(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	makeBuilds(t, h, 4)
	for _, bad := range []string{"0", "-1", "501", "abc", "1.5"} {
		if code, e := call(t, h, "GET", "/nodes/v1/builds?limit="+bad, ""); code != 400 || e["code"] != "invalid_request" {
			t.Fatalf("limit=%s: %d %v", bad, code, e)
		}
	}
	for _, ok := range []string{"1", "500"} {
		if code, _ := call(t, h, "GET", "/nodes/v1/builds?limit="+ok, ""); code != 200 {
			t.Fatalf("limit=%s must be accepted: %d", ok, code)
		}
	}
	// Exactly a page's worth: no next_cursor. One short of it: there is one.
	if _, page := call(t, h, "GET", "/nodes/v1/builds?limit=4", ""); page["next_cursor"] != nil || len(idsOf(t, page)) != 4 {
		t.Fatal("no next_cursor on the last page", page)
	}
	_, page := call(t, h, "GET", "/nodes/v1/builds?limit=3", "")
	next, _ := page["next_cursor"].(string)
	if next == "" {
		t.Fatal(page)
	}
	if _, last := call(t, h, "GET", "/nodes/v1/builds?limit=3&cursor="+next, ""); len(idsOf(t, last)) != 1 || last["next_cursor"] != nil {
		t.Fatal("the last page has the rest and no next_cursor", last)
	}
	for _, bad := range []string{"abc", "0", "-3", "007", "1e3"} {
		if code, e := call(t, h, "GET", "/nodes/v1/builds?cursor="+bad, ""); code != 400 || e["code"] != "invalid_cursor" || e["hint"] == "" {
			t.Fatalf("cursor=%s: %d %v", bad, code, e)
		}
	}
	// A cursor past everything is just an empty last page.
	if code, p := call(t, h, "GET", "/nodes/v1/builds?cursor=1", ""); code != 200 || len(idsOf(t, p)) != 0 || p["next_cursor"] != nil {
		t.Fatal(code, p)
	}
}

func TestAPIBuildsDefaultPageIs50(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	makeBuilds(t, h, 55)
	_, page := call(t, h, "GET", "/nodes/v1/builds", "")
	if n := len(idsOf(t, page)); n != 50 || page["next_cursor"] == nil {
		t.Fatalf("default page: %d builds, %v", n, page["next_cursor"])
	}
}

// Filters keep working with the cursor.
func TestAPIBuildsFiltersWithCursor(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	ids := makeBuilds(t, h, 6)
	for _, id := range ids[:4] {
		finishBuild(t, s, id, "succeeded")
	}
	_, p := call(t, h, "GET", "/nodes/v1/builds?status=succeeded&limit=3", "")
	next, _ := p["next_cursor"].(string)
	if got := idsOf(t, p); len(got) != 3 || next == "" {
		t.Fatal(p)
	}
	_, p2 := call(t, h, "GET", "/nodes/v1/builds?status=succeeded&limit=3&cursor="+next, "")
	if got := idsOf(t, p2); len(got) != 1 || got[0] != ids[0] || p2["next_cursor"] != nil {
		t.Fatal(p2)
	}
}
