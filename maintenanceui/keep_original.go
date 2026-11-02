package maintenanceui

import "net/http"

// keepOriginal is the owner's "Keep original" for a finished encode that is
// waiting to replace its original: the smaller copy is dropped, the original is
// not touched and the file leaves the ready-to-replace list. It repeats
// harmlessly. A file in a batch, or one whose replacement has started, is
// refused with the reason.
func (s *Server) keepOriginal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if decode(w, r, &body) != nil || body.ID == "" {
		fail(w, 400, "provide a job ID")
		return
	}
	if !s.engine.AllowDestructive() {
		fail(w, 403, "removing files is not enabled on this server")
		return
	}
	if _, err := s.engine.KeepOriginal(r.Context(), body.ID); err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": body.ID})
}

// discardArchivedCandidates drops the smaller copies of archived encodes.
// Archiving is the owner's decision to discard a job, and its copy keeps using
// disk until someone deletes it. Each id is checked here (its workflow is
// archived and nothing in it is running) and again by the engine, which never
// touches an original and keeps a copy that a replacement or a batch review
// still references. It repeats harmlessly and answers how many went.
func (s *Server) discardArchivedCandidates(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if decode(w, r, &body) != nil || len(body.IDs) == 0 || len(body.IDs) > savingsReadyListed {
		fail(w, 400, "provide the archived job IDs")
		return
	}
	if !s.engine.AllowDestructive() {
		fail(w, 403, "removing files is not enabled on this server")
		return
	}
	st := s.engine.Deps().Store
	if st == nil {
		fail(w, 503, "maintenance store is required")
		return
	}
	archives, err := st.MaintenanceArchives()
	if err != nil {
		fail(w, 500, "read archive state")
		return
	}
	roots, workflows, err := s.workflowIndex()
	if err != nil {
		fail(w, 500, "read maintenance history")
		return
	}
	removed, kept, firstError := 0, 0, ""
	refuse := func(reason string) {
		kept++
		if firstError == "" {
			firstError = reason
		}
	}
	for _, id := range body.IDs {
		root, members, _ := memberIDsOf(roots, workflows, id)
		if root == "" || archives[root] == "" {
			refuse("a job is not archived")
			continue
		}
		running := false
		for _, member := range members {
			if inst, err := st.GetActionInstanceIfExists(member); err == nil && inst != nil && operationActive(inst.Status) {
				running = true
			}
		}
		if running {
			refuse("an archived job is running again")
			continue
		}
		if _, err := s.engine.KeepOriginalOfArchived(r.Context(), id); err != nil {
			refuse(err.Error())
			continue
		}
		removed++
	}
	writeJSON(w, 200, map[string]any{"removed": removed, "kept": kept, "error": firstError})
}
