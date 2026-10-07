package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestRealAdLibraryBenchmark(t *testing.T) {
	file := os.Getenv("NAV_AD_LIBRARY_BENCHMARK")
	if file == "" {
		t.Skip("opt-in confirmed native transcript/audio benchmark")
	}
	var fixture struct {
		Seed struct {
			Audio           string
			Transcript      podcast.Transcript
			Classifications []podcast.Classification
		}
		Cases []struct {
			Name          string
			Audio         string
			Transcript    podcast.Transcript
			Labels        map[string]string
			MinKnownUnits int
		}
	}
	if err := podcast.ReadJSON(file, &fixture); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultWorkerConfig()
	cfg.StateDir = t.TempDir()
	w := NewWorker(cfg)
	ctx := t.Context()
	tr := fixture.Seed.Transcript
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
	sourceHash, err := hashLocalFileSHA256(ctx, fixture.Seed.Audio)
	if err != nil || tr.SourceHash != "sha256:"+sourceHash {
		t.Fatal("seed audio identity differs")
	}
	p := podcast.DefaultPolicy()
	p.KnownAdsFirstPass = true
	p.Remove = []string{"paid_ad", "cross_promo"}
	blocks, _ := podcast.Blocks(tr, p)
	classes := map[string]podcast.Classification{}
	for i, c := range fixture.Seed.Classifications {
		if c.BlockID != blocks[i].ID {
			t.Fatal("seed blocks changed")
		}
		c.BlockDigest = blocks[i].Digest
		c.TranscriptDigest = podcast.Digest(tr)
		classes[c.BlockID] = c
	}
	cuts, err := podcast.Plan(tr, p, classes)
	if err != nil {
		t.Fatal(err)
	}
	task := &podcast.Task{Version: 1, Operation: "render", Language: "en_US", Cuts: &cuts, Learning: &podcast.AdLearning{Scope: "benchmark-generation-why", Policy: p, Classifications: classes, ApprovedDigest: podcast.Digest(cuts)}}
	jobDir := filepath.Join(cfg.StateDir, "seed")
	jobFile := filepath.Join(jobDir, "job.json")
	os.MkdirAll(jobDir, 0700)
	if err := SaveJobAtomic(jobFile, &JobRecord{ID: "seed", Status: "running", Plan: &transcode.Plan{Podcast: task}}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	learned, err := w.learnAds(ctx, jobDir, jobFile, fixture.Seed.Audio, tr, task)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := w.adCatalog(task.Learning.Scope)
	if err != nil {
		t.Fatal(err)
	}
	results := []map[string]any{}
	for _, c := range fixture.Cases {
		if err := c.Transcript.Validate(); err != nil {
			t.Fatal(err)
		}
		sha, err := hashLocalFileSHA256(ctx, c.Audio)
		if err != nil || c.Transcript.SourceHash != "sha256:"+sha {
			t.Fatal("case audio identity differs")
		}
		caseDir := t.TempDir()
		report, err := w.matchAds(ctx, caseDir, c.Audio, c.Transcript.SourceHash, catalog)
		if err != nil {
			t.Fatal(err)
		}
		known, err := podcast.KnownAdUnits(c.Transcript, report, p)
		if err != nil {
			t.Fatal(err)
		}
		var knownMS, falseMS int64
		for _, u := range c.Transcript.Units {
			if k, ok := known[u.ID]; ok {
				knownMS += u.EndMS - u.StartMS
				if c.Labels[u.ID] != k.Label {
					falseMS += u.EndMS - u.StartMS
				}
			}
		}
		if falseMS != 0 {
			t.Fatalf("%s false-labelled content/category: %dms", c.Name, falseMS)
		}
		if len(known) < c.MinKnownUnits {
			t.Fatalf("%s known=%d expected minimum %d", c.Name, len(known), c.MinKnownUnits)
		}
		result := map[string]any{"episode": c.Name, "duration_ms": c.Transcript.DurationMS, "candidate_matches": len(report.Matches), "known_units": len(known), "known_ms": knownMS, "false_label_ms": falseMS, "wall_seconds": report.WallSeconds, "decode_seconds": report.DecodeSeconds, "search_seconds": report.SearchSeconds, "matches": report.Matches}
		results = append(results, result)
		raw, _ := json.Marshal(result)
		t.Log(string(raw))
	}
	out := map[string]any{"algorithm": podcast.AdAlgorithm, "learned_ads": learned, "library_references": len(catalog.References), "total_wall_seconds": time.Since(started).Seconds(), "cases": results}
	if destination := os.Getenv("NAV_AD_LIBRARY_BENCHMARK_RESULT"); destination != "" {
		if err := podcast.WriteJSON(destination, out); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdLibraryQueueLearningMatchingScopeAndRevocation(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "original.mp3")
	if b, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "anoisesrc=color=pink:amplitude=0.2:sample_rate=44100:duration=40:seed=13", "-c:a", "libmp3lame", "-q:a", "2", source).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s %v", b, err)
	}
	cfg := DefaultWorkerConfig()
	cfg.StateDir = filepath.Join(dir, "jobs")
	cfg.LocalWorkDir = filepath.Join(dir, "work")
	cfg.AllowedRoots = []string{dir}
	cfg.ExternalRoots = nil
	cfg.StagingPolicy = StagingPolicy("never")
	cfg.AppleSpeechPath = "/must-not-run"
	w := NewWorker(cfg)
	w.SetTranscodeSpawner(func(string, string, string) (int, string, error) { return os.Getpid(), "", nil })
	w.SetAliveFunc(func(*JobRecord) bool { return true })
	ctx := t.Context()
	hash, err := hashLocalFileSHA256(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	duration, err := w.podcastDuration(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	tr := podcast.Transcript{SchemaVersion: 1, SourceHash: "sha256:" + hash, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", DurationMS: duration}
	for i := 0; i < 80; i++ {
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i+1), StartMS: int64(i * 500), EndMS: int64(i*500 + 480), Text: "native spoken unit", Timing: "native_result"})
	}
	run := func(id string, task *podcast.Task) JobStatusResponse {
		t.Helper()
		ext := "json"
		if task.Operation == "render" {
			ext = "mp3"
		}
		plan := &transcode.Plan{Podcast: task, Container: ext}
		plan.PlanDigest, _ = transcode.DigestPlan(plan)
		req := SubmitRequest{ID: id, SourcePath: source, CandidatePath: filepath.Join(dir, id+"."+ext), SourceSHA256: hash, Profile: "podcast-v1", Plan: plan}
		if _, err := w.Submit(ctx, req, "unused", "unused"); err != nil {
			t.Fatal(err)
		}
		if task.Operation == "transcribe" {
			if err := podcast.WriteJSON(filepath.Join(cfg.StateDir, id, "podcast-transcript.json"), tr); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.InternalRun(ctx, id); err != nil {
			t.Fatal(err)
		}
		s, err := w.Status(ctx, id)
		if err != nil || s.Status != "completed" {
			t.Fatalf("job %s %+v %v", id, s, err)
		}
		return s
	}
	run("native", &podcast.Task{Version: 1, Operation: "transcribe", Language: "en_US"})
	p := podcast.DefaultPolicy()
	p.KnownAdsFirstPass = true
	bs, _ := podcast.Blocks(tr, p)
	classes := map[string]podcast.Classification{}
	for _, b := range bs {
		c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "luna", Decisions: []podcast.Decision{{FirstID: tr.Units[0].ID, LastID: tr.Units[23].ID, Label: "paid_ad", Reason: "confirmed"}, {FirstID: tr.Units[24].ID, LastID: tr.Units[79].ID, Label: "content", Reason: "episode"}}}
		classes[b.ID] = c
	}
	cuts, err := podcast.Plan(tr, p, classes)
	if err != nil {
		t.Fatal(err)
	}
	learning := &podcast.AdLearning{Scope: "show", Policy: p, Classifications: classes, ApprovedDigest: podcast.Digest(cuts)}
	render := run("render", &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: "native", Cuts: &cuts, Learning: learning})
	if render.Podcast.LearnedAds != 1 || !render.Podcast.DecodePassed {
		t.Fatalf("missing validated learning: %+v", render.Podcast)
	}
	catalog, err := w.adCatalog("show")
	if err != nil || len(catalog.References) != 1 {
		t.Fatalf("catalog %+v %v", catalog, err)
	}
	match := run("matching", &podcast.Task{Version: 1, Operation: "match_ads", Language: "en_US", Catalog: &catalog})
	var report podcast.AdMatchReport
	if err := podcast.ReadJSON(filepath.Join(dir, "matching.json"), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Matches) != 1 || report.Matches[0].StartMS != 0 || match.Podcast.MatchDigest != podcast.Digest(report) {
		t.Fatalf("wrong recording bounds %+v", report)
	}
	// A synced scan checkpoint must recover the SAME ID after worker death.
	jobFile := filepath.Join(cfg.StateDir, "matching", "job.json")
	job, _ := LoadJob(jobFile)
	job.Status = "running"
	job.Podcast = nil
	job.EncodeComplete = false
	job.PID = 999999
	SaveJobAtomic(jobFile, job)
	os.Remove(filepath.Join(cfg.StateDir, "matching", "terminal.json"))
	w.SetAliveFunc(func(*JobRecord) bool { return false })
	if err := w.ReconcileStartup(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ = LoadJob(jobFile)
	if job.Status != "queued" {
		t.Fatalf("scan checkpoint not requeued %+v", job)
	}
	w.SetAliveFunc(func(*JobRecord) bool { return true })
	if err := w.InternalRun(ctx, "matching"); err != nil {
		t.Fatal(err)
	}
	other, err := w.adCatalog("another-show")
	if err != nil {
		t.Fatal(err)
	}
	run("other-scope", &podcast.Task{Version: 1, Operation: "match_ads", Language: "en_US", Catalog: &other})
	var otherReport podcast.AdMatchReport
	podcast.ReadJSON(filepath.Join(dir, "other-scope.json"), &otherReport)
	if len(otherReport.Matches) != 0 {
		t.Fatal("cross-podcast scope leaked")
	}
	ref := catalog.References[0]
	if err := w.revokeAd("show", ref.ID); err != nil {
		t.Fatal(err)
	}
	revoked, err := w.adCatalog("show")
	if err != nil || !revoked.References[0].Revoked {
		t.Fatalf("revocation not durable %+v %v", revoked, err)
	}
	run("revoked-snapshot", &podcast.Task{Version: 1, Operation: "match_ads", Language: "en_US", Catalog: &revoked})
	var revokedReport podcast.AdMatchReport
	podcast.ReadJSON(filepath.Join(dir, "revoked-snapshot.json"), &revokedReport)
	if len(revokedReport.Matches) != 0 {
		t.Fatal("revoked recording matched")
	}
	// Completed render recovery cannot bypass tombstones, even if the candidate
	// was already committed before the runner died.
	known, err := podcast.KnownAdUnits(tr, report, p)
	if err != nil {
		t.Fatal(err)
	}
	var evidence *podcast.AdEvidence
	for _, k := range known {
		cp := k.Evidence
		evidence = &cp
		break
	}
	if evidence == nil {
		t.Fatal("fixture lacks interior units")
	}
	renderFile := filepath.Join(cfg.StateDir, "render", "job.json")
	renderJob, _ := LoadJob(renderFile)
	renderJob.Status = "running"
	renderJob.Plan.Podcast.MatchJobID = "matching"
	learning.Classifications[bs[0].ID] = podcast.Classification{Decisions: []podcast.Decision{{Evidence: evidence}}}
	renderJob.Plan.Podcast.Learning = learning
	SaveJobAtomic(renderFile, renderJob)
	r, err := resolveOperationalForExecution(renderJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.finalizeOperational(ctx, filepath.Dir(renderFile), renderFile, renderJob, r); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("terminal recovery bypassed revoke: %v", err)
	}
	saved, _ := LoadJob(renderFile)
	if saved.Status != "failed" || saved.FailureClassification != "podcast_ad_evidence_invalid" {
		t.Fatalf("revoked recovery not terminal %+v", saved)
	}
	if b, err := os.ReadFile(source); err != nil || len(b) == 0 {
		t.Fatal("source changed")
	}
	if current, _ := hashLocalFileSHA256(ctx, source); current != hash {
		t.Fatal("original bytes changed")
	}
}

func TestAdLibraryRejectsTamperedManifest(t *testing.T) {
	cfg := DefaultWorkerConfig()
	cfg.StateDir = t.TempDir()
	w := NewWorker(cfg)
	catalog := podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: "show", References: []podcast.AdReference{{TextDigest: "sha256:" + strings.Repeat("a", 64), ID: "sha256:" + strings.Repeat("a", 64), Digest: "sha256:" + strings.Repeat("b", 64), SourceHash: "sha256:" + strings.Repeat("a", 64), CutsDigest: "sha256:" + strings.Repeat("a", 64), DurationMS: 10000, Label: "paid_ad"}}}
	if _, err := w.matchAds(context.Background(), t.TempDir(), "not-opened", "sha256:"+strings.Repeat("a", 64), catalog); err == nil {
		t.Fatal("missing/changed reference accepted")
	}
}
