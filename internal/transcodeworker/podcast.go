package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
)

func (w *Worker) executePodcast(ctx context.Context, dir, file string, job *JobRecord, r *resolvedOperational) error {
	fail := func(err error) error { return w.failJobTerminal(dir, file, job, err) }
	for _, path := range []string{job.Source, job.Candidate} {
		if err := w.requirePodcastMediaPath(path); err != nil {
			return fail(err)
		}
	}
	if err := w.ensureExternalHealthy(ctx, job.Source); err != nil {
		return fail(err)
	}
	if cancelled, err := w.ensureStaged(ctx, dir, file, job, r); err != nil {
		return fail(err)
	} else if cancelled {
		return nil
	}
	if err := verifyLocalDigest(ctx, r.effectiveInput, job.SourceSHA256); err != nil {
		return fail(err)
	}
	task := job.Plan.Podcast
	result := &podcast.Result{Operation: task.Operation, SourceHash: "sha256:" + strings.TrimPrefix(job.SourceSHA256, "sha256:")}
	phase := "transcribing"
	if task.Operation == "render" {
		phase = "cutting"
	} else if task.Operation == "match_ads" {
		phase = "matching_known_ads"
	}
	if cancelled, err := w.persistOperationalProgress(dir, file, job, func(l *JobRecord) { l.Phase = phase }); err != nil {
		return err
	} else if cancelled {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.localCandidate), 0700); err != nil {
		return fail(err)
	}
	// Keep a private transcript in the worker job even after shared-storage cleanup.
	transcriptPath := filepath.Join(dir, "podcast-transcript.json")
	if task.Operation == "match_ads" {
		path := filepath.Join(dir, "podcast-matches.json")
		var report podcast.AdMatchReport
		if err := podcast.ReadJSON(path, &report); os.IsNotExist(err) {
			report, err = w.matchAds(ctx, dir, r.effectiveInput, result.SourceHash, *task.Catalog)
			if err != nil {
				return fail(err)
			}
			if err := podcast.WriteJSON(path, report); err != nil {
				return fail(err)
			}
		} else if err != nil {
			return fail(err)
		}
		if err := report.Validate(); err != nil {
			return fail(err)
		}
		if report.SourceHash != result.SourceHash || podcast.Digest(report.Catalog) != podcast.Digest(task.Catalog) {
			return fail(fmt.Errorf("matching checkpoint identity differs"))
		}
		result.MatchDigest = podcast.Digest(report)
		result.DurationMS = report.DurationMS
		if cancelled, err := w.publishPodcastFile(ctx, dir, file, path, r.localCandidate); err != nil {
			return fail(err)
		} else if cancelled {
			return nil
		}
	} else if task.Operation == "transcribe" {
		var t podcast.Transcript
		if err := podcast.ReadJSON(transcriptPath, &t); err != nil {
			args := []string{r.effectiveInput, transcriptPath, task.Language}
			if w.cfg.AppleSpeechInstallAssets {
				args = append(args, "--install-assets")
			}
			if err = w.podcastCommand(ctx, dir, w.cfg.AppleSpeechPath, args...); err != nil {
				return fail(fmt.Errorf("apple ASR: %w", err))
			}
			if err = podcast.ReadJSON(transcriptPath, &t); err != nil {
				return fail(err)
			}
		}
		if err := t.Validate(); err != nil {
			return fail(err)
		}
		if t.SourceHash != result.SourceHash || !strings.EqualFold(strings.ReplaceAll(t.Language, "-", "_"), strings.ReplaceAll(task.Language, "-", "_")) {
			return fail(fmt.Errorf("ASR source/language mismatch"))
		}
		if err := podcast.WriteJSON(transcriptPath, t); err != nil {
			return fail(err)
		}
		result.TranscriptDigest = podcast.Digest(t)
		result.DurationMS = t.DurationMS
		result.UnitCount = len(t.Units)
		result.ASRWallSeconds = t.WallSeconds
		result.RealtimeFactor = t.RealtimeFactor
		if cancelled, err := w.publishPodcastFile(ctx, dir, file, transcriptPath, r.localCandidate); err != nil {
			return fail(err)
		} else if cancelled {
			return nil
		}
	} else {
		asr, err := LoadJob(filepath.Join(w.cfg.StateDir, task.ASRJobID, "job.json"))
		if err != nil {
			return fail(err)
		}
		if asr.Status != "completed" || asr.Plan == nil || asr.Plan.Podcast == nil || asr.Plan.Podcast.Operation != "transcribe" || asr.SourceSHA256 != job.SourceSHA256 {
			return fail(fmt.Errorf("completed matching ASR checkpoint is required"))
		}
		var t podcast.Transcript
		if err := podcast.ReadJSON(filepath.Join(w.cfg.StateDir, task.ASRJobID, "podcast-transcript.json"), &t); err != nil {
			return fail(err)
		}
		if err := podcast.VerifyCuts(t, *task.Cuts); err != nil {
			return fail(err)
		}
		if task.Learning != nil {
			if _, err := podcast.AdSeeds(t, *task.Learning, *task.Cuts); err != nil {
				return fail(err)
			}
		}
		if err := w.verifyAdDecisionEvidence(task, t, job.SourceSHA256); err != nil {
			return fail(err)
		}
		result.TranscriptDigest = podcast.Digest(t)
		result.DurationMS = t.DurationMS
		result.RemovedMS = task.Cuts.RemovedMS
		if err := podcast.WriteJSON(filepath.Join(dir, "cuts.json"), task.Cuts); err != nil {
			return fail(err)
		}
		// Render privately, validate before making the local candidate visible.
		tmp := filepath.Join(dir, "podcast-render.mp3")
		args := []string{"-nostdin", "-v", "error", "-y", "-i", r.effectiveInput, "-filter_complex", podcastFilter(*task.Cuts), "-map", "[out]", "-map_metadata", "0", "-map_chapters", "-1", "-c:a", "libmp3lame", "-q:a", "2", tmp}
		format, _ := exec.CommandContext(ctx, w.ffprobePath, "-v", "error", "-show_entries", "format=format_name", "-of", "default=nw=1:nk=1", r.effectiveInput).Output()
		if len(task.Cuts.Ranges) == 0 && strings.TrimSpace(string(format)) == "mp3" {
			if err := StageInputAtomic(ctx, r.effectiveInput, tmp); err != nil {
				return fail(err)
			}
		} else if err := w.podcastCommand(ctx, dir, w.ffmpegPath, args...); err != nil {
			return fail(err)
		}
		if err := podcast.EmbedTranscript(tmp, task.ASRJobID, t, *task.Cuts); err != nil {
			return fail(fmt.Errorf("embed transcript: %w", err))
		}
		duration, err := w.podcastDuration(ctx, tmp)
		if err != nil {
			return fail(err)
		}
		if math.Abs(float64(duration-(t.DurationMS-task.Cuts.RemovedMS))) > 250 {
			return fail(fmt.Errorf("podcast duration validation failed"))
		}
		if err := w.podcastCommand(ctx, dir, w.ffmpegPath, "-nostdin", "-v", "error", "-xerror", "-i", tmp, "-map", "0:a:0", "-f", "null", "-"); err != nil {
			return fail(fmt.Errorf("full audio decode validation: %w", err))
		}
		result.OutputDurationMS = duration
		result.DecodePassed = true
		if err := verifyLocalDigest(ctx, r.effectiveInput, job.SourceSHA256); err != nil {
			return fail(err)
		}
		if task.Learning != nil {
			learned, err := w.learnAds(ctx, dir, file, r.effectiveInput, t, task)
			if errors.Is(err, errPodcastPublicationCancelled) {
				return nil
			}
			if err != nil {
				return fail(err)
			}
			result.LearnedAds = learned
		}
		if cancelled, err := w.publishPodcastFile(ctx, dir, file, tmp, r.localCandidate); err != nil {
			return fail(err)
		} else if cancelled {
			return nil
		}
	}
	if err := verifyLocalDigest(ctx, r.effectiveInput, job.SourceSHA256); err != nil {
		return fail(err)
	}
	hash, err := hashLocalFileSHA256(ctx, r.localCandidate)
	if err != nil {
		return fail(err)
	}
	fi, err := os.Stat(r.localCandidate)
	if err != nil {
		return fail(err)
	}
	if cancelled, err := w.persistOperationalProgress(dir, file, job, func(l *JobRecord) {
		l.Podcast = result
		l.EncodeComplete = true
		l.LocalCandidatePath = r.localCandidate
		l.CandidateSHA256 = hash
		l.CandidateSizeBytes = fi.Size()
		l.ValidationFinishedAt = time.Now().UTC()
	}); err != nil {
		return err
	} else if cancelled {
		return nil
	}
	return w.finalizeOperational(ctx, dir, file, job, r)
}

// Semantic media must not alias worker-owned checkpoints or render scratch.
// Resolve existing ancestors so a symlinked output parent cannot reach them.
func (w *Worker) requirePodcastMediaPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	physical, err := physicalScratchPath(abs)
	if err != nil {
		return err
	}
	for _, root := range []string{w.cfg.StateDir, w.localWorkDir()} {
		root, err = filepath.Abs(root)
		if err != nil {
			return err
		}
		root, err = physicalScratchPath(root)
		if err != nil {
			return err
		}
		if IsPathWithinAllowedRoots(physical, []string{root}) {
			return fmt.Errorf("podcast source and candidate must remain outside worker state/scratch: %s", path)
		}
	}
	return nil
}

func (w *Worker) podcastCommand(ctx context.Context, dir, binary string, args ...string) error {
	f, err := os.OpenFile(filepath.Join(dir, "podcast.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w (see podcast.log)", filepath.Base(binary), err)
	}
	return nil
}
func (w *Worker) podcastDuration(ctx context.Context, path string) (int64, error) {
	b, err := exec.CommandContext(ctx, w.ffprobePath, "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, fmt.Errorf("invalid audio duration")
	}
	return int64(math.Round(seconds * 1000)), nil
}
func podcastFilter(c podcast.Cuts) string {
	var filters, names []string
	start := int64(0)
	keep := func(a, z int64) {
		if z <= a {
			return
		}
		name := fmt.Sprintf("k%d", len(names))
		filters = append(filters, fmt.Sprintf("[0:a:0]atrim=start=%.3f:end=%.3f,asetpts=PTS-STARTPTS[%s]", float64(a)/1000, float64(z)/1000, name))
		names = append(names, "["+name+"]")
	}
	for _, r := range c.Ranges {
		keep(start, r.StartMS)
		start = r.EndMS
	}
	keep(start, c.DurationMS)
	filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=0:a=1,asetnsamples=n=1152:p=0[out]", strings.Join(names, ""), len(names)))
	return strings.Join(filters, ";")
}

// A fully written native transcript is a durable ASR checkpoint, even if the
// runner died before updating job.json. Requeue the SAME identity and let the
// existing scheduler run validation/publication without invoking ASR again.
func podcastASRCheckpoint(dir string, job *JobRecord) bool {
	if job.Plan == nil || job.Plan.Podcast == nil {
		return false
	}
	if job.Plan.Podcast.Operation == "match_ads" {
		var r podcast.AdMatchReport
		return podcast.ReadJSON(filepath.Join(dir, "podcast-matches.json"), &r) == nil && r.Validate() == nil && r.SourceHash == "sha256:"+strings.TrimPrefix(job.SourceSHA256, "sha256:") && podcast.Digest(r.Catalog) == podcast.Digest(job.Plan.Podcast.Catalog)
	}
	if job.Plan.Podcast.Operation != "transcribe" {
		return false
	}
	var t podcast.Transcript
	return podcast.ReadJSON(filepath.Join(dir, "podcast-transcript.json"), &t) == nil && t.Validate() == nil && t.SourceHash == "sha256:"+strings.TrimPrefix(job.SourceSHA256, "sha256:") && strings.ReplaceAll(t.Language, "-", "_") == strings.ReplaceAll(job.Plan.Podcast.Language, "-", "_")
}

// Atomic, exclusive publication. Equality permits recovery after a crash just
// after publication; unrelated files are never overwritten.
var errPodcastPublicationCancelled = errors.New("podcast publication cancelled")
var errPodcastAtomicPublicationUnsupported = errors.New("podcast_atomic_publication_unsupported: atomic no-clobber publication is required")

type podcastPublicationGuardKey struct{}
type podcastPublicationGuard func(func() error) error

func (w *Worker) guardPodcastPublication(ctx context.Context, dir, file string, publish func() error) error {
	lock, err := acquireJobLock(dir)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	latest, err := LoadJob(file)
	if err != nil {
		return err
	}
	if latest.Status == "cancelled" {
		return errPodcastPublicationCancelled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publish()
}

func (w *Worker) publishPodcastFile(ctx context.Context, dir, file, src, dst string) (bool, error) {
	err := publishPodcastFile(ctx, src, dst, func(publish func() error) error {
		// Copying happens outside the lock so Cancel remains responsive. Only
		// the atomic commit is serialized with its durable cancellation state.
		return w.guardPodcastAdPublication(ctx, dir, file, publish)
	})
	if errors.Is(err, errPodcastPublicationCancelled) {
		return true, nil
	}
	return false, err
}

func publishPodcastFile(ctx context.Context, src, dst string, guard func(func() error) error) error {
	owned, err := os.CreateTemp(filepath.Dir(dst), ".podcast-publish-*")
	if err != nil {
		return err
	}
	tmp := owned.Name()
	owned.Close()
	if err := os.Remove(tmp); err != nil {
		return err
	}
	if err := StageInputAtomic(ctx, src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	return guard(func() error {
		if err = os.Link(tmp, dst); err != nil {
			same, e := filesHaveEqualContent(ctx, tmp, dst)
			if e != nil || !same {
				return fmt.Errorf("podcast destination exists or cannot be published: %w", err)
			}
		}
		d, err := os.Open(filepath.Dir(dst))
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	})
}
