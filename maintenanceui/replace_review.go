package maintenanceui

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// replace replaces exactly the files the person reviewed: POST
// {"ids": [transcode job ids], "key": "..."}. All of them or none: when one
// is not ready now, the answer is 409 with every file's readiness and nothing
// is started. A file of a batch that is still encoding is replaced now; the
// batch's other files are untouched and wait for their own review.
func (s *Server) replace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
		Key string   `json:"key"`
	}
	if decode(w, r, &body) != nil || len(body.IDs) == 0 || len(body.IDs) > 1000 {
		fail(w, 400, `send {"ids": [the file jobs reviewed], "key": "..."}`)
		return
	}
	seen := map[string]bool{}
	for _, id := range body.IDs {
		if strings.TrimSpace(id) == "" || seen[strings.TrimSpace(id)] {
			fail(w, 400, "list each file once")
			return
		}
		seen[strings.TrimSpace(id)] = true
	}
	if !reconfigureKey.MatchString(body.Key) {
		fail(w, 400, "a valid submission key is required")
		return
	}
	if !s.cfg.AllowDestructive {
		fail(w, 403, "replacement is disabled in the server settings")
		return
	}
	started, err := s.engine.ReplaceReviewed(r.Context(), body.IDs, body.Key)
	var refused *action.ErrReplaceRefused
	switch {
	case errors.As(err, &refused):
		writeJSON(w, 409, map[string]any{"error": refused.Error(), "code": "not_ready", "ready": refused.Ready, "not_ready": refused.NotReady})
		return
	case err != nil:
		writeJSON(w, 409, map[string]any{"error": err.Error(), "replacements": started})
		return
	}
	writeJSON(w, 202, map[string]any{"replacements": started})
}

// replaceWhenDone switches whether a job or batch that has not ended replaces
// each file by itself once that file is done: POST {"id", "replace_when_done"}.
// On, it replaces only files that finish from now on: files already ready
// still need POST /api/maintenance/replace. Off, it stops the replacements not
// yet approved; one already under way finishes.
func (s *Server) replaceWhenDone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID    string `json:"id"`
		Value *bool  `json:"replace_when_done"`
	}
	if decode(w, r, &body) != nil || strings.TrimSpace(body.ID) == "" || body.Value == nil {
		fail(w, 400, `send {"id": "...", "replace_when_done": true|false}`)
		return
	}
	inst, err := s.engine.SetReplaceWhenDone(r.Context(), strings.TrimSpace(body.ID), *body.Value)
	switch {
	case errors.Is(err, action.ErrReplaceWhenDoneFinished):
		writeJSON(w, 409, map[string]string{"error": err.Error(), "code": "finished"})
		return
	case err != nil:
		fail(w, 404, "job is unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"id": inst.ID, "replace_when_done": s.engine.ReplaceWhenDone(inst)})
}

// preferences are the server-side defaults both clients show:
// GET, or PUT {"replace_when_done_default": bool}.
func (s *Server) preferences(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Deps().Store
	if st == nil {
		fail(w, 500, "settings store unavailable")
		return
	}
	if r.Method == http.MethodPut {
		var body struct {
			ReplaceWhenDoneDefault *bool `json:"replace_when_done_default"`
		}
		if decode(w, r, &body) != nil || body.ReplaceWhenDoneDefault == nil {
			fail(w, 400, `send {"replace_when_done_default": true|false}`)
			return
		}
		if err := st.SetReplaceWhenDoneDefault(*body.ReplaceWhenDoneDefault); err != nil {
			fail(w, 500, "preferences not saved")
			return
		}
	}
	value, err := st.ReplaceWhenDoneDefault()
	if err != nil {
		fail(w, 500, "preferences unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{store.ReplaceWhenDoneDefaultKey: value})
}
