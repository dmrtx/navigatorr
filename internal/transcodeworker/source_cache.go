package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file implements a narrowly-scoped worker-local source cache that lets a
// benchmark and a subsequent full transcode for the SAME immutable source share
// a single NAS read.
//
// Design intent (correctness-first):
//   - The desired heavy-I/O path is: NAS source -> one local staged/cache copy
//     -> benchmark samples/metrics locally -> full encode locally -> full
//     validation locally -> one publish of the accepted candidate -> lightweight
//     post-publish verification -> cleanup.
//   - Cache entries are content-addressed by stable source identity: cleaned
//     absolute source path + size + modtime + the coordinator preflight SHA-256
//     of the source bytes. Any change in size, modtime, or CONTENT is a miss, so
//     a same path/size/mtime source with changed content can never be reused.
//   - Entries live under <LocalWorkDir>/_source-cache, never under NAS roots.
//     Population is atomic (temp file + no-clobber rename) and the LOCAL copied
//     bytes are hashed and compared to the expected digest before publish; a
//     mismatch is never published. No additional NAS read is ever performed.
//   - Cache files are shared and immutable once published. Active jobs never
//     read the shared filename directly: they acquire a JOB-LOCAL immutable
//     hard link (falling back to a safe local copy) so eviction may unlink the
//     shared name without breaking an in-flight benchmark or transcode. Per-job
//     cleanup removes only the job-owned link/copy; only bounded TTL/size sweeps
//     delete shared entries.
//   - StagingPolicy semantics are preserved: the cache is consulted only when
//     the source would otherwise require a NAS read (external path or SMB-direct
//     mapping). Local sources are read in place and never cached.
//   - SMB-direct/path-mapping behavior is preserved: stat/download go through
//     the same mediaStore seams as staging.
//
// Configuration lives on WorkerConfig (SourceCache* fields) with conservative
// defaults applied in normalizeOperational. Disabling the cache restores the
// pre-optimization behavior exactly.

// DefaultSourceCacheMaxBytes bounds the shared cache (20 GiB). Sweeps are
// best-effort and never fail a job.
const DefaultSourceCacheMaxBytes = 20 * 1024 * 1024 * 1024

// DefaultSourceCacheTTL bounds entry lifetime (72h). Expired entries are
// eligible for sweeping but are still validated on lookup; expiry alone never
// causes a stale hit.
const DefaultSourceCacheTTL = 72 * time.Hour

// sourceCacheDir returns the bounded local directory holding shared source
// cache entries. It is always beneath the operational local work dir.
func (w *Worker) sourceCacheDir() string {
	base := w.localWorkDir()
	if strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(filepath.Clean(base), "_source-cache")
}

// sourceCacheEnabled reports whether the shared source cache may be used.
// An explicitly disabled cache restores legacy per-job staging/downloads.
func (w *Worker) sourceCacheEnabled() bool {
	if w == nil || w.cfg == nil {
		return false
	}
	if w.cfg.DisableSourceCache {
		return false
	}
	return strings.TrimSpace(w.localWorkDir()) != ""
}

// sourceCacheMaxBytes returns the configured bound or the conservative default.
func (w *Worker) sourceCacheMaxBytes() int64 {
	if w == nil || w.cfg == nil || w.cfg.SourceCacheMaxBytes <= 0 {
		return DefaultSourceCacheMaxBytes
	}
	return w.cfg.SourceCacheMaxBytes
}

// sourceCacheTTL returns the configured TTL or the conservative default.
func (w *Worker) sourceCacheTTL() time.Duration {
	if w == nil || w.cfg == nil || w.cfg.SourceCacheTTLHours <= 0 {
		return DefaultSourceCacheTTL
	}
	return time.Duration(w.cfg.SourceCacheTTLHours) * time.Hour
}

// sourceIdentity captures the stable fingerprint used as the cache key.
type sourceIdentity struct {
	Source  string `json:"source"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time_unix_nano"`
	Mode    uint32 `json:"mode,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// sourceCacheKey derives the deterministic cache key for an identity. The
// cleaned source path binds the entry to one NAS object; size+mtime+expected
// SHA-256 bind it to one immutable CONTENT version of that object. A same
// path/size/mtime source with changed content therefore hashes to a different
// key and is a miss.
func sourceCacheKey(cleanSource string, fi os.FileInfo, expectedSHA string) string {
	mt := fi.ModTime().UTC().UnixNano()
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%s", cleanSource, fi.Size(), mt, fi.Mode().Perm(), strings.TrimSpace(expectedSHA))))
	return hex.EncodeToString(h[:])
}

// sourceCachePaths returns the deterministic data + meta paths for a key. The
// data path carries the source extension; the sidecar is always <key>.meta.json.
func sourceCachePaths(cacheDir, key, sourcePath string) (dataPath, metaPath string) {
	ext := safeFileExtension(sourcePath)
	dataPath = filepath.Join(cacheDir, key+ext)
	metaPath = filepath.Join(cacheDir, key+".meta.json")
	return dataPath, metaPath
}

// cacheMetaPathForData derives the sidecar path for a data filename in the
// cache directory. The key is the hex prefix before the first '.'; a data path
// always includes the source extension while the metadata path is
// "<key>.meta.json" with no extension. Deriving it here (instead of appending
// ".meta.json" to the full data path) is required so sweeps remove the correct
// sidecar.
func cacheMetaPathForData(cacheDir, dataName string) string {
	key := dataName
	if i := strings.IndexByte(dataName, '.'); i >= 0 {
		key = dataName[:i]
	}
	return filepath.Join(cacheDir, key+".meta.json")
}

// statSourceForCache stats a source through the same backend seams as staging
// (SMB-direct store when mapped, os.Stat otherwise) and requires a regular
// non-empty file. Failures are fail-closed and never treated as a cache hit.
func (w *Worker) statSourceForCache(ctx context.Context, cleanSource string) (os.FileInfo, error) {
	fi, err := w.statMedia(ctx, cleanSource)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", ErrSourceInvalid, cleanSource)
	}
	if fi.Size() == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrSourceInvalid, cleanSource)
	}
	return fi, nil
}

// hashLocalFileSHA256 hashes LOCAL bytes only (never a NAS path) and returns
// the lowercase hex digest. It honors cancellation between chunks.
func hashLocalFileSHA256(ctx context.Context, path string) (string, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return "", fmt.Errorf("%w: local file for hashing %s is not a regular file", ErrSourceInvalid, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: opening local file for hashing %s: %v", ErrStorageIO, path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(before, fi) {
		return "", fmt.Errorf("%w: local file for hashing %s is not a regular file", ErrSourceInvalid, path)
	}
	h := sha256.New()
	var total int64
	buf := make([]byte, 128*1024)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("%w: reading local file for hashing %s: %v", ErrStorageIO, path, rerr)
		}
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || total != before.Size() || after.Size() != total || !before.ModTime().Equal(after.ModTime()) {
		return "", fmt.Errorf("%w: local file changed while hashing %s", ErrSourceInvalid, path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyLocalDigest enforces the expected source digest against LOCAL bytes.
// An empty expected digest is a legacy no-op (size identity only); a mismatch
// fails closed and the caller must never publish the cache entry.
func verifyLocalDigest(ctx context.Context, path, expectedSHA string) error {
	norm, err := normalizeExpectedSourceSHA(expectedSHA)
	if err != nil {
		return err
	}
	if norm == "" {
		return nil
	}
	actual, err := hashLocalFileSHA256(ctx, path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(norm, actual) {
		return fmt.Errorf("%w: local source bytes do not match expected sha256 (fail closed, cache entry never published)", ErrSizeMismatch)
	}
	return nil
}

// normalizeExpectedSourceSHA trims/validates an expected digest without adding
// a cross-package dependency at the call sites.
func normalizeExpectedSourceSHA(raw string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	trimmed = strings.TrimPrefix(trimmed, "sha256:")
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) != 64 {
		return "", fmt.Errorf("%w: expected source sha256 must be 64 hex characters", ErrSourceInvalid)
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return "", fmt.Errorf("%w: expected source sha256 is not valid hex", ErrSourceInvalid)
	}
	return trimmed, nil
}

// lookupSourceCache checks for a usable cache entry without mutating anything.
// It returns ok=false on any mismatch (missing file, non-regular, size
// mismatch, meta mismatch, content mismatch, expired). Expired but otherwise
// valid entries are treated as misses so they can be repopulated; they are
// never served stale.
func lookupSourceCache(cacheDir, cleanSource string, fi os.FileInfo, expectedSHA string) (cachedPath string, ok bool) {
	if strings.TrimSpace(cacheDir) == "" || fi == nil {
		return "", false
	}
	norm, err := normalizeExpectedSourceSHA(expectedSHA)
	if err != nil {
		return "", false
	}
	key := sourceCacheKey(cleanSource, fi, norm)
	dataPath, metaPath := sourceCachePaths(cacheDir, key, cleanSource)
	ci, err := os.Lstat(dataPath)
	if err != nil || !ci.Mode().IsRegular() || ci.Size() != fi.Size() {
		return "", false
	}
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return "", false
	}
	var meta sourceIdentity
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return "", false
	}
	if meta.Source != cleanSource || meta.Size != fi.Size() ||
		meta.ModTime != fi.ModTime().UTC().UnixNano() {
		return "", false
	}
	// Content identity is mandatory when an expected digest is supplied: a
	// sidecar without it (or a different one) is never a hit.
	if norm != "" && !strings.EqualFold(meta.SHA256, norm) {
		return "", false
	}
	if norm == "" && strings.TrimSpace(meta.SHA256) != "" {
		// A content-addressed entry must not be masked by a digest-less
		// lookup; fail closed to a miss.
		return "", false
	}
	return dataPath, true
}

// writeSourceCacheMeta persists the identity sidecar next to a populated entry.
// It is best-effort for readers (lookup fails closed without it) but is
// written atomically so a crash never leaves a half-written meta.
func writeSourceCacheMeta(metaPath string, meta sourceIdentity) {
	data, err := json.Marshal(meta)
	if err != nil {
		return
	}
	dir := filepath.Dir(metaPath)
	_ = os.MkdirAll(dir, 0o755)
	tmp, err := os.CreateTemp(dir, ".cache-meta-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, _ = tmp.Write(data)
	_ = tmp.Sync()
	_ = tmp.Close()
	_ = os.Rename(tmpName, metaPath)
}

// populateSourceCacheFromLocal atomically publishes an already-local file
// (a staged input or a verified download) into the shared cache without any
// NAS read. Before publishing it hashes the LOCAL file and compares it to the
// expected source digest; a mismatch is never published. Concurrent publishers
// race safely via no-clobber commit: the loser keeps the winner after
// revalidation.
func populateSourceCacheFromLocal(ctx context.Context, cacheDir, cleanSource string, fi os.FileInfo, localPath, expectedSHA string) (string, error) {
	if strings.TrimSpace(cacheDir) == "" {
		return "", fmt.Errorf("%w: empty cache dir", ErrDestinationInvalid)
	}
	norm, err := normalizeExpectedSourceSHA(expectedSHA)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("%w: creating cache dir: %v", ErrStorageIO, err)
	}
	key := sourceCacheKey(cleanSource, fi, norm)
	dataPath, metaPath := sourceCachePaths(cacheDir, key, cleanSource)
	if hit, ok := lookupSourceCache(cacheDir, cleanSource, fi, norm); ok {
		return hit, nil
	}
	// Verify the LOCAL bytes against the expected content digest before any
	// publish. A changed source is never cached.
	if err := verifyLocalDigest(ctx, localPath, norm); err != nil {
		return "", err
	}
	// Stage through a unique temp file in the cache dir, then no-clobber
	// publish. A concurrent winner yields ErrDestinationExists, which is a
	// successful share, not a failure.
	tmp, err := os.CreateTemp(cacheDir, ".cache-stage-*")
	if err != nil {
		return "", fmt.Errorf("%w: creating cache temp: %v", ErrStorageIO, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := copyRegularFileTo(ctx, localPath, tmp, fi.Size(), 0o644); err != nil {
		return "", err
	}
	if err := commitNoReplace(ctx, tmpName, dataPath); err != nil {
		// Another job won the race: revalidate the winner before reuse.
		if hit, ok := lookupSourceCache(cacheDir, cleanSource, fi, norm); ok {
			committed = true
			_ = os.Remove(tmpName)
			return hit, nil
		}
		return "", err
	}
	committed = true
	writeSourceCacheMeta(metaPath, sourceIdentity{
		Source:  cleanSource,
		Size:    fi.Size(),
		ModTime: fi.ModTime().UTC().UnixNano(),
		Mode:    uint32(fi.Mode().Perm()),
		SHA256:  norm,
	})
	syncDir(cacheDir)
	return dataPath, nil
}

// ensureSourceCached guarantees a local cache entry for an external/SMB source
// with at most one NAS read. On a hit it returns the shared path without any
// network I/O. On a miss it downloads once (through the mediaStore seam when
// mapped, else a filesystem copy) into a temp file, hashes the LOCAL bytes
// against the expected digest, and publishes atomically. The returned path is
// shared and must never be removed by per-job cleanup.
func (w *Worker) ensureSourceCached(ctx context.Context, cleanSource, expectedSHA string) (string, error) {
	norm, err := normalizeExpectedSourceSHA(expectedSHA)
	if err != nil {
		return "", err
	}
	cacheDir := w.sourceCacheDir()
	fi, err := w.statSourceForCache(ctx, cleanSource)
	if err != nil {
		return "", err
	}
	if hit, ok := lookupSourceCache(cacheDir, cleanSource, fi, norm); ok {
		_ = w.sweepSourceCache()
		return hit, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("%w: creating cache dir: %v", ErrStorageIO, err)
	}
	key := sourceCacheKey(cleanSource, fi, norm)
	dataPath, metaPath := sourceCachePaths(cacheDir, key, cleanSource)
	tmp, err := os.CreateTemp(cacheDir, ".cache-dl-*")
	if err != nil {
		return "", fmt.Errorf("%w: creating cache temp: %v", ErrStorageIO, err)
	}
	tmpName := tmp.Name()
	// Close the empty handle; download seams open their own destination.
	_ = tmp.Close()
	_ = os.Remove(tmpName)
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
			_ = os.Remove(tmpName + ".tmp")
		}
	}()
	if w.mediaStore != nil && w.mediaStore.Maps(cleanSource) {
		if derr := w.mediaStore.DownloadAtomic(ctx, cleanSource, tmpName); derr != nil {
			return "", derr
		}
	} else {
		if serr := stageInputAtomic(ctx, cleanSource, tmpName, nil); serr != nil {
			// stageInputAtomic expects the destination to be absent and
			// publishes via temp+rename internally; it cannot write directly
			// to tmpName which we already created. Remove it and retry as a
			// plain staged copy target.
			_ = os.Remove(tmpName)
			if serr2 := StageInputAtomic(ctx, cleanSource, tmpName); serr2 != nil {
				return "", serr2
			}
		}
	}
	// Verify the download matches the fingerprinted identity AND the expected
	// content digest before publish. A source that changed mid-download is
	// never cached. Only LOCAL bytes are hashed here; no NAS read is added.
	di, err := os.Lstat(tmpName)
	if err != nil || !di.Mode().IsRegular() || di.Size() != fi.Size() {
		return "", fmt.Errorf("%w: downloaded source %s size mismatch (fail closed)", ErrSizeMismatch, cleanSource)
	}
	if err := verifyLocalDigest(ctx, tmpName, norm); err != nil {
		return "", err
	}
	if err := commitNoReplace(ctx, tmpName, dataPath); err != nil {
		if hit, ok := lookupSourceCache(cacheDir, cleanSource, fi, norm); ok {
			_ = os.Remove(tmpName)
			committed = true
			return hit, nil
		}
		return "", err
	}
	committed = true
	writeSourceCacheMeta(metaPath, sourceIdentity{
		Source:  cleanSource,
		Size:    fi.Size(),
		ModTime: fi.ModTime().UTC().UnixNano(),
		Mode:    uint32(fi.Mode().Perm()),
		SHA256:  norm,
	})
	syncDir(cacheDir)
	_ = w.sweepSourceCache()
	return dataPath, nil
}

// acquireCachedSourceLease gives the calling job a JOB-LOCAL immutable name for
// a shared cache entry without a second NAS read and without depending on the
// evictable shared filename. It first tries a hard link (same filesystem), so
// the inode survives shared-name eviction and active jobs are never broken. If
// linking is unsupported (e.g. cross-device), it falls back to a safe atomic
// local copy. The returned path is job-owned and MUST be removed by per-job
// cleanup; the shared cache entry is never touched.
func (w *Worker) acquireCachedSourceLease(ctx context.Context, cachePath, leasePath string) (string, error) {
	if strings.TrimSpace(cachePath) == "" || strings.TrimSpace(leasePath) == "" {
		return "", fmt.Errorf("%w: empty cache/lease path", ErrDestinationInvalid)
	}
	info, err := regularFileInfo(cachePath)
	if err != nil {
		return "", err
	}
	// Idempotent resume: an existing valid job-local lease is reused as-is.
	if li, lerr := os.Lstat(leasePath); lerr == nil {
		if li.Mode().IsRegular() && li.Size() == info.Size() {
			return leasePath, nil
		}
		// A stale/partial lease is replaced below; it is job-owned.
		_ = os.Remove(leasePath)
	} else if !os.IsNotExist(lerr) {
		return "", fmt.Errorf("%w: statting lease %s: %v", ErrStorageIO, leasePath, lerr)
	}
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o755); err != nil {
		return "", fmt.Errorf("%w: creating lease directory: %v", ErrStorageIO, err)
	}
	if err := os.Link(cachePath, leasePath); err == nil {
		return leasePath, nil
	}
	// Cross-device / unsupported: safe atomic local-copy fallback. This is the
	// only case that performs a (local, not NAS) full copy.
	if err := copyCacheToStaged(ctx, cachePath, leasePath); err != nil {
		return "", err
	}
	return leasePath, nil
}

// isCachePath reports whether path names a file inside this worker's shared
// source cache. Per-job cleanup uses it to avoid ever deleting shared entries.
func (w *Worker) isCachePath(path string) bool {
	dir := strings.TrimSpace(w.sourceCacheDir())
	p := strings.TrimSpace(path)
	if dir == "" || p == "" {
		return false
	}
	cleanDir := filepath.Clean(dir)
	cleanP := filepath.Clean(p)
	if cleanP == cleanDir {
		return true
	}
	rel, err := filepath.Rel(cleanDir, cleanP)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// copyCacheToStaged performs a purely local copy from the shared cache to a
// per-job path. No NAS I/O occurs here. It is the fallback when hard linking is
// unavailable.
func copyCacheToStaged(ctx context.Context, cachePath, stagedFinal string) error {
	if strings.TrimSpace(cachePath) == "" || strings.TrimSpace(stagedFinal) == "" {
		return fmt.Errorf("%w: empty cache/staged path", ErrDestinationInvalid)
	}
	info, err := regularFileInfo(cachePath)
	if err != nil {
		return err
	}
	if exists, err := pathExists(stagedFinal); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: %s", ErrDestinationExists, stagedFinal)
	}
	if err := os.MkdirAll(filepath.Dir(stagedFinal), 0o755); err != nil {
		return fmt.Errorf("%w: creating staging directory: %v", ErrStorageIO, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(stagedFinal), "."+filepath.Base(stagedFinal)+".cache-*")
	if err != nil {
		return fmt.Errorf("%w: creating staging temp: %v", ErrStorageIO, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := copyRegularFileTo(ctx, cachePath, tmp, info.Size(), info.Mode().Perm()); err != nil {
		return err
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if err := commitNoReplace(ctx, tmpPath, stagedFinal); err != nil {
		return err
	}
	committed = true
	syncDir(filepath.Dir(stagedFinal))
	return nil
}

// shouldCacheSourceForBenchmark reports whether a benchmark source should be
// served from the shared source cache. It mirrors the staging decision so
// staging_policy semantics are preserved: local sources are read in place,
// while external (NAS) and SMB-direct sources share one local copy between
// benchmark and full transcode.
func (w *Worker) shouldCacheSourceForBenchmark(cleanSource string) bool {
	if w == nil || w.cfg == nil || strings.TrimSpace(cleanSource) == "" {
		return false
	}
	if w.mediaStore != nil && w.mediaStore.Maps(cleanSource) {
		return true
	}
	policy := w.cfg.StagingPolicy
	if strings.TrimSpace(string(policy)) == "" {
		policy = DefaultStagingPolicy()
	}
	stage, err := ShouldStageInput(policy, cleanSource, w.cfg.ExternalRoots)
	if err != nil {
		return false
	}
	return stage
}

// sweepSourceCache enforces the bounded size/TTL without ever failing a job.
// It never touches temp files, meta sidecars without data, per-job work dirs,
// or entries that fail to stat. Deletion is best-effort. Active jobs hold their
// own job-local hard links, so unlinking a shared name here never breaks an
// in-flight benchmark or transcode.
func (w *Worker) sweepSourceCache() error {
	if !w.sourceCacheEnabled() {
		return nil
	}
	cacheDir := w.sourceCacheDir()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return nil // best-effort: never fail a job on sweep I/O
	}
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var datas []entry
	var total int64
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".meta.json") || strings.HasSuffix(name, ".lock") {
			continue
		}
		full := filepath.Join(cacheDir, name)
		fi, err := os.Lstat(full)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		datas = append(datas, entry{path: full, size: fi.Size(), mod: fi.ModTime()})
		total += fi.Size()
	}
	ttl := w.sourceCacheTTL()
	now := time.Now()
	// Expire by TTL first (oldest first), then enforce size bound LRU.
	for _, d := range datas {
		if ttl > 0 && now.Sub(d.mod) > ttl {
			_ = os.Remove(d.path)
			_ = os.Remove(cacheMetaPathForData(cacheDir, filepath.Base(d.path)))
			// Recompute total lazily below; keep sweeping.
		}
	}
	// Re-list sizes after TTL expiry for the size bound.
	if max := w.sourceCacheMaxBytes(); max > 0 {
		// Re-stat remaining entries oldest-first.
		entries2, err := os.ReadDir(cacheDir)
		if err != nil {
			return nil
		}
		var remaining []entry
		var tot int64
		for _, e := range entries2 {
			name := e.Name()
			if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".meta.json") || strings.HasSuffix(name, ".lock") {
				continue
			}
			full := filepath.Join(cacheDir, name)
			fi, err := os.Lstat(full)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			remaining = append(remaining, entry{path: full, size: fi.Size(), mod: fi.ModTime()})
			tot += fi.Size()
		}
		if tot > max {
			// Oldest first.
			for i := 0; i < len(remaining); i++ {
				for j := i + 1; j < len(remaining); j++ {
					if remaining[j].mod.Before(remaining[i].mod) {
						remaining[i], remaining[j] = remaining[j], remaining[i]
					}
				}
			}
			for _, d := range remaining {
				if tot <= max {
					break
				}
				// Never delete a file modified within the last hour: it may
				// belong to an active job that just populated it.
				if now.Sub(d.mod) < time.Hour {
					continue
				}
				if err := os.Remove(d.path); err == nil {
					_ = os.Remove(cacheMetaPathForData(cacheDir, filepath.Base(d.path)))
					tot -= d.size
				}
			}
		}
	}
	return nil
}
