// Package fsop exposes a deliberately narrow filesystem surface: stat,
// bounded listing, content hashing, and moves/deletes confined to an
// allowlist of roots. There is no generic shell and no arbitrary-file read:
// every path is resolved (symlinks included) and must stay inside a root.
package fsop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				abs = real
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
	real := evalExistingSymlinks(abs)
	for _, root := range roots {
		if real == root || strings.HasPrefix(real, root+string(os.PathSeparator)) {
			return real, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed roots", path)
}

func evalExistingSymlinks(p string) string {
	real, err := filepath.EvalSymlinks(p)
	if err == nil {
		return filepath.Clean(real)
	}
	dir, file := filepath.Split(filepath.Clean(p))
	if dir == p || dir == "" || dir == "." || dir == string(filepath.Separator) {
		return filepath.Clean(p)
	}
	return filepath.Join(evalExistingSymlinks(filepath.Clean(dir)), file)
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

// renameFn is os.Rename; tests replace it to simulate a rename across
// filesystems.
var renameFn = os.Rename

// MoveResult says how a move was done, for the action log.
type MoveResult struct {
	// Method is "rename" (same filesystem) or "copy" (the rename crossed
	// filesystems, so the file was copied, verified and the source removed).
	Method string `json:"method"`
	Bytes  int64  `json:"bytes,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// CreatedDirs lists the destination folders that did not exist.
	CreatedDirs []string `json:"created_dirs,omitempty"`
}

// Move moves source to destination. Both must resolve inside the write
// roots, so files cannot be smuggled out of the media area. It never
// overwrites. Missing folders above the destination are created (inside the
// write roots, as plain directories). When the rename crosses filesystems
// (EXDEV, e.g. two separate mounts of the same share) a file is copied to a
// temporary name next to the destination, read back and compared by size and
// SHA-256, and only then put in place and removed from the source. A failure
// at any point leaves the source untouched. Directories only move within one
// filesystem.
func (r *Resolver) Move(ctx context.Context, source, destination string) (MoveResult, error) {
	var res MoveResult
	src, err := r.ResolveWrite(source)
	if err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	dst, err := r.ResolveWrite(destination)
	if err != nil {
		return res, fmt.Errorf("destination: %w", err)
	}
	if _, err := os.Stat(src); err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	if _, err := os.Lstat(dst); err == nil {
		return res, fmt.Errorf("destination %q already exists", destination)
	}
	if r.isRoot(dst) {
		return res, fmt.Errorf("destination %q is an allowed root", destination)
	}
	created, err := mkdirAllReport(filepath.Dir(dst))
	res.CreatedDirs = created
	if err != nil {
		return res, fmt.Errorf("creating destination folder: %w", err)
	}
	err = renameFn(src, dst)
	if err == nil {
		res.Method = "rename"
		return res, nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return res, err
	}
	n, sum, err := copyVerifiedMove(ctx, src, dst)
	if err != nil {
		return res, fmt.Errorf("%w (the source was not touched)", err)
	}
	res.Method, res.Bytes, res.SHA256 = "copy", n, sum
	return res, nil
}

func (r *Resolver) isRoot(p string) bool {
	for _, root := range r.writeRoots {
		if p == root {
			return true
		}
	}
	return false
}

// mkdirAllReport creates dir and its missing parents, returning the ones it
// made (outermost first).
func mkdirAllReport(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append([]string{d}, missing...)
		if filepath.Dir(d) == d {
			break
		}
	}
	var made []string
	for _, d := range missing {
		if err := os.Mkdir(d, 0o755); err != nil && !os.IsExist(err) {
			return made, err
		}
		made = append(made, d)
	}
	return made, nil
}

// copyVerifiedMove is the cross-filesystem move of one regular file.
func copyVerifiedMove(ctx context.Context, src, dst string) (int64, string, error) {
	before, err := os.Stat(src)
	if err != nil {
		return 0, "", err
	}
	if !before.Mode().IsRegular() {
		return 0, "", fmt.Errorf("%q is on another filesystem than the destination and only files can be copied across (move it in smaller steps)", filepath.Base(src))
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	tmp := fmt.Sprintf("%s.moving-%d", dst, time.Now().UnixNano())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	keep := false
	defer func() {
		out.Close()
		if !keep {
			os.Remove(tmp)
		}
	}()
	h := sha256.New()
	n, err := copyCtx(ctx, io.MultiWriter(out, h), in)
	if err != nil {
		return 0, "", err
	}
	want := hex.EncodeToString(h.Sum(nil))
	if err := out.Sync(); err != nil {
		return 0, "", fmt.Errorf("flushing the copy: %w", err)
	}
	if err := out.Close(); err != nil {
		return 0, "", fmt.Errorf("closing the copy: %w", err)
	}
	// The source must not have changed while it was read.
	after, err := os.Stat(src)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || n != before.Size() {
		return 0, "", fmt.Errorf("the source changed while it was copied")
	}
	// Read the copy back from disk and compare.
	got, gn, err := hashPath(ctx, tmp)
	if err != nil {
		return 0, "", fmt.Errorf("reading the copy back: %w", err)
	}
	if gn != n || got != want {
		return 0, "", fmt.Errorf("the copy does not match the original (%d of %d bytes)", gn, n)
	}
	_ = os.Chmod(tmp, before.Mode().Perm())
	_ = os.Chtimes(tmp, before.ModTime(), before.ModTime())
	// Put it in place without ever replacing something that appeared since.
	if err := os.Link(tmp, dst); err != nil {
		if _, serr := os.Lstat(dst); serr == nil {
			return 0, "", fmt.Errorf("destination %q already exists", dst)
		}
		// No hard links on this mount: a rename inside one folder is fine.
		if rerr := os.Rename(tmp, dst); rerr != nil {
			return 0, "", rerr
		}
		keep = true
	} else {
		os.Remove(tmp)
		keep = true
	}
	if err := os.Remove(src); err != nil {
		if rerr := os.Remove(dst); rerr != nil {
			return 0, "", fmt.Errorf("copied and verified, but the source could not be removed (%v) and the copy could not be undone (%v): both exist", err, rerr)
		}
		return 0, "", fmt.Errorf("the source could not be removed: %w", err)
	}
	return n, want, nil
}

// copyCtx copies in chunks, stopping when ctx is cancelled.
func copyCtx(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, fmt.Errorf("cancelled after %d bytes: %w", n, err)
		}
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			n += int64(nw)
			if ew != nil {
				return n, ew
			}
			if nw != nr {
				return n, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return n, nil
		}
		if er != nil {
			return n, er
		}
	}
}

func hashPath(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := copyCtx(ctx, h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
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

// Hash returns the SHA-256 of a file inside the read roots. The copy runs
// in chunks against ctx so hashing a multi-GB media file can be cancelled
// instead of wedging the caller until it finishes.
func (r *Resolver) Hash(ctx context.Context, path string) (string, int64, error) {
	real, err := r.ResolveRead(path)
	if err != nil {
		return "", 0, err
	}
	h, n, err := hashPath(ctx, real)
	if err != nil {
		return "", 0, fmt.Errorf("hash: %w", err)
	}
	return h, n, nil
}
