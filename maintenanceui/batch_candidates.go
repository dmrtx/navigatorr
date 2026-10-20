package maintenanceui

import (
	"net/http"

	"github.com/jakenesler/navigatorr/action"
)

// candidateReview is the review plus how each candidate's size budget encode
// ended, keyed by its transcode action, for those that came from one.
type candidateReview struct {
	*action.BatchCandidateReview
	SizeBudgetOutcomes map[string]string `json:"size_budget_outcomes,omitempty"`
}

func (s *Server) batchCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		review, err := s.engine.BatchCandidates(r.Context(), r.URL.Query().Get("id"))
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		out := candidateReview{BatchCandidateReview: review}
		if st := s.engine.Deps().Store; st != nil {
			for _, member := range review.Members {
				child, err := st.GetActionInstanceIfExists(member.SourceAction)
				if err != nil || child == nil {
					continue
				}
				r := operationRecord{inst: *child, outputs: decodeOperationJSON(child.OutputsJSON), state: decodeOperationJSON(child.StateJSON)}
				if outcome := operationSizeBudgetOutcome(r); outcome != "" {
					if out.SizeBudgetOutcomes == nil {
						out.SizeBudgetOutcomes = map[string]string{}
					}
					out.SizeBudgetOutcomes[member.SourceAction] = outcome
				}
			}
		}
		writeJSON(w, 200, out)
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
