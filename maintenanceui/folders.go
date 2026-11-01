package maintenanceui

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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
	timing := newServerTiming()
	q := strings.ToLower(r.URL.Query().Get("q"))
	order := r.URL.Query().Get("sort")
	listing := r.URL.Query().Get("listing")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(0, offset)
	// window is the page's items; total counts every entry the folder lists.
	var window []folderItem
	var total int
	pending := false
	began := time.Now()
	if listing != "" {
		list, err := s.folderListings.load(listing, path, q, order)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		pending = list.Pending
		total = len(list.Items)
		offset = min(offset, total)
		window = list.Items[offset:min(offset+folderPageSize, total)]
	} else {
		entries, err := os.ReadDir(path)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		timing.since("readdir", began)
		began = time.Now()
		sized := order == "size_desc" || order == "size_asc"
		candidates := make([]folderCandidate, 0, len(entries))
		for _, d := range entries {
			if strings.HasPrefix(d.Name(), ".") || (!d.IsDir() && !videoPath(d.Name())) || (q != "" && !strings.Contains(strings.ToLower(d.Name()), q)) {
				continue
			}
			c := folderCandidate{entry: d, key: strings.ToLower(filepath.Join(path, d.Name()))}
			switch {
			case sized || d.Type()&fs.ModeSymlink != 0:
				// Sizes order the whole folder, and a link has to be followed to know
				// where it leads: both need the entry's own stat now.
				stat, ok := folderEntryStat(resolver, path, d)
				if !ok {
					continue
				}
				c.stat, c.resolved, c.key = stat, true, strings.ToLower(stat.Path)
			case !d.IsDir() && !d.Type().IsRegular():
				continue
			}
			candidates = append(candidates, c)
		}
		total = len(candidates)
		offset = min(offset, total)
		if sized {
			items := make([]folderItem, 0, len(candidates))
			for _, c := range candidates {
				item := folderItem{Stat: c.stat}
				if c.stat.IsDir {
					size := s.folderSizes.get(c.stat.Path, resolver, s.engine.Deps().Store, s.nasMeter())
					item.FolderSize = &size
					pending = pending || size.Status != "ready" || size.Updating
				}
				items = append(items, item)
			}
			sortFolderItems(items, order)
			listing, err = s.folderListings.save(folderListing{Path: path, Query: q, Order: order, Items: items, Pending: pending})
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			window = items[offset:min(offset+folderPageSize, total)]
		} else {
			// Names alone put the entries in order, so only the page's entries are
			// asked about their size and time; on a NAS mount every stat is a round trip.
			sort.SliceStable(candidates, func(i, j int) bool {
				if order == "name_desc" {
					return candidates[i].key > candidates[j].key
				}
				return candidates[i].key < candidates[j].key
			})
			for _, c := range candidates[offset:min(offset+folderPageSize, total)] {
				if !c.resolved {
					stat, ok := folderEntryStat(resolver, path, c.entry)
					if !ok {
						continue
					}
					c.stat = stat
				}
				window = append(window, folderItem{Stat: c.stat})
			}
		}
		timing.since("entries", began)
	}
	began = time.Now()
	marks := s.fileStatuses()
	timing.since("status", began)
	began = time.Now()
	page := make([]folderItem, 0, len(window))
	for _, item := range window {
		if item.IsDir {
			item.Status = marks.under(item.Path)
		} else {
			item.Status = marks.file(item.Path)
		}
		if item.IsDir && r.URL.Query().Get("sizes") == "1" {
			size := s.folderSizes.get(item.Path, resolver, s.engine.Deps().Store, s.nasMeter())
			item.FolderSize = &size
		}
		page = append(page, item)
	}
	timing.since("sizes", began)
	timing.set(w)
	writeJSONTagged(w, r, 200, map[string]any{"path": path, "items": page, "total": total, "has_more": offset+folderPageSize < total, "listing": listing, "size_order_pending": pending})
}

// folderPageSize is how many entries one page of a folder lists.
const folderPageSize = 100

type folderCandidate struct {
	entry fs.DirEntry
	// key orders the entries: the lower-cased path, which for a link is where it leads.
	key      string
	stat     fsop.Stat
	resolved bool
}

// folderEntryStat describes one entry the way the listing shows it, or says it
// must not be shown: a link that leads outside the read roots, to nothing, or
// to something that is neither a folder nor a regular file. An entry that is
// not a link lies where it is listed, inside the folder that was already
// resolved, so one stat of its own is all it takes.
func folderEntryStat(resolver *fsop.Resolver, dir string, d fs.DirEntry) (fsop.Stat, bool) {
	p := filepath.Join(dir, d.Name())
	if d.Type()&fs.ModeSymlink != 0 {
		stat, err := resolver.FileStat(p)
		if err != nil {
			return fsop.Stat{}, false
		}
		info, err := os.Stat(stat.Path)
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fsop.Stat{}, false
		}
		return stat, true
	}
	if !d.IsDir() && !d.Type().IsRegular() {
		return fsop.Stat{}, false
	}
	info, err := d.Info()
	if err != nil {
		return fsop.Stat{}, false
	}
	return fsop.Stat{Path: p, Size: info.Size(), IsDir: d.IsDir(), ModTime: info.ModTime().UTC().Format(time.RFC3339)}, true
}
