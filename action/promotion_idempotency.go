package action

import (
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

func (e *Engine) existingPromotion(inst *store.ActionInstance, tmpl ActionTemplate, inputs map[string]any) (*ActionResult, error) {
	ec := parseExecutionContext(inst, e)
	for _, key := range []string{"service", "transcode_action_id", "series_id"} {
		if fmt.Sprint(ec.Inputs[key]) != fmt.Sprint(inputs[key]) {
			return nil, fmt.Errorf("promotion already exists for this candidate with different %s", key)
		}
	}
	return buildActionResult(inst, len(tmpl.Steps), ec), nil
}
