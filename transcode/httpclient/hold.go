package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrHoldUnsupported marks a worker that predates the queue hold.
var ErrHoldUnsupported = errors.New("worker does not support holding its queue")

// Hold reads whether the worker is holding its queue.
func (e *HTTPExecutor) Hold(ctx context.Context) (bool, error) {
	return e.holdRequest(ctx, http.MethodGet, nil)
}

// SetHold tells the worker to hold (or release) the jobs it has accepted but
// not started. Running jobs are never interrupted. It returns the hold the
// worker reports afterwards.
func (e *HTTPExecutor) SetHold(ctx context.Context, hold bool) (bool, error) {
	payload, err := json.Marshal(map[string]bool{"hold": hold})
	if err != nil {
		return false, err
	}
	return e.holdRequest(ctx, http.MethodPut, payload)
}

func (e *HTTPExecutor) holdRequest(ctx context.Context, method string, payload []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, method, "/v1/hold", payload)
	if err != nil {
		return false, err
	}
	data, code, _, err := e.do(req, "hold", "", false)
	if err != nil {
		return false, err
	}
	switch {
	case code == http.StatusOK:
		var out struct {
			Hold bool `json:"hold"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return false, fmt.Errorf("worker hold reply: %w", err)
		}
		return out.Hold, nil
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
		return false, ErrHoldUnsupported
	}
	return false, fmt.Errorf("worker hold HTTP %d", code)
}
