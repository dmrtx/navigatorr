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

// HoldStatus is what a worker reports about its queue hold. Active is how
// many jobs it is running right now; ActiveKnown is false from a worker that
// does not report it.
type HoldStatus struct {
	Hold        bool
	Active      int
	ActiveKnown bool
}

// Hold reads whether the worker is holding its queue.
func (e *HTTPExecutor) Hold(ctx context.Context) (HoldStatus, error) {
	return e.holdRequest(ctx, http.MethodGet, nil)
}

// SetHold tells the worker to hold (or release) the jobs it has accepted but
// not started. Running jobs are never interrupted. It returns the hold the
// worker reports afterwards.
func (e *HTTPExecutor) SetHold(ctx context.Context, hold bool) (HoldStatus, error) {
	payload, err := json.Marshal(map[string]bool{"hold": hold})
	if err != nil {
		return HoldStatus{}, err
	}
	return e.holdRequest(ctx, http.MethodPut, payload)
}

func (e *HTTPExecutor) holdRequest(ctx context.Context, method string, payload []byte) (HoldStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, method, "/v1/hold", payload)
	if err != nil {
		return HoldStatus{}, err
	}
	data, code, _, err := e.do(req, "hold", "", false)
	if err != nil {
		return HoldStatus{}, err
	}
	switch {
	case code == http.StatusOK:
		var out struct {
			Hold   bool `json:"hold"`
			Active *int `json:"active"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return HoldStatus{}, fmt.Errorf("worker hold reply: %w", err)
		}
		st := HoldStatus{Hold: out.Hold}
		if out.Active != nil {
			st.Active, st.ActiveKnown = *out.Active, true
		}
		return st, nil
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
		return HoldStatus{}, ErrHoldUnsupported
	}
	return HoldStatus{}, fmt.Errorf("worker hold HTTP %d", code)
}
