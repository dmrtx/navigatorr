package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrSuspendUnsupported marks a worker that predates the suspend.
var ErrSuspendUnsupported = errors.New("worker does not support suspending its encodes")

// SuspendStatus is what a worker reports about its suspend. Active is how many
// jobs it is running and Frozen how many of those their runners report frozen;
// CountsKnown is false from a worker that does not report them.
type SuspendStatus struct {
	Suspend     bool
	Active      int
	Frozen      int
	CountsKnown bool
}

// Suspend reads whether the worker is suspended.
func (e *HTTPExecutor) Suspend(ctx context.Context) (SuspendStatus, error) {
	return e.suspendRequest(ctx, http.MethodGet, nil)
}

// SetSuspend freezes (or continues) the encodes the worker is running and
// holds its queue while frozen. It returns the state the worker reports
// afterwards.
func (e *HTTPExecutor) SetSuspend(ctx context.Context, suspend bool) (SuspendStatus, error) {
	payload, err := json.Marshal(map[string]bool{"suspend": suspend})
	if err != nil {
		return SuspendStatus{}, err
	}
	return e.suspendRequest(ctx, http.MethodPut, payload)
}

func (e *HTTPExecutor) suspendRequest(ctx context.Context, method string, payload []byte) (SuspendStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, method, "/v1/suspend", payload)
	if err != nil {
		return SuspendStatus{}, err
	}
	data, code, _, err := e.do(req, "suspend", "", false)
	if err != nil {
		return SuspendStatus{}, err
	}
	switch {
	case code == http.StatusOK:
		var out struct {
			Suspend bool `json:"suspend"`
			Active  *int `json:"active"`
			Frozen  *int `json:"frozen"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return SuspendStatus{}, fmt.Errorf("worker suspend reply: %w", err)
		}
		st := SuspendStatus{Suspend: out.Suspend}
		if out.Active != nil && out.Frozen != nil {
			st.Active, st.Frozen, st.CountsKnown = *out.Active, *out.Frozen, true
		}
		return st, nil
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
		return SuspendStatus{}, ErrSuspendUnsupported
	}
	return SuspendStatus{}, fmt.Errorf("worker suspend HTTP %d", code)
}
