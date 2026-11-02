// Package fsop exposes a deliberately narrow filesystem surface: stat,
// bounded listing, content hashing, and moves/deletes confined to an
// allowlist of roots. There is no generic shell and no arbitrary-file read:
// every path is resolved (symlinks included) and must stay inside a root.
package fsop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Resolver confines operations to configured roots.
type Resolver struct {
	readRoots  []string
	writeRoots []string
	// writeMounts says, per write root, whether it was a mount point (on another
	// device than its parent) when first seen. Device numbers are reassigned
	// when a share is remounted, so only this stable property is compared: a
	// root that was a mount and no longer is has lost its mount and is now an
	// empty folder on the local disk, so nothing is created or copied into it.
	rootMu      sync.Mutex
	writeMounts map[string]bool
	// placeMu serializes the last step of a cross-filesystem move, so two
	// moves of this process cannot race for one destination name.
	placeMu sync.Mutex
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
	r := &Resolver{readRoots: rr, writeRoots: wr, writeMounts: map[string]bool{}}
	for _, root := range wr {
		r.noteRoot(root)
	}
	return r, nil
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
var renameFn = renameChecked

// renameChecked renames without replacing an existing destination, where the
// kernel can say so; elsewhere the caller's earlier check is all there is.
// A rename across filesystems fails with EXDEV either way.
func renameChecked(from, to string) error {
	if supported, err := renameNoReplace(from, to); supported {
		return err
	}
	return os.Rename(from, to)
}

// mountPointFn tells whether p sits on another device than its parent.
var mountPointFn = isMountPoint

func isMountPoint(p string) (bool, error) {
	a, err := SpaceOf(p)
	if err != nil {
		return false, err
	}
	b, err := SpaceOf(filepath.Dir(p))
	if err != nil {
		return false, err
	}
	return a.Device != b.Device, nil
}

// noteRoot records whether a root is a mount point, once it exists.
func (r *Resolver) noteRoot(root string) {
	r.rootMu.Lock()
	defer r.rootMu.Unlock()
	if _, seen := r.writeMounts[root]; seen {
		return
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return
	}
	if m, err := mountPointFn(root); err == nil {
		r.writeMounts[root] = m
	}
}

// Seams for tests: linking the copy into place, removing the source, and a
// hook that runs after the copy is read back.
var (
	linkFn          = os.Link
	removeSourceFn  = os.Remove
	afterVerifyHook func()
)

// MoveResult says how a move was done, for the action log.
type MoveResult struct {
	// Method is "rename" (same filesystem) or "copy" (the rename crossed
	// filesystems, so the file was copied, verified and the source removed).
	Method string `json:"method"`
	Bytes  int64  `json:"bytes,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// CreatedDirs lists the destination folders this move created.
	CreatedDirs []string `json:"created_dirs,omitempty"`
}

// Move moves source to destination. Both must resolve inside the write
// roots, so files cannot be smuggled out of the media area. It never
// overwrites. Missing folders above the destination are created (inside the
// write roots, as plain directories, and never in a root that has lost its
// mount). When the rename crosses filesystems (EXDEV, e.g. two separate
// mounts of the same share) a file is copied to a temporary name next to the
// destination, read back and compared by size and SHA-256, and only then put
// in place and removed from the source. A failure before the source is
// removed leaves it untouched, and the verified copy is never deleted once
// the source is gone. Directories only move within one filesystem.
// Limits: the read-back may be served from the page cache, so it proves what
// was written, not what the share stored; a process killed mid-copy leaves a
// hidden .nav-mv-* file and any folders it made.
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
	if strings.HasPrefix(dst, src+string(os.PathSeparator)) {
		return res, fmt.Errorf("cannot move %q into itself", source)
	}
	if err := r.rootsMounted(src, dst); err != nil {
		return res, err
	}
	parent := filepath.Dir(dst)
	created, err := mkdirAllReport(parent)
	if err == nil {
		// Nothing swapped in a link while the folders were made.
		var real string
		if real, err = r.ResolveWrite(parent); err == nil && real != parent {
			err = fmt.Errorf("%q now resolves to %q", parent, real)
		}
	}
	if err != nil {
		removeEmptyDirs(created)
		return res, fmt.Errorf("creating destination folder: %w", err)
	}
	fail := func(err error) (MoveResult, error) {
		removeEmptyDirs(created)
		return MoveResult{}, err
	}
	res.CreatedDirs = created
	err = renameFn(src, dst)
	if err == nil {
		res.Method = "rename"
		return res, nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return fail(err)
	}
	n, sum, err := r.copyVerifiedMove(ctx, src, dst)
	if err != nil {
		var done *copiedError
		if errors.As(err, &done) {
			// The copy is in place: keep its folders and say what happened.
			res.Method, res.Bytes, res.SHA256 = "copy_source_kept", done.bytes, done.sum
			return res, err
		}
		return fail(fmt.Errorf("%w (the source was not touched)", err))
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

// rootsMounted refuses to work when a write root holding any of paths is
// missing, or was a mount point and no longer is. A missing root is never
// created: its folders would land on the local disk.
func (r *Resolver) rootsMounted(paths ...string) error {
	for _, p := range paths {
		for _, root := range r.writeRoots {
			if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
				continue
			}
			if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
				return fmt.Errorf("allowed root %q is missing (is its share mounted?); nothing was moved", root)
			}
			r.noteRoot(root)
			r.rootMu.Lock()
			was, known := r.writeMounts[root]
			r.rootMu.Unlock()
			if known && was {
				if now, err := mountPointFn(root); err != nil || !now {
					return fmt.Errorf("allowed root %q is no longer a mounted share; nothing was moved", root)
				}
			}
		}
	}
	return nil
}

// mkdirAllReport creates dir and its missing parents, returning the ones it
// made (outermost first). Every folder must really be a directory, never a
// link that appeared meanwhile.
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
		err := os.Mkdir(d, 0o755)
		switch {
		case err == nil:
			made = append(made, d)
		case os.IsExist(err):
			// Made by someone else just now: fine only as a plain directory.
			if fi, lerr := os.Lstat(d); lerr != nil || !fi.IsDir() {
				return made, fmt.Errorf("%q is not a plain directory", d)
			}
		default:
			return made, err
		}
	}
	return made, nil
}

// removeEmptyDirs takes back folders a failed move created, innermost first.
// os.Remove refuses a folder that holds anything.
func removeEmptyDirs(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
}

// copiedError means the file was copied and verified and is in place, but the
// source could not be removed: both exist, nothing was lost.
type copiedError struct {
	msg   string
	bytes int64
	sum   string
}

func (e *copiedError) Error() string { return e.msg }

// tempName is a short hidden name next to dst, so long titles do not hit the
// file name limit.
func tempName(dst string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return filepath.Join(filepath.Dir(dst), fmt.Sprintf(".nav-mv-%d", time.Now().UnixNano()))
	}
	return filepath.Join(filepath.Dir(dst), ".nav-mv-"+hex.EncodeToString(b[:]))
}

// copyVerifiedMove is the cross-filesystem move of one regular file.
func (r *Resolver) copyVerifiedMove(ctx context.Context, src, dst string) (int64, string, error) {
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
	tmp := tempName(dst)
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
	unchanged := func() bool {
		now, err := os.Stat(src)
		return err == nil && now.Size() == before.Size() && now.ModTime().Equal(before.ModTime())
	}
	if n != before.Size() || !unchanged() {
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
	if afterVerifyHook != nil {
		afterVerifyHook()
	}
	// The read-back can take minutes on a big file: check the source again
	// before the copy is put in place.
	if !unchanged() {
		return 0, "", fmt.Errorf("the source changed while the copy was verified")
	}
	_ = os.Chmod(tmp, before.Mode().Perm())
	_ = os.Chtimes(tmp, before.ModTime(), before.ModTime())
	if err := r.place(tmp, dst); err != nil {
		return 0, "", err
	}
	keep = true
	// From here the copy is the verified one: it is never deleted again.
	both := func(why string, err error) error {
		return &copiedError{bytes: n, sum: want, msg: fmt.Sprintf("copied and verified to %q, but %s (%v): both files exist, nothing was lost", dst, why, err)}
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return n, want, both("the destination folder could not be flushed", err)
	}
	if !unchanged() {
		return n, want, both("the source changed while the copy was verified, so it was kept", errors.New("source modified"))
	}
	if err := removeSourceFn(src); err != nil {
		return n, want, both("the source could not be removed", err)
	}
	return n, want, nil
}

// place puts the verified temporary file at dst without ever replacing
// something that is there, trying a hard link, then a rename that refuses to
// replace, and only on a filesystem with neither, a checked rename.
func (r *Resolver) place(tmp, dst string) error {
	r.placeMu.Lock()
	defer r.placeMu.Unlock()
	exists := func() error {
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("destination %q already exists", dst)
		}
		return nil
	}
	if err := exists(); err != nil {
		return err
	}
	if err := linkFn(tmp, dst); err == nil {
		os.Remove(tmp)
		return nil
	}
	if err := exists(); err != nil {
		return err
	}
	supported, err := renameNoReplace(tmp, dst)
	if supported {
		return err
	}
	if err := exists(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// syncDir flushes a directory entry. Filesystems that cannot flush a
// directory answer with all kinds of errors, so only a real I/O failure or a
// full disk stops the move.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	err = d.Sync()
	if errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ENOSPC) {
		return err
	}
	return nil
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
