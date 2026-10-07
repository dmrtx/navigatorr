package action

import (
	"context"
	"fmt"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func valueAdReport(r *podcast.AdMatchReport) podcast.AdMatchReport {
	if r == nil {
		return podcast.AdMatchReport{}
	}
	return *r
}
func knownRanges(s podcastSession, b podcast.Block) []podcast.Decision {
	var ranges []podcast.Decision
	lastIndex := -2
	for i := b.First; i <= b.Last; i++ {
		u := s.Transcript.Units[i]
		k, ok := s.Known[u.ID]
		if !ok {
			continue
		}
		if len(ranges) > 0 && lastIndex == i-1 && ranges[len(ranges)-1].Label == k.Label && *ranges[len(ranges)-1].Evidence == k.Evidence {
			ranges[len(ranges)-1].LastID = u.ID
		} else {
			evidence := k.Evidence
			ranges = append(ranges, podcast.Decision{FirstID: u.ID, LastID: u.ID, Label: k.Label, Reason: "verified audio and native transcript text", Evidence: &evidence})
		}
		lastIndex = i
	}
	return ranges
}
func attachKnownAds(ec *ExecutionContext, s *podcastSession) error {
	if !s.Policy.KnownAdsFirstPass {
		return nil
	}
	var report podcast.AdMatchReport
	if err := podcast.ReadJSON(getString(ec.State, "podcast_match_path"), &report); err != nil {
		return err
	}
	known, err := podcast.KnownAdUnits(s.Transcript, report, s.Policy)
	if err != nil {
		return err
	}
	s.Matches = &report
	s.Known = known
	for _, b := range s.Blocks {
		complete := true
		for i := b.First; i <= b.Last; i++ {
			if _, ok := known[s.Transcript.Units[i].ID]; !ok {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(s.Transcript), PromptVersion: podcast.PromptVersion, Model: podcast.AdAlgorithm, Reasoning: "complete acoustic match"}
		c, err = podcast.MergeKnownAdDecisions(s.Transcript, b, c, known, nil)
		if err != nil {
			return err
		}
		s.Classifications[b.ID] = c
	}
	m := podcastSummary(ec)
	m["known_ad_units"] = len(known)
	m["llm_unknown_units"] = len(s.Transcript.Units) - len(known)
	m["acoustic_algorithm"] = podcast.AdAlgorithm
	return nil
}
func (e *Engine) checkKnownAds(ctx context.Context, s podcastSession) error {
	if s.Matches == nil || len(s.Known) == 0 {
		return nil
	}
	client, ok := e.deps.Transcode.(transcode.PodcastAdExecutor)
	if !ok {
		return fmt.Errorf("ad catalog API unavailable")
	}
	catalog, err := client.AdCatalog(ctx, s.Matches.Catalog.Scope)
	if err != nil {
		return err
	}
	active := map[string]podcast.AdReference{}
	for _, r := range catalog.References {
		if !r.Revoked {
			active[r.ID] = r
		}
	}
	original := map[string]podcast.AdReference{}
	for _, r := range s.Matches.Catalog.References {
		original[r.ID] = r
	}
	for _, k := range s.Known {
		r, ok := active[k.Evidence.ReferenceID]
		if !ok || r != original[k.Evidence.ReferenceID] {
			return fmt.Errorf("known ad reference changed or revoked; classifications, cuts and approval cannot be used; start a new action")
		}
	}
	return nil
}
func (e *Engine) PodcastAdLibrary(ctx context.Context, scope, revoke string) (podcast.AdCatalog, error) {
	if e.deps.Config == nil {
		return podcast.AdCatalog{}, fmt.Errorf("podcast config unavailable")
	}
	if _, err := e.deps.Config.Podcasts.Policy(scope); err != nil {
		return podcast.AdCatalog{}, err
	}
	client, ok := e.deps.Transcode.(transcode.PodcastAdExecutor)
	if !ok {
		return podcast.AdCatalog{}, fmt.Errorf("ad catalog API unavailable")
	}
	if revoke != "" {
		if err := client.RevokeAd(ctx, scope, revoke); err != nil {
			return podcast.AdCatalog{}, err
		}
	}
	return client.AdCatalog(ctx, scope)
}
