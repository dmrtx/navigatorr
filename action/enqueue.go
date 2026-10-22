package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/store"
)

type requestOriginKey struct{}

// WithOrigin records provenance independently of immutable execution inputs.
func WithOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, requestOriginKey{}, origin)
}

// EnqueueWorker returns a persisted receipt before expensive preflight work.
// Existing identities remain readable when the worker is offline; readiness
// gates only new admissions, not recovery of a response lost in transport.
func (e *Engine) EnqueueWorker(ctx context.Context, name string, inputs map[string]any, key string) (*ActionResult, error) {
	if !RequiresWorker(name) {
		return nil, fmt.Errorf("action %q is not a worker workflow", name)
	}
	if e.deps.Store == nil {
		return nil, fmt.Errorf("action store is required")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = generateActionID("submission")
	}
	old, err := e.deps.Store.FindActionByIdempotencyKey(name, key)
	if err != nil {
		return nil, err
	}
	if old != nil {
		// A key that already exists is a replay (or a conflict): Enqueue
		// answers from the stored action and judges the inputs only when
		// they differ from it.
		return e.Enqueue(ctx, name, inputs, key)
	}
	// A new submission: a bad request is refused before the worker is asked
	// anything and before the key is reserved.
	if tmpl, ok := e.GetTemplate(name); ok {
		if _, err := e.validateActionInputs(ctx, tmpl, inputs); err != nil {
			return nil, err
		}
	}
	if err := e.CheckWorkerAdmissionFor(ctx, name); err != nil {
		return nil, err
	}
	return e.Enqueue(ctx, name, inputs, key)
}

// Enqueue admits work durably without inspecting files or contacting a worker.
// The process reconciler owns execution, including the initial preflight.
func (e *Engine) Enqueue(ctx context.Context, name string, inputs map[string]any, key string) (*ActionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == removedBenchmarkAction {
		return nil, inputErr("", removedBenchmarkMessage)
	}
	tmpl, ok := e.GetTemplate(name)
	if !ok || !tmpl.AutoReconcile || !tmpl.ImmutableInputs {
		return nil, fmt.Errorf("action %q does not support background admission", name)
	}
	if e.deps.Store == nil {
		return nil, fmt.Errorf("action store is required")
	}
	key = strings.TrimSpace(key)
	if len(key) == 0 || len(key) > 200 {
		return nil, fmt.Errorf("a submission key of 1–200 bytes is required")
	}
	b, err := json.Marshal(inputs)
	if err != nil {
		return nil, err
	}
	var copy map[string]any
	if err := json.Unmarshal(b, &copy); err != nil || copy == nil {
		return nil, fmt.Errorf("inputs must be an object")
	}
	if name == "promote_transcode_candidate" {
		key, err = promotionIdempotency(copy)
		if err != nil {
			return nil, err
		}
	}
	// A replay whose inputs are identical to the stored action's answers from
	// the stored action, even if the configuration has changed since (a lost
	// response must stay recoverable). Only a submission that would create a
	// new action, or that differs from the stored one, is judged.
	if old, err := e.deps.Store.FindActionByIdempotencyKey(name, key); err != nil {
		return nil, err
	} else if old != nil {
		if matches, err := immutableActionInputsMatch(old.InputsJSON, copy); err == nil && matches {
			return e.Status(ctx, old.ID)
		}
	}
	// Judge the request before any write: an invalid one must not bind its
	// idempotency key to inputs that can never run.
	if copy, err = e.validateActionInputs(ctx, tmpl, copy); err != nil {
		return nil, err
	}
	// Recover synchronous admissions from earlier versions too, including
	// terminal actions that predate durable submission receipts.
	if old, err := e.deps.Store.FindActionByIdempotencyKey(name, key); err != nil {
		return nil, err
	} else if old != nil {
		matches, err := immutableActionInputsMatch(old.InputsJSON, copy)
		if err != nil || !matches {
			return nil, fmt.Errorf("submission key already belongs to different immutable inputs")
		}
		return e.Status(ctx, old.ID)
	}
	b, err = json.Marshal(copy)
	if err != nil {
		return nil, err
	}
	origin, _ := ctx.Value(requestOriginKey{}).(string)
	if origin == "" {
		origin = "unknown"
	}
	inst := store.ActionInstance{ID: generateActionID(name), ActionName: name, InputsJSON: string(b), StateJSON: toJSON(map[string]any{"request_origin": origin, "background_admission": true}), IdempotencyKey: strings.TrimSpace(key)}
	id, err := e.deps.Store.QueueAction(inst, name+":"+key)
	if err != nil {
		// A promotion can also be created simultaneously through synchronous MCP.
		if old, lookupErr := e.deps.Store.FindActionByIdempotencyKey(name, key); lookupErr == nil && old != nil {
			id, err = old.ID, nil
		}
	}
	if err != nil {
		return nil, err
	}
	old, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return nil, err
	}
	matches, err := immutableActionInputsMatch(old.InputsJSON, copy)
	if err != nil || !matches {
		return nil, fmt.Errorf("submission key already belongs to different immutable inputs")
	}
	return e.Status(ctx, id)
}
