package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestAutomaticPodcastPublishesWithoutLLMAndKeepsReviewPending(t *testing.T) {
	for _, hasMatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("matched_%t", hasMatch), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			source, out, cached := filepath.Join(dir, "source.mp3"), filepath.Join(dir, "clean.mp3"), filepath.Join(dir, "cached.json")
			original := []byte("immutable original")
			if err := os.WriteFile(source, original, 0600); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(original)
			tr := podcast.Transcript{SchemaVersion: 1, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", SourceHash: "sha256:" + hex.EncodeToString(hash[:]), DurationMS: 40000}
			for i := 0; i < 100; i++ {
				tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i+1), StartMS: int64(i * 400), EndMS: int64(i*400 + 350), Text: "episode word", Timing: "native_result"})
			}
			if err := podcast.WriteJSON(cached, tr); err != nil {
				t.Fatal(err)
			}
			ref := podcast.AdReference{ID: podcast.Digest("reference"), Digest: podcast.Digest("reference"), TextDigest: podcast.NativeAdTextDigest(tr.Units[3:28]), SourceHash: tr.SourceHash, CutsDigest: podcast.Digest("reviewed cuts"), DurationMS: 10000, Label: "paid_ad"}
			tc := &adTestExecutor{catalog: podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: "show", References: []podcast.AdReference{ref}}}
			statuses := map[string]transcode.JobStatus{"cached-asr": {ID: "cached-asr", Status: transcode.StatusCompleted, Podcast: &podcast.Result{Operation: "transcribe", SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr)}}}
			requests := map[string]transcode.Request{}
			submits := 0
			tc.mockTranscodeExecutor = &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
				return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, AutomaticKnownAds: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US", AdAlgorithm: podcast.AdAlgorithm}}, nil
			}, statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
				if st, ok := statuses[id]; ok {
					return st, nil
				}
				return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
			}, submitFunc: func(_ context.Context, r transcode.Request) (transcode.Job, error) {
				requests[r.ID] = r
				statuses[r.ID] = transcode.JobStatus{ID: r.ID, Status: transcode.StatusQueued}
				submits++
				return transcode.Job{}, &transcode.UncertainError{Op: "submit", Err: fmt.Errorf("lost ACK")}
			}}
			e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
			defer st.Close()
			e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"show": {Enabled: true, KnownAdsFirstPass: true}}}
			inputs := map[string]any{"path": source, "podcast_id": "show", "output_path": out, "cached_transcript_path": cached, "cached_transcript_digest": podcast.Digest(tr), "cached_asr_job_id": "cached-asr", "processing_mode": podcast.AnalysisKnownAdsOnly}
			for _, mode := range []string{"unknown", podcast.AnalysisKnownAdsOnly} {
				inputs["processing_mode"] = mode
				if mode == podcast.AnalysisKnownAdsOnly {
					e.deps.Config.Podcasts.Podcasts["show"] = config.PodcastSettings{Enabled: true}
				}
				if _, err := e.Run(ctx, "clean_podcast_ads", inputs); err == nil {
					t.Fatalf("invalid or unconfigured automatic mode %s admitted", mode)
				}
			}
			e.deps.Config.Podcasts.Podcasts["show"] = config.PodcastSettings{Enabled: true, KnownAdsFirstPass: true}
			r, err := e.Run(ctx, "clean_podcast_ads", inputs)
			if err != nil || r.Status != StatusWaitingExternal {
				t.Fatalf("run %+v %v", r, err)
			}
			matchID := getString(r.State, "podcast_match_ads_job")
			request := requests[matchID]
			report := podcast.AdMatchReport{Algorithm: podcast.AdAlgorithm, SourceHash: tr.SourceHash, Catalog: *request.Plan.Podcast.Catalog, DurationMS: tr.DurationMS}
			if hasMatch {
				report.Matches = []podcast.AdMatch{{ReferenceID: ref.ID, Label: ref.Label, StartMS: 1000, EndMS: 11000, MinCorrelation: .99}}
			}
			if err := podcast.WriteJSON(request.CandidatePath, report); err != nil {
				t.Fatal(err)
			}
			attest := func(file string) (string, int64) {
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				h := sha256.Sum256(raw)
				return hex.EncodeToString(h[:]), int64(len(raw))
			}
			digest, size := attest(request.CandidatePath)
			statuses[matchID] = transcode.JobStatus{ID: matchID, Status: transcode.StatusCompleted, CandidatePath: request.CandidatePath, CandidateSHA256: digest, CandidateSizeBytes: size, Podcast: &podcast.Result{Operation: "match_ads", SourceHash: tr.SourceHash, MatchDigest: podcast.Digest(report)}}
			e = NewEngine(e.Deps())
			r, err = e.Resume(ctx, r.ID, "", nil)
			if err != nil || r.Status != StatusWaitingExternal || submits != 2 {
				t.Fatalf("automatic pass waited for LLM or repeated ASR %+v %v submits=%d", r, err, submits)
			}
			ec := parseExecutionContextMust(t, e, r.ID)
			s, err := loadPodcastSession(ec)
			if err != nil || len(s.Classifications) != 0 || len(s.Reads) != 0 || s.ApprovedDigest != "" || s.Cuts.AnalysisMode != podcast.AnalysisKnownAdsOnly {
				t.Fatalf("automatic pass fabricated LLM evidence: %+v %v", s, err)
			}
			if hasMatch != (s.Cuts.RemovedMS > 0) {
				t.Fatal("known ad cuts differ")
			}
			renderID := getString(r.State, "podcast_render_job")
			render := requests[renderID]
			if render.Plan.Podcast.Automatic == nil || render.Plan.Podcast.Learning != nil || render.Plan.Podcast.MatchJobID != matchID || render.Plan.Podcast.ASRJobID != "cached-asr" {
				t.Fatal("automatic render evidence or ASR not preserved")
			}
			// Lost render ACK/restart keeps the same request. No read/classify/approve call.
			e = NewEngine(e.Deps())
			r, err = e.Resume(ctx, r.ID, "", nil)
			if err != nil || submits != 2 {
				t.Fatalf("duplicate render after restart: %v %d", err, submits)
			}
			if err := os.WriteFile(out, []byte("validated automatic audio"), 0600); err != nil {
				t.Fatal(err)
			}
			digest, size = attest(out)
			statuses[renderID] = transcode.JobStatus{ID: renderID, Status: transcode.StatusCompleted, CandidatePath: render.CandidatePath, CandidateSHA256: digest, CandidateSizeBytes: size, Podcast: &podcast.Result{Operation: "render", SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr), RemovedMS: s.Cuts.RemovedMS, OutputDurationMS: tr.DurationMS - s.Cuts.RemovedMS, DecodePassed: true}}
			r, err = e.Resume(ctx, r.ID, "", nil)
			if err != nil || r.Status != StatusCompleted {
				t.Fatalf("accept status=%s error=%s %v", r.Status, r.Error, err)
			}
			m := podcastSummary(parseExecutionContextMust(t, e, r.ID))
			if m["feed_ready"] != true || m["llm_reviewed"] != false || m["pending_llm"] != true || m["analysis_mode"] != podcast.AnalysisKnownAdsOnly || m["coverage"] == float64(1) {
				t.Fatalf("publication misrepresents LLM review: %+v", m)
			}
			if current, err := os.ReadFile(source); err != nil || string(current) != string(original) {
				t.Fatal("original changed")
			}
			// Explicit full revision borrows ASR but must wait for actual LLM coverage.
			inputs["processing_mode"] = podcast.AnalysisFull
			inputs["output_path"] = filepath.Join(dir, "reviewed.mp3")
			r, err = e.Run(ctx, "clean_podcast_ads", inputs)
			if err != nil {
				t.Fatal(err)
			}
			matchID = getString(r.State, "podcast_match_ads_job")
			request = requests[matchID]
			if err := podcast.WriteJSON(request.CandidatePath, report); err != nil {
				t.Fatal(err)
			}
			digest, size = attest(request.CandidatePath)
			statuses[matchID] = transcode.JobStatus{ID: matchID, Status: transcode.StatusCompleted, CandidatePath: request.CandidatePath, CandidateSHA256: digest, CandidateSizeBytes: size, Podcast: &podcast.Result{Operation: "match_ads", SourceHash: tr.SourceHash, MatchDigest: podcast.Digest(report)}}
			r, err = e.Resume(ctx, r.ID, "", nil)
			if err != nil || r.WaitingCondition != "podcast_classification" || getString(r.State, "podcast_transcribe_job") != "cached-asr" || submits != 3 {
				t.Fatalf("later review reused fake classification or submitted ASR: %+v %v submits=%d", r, err, submits)
			}
		})
	}
}
