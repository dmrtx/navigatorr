package maintenanceui

import (
	"net/http"
	"strings"
)

type batchItemView struct {
	ItemKey       string   `json:"item_key"`
	FilePath      string   `json:"file_path"`
	DisplayLabel  string   `json:"display_label"`
	Status        string   `json:"status"`
	ChildActionID string   `json:"child_action_id,omitempty"`
	Decision      string   `json:"decision"`
	Error         string   `json:"error,omitempty"`
	Profile       string   `json:"profile,omitempty"`
	Reasons       []string `json:"reasons,omitempty"`
}

// Batch items come from their durable relation, rather than the intentionally
// truncated MCP output snapshot. Known batch ownership prevents arbitrary
// workflow IDs from being used to inspect unrelated maintenance records.
func (s *Server) batchItems(w http.ResponseWriter, req *http.Request) {
	offset, limit, err := operationPaging(req)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	id := strings.TrimSpace(req.URL.Query().Get("id"))
	if id == "" {
		fail(w, 400, "batch id is required")
		return
	}
	st := s.engine.Deps().Store
	if st == nil {
		fail(w, 503, "maintenance store is required")
		return
	}
	inst, err := st.GetActionInstanceIfExists(id)
	if err != nil {
		fail(w, 500, "read batch")
		return
	}
	if inst == nil || inst.ActionName != "transcode_batch" {
		fail(w, 404, "batch not found")
		return
	}
	items, err := st.ListTranscodeBatchItems(id)
	if err == nil {
		items, _, err = s.previewPendingItems(*inst, items)
	}
	if err != nil {
		fail(w, 500, "read batch items")
		return
	}
	views := []batchItemView{}
	end := min(offset, len(items)) + min(limit, max(0, len(items)-offset))
	if offset < len(items) {
		for _, item := range items[offset:end] {
			views = append(views, batchItemView{ItemKey: item.ItemKey, FilePath: item.FilePath, DisplayLabel: item.DisplayLabel, Status: item.Status, ChildActionID: item.ChildActionID, Decision: item.Decision, Error: batchReasonText(item.Error, true), Profile: item.Profile, Reasons: item.Reasons})
		}
	}
	writeJSON(w, 200, map[string]any{"items": views, "total": len(items), "offset": offset, "has_more": end < len(items)})
}
