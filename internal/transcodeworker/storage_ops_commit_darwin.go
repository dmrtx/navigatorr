//go:build darwin

package transcodeworker

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// renameNoReplace atomically renames src to dst and fails with fs.ErrExist when
// dst already exists. It uses Darwin's renamex_np(RENAME_EXCL), which is a true
// rename-no-replace performed by the VFS and therefore works on filesystems
// such as SMB/NAS mounts where hard links may not be supported.
//
// If the filesystem does not support an exclusive rename the error is mapped to
// errNoReplaceUnsupported so the caller can try the hard-link fallback; the
// call never degrades to a clobbering rename.
func renameNoReplace(src, dst string) error {
	err := unix.RenamexNp(src, dst, unix.RENAME_EXCL)
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
