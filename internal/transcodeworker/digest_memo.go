package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// fileFingerprint identifies one version of one local file without reading
// it. See fingerprintOf for what each platform can supply.
type fileFingerprint struct {
	dev, ino uint64
	size     int64
	modNano  int64
}

// digestMemo remembers SHA-256 digests that this job run already established
// for local files, bound to each file's strict stat fingerprint. A digest is
// established either by a full read (hashLocalFileSHA256) or by hashing bytes
// while they streamed to disk (download, staging copy).
//
// The memo lives on one job run's context, never on Worker and never on disk.
// A restart, a resumed finalization runner, a benchmark or another job starts
// empty and reads the bytes again, so every check whose purpose is to detect a
// change across a wait or a restart still does a full read.
//
// Only files under the worker's private roots (local work dir, source cache)
// are remembered. Files on shared or network storage, which other writers can
// touch with coarse timestamps, are always read in full.
type digestMemo struct {
	mu     sync.Mutex
	roots  []string
	byFile map[fileFingerprint]string
	reads  int // full local reads performed for a digest
	reused int // digests answered from a fingerprint
}

type digestMemoKey struct{}

// onLocalHashRead observes every whole-file local hash. Tests only.
var onLocalHashRead func(path string)

// withDigestMemo attaches a fresh memo scoped to the given private roots
// unless ctx already carries one.
func withDigestMemo(ctx context.Context, privateRoots ...string) context.Context {
	if digestMemoFrom(ctx) != nil {
		return ctx
	}
	m := &digestMemo{byFile: map[fileFingerprint]string{}}
	for _, r := range privateRoots {
		if r = strings.TrimSpace(r); r != "" {
			m.roots = append(m.roots, filepath.Clean(r))
		}
	}
	return context.WithValue(ctx, digestMemoKey{}, m)
}

func (m *digestMemo) private(path string) bool {
	clean := filepath.Clean(path)
	for _, r := range m.roots {
		if rel, err := filepath.Rel(r, clean); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "." {
			return true
		}
	}
	return false
}

func digestMemoFrom(ctx context.Context) *digestMemo {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(digestMemoKey{}).(*digestMemo)
	return m
}

func (m *digestMemo) remember(path string, fi os.FileInfo, sha string) {
	if m == nil || strings.TrimSpace(sha) == "" || !m.private(path) {
		return
	}
	fp, ok := fingerprintOf(fi)
	if !ok {
		return
	}
	m.mu.Lock()
	m.byFile[fp] = strings.ToLower(sha)
	m.mu.Unlock()
}

func (m *digestMemo) lookup(path string, fi os.FileInfo) (string, bool) {
	if m == nil || !m.private(path) {
		return "", false
	}
	fp, ok := fingerprintOf(fi)
	if !ok {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sha, ok := m.byFile[fp]
	if ok {
		m.reused++
	}
	return sha, ok
}

// fullReads reports how many whole-file reads this memo has seen.
func (m *digestMemo) fullReads() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads
}

func (m *digestMemo) countRead() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.reads++
	m.mu.Unlock()
}

// rememberLocalDigest records a digest computed while bytes streamed into
// path. Call it right after the file was committed at path; a link or rename
// keeps the fingerprint.
func rememberLocalDigest(ctx context.Context, path, sha string) {
	m := digestMemoFrom(ctx)
	if m == nil {
		return
	}
	if fi, err := os.Lstat(path); err == nil {
		m.remember(path, fi, sha)
	}
}

// verifiedLocalSHA256 returns the digest of path, reading the bytes only when
// this job run has not already established it for this exact file version.
func verifiedLocalSHA256(ctx context.Context, path string) (string, error) {
	if m := digestMemoFrom(ctx); m != nil {
		if fi, err := os.Lstat(path); err == nil {
			if sha, ok := m.lookup(path, fi); ok {
				return sha, nil
			}
		}
	}
	return hashLocalFileSHA256(ctx, path)
}
