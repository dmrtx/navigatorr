package maintenanceui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
)

type folderItem struct {
	fsop.Stat
	FolderSize *folderSize `json:"folder_size,omitempty"`
}
type folderSize struct {
	Status     string    `json:"status"`
	Bytes      *int64    `json:"bytes,omitempty"`
	MeasuredAt time.Time `json:"measured_at,omitempty"`
	Note       string    `json:"note,omitempty"`
	Updating   bool      `json:"updating,omitempty"`
}
type folderSnapshot struct {
	Size folderSize `json:"size"`
	// This fingerprint describes names, sizes and timestamps. It is not a
	// content hash and must never authorize a replacement.
	Fingerprint string            `json:"fingerprint"`
	Directories map[string]string `json:"directories"`
	CheckedAt   time.Time         `json:"checked_at"`
}

// Cached totals survive restarts and remain visible during refresh. Background
// checks detect directory changes; a periodic metadata scan also catches edits
// to existing files, which do not change their parent directory's timestamp.
// Admission and traversal are bounded independently of browser polling.
type folderSizeCache struct {
	mu      sync.Mutex
	entries map[string]folderSnapshot
	slots   chan struct{}
}

func (c *folderSizeCache) get(path string, resolver *fsop.Resolver, st *store.Store) folderSize {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]folderSnapshot{}
		c.slots = make(chan struct{}, 2)
	}
	entry, exists := c.entries[path]
	inMemory := exists
	if !exists && st != nil {
		raw, err := st.FolderSizeSnapshot(path)
		if err == nil && raw != "" && json.Unmarshal([]byte(raw), &entry) == nil && entry.Size.Status == "ready" && entry.Size.Bytes != nil {
			exists = true
		}
	}
	if !inMemory && len(c.entries) >= 256 {
		oldest := ""
		for key, cached := range c.entries {
			if !cached.Size.Updating && (oldest == "" || cached.CheckedAt.Before(c.entries[oldest].CheckedAt)) {
				oldest = key
			}
		}
		if oldest == "" {
			return folderSize{Status: "unavailable", Note: "Size scan capacity reached. Refresh to try again."}
		}
		delete(c.entries, oldest)
	}
	checkInterval := 30 * time.Second
	if entry.Size.Status != "ready" || entry.Size.Note != "" {
		checkInterval = 5 * time.Minute
	}
	if exists && (entry.Size.Updating || time.Since(entry.CheckedAt) < checkInterval) {
		c.entries[path] = entry
		return entry.Size
	}
	if !exists {
		entry.Size = folderSize{Status: "calculating"}
	}
	entry.Size.Updating = true
	c.entries[path] = entry
	go func(previous folderSnapshot) {
		c.slots <- struct{}{}
		defer func() { <-c.slots }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		value := previous
		if previous.Size.Status != "ready" || time.Since(previous.Size.MeasuredAt) >= 30*time.Minute || folderDirectoriesChanged(ctx, path, previous.Directories, resolver) {
			value = measureFolderSnapshot(ctx, path, resolver, 100000)
			if value.Size.Status != "ready" && previous.Size.Status == "ready" {
				value = previous
				value.Size.Note = "Could not refresh the size; showing the last complete measurement."
			}
		}
		value.Size.Updating = false
		value.CheckedAt = time.Now().UTC()
		if st != nil && value.Size.Status == "ready" && (previous.Fingerprint != value.Fingerprint || !previous.Size.MeasuredAt.Equal(value.Size.MeasuredAt)) {
			if raw, err := json.Marshal(value); err == nil {
				_ = st.SaveFolderSizeSnapshot(path, string(raw))
			}
		}
		c.mu.Lock()
		c.entries[path] = value
		c.mu.Unlock()
	}(entry)
	return entry.Size
}
func folderStamp(info fs.FileInfo) string {
	return fmt.Sprintf("%d:%d:%d", info.ModTime().UnixNano(), info.Size(), info.Mode())
}
func folderDirectoriesChanged(ctx context.Context, root string, dirs map[string]string, resolver *fsop.Resolver) bool {
	if len(dirs) == 0 {
		return true
	}
	for path, stamp := range dirs {
		if ctx.Err() != nil || (path != root && !withinFolder(root, path)) {
			return true
		}
		real, err := resolver.ResolveRead(path)
		if err != nil || real != path {
			return true
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || folderStamp(info) != stamp {
			return true
		}
	}
	return false
}
func withinFolder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && len(rel) > 0 && (len(rel) < 3 || rel[:3] != ".."+string(os.PathSeparator))
}
func measureFolderSize(ctx context.Context, path string, resolver *fsop.Resolver, limit int) folderSize {
	return measureFolderSnapshot(ctx, path, resolver, limit).Size
}
func measureFolderSnapshot(ctx context.Context, path string, resolver *fsop.Resolver, limit int) folderSnapshot {
	var total int64
	visited := 0
	hash := sha256.New()
	dirs := map[string]string{}
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > limit {
			return fmt.Errorf("folder scan limit reached")
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		// WalkDir does not follow links. Validate each directory's canonical path
		// rather than repeatedly resolving every ancestor of every video file.
		if d.IsDir() {
			real, err := resolver.ResolveRead(p)
			if err != nil {
				return err
			}
			if real != p {
				return fmt.Errorf("folder changed during measurement")
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\n", p, info.Size(), info.ModTime().UnixNano(), info.Mode())
		if info.IsDir() {
			dirs[p] = folderStamp(info)
		} else if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err == nil && folderDirectoriesChanged(ctx, path, dirs, resolver) {
		err = fmt.Errorf("folder changed during measurement")
	}
	result := folderSize{Status: "ready", Bytes: &total, MeasuredAt: time.Now().UTC()}
	if err != nil {
		result.Status = "partial"
		result.Note = "Incomplete scan; shown size is a lower bound. Open the folder for details."
		if total == 0 {
			result.Status = "unavailable"
			result.Bytes = nil
			result.Note = "Folder size could not be measured within the scan limits or permissions."
		}
	}
	return folderSnapshot{Size: result, Fingerprint: hex.EncodeToString(hash.Sum(nil)), Directories: dirs, CheckedAt: time.Now().UTC()}
}
