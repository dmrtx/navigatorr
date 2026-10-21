package transcodeworker

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// The worker used to run CRF sample tests. Their state lived in
// <StateDir>/bench-*/ (benchmark.json plus the extracted samples) and their
// reusable results in <LocalWorkDir>/_sample-cache. Nothing reads either now,
// so startup removes what an older build left behind. Only those two names
// are touched, only directly under the worker's own state and work dirs, and
// never anything reached through a symlink or holding a transcode job.

const (
	legacyBenchmarkDirPrefix = "bench-"
	legacySampleCacheDirName = "_sample-cache"
)

// removeLegacySampleTestState deletes the retired sample test state and logs
// one line when it removed any. It never fails startup: what cannot be
// removed stays for the next start. A sample test whose runner process is
// still alive is left to finish.
func (w *Worker) removeLegacySampleTestState() {
	if strings.TrimSpace(w.cfg.StateDir) == "" {
		return
	}
	benchDirs, kept := 0, 0
	stateDir := filepath.Clean(w.cfg.StateDir)
	if entries, err := os.ReadDir(stateDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || !strings.HasPrefix(name, legacyBenchmarkDirPrefix) {
				continue
			}
			dir := filepath.Join(stateDir, name)
			if _, err := os.Stat(filepath.Join(dir, "job.json")); err == nil {
				continue
			}
			if legacyBenchmarkRunning(filepath.Join(dir, "benchmark.json")) {
				kept++
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				log.Printf("worker could not remove retired sample test state %s: %v", name, err)
				continue
			}
			benchDirs++
		}
	}
	cacheRemoved := false
	cache := filepath.Join(w.localWorkDir(), legacySampleCacheDirName)
	if fi, err := os.Lstat(cache); err == nil && fi.IsDir() {
		if err := os.RemoveAll(cache); err != nil {
			log.Printf("worker could not remove the retired sample cache: %v", err)
		} else {
			cacheRemoved = true
		}
	}
	if benchDirs > 0 || cacheRemoved || kept > 0 {
		log.Printf("worker removed retired sample test state: %d job dirs, sample cache removed=%t, %d still running left in place", benchDirs, cacheRemoved, kept)
	}
}

// legacyBenchmarkRunning reports whether benchmark.json names a sample test
// that has not finished and whose runner process is still alive. An unreadable
// or unparsable record counts as finished.
func legacyBenchmarkRunning(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var record struct {
		Status string `json:"status"`
		PID    int    `json:"pid"`
	}
	if json.Unmarshal(data, &record) != nil {
		return false
	}
	if record.Status != "queued" && record.Status != "running" {
		return false
	}
	return IsProcessAlive(record.PID)
}
