package maintenanceui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/internal/nashelper"
	"github.com/jakenesler/navigatorr/store"
)

func TestLibraryAsksTheArrOncePerCollectionAndKeepsPagesIndependent(t *testing.T) {
	s, h := testUI(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var movies []map[string]any
		for i := 0; i < 250; i++ {
			movies = append(movies, map[string]any{"id": i + 1, "title": fmt.Sprintf("Movie %03d", i), "path": fmt.Sprintf("/media/movies/%03d", i), "overview": "dropped", "statistics": map[string]any{"sizeOnDisk": 1000 + i}})
		}
		_ = json.NewEncoder(w).Encode(movies)
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: upstream.URL, APIVersion: "/api/v3"}}})
	page := func(query string) (items []map[string]any, total int, header http.Header) {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/library?service=radarr&"+query, "", true)
		var body struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return body.Items, body.Total, w.Header()
	}
	first, total, header := page("offset=0")
	if len(first) != 100 || total != 250 || first[0]["title"] != "Movie 000" || first[0]["overview"] != nil {
		t.Fatal(len(first), total, first[0])
	}
	if !strings.Contains(header.Get("Server-Timing"), "arr;") || !strings.Contains(header.Get("Server-Timing"), "total;") {
		t.Fatal("no timing:", header.Get("Server-Timing"))
	}
	third, _, header := page("offset=200")
	search, searchTotal, _ := page("q=movie%2024&sort=size_desc")
	if len(third) != 50 || searchTotal != 10 || search[0]["title"] != "Movie 249" {
		t.Fatal(len(third), searchTotal, search)
	}
	if calls.Load() != 1 || !strings.Contains(header.Get("Server-Timing"), `desc="cached"`) {
		t.Fatalf("paging and searching asked the *arr %d times", calls.Load())
	}
	// A page is marked on a copy: the shared list never learns of the mark.
	st := s.engine.Deps().Store
	if err := st.CreateActionInstance(store.ActionInstance{ID: "batch", ActionName: "transcode_batch", Status: store.ActionStatusRunning, InputsJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "a", FilePath: "/media/movies/000/film.mkv", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	s.fileStatusCache.invalidate()
	if marked, _, _ := page("offset=0"); marked[0]["nav_status"] == nil {
		t.Fatal("the first title is not marked")
	}
	if _, hit, _ := s.arrLists.get("radarr", false, newServerTiming(), nil); !hit {
		t.Fatal("expected a cached list")
	}
	radarr := s.arrLists.entry("radarr")
	radarr.mu.Lock()
	shared := radarr.items
	radarr.mu.Unlock()
	for _, item := range shared {
		if _, leaked := item["nav_status"]; leaked {
			t.Fatal("a mark leaked into the shared list")
		}
	}
	// A refresh, or any change made through the console, asks again.
	page("refresh=1")
	if calls.Load() != 2 {
		t.Fatal("refresh=1 did not ask again", calls.Load())
	}
	request(h, "POST", "/api/maintenance/rerun", `{}`, true)
	page("offset=0")
	if calls.Load() != 3 {
		t.Fatal("a change did not end the cached list", calls.Load())
	}
}

func TestLibraryFailedFetchIsNotKept(t *testing.T) {
	s, h := testUI(t)
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", 500)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "title": "A", "path": "/media/a"}})
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: upstream.URL, APIVersion: "/api/v3"}}})
	fail.Store(true)
	if w := request(h, "GET", "/api/maintenance/library?service=radarr", "", true); w.Code != 502 {
		t.Fatal(w.Code)
	}
	fail.Store(false)
	if w := request(h, "GET", "/api/maintenance/library?service=radarr", "", true); w.Code != 200 {
		t.Fatal("recovery", w.Code, w.Body.String())
	}
}

func TestFileStatusesServeAnOldIndexWhileRefreshingAndWaitAfterAChange(t *testing.T) {
	s, _ := testUI(t)
	st := s.engine.Deps().Store
	if err := st.CreateActionInstance(store.ActionInstance{ID: "batch", ActionName: "transcode_batch", Status: store.ActionStatusRunning, InputsJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	add := func(key string) {
		t.Helper()
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: key, FilePath: "/media/" + key + ".mkv", Status: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	add("one")
	if s.fileStatuses().file("/media/one.mkv") == nil {
		t.Fatal("first read must wait for an index")
	}
	add("two")
	// Not old enough to read again: the index is used as it is.
	if s.fileStatuses().file("/media/two.mkv") != nil {
		t.Fatal("a fresh index was read again")
	}
	// Old, but not too old: shown at once, while the new one is read.
	c := &s.fileStatusCache
	c.mu.Lock()
	c.at = time.Now().Add(-10 * time.Second)
	c.mu.Unlock()
	if s.fileStatuses().file("/media/two.mkv") != nil {
		t.Fatal("an old index must be shown without waiting")
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.fileStatuses().file("/media/two.mkv") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the background read never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A change made through the console is never hidden behind an old index.
	add("three")
	c.invalidate()
	if s.fileStatuses().file("/media/three.mkv") == nil {
		t.Fatal("a read after a change must be fresh")
	}
}

func TestFolderListingPagesByNameWithoutStattingTheRest(t *testing.T) {
	s, h := testUI(t)
	root := t.TempDir()
	for i := 0; i < 130; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("film%03d.mkv", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0600)
	os.Mkdir(filepath.Join(root, "Extras"), 0700)
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	os.WriteFile(outside, []byte("x"), 0600)
	os.Symlink(outside, filepath.Join(root, "escaped.mkv"))
	os.Symlink(filepath.Join(root, "film000.mkv"), filepath.Join(root, "inside.mkv"))
	os.Symlink(filepath.Join(root, "missing.mkv"), filepath.Join(root, "broken.mkv"))
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	type page struct {
		Items []struct {
			Path    string `json:"path"`
			Size    int64  `json:"size"`
			IsDir   bool   `json:"is_dir"`
			ModTime string `json:"mod_time"`
		} `json:"items"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	read := func(query string) (page, http.Header) {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/folder?path="+url.QueryEscape(root)+query, "", true)
		var p page
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return p, w.Header()
	}
	first, header := read("")
	// 130 films, Extras and the link inside the folder; the escaped and broken links and the note are not listed.
	if first.Total != 132 || len(first.Items) != 100 || !first.HasMore || !strings.Contains(header.Get("Server-Timing"), "entries;") {
		t.Fatal(first.Total, len(first.Items), first.HasMore, header)
	}
	if filepath.Base(first.Items[0].Path) != "Extras" || !first.Items[0].IsDir || first.Items[1].Size != 1 || first.Items[1].ModTime == "" {
		t.Fatalf("%+v", first.Items[:2])
	}
	rest, _ := read("&offset=100")
	if len(rest.Items) != 32 || rest.HasMore {
		t.Fatal(len(rest.Items), rest.HasMore)
	}
	// The link resolves to its target, as before, so it sorts as film000.
	names := map[string]int{}
	for _, p := range []page{first, rest} {
		for _, item := range p.Items {
			names[filepath.Base(item.Path)]++
			if strings.Contains(item.Path, "escaped") || strings.Contains(item.Path, "broken") {
				t.Fatal("listed", item.Path)
			}
		}
	}
	if names["film000.mkv"] != 2 || names["inside.mkv"] != 0 || len(names) != 131 {
		t.Fatal(names["film000.mkv"], names["inside.mkv"], len(names))
	}
	desc, _ := read("&sort=name_desc")
	if filepath.Base(desc.Items[0].Path) != "film129.mkv" {
		t.Fatal(desc.Items[0].Path)
	}
}

func TestLibraryIndexViewAndETag(t *testing.T) {
	s, h := testUI(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var movies []map[string]any
		for i := 0; i < 250; i++ {
			movies = append(movies, map[string]any{"id": i + 1, "title": fmt.Sprintf("Movie %03d", i), "year": 2000 + i%20, "path": fmt.Sprintf("/media/movies/%03d", i), "overview": "dropped", "statistics": map[string]any{"sizeOnDisk": 1000 + i}})
		}
		_ = json.NewEncoder(w).Encode(movies)
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: upstream.URL, APIVersion: "/api/v3"}}})
	get := func(query, ifNoneMatch string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/maintenance/library?service=radarr&"+query, nil)
		r.Header.Set("Authorization", "Bearer "+testToken)
		if ifNoneMatch != "" {
			r.Header.Set("If-None-Match", ifNoneMatch)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	first := get("view=index", "")
	var body struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
		View  string           `json:"view"`
	}
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &body) != nil || body.Total != 250 || len(body.Items) != 250 || body.View != "index" {
		t.Fatal(first.Code, first.Body.Len())
	}
	if body.Items[0]["title"] != "Movie 000" || body.Items[0]["size"] != float64(1000) || body.Items[0]["overview"] != nil || body.Items[0]["year"] != float64(2000) {
		t.Fatalf("compact item: %+v", body.Items[0])
	}
	tag := first.Header().Get("ETag")
	if !strings.HasPrefix(tag, `W/"`) || first.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("tag", tag, first.Header())
	}
	// Same list, same tag: no body.
	again := get("view=index", tag)
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 || again.Header().Get("ETag") != tag {
		t.Fatal("expected 304", again.Code, again.Body.Len())
	}
	if get("view=index", `"other", `+tag).Code != http.StatusNotModified || get("view=index", "*").Code != http.StatusNotModified {
		t.Fatal("a listed tag or * must match")
	}
	if get("view=index", `W/"stale"`).Code != 200 {
		t.Fatal("a stale tag must get the list")
	}
	if calls.Load() != 1 {
		t.Fatal("the *arr was asked", calls.Load())
	}
	// A change in a mark changes the tag, so a 304 never hides it.
	st := s.engine.Deps().Store
	if err := st.CreateActionInstance(store.ActionInstance{ID: "batch", ActionName: "transcode_batch", Status: store.ActionStatusRunning, InputsJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "a", FilePath: "/media/movies/000/film.mkv", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	s.fileStatusCache.invalidate()
	changed := get("view=index", tag)
	if changed.Code != 200 || changed.Header().Get("ETag") == tag || !strings.Contains(changed.Body.String(), `"nav_status"`) {
		t.Fatal("a new mark must give a new tag", changed.Code)
	}
	// The paged form is unchanged for older clients, and also carries a tag.
	paged := get("offset=0", "")
	var page struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"has_more"`
	}
	if paged.Code != 200 || json.Unmarshal(paged.Body.Bytes(), &page) != nil || len(page.Items) != 100 || !page.HasMore || paged.Header().Get("ETag") == "" {
		t.Fatal(paged.Code)
	}
	if get("offset=0", paged.Header().Get("ETag")).Code != http.StatusNotModified {
		t.Fatal("a page must revalidate too")
	}
	// A title is not an index view; a missing service stays an error.
	if w := get("view=index&id=3", ""); w.Code != 200 || strings.Contains(w.Body.String(), `"view":"index"`) {
		t.Fatal("per-title requests keep their own form", w.Code)
	}
}

func TestFailedRefreshKeepsTheLastGoodListAndLoginDoesNotEmptyTheCaches(t *testing.T) {
	s, h := testUI(t)
	var down atomic.Bool
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if down.Load() {
			http.Error(w, "down", 500)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "title": "A", "path": "/media/a"}})
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: upstream.URL, APIVersion: "/api/v3"}}})
	if w := request(h, "GET", "/api/maintenance/library?service=radarr", "", true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	// Anyone may post to login; that must not make the next page ask again.
	request(h, "POST", "/api/maintenance/login", `{"token":"wrong"}`, false)
	request(h, "POST", "/api/maintenance/logout", ``, false)
	if w := request(h, "GET", "/api/maintenance/library?service=radarr", "", true); w.Code != 200 || calls.Load() != 1 {
		t.Fatal("login emptied the cache", w.Code, calls.Load())
	}
	// An explicit refresh that fails is an error, but the good list is still there.
	down.Store(true)
	if w := request(h, "GET", "/api/maintenance/library?service=radarr&refresh=1", "", true); w.Code != 502 {
		t.Fatal(w.Code)
	}
	if w := request(h, "GET", "/api/maintenance/library?service=radarr", "", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"A"`) {
		t.Fatal("the last good list was lost", w.Code)
	}
}

func TestUnreadableStoreIsNotReadAgainOnEveryPage(t *testing.T) {
	s, _ := testUI(t)
	s.engine.Deps().Store.Close()
	if s.fileStatuses() != nil {
		t.Fatal("an unreadable store has no marks")
	}
	c := &s.fileStatusCache
	c.mu.Lock()
	first := c.failedAt
	c.mu.Unlock()
	if first.IsZero() {
		t.Fatal("the failure was not noted")
	}
	if s.fileStatuses() != nil {
		t.Fatal("still no marks")
	}
	c.mu.Lock()
	again := c.failedAt
	c.mu.Unlock()
	if !again.Equal(first) {
		t.Fatal("the store was read again inside the retry interval")
	}
}

func TestARefreshThatWaitedBehindAFetchDoesNotFetchAgain(t *testing.T) {
	s, h := testUI(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "title": "A", "path": "/media/a"}})
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: upstream.URL, APIVersion: "/api/v3"}}})
	done := make(chan int, 3)
	for i := 0; i < 3; i++ {
		go func() {
			done <- request(h, "GET", "/api/maintenance/library?service=radarr&refresh=1", "", true).Code
		}()
		time.Sleep(50 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		if code := <-done; code != 200 {
			t.Fatal(code)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("three refreshes that overlapped asked the *arr %d times", calls.Load())
	}
}

// With the NAS helper mapped to the folder, the listing comes from the helper
// in one call: same names, order, totals and page as the mount gives, links are
// still followed through the mount and refused outside the read roots, and no
// entry that is not a link is stat'ed through the mount.
func TestFolderListingFromTheHelperMatchesTheMount(t *testing.T) {
	s, h := testUI(t)
	root := t.TempDir()
	var entries []nashelper.ListEntry
	for i := 0; i < 130; i++ {
		name := fmt.Sprintf("film%03d.mkv", i)
		// Nothing exists on the mount for these: the sizes can only come from the helper.
		entries = append(entries, nashelper.ListEntry{Name: name, Type: nashelper.EntryFile, Size: int64(1000 + i), MtimeNs: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano()})
	}
	entries = append(entries,
		nashelper.ListEntry{Name: "Extras", Type: nashelper.EntryDir, MtimeNs: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano()},
		nashelper.ListEntry{Name: "target.mkv", Type: nashelper.EntryFile, Size: 5},
		nashelper.ListEntry{Name: "notes.txt", Type: nashelper.EntryFile, Size: 1},
		nashelper.ListEntry{Name: ".hidden.mkv", Type: nashelper.EntryFile, Size: 1},
		nashelper.ListEntry{Name: "pipe.mkv", Type: nashelper.EntryOther},
		nashelper.ListEntry{Name: "escaped.mkv", Type: nashelper.EntrySymlink},
		nashelper.ListEntry{Name: "inside.mkv", Type: nashelper.EntrySymlink},
		nashelper.ListEntry{Name: "broken.mkv", Type: nashelper.EntrySymlink},
	)
	target := filepath.Join(root, "target.mkv")
	os.WriteFile(target, []byte("12345"), 0600)
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	os.WriteFile(outside, []byte("x"), 0600)
	os.Symlink(outside, filepath.Join(root, "escaped.mkv"))
	os.Symlink(target, filepath.Join(root, "inside.mkv"))
	os.Symlink(filepath.Join(root, "missing.mkv"), filepath.Join(root, "broken.mkv"))
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	canonical := deps.Fs.ReadRoots()[0]
	meter := &fakeMeter{prefix: filepath.Dir(canonical), list: map[string]nashelper.ListResponse{canonical: {InstanceID: "test", Entries: entries}}}
	s.nas = meter

	type page struct {
		Items []struct {
			Path    string `json:"path"`
			Size    int64  `json:"size"`
			IsDir   bool   `json:"is_dir"`
			ModTime string `json:"mod_time"`
		} `json:"items"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	read := func(query string) (page, http.Header) {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/folder?path="+url.QueryEscape(canonical)+query, "", true)
		var p page
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return p, w.Header()
	}
	first, header := read("")
	// 130 films, Extras, the target and the link that leads to it; the escaped and broken links, the note, the hidden file and the pipe are not listed.
	if first.Total != 133 || len(first.Items) != 100 || !first.HasMore {
		t.Fatal(first.Total, len(first.Items), first.HasMore)
	}
	if !strings.Contains(header.Get("Server-Timing"), `list;dur=0;desc="helper"`) {
		t.Fatal(header.Get("Server-Timing"))
	}
	if filepath.Base(first.Items[0].Path) != "Extras" || !first.Items[0].IsDir || first.Items[1].Size != 1000 || first.Items[1].ModTime != "2026-01-02T03:04:05Z" {
		t.Fatalf("%+v", first.Items[:2])
	}
	rest, _ := read("&offset=100")
	if len(rest.Items) != 33 || rest.HasMore {
		t.Fatal(len(rest.Items), rest.HasMore)
	}
	names := map[string]int{}
	for _, p := range []page{first, rest} {
		for _, item := range p.Items {
			names[filepath.Base(item.Path)]++
			if strings.Contains(item.Path, "escaped") || strings.Contains(item.Path, "broken") || strings.Contains(item.Path, "pipe") || strings.Contains(item.Path, "notes") {
				t.Fatal("listed", item.Path)
			}
		}
	}
	// The link resolves to its target, as through the mount, so the target shows twice.
	if names["target.mkv"] != 2 || names["inside.mkv"] != 0 || len(names) != 132 {
		t.Fatal(names["target.mkv"], names["inside.mkv"], len(names))
	}
	if len(meter.listed) != 2 {
		t.Fatal("helper asked", meter.listed)
	}

	// Sizes order the whole folder from the helper's sizes, without a stat of each entry.
	bySize, _ := read("&sort=size_desc")
	largest := ""
	for _, item := range bySize.Items {
		if !item.IsDir {
			largest = filepath.Base(item.Path)
			break
		}
	}
	if bySize.Total != 133 || largest != "film129.mkv" {
		t.Fatal(bySize.Total, largest)
	}
}

// A helper that cannot answer (older release, unreachable, too many entries)
// leaves the listing to the mount, with the same result.
func TestFolderListingFallsBackToTheMountWhenTheHelperCannotList(t *testing.T) {
	s, h := testUI(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.mkv"), []byte("abc"), 0600)
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	canonical := deps.Fs.ReadRoots()[0]
	meter := &fakeMeter{prefix: filepath.Dir(canonical)}
	s.nas = meter
	w := request(h, "GET", "/api/maintenance/folder?path="+url.QueryEscape(canonical), "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"size":3`) || !strings.Contains(w.Header().Get("Server-Timing"), `list;dur=0;desc="mount"`) {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	if len(meter.listed) != 1 {
		t.Fatal(meter.listed)
	}
}
