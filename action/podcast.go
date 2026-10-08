package action

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

type podcastSession struct {
	Matches         *podcast.AdMatchReport            `json:"matches,omitempty"`
	Known           map[string]podcast.AdKnownUnit    `json:"known,omitempty"`
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
		Description:    "Durable podcast cleaning. Original preserved. ASR runs on the existing worker queue. Default processing_mode=full: the orchestrating LLM reads EVERY podcast_block page, submits ID-based labels with podcast_classify, resumes decision=plan, and reviews exact cuts when required. processing_mode=known_ads_only automatically renders only verified acoustic/native-text matches without LLM, leaves unknown audio intact and publishes with llm_reviewed=false. Use a full revision from the preserved original and cached transcript for later review. Output is a validated, separate MP3 for the existing feed/download integration.",
		RequiredInputs: []string{"path", "podcast_id", "output_path"}, OptionalInputs: []string{"episode_id", "feed_id", "idempotency_key", "source_sha256", "cached_transcript_path", "cached_transcript_digest", "cached_asr_job_id", "processing_mode"},
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
	if policy.KnownAdsFirstPass {
		if _, ok := e.deps.Transcode.(transcode.PodcastAdExecutor); !ok || caps.Podcast == nil || caps.Podcast.AdAlgorithm != podcast.AdAlgorithm {
			return StepResult{}, fmt.Errorf("worker does not support the configured known-ad first pass")
		}
	}
	if podcastAnalysisMode(ec) == podcast.AnalysisKnownAdsOnly && (caps.Podcast == nil || !caps.Podcast.AutomaticKnownAds) {
		return StepResult{}, fmt.Errorf("worker does not support automatic known-ad publication; upgrade the worker")
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
	summary["analysis_mode"] = podcastAnalysisMode(ec)
	summary["llm_reviewed"] = false
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
func podcastAnalysisMode(ec *ExecutionContext) string {
	if getString(ec.Inputs, "processing_mode") == podcast.AnalysisKnownAdsOnly {
		return podcast.AnalysisKnownAdsOnly
	}
	return podcast.AnalysisFull
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
	if err == nil && s.Policy.KnownAdsFirstPass {
		known, e := podcast.KnownAdUnits(s.Transcript, valueAdReport(s.Matches), s.Policy)
		// An empty map is omitted in the checkpoint and decodes as nil. Compare
		// actual unit evidence so a no-match pass survives serialization/restart.
		if e != nil || s.Matches == nil || s.Matches.Catalog.Scope != getString(ec.Inputs, "podcast_id") || podcast.Digest(s.Matches) != podcastSummary(ec)["match_digest"] || !maps.Equal(known, s.Known) {
			err = fmt.Errorf("acoustic checkpoint evidence changed")
		}
	}
	if err == nil && s.Cuts != nil && podcastAnalysisMode(ec) == podcast.AnalysisKnownAdsOnly {
		cuts, e := podcast.PlanKnownAds(s.Transcript, s.Policy, valueAdReport(s.Matches))
		if e != nil || podcast.Digest(s.Cuts) != podcast.Digest(cuts) || s.ApprovedDigest != "" {
			err = fmt.Errorf("automatic cut checkpoint evidence changed")
		}
	}
	return s, err
}
func savePodcastSession(ec *ExecutionContext, s podcastSession) error {
	return podcast.WriteJSON(getString(ec.State, "podcast_session"), s)
}
func (e *Engine) podcastWorkerStage(ctx context.Context, ec *ExecutionContext, op string) (StepResult, error) {
	if op == "transcribe" {
		var p podcast.Policy
		if err := decodePodcastValue(ec.State["podcast_policy"], &p); err != nil {
			return StepResult{}, err
		}
		if p.KnownAdsFirstPass && getString(ec.State, "podcast_match_path") == "" {
			r, err := e.podcastWorkerStage(ctx, ec, "match_ads")
			if err != nil || r.Status != StepCompleted {
				return r, err
			}
		}
	}
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
	if getString(ec.State, "job_id") != id {
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
		if op == "match_ads" {
			var catalog podcast.AdCatalog
			if raw := ec.State["podcast_ad_catalog"]; raw != nil {
				if err := decodePodcastValue(raw, &catalog); err != nil {
					return StepResult{}, err
				}
			} else {
				catalog, err = e.PodcastAdLibrary(ctx, getString(ec.Inputs, "podcast_id"), "")
				if err != nil {
					if isRetryableWorkerPollError(err) {
						return wait("Waiting for known-ad catalog")
					}
					return StepResult{}, err
				}
				ec.State["podcast_ad_catalog"] = catalog
			}
			task.Catalog = &catalog
		}
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
			automatic := podcastAnalysisMode(ec) == podcast.AnalysisKnownAdsOnly
			if !automatic && s.Policy.ReviewRequired && s.ApprovedDigest != podcast.Digest(s.Cuts) {
				return StepResult{}, fmt.Errorf("cuts approval is missing or stale")
			}
			if err := e.checkKnownAds(ctx, s); err != nil {
				return StepResult{}, err
			}
			if automatic {
				task.Automatic = &podcast.AdAutomatic{Scope: getString(ec.Inputs, "podcast_id"), Policy: s.Policy}
				task.MatchJobID = getString(ec.State, "podcast_match_ads_job")
			} else if s.Policy.KnownAdsFirstPass {
				task.Learning = &podcast.AdLearning{Scope: getString(ec.Inputs, "podcast_id"), Policy: s.Policy, Classifications: s.Classifications, ApprovedDigest: s.ApprovedDigest}
				task.MatchJobID = getString(ec.State, "podcast_match_ads_job")
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
		if op == "match_ads" {
			var report podcast.AdMatchReport
			if err := podcast.ReadJSON(candidate, &report); err != nil {
				return StepResult{}, err
			}
			var catalog podcast.AdCatalog
			if err := decodePodcastValue(ec.State["podcast_ad_catalog"], &catalog); err != nil {
				return StepResult{}, err
			}
			if err := report.Validate(); err != nil {
				return StepResult{}, err
			}
			if report.SourceHash != st.Podcast.SourceHash || podcast.Digest(report) != st.Podcast.MatchDigest || podcast.Digest(report.Catalog) != podcast.Digest(catalog) {
				return StepResult{}, fmt.Errorf("known-ad report identity differs")
			}
			path := filepath.Join(filepath.Dir(getString(ec.State, "podcast_session")), "matches.json")
			if err := podcast.WriteJSON(path, report); err != nil {
				return StepResult{}, err
			}
			ec.State["podcast_match_path"] = path
			podcastSummary(ec)["match_digest"] = podcast.Digest(report)
			podcastSummary(ec)["known_ad_matches"] = len(report.Matches)
			podcastSummary(ec)["first_pass_wall_seconds"] = report.WallSeconds
			podcastSummary(ec)["ad_catalog_digest"] = podcast.Digest(catalog)
		} else if op == "transcribe" {
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
				s := podcastSession{Version: podcast.Version, Policy: p, Transcript: t, Blocks: blocks, Classifications: map[string]podcast.Classification{}, Reads: map[string]map[int]bool{}}
				if err := attachKnownAds(ec, &s); err != nil {
					return StepResult{}, err
				}
				if err := savePodcastSession(ec, s); err != nil {
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
			s, err := loadPodcastSession(ec)
			if err != nil {
				return StepResult{}, err
			}
			if err := e.checkKnownAds(ctx, s); err != nil {
				return StepResult{}, err
			}
			if !st.Podcast.DecodePassed {
				return StepResult{}, fmt.Errorf("worker did not validate full audio decode")
			}
			ec.State["podcast_render_evidence"] = st.Podcast
			ec.State["podcast_output_sha256"] = actual
			podcastSummary(ec)["learned_ads"] = st.Podcast.LearnedAds
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
	if err := e.checkKnownAds(ctx, s); err != nil {
		return StepResult{}, err
	}
	if podcastAnalysisMode(ec) == podcast.AnalysisKnownAdsOnly {
		cuts, err := podcast.PlanKnownAds(s.Transcript, s.Policy, valueAdReport(s.Matches))
		if err != nil {
			return StepResult{}, err
		}
		s.Cuts, s.ApprovedDigest, s.ReviewReads = &cuts, "", nil
		if err := savePodcastSession(ec, s); err != nil {
			return StepResult{}, err
		}
		if err := podcast.WriteJSON(filepath.Join(filepath.Dir(getString(ec.State, "podcast_session")), "cuts.json"), cuts); err != nil {
			return StepResult{}, err
		}
		m["phase"] = "automatic_cuts_ready"
		m["coverage"] = float64(len(s.Known)) / float64(len(s.Transcript.Units))
		m["cuts_digest"] = podcast.Digest(cuts)
		m["removed_ms"] = cuts.RemovedMS
		m["cut_count"] = len(cuts.Ranges)
		return StepResult{Status: StepCompleted, Outputs: ec.State}, nil
	}
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
	if podcastAnalysisMode(ec) != podcast.AnalysisKnownAdsOnly && s.Policy.ReviewRequired && s.ApprovedDigest != podcast.Digest(s.Cuts) {
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
	if err := e.checkKnownAds(ctx, s); err != nil {
		return StepResult{}, err
	}
	var result podcast.Result
	if err := decodePodcastValue(ec.State["podcast_render_evidence"], &result); err != nil {
		return StepResult{}, err
	}
	if s.Cuts == nil || result.TranscriptDigest != podcast.Digest(s.Transcript) || result.RemovedMS != s.Cuts.RemovedMS {
		return StepResult{}, fmt.Errorf("render validation does not match reviewed cuts")
	}
	if s.Policy.KnownAdsFirstPass && podcastAnalysisMode(ec) == podcast.AnalysisFull {
		// Rendering learned approved recordings. Refresh the consumer's durable mirror
		// before accepting this review, so the next download needs no worker.
		if _, err := e.PodcastAdLibrary(ctx, getString(ec.Inputs, "podcast_id"), ""); err != nil {
			if isRetryableWorkerPollError(err) {
				return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: "Waiting to synchronize learned ad references"}, nil
			}
			return StepResult{}, err
		}
	}
	m := podcastSummary(ec)
	m["phase"] = "completed"
	m["published"] = true
	m["feed_ready"] = true
	m["output_sha256"] = ec.State["podcast_output_sha256"]
	m["output_duration_ms"] = result.OutputDurationMS
	m["decode_passed"] = true
	m["analysis_mode"] = podcastAnalysisMode(ec)
	m["llm_reviewed"] = podcastAnalysisMode(ec) == podcast.AnalysisFull
	m["pending_llm"] = podcastAnalysisMode(ec) == podcast.AnalysisKnownAdsOnly
	return StepResult{Status: StepCompleted, Outputs: ec.State}, nil
}
