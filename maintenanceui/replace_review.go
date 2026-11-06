package maintenanceui

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// replace replaces exactly the files the person reviewed: POST
// {"ids": [transcode job ids], "digests": {"<id>": "sha256:..."}, "key": "..."}.
// digests carries, for a file whose replacement already waits on a plan, the
// digest of the plan shown; without it, or with another, that file is
// refused as plan_changed with the plan's digest, and a plan with a regrab
// warning is refused as needs_decision. All of them or none: when one is not
// ready now, the answer is 409 with every file's readiness and nothing is
// started. If a submission still fails partway, the answer is 207 and says
// which files started. A key's answer is kept: sent again with the same
// files, the same answer; with other files, 409 key_reused.
func (s *Server) replace(w http.ResponseWriter, r *http.Request) {
	var body action.ReplaceRequest
	if decode(w, r, &body) != nil || len(body.IDs) == 0 || len(body.IDs) > 1000 {
		fail(w, 400, `send {"ids": [the file jobs reviewed], "digests": {"<id>": "<plan digest>"}, "key": "..."}`)
		return
	}
	seen := map[string]bool{}
	for i, id := range body.IDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			fail(w, 400, "list each file once")
			return
		}
		seen[id] = true
		body.IDs[i] = id
	}
	for id := range body.Digests {
		if !seen[id] {
			fail(w, 400, "send a digest only for a listed file")
			return
		}
	}
	if !reconfigureKey.MatchString(body.Key) {
		fail(w, 400, "a valid submission key is required")
		return
	}
	if !s.cfg.AllowDestructive {
		fail(w, 403, "replacement is disabled in the server settings")
		return
	}
	answer, err := s.engine.ReplaceReviewed(r.Context(), body)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, answer.Status, answer.Body)
}

// replaceWhenDone switches whether a job or batch that has not ended replaces
// each file by itself once that file is done: POST {"id", "replace_when_done"}.
// On, it replaces only files that finish from now on: files already ready
// still need POST /api/maintenance/replace. Off, it stops the replacements not
// yet approved, also on a job that has ended while one of its automatic
// replacements is not approved yet; one already under way finishes.
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
// GET, or PUT {"replace_when_done_default": bool} and/or
// {"conversion_defaults": {...}}.
func (s *Server) preferences(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Deps().Store
	if st == nil {
		fail(w, 500, "settings store unavailable")
		return
	}
	if r.Method == http.MethodPut {
		var body struct {
			ReplaceWhenDoneDefault *bool                     `json:"replace_when_done_default"`
			ConversionDefaults     *store.ConversionDefaults `json:"conversion_defaults"`
		}
		if decode(w, r, &body) != nil || (body.ReplaceWhenDoneDefault == nil && body.ConversionDefaults == nil) {
			fail(w, 400, `send {"replace_when_done_default": true|false} or {"conversion_defaults": {...}}`)
			return
		}
		if body.ConversionDefaults != nil {
			if err := body.ConversionDefaults.Validate(); err != nil {
				fail(w, 400, err.Error())
				return
			}
			if err := st.SetConversionDefaults(*body.ConversionDefaults); err != nil {
				fail(w, 500, "preferences not saved")
				return
			}
		}
		if body.ReplaceWhenDoneDefault != nil {
			if err := st.SetReplaceWhenDoneDefault(*body.ReplaceWhenDoneDefault); err != nil {
				fail(w, 500, "preferences not saved")
				return
			}
		}
	}
	value, err := st.ReplaceWhenDoneDefault()
	if err != nil {
		fail(w, 500, "preferences unavailable")
		return
	}
	conversion, err := st.ConversionDefaults()
	if err != nil {
		fail(w, 500, "preferences unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{store.ReplaceWhenDoneDefaultKey: value, store.ConversionDefaultsKey: conversion})
}
