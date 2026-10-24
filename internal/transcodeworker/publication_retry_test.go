package transcodeworker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/smbdirect"
	"github.com/jakenesler/navigatorr/transcode"
)

// runUntilRetriesRunOut drives the recovery sweep through the whole automatic
// budget, leaving the job waiting for a person.
func runUntilRetriesRunOut(t *testing.T, w *Worker, id string, finished <-chan error) {
	t.Helper()
	for attempt := 1; attempt <= MaxFinalizationRetries; attempt++ {
		makeFinalizationDue(t, w, id)
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("scheduled retry never ran")
		}
	}
	status, err := w.Status(context.Background(), id)
	if err != nil || !status.RecoveryRequired {
		t.Fatalf("automatic retries should be spent: %+v, err=%v", status, err)
	}
}

func TestRetryPublicationPublishesTheSameEncodeAfterRetriesRunOut(t *testing.T) {
	failure := &smbdirect.Error{Class: smbdirect.SMBSessionInvalid, Op: "publish", Err: errors.New("session expired")}
	// The first publication and every automatic retry fail; the fifth call works.
	w, store, id, encodes := schedulerPublicationFailure(t, failure, 1+MaxFinalizationRetries)
	finished := installScheduledFinalizer(w)
	stop := w.RunScheduler(context.Background(), "test", "", 5*time.Millisecond)
	defer stop()
	runUntilRetriesRunOut(t, w, id, finished)

	if _, err := w.RetryPublication(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	status, err := w.Status(context.Background(), id)
	if err != nil || status.RecoveryRequired {
		t.Fatalf("a requested retry should leave the worker recovering: %+v, err=%v", status, err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the requested retry never ran")
	}
	done := pr6b1LoadJob(t, w.cfg.StateDir, id)
	if done.Status != "completed" || done.OperatorRetryRequested || encodes.Load() != 1 || store.calls.Load() != 2+MaxFinalizationRetries {
		t.Fatalf("job=%+v encodes=%d publications=%d", done, encodes.Load(), store.calls.Load())
	}
}

// A failure the worker never retries on its own (a refused login, here) still
// gets exactly one attempt when a person asks, and then waits again.
func TestRetryPublicationAllowsOneAttemptPastAPermanentFailure(t *testing.T) {
	failure := &smbdirect.Error{Class: smbdirect.SMBAuthFailed, Op: "publish", Err: errors.New("invalid credentials")}
	w, store, id, encodes := schedulerPublicationFailure(t, failure, 100)
	finished := installScheduledFinalizer(w)
	stop := w.RunScheduler(context.Background(), "test", "", 5*time.Millisecond)
	defer stop()

	if _, err := w.RetryPublication(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("the publication was expected to fail again")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the requested retry never ran")
	}
	select {
	case err := <-finished:
		t.Fatalf("a permanent failure retried again on its own: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	job := pr6b1LoadJob(t, w.cfg.StateDir, id)
	if job.OperatorRetryRequested || !job.NextFinalizationAt.IsZero() || store.calls.Load() != 2 || encodes.Load() != 1 {
		t.Fatalf("job=%+v publications=%d encodes=%d", job, store.calls.Load(), encodes.Load())
	}
	status, err := w.Status(context.Background(), id)
	if err != nil || !status.RecoveryRequired {
		t.Fatalf("the job should wait for a person again: %+v, err=%v", status, err)
	}
}

func TestRetryPublicationRefusesWhatIsNotWaiting(t *testing.T) {
	failure := &smbdirect.Error{Class: smbdirect.SMBTransportError, Op: "publish", Err: errors.New("connection reset")}
	w, _, id, _ := schedulerPublicationFailure(t, failure, 1)
	finished := installScheduledFinalizer(w)
	stop := w.RunScheduler(context.Background(), "test", "", 5*time.Millisecond)
	defer stop()
	makeFinalizationDue(t, w, id)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not publish")
	}
	if _, err := w.RetryPublication(context.Background(), id); !errors.Is(err, ErrNoPublicationToRetry) {
		t.Fatalf("a finished job has nothing to retry: %v", err)
	}
	if _, err := w.RetryPublication(context.Background(), "job-missing-1"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("an unknown job is not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.cfg.StateDir, "job-missing-1")); !os.IsNotExist(err) {
		t.Fatalf("retrying an unknown job must not create its directory: %v", err)
	}
}

func TestRetryPublicationRouteAnswersWithTheWorkerEnvelope(t *testing.T) {
	failure := &smbdirect.Error{Class: smbdirect.SMBAuthFailed, Op: "publish", Err: errors.New("invalid credentials")}
	w, _, id, _ := schedulerPublicationFailure(t, failure, 100)
	srv := NewServer(w, "test-exe", "", "")

	assertEnvelope(t, doRequest(t, srv, http.MethodGet, "/v1/jobs/"+id+"/retry-publication", "", ""), http.StatusMethodNotAllowed, transcode.WorkerErrorMethodNotAllowed, false)
	assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/job-missing-1/retry-publication", "", ""), http.StatusNotFound, transcode.WorkerErrorNotFound, false)
	if rec := doRequest(t, srv, http.MethodPost, "/v1/jobs/"+id+"/retry-publication", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("status=%d (%s)", rec.Code, rec.Body.String())
	}
	if !pr6b1LoadJob(t, w.cfg.StateDir, id).OperatorRetryRequested {
		t.Fatal("the request was not recorded")
	}
	// Asking twice is harmless: the request only records the ask.
	if rec := doRequest(t, srv, http.MethodPost, "/v1/jobs/"+id+"/retry-publication", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("second request status=%d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := w.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/"+id+"/retry-publication", "", ""), http.StatusConflict, transcode.WorkerErrorNothingToRetry, false)
}
