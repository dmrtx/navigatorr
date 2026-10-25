//go:build !linux

package nashelper

import (
	"context"
	"io/fs"
	"path/filepath"
)

// Usage on a development machine: a plain walk, which does not guard against
// a link swapped in mid-walk. The helper serves the library from Linux only.
func (r *Roots) Usage(ctx context.Context, path string, tick func(bytes int64)) (UsageResult, error) {
	dir, err := r.OpenDir(path)
	if err != nil {
		return UsageResult{}, err
	}
	dir.Close()
	var res UsageResult
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if werr != nil {
			res.Skipped++
			return nil
		}
		switch {
		case d.IsDir():
			if p != path {
				res.Dirs++
			}
		case d.Type().IsRegular():
			if info, ierr := d.Info(); ierr == nil {
				res.Files++
				res.Bytes += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return UsageResult{}, err
	}
	if tick != nil {
		tick(res.Bytes)
	}
	return res, nil
}
