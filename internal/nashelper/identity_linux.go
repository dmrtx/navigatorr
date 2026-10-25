package nashelper

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// fsIocGetversion is FS_IOC_GETVERSION: the inode's generation number.
	fsIocGetversion = 0x80087601
	// btrfsIocInoLookup is BTRFS_IOC_INO_LOOKUP, which with objectid 256 names the
	// subvolume (tree) the file lives in. It needs no privilege.
	btrfsIocInoLookup = 0xd0009412
	btrfsSuperMagic   = 0x9123683e
	btrfsFirstFree    = 256
)

type btrfsInoLookupArgs struct {
	treeid   uint64
	objectid uint64
	name     [4080]byte
}

// identityOf reads the identity from the open descriptor, never from a path.
func identityOf(f *os.File) (Identity, error) {
	fd := int(f.Fd())
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Identity{}, err
	}
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &sfs); err != nil {
		return Identity{}, err
	}
	id := Identity{
		FSID:    fmt.Sprintf("%08x%08x", uint32(sfs.Fsid.Val[0]), uint32(sfs.Fsid.Val[1])),
		FSType:  strconv.FormatInt(int64(sfs.Type), 16),
		Ino:     st.Ino,
		Size:    st.Size,
		MtimeNs: st.Mtim.Nano(),
		CtimeNs: st.Ctim.Nano(),
	}
	// The generation tells a file from a later one that reused its inode number.
	if gen, err := unix.IoctlGetInt(fd, fsIocGetversion); err == nil {
		id.Generation = strconv.FormatUint(uint64(gen), 10)
	}
	if int64(sfs.Type) == btrfsSuperMagic {
		args := btrfsInoLookupArgs{objectid: btrfsFirstFree}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), btrfsIocInoLookup, uintptr(unsafe.Pointer(&args)))
		if errno == 0 {
			id.Subvol = strconv.FormatUint(args.treeid, 10)
		}
	}
	runtime.KeepAlive(f)
	return id, nil
}

// lowerIOPriority puts the calling thread in the idle I/O class, so a hash never
// competes with the NAS's other work. The caller must have locked its thread and
// never unlocks it: the runtime discards the thread when the goroutine ends.
func lowerIOPriority() {
	const (
		ioprioWhoProcess = 1
		ioprioClassIdle  = 3
		ioprioClassShift = 13
	)
	_, _, _ = unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassIdle<<ioprioClassShift)
}
