package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jakenesler/navigatorr/transcode"
)

func (e *HTTPExecutor) Activity(ctx context.Context) (transcode.WorkerActivity, error) {
	var activity transcode.WorkerActivity
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
