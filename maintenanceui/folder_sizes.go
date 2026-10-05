package maintenanceui

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
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
}

// Size walks are optional and never block navigation. Only two walks may run
// at once; pending visible folders are admitted on the next browser poll.
type folderSizeCache struct {
	mu      sync.Mutex
	entries map[string]folderSize
	slots   chan struct{}
}

func (c *folderSizeCache) get(path string, resolver *fsop.Resolver) folderSize {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]folderSize{}
		c.slots = make(chan struct{}, 2)
	}
	if entry, ok := c.entries[path]; ok && (entry.Status == "calculating" || time.Since(entry.MeasuredAt) < 5*time.Minute) {
		return entry
	}
	pending := folderSize{Status: "calculating"}
	select {
	case c.slots <- struct{}{}:
	default:
		return pending
	}
	if len(c.entries) >= 256 {
		oldest := ""
		for key, entry := range c.entries {
			if entry.Status != "calculating" && (oldest == "" || entry.MeasuredAt.Before(c.entries[oldest].MeasuredAt)) {
				oldest = key
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[path] = pending
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		value := measureFolderSize(ctx, path, resolver, 100000)
		c.mu.Lock()
		c.entries[path] = value
		c.mu.Unlock()
		<-c.slots
	}()
	return pending
}

func measureFolderSize(ctx context.Context, path string, resolver *fsop.Resolver, limit int) folderSize {
	var total int64
	visited := 0
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
		real, err := resolver.ResolveRead(p)
		if err != nil {
			return err
		}
		if real != p {
			return fmt.Errorf("folder changed during measurement")
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
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
	return result
}
