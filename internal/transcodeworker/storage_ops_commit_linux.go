//go:build linux

package transcodeworker

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// localNoReplaceFilesystems are the statfs magic numbers of local
// filesystems whose renameat2(RENAME_NOREPLACE) is enforced atomically by
// the filesystem itself.
//
// Network filesystems are deliberately absent. On CIFS/SMB the kernel checks
// for the target with a client-side lookup and may then send the SMB rename
// with ReplaceIfExists set, so another client could create the destination in
// between and have it replaced. NFS has a similar client-side window. Those
// mounts, and anything unrecognised, keep the hard-link commit, which the
// server refuses atomically when the name exists.
var localNoReplaceFilesystems = map[uint32]bool{
	0xEF53:     true, // ext2/3/4
	0x58465342: true, // xfs
	0x9123683E: true, // btrfs
	0x01021994: true, // tmpfs
	0xF2F52010: true, // f2fs
	0x2FC12FC1: true, // zfs
	0x794C7630: true, // overlayfs
}

// renameNoReplace atomically renames src to dst and fails with fs.ErrExist when
// dst already exists, using renameat2(RENAME_NOREPLACE) on local filesystems
// only (see localNoReplaceFilesystems). Elsewhere, and on a kernel or
// filesystem without the flag, it reports errNoReplaceUnsupported so the caller
// uses the hard-link commit. It never degrades to a clobbering rename.
func renameNoReplace(src, dst string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(filepath.Dir(dst), &st); err != nil || !localNoReplaceFilesystems[uint32(st.Type)] {
		return errNoReplaceUnsupported
	}
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
