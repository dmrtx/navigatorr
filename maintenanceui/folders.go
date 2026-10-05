package maintenanceui

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		query := strings.ToLower(r.URL.Query().Get("q"))
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
			if d.Type()&os.ModeSymlink != 0 || !videoPath(p) || (query != "" && !strings.Contains(strings.ToLower(d.Name()), query)) {
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
	q := strings.ToLower(r.URL.Query().Get("q"))
	order := r.URL.Query().Get("sort")
	listing := r.URL.Query().Get("listing")
	var items []folderItem
	pending := false
	if listing != "" {
		list, err := s.folderListings.load(listing, path, q, order)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		items, pending = list.Items, list.Pending
	} else {
		entries, err := os.ReadDir(path)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		items = []folderItem{}
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
			item := folderItem{Stat: stat}
			if stat.IsDir && (order == "size_desc" || order == "size_asc") {
				size := s.folderSizes.get(stat.Path, resolver, s.engine.Deps().Store)
				item.FolderSize = &size
				pending = pending || size.Status != "ready" || size.Updating
			}
			items = append(items, item)
		}
		sortFolderItems(items, order)
		if order == "size_desc" || order == "size_asc" {
			listing, err = s.folderListings.save(folderListing{Path: path, Query: q, Order: order, Items: items, Pending: pending})
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(0, min(offset, len(items)))
	end := min(offset+100, len(items))
	page := make([]folderItem, 0, end-offset)
	for _, item := range items[offset:end] {
		if item.IsDir && r.URL.Query().Get("sizes") == "1" {
			size := s.folderSizes.get(item.Path, resolver, s.engine.Deps().Store)
			item.FolderSize = &size
		}
		page = append(page, item)
	}
	writeJSON(w, 200, map[string]any{"path": path, "items": page, "total": len(items), "has_more": end < len(items), "listing": listing, "size_order_pending": pending})
}
