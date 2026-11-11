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
	root, ids, err := s.workflowMemberIDs(input.ID)
	if err != nil {
		fail(w, 500, "read maintenance history")
		return
	}
	if root == "" {
		fail(w, 404, "job not found")
		return
	}
	// A preview archived by the rule has no stored row, so removing one would
	// report a restore that the next read undoes. Say so instead.
	if !*input.Archived {
		inst, err := st.GetActionInstance(root)
		if err != nil {
			fail(w, 500, "read maintenance history")
			return
		}
		if inst != nil {
			started, err := st.FindActionByIdempotencyKey("transcode_batch", previewExecutionPrefix+root)
			if err != nil {
				fail(w, 500, "read maintenance history")
				return
			}
			rec := operationRecord{inst: *inst, inputs: decodeOperationJSON(inst.InputsJSON), state: decodeOperationJSON(inst.StateJSON)}
			if autoArchivedPreview(rec, started != nil) {
				fail(w, 409, "a finished preview archives itself; start it from its page if you want it to run")
				return
			}
		}
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
