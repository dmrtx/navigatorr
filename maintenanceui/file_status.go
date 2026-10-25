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

// fileStatusCache keeps the index for a few seconds, so a page of folders or
// titles reads the store once however many rows it shows.
type fileStatusCache struct {
	mu    sync.Mutex
	at    time.Time
	index *fileStatusIndex
}

const fileStatusTTL = 3 * time.Second

// fileStatuses is the current index, or nil when the store cannot be read: the
// Files tab then simply shows no marks.
func (s *Server) fileStatuses() *fileStatusIndex {
	st := s.engine.Deps().Store
	if st == nil {
		return nil
	}
	c := &s.fileStatusCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.index != nil && time.Since(c.at) < fileStatusTTL {
		return c.index
	}
	rows, err := st.ListFileStatusRows()
	if err != nil {
		return nil
	}
	c.index, c.at = buildFileStatusIndex(rows), time.Now()
	return c.index
}
