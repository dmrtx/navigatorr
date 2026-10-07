package maintenanceui

import "net/http"

func (s *Server) batchCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		review, err := s.engine.BatchCandidates(r.Context(), r.URL.Query().Get("id"))
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, review)
		return
	}
	var body struct {
		ID       string   `json:"id"`
		Version  string   `json:"version"`
		Keys     []string `json:"item_keys"`
		Digest   string   `json:"digest"`
		Decision string   `json:"decision"`
		Key      string   `json:"key"`
	}
	if decode(w, r, &body) != nil || body.ID == "" {
		fail(w, 400, "invalid candidate review request")
		return
	}
	if !s.cfg.AllowDestructive {
		fail(w, 403, "replacement is disabled in the server settings")
		return
	}
	inputs := map[string]any{"id": body.ID}
	if body.Decision != "" {
		if body.Digest == "" || (body.Decision != "approve" && body.Decision != "reject") {
			fail(w, 400, "review digest and decision are required")
			return
		}
		inputs["kind"], inputs["digest"], inputs["decision"] = "review_batch_promotion", body.Digest, body.Decision
	} else {
		if body.Version == "" || len(body.Keys) == 0 || len(body.Keys) > 1000 {
			fail(w, 400, "select candidates from the current review")
			return
		}
		inputs["kind"], inputs["version"], inputs["item_keys"] = "prepare_batch_promotion", body.Version, body.Keys
	}
	s.admitCommand(w, r, inputs, body.Key)
}
