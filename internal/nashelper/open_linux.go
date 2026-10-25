package nashelper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Resolver names how paths are resolved under a root.
const (
	ResolverOpenat2 = "openat2"
	ResolverWalk    = "walk"
)

// Roots is the set of directories the helper serves. A file is opened relative to
// a descriptor of its root and never by a path the kernel could resolve through a
// symbolic link, so there is no check-then-open race to win.
//
// Where the kernel has openat2 (5.6 and later) the kernel refuses any path that
// leaves the root. Older kernels, such as the 4.4 of Synology DSM 7, have no
// openat2: there the path is walked one component at a time, each opened with
// O_NOFOLLOW relative to the previous directory descriptor and never through "..",
// which cannot leave the root either. Mount points nested inside a root are not
// detected by either; the library is served from one bind mount.
type Roots struct {
	paths    []string
	resolver string

	mu  sync.RWMutex
	fds []int
}

// OpenRoots opens each root directory and picks the resolver. A root is
// resolved once, here.
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
	r.resolver = ResolverWalk
	if openat2Works(r.fds[0]) {
		r.resolver = ResolverOpenat2
	}
	return r, nil
}

// openat2Works probes the syscall once. A kernel without it answers ENOSYS, and a
// container seccomp profile that does not know it answers EPERM.
func openat2Works(rootFd int) bool {
	fd, err := unix.Openat2(rootFd, ".", &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH,
	})
	if err != nil {
		return false
	}
	_ = unix.Close(fd)
	return true
}

// Close releases the root descriptors. A request that arrives later is refused.
func (r *Roots) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, fd := range r.fds {
		_ = unix.Close(fd)
	}
	r.fds = nil
}

// Paths lists the served roots.
func (r *Roots) Paths() []string { return append([]string(nil), r.paths...) }

// Resolver reports how paths are resolved: ResolverOpenat2 or ResolverWalk.
func (r *Roots) Resolver() string { return r.resolver }

var errClosed = &Error{Code: CodeInternal, Message: "the helper is shutting down"}

// Open opens a regular file under a root for reading.
func (r *Roots) Open(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, errOutside
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.fds == nil {
		return nil, errClosed
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
		var fd int
		var err error
		if r.resolver == ResolverOpenat2 {
			fd, err = openBeneath(r.fds[i], rel)
		} else {
			fd, err = walkBeneath(r.fds[i], rel)
		}
		if err != nil {
			switch {
			case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR):
				return nil, errNotFound
			case errors.Is(err, unix.EXDEV), errors.Is(err, unix.ELOOP):
				return nil, errOutside
			case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
				return nil, errDenied
			case errors.Is(err, unix.EAGAIN):
				return nil, &Error{Code: CodeBusy, Message: "the path is being renamed; try again"}
			}
			return nil, &os.PathError{Op: r.resolver, Path: rel, Err: err}
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

func openBeneath(rootFd int, rel string) (int, error) {
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	var fd int
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		fd, err = unix.Openat2(rootFd, rel, how)
		// EAGAIN: the kernel could not rule out a race with a rename; EINTR.
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			break
		}
	}
	return fd, err
}

// walkBeneath opens rel one component at a time. Every step is relative to the
// descriptor of the directory before it and uses O_NOFOLLOW, so a symbolic link
// anywhere on the path fails instead of being followed, and ".." is refused, so
// the walk cannot leave the root however the tree is renamed meanwhile.
func walkBeneath(rootFd int, rel string) (int, error) {
	parts := strings.Split(rel, "/")
	cur := rootFd
	owned := false
	release := func() {
		if owned {
			_ = unix.Close(cur)
		}
	}
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			release()
			return -1, unix.EXDEV
		}
		last := i == len(parts)-1
		flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if last {
			flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		}
		next, err := openatRetry(cur, part, flags)
		if err != nil {
			// A symbolic link where a directory should be opens as ENOTDIR with
			// O_DIRECTORY|O_NOFOLLOW; tell it from a missing directory.
			if !last && errors.Is(err, unix.ENOTDIR) {
				var st unix.Stat_t
				if unix.Fstatat(cur, part, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
					err = unix.ELOOP
				}
			}
			release()
			return -1, err
		}
		release()
		cur, owned = next, true
	}
	return cur, nil
}

func openatRetry(dirFd int, name string, flags int) (int, error) {
	for {
		fd, err := unix.Openat(dirFd, name, flags, 0)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}

// forceWalk makes the roots use the component walk even where openat2 exists, so
// a test on a modern kernel exercises the path the NAS takes.
func (r *Roots) forceWalk() { r.resolver = ResolverWalk }
