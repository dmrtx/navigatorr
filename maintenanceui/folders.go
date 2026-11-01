package maintenanceui

import (
	"context"
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
	"github.com/jakenesler/navigatorr/internal/nashelper"
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
		sized := order == "size_desc" || order == "size_asc"
		candidates, err := s.folderCandidates(r.Context(), resolver, path, q, sized, timing)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		began = time.Now()
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
					stat, ok := folderEntryStat(resolver, path, c)
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
	name string
	// kind of the entry itself; a link is never followed to tell.
	link, dir, regular bool
	// entry is the mount's listing entry, when the listing came from the mount.
	entry fs.DirEntry
	// key orders the entries: the lower-cased path, which for a link is where it leads.
	key      string
	stat     fsop.Stat
	resolved bool
}

// folderCandidates lists the folder's entries that the listing may show. The
// NAS helper reads the folder where it lives, with type, size and time in one
// answer; without it (not configured, not mapped, too many entries, an older
// helper, unreachable) the folder is listed through the mount. A link is
// followed through the mount either way, since only the mount's read roots
// decide where a link may lead. When the order needs every size (sized), or a
// link has to be followed, the entry's stat is taken now; otherwise it is left
// for the entries of the page.
func (s *Server) folderCandidates(ctx context.Context, resolver *fsop.Resolver, path, q string, sized bool, timing *serverTiming) ([]folderCandidate, error) {
	began := time.Now()
	var raw []folderCandidate
	fromHelper := false
	if meter := s.nasMeter(); meter != nil {
		if _, mapped := meter.Remote(path); mapped {
			res, err := meter.List(ctx, path)
			if err != nil && ctx.Err() != nil {
				// The person left: listing the folder again through the mount is for no one.
				return nil, ctx.Err()
			}
			if err == nil {
				fromHelper = true
				raw = make([]folderCandidate, 0, len(res.Entries))
				for _, e := range res.Entries {
					c := folderCandidate{name: e.Name, link: e.Type == nashelper.EntrySymlink, dir: e.Type == nashelper.EntryDir, regular: e.Type == nashelper.EntryFile}
					if c.dir || c.regular {
						c.stat = fsop.Stat{Path: filepath.Join(path, e.Name), Size: e.Size, IsDir: c.dir, ModTime: time.Unix(0, e.MtimeNs).UTC().Format(time.RFC3339)}
						c.resolved = true
					}
					raw = append(raw, c)
				}
			}
		}
	}
	if !fromHelper {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		raw = make([]folderCandidate, 0, len(entries))
		for _, d := range entries {
			raw = append(raw, folderCandidate{name: d.Name(), link: d.Type()&fs.ModeSymlink != 0, dir: d.IsDir(), regular: d.Type().IsRegular(), entry: d})
		}
	}
	if fromHelper {
		timing.note("list", "helper")
	} else {
		timing.note("list", "mount")
	}
	timing.since("readdir", began)
	began = time.Now()
	candidates := make([]folderCandidate, 0, len(raw))
	for _, c := range raw {
		if strings.HasPrefix(c.name, ".") || (!c.dir && !videoPath(c.name)) || (q != "" && !strings.Contains(strings.ToLower(c.name), q)) {
			continue
		}
		c.key = strings.ToLower(filepath.Join(path, c.name))
		switch {
		case c.link || (sized && !c.resolved):
			stat, ok := folderEntryStat(resolver, path, c)
			if !ok {
				continue
			}
			c.stat, c.resolved, c.key = stat, true, strings.ToLower(stat.Path)
		case !c.dir && !c.regular:
			continue
		}
		candidates = append(candidates, c)
	}
	timing.since("stat", began)
	return candidates, nil
}

// folderEntryStat describes one entry the way the listing shows it, or says it
// must not be shown: a link that leads outside the read roots, to nothing, or
// to something that is neither a folder nor a regular file. An entry that is
// not a link lies where it is listed, inside the folder that was already
// resolved, so one stat of its own is all it takes.
func folderEntryStat(resolver *fsop.Resolver, dir string, c folderCandidate) (fsop.Stat, bool) {
	p := filepath.Join(dir, c.name)
	if c.link {
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
	if (!c.dir && !c.regular) || c.entry == nil {
		return fsop.Stat{}, false
	}
	info, err := c.entry.Info()
	if err != nil {
		return fsop.Stat{}, false
	}
	return fsop.Stat{Path: p, Size: info.Size(), IsDir: c.dir, ModTime: info.ModTime().UTC().Format(time.RFC3339)}, true
}
