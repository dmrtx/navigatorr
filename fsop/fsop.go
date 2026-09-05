// Package fsop exposes a deliberately narrow filesystem surface: stat,
// bounded listing, content hashing, and moves/deletes confined to an
// allowlist of roots. There is no generic shell and no arbitrary-file read:
// every path is resolved (symlinks included) and must stay inside a root.
package fsop

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Resolver confines operations to configured roots.
type Resolver struct {
	readRoots  []string
	writeRoots []string
}

// NewResolver canonicalizes the configured roots. Empty write roots mean
// "no writes allowed".
func NewResolver(readRoots, writeRoots []string) (*Resolver, error) {
	canon := func(roots []string) ([]string, error) {
		var out []string
		for _, r := range roots {
			if r == "" {
				continue
			}
			abs, err := filepath.Abs(r)
			if err != nil {
				return nil, fmt.Errorf("resolving root %q: %w", r, err)
			}
			out = append(out, filepath.Clean(abs))
		}
		return out, nil
	}
	rr, err := canon(readRoots)
	if err != nil {
		return nil, err
	}
	wr, err := canon(writeRoots)
	if err != nil {
		return nil, err
	}
	return &Resolver{readRoots: rr, writeRoots: wr}, nil
}

// resolve checks path against roots after evaluating symlinks.
func resolve(path string, roots []string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Missing file: fall back to the lexical path so callers get a
		// clean "not inside roots" or "not found" rather than a symlink error.
		real = filepath.Clean(abs)
	} else {
		real = filepath.Clean(real)
	}
	for _, root := range roots {
		if real == root || strings.HasPrefix(real, root+string(os.PathSeparator)) {
			return real, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed roots", path)
}

// ResolveRead validates a read path.
func (r *Resolver) ResolveRead(path string) (string, error) {
	return resolve(path, r.readRoots)
}

// ResolveWrite validates a write path.
func (r *Resolver) ResolveWrite(path string) (string, error) {
	if len(r.writeRoots) == 0 {
		return "", fmt.Errorf("writes are not configured (no allowed_write_roots)")
	}
	return resolve(path, r.writeRoots)
}

// Stat describes one file.
type Stat struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	ModTime string `json:"mod_time"`
}

// FileStat stats a path inside the read roots.
func (r *Resolver) FileStat(path string) (Stat, error) {
	real, err := r.ResolveRead(path)
	if err != nil {
		return Stat{}, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return Stat{}, err
	}
	return Stat{Path: real, Size: fi.Size(), IsDir: fi.IsDir(),
		ModTime: fi.ModTime().UTC().Format(time.RFC3339)}, nil
}

// List returns directory entries, bounded. depth 1 lists the directory
// itself; higher depths recurse up to depth.
func (r *Resolver) List(path string, depth, limit int) ([]Stat, error) {
	if depth <= 0 {
		depth = 1
	}
	if depth > 4 {
		depth = 4
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	real, err := r.ResolveRead(path)
	if err != nil {
		return nil, err
	}
	var out []Stat
	var walk func(dir string, d int) error
	walk = func(dir string, d int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if len(out) >= limit {
				return nil
			}
			p := filepath.Join(dir, e.Name())
			fi, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, Stat{Path: p, Size: fi.Size(), IsDir: e.IsDir(),
				ModTime: fi.ModTime().UTC().Format(time.RFC3339)})
			if e.IsDir() && d < depth {
				_ = walk(p, d+1)
			}
		}
		return nil
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []Stat{{Path: real, Size: fi.Size(),
			ModTime: fi.ModTime().UTC().Format(time.RFC3339)}}, nil
	}
	if err := walk(real, 1); err != nil {
		return nil, err
	}
	return out, nil
}

// Move renames source to destination. Both must resolve inside the write
// roots, so files cannot be smuggled out of the media area.
func (r *Resolver) Move(source, destination string) error {
	src, err := r.ResolveWrite(source)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	dst, err := r.ResolveWrite(destination)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination %q already exists", destination)
	}
	return os.Rename(src, dst)
}

// Delete removes a file (never a directory tree) inside the write roots.
func (r *Resolver) Delete(path string) error {
	real, err := r.ResolveWrite(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("refusing to delete directory %q", path)
	}
	return os.Remove(real)
}

// Hash returns the SHA-256 of a file inside the read roots.
func (r *Resolver) Hash(path string) (string, int64, error) {
	real, err := r.ResolveRead(path)
	if err != nil {
		return "", 0, err
	}
	f, err := os.Open(real)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
