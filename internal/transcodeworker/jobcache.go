package transcodeworker

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Job record index.
//
// The server polls the worker every few seconds (hold, suspend, job status)
// and the scheduler ticks every two seconds. Each of those used to list the
// state directory and read and decode every job.json in it, so an idle worker
// with a long history spent a core fraction decoding records that had not
// changed in weeks.
//
// Two read-through caches remove the decoding, and neither can serve something
// the disk no longer says:
//
//   - The record index keeps the decoded JobRecord of a job.json with the
//     file's modification time and size. Every read stats the file (cheap) and
//     decodes it again whenever either changed, a file that vanished leaves the
//     index, and every write made through SaveJobAtomic drops its entry. There
//     is no time-based shortcut: a record another process rewrote is seen by
//     the next read.
//
//   - The directory index keeps the listing of the state directory with its
//     modification time. Creating, renaming or removing a job directory moves
//     that time, so a new, hidden or deleted job shows at the next read.
//
// Both refuse to remember anything modified within jobCacheRacyWindow: a file
// rewritten twice inside one timestamp tick would otherwise look unchanged.
// A record is also cached only if its file looked the same before and after it
// was decoded, so a write racing the decode is never installed.
//
// peekJob hands out the cached record itself, not a copy, so a scan over
// every job allocates nothing per record. The caller must treat it as
// read-only, fields and everything its slices and pointers refer to; code that
// goes on to change and save a record loads it with LoadJob.
const (
	jobCacheRacyWindow = 2 * time.Second
	jobCacheMaxEntries = 1 << 14
)

type fileSig struct {
	mtime int64
	size  int64
}

func sigOf(fi os.FileInfo) fileSig { return fileSig{mtime: fi.ModTime().UnixNano(), size: fi.Size()} }

type jobCacheEntry struct {
	sig fileSig
	job *JobRecord
}

var jobIndex = struct {
	sync.Mutex
	m map[string]*jobCacheEntry
}{m: map[string]*jobCacheEntry{}}

// indexKey is the one spelling an index entry is stored and looked up under.
func indexKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

func dropJobIndex(path string) {
	k := indexKey(path)
	jobIndex.Lock()
	delete(jobIndex.m, k)
	jobIndex.Unlock()
}

// invalidateJobIndexDir forgets every cached record under dir.
func invalidateJobIndexDir(dir string) {
	prefix := indexKey(dir) + string(filepath.Separator)
	jobIndex.Lock()
	for p := range jobIndex.m {
		if strings.HasPrefix(p, prefix) {
			delete(jobIndex.m, p)
		}
	}
	jobIndex.Unlock()
}

// peekJob is LoadJob through the record index; see the notes above for what the
// caller may do with the result. Errors match LoadJob's: a missing file still
// satisfies errors.Is(err, fs.ErrNotExist).
func peekJob(path string) (*JobRecord, error) {
	key := indexKey(path)
	fi, err := os.Stat(path)
	if err != nil {
		dropJobIndex(path)
		return nil, fmt.Errorf("reading job file %s: %w", path, err)
	}
	sig := sigOf(fi)
	jobIndex.Lock()
	e := jobIndex.m[key]
	jobIndex.Unlock()
	if e != nil && e.sig == sig {
		return e.job, nil
	}
	job, err := LoadJob(path)
	if err != nil {
		dropJobIndex(path)
		return nil, err
	}
	after, aerr := os.Stat(path)
	if aerr != nil || sigOf(after) != sig || time.Since(fi.ModTime()) <= jobCacheRacyWindow {
		dropJobIndex(path)
		return job, nil
	}
	jobIndex.Lock()
	if len(jobIndex.m) >= jobCacheMaxEntries {
		jobIndex.m = map[string]*jobCacheEntry{}
	}
	jobIndex.m[key] = &jobCacheEntry{sig: sig, job: job}
	jobIndex.Unlock()
	return job, nil
}

type dirListing struct {
	mtime   int64
	entries []fs.DirEntry
}

var dirIndex = struct {
	sync.Mutex
	m map[string]*dirListing
}{m: map[string]*dirListing{}}

// readJobDirs lists the state directory through the directory index. The
// returned slice is shared and must not be modified. A listing is refreshed
// whenever the directory's modification time moved, and when the listing
// changed, cached records of jobs that left it are forgotten.
func (w *Worker) readJobDirs() ([]fs.DirEntry, error) {
	dir := w.cfg.StateDir
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	mtime := fi.ModTime().UnixNano()
	dirIndex.Lock()
	l := dirIndex.m[dir]
	if l != nil && l.mtime == mtime {
		entries := l.entries
		dirIndex.Unlock()
		return entries, nil
	}
	dirIndex.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	// A listing taken while the directory was being modified is not kept.
	if time.Since(fi.ModTime()) <= jobCacheRacyWindow {
		dirIndex.Lock()
		delete(dirIndex.m, dir)
		dirIndex.Unlock()
		return entries, nil
	}
	if l != nil {
		forgetDepartedJobs(dir, entries)
	}
	dirIndex.Lock()
	dirIndex.m[dir] = &dirListing{mtime: mtime, entries: entries}
	dirIndex.Unlock()
	return entries, nil
}

// forgetDepartedJobs drops cached records of directories no longer listed.
func forgetDepartedJobs(dir string, entries []fs.DirEntry) {
	present := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		present[e.Name()] = struct{}{}
	}
	prefix := indexKey(dir) + string(filepath.Separator)
	jobIndex.Lock()
	defer jobIndex.Unlock()
	for p := range jobIndex.m {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, string(filepath.Separator))
		if _, ok := present[name]; !ok {
			delete(jobIndex.m, p)
		}
	}
}
