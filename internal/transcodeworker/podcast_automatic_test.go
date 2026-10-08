package transcodeworker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/podcast/acoustic"
	"github.com/jakenesler/navigatorr/transcode"
)

func automaticPodcastFixture(t *testing.T, categories ...string) (*Worker, string, podcast.Transcript, podcast.AdMatchReport, *podcast.Task) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg unavailable")
	}
	base := t.TempDir()
	source, tr := adCategoryDedupFixture(t, base)
	cfg := DefaultWorkerConfig()
	cfg.StateDir, cfg.LocalWorkDir = filepath.Join(base, "jobs"), filepath.Join(base, "work")
	cfg.AllowedRoots, cfg.ExternalRoots, cfg.StagingPolicy = []string{base}, nil, StagingPolicy("never")
	cfg.AppleSpeechPath = "/native-asr-must-not-run"
	w := NewWorker(cfg)
	w.SetTranscodeSpawner(func(string, string, string) (int, string, error) { return os.Getpid(), "", nil })
	w.SetAliveFunc(func(*JobRecord) bool { return true })
	pcm, err := w.decodeAdPCM(t.Context(), t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := acoustic.ReadSamples(pcm, 0, int(tr.Units[23].EndMS*acoustic.Rate/1000))
	pcm.Close()
	os.Remove(pcm.Name())
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
	}
	for _, label := range categories {
		recording := adRecording{Algorithm: podcast.AdAlgorithm, Scope: "show", Label: label, SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr), CutsDigest: podcast.Digest("reviewed native seed"), ClassificationDigest: podcast.Digest("reviewed native labels"), FirstID: tr.Units[0].ID, LastID: tr.Units[23].ID, StartMS: 0, EndMS: tr.Units[23].EndMS, PCM: raw, TextDigest: podcast.NativeAdTextDigest(tr.Units[:24])}
		ref := recordingReference(recording)
		path := adFile(w.adDir("show"), ref.ID)
		if err := podcast.WriteJSON(path, recording); err != nil {
			t.Fatal(err)
		}
		if err := podcast.WriteJSON(path+".meta", ref); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := w.adCatalog("show")
	if err != nil {
		t.Fatal(err)
	}
	report, err := w.matchAds(t.Context(), t.TempDir(), source, tr.SourceHash, catalog)
	if err != nil {
		t.Fatal(err)
	}
	matchDir := filepath.Join(cfg.StateDir, "matching")
	if err := podcast.WriteJSON(filepath.Join(matchDir, "podcast-matches.json"), report); err != nil {
		t.Fatal(err)
	}
	if err := SaveJobAtomic(filepath.Join(matchDir, "job.json"), &JobRecord{ID: "matching", Status: "completed", SourceSHA256: strings.TrimPrefix(tr.SourceHash, "sha256:"), Plan: &transcode.Plan{Podcast: &podcast.Task{Version: 1, Operation: "match_ads", Language: "en_US", Catalog: &catalog}}, Podcast: &podcast.Result{Operation: "match_ads", SourceHash: tr.SourceHash, MatchDigest: podcast.Digest(report)}}); err != nil {
		t.Fatal(err)
	}
	asrDir := filepath.Join(cfg.StateDir, "native")
	if err := podcast.WriteJSON(filepath.Join(asrDir, "podcast-transcript.json"), tr); err != nil {
		t.Fatal(err)
	}
	if err := SaveJobAtomic(filepath.Join(asrDir, "job.json"), &JobRecord{ID: "native", Status: "completed", SourceSHA256: strings.TrimPrefix(tr.SourceHash, "sha256:"), Plan: &transcode.Plan{Podcast: &podcast.Task{Version: 1, Operation: "transcribe", Language: "en_US"}}, Podcast: &podcast.Result{Operation: "transcribe", SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr)}}); err != nil {
		t.Fatal(err)
	}
	p := podcast.DefaultPolicy()
	p.KnownAdsFirstPass, p.MaxRemovedFraction = true, .5
	cuts, err := podcast.PlanKnownAds(tr, p, report)
	if err != nil {
		t.Fatal(err)
	}
	task := &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: "native", MatchJobID: "matching", Cuts: &cuts, Automatic: &podcast.AdAutomatic{Scope: "show", Policy: p}}
	return w, source, tr, report, task
}

func TestAutomaticKnownAdsTaskContract(t *testing.T) {
	_, _, _, _, original := automaticPodcastFixture(t, "paid_ad")
	if err := original.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*podcast.Task)
	}{
		{"missing_match", func(v *podcast.Task) { v.MatchJobID = "" }},
		{"missing_automatic", func(v *podcast.Task) { v.Automatic = nil }},
		{"mixed_learning", func(v *podcast.Task) { v.Learning = &podcast.AdLearning{} }},
		{"wrong_mode", func(v *podcast.Task) { v.Cuts.AnalysisMode = "" }},
		{"unknown_mode", func(v *podcast.Task) { v.Cuts.AnalysisMode = "invented" }},
		{"wrong_policy", func(v *podcast.Task) { v.Cuts.PolicyDigest = podcast.Digest("other policy") }},
		{"disabled_first_pass", func(v *podcast.Task) { v.Automatic.Policy.KnownAdsFirstPass = false }},
		{"missing_scope", func(v *podcast.Task) { v.Automatic.Scope = "" }},
		{"wrong_language", func(v *podcast.Task) { v.Language = "es_ES" }},
		{"transcribe_proof", func(v *podcast.Task) { v.Operation = "transcribe" }},
		{"match_proof", func(v *podcast.Task) { v.Operation = "match_ads"; v.Catalog = &podcast.AdCatalog{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *original
			cuts, automatic := *original.Cuts, *original.Automatic
			copy.Cuts, copy.Automatic = &cuts, &automatic
			tc.mutate(&copy)
			if err := copy.Validate(); err == nil {
				t.Fatal("invalid automatic task accepted")
			}
		})
	}
}

func TestAutomaticKnownAdsRecomputesExactCuts(t *testing.T) {
	w, _, tr, report, task := automaticPodcastFixture(t, "paid_ad")
	if err := w.verifyAdDecisionEvidence(task, tr, tr.SourceHash); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*podcast.Task)
	}{
		{"omit_known_cut", func(v *podcast.Task) {
			v.Cuts.Ranges = v.Cuts.Ranges[1:]
			v.Cuts.RemovedMS = v.Cuts.Ranges[0].EndMS - v.Cuts.Ranges[0].StartMS
		}},
		{"cut_unknown_boundary", func(v *podcast.Task) {
			v.Cuts.RemovedMS += v.Cuts.Ranges[0].StartMS
			v.Cuts.Ranges[0].FirstID = tr.Units[0].ID
			v.Cuts.Ranges[0].StartMS = 0
		}},
		{"wrong_scope", func(v *podcast.Task) { v.Automatic.Scope = "another-show" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *task
			cuts, automatic := *task.Cuts, *task.Automatic
			cuts.Ranges = append([]podcast.Cut{}, cuts.Ranges...)
			copy.Cuts, copy.Automatic = &cuts, &automatic
			tc.mutate(&copy)
			if _, err := automaticAdReferences(&copy, tr, &report); err == nil {
				t.Fatal("cuts not derived from exact native evidence accepted")
			}
		})
	}
	tr.Units[5].Text = "changed offer hidden under the same music"
	if _, err := automaticAdReferences(task, tr, &report); err == nil {
		t.Fatal("changed native text accepted")
	}
}

func TestAutomaticKnownAdsRenderPreservesUnknownAndNeverLearns(t *testing.T) {
	for _, tc := range []struct {
		name       string
		categories []string
		wantCuts   bool
	}{
		{"known", []string{"paid_ad"}, true},
		{"empty_library", nil, false},
		{"conflicting_categories", []string{"paid_ad", "cross_promo"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, source, tr, _, task := automaticPodcastFixture(t, tc.categories...)
			if (len(task.Cuts.Ranges) > 0) != tc.wantCuts || task.Learning != nil || !task.Automatic.Policy.ReviewRequired {
				t.Fatal("fixture must have real known-only cuts and no fabricated review")
			}
			before, err := w.adCatalog("show")
			if err != nil {
				t.Fatal(err)
			}
			plan := &transcode.Plan{Container: "mp3", Podcast: task}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			req := SubmitRequest{ID: "automatic-render", SourcePath: source, CandidatePath: filepath.Join(filepath.Dir(source), "automatic.mp3"), SourceSHA256: strings.TrimPrefix(tr.SourceHash, "sha256:"), Profile: "podcast-v1", Plan: plan}
			if _, err := w.Submit(t.Context(), req, "unused", "unused"); err != nil {
				t.Fatal(err)
			}
			if err := w.InternalRun(t.Context(), req.ID); err != nil {
				t.Fatal(err)
			}
			status, err := w.Status(t.Context(), req.ID)
			if err != nil || status.Status != "completed" || status.Podcast == nil || !status.Podcast.DecodePassed || status.Podcast.LearnedAds != 0 || status.Podcast.RemovedMS != task.Cuts.RemovedMS {
				t.Fatalf("automatic render: %+v %v", status, err)
			}
			after, err := w.adCatalog("show")
			if err != nil || podcast.Digest(before) != podcast.Digest(after) {
				t.Fatal("automatic rendering changed the library", err)
			}
			data, err := os.ReadFile(req.CandidatePath)
			if err != nil {
				t.Fatal(err)
			}
			prefix := []byte("\x00application/json\x00navigatorr-transcript.json\x00Navigatorr original-audio transcript\x00")
			at := bytes.Index(data, prefix)
			if at < 0 {
				t.Fatal("missing embedded native transcript")
			}
			var embedded podcast.EmbeddedTranscript
			if err := json.NewDecoder(bytes.NewReader(data[at+len(prefix):])).Decode(&embedded); err != nil {
				t.Fatal(err)
			}
			if embedded.Cuts.AnalysisMode != podcast.AnalysisKnownAdsOnly || podcast.Digest(embedded.Transcript) != podcast.Digest(tr) || podcast.Digest(embedded.Cuts) != podcast.Digest(task.Cuts) {
				t.Fatal("automatic provenance/native transcript changed")
			}
			if hash, err := hashLocalFileSHA256(t.Context(), source); err != nil || hash != strings.TrimPrefix(tr.SourceHash, "sha256:") {
				t.Fatal("original changed", err)
			}
		})
	}
}

func TestAutomaticKnownAdsRevocationBlocksPublicationAndRecovery(t *testing.T) {
	for _, state := range []FinalizationState{FinalizationStateNotRequired, FinalizationStateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			w, source, tr, report, task := automaticPodcastFixture(t, "paid_ad")
			candidate := filepath.Join(filepath.Dir(source), "validated.mp3")
			if err := os.WriteFile(candidate, []byte("private validated automatic candidate"), 0600); err != nil {
				t.Fatal(err)
			}
			hash, _ := hashLocalFileSHA256(t.Context(), candidate)
			info, _ := os.Stat(candidate)
			job := &JobRecord{ID: "automatic-recovery", Status: "running", SourceSHA256: strings.TrimPrefix(tr.SourceHash, "sha256:"), Candidate: candidate, Plan: &transcode.Plan{Podcast: task}, EncodeComplete: true, CandidateSHA256: hash, CandidateSizeBytes: info.Size(), FinalizationState: string(state)}
			dir := filepath.Join(w.cfg.StateDir, job.ID)
			file := filepath.Join(dir, "job.json")
			if err := SaveJobAtomic(file, job); err != nil {
				t.Fatal(err)
			}
			if err := w.revokeAd("show", report.Matches[0].ReferenceID); err != nil {
				t.Fatal(err)
			}
			published := false
			if err := w.guardPodcastAdPublication(t.Context(), dir, file, func() error { published = true; return nil }); err == nil || published {
				t.Fatal("revoked automatic evidence published")
			}
			_ = w.finalizeOperational(t.Context(), dir, file, job, &resolvedOperational{localCandidate: candidate, destination: candidate, finalization: state})
			got, err := LoadJob(file)
			if err != nil || got.Status == "completed" || !strings.Contains(got.Error, "revoked") {
				t.Fatalf("automatic recovery bypassed revoke: %+v %v", got, err)
			}
		})
	}
}

func TestAutomaticKnownAdsRecoveryRejectsChangedNativeASR(t *testing.T) {
	w, _, tr, _, task := automaticPodcastFixture(t, "paid_ad")
	job := &JobRecord{SourceSHA256: strings.TrimPrefix(tr.SourceHash, "sha256:"), Plan: &transcode.Plan{Podcast: task}}
	tr.Units[5].Text = "modified native checkpoint"
	if err := podcast.WriteJSON(filepath.Join(w.cfg.StateDir, "native", "podcast-transcript.json"), tr); err != nil {
		t.Fatal(err)
	}
	if release, err := w.lockAdEvidence(job); err == nil {
		release()
		t.Fatal("changed native ASR evidence admitted during automatic recovery")
	}
}
