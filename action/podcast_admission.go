package action

import (
	"fmt"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/store"
)

func (e *Engine) freezePodcastInputs(inputs map[string]any) error {
	if e.deps.Config == nil {
		return fmt.Errorf("podcast configuration missing")
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
