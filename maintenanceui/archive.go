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
	// A preview archived by the rule has no stored row: restoring one would report
	// something the next read undoes, and archiving one would leave a row that hides
	// the batch started from it. Restore says so; archive has nothing to do.
	auto, err := s.archivesItself(root)
	if err != nil {
		fail(w, 500, "read maintenance history")
		return
	}
	if auto {
		if *input.Archived {
			writeJSON(w, 200, map[string]any{"id": input.ID, "archived": true})
		} else {
			fail(w, 409, "a finished preview archives itself; start it from its page if you want it to run")
		}
		return
	}
	// A batch started from the preview a moment ago may not be in the member list yet:
	// the store checks it by key, in the archive's own transaction.
	if err := st.SetMaintenanceArchivedWith(root, ids, *input.Archived, []store.ActionKey{{ActionName: "transcode_batch", IdempotencyKey: previewExecutionPrefix + root}}); err != nil {
		if errors.Is(err, store.ErrArchiveActiveWorkflow) {
			fail(w, 409, "finish or cancel active work before archiving")
		} else {
			fail(w, 500, "save archive state")
		}
		return
	}
	// The preview may have finished between the check and the write.
	if !*input.Archived {
		auto, err := s.archivesItself(root)
		if err != nil {
			fail(w, 500, "read maintenance history")
			return
		}
		if auto {
			fail(w, 409, "a finished preview archives itself; start it from its page if you want it to run")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"id": input.ID, "archived": *input.Archived})
}

// archivesItself reports whether the workflow's root is a preview the rule archives.
func (s *Server) archivesItself(root string) (bool, error) {
	st := s.engine.Deps().Store
	inst, err := st.GetActionInstance(root)
	if err != nil || inst == nil {
		return false, err
	}
	started, err := st.FindActionByIdempotencyKey("transcode_batch", previewExecutionPrefix+root)
	if err != nil {
		return false, err
	}
	rec := operationRecord{inst: *inst, inputs: decodeOperationJSON(inst.InputsJSON), state: decodeOperationJSON(inst.StateJSON)}
	return autoArchivedPreview(rec, started != nil), nil
}
