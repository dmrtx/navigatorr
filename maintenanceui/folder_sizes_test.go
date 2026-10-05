package maintenanceui

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/fsop"
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
	os.WriteFile(filepath.Join(folder, "another.mkv"), make([]byte, 20), 0600)
	cached := read()
	if *cached.Bytes != 12 {
		t.Fatal("cache did not retain measured snapshot")
	}
	c := &s.folderSizes
	c.mu.Lock()
	entry := c.entries[folder]
	entry.MeasuredAt = time.Now().Add(-6 * time.Minute)
	c.entries[folder] = entry
	c.mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for value = read(); value.Status == "calculating" && time.Now().Before(deadline); value = read() {
		time.Sleep(time.Millisecond)
	}
	if value.Status != "ready" || value.Bytes == nil || *value.Bytes != 32 {
		t.Fatal("expired cache not refreshed", value)
	}
	c.slots <- struct{}{}
	c.slots <- struct{}{}
	if c.get(filepath.Join(root, "not-admitted"), deps.Fs).Status != "calculating" {
		t.Fatal("busy walks not bounded")
	}
	<-c.slots
	<-c.slots
}
