//go:build !linux

package nashelper

import (
	"context"
	"os"
	"path/filepath"
)

// Usage on a development machine: a plain walk, which does not guard against
// a link swapped in mid-walk. The helper serves the library from Linux only.
func (r *Roots) Usage(ctx context.Context, path string, tick func(bytes int64), childDepth int) (UsageResult, error) {
	dir, err := r.OpenDir(path)
	if err != nil {
		return UsageResult{}, err
	}
	dir.Close()
	var res UsageResult
	col := newUsageCollector(childDepth)
	if info, ierr := os.Lstat(path); ierr == nil {
		col.enter(".", info.ModTime().Unix(), &res)
	}
	if err := walkPlain(ctx, path, &res, col, 0, "."); err != nil {
		return UsageResult{}, err
	}
	col.finish(&res)
	if tick != nil {
		tick(res.Bytes)
	}
	return res, nil
}

func walkPlain(ctx context.Context, dir string, res *UsageResult, col *usageCollector, depth int, rel string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		res.Skipped++
		return nil
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case e.IsDir():
			info, ierr := e.Info()
			if ierr != nil {
				res.Skipped++
				continue
			}
			res.Dirs++
			childRel := e.Name()
			if rel != "." {
				childRel = rel + "/" + e.Name()
			}
			mark := col.enter(childRel, info.ModTime().Unix(), res)
			if err := walkPlain(ctx, filepath.Join(dir, e.Name()), res, col, depth+1, childRel); err != nil {
				return err
			}
			col.leave(mark, childRel, depth+1, res)
		case e.Type().IsRegular():
			if info, ierr := e.Info(); ierr == nil {
				res.Files++
				res.Bytes += info.Size()
			}
		}
	}
	return nil
}
