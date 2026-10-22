package maintenanceui

import (
	"net/http"

	"github.com/jakenesler/navigatorr/action"
)

// candidateReview is the review plus, for each member, the facts a person
// needs to decide on it (sizes, measured quality, tracks), keyed by its
// transcode action. The facts are display only: the version and the plan
// digest cover the members alone.
type candidateReview struct {
	*action.BatchCandidateReview
	MemberFacts map[string]replacementFacts `json:"member_facts"`
}

// preparedPlan is a batch's prepared replacement plan with the same display
// facts. The digest is the server's; an approval must quote it.
type preparedPlan struct {
	Prepared bool `json:"prepared"`
	*action.BatchPromotionPlanRead
	MemberFacts map[string]replacementFacts `json:"member_facts"`
}

func (s *Server) batchCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		id := r.URL.Query().Get("id")
		if r.URL.Query().Get("plan") != "" {
			plan, err := s.engine.BatchPromotionPlan(r.Context(), id)
			if err != nil {
				fail(w, 409, err.Error())
				return
			}
			if plan == nil {
				writeJSON(w, 200, map[string]any{"prepared": false})
				return
			}
			ids := make([]string, 0, len(plan.Members))
			for _, member := range plan.Members {
				ids = append(ids, member.SourceAction)
			}
			writeJSON(w, 200, preparedPlan{Prepared: true, BatchPromotionPlanRead: plan, MemberFacts: s.memberFacts(ids)})
			return
		}
		review, err := s.engine.BatchCandidates(r.Context(), id)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		ids := make([]string, 0, len(review.Members))
		for _, member := range review.Members {
			ids = append(ids, member.SourceAction)
		}
		writeJSON(w, 200, candidateReview{BatchCandidateReview: review, MemberFacts: s.memberFacts(ids)})
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
