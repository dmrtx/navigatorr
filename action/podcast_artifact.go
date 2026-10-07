package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

// Export complete pages without changing classifications, approvals or read receipts.
func (e *Engine) PodcastArtifact(ctx context.Context, id, artifact string, offset int) (map[string]any, error) {
	_, ec, release, err := e.podcastContext(ctx, id, false)
	if err != nil {
		return nil, err
	}
	defer release()
	s, err := loadPodcastSession(ec)
	if err != nil {
		return nil, err
	}
	rows := []any{}
	info := map[string]any{}
	artifactDigest := ""
	switch artifact {
	case "transcript":
		artifactDigest = podcast.Digest(s.Transcript)
		for _, u := range s.Transcript.Units {
			rows = append(rows, u)
		}
		t := s.Transcript
		t.Units = nil
		info["transcript"] = t
	case "classifications":
		artifactDigest = podcast.Digest(s.Classifications)
		for _, b := range s.Blocks {
			if c, ok := s.Classifications[b.ID]; ok {
				for _, d := range c.Decisions {
					rows = append(rows, map[string]any{"block_id": c.BlockID, "block_digest": c.BlockDigest, "transcript_digest": c.TranscriptDigest, "prompt_version": c.PromptVersion, "model": c.Model, "reasoning": c.Reasoning, "decision": d})
				}
			}
		}
	case "cuts":
		artifactDigest = podcast.Digest(s.Cuts)
		if s.Cuts == nil {
			return nil, fmt.Errorf("cuts are not planned yet")
		}
		for _, r := range s.Cuts.Ranges {
			rows = append(rows, r)
		}
		c := *s.Cuts
		c.Ranges = nil
		info["cuts"] = c
	default:
		return nil, fmt.Errorf("unsupported podcast artifact")
	}
	if offset < 0 || offset > len(rows) {
		return nil, fmt.Errorf("invalid artifact offset")
	}
	end := offset
	page := []any{}
	response := func() map[string]any {
		return map[string]any{"action_id": id, "feed_id": podcastSummary(ec)["feed_id"], "episode_id": podcastSummary(ec)["episode_id"], "artifact": artifact, "artifact_digest": artifactDigest, "source_hash": s.Transcript.SourceHash, "transcript_digest": podcast.Digest(s.Transcript), "asr_job_id": getString(ec.State, "podcast_transcribe_job"), "policy": s.Policy, "info": info, "rows": page, "offset": offset, "next_offset": end, "has_more": end < len(rows), "total_rows": len(rows), "untrusted_transcript": true, "timeline": "original_audio"}
	}
	for ; end < len(rows) && end < offset+80; end++ {
		page = append(page, rows[end])
		b, err := json.MarshalIndent(response(), "", "  ")
		if err != nil {
			return nil, err
		}
		if len(b) > 48*1024 {
			page = page[:len(page)-1]
			if end == offset {
				return nil, fmt.Errorf("artifact row exceeds response budget")
			}
			break
		}
	}
	return response(), nil
}

func (e *Engine) podcastReuseTranscript(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	file, err := e.deps.Fs.ResolveRead(getString(ec.Inputs, "cached_transcript_path"))
	if err != nil {
		return StepResult{}, err
	}
	var t podcast.Transcript
	if err = podcast.ReadJSON(file, &t); err != nil {
		return StepResult{}, err
	}
	if err = t.Validate(); err != nil {
		return StepResult{}, err
	}
	var policy podcast.Policy
	if err = decodePodcastValue(ec.State["podcast_policy"], &policy); err != nil {
		return StepResult{}, err
	}
	digest := podcast.Digest(t)
	if digest != getString(ec.Inputs, "cached_transcript_digest") || t.SourceHash != "sha256:"+getString(ec.State, "source_sha256") || !strings.EqualFold(strings.ReplaceAll(t.Language, "-", "_"), strings.ReplaceAll(policy.Language, "-", "_")) {
		return StepResult{}, fmt.Errorf("cached transcript source, language or digest differs")
	}
	jobID := getString(ec.Inputs, "cached_asr_job_id")
	st, err := e.deps.Transcode.Status(ctx, jobID)
	if err != nil {
		if isRetryableWorkerPollError(err) {
			return StepResult{Status: StepWaitingExternal, WaitingCondition: "worker_reconciling", WaitingReason: "Verifying existing ASR checkpoint; transcription will not be repeated", Outputs: ec.State}, nil
		}
		return StepResult{}, fmt.Errorf("cached ASR checkpoint is unavailable; refusing new transcription: %w", err)
	}
	if st.Status != transcode.StatusCompleted || st.Podcast == nil || st.Podcast.Operation != "transcribe" || st.Podcast.SourceHash != t.SourceHash || st.Podcast.TranscriptDigest != digest {
		return StepResult{}, fmt.Errorf("cached transcript lacks matching completed native ASR evidence")
	}
	blocks, err := podcast.Blocks(t, policy)
	if err != nil {
		return StepResult{}, err
	}
	if _, err = loadPodcastSession(ec); err != nil {
		if !os.IsNotExist(err) {
			return StepResult{}, err
		}
		s := podcastSession{Version: podcast.Version, Policy: policy, Transcript: t, Blocks: blocks, Classifications: map[string]podcast.Classification{}, Reads: map[string]map[int]bool{}}
		if err = attachKnownAds(ec, &s); err != nil {
			return StepResult{}, err
		}
		if err = savePodcastSession(ec, s); err != nil {
			return StepResult{}, err
		}
	}
	ec.State["podcast_transcribe_job"] = jobID
	m := podcastSummary(ec)
	m["phase"] = "awaiting_classification"
	m["transcript_digest"] = digest
	m["transcript_reused"] = true
	m["source_asr_wall_seconds"] = t.WallSeconds
	m["asr_wall_seconds"] = 0
	m["total_blocks"] = len(blocks)
	m["unit_count"] = len(t.Units)
	m["duration_ms"] = t.DurationMS
	return StepResult{Status: StepCompleted, Outputs: ec.State}, nil
}
