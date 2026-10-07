package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestPodcastCachedTranscriptReuseAndGuards(t *testing.T) {
	for _, mutation := range []string{"valid", "changed-text", "wrong-source", "wrong-language", "missing-worker", "wrong-worker-digest"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "original.mp3")
			os.WriteFile(source, []byte("original bytes"), 0600)
			submissions := 0
			var trusted podcast.Transcript
			tc := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
				return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US"}}, nil
			}, submitFunc: func(context.Context, transcode.Request) (transcode.Job, error) {
				submissions++
				return transcode.Job{}, fmt.Errorf("ASR must not be submitted")
			}, statusFunc: func(context.Context, string) (transcode.JobStatus, error) {
				if mutation == "missing-worker" {
					return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
				}
				digest := podcast.Digest(trusted)
				if mutation == "wrong-worker-digest" {
					digest = "sha256:" + strings.Repeat("b", 64)
				}
				return transcode.JobStatus{Status: transcode.StatusCompleted, Podcast: &podcast.Result{Operation: "transcribe", SourceHash: trusted.SourceHash, TranscriptDigest: digest}}, nil
			}}
			e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
			defer st.Close()
			e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "sessions"), Podcasts: map[string]config.PodcastSettings{"show": {Enabled: true, Remove: []string{"paid_ad", "cross_promo"}}}}
			h, _, err := e.deps.Fs.Hash(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			trusted = podcast.Transcript{SchemaVersion: 1, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", SourceHash: "sha256:" + strings.TrimPrefix(h, "sha256:"), DurationMS: 60000, Units: []podcast.Unit{{ID: "u1", StartMS: 0, EndMS: 1000, Text: "another podcast trailer", Timing: "native_result"}}}
			if mutation == "valid" {
				trusted.Units = nil
				for i := 0; i < 120; i++ {
					trusted.Units = append(trusted.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i+1), StartMS: int64(i * 100), EndMS: int64(i*100 + 80), Text: "native word", Timing: "native_result"})
				}
			}
			cached := trusted
			cached.Units = append([]podcast.Unit{}, trusted.Units...)
			switch mutation {
			case "changed-text":
				cached.Units[0].Text = "fabricated"
			case "wrong-source":
				cached.SourceHash = "sha256:" + strings.Repeat("a", 64)
			case "wrong-language":
				cached.Language = "es_ES"
			}
			file := filepath.Join(dir, "transcript.json")
			podcast.WriteJSON(file, cached)
			r, err := e.Run(context.Background(), "clean_podcast_ads", map[string]any{"path": source, "output_path": filepath.Join(dir, "clean.mp3"), "podcast_id": "show", "cached_transcript_path": file, "cached_transcript_digest": podcast.Digest(trusted), "cached_asr_job_id": "old-asr"})
			if submissions != 0 {
				t.Fatalf("ASR resubmitted %d", submissions)
			}
			if mutation != "valid" {
				if err == nil && r.Status != StatusFailed {
					t.Fatalf("mutation accepted %+v", r)
				}
				return
			}
			if err != nil || r.WaitingCondition != "podcast_classification" {
				t.Fatalf("reuse %+v %v", r, err)
			}
			ec := parseExecutionContextMust(t, e, r.ID)
			s, err := loadPodcastSession(ec)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Classifications) != 0 || len(s.Reads) != 0 || s.Cuts != nil || s.ApprovedDigest != "" || getString(ec.State, "podcast_transcribe_job") != "old-asr" || getString(ec.State, "job_id") != "" {
				t.Fatal("reanalysis inherited decisions or owns borrowed ASR")
			}
			if podcastSummary(ec)["transcript_reused"] != true {
				t.Fatal("missing reuse evidence")
			}
			page, err := e.PodcastArtifact(context.Background(), r.ID, "transcript", 0)
			if err != nil || page["total_rows"] != 120 {
				t.Fatalf("export %+v %v", page, err)
			}
			if page["next_offset"] != 80 || page["has_more"] != true {
				t.Fatal("first page incomplete")
			}
			page2, err := e.PodcastArtifact(context.Background(), r.ID, "transcript", 80)
			if err != nil || page2["next_offset"] != 120 || page2["has_more"] != false || page2["artifact_digest"] != page["artifact_digest"] {
				t.Fatalf("second page %+v %v", page2, err)
			}
			s, _ = loadPodcastSession(parseExecutionContextMust(t, e, r.ID))
			if len(s.Reads) != 0 {
				t.Fatal("artifact export granted classification coverage")
			}
			e = NewEngine(e.Deps())
			r, err = e.Resume(context.Background(), r.ID, "", nil)
			if err != nil || submissions != 0 || r.WaitingCondition != "podcast_classification" {
				t.Fatalf("restart %+v %v", r, err)
			}
		})
	}
}
