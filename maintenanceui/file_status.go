package maintenanceui

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// What the Files tab says about a file Navigatorr has been asked to convert.
// Listed from the most to the least worth looking at.
const (
	fileAttention = "attention" // a decision is waiting
	fileWorking   = "working"   // converting, or being replaced
	fileQueued    = "queued"    // added, waiting for its turn
	fileConverted = "converted" // a converted copy is ready; the original is untouched
	fileReplaced  = "replaced"  // the converted file is now the file
	fileKept      = "kept"      // checked; the original stays as it is
	fileFailed    = "failed"    // the conversion did not finish; the file itself is fine
)

var fileStateRank = map[string]int{fileAttention: 7, fileWorking: 6, fileQueued: 5, fileConverted: 4, fileReplaced: 3, fileKept: 2, fileFailed: 1}

// fileStatusView is one file's state, or a folder's: the state most worth
// looking at among its files, how many files are in it, and the full tally.
type fileStatusView struct {
	State  string         `json:"state"`
	Count  int            `json:"count,omitempty"`
	Counts map[string]int `json:"counts,omitempty"`
}

func terminalActionStatus(status string) bool {
	return status == store.ActionStatusCompleted || status == store.ActionStatusFailed || status == store.ActionStatusCancelled
}

// fileStateOf reads one batch item the way the batch's file list does: the
// attempt's own outcome wins over the item's row. "" means the item says
// nothing about its file, such as a cancelled one or the leftovers of a batch
// that has ended.
func fileStateOf(row store.FileStatusRow) string {
	if row.Replaced {
		return fileReplaced
	}
	if row.ReplacementStatus != "" && !terminalActionStatus(row.ReplacementStatus) {
		return fileWorking
	}
	status := row.ItemStatus
	switch {
	case row.ChildStatus == store.ActionStatusCompleted && row.ChildSkipped:
		status = "skip"
	case terminalActionStatus(row.ChildStatus) || row.ChildStatus == store.ActionStatusWaitingDecision:
		status = row.ChildStatus
	}
	ended := terminalActionStatus(row.BatchStatus)
	switch status {
	case "queued", "waiting_for_slot", "pending":
		if !ended {
			return fileQueued
		}
	case "running", store.ActionStatusWaitingExternal:
		if !ended {
			// In the worker's line is still waiting for its turn.
			if row.ChildQueuedOnWorker {
				return fileQueued
			}
			return fileWorking
		}
	case "waiting_decision", "review":
		if !ended {
			return fileAttention
		}
	case "completed":
		if row.Decision == "transcode" && row.CandidatePath != "" {
			return fileConverted
		}
		return fileKept
	case "skip":
		return fileKept
	case "failed":
		// Turning a candidate down is a choice, not a failed conversion.
		if strings.Contains(row.Error, "rejected by user decision") {
			return fileKept
		}
		return fileFailed
	}
	return ""
}

// fileStatusIndex holds the state of every file a batch has touched, by clean
// path, with the paths sorted so a folder's files are one contiguous run.
type fileStatusIndex struct {
	paths []string
	state map[string]string
}

func buildFileStatusIndex(rows []store.FileStatusRow) *fileStatusIndex {
	index := &fileStatusIndex{state: map[string]string{}}
	for _, row := range rows {
		state := fileStateOf(row)
		if state == "" || row.FilePath == "" {
			continue
		}
		path := filepath.Clean(row.FilePath)
		if fileStateRank[state] >= fileStateRank[index.state[path]] {
			index.state[path] = state
		}
	}
	index.paths = make([]string, 0, len(index.state))
	for path := range index.state {
		index.paths = append(index.paths, path)
	}
	sort.Strings(index.paths)
	return index
}

func summarizeFileStates(counts map[string]int) *fileStatusView {
	files, best := 0, ""
	for state, n := range counts {
		files += n
		if n > 0 && fileStateRank[state] > fileStateRank[best] {
			best = state
		}
	}
	if best == "" {
		return nil
	}
	view := &fileStatusView{State: best}
	if files > 1 {
		view.Count, view.Counts = counts[best], counts
	}
	return view
}

// file is one file's state; nil when no batch has touched it.
func (x *fileStatusIndex) file(path string) *fileStatusView {
	if x == nil || path == "" {
		return nil
	}
	if state := x.state[filepath.Clean(path)]; state != "" {
		return &fileStatusView{State: state}
	}
	return nil
}

// under summarizes every file inside a folder, at any depth.
func (x *fileStatusIndex) under(dir string) *fileStatusView {
	if x == nil || dir == "" {
		return nil
	}
	prefix := strings.TrimSuffix(filepath.Clean(dir), string(filepath.Separator)) + string(filepath.Separator)
	counts := map[string]int{}
	for i := sort.SearchStrings(x.paths, prefix); i < len(x.paths) && strings.HasPrefix(x.paths[i], prefix); i++ {
		counts[x.state[x.paths[i]]]++
	}
	return summarizeFileStates(counts)
}

// among summarizes the listed files, such as one season of a series.
func (x *fileStatusIndex) among(paths []string) *fileStatusView {
	if x == nil {
		return nil
	}
	counts := map[string]int{}
	for _, path := range paths {
		if path != "" {
			if state := x.state[filepath.Clean(path)]; state != "" {
				counts[state]++
			}
		}
	}
	return summarizeFileStates(counts)
}

// fileStatusCache keeps the index so a page of folders or titles reads the
// store once however many rows it shows, and so a page does not wait for the
// read at all once there is an index to show.
//
// Reading every batch item ever run is not cheap (about 0.4 s for ten thousand
// files, with the store locked all the while), and the Files tab asks on every
// page. So:
//   - an index younger than fileStatusTTL is used as it is;
//   - an older one, up to fileStatusStale, is used as well while a single
//     refresh runs in the background, and the refresh is not started again
//     until ten times its own cost has passed, so the read never takes more
//     than a tenth of the store's time however often the tab is polled;
//   - after something is changed through the console the index is dropped and
//     the next page waits for a new one, so a retry shows at once.
type fileStatusCache struct {
	mu      sync.Mutex
	at      time.Time
	index   *fileStatusIndex
	cost    time.Duration
	gen     uint64 // moves when the console changed something
	validAt uint64 // the generation index was read in
	loading chan struct{}
}

const (
	fileStatusTTL   = 3 * time.Second
	fileStatusStale = time.Minute
	// fileStatusFreshCap keeps a slow read from making the index "fresh" for
	// longer than it may be shown stale, which would leave nothing to refresh it.
	fileStatusFreshCap = 30 * time.Second
)

// invalidate says the console changed something the index may show.
func (c *fileStatusCache) invalidate() {
	c.mu.Lock()
	c.gen++
	c.mu.Unlock()
}

// refreshLocked starts the read if none is running and returns the channel that
// closes when it ends. c.mu must be held.
func (c *fileStatusCache) refreshLocked(st *store.Store) chan struct{} {
	if c.loading != nil {
		return c.loading
	}
	done := make(chan struct{})
	c.loading = done
	gen := c.gen
	go func() {
		began := time.Now()
		rows, err := st.ListFileStatusRows()
		var index *fileStatusIndex
		if err == nil {
			index = buildFileStatusIndex(rows)
		}
		c.mu.Lock()
		if err == nil {
			c.index, c.at, c.cost, c.validAt = index, time.Now(), time.Since(began), gen
		} else if c.index != nil {
			// The store could not be read: keep showing what there is, and try again
			// after the usual interval instead of on every request.
			c.at, c.validAt = time.Now(), gen
		}
		c.loading = nil
		c.mu.Unlock()
		close(done)
	}()
	return done
}

// fileStatuses is the current index, or nil when the store cannot be read: the
// Files tab then simply shows no marks.
func (s *Server) fileStatuses() *fileStatusIndex {
	st := s.engine.Deps().Store
	if st == nil {
		return nil
	}
	c := &s.fileStatusCache
	c.mu.Lock()
	age := time.Since(c.at)
	if c.index != nil && c.validAt == c.gen {
		if age < min(max(fileStatusTTL, 10*c.cost), fileStatusFreshCap) {
			defer c.mu.Unlock()
			return c.index
		}
		if age < fileStatusStale {
			c.refreshLocked(st)
			defer c.mu.Unlock()
			return c.index
		}
	}
	// A read that began before the last change shows the state before it, so
	// wait for one that began after (a second read at most).
	for attempt := 0; ; attempt++ {
		done := c.refreshLocked(st)
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		if c.validAt == c.gen || attempt >= 1 {
			defer c.mu.Unlock()
			return c.index
		}
	}
}
