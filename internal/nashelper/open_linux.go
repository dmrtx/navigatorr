package nashelper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Roots is the set of directories the helper serves. Files are opened relative
// to a descriptor of their root with the kernel refusing any path that leaves it
// or crosses a symbolic link, so there is no check-then-open race to win.
type Roots struct {
	paths []string
	fds   []int
}

// OpenRoots opens each root directory. A root is resolved once, here.
func OpenRoots(paths []string) (*Roots, error) {
	r := &Roots{}
	for _, p := range paths {
		clean := filepath.Clean(p)
		if !filepath.IsAbs(clean) {
			r.Close()
			return nil, fmt.Errorf("root %q is not an absolute path", p)
		}
		fd, err := unix.Open(clean, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("open root %q: %w", p, err)
		}
		r.paths = append(r.paths, clean)
		r.fds = append(r.fds, fd)
	}
	if len(r.fds) == 0 {
		return nil, errors.New("no roots")
	}
	return r, nil
}

// Close releases the root descriptors.
func (r *Roots) Close() {
	for _, fd := range r.fds {
		_ = unix.Close(fd)
	}
	r.fds = nil
}

// Paths lists the served roots.
func (r *Roots) Paths() []string { return append([]string(nil), r.paths...) }

// Open opens a regular file under a root for reading.
func (r *Roots) Open(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, errOutside
	}
	for i, root := range r.paths {
		if clean == root {
			return nil, errNotFile
		}
		prefix := strings.TrimSuffix(root, "/") + "/"
		if !strings.HasPrefix(clean, prefix) {
			continue
		}
		rel := strings.TrimPrefix(clean, prefix)
		how := &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOFOLLOW,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
		}
		var fd int
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			fd, err = unix.Openat2(r.fds[i], rel, how)
			// EAGAIN: the kernel could not rule out a race with a rename; EINTR.
			if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if err != nil {
			switch {
			case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR):
				return nil, errNotFound
			case errors.Is(err, unix.EXDEV), errors.Is(err, unix.ELOOP):
				return nil, errOutside
			case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
				return nil, errDenied
			}
			return nil, &os.PathError{Op: "openat2", Path: rel, Err: err}
		}
		f := os.NewFile(uintptr(fd), clean)
		var st syscall.Stat_t
		if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
			f.Close()
			return nil, err
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
			f.Close()
			return nil, errNotFile
		}
		return f, nil
	}
	return nil, errOutside
}
