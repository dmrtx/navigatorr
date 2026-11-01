package nashelper

import (
	"context"
	"errors"
	"io"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// List returns every entry of a directory with its type, size and modification
// time. Each entry is looked at relative to the directory's descriptor and
// without following links, so a link is reported as a link and nothing outside
// the root is ever opened. An entry that vanished since it was listed, or that
// cannot be looked at, is left out; a name that is not valid UTF-8 refuses the
// listing; a directory with more than MaxListEntries entries is refused.
func (r *Roots) List(ctx context.Context, path string) (ListResponse, error) {
	dir, err := r.OpenDir(path)
	if err != nil {
		return ListResponse{}, err
	}
	defer dir.Close()
	var self unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &self); err != nil {
		return ListResponse{}, err
	}
	// The stamp is taken before the entries, so a change made while they are
	// read shows up later as newer than the stamp.
	res := ListResponse{DirMtimeNs: statMtimeNs(&self), Entries: []ListEntry{}}
	for {
		names, err := dir.Readdirnames(512)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return ListResponse{}, err
			}
			var st unix.Stat_t
			if serr := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
				// An entry that vanished or cannot be looked at is left out, as the
				// mount leaves out an entry it cannot describe.
				continue
			}
			if !utf8.ValidString(name) {
				// JSON would carry it as U+FFFD, a name that does not exist on disk:
				// the caller lists this folder through the mount instead.
				return ListResponse{}, errBadName
			}
			if len(res.Entries) >= MaxListEntries {
				return ListResponse{}, errTooMany
			}
			res.Entries = append(res.Entries, ListEntry{Name: name, Type: entryType(st.Mode), Size: st.Size, MtimeNs: statMtimeNs(&st)})
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ListResponse{}, err
		}
	}
	return res, nil
}

func entryType(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return EntryFile
	case unix.S_IFDIR:
		return EntryDir
	case unix.S_IFLNK:
		return EntrySymlink
	}
	return EntryOther
}

func statMtimeNs(st *unix.Stat_t) int64 { return int64(st.Mtim.Sec)*1e9 + int64(st.Mtim.Nsec) }
