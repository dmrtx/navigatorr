package nashelper

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// maxUsageDepth stops a pathological tree, or one being rearranged under the
// walk, from using every descriptor.
const maxUsageDepth = 64

// Usage sums the apparent size of the regular files under path. Every directory
// is opened relative to its parent with O_NOFOLLOW, so a link swapped in while
// the walk runs is skipped, never followed out of the root, and a directory on
// another filesystem is left alone. tick receives the bytes counted so far.
//
// childDepth asks for the size of every folder up to that many levels below path
// to be reported as well (see UsageChild); 0 asks for none.
func (r *Roots) Usage(ctx context.Context, path string, tick func(bytes int64), childDepth int) (UsageResult, error) {
	dir, err := r.OpenDir(path)
	if err != nil {
		return UsageResult{}, err
	}
	defer dir.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &st); err != nil {
		return UsageResult{}, err
	}
	var res UsageResult
	col := newUsageCollector(childDepth)
	col.enter(".", int64(st.Mtim.Sec), &res)
	if err := walkUsage(ctx, dir, uint64(st.Dev), &res, tick, col, 0, "."); err != nil {
		return UsageResult{}, err
	}
	col.finish(&res)
	return res, nil
}

// walkUsage stamps each directory from the stat taken before its entries are
// read, so a change made during the walk shows up as newer than the stamp.
func walkUsage(ctx context.Context, dir *os.File, dev uint64, res *UsageResult, tick func(int64), col *usageCollector, depth int, rel string) error {
	if depth > maxUsageDepth {
		res.Skipped++
		return nil
	}
	for {
		names, err := dir.Readdirnames(512)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			var st unix.Stat_t
			if serr := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
				// An entry that vanished since it was listed is not a loss.
				if !errors.Is(serr, unix.ENOENT) {
					res.Skipped++
				}
				continue
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				res.Files++
				res.Bytes += st.Size
				if tick != nil && res.Files%256 == 0 {
					tick(res.Bytes)
				}
			case unix.S_IFDIR:
				if uint64(st.Dev) != dev {
					res.Skipped++
					continue
				}
				fd, oerr := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if oerr != nil {
					if !errors.Is(oerr, unix.ENOENT) {
						res.Skipped++
					}
					continue
				}
				child := os.NewFile(uintptr(fd), name)
				res.Dirs++
				childRel := name
				if rel != "." {
					childRel = rel + "/" + name
				}
				mark := col.enter(childRel, int64(st.Mtim.Sec), res)
				werr := walkUsage(ctx, child, dev, res, tick, col, depth+1, childRel)
				child.Close()
				if werr != nil {
					return werr
				}
				col.leave(mark, childRel, depth+1, res)
			default:
				// Links, sockets and devices hold no library bytes.
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if tick != nil {
		tick(res.Bytes)
	}
	return nil
}
