//go:build linux

package action

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// promotionFilesystemName is the filesystem type of dir by its statfs magic
// number, so a probe failure says what kind of mount it ran on.
func promotionFilesystemName(dir string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return ""
	}
	switch magic := int64(st.Type); magic {
	case unix.EXT4_SUPER_MAGIC:
		return "ext4"
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs"
	case unix.XFS_SUPER_MAGIC:
		return "xfs"
	case unix.TMPFS_MAGIC:
		return "tmpfs"
	case unix.OVERLAYFS_SUPER_MAGIC:
		return "overlay"
	case unix.NFS_SUPER_MAGIC:
		return "nfs"
	case unix.SMB_SUPER_MAGIC:
		return "smb"
	case unix.CIFS_SUPER_MAGIC:
		return "cifs"
	case unix.SMB2_SUPER_MAGIC:
		return "smb2"
	case unix.FUSE_SUPER_MAGIC:
		return "fuse"
	default:
		return fmt.Sprintf("magic:%#x", magic)
	}
}
