package maintenanceui

import (
	"net/http"
	"time"
)

// workflowIndex reads every maintenance action once and groups them into queue
// workflows: the root of each action and the members of each root.
func (s *Server) workflowIndex() (map[string]string, map[string][]operationRecord, error) {
	st := s.engine.Deps().Store
	instances, err := st.ListMaintenanceActionSnapshot()
	if err != nil {
		return nil, nil, err
	}
	records := s.recordMemo.records(instances, time.Now().UTC())
	byID := make(map[string]operationRecord, len(records))
	for _, r := range records {
		byID[r.inst.ID] = r
	}
	roots, _, members := queueWorkflows(records, byID, queueReplacementLinks(records))
	return roots, members, nil
}

// workflowMemberIDs returns the root of a queue workflow and every action in
// it, or "" when the id belongs to no workflow.
func (s *Server) workflowMemberIDs(id string) (string, []string, error) {
	roots, members, err := s.workflowIndex()
	if err != nil {
		return "", nil, err
	}
	return memberIDsOf(roots, members, id)
}

func memberIDsOf(roots map[string]string, members map[string][]operationRecord, id string) (string, []string, error) {
	root := roots[id]
	if root == "" {
		return "", nil, nil
	}
	ids := make([]string, 0, len(members[root]))
	for _, member := range members[root] {
		ids = append(ids, member.inst.ID)
	}
	return root, ids, nil
}

// jobLeftovers reports, and on POST removes, the temporary files a job's
// transcode attempts left beside the originals: candidates that nothing can
// promote any more, such as those of cancelled or failed attempts, or of
// attempts whose original has since been replaced. Originals and candidates
// that can still replace them are never listed or touched.
func (s *Server) jobLeftovers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if r.Method == http.MethodGet {
		body.ID = r.URL.Query().Get("id")
	} else if decode(w, r, &body) != nil {
		fail(w, 400, "provide a job ID")
		return
	}
	if body.ID == "" {
		fail(w, 400, "provide a job ID")
		return
	}
	if s.engine.Deps().Store == nil {
		fail(w, 503, "maintenance store is required")
		return
	}
	root, ids, err := s.workflowMemberIDs(body.ID)
	if err != nil {
		fail(w, 500, "read maintenance history")
		return
	}
	if root == "" {
		fail(w, 404, "job not found")
		return
	}
	page, err := s.engine.LeftoverCandidatesOf(r.Context(), ids)
	if err != nil {
		fail(w, 500, "read temporary files")
		return
	}
	canDelete := s.engine.AllowDestructive()
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"id": body.ID, "files": len(page.Items), "bytes": page.Bytes, "can_delete": canDelete})
		return
	}
	if !canDelete {
		fail(w, 403, "removing files is not enabled on this server")
		return
	}
	removed, kept, freed, firstError := 0, 0, int64(0), ""
	for _, item := range page.Items {
		if _, err := s.engine.DiscardLeftoverCandidate(r.Context(), item.ActionID); err != nil {
			kept++
			if firstError == "" {
				firstError = err.Error()
			}
			continue
		}
		removed++
		freed += item.Bytes + item.PartialBytes
	}
	writeJSON(w, 200, map[string]any{"id": body.ID, "removed": removed, "kept": kept, "freed_bytes": freed, "error": firstError})
}
