package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type WorkerActivityJob struct {
	ID             string                    `json:"id"`
	Kind           string                    `json:"kind"`
	File           string                    `json:"file"`
	Phase          string                    `json:"phase"`
	StartedAt      time.Time                 `json:"started_at"`
	HeartbeatAt    time.Time                 `json:"heartbeat_at"`
	LastProgressAt time.Time                 `json:"last_progress_at"`
	Progress       *float64                  `json:"progress,omitempty"`
	Details        *BenchmarkProgressDetails `json:"details,omitempty"`
}
type WorkerActivity struct {
	SlotsTotal       int                 `json:"worker_slots_total"`
	SlotsUsed        int                 `json:"worker_slots_used"`
	Jobs             []WorkerActivityJob `json:"jobs"`
	CheckedAt        time.Time           `json:"checked_at"`
	DetailsAvailable bool                `json:"details_available"`
}

func (e *HTTPExecutor) Activity(ctx context.Context) (WorkerActivity, error) {
	var activity WorkerActivity
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, http.MethodGet, "/v1/activity", nil)
	if err != nil {
		return activity, err
	}
	data, code, _, err := e.do(req, "activity", "", false)
	if err != nil {
		return activity, err
	}
	if code == 404 {
		// During a rolling upgrade an older worker can still report real capacity.
		req, err = e.newRequest(ctx, http.MethodGet, "/v1/ready", nil)
		if err != nil {
			return activity, err
		}
		data, code, _, err = e.do(req, "ready", "", false)
		if err != nil {
			return activity, err
		}
	}
	if code != 200 {
		return activity, fmt.Errorf("worker activity unavailable")
	}
	if err = json.Unmarshal(data, &activity); err != nil || activity.SlotsTotal < 1 || activity.SlotsUsed < 0 {
		return activity, fmt.Errorf("worker capacity unknown")
	}
	return activity, nil
}
