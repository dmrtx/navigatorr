package action

import (
	"fmt"
	"regexp"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/store"
)

func (e *Engine) freezePodcastInputs(inputs map[string]any) error {
	if e.deps.Config == nil {
		return fmt.Errorf("podcast configuration missing")
	}
	cached := 0
	for _, key := range []string{"cached_transcript_path", "cached_transcript_digest", "cached_asr_job_id"} {
		if getString(inputs, key) != "" {
			cached++
		}
	}
	if cached != 0 && cached != 3 {
		return fmt.Errorf("cached transcript requires path, digest and ASR job ID together")
	}
	if cached == 3 && (!podcast.ValidHash(getString(inputs, "cached_transcript_digest")) || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(getString(inputs, "cached_asr_job_id"))) {
		return fmt.Errorf("invalid cached transcript identity")
	}
	p, err := e.deps.Config.Podcasts.Policy(getString(inputs, "podcast_id"))
	if err != nil {
		return err
	}
	inputs["podcast_policy_digest"] = podcast.Digest(p)
	inputs["podcast_pipeline_version"] = podcast.Version
	return nil
}

// Podcast submission identities survive terminal states. Recovery is an
// explicit action_retry, never another ASR run under the same submission key.
func (e *Engine) existingPodcast(inst *store.ActionInstance, tmpl ActionTemplate, inputs map[string]any) (*ActionResult, error) {
	matches, err := immutableActionInputsMatch(inst.InputsJSON, inputs)
	if err != nil {
		return nil, fmt.Errorf("checking immutable podcast inputs for %s: %w", inst.ID, err)
	}
	if !matches {
		return nil, fmt.Errorf("podcast idempotency key %q already belongs to action %s with different immutable inputs or policy", inst.IdempotencyKey, inst.ID)
	}
	return buildActionResult(inst, len(tmpl.Steps), parseExecutionContext(inst, e)), nil
}
