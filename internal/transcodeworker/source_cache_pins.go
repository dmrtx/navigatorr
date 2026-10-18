package transcodeworker

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// benchmarkPinOwner pins an entry a benchmark staged or reused, so the full
// transcode that usually follows finds it even when the cache is under size
// pressure. The job that completes for the same source releases it.
const benchmarkPinOwner = "benchmark"

// sourceCachePinDir holds one empty marker file per (entry, owner). The dot
// prefix keeps the sweep's data listing from ever counting it.
func sourceCachePinDir(cacheDir string) string { return filepath.Join(cacheDir, ".pins") }

func pinOwnerTag(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:8])
}

// sourceCacheKeyOfData returns the cache key of a data file name.
func sourceCacheKeyOfData(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// pinSourceCacheEntry protects one cache entry from the sweep on behalf of
// owner (a job ID or benchmarkPinOwner). Best effort: a pin that cannot be
// written only leaves the entry subject to ordinary eviction.
func pinSourceCacheEntry(cacheDir, cleanSource string, fi os.FileInfo, expectedSHA, owner string) {
	if strings.TrimSpace(cacheDir) == "" || fi == nil || strings.TrimSpace(owner) == "" {
		return
	}
	norm, err := normalizeExpectedSourceSHA(expectedSHA)
	if err != nil {
		return
	}
	dir := sourceCachePinDir(cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	pin := filepath.Join(dir, sourceCacheKey(cleanSource, fi, norm)+"."+pinOwnerTag(owner))
	if f, err := os.OpenFile(pin, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_ = f.Close()
	}
	now := time.Now()
	_ = os.Chtimes(pin, now, now)
}

// releaseSourceCachePins drops every pin owner holds. When the owner's job
// completed (withBenchmark), it also drops the benchmark pin of each entry it
// pinned: the source has been benchmarked and transcoded. A failed job keeps
// the benchmark pin so a manual retry still finds the entry.
func releaseSourceCachePins(cacheDir, owner string, withBenchmark bool) {
	if strings.TrimSpace(cacheDir) == "" || strings.TrimSpace(owner) == "" {
		return
	}
	dir := sourceCachePinDir(cacheDir)
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	suffix := "." + pinOwnerTag(owner)
	benchmark := "." + pinOwnerTag(benchmarkPinOwner)
	for _, e := range names {
		name := e.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		key := strings.TrimSuffix(name, suffix)
		_ = os.Remove(filepath.Join(dir, name))
		if withBenchmark {
			_ = os.Remove(filepath.Join(dir, key+benchmark))
		}
	}
}

// liveSourceCachePins returns the keys pinned within ttl and deletes expired
// pins, so an owner that never released its pin (crash, abandoned job) holds
// an entry for at most one TTL.
func liveSourceCachePins(cacheDir string, ttl time.Duration, now time.Time) map[string]bool {
	live := map[string]bool{}
	dir := sourceCachePinDir(cacheDir)
	names, err := os.ReadDir(dir)
	if err != nil {
		return live
	}
	for _, e := range names {
		full := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(full)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if ttl > 0 && now.Sub(fi.ModTime()) > ttl {
			_ = os.Remove(full)
			continue
		}
		live[sourceCacheKeyOfData(e.Name())] = true
	}
	return live
}

// effectiveSourceCacheMaxBytes keeps the configured bound but never lets it
// fall below 1.5x the largest entry, so one large source cannot be evicted by
// its own arrival and a benchmark followed by its transcode still shares it.
func effectiveSourceCacheMaxBytes(configured, largest int64) int64 {
	if floor := largest + largest/2; floor > configured {
		return floor
	}
	return configured
}

// sourceCacheFreeFraction is the share of the work disk the cache always
// leaves free.
const sourceCacheFreeFraction = 10

// sourceCacheDiskBudget reports the free bytes on the cache's filesystem and
// the floor the cache must leave free (a tenth of the volume).
func sourceCacheDiskBudget(cacheDir string) (free, reserve int64, ok bool) {
	avail, total, ok := diskSpaceFn(cacheDir)
	if !ok || total == 0 {
		return 0, 0, false
	}
	return int64(avail), int64(total / sourceCacheFreeFraction), true
}

// diskSpaceFn is diskSpace, replaceable by tests.
var diskSpaceFn = diskSpace

var sourceCacheNotes sync.Map // cacheDir|kind -> last message

// noteSourceCache logs a cache-bound decision once per change, not on every
// sweep.
func noteSourceCache(cacheDir, kind, msg string) {
	if prev, loaded := sourceCacheNotes.Swap(cacheDir+"|"+kind, msg); loaded && prev == msg {
		return
	}
	log.Print(msg)
}
