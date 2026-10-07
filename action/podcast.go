package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

type podcastSession struct {
	Version         int                               `json:"version"`
	Policy          podcast.Policy                    `json:"policy"`
	Transcript      podcast.Transcript                `json:"transcript"`
	Blocks          []podcast.Block                   `json:"blocks"`
	Classifications map[string]podcast.Classification `json:"classifications"`
	Reads           map[string]map[int]bool           `json:"reads"`
	Cuts            *podcast.Cuts                     `json:"cuts,omitempty"`
	ApprovedDigest  string                            `json:"approved_digest,omitempty"`
	ReviewReads     map[int]bool                      `json:"review_reads,omitempty"`
}

func (e *Engine) registerPodcastTemplate() {
	e.RegisterTemplate(ActionTemplate{Name: "clean_podcast_ads", Version: podcast.Version, AutoReconcile: true, ImmutableInputs: true,
		Description:    "Durable podcast cleaning. Original preserved. ASR runs on the existing worker queue. The orchestrating LLM reads EVERY podcast_block page and submits ID-based labels with podcast_classify; Navigatorr never invokes a classifier model. Resume with decision=plan after complete coverage. Review cuts with podcast_review before rendering when required. Output is a validated, separate MP3 for the existing feed/download integration.",
		RequiredInputs: []string{"path", "podcast_id", "output_path"}, OptionalInputs: []string{"episode_id", "feed_id", "idempotency_key", "source_sha256", "cached_transcript_path", "cached_transcript_digest", "cached_asr_job_id"},
		Steps: []StepDefinition{
			{Name: "podcast_preflight", Run: e.podcastPreflight},
			{Name: "podcast_transcribe", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
				return e.podcastWorkerStage(ctx, ec, "transcribe")
			}},
			{Name: "podcast_classify", Run: e.podcastClassification},
			{Name: "podcast_review", Run: e.podcastReview},
			{Name: "podcast_render", Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
				return e.podcastWorkerStage(ctx, ec, "render")
			}},
			{Name: "podcast_accept", Run: e.podcastAccept},
		},
	})
}
func (e *Engine) podcastPreflight(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if e.deps.Config == nil || e.deps.Fs == nil || e.deps.Transcode == nil {
		return StepResult{}, fmt.Errorf("podcast requires filesystem, config and worker")
	}
	c := e.deps.Config.Podcasts
	if err := c.Validate(); err != nil {
		return StepResult{}, err
	}
	policy, err := c.Policy(getString(ec.Inputs, "podcast_id"))
	if err != nil {
		return StepResult{}, err
	}
	if frozen := getString(ec.Inputs, "podcast_policy_digest"); frozen != "" && frozen != podcast.Digest(policy) {
		return StepResult{}, fmt.Errorf("podcast policy changed after admission; submit a new action")
	}
	source, err := e.deps.Fs.ResolveRead(getString(ec.Inputs, "path"))
	if err != nil {
		return StepResult{}, err
	}
	output, err := e.deps.Fs.ResolveWrite(getString(ec.Inputs, "output_path"))
	if err != nil {
		return StepResult{}, err
	}
	if _, err := e.deps.Fs.ResolveRead(output); err != nil {
		return StepResult{}, fmt.Errorf("output must also be in allowed_read_roots: %w", err)
	}
	if source == output || strings.ToLower(filepath.Ext(output)) != ".mp3" {
		return StepResult{}, fmt.Errorf("output must be a separate MP3 path")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return StepResult{}, fmt.Errorf("output already exists or cannot be inspected")
	}
	caps, err := e.deps.Transcode.Capabilities(ctx)
	if err != nil {
		if isRetryableWorkerPollError(err) {
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: "Waiting for podcast capabilities"}, nil
		}
		return StepResult{}, err
	}
	if caps.Podcast == nil || !caps.Podcast.Available || !caps.Podcast.NativeTimingVerified || !caps.Podcast.MP3RenderVerified || strings.ReplaceAll(caps.Podcast.VerifiedLanguage, "-", "_") != strings.ReplaceAll(policy.Language, "-", "_") {
		reason := "worker did not report podcast capability"
		if caps.Podcast != nil {
			reason = caps.Podcast.Reason
			if reason == "" {
				reason = fmt.Sprintf("available=%t native_timing=%t mp3_render=%t verified_language=%s", caps.Podcast.Available, caps.Podcast.NativeTimingVerified, caps.Podcast.MP3RenderVerified, caps.Podcast.VerifiedLanguage)
			}
		}
		return StepResult{}, fmt.Errorf("worker has no verified native ASR/MP3 capability for %s: %s", policy.Language, reason)
	}
	hash, _, err := e.deps.Fs.Hash(ctx, source)
	if err != nil {
		return StepResult{}, err
	}
	if expected := getString(ec.Inputs, "source_sha256"); expected != "" {
		normalized, err := transcode.NormalizeSourceSHA256(expected)
		if err != nil || normalized != strings.TrimPrefix(hash, "sha256:") {
			return StepResult{}, fmt.Errorf("input audio hash differs from source_sha256")
		}
	}
	path := filepath.Join(c.ArtifactDir, ec.InstanceID, "session.json")
	summary := map[string]any{"phase": "transcribing", "podcast_id": getString(ec.Inputs, "podcast_id"), "episode_id": getString(ec.Inputs, "episode_id"), "feed_id": getString(ec.Inputs, "feed_id"), "source_hash": "sha256:" + strings.TrimPrefix(hash, "sha256:"), "policy_digest": podcast.Digest(policy), "prompt_version": podcast.PromptVersion, "original_preserved": true, "published": false, "output_path": output, "human_audio_review_passed": false}
	ec.State["podcast_session"] = path
	ec.State["podcast_policy"] = policy
	ec.State["podcast"] = summary
	ec.State["resolved_path"] = source
	ec.State["podcast_output"] = output
	ec.State["source_sha256"] = strings.TrimPrefix(hash, "sha256:")
	return StepResult{Status: StepCompleted, Outputs: ec.State}, nil
}
func podcastSummary(ec *ExecutionContext) map[string]any {
	m, _ := ec.State["podcast"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		ec.State["podcast"] = m
	}
	return m
}
func loadPodcastSession(ec *ExecutionContext) (podcastSession, error) {
	var s podcastSession
	path := getString(ec.State, "podcast_session")
	if path == "" {
		return s, fmt.Errorf("podcast checkpoint missing")
	}
	err := podcast.ReadJSON(path, &s)
	if err == nil {
		err = s.Transcript.Validate()
	}
	if err == nil && (s.Version != podcast.Version || s.Transcript.SourceHash != "sha256:"+getString(ec.State, "source_sha256") || podcast.Digest(s.Policy) != podcastSummary(ec)["policy_digest"]) {
		err = fmt.Errorf("podcast checkpoint identity changed")
	}
	if err == nil {
		if expected, _ := podcastSummary(ec)["transcript_digest"].(string); expected != "" && expected != podcast.Digest(s.Transcript) {
			err = fmt.Errorf("immutable transcript checkpoint changed")
		}
	}
	return s, err
}
func savePodcastSession(ec *ExecutionContext, s podcastSession) error {
	return podcast.WriteJSON(getString(ec.State, "podcast_session"), s)
}
func (e *Engine) podcastWorkerStage(ctx context.Context, ec *ExecutionContext, op string) (StepResult, error) {
	if op == "transcribe" && getString(ec.Inputs, "cached_transcript_path") != "" {
		return e.podcastReuseTranscript(ctx, ec)
	}
	key := "podcast_" + op + "_job"
	id := getString(ec.State, key)
	wait := func(reason string) (StepResult, error) {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: reason, Outputs: ec.State}, nil
	}
	if id == "" {
		id = fmt.Sprintf("podcast-%s-%s-%d", ec.InstanceID, op, getInt(ec.State, "podcast_"+op+"_attempt"))
		ec.State[key] = id
		ec.State["job_id"] = id
		ec.State["external_reference"] = id
		if err := e.persistExecutionState(ctx, ec); err != nil {
			return StepResult{}, err
		}
	}
	st, err := e.deps.Transcode.Status(ctx, id)
	if err != nil {
		// Only a definitive 404 permits submission. Uncertain transport must
		// reconcile the persisted identity before any new remote side effect.
		he, ok := transcodeHTTPError(err)
		if !ok || he.StatusCode != 404 {
			if isRetryableWorkerPollError(err) {
				return wait("Podcast worker status unavailable; reconciling the existing job")
			}
			return StepResult{}, err
		}
		var policy podcast.Policy
		if err := decodePodcastValue(ec.State["podcast_policy"], &policy); err != nil {
			return StepResult{}, err
		}
		task := &podcast.Task{Version: podcast.Version, Operation: op, Language: policy.Language}
		container := "json"
		candidate := filepath.Join(filepath.Dir(getString(ec.State, "podcast_output")), ".navigatorr-"+id+".json")
		if op == "render" {
			s, err := loadPodcastSession(ec)
			if err != nil {
				return StepResult{}, err
			}
			if s.Cuts == nil {
				return StepResult{}, fmt.Errorf("cuts checkpoint missing")
			}
			if s.Policy.ReviewRequired && s.ApprovedDigest != podcast.Digest(s.Cuts) {
				return StepResult{}, fmt.Errorf("cuts approval is missing or stale")
			}
			task.Cuts = s.Cuts
			task.ASRJobID = getString(ec.State, "podcast_transcribe_job")
			candidate = getString(ec.State, "podcast_output")
			container = "mp3"
		}
		if _, err := e.deps.Fs.ResolveWrite(candidate); err != nil {
			return StepResult{}, err
		}
		plan := &transcode.Plan{Podcast: task, Container: container}
		plan.PlanDigest, _ = transcode.DigestPlan(plan)
		req := transcode.Request{ID: id, SourcePath: getString(ec.State, "resolved_path"), CandidatePath: candidate, Profile: "podcast-v1", Plan: plan, SourceSHA256: getString(ec.State, "source_sha256"), IdempotencyKey: id}
		ec.State["podcast_"+op+"_candidate"] = candidate
		ec.State["podcast_"+op+"_request"] = req
		if err := e.persistExecutionState(ctx, ec); err != nil {
			return StepResult{}, err
		}
		_, err = e.deps.Transcode.Submit(ctx, req)
		if err != nil && !isRetryableWorkerPollError(err) {
			return StepResult{}, err
		}
		return wait("Podcast " + op + " queued; the existing reconciler will follow it")
	}
	podcastSummary(ec)["phase"] = st.Phase
	ec.State["transcode_status"] = st.Status
	ec.State["transcode_phase"] = st.Phase
	ec.State["phase_costs"] = st.PhaseCosts
	switch st.Status {
	case transcode.StatusQueued, transcode.StatusRunning:
		return wait("Podcast " + op + " is " + st.Status)
	case transcode.StatusFailed, transcode.StatusCancelled:
		return StepResult{}, fmt.Errorf("podcast %s worker failed: %s", op, st.Error)
	case transcode.StatusCompleted:
		if st.Podcast == nil || st.Podcast.Operation != op || st.Podcast.SourceHash != "sha256:"+getString(ec.State, "source_sha256") {
			return StepResult{}, fmt.Errorf("worker podcast evidence missing or incorrect")
		}
		candidate := getString(ec.State, "podcast_"+op+"_candidate")
		if st.CandidatePath != candidate {
			return StepResult{}, fmt.Errorf("worker candidate path mismatch")
		}
		actual, size, err := e.deps.Fs.Hash(ctx, candidate)
		if err != nil {
			return StepResult{}, err
		}
		if strings.TrimPrefix(actual, "sha256:") != strings.TrimPrefix(st.CandidateSHA256, "sha256:") || size != st.CandidateSizeBytes {
			return StepResult{}, fmt.Errorf("worker output identity mismatch")
		}
		if op == "transcribe" {
			var t podcast.Transcript
			if err := podcast.ReadJSON(candidate, &t); err != nil {
				return StepResult{}, err
			}
			if podcast.Digest(t) != st.Podcast.TranscriptDigest || t.SourceHash != st.Podcast.SourceHash {
				return StepResult{}, fmt.Errorf("ASR artifact identity mismatch")
			}
			var p podcast.Policy
			if err := decodePodcastValue(ec.State["podcast_policy"], &p); err != nil {
				return StepResult{}, err
			}
			blocks, err := podcast.Blocks(t, p)
			if err != nil {
				return StepResult{}, err
			}
			// Recovery must not erase classifications already durably saved.
			if _, err := os.Stat(getString(ec.State, "podcast_session")); os.IsNotExist(err) {
				if err := savePodcastSession(ec, podcastSession{Version: podcast.Version, Policy: p, Transcript: t, Blocks: blocks, Classifications: map[string]podcast.Classification{}, Reads: map[string]map[int]bool{}}); err != nil {
					return StepResult{}, err
				}
			} else if err != nil {
				return StepResult{}, err
			} else {
				s, err := loadPodcastSession(ec)
				if err != nil || podcast.Digest(s.Transcript) != podcast.Digest(t) {
					return StepResult{}, fmt.Errorf("existing transcript checkpoint differs")
				}
			}
			m := podcastSummary(ec)
			m["phase"] = "awaiting_classification"
			m["transcript_digest"] = st.Podcast.TranscriptDigest
			m["total_blocks"] = len(blocks)
			m["classified_blocks"] = 0
			m["unit_count"] = len(t.Units)
			m["duration_ms"] = t.DurationMS
			m["asr_wall_seconds"] = t.WallSeconds
			m["realtime_factor"] = t.RealtimeFactor
		} else {
			if !st.Podcast.DecodePassed {
				return StepResult{}, fmt.Errorf("worker did not validate full audio decode")
			}
			ec.State["podcast_render_evidence"] = st.Podcast
			ec.State["podcast_output_sha256"] = actual
		}
		return StepResult{Status: StepCompleted, Outputs: ec.State}, nil
	default:
		return StepResult{}, fmt.Errorf("unknown podcast worker status")
	}
}
func (e *Engine) podcastClassification(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	s, err := loadPodcastSession(ec)
	if err != nil {
		return StepResult{}, err
	}
	m := podcastSummary(ec)
	m["classified_blocks"] = len(s.Classifications)
	m["total_blocks"] = len(s.Blocks)
	if ec.Decision != "plan" {
		m["phase"] = "awaiting_classification"
		return StepResult{Status: StepWaitingDecision, WaitingCondition: "podcast_classification", WaitingReason: "Orchestrating LLM: read every podcast_block page, classify every unit, then action_resume decision=plan", Outputs: ec.State}, nil
	}
	cuts, err := podcast.Plan(s.Transcript, s.Policy, s.Classifications)
	ec.Decision = ""
	if err != nil {
		m["classification_error"] = err.Error()
		return StepResult{Status: StepWaitingDecision, WaitingCondition: "podcast_classification", WaitingReason: err.Error(), Outputs: ec.State}, nil
	}
	s.Cuts = &cuts
	s.ApprovedDigest = ""
	s.ReviewReads = nil
	if err := savePodcastSession(ec, s); err != nil {
		return StepResult{}, err
	}
	if err := podcast.WriteJSON(filepath.Join(filepath.Dir(getString(ec.State, "podcast_session")), "cuts.json"), cuts); err != nil {
		return StepResult{}, err
	}
	delete(m, "classification_error")
	m["phase"] = "cuts_ready"
	m["coverage"] = 1
	m["cuts_digest"] = podcast.Digest(cuts)
	m["removed_ms"] = cuts.RemovedMS
	m["cut_count"] = len(cuts.Ranges)
	return StepResult{Status: StepCompleted, Outputs: ec.State}, nil
}
func (e *Engine) podcastReview(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	s, err := loadPodcastSession(ec)
	if err != nil {
		return StepResult{}, err
	}
	if s.Cuts == nil {
		return StepResult{}, fmt.Errorf("cuts missing")
	}
	if s.Policy.ReviewRequired && s.ApprovedDigest != podcast.Digest(s.Cuts) {
		return StepResult{Status: StepWaitingDecision, WaitingCondition: "podcast_review", WaitingReason: "Read podcast_review, inspect ID-derived boundaries, approve its exact digest to render a separate copy", Outputs: ec.State}, nil
	}
	return StepResult{Status: StepCompleted}, nil
}
func (e *Engine) podcastAccept(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	outputHash, _, err := e.deps.Fs.Hash(ctx, getString(ec.State, "podcast_output"))
	if err != nil || outputHash != getString(ec.State, "podcast_output_sha256") {
		return StepResult{}, fmt.Errorf("validated output changed before acceptance")
	}
	hash, _, err := e.deps.Fs.Hash(ctx, getString(ec.State, "resolved_path"))
	if err != nil {
		return StepResult{}, err
	}
	if strings.TrimPrefix(hash, "sha256:") != getString(ec.State, "source_sha256") {
		return StepResult{}, fmt.Errorf("original changed; output cannot be accepted for the feed")
	}
	s, err := loadPodcastSession(ec)
	if err != nil {
		return StepResult{}, err
	}
	var result podcast.Result
	if err := decodePodcastValue(ec.State["podcast_render_evidence"], &result); err != nil {
		return StepResult{}, err
	}
	if s.Cuts == nil || result.TranscriptDigest != podcast.Digest(s.Transcript) || result.RemovedMS != s.Cuts.RemovedMS {
		return StepResult{}, fmt.Errorf("render validation does not match reviewed cuts")
	}
	m := podcastSummary(ec)
	m["phase"] = "completed"
	m["published"] = true
	m["feed_ready"] = true
	m["output_sha256"] = ec.State["podcast_output_sha256"]
	m["output_duration_ms"] = result.OutputDurationMS
	m["decode_passed"] = true
	return StepResult{Status: StepCompleted, Outputs: ec.State}, nil
}
