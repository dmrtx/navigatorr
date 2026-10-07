package maintenanceui

import (
	"github.com/jakenesler/navigatorr/podcast"
	"net/http"
	"sort"
)

func (s *Server) podcasts(w http.ResponseWriter, r *http.Request) {
	ids := []string{}
	policies := map[string]any{}
	if s.cfg.Podcasts.Enabled {
		for id, p := range s.cfg.Podcasts.Podcasts {
			if p.Enabled {
				ids = append(ids, id)
				if policy, err := s.cfg.Podcasts.Policy(id); err == nil {
					policies[id] = map[string]any{"digest": podcast.Digest(policy), "pipeline_version": podcast.Version, "prompt_version": podcast.PromptVersion, "known_ads_first_pass": policy.KnownAdsFirstPass}
				}
			}
		}
	}
	sort.Strings(ids)
	writeJSON(w, 200, map[string]any{"enabled": s.cfg.Podcasts.Enabled, "podcasts": ids, "policies": policies})
}
