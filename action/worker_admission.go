package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

type WorkerAdmissionError struct {
	Connected bool
	// TimedOut marks deep checks that did not finish: admission is unknown,
	// not proven broken. Submission is still refused.
	TimedOut bool
	Message  string
}

func (e *WorkerAdmissionError) Error() string { return e.Message }

func RequiresWorker(name string) bool {
	return name == "clean_podcast_ads" || name == "transcode_media" || name == "transcode_batch"
}

// Replacement retries use already encoded candidates at the final batch step.
func RetryRequiresWorker(inst *store.ActionInstance) bool {
	return inst != nil && RequiresWorker(inst.ActionName) && !(inst.ActionName == "transcode_batch" && inst.CurrentStep >= 2)
}

// Reviewing a single existing candidate does not submit an encode.
// A batch result decision may advance to its next file, so it needs a worker
// unless it is already at the final replacement-plan review.
func ResumeRequiresWorker(inst *store.ActionInstance, decision string) bool {
	if inst == nil || !RequiresWorker(inst.ActionName) {
		return false
	}
	switch decision {
	case "cancel", "abort", "pause":
		return false
	case "accept_loss":
		return inst.ActionName == "transcode_batch"
	case "approve", "reject":
		if inst.ActionName == "transcode_batch" {
			for _, raw := range []string{inst.StateJSON, inst.OutputsJSON} {
				var data struct {
					Plan *struct {
						Members []json.RawMessage `json:"members"`
					} `json:"batch_promotion_plan"`
				}
				if json.Unmarshal([]byte(raw), &data) == nil && data.Plan != nil && len(data.Plan.Members) > 0 {
					return false
				}
			}
		}
		if decision == "reject" {
			return inst.ActionName == "transcode_batch"
		}
	}
	if inst.ActionName == "clean_podcast_ads" && inst.CurrentStep == 2 {
		if decision != "plan" {
			return false
		}
		var checkpoint struct {
			Policy struct {
				ReviewRequired bool `json:"review_required"`
			} `json:"podcast_policy"`
		}
		// Planning into the review boundary needs only durable local artifacts.
		// With review disabled, planning can immediately submit render work.
		return json.Unmarshal([]byte(inst.StateJSON), &checkpoint) != nil || !checkpoint.Policy.ReviewRequired
	}
	return true
}

// CheckWorkerAdmission is shared by external adapters: it passes when any
// worker could take a new job. A lost connection after admission is still
// handled by durable reconciliation, not by duplicating jobs.
func (e *Engine) CheckWorkerAdmission(ctx context.Context) error {
	return e.checkAdmissionAmong(ctx, e.deps.Workers.All())
}

// CheckWorkerAdmissionFor is the admission of a new action: podcast work
// needs the podcast worker, anything else any worker.
func (e *Engine) CheckWorkerAdmissionFor(ctx context.Context, actionName string) error {
	if actionName == "clean_podcast_ads" {
		if w, ok := e.deps.Workers.Podcast(); ok {
			return e.checkAdmissionAmong(ctx, []transcode.NamedWorker{w})
		}
	}
	return e.CheckWorkerAdmission(ctx)
}

// CheckInstanceWorkerAdmission is the admission of an existing action going
// on: an action assigned a worker needs that worker, which its jobs never
// leave; any other the admission of a new action of its kind.
func (e *Engine) CheckInstanceWorkerAdmission(ctx context.Context, inst *store.ActionInstance) error {
	if inst == nil {
		return e.CheckWorkerAdmission(ctx)
	}
	var state map[string]any
	_ = json.Unmarshal([]byte(inst.StateJSON), &state)
	name := assignedWorker(state)
	if name == "" {
		return e.CheckWorkerAdmissionFor(ctx, inst.ActionName)
	}
	w, ok := e.deps.Workers.Get(name)
	if !ok {
		return &WorkerAdmissionError{Message: fmt.Sprintf("this job's worker %q is not configured on this server; nothing was sent", name)}
	}
	return e.checkAdmissionAmong(ctx, []transcode.NamedWorker{w})
}

// checkAdmissionAmong passes when any of the workers passes. They are checked
// at once, so an offline worker never delays the others. When none passes, the
// error is a connected worker's when there is one: its checks are what to fix.
func (e *Engine) checkAdmissionAmong(ctx context.Context, workers []transcode.NamedWorker) error {
	if len(workers) == 0 {
		return &WorkerAdmissionError{Message: "video worker is not configured; no job was submitted"}
	}
	errs := make([]error, len(workers))
	var wg sync.WaitGroup
	for i, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = checkWorkerAdmission(ctx, w.Executor)
		}()
	}
	wg.Wait()
	var chosen error
	chosenConnected := false
	for i, err := range errs {
		if err == nil {
			return nil
		}
		connected := false
		var admission *WorkerAdmissionError
		if errors.As(err, &admission) {
			connected = admission.Connected
			// With several workers, say which one refused.
			if e.deps.Workers.Len() > 1 {
				named := *admission
				named.Message = fmt.Sprintf("worker %s: %s", workers[i].Name, admission.Message)
				err = &named
			}
		}
		if chosen == nil || (connected && !chosenConnected) {
			chosen, chosenConnected = err, connected
		}
	}
	return chosen
}

// checkWorkerAdmission checks one worker: ready, then its deep checks.
func checkWorkerAdmission(ctx context.Context, exec transcode.Executor) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var err error
	if worker, ok := exec.(transcode.AvailabilityExecutor); ok {
		err = worker.Ready(ctx)
	} else {
		_, err = exec.Capabilities(ctx)
	}
	if err != nil {
		return &WorkerAdmissionError{Message: "video worker is offline or not ready; no job was submitted"}
	}
	if err := exec.Doctor(ctx); err != nil {
		message := "video worker is connected but its checks failed; no job was submitted"
		if text := strings.ToLower(err.Error()); strings.Contains(text, "root") || strings.Contains(text, "smb") || strings.Contains(text, "storage") {
			message = "video worker is connected but cannot access its media storage; no job was submitted"
		}
		return &WorkerAdmissionError{Connected: true, TimedOut: errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded), Message: message}
	}
	return nil
}
