//go:build linux

package transcodeworker

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// renameNoReplace atomically renames src to dst and fails with fs.ErrExist when
// dst already exists, using renameat2(RENAME_NOREPLACE). Local filesystems and
// CIFS/SMB3 mounts on current kernels support it, so a mounted publication
// commits with one rename instead of needing hard links or a copy.
//
// A kernel or filesystem without the flag reports EINVAL/ENOSYS/ENOTSUP; that
// maps to errNoReplaceUnsupported so the caller tries the hard-link fallback.
// It never degrades to a clobbering rename.
func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EISDIR):
		return fs.ErrExist
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return errNoReplaceUnsupported
	default:
		return err
	}
}
