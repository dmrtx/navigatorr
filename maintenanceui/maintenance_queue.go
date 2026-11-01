package maintenanceui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// The maintenance queue is the list of jobs the owner or a model keeps adding
// to the store: files worth transcoding, titles missing a usable language.
// The console shows them and lets the owner close one by hand. An entry knows
// its title and the *arr's id for it, not a path, so the folder is looked up
// in Sonarr or Radarr, the same list Files browses.

type maintenanceQueueItem struct {
	ID        int64    `json:"id"`
	Title     string   `json:"title"`
	Year      *float64 `json:"year,omitempty"`
	Service   string   `json:"service"`
	MediaType string   `json:"media_type"`
	MediaID   string   `json:"media_id"`
	IssueType string   `json:"issue_type"`
	Priority  int      `json:"priority"`
	Status    string   `json:"status"`
	// State is what the console says: open, processed or dismissed.
	State string `json:"state"`
	// ClosedBy is "console" when the owner closed it, "workflow" for a verified replacement.
	ClosedBy  string `json:"closed_by,omitempty"`
	SizeBytes *int64 `json:"size_bytes,omitempty"`
	Notes     string `json:"notes"`
	Folder    string `json:"folder,omitempty"`
	InFiles   bool   `json:"in_files"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func maintenanceState(it store.MaintenanceItem) (state, closedBy string) {
	switch it.Status {
	case store.MaintDone:
		if it.ConsoleClosed() == "processed" {
			return "processed", "console"
		}
		return "processed", "workflow"
	case store.MaintFailed:
		if it.ConsoleClosed() == "dismissed" {
			return "dismissed", "console"
		}
		return "dismissed", "workflow"
	}
	return "open", ""
}

// libraryFolders maps each service's title ids to the folder and year *arr knows.
// A service that does not answer leaves its entries without a folder; the page
// still loads.
func (s *Server) libraryFolders(ctx context.Context, services map[string]bool) map[string]map[string]map[string]any {
	out := map[string]map[string]map[string]any{}
	for name := range services {
		if name != "sonarr" && name != "radarr" {
			continue
		}
		svc, err := s.registry.Get(name)
		if err != nil {
			continue
		}
		endpoint := "/api/v3/series"
		if name == "radarr" {
			endpoint = "/api/v3/movie"
		}
		list, _, err := s.arrLists.get(name, false, newServerTiming(), func(*serverTiming) ([]map[string]any, error) {
			// The read is shared with whoever asks while it runs, and a stale list
			// may be read again after this request is over.
			fetchCtx, cancel := context.WithTimeout(context.Background(), arrFetchTimeout)
			defer cancel()
			b, err := svc.Get(fetchCtx, endpoint, nil)
			if err != nil {
				return nil, fmt.Errorf("library service unavailable: %w", err)
			}
			var records []map[string]any
			if json.Unmarshal(b, &records) != nil {
				return nil, fmt.Errorf("invalid library response")
			}
			projected := make([]map[string]any, len(records))
			for i, rec := range records {
				projected[i] = projectLibraryRecord(rec)
			}
			return projected, nil
		})
		if err != nil {
			continue
		}
		byID := make(map[string]map[string]any, len(list))
		for _, title := range list {
			if id, ok := title["id"].(float64); ok {
				byID[strconv.Itoa(int(id))] = title
			}
		}
		out[name] = byID
	}
	return out
}

// underRoots reports whether path is inside a folder the Files tab can browse.
func underRoots(path string, roots []string) bool {
	path = filepath.Clean(path)
	for _, root := range roots {
		root = strings.TrimRight(root, "/")
		if root != "" && (path == root || strings.HasPrefix(path, root+"/")) {
			return true
		}
	}
	return false
}

func (s *Server) maintenanceQueueItems(ctx context.Context, items []store.MaintenanceItem) []maintenanceQueueItem {
	services := map[string]bool{}
	for _, it := range items {
		services[it.Service] = true
	}
	titles := s.libraryFolders(ctx, services)
	roots := s.cfg.Media.AllowedReadRoots
	out := make([]maintenanceQueueItem, 0, len(items))
	for _, it := range items {
		state, closedBy := maintenanceState(it)
		row := maintenanceQueueItem{
			ID: it.ID, Title: it.Title, Service: it.Service, MediaType: it.MediaType, MediaID: it.MediaID,
			IssueType: it.IssueType, Priority: it.Priority, Status: it.Status, State: state, ClosedBy: closedBy,
			SizeBytes: it.CurrentSize, Notes: it.Notes, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
		}
		if title := titles[it.Service][it.MediaID]; title != nil {
			if folder, _ := title["path"].(string); strings.HasPrefix(folder, "/") {
				row.Folder = folder
				row.InFiles = underRoots(folder, roots)
			}
			if year, ok := title["year"].(float64); ok && year > 0 {
				row.Year = &year
			}
			if row.SizeBytes == nil {
				if size, ok := title["size"].(float64); ok && size > 0 {
					n := int64(size)
					row.SizeBytes = &n
				}
			}
		}
		out = append(out, row)
	}
	return out
}

func (s *Server) maintenanceQueue(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Deps().Store
	if st == nil {
		fail(w, 503, "maintenance store is required")
		return
	}
	if r.Method == http.MethodPost {
		s.maintenanceQueueAct(w, r, st)
		return
	}
	var statuses []string
	switch r.URL.Query().Get("state") {
	case "", "open":
		statuses = []string{store.MaintPending, store.MaintResearching, store.MaintCandidate, store.MaintDownloading,
			store.MaintDownloaded, store.MaintVerifying, store.MaintImporting, store.MaintReplacing, store.MaintBlocked}
	case "processed":
		statuses = []string{store.MaintDone}
	case "dismissed":
		statuses = []string{store.MaintFailed}
	case "all":
	default:
		fail(w, 400, "state must be open, processed, dismissed or all")
		return
	}
	issue := r.URL.Query().Get("issue_type")
	items, err := st.ListItemsForConsole(statuses, issue, 0)
	if err != nil {
		fail(w, 500, "read the maintenance queue")
		return
	}
	counts, err := st.CountItemsForConsole(issue)
	if err != nil {
		fail(w, 500, "count the maintenance queue")
		return
	}
	began := time.Now()
	rows := s.maintenanceQueueItems(r.Context(), items)
	timing := newServerTiming()
	timing.since("library", began)
	timing.set(w)
	writeJSONTagged(w, r, 200, map[string]any{"items": rows, "counts": counts, "total": len(rows)})
}

func (s *Server) maintenanceQueueAct(w http.ResponseWriter, r *http.Request, st *store.Store) {
	var input struct {
		ID     int64  `json:"id"`
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := decode(w, r, &input); err != nil || input.ID <= 0 {
		fail(w, 400, "provide an id and an action: processed, dismiss or restore")
		return
	}
	if len(input.Note) > 500 {
		fail(w, 400, "note is limited to 500 characters")
		return
	}
	note := strings.TrimSpace(input.Note)
	var (
		it  store.MaintenanceItem
		err error
	)
	switch input.Action {
	case "processed":
		it, err = st.MarkItemProcessed(input.ID, note)
	case "dismiss":
		it, err = st.DismissItem(input.ID, note)
	case "restore":
		it, err = st.RestoreItem(input.ID)
	default:
		fail(w, 400, "action must be processed, dismiss or restore")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "no such maintenance item") {
			fail(w, 404, "maintenance entry not found")
		} else {
			fail(w, 409, err.Error())
		}
		return
	}
	_ = st.LogAction("maintenance_console", it.Service, it.Title,
		fmt.Sprintf(`{"id":%d,"action":%q}`, it.ID, input.Action), "item "+strconv.FormatInt(it.ID, 10)+" status="+it.Status)
	counts, _ := st.CountItemsForConsole("")
	rows := s.maintenanceQueueItems(r.Context(), []store.MaintenanceItem{it})
	writeJSON(w, 200, map[string]any{"item": rows[0], "counts": counts})
}
