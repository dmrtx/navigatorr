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
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/internal/nashelper"
	"github.com/jakenesler/navigatorr/store"
)

type folderItem struct {
	fsop.Stat
	FolderSize *folderSize `json:"folder_size,omitempty"`
	// Status is what a conversion batch has done with the file, or with the
	// files inside a folder.
	Status *fileStatusView `json:"nav_status,omitempty"`
}
type folderSize struct {
	Status     string    `json:"status"`
	Bytes      *int64    `json:"bytes,omitempty"`
	MeasuredAt time.Time `json:"measured_at,omitempty"`
	Note       string    `json:"note,omitempty"`
	Updating   bool      `json:"updating,omitempty"`
	// Queued says the measurement has not started: it waits for a scan slot or
	// for the NAS helper to get to it.
	Queued bool `json:"queued,omitempty"`
}
type folderSnapshot struct {
	Size folderSize `json:"size"`
	// Source is "helper" when the NAS measured the folder from inside. Its
	// directory stamps then hold whole-second modification times ("s:<unix>"),
	// and a helper that sends none leaves nothing to compare against.
	Source string `json:"source,omitempty"`
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
//
// Only folderSizeSlots measurements run at once. The rest wait. A folder never
// measured goes before one that only needs checking again, because the one has
// nothing to show; among equals the next slot goes to the folder asked about
// most recently, because that is the one on screen, and folders from a page the
// user has left wait behind it.
type folderSizeCache struct {
	mu      sync.Mutex
	entries map[string]folderSnapshot
	active  int
	waiting map[string]*sizeTicket
}
type sizeTicket struct {
	wanted time.Time
	// fresh is a folder with no size to show yet.
	fresh bool
	start chan struct{}
}

const (
	folderSizeSlots = 6
	// folderRunBudget bounds one walk once it runs. Time spent queued is not
	// counted, or a busy helper would make every folder look unmeasurable.
	folderRunBudget = 2 * time.Minute
	// folderQueueBudget bounds the wait for a slot and the helper's queue together
	// with the run.
	folderQueueBudget = 10 * time.Minute
)

// dispatchLocked starts waiting measurements while a slot is free, newest ask first.
func (c *folderSizeCache) dispatchLocked() {
	for c.active < folderSizeSlots && len(c.waiting) > 0 {
		pick := ""
		for path, ticket := range c.waiting {
			best := c.waiting[pick]
			if pick == "" || (ticket.fresh && !best.fresh) || (ticket.fresh == best.fresh && ticket.wanted.After(best.wanted)) {
				pick = path
			}
		}
		ticket := c.waiting[pick]
		delete(c.waiting, pick)
		c.active++
		if entry, ok := c.entries[pick]; ok {
			entry.Size.Queued = false
			c.entries[pick] = entry
		}
		close(ticket.start)
	}
}

func (c *folderSizeCache) get(path string, resolver *fsop.Resolver, st *store.Store, meter nasMeter) folderSize {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]folderSnapshot{}
	}
	if c.waiting == nil {
		c.waiting = map[string]*sizeTicket{}
	}
	if ticket := c.waiting[path]; ticket != nil {
		ticket.wanted = time.Now()
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
	entry.Size.Queued = true
	c.entries[path] = entry
	ticket := &sizeTicket{wanted: time.Now(), fresh: !exists, start: make(chan struct{})}
	c.waiting[path] = ticket
	c.dispatchLocked()
	go func(previous folderSnapshot) {
		<-ticket.start
		defer func() {
			c.mu.Lock()
			c.active--
			c.dispatchLocked()
			c.mu.Unlock()
		}()
		// Through the helper the walk has its own run budget and this bounds the
		// queue as well; over the mount there is no queue, only the walk.
		budget := folderRunBudget
		if meter != nil {
			if _, mapped := meter.Remote(path); mapped {
				budget = folderQueueBudget
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		onState := func(state string) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if current, ok := c.entries[path]; ok && current.Size.Updating {
				current.Size.Queued = state == nashelper.JobQueued
				c.entries[path] = current
			}
		}
		value := previous
		stamped := previous.Source != "helper" || len(previous.Directories) > 0
		if previous.Size.Status != "ready" || time.Since(previous.Size.MeasuredAt) >= helperRemeasure(previous, 30*time.Minute) || (stamped && folderDirectoriesChanged(ctx, path, previous.Directories, resolver)) {
			began := time.Now().UTC()
			value = measureFolderSnapshot(ctx, path, resolver, 100000, meter, measureHooks{
				state:    onState,
				children: func(res nashelper.UsageResult) { c.adopt(st, path, began, res) },
			})
			if value.Size.Status != "ready" && previous.Size.Status == "ready" {
				value = previous
				value.Size.Note = "Could not refresh the size; showing the last complete measurement."
			}
		}
		value.Size.Updating = false
		value.Size.Queued = false
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

// secondsStamp is the stamp for a directory the NAS helper measured: its
// modification time in whole seconds, which the helper and the mount agree on
// where the nanoseconds, sizes and modes they report may not.
func secondsStamp(info fs.FileInfo) string { return fmt.Sprintf("s:%d", info.ModTime().Unix()) }
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
		if err != nil || !info.IsDir() {
			return true
		}
		if strings.HasPrefix(stamp, "s:") {
			if secondsStamp(info) != stamp {
				return true
			}
		} else if folderStamp(info) != stamp {
			return true
		}
	}
	return false
}
func withinFolder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && len(rel) > 0 && (len(rel) < 3 || rel[:3] != ".."+string(os.PathSeparator))
}

// helperRemeasure is how long a size measured by the NAS helper is trusted. A
// helper that sent no directory stamps leaves nothing to detect a change with,
// but its walk is cheap, so it is simply repeated sooner.
func helperRemeasure(previous folderSnapshot, otherwise time.Duration) time.Duration {
	if previous.Source == "helper" && len(previous.Directories) == 0 {
		return 10 * time.Minute
	}
	return otherwise
}
func measureFolderSize(ctx context.Context, path string, resolver *fsop.Resolver, limit int) folderSize {
	return measureFolderSnapshot(ctx, path, resolver, limit, nil, measureHooks{}).Size
}

// measureHooks lets a caller follow a measurement.
type measureHooks struct {
	// state is told whether the helper has the walk queued or running.
	state func(string)
	// children receives the helper's answer, which also sizes the folders inside.
	children func(nashelper.UsageResult)
}

// helperSnapshot is the cached form of a size the NAS helper measured, with the
// modification times it saw to tell later whether anything changed.
func helperSnapshot(path string, bytes, files, dirs, skipped int64, stamps map[string]int64, now time.Time) folderSnapshot {
	snap := folderSnapshot{Size: folderSize{Status: "ready", Bytes: &bytes, MeasuredAt: now}, Source: "helper", CheckedAt: now,
		Fingerprint: fmt.Sprintf("helper:%d:%d:%d", bytes, files, dirs)}
	if len(stamps) > 0 {
		snap.Directories = make(map[string]string, len(stamps))
		for rel, sec := range stamps {
			snap.Directories[filepath.Join(path, filepath.FromSlash(rel))] = fmt.Sprintf("s:%d", sec)
		}
	}
	if skipped > 0 {
		snap.Size.Note = fmt.Sprintf("%d entries could not be read; the size may be a little low.", skipped)
	}
	return snap
}

// adopt takes the sizes of the folders inside root from a walk of root, so
// opening one of them later finds its size already measured. A folder whose own
// measurement is under way is left to it, and so is one measured after the walk
// began, which knows more.
func (c *folderSizeCache) adopt(st *store.Store, root string, began time.Time, res nashelper.UsageResult) {
	if len(res.Children) == 0 {
		return
	}
	now := time.Now().UTC()
	saved := make(map[string]string, len(res.Children))
	c.mu.Lock()
	for _, child := range res.Children {
		path := filepath.Join(root, filepath.FromSlash(child.Path))
		if path == root || !withinFolder(root, path) {
			continue
		}
		if current, ok := c.entries[path]; ok && (current.Size.Updating || current.Size.MeasuredAt.After(began)) {
			continue
		}
		snap := helperSnapshot(path, child.Bytes, child.Files, child.Dirs, child.Skipped, child.Stamps, now)
		if _, ok := c.entries[path]; ok {
			c.entries[path] = snap
		}
		if raw, err := json.Marshal(snap); err == nil {
			saved[path] = string(raw)
		}
	}
	c.mu.Unlock()
	if st != nil {
		_ = st.SaveFolderSizeSnapshots(saved)
	}
}

// measureFolderSnapshot sizes a folder, by the NAS helper when one covers it and
// answers, and by walking the share otherwise.
func measureFolderSnapshot(ctx context.Context, path string, resolver *fsop.Resolver, limit int, meter nasMeter, hooks measureHooks) folderSnapshot {
	if meter != nil {
		if _, mapped := meter.Remote(path); mapped {
			opts := []nashelper.UsageOption{nashelper.UsageInteractive(), nashelper.UsageRunBudget(folderRunBudget), nashelper.UsageChildren(2)}
			if hooks.state != nil {
				opts = append(opts, nashelper.UsageOnState(hooks.state))
			}
			if res, err := meter.Usage(ctx, path, nil, opts...); err == nil {
				snap := helperSnapshot(path, res.Bytes, res.Files, res.Dirs, res.Skipped, res.Stamps, time.Now().UTC())
				if hooks.children != nil {
					hooks.children(res)
				}
				return snap
			} else if !nashelper.ShouldFallBack(err) {
				return folderSnapshot{Size: folderSize{Status: "unavailable", Note: "Folder size could not be measured."}, CheckedAt: time.Now().UTC()}
			}
		}
	}
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
