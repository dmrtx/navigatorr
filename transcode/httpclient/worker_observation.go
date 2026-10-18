package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Scheduler probes the same bounded liveness response used by Health; it does
// not turn HTTP success into a claim that either scheduler sweep is healthy.
func (e *HTTPExecutor) Scheduler(ctx context.Context) (transcode.SchedulerObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, http.MethodGet, "/v1/health", nil)
	if err != nil {
		return transcode.SchedulerObservation{}, err
	}
	data, code, _, err := e.do(req, "health", "", false)
	if err != nil {
		return transcode.SchedulerObservation{}, err
	}
	if code != http.StatusOK {
		return transcode.SchedulerObservation{}, fmt.Errorf("worker health HTTP %d", code)
	}
	var body struct {
		OK        bool                           `json:"ok"`
		Scheduler transcode.SchedulerObservation `json:"scheduler"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return transcode.SchedulerObservation{}, err
	}
	if !body.OK {
		return transcode.SchedulerObservation{}, fmt.Errorf("worker liveness unconfirmed")
	}
	if body.Scheduler.Health != "ok" && body.Scheduler.Health != "degraded" {
		body.Scheduler.Health = "unknown"
	}
	if body.Scheduler.ObservedAt.IsZero() {
		body.Scheduler.Health = "unknown"
		body.Scheduler.Stale = true
	}
	return transcode.NormalizeSchedulerObservation(body.Scheduler, time.Now().UTC()), nil
}
