//go:build !linux

package nashelper

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
)

// Roots on a platform without openat2. It exists so the package builds and its
// tests run on a development machine; it checks the path before opening it and
// so is not safe against a concurrent change. The helper serves the library from
// Linux only.
type Roots struct {
	paths  []string
	closed atomic.Bool
}

// OpenRoots records the served roots.
func OpenRoots(paths []string) (*Roots, error) {
	r := &Roots{}
	for _, p := range paths {
		clean, err := filepath.EvalSymlinks(filepath.Clean(p))
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(clean) {
			return nil, errors.New("root is not an absolute path")
		}
		r.paths = append(r.paths, clean)
	}
	if len(r.paths) == 0 {
		return nil, errors.New("no roots")
	}
	return r, nil
}

// Resolver names the development fallback.
func (r *Roots) Resolver() string { return "dev" }

// Close refuses later requests.
func (r *Roots) Close() { r.closed.Store(true) }

// Paths lists the served roots.
func (r *Roots) Paths() []string { return append([]string(nil), r.paths...) }

// Open opens a regular file under a root for reading.
func (r *Roots) Open(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, errOutside
	}
	if r.closed.Load() {
		return nil, errClosed
	}
	for _, root := range r.paths {
		if clean == root {
			return nil, errNotFile
		}
		if !strings.HasPrefix(clean, strings.TrimSuffix(root, "/")+"/") {
			continue
		}
		resolved, err := filepath.EvalSymlinks(clean)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
				return nil, errNotFound
			}
			return nil, err
		}
		if resolved != clean {
			return nil, errOutside
		}
		f, err := os.Open(clean)
		if err != nil {
			if os.IsPermission(err) {
				return nil, errDenied
			}
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if !info.Mode().IsRegular() {
			f.Close()
			return nil, errNotFile
		}
		return f, nil
	}
	return nil, errOutside
}

// forceWalk does nothing on a development machine.
func (r *Roots) forceWalk() {}

var errClosed = &Error{Code: CodeInternal, Message: "the helper is shutting down"}
