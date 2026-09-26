package action

import (
	"context"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/store"
)

func promotionIdempotency(inputs map[string]any) (string, error) {
	source := strings.TrimSpace(getString(inputs, "transcode_action_id"))
	if source == "" {
		return "", fmt.Errorf("transcode_action_id is required")
	}
	service := strings.ToLower(strings.TrimSpace(getString(inputs, "service")))
	if service == "" {
		service = "sonarr"
	}
	inputs["service"] = service
	inputs["transcode_action_id"] = source
	return "promote:" + service + ":" + source, nil
}

func (e *Engine) existingPromotion(ctx context.Context, inst *store.ActionInstance, tmpl ActionTemplate, inputs map[string]any) (*ActionResult, error) {
	ec := parseExecutionContext(inst, e)
	for _, key := range []string{"service", "transcode_action_id", "series_id", "batch_promote_parent_id", "batch_promote_item_key", "batch_promote_digest"} {
		if fmt.Sprint(ec.Inputs[key]) != fmt.Sprint(inputs[key]) {
			return nil, fmt.Errorf("promotion already exists for this candidate with different %s", key)
		}
	}
	// A step-zero failure cannot have performed a promotion side effect. Retry it
	// when the same idempotent request is submitted again so validation fixes
	// (for example accepting a JSON string series_id) do not leave the candidate
	// permanently pinned to an obsolete failed workflow. Later failures remain
	// explicit action_retry operations because they may need reconciliation.
	if inst.Status == StatusFailed && inst.CurrentStep == 0 {
		return e.Retry(ctx, inst.ID)
	}
	return buildActionResult(inst, len(tmpl.Steps), ec), nil
}
