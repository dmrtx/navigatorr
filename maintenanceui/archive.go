package maintenanceui

import (
	"errors"
	"net/http"

	"github.com/jakenesler/navigatorr/store"
)

func (s *Server) archive(w http.ResponseWriter, req *http.Request) {
	var input struct {
		ID       string `json:"id"`
		Archived *bool  `json:"archived"`
	}
	if err := decode(w, req, &input); err != nil || input.ID == "" || input.Archived == nil {
		fail(w, 400, "provide a job ID and archived flag")
		return
	}
	st := s.engine.Deps().Store
	if st == nil {
		fail(w, 503, "maintenance store is required")
		return
	}
	instances, err := st.ListMaintenanceActionSnapshot()
	if err != nil {
		fail(w, 500, "read maintenance history")
		return
	}
	records := make([]operationRecord, 0, len(instances))
	byID := map[string]operationRecord{}
	for _, inst := range instances {
		r := operationRecord{inst: inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
		records = append(records, r)
		byID[inst.ID] = r
	}
	roots, _, members := queueWorkflows(records, byID, queueReplacementLinks(records))
	root := roots[input.ID]
	if root == "" {
		fail(w, 404, "job not found")
		return
	}
	ids := make([]string, 0, len(members[root]))
	for _, member := range members[root] {
		ids = append(ids, member.inst.ID)
	}
	if err := st.SetMaintenanceArchived(root, ids, *input.Archived); err != nil {
		if errors.Is(err, store.ErrArchiveActiveWorkflow) {
			fail(w, 409, "finish or cancel active work before archiving")
		} else {
			fail(w, 500, "save archive state")
		}
		return
	}
	writeJSON(w, 200, map[string]any{"id": input.ID, "archived": *input.Archived})
}
