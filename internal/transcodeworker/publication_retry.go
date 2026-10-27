package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// ErrNoPublicationToRetry reports a job that is not waiting to publish its
// finished output: it is still encoding, already finished, or a runner is
// publishing it right now.
var ErrNoPublicationToRetry = errors.New("job is not waiting to retry a publication")

// shortenPublicationNames renames the destination of a job queued before
// candidate names were bounded, whose candidate or ".partial.<job id>" upload
// file name exceeds the filesystem's 255-byte limit and so can never be
// published. Only the stored names change, in the same directory, with the job
// id and extension kept; the encoded file is untouched and the status now
// reports the shortened candidate path, which the coordinator adopts. A name
// that already fits is left exactly as it is. The idempotency digest is not
// recomputed: it identifies the original request.
func shortenPublicationNames(job *JobRecord) {
	dest := job.Destination()
	short := transcode.ShortenCandidatePath(dest, job.ID)
	if dest == "" || short == dest {
		return
	}
	if job.Candidate == dest {
		job.Candidate = short
	}
	if job.IntendedDestination != "" {
		job.IntendedDestination = short
	}
	if job.PartialPath != "" {
		job.PartialPath = PartialPathFor(short, job.ID)
	}
}

// RetryPublication is an operator's "try again" for a job whose encode
// finished but whose output could not be published, after the worker stopped
// retrying on its own. It gives the job a fresh recovery budget and marks one
// attempt as requested; the next recovery sweep then publishes the same
// encoded candidate. It never runs the encoder, never touches the candidate
// and never starts a runner itself, so it cannot race the sweep: both change
// the record only under the job lock.
func (w *Worker) RetryPublication(ctx context.Context, jobID string) (JobStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: err.Error()}, err
	}
	jobDir := filepath.Join(w.cfg.StateDir, jobID)
	jobFile := filepath.Join(jobDir, "job.json")
	// An unknown ID is answered before taking the lock, which would otherwise
	// create an empty job directory for it.
	if _, err := os.Stat(jobFile); errors.Is(err, fs.ErrNotExist) {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: "job not found"}, fmt.Errorf("%w: %w", ErrJobNotFound, err)
	}
	jobLock, err := acquireJobLock(jobDir)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: fmt.Sprintf("acquiring job lock: %v", err)}, err
	}
	defer jobLock.Unlock()

	job, err := loadJobForAPI(jobFile)
	if err != nil {
		return JobStatusResponse{ID: jobID, Status: "failed", Error: jobLoadErrorMessage(err)}, err
	}
	if !isPostEncodeResume(job) || (job.PID != 0 && w.jobAlive(job)) {
		return JobStatusResponse{ID: job.ID, Status: job.Status, CandidatePath: job.Candidate}, ErrNoPublicationToRetry
	}
	shortenPublicationNames(job)
	job.FinalizationRetryCount = 0
	job.NextFinalizationAt = time.Time{}
	job.OperatorRetryRequested = true
	job.JobTelemetry = telemetryFor(job, time.Now().UTC())
	if err := SaveJobAtomic(jobFile, job); err != nil {
		return JobStatusResponse{ID: job.ID, Status: job.Status, CandidatePath: job.Candidate, Error: err.Error()}, fmt.Errorf("saving publication retry for job %q: %w", job.ID, err)
	}
	return JobStatusResponse{ID: job.ID, Status: job.Status, CandidatePath: job.Candidate}, nil
}
