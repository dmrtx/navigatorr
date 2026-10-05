package maintenanceui

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/fsop"
)

func videoPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".m4v", ".avi", ".mov", ".ts", ".m2ts", ".webm", ".mpg", ".mpeg", ".vob", ".wmv", ".flv", ".ogv":
		return true
	}
	return false
}

// Folder sources are resolved against the same read allowlist as MCP. Selection
// returns an explicit bounded snapshot, never an implicitly truncated batch.
func (s *Server) folder(w http.ResponseWriter, r *http.Request) {
	resolver := s.engine.Deps().Fs
	if resolver == nil {
		fail(w, 400, "filesystem navigation is unavailable")
		return
	}
	path, err := resolver.ResolveRead(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if r.URL.Query().Get("files") == "1" {
		paths := []string{}
		visited := 0
		recursive := r.URL.Query().Get("recursive") == "1"
		err = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := r.Context().Err(); err != nil {
				return err
			}
			visited++
			if visited > 10000 {
				return fmt.Errorf("selection exceeds 10000 entries; choose a smaller folder")
			}
			if d.IsDir() {
				if p != path && (!recursive || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 || !videoPath(p) {
				return nil
			}
			real, err := resolver.ResolveRead(p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			paths = append(paths, real)
			if len(paths) > 1000 {
				return fmt.Errorf("selection exceeds 1000 videos; choose a smaller folder")
			}
			return nil
		})
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"path": path, "paths": paths})
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	items := []fsop.Stat{}
	q := strings.ToLower(r.URL.Query().Get("q"))
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), ".") || (!d.IsDir() && !videoPath(d.Name())) || (q != "" && !strings.Contains(strings.ToLower(d.Name()), q)) {
			continue
		}
		// Skip escaped links and nonregular files. Stat supplies measured bytes.
		stat, err := resolver.FileStat(filepath.Join(path, d.Name()))
		if err != nil {
			continue
		}
		info, err := os.Stat(stat.Path)
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		items = append(items, stat)
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(0, min(offset, len(items)))
	end := min(offset+100, len(items))
	writeJSON(w, 200, map[string]any{"path": path, "items": items[offset:end], "total": len(items), "has_more": end < len(items)})
}
