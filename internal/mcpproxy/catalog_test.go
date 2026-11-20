package mcpproxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func twoPages(f *fake) {
	names := ProxyTools()
	f.setPage("", toolsResult(names[:20], "p2"))
	f.setPage("p2", toolsResult(names[20:], ""))
}

func TestCatalogFollowsPages(t *testing.T) {
	f := newFake(t)
	twoPages(f)
	g := newGateway(t, f.url(), nil)
	if n := listedNames(t, g.list()); len(n) != 50 {
		t.Fatalf("want 50 tools across two pages, got %d", len(n))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.cursors, "|") != "|p2" {
		t.Errorf("cursors %q", f.cursors)
	}
}

func TestCatalogIsCachedForTheTTL(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), nil)
	g.list()
	g.list()
	if f.lists.Load() != 1 {
		t.Fatalf("lists %d, want 1 within the TTL", f.lists.Load())
	}
	g.clock.Advance(61 * time.Second)
	g.list()
	if f.lists.Load() != 2 {
		t.Fatalf("lists %d, want a refresh after the TTL", f.lists.Load())
	}
}

func TestPageFailureKeepsThePreviousCatalogAndBacksOff(t *testing.T) {
	f := newFake(t)
	twoPages(f)
	g := newGateway(t, f.url(), nil)
	if len(listedNames(t, g.list())) != 50 {
		t.Fatal("first catalog")
	}
	f.onList = func(w http.ResponseWriter, r *http.Request, env envelope) bool {
		if strings.Contains(string(env.Params), "p2") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		return false
	}
	g.clock.Advance(61 * time.Second)
	if len(listedNames(t, g.list())) != 50 {
		t.Fatal("a failed refresh must serve the last good catalog")
	}
	after := f.lists.Load() // page 1 ok, page 2 failed
	g.clock.Advance(30 * time.Second)
	g.list()
	if f.lists.Load() != after {
		t.Error("no new attempt before the 60 s back-off ends")
	}
	g.clock.Advance(31 * time.Second)
	f.onList = nil
	g.list()
	if f.lists.Load() == after {
		t.Error("expected a new attempt after the back-off")
	}
	// The new attempt started again from the first page.
	f.mu.Lock()
	last := f.cursors[len(f.cursors)-2:]
	f.mu.Unlock()
	if last[0] != "" || last[1] != "p2" {
		t.Errorf("cursors of the retry: %q", last)
	}
}

func TestSessionLossOnALaterPageAbortsTheRefresh(t *testing.T) {
	f := newFake(t)
	twoPages(f)
	g := newGateway(t, f.url(), nil)
	g.list()
	g.clock.Advance(61 * time.Second)
	f.onList = func(w http.ResponseWriter, r *http.Request, env envelope) bool {
		if strings.Contains(string(env.Params), "p2") {
			sessionGone(w)
			return true
		}
		return false
	}
	inits := f.inits.Load()
	if len(listedNames(t, g.list())) != 50 {
		t.Fatal("the previous catalog must be served")
	}
	if f.inits.Load() != inits {
		t.Error("a 404 on a later page must not reinitialize mid-pagination")
	}
}

func TestRepeatedCursorIsAnError(t *testing.T) {
	f := newFake(t)
	f.setPage("", toolsResult(ProxyTools()[:10], "again"))
	f.setPage("again", toolsResult(ProxyTools()[10:20], "again"))
	g := newGateway(t, f.url(), nil)
	r := g.list()
	want200JSON(t, r)
	if _, res, e := r.rpc(t); res != nil || e == nil {
		t.Fatalf("want a JSON-RPC error, got %s", r.body)
	}
}

func TestPageCap(t *testing.T) {
	f := newFake(t)
	f.onList = func(w http.ResponseWriter, r *http.Request, env envelope) bool {
		n := int(f.lists.Load())
		jsonOK(w, env.ID, toolsResult(nil, fmt.Sprintf("c%d", n)))
		return true
	}
	g := newGateway(t, f.url(), func(o *Options) { o.MaxPages = 3 })
	r := g.list()
	if _, res, e := r.rpc(t); res != nil || e == nil {
		t.Fatalf("want an error past the page cap, got %s", r.body)
	}
	if f.lists.Load() != 3 {
		t.Errorf("fetched %d pages, cap is 3", f.lists.Load())
	}
}

func TestMissingProxiedToolNeverReplacesTheCatalog(t *testing.T) {
	f := newFake(t)
	missing := toolsResult(ProxyTools()[1:], "")
	f.setPage("", missing)
	g := newGateway(t, f.url(), nil)

	// No catalog yet: an error, not a short list.
	r := g.list()
	if _, res, e := r.rpc(t); res != nil || e == nil {
		t.Fatalf("want an error without a complete catalog, got %s", r.body)
	}
	if len(g.logs.errors()) == 0 {
		t.Error("an incomplete catalog must be logged as an ERROR")
	}

	// A complete one loads; a later incomplete refresh does not replace it.
	f.setPage("", toolsResult(ProxyTools(), ""))
	if len(listedNames(t, g.list())) != 50 {
		t.Fatal("complete catalog")
	}
	f.setPage("", missing)
	g.clock.Advance(61 * time.Second)
	if len(listedNames(t, g.list())) != 50 {
		t.Error("the cache must survive an incomplete refresh")
	}
}

func TestExtraUpstreamToolsAreNotPublished(t *testing.T) {
	f := newFake(t)
	f.setPage("", toolsResult(append(ProxyTools(), "recipe_list", "list_services"), ""))
	g := newGateway(t, f.url(), nil)
	for _, n := range listedNames(t, g.list()) {
		if n == "recipe_list" || n == "list_services" {
			t.Errorf("%s must not be published from Transcode", n)
		}
	}
}

func TestConcurrentListsRefreshOnce(t *testing.T) {
	f := newFake(t)
	f.onList = func(w http.ResponseWriter, r *http.Request, env envelope) bool {
		time.Sleep(150 * time.Millisecond)
		return false
	}
	g := newGateway(t, f.url(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if len(listedNames(t, g.list())) != 50 {
				t.Error("short list")
			}
		}()
	}
	wg.Wait()
	if f.lists.Load() != 1 || f.inits.Load() != 1 {
		t.Errorf("lists=%d inits=%d, want one of each", f.lists.Load(), f.inits.Load())
	}
}

func TestHungTranscodeWithACacheAnswersWithinTheBudget(t *testing.T) {
	f := newFake(t)
	g := newGateway(t, f.url(), func(o *Options) { o.RefreshBudget = 200 * time.Millisecond })
	g.list()
	f.onList = func(w http.ResponseWriter, r *http.Request, env envelope) bool {
		<-r.Context().Done()
		return true
	}
	g.clock.Advance(61 * time.Second)
	start := time.Now()
	if len(listedNames(t, g.list())) != 50 {
		t.Fatal("the old catalog must be served")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s, the refresh budget is 200ms", d)
	}
}

func TestTranscodeDownAtStartThenRecovers(t *testing.T) {
	addr := closedAddr(t)
	g := newGateway(t, "http://"+addr+"/mcp", nil)

	r := g.list()
	want200JSON(t, r)
	if _, res, e := r.rpc(t); res != nil || e == nil {
		t.Fatalf("want a JSON-RPC error, not a short list: %s", r.body)
	}
	// Transcode comes up on that address: the next tools/list works with no
	// restart and no waiting between attempts.
	f := newFakeOn(t, listenOn(t, addr))
	if n := listedNames(t, g.list()); len(n) != 50 {
		t.Fatalf("after recovery: %d tools", len(n))
	}

	// And with a catalog, Transcode going away keeps serving it.
	f.srv.Close()
	g.clock.Advance(61 * time.Second)
	if n := listedNames(t, g.list()); len(n) != 50 {
		t.Fatalf("last good catalog: %d tools", len(n))
	}
}
