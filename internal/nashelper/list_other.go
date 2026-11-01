//go:build !linux

package nashelper

import (
	"context"
	"os"
	"path/filepath"
)

// List on a development machine: plain reads by path, which do not guard
// against a link swapped in mid-listing. The helper serves the library from
// Linux only.
func (r *Roots) List(ctx context.Context, path string) (ListResponse, error) {
	dir, err := r.OpenDir(path)
	if err != nil {
		return ListResponse{}, err
	}
	dir.Close()
	self, err := os.Lstat(path)
	if err != nil {
		return ListResponse{}, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return ListResponse{}, err
	}
	res := ListResponse{DirMtimeNs: self.ModTime().UnixNano(), Entries: []ListEntry{}}
	for _, d := range entries {
		if err := ctx.Err(); err != nil {
			return ListResponse{}, err
		}
		info, err := os.Lstat(filepath.Join(path, d.Name()))
		if err != nil {
			continue
		}
		if len(res.Entries) >= MaxListEntries {
			return ListResponse{}, errTooMany
		}
		kind := EntryOther
		switch {
		case info.Mode().IsRegular():
			kind = EntryFile
		case info.IsDir():
			kind = EntryDir
		case info.Mode()&os.ModeSymlink != 0:
			kind = EntrySymlink
		}
		res.Entries = append(res.Entries, ListEntry{Name: d.Name(), Type: kind, Size: info.Size(), MtimeNs: info.ModTime().UnixNano()})
	}
	return res, nil
}
