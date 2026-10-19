package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jakenesler/navigatorr/transcode"
)

// ErrSlotsUnsupported marks a worker that predates per-kind slots.
var ErrSlotsUnsupported = errors.New("worker does not support per-kind slots")

// SlotSplitRejectedError is the worker refusing a split as invalid. The
// message is the worker's own and safe to show an operator.
type SlotSplitRejectedError struct{ Message string }

func (e *SlotSplitRejectedError) Error() string { return e.Message }

type slotsResponse struct {
	Slots    transcode.WorkerSlots `json:"slots"`
	Warnings []string              `json:"warnings"`
	Error    string                `json:"error"`
}

// Slots reads the worker's slot split and per-kind use.
func (e *HTTPExecutor) Slots(ctx context.Context) (transcode.WorkerSlots, error) {
	body, err := e.slotsRequest(ctx, http.MethodGet, nil)
	return body.Slots, err
}

// SetSlots pushes a slot split to the worker, which applies it at its next
// admission without interrupting running jobs. Both values zero clears the
// split. It returns the worker's slots afterwards and any CPU-budget warnings.
func (e *HTTPExecutor) SetSlots(ctx context.Context, split transcode.SlotSplit) (transcode.WorkerSlots, []string, error) {
	payload, err := json.Marshal(split)
	if err != nil {
		return transcode.WorkerSlots{}, nil, err
	}
	body, err := e.slotsRequest(ctx, http.MethodPut, payload)
	return body.Slots, body.Warnings, err
}

func (e *HTTPExecutor) slotsRequest(ctx context.Context, method string, payload []byte) (slotsResponse, error) {
	var out slotsResponse
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, method, "/v1/slots", payload)
	if err != nil {
		return out, err
	}
	data, code, _, err := e.do(req, "slots", "", false)
	if err != nil {
		return out, err
	}
	_ = json.Unmarshal(data, &out)
	switch {
	case code == http.StatusOK:
		return out, nil
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
		return out, ErrSlotsUnsupported
	case code == http.StatusBadRequest && out.Error != "":
		return out, &SlotSplitRejectedError{Message: out.Error}
	}
	return out, fmt.Errorf("worker slots HTTP %d", code)
}
