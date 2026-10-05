package maintenanceui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
)

func TestFolderSizeCountsAllRegularFilesWithoutFollowingLinks(t *testing.T) {
	root := t.TempDir()
	resolver, _ := fsop.NewResolver([]string{root}, nil)
	root, _ = resolver.ResolveRead(root)
	os.MkdirAll(filepath.Join(root, ".recovery"), 0700)
	os.WriteFile(filepath.Join(root, "video.mkv"), make([]byte, 11), 0600)
	os.WriteFile(filepath.Join(root, "notes.txt"), make([]byte, 7), 0600)
	os.WriteFile(filepath.Join(root, ".recovery", "old.mkv"), make([]byte, 5), 0600)
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	os.WriteFile(outside, make([]byte, 1000), 0600)
	os.Symlink(outside, filepath.Join(root, "escaped.mkv"))
	os.Symlink(filepath.Join(root, "video.mkv"), filepath.Join(root, "duplicate.mkv"))
	result := measureFolderSize(context.Background(), root, resolver, 100)
	if result.Status != "ready" || result.Bytes == nil || *result.Bytes != 23 {
		t.Fatalf("wrong total: %+v", result)
	}
	partial := measureFolderSize(context.Background(), root, resolver, 4)
	if partial.Status != "partial" || partial.Bytes == nil || *partial.Bytes >= 23 {
		t.Fatalf("truncated total presented as exact: %+v", partial)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unavailable := measureFolderSize(ctx, root, resolver, 100)
	if unavailable.Status != "unavailable" || unavailable.Bytes != nil {
		t.Fatalf("failed scan shown as empty: %+v", unavailable)
	}
	empty := filepath.Join(root, "empty")
	os.Mkdir(empty, 0700)
	result = measureFolderSize(context.Background(), empty, resolver, 100)
	if result.Status != "ready" || result.Bytes == nil || *result.Bytes != 0 {
		t.Fatal(result)
	}
}

func TestFolderSizesAreAuthenticatedBoundedAndCached(t *testing.T) {
	s, h := testUI(t)
	root := t.TempDir()
	folder := filepath.Join(root, "Season 1")
	os.Mkdir(folder, 0700)
	os.WriteFile(filepath.Join(folder, "episode.mkv"), make([]byte, 12), 0600)
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	root, _ = deps.Fs.ResolveRead(root)
	folder, _ = deps.Fs.ResolveRead(folder)
	base := "/api/maintenance/folder?path=" + url.QueryEscape(root) + "&sizes=1"
	if request(h, "GET", base, "", false).Code != 401 {
		t.Fatal("sizes are not authenticated")
	}
	if request(h, "GET", "/api/maintenance/folder?path=/outside&sizes=1", "", true).Code != 400 {
		t.Fatal("outside root allowed")
	}
	read := func() folderSize {
		w := request(h, "GET", base, "", true)
		var page struct {
			Items []folderItem `json:"items"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].FolderSize == nil {
			t.Fatal(w.Body.String())
		}
		return *page.Items[0].FolderSize
	}
	deadline := time.Now().Add(2 * time.Second)
	var value folderSize
	for value = read(); value.Status == "calculating" && time.Now().Before(deadline); value = read() {
		time.Sleep(time.Millisecond)
	}
	if value.Status != "ready" || value.Bytes == nil || *value.Bytes != 12 {
		t.Fatal(value)
	}

	// A newly constructed UI cache reads the persisted total immediately.
	c := &s.folderSizes
	cold := &folderSizeCache{}
	restored := cold.get(folder, deps.Fs, deps.Store)
	if restored.Status != "ready" || *restored.Bytes != 12 {
		t.Fatal("lost total on restart", restored)
	}
	os.WriteFile(filepath.Join(folder, "another.mkv"), make([]byte, 20), 0600)
	c.mu.Lock()
	entry := c.entries[folder]
	entry.CheckedAt = time.Now().Add(-time.Minute)
	c.entries[folder] = entry
	c.mu.Unlock()
	cached := read()
	if cached.Status != "ready" || *cached.Bytes != 12 || !cached.Updating {
		t.Fatal("refresh hid cached total", cached)
	}
	deadline = time.Now().Add(2 * time.Second)
	for value = read(); value.Updating && time.Now().Before(deadline); value = read() {
		time.Sleep(time.Millisecond)
	}
	if value.Status != "ready" || *value.Bytes != 32 {
		t.Fatal("changed directory not refreshed", value)
	}
	// Editing a file leaves the directory timestamp unchanged. Periodic metadata
	// verification must still refresh that size and the metadata fingerprint.
	c.mu.Lock()
	entry = c.entries[folder]
	before := entry.Fingerprint
	entry.CheckedAt = time.Now().Add(-time.Minute)
	entry.Size.MeasuredAt = time.Now().Add(-31 * time.Minute)
	c.entries[folder] = entry
	c.mu.Unlock()
	os.WriteFile(filepath.Join(folder, "another.mkv"), make([]byte, 30), 0600)
	deadline = time.Now().Add(2 * time.Second)
	for value = read(); value.Updating && time.Now().Before(deadline); value = read() {
		time.Sleep(time.Millisecond)
	}
	if *value.Bytes != 42 {
		t.Fatal("in-place edit missed", value)
	}
	c.mu.Lock()
	after := c.entries[folder].Fingerprint
	c.mu.Unlock()
	if before == after {
		t.Fatal("metadata fingerprint unchanged after file growth")
	}
}

func TestFolderScansFinishWithoutBrowserPollingAndCacheSurvivesStoreReopen(t *testing.T) {
	root := t.TempDir()
	resolver, _ := fsop.NewResolver([]string{root}, nil)
	root, _ = resolver.ResolveRead(root)
	db := filepath.Join(t.TempDir(), "cache.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	cache := &folderSizeCache{}
	for i := 0; i < 6; i++ {
		path := filepath.Join(root, fmt.Sprint(i))
		os.Mkdir(path, 0700)
		os.WriteFile(filepath.Join(path, "video.mkv"), make([]byte, i+1), 0600)
		cache.get(path, resolver, st)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		done := true
		cache.mu.Lock()
		for _, entry := range cache.entries {
			done = done && !entry.Size.Updating
		}
		cache.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending folders require another poll")
		}
		time.Sleep(time.Millisecond)
	}
	st.Close()
	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cold := &folderSizeCache{}
	restored := cold.get(filepath.Join(root, "5"), resolver, st)
	if restored.Status != "ready" || restored.Bytes == nil || *restored.Bytes != 6 || restored.Updating {
		t.Fatal("cache did not survive database reopen", restored)
	}
}
