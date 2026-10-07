package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewedPodcastProofAboveOneMiBAcceptedOverHTTP(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp3")
	os.WriteFile(source, []byte("frozen input"), 0600)
	sha, _ := hashLocalFileSHA256(t.Context(), source)
	tr := podcast.Transcript{SchemaVersion: 1, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", SourceHash: "sha256:" + sha, DurationMS: 3600000}
	for i := 0; i < 3600; i++ {
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i), StartMS: int64(i * 1000), EndMS: int64(i*1000 + 900), Text: "word", Timing: "native_result"})
	}
	p := podcast.DefaultPolicy()
	p.KnownAdsFirstPass = true
	bs, _ := podcast.Blocks(tr, p)
	classes := map[string]podcast.Classification{}
	for _, b := range bs {
		c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "luna"}
		for i := b.First; i <= b.Last; i++ {
			label := "content"
			if i < 15 {
				label = "paid_ad"
			}
			c.Decisions = append(c.Decisions, podcast.Decision{FirstID: tr.Units[i].ID, LastID: tr.Units[i].ID, Label: label, Reason: strings.Repeat("x", 300)})
		}
		classes[b.ID] = c
	}
	cuts, err := podcast.Plan(tr, p, classes)
	if err != nil {
		t.Fatal(err)
	}
	task := &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: "native", Cuts: &cuts, Learning: &podcast.AdLearning{Scope: "show", Policy: p, Classifications: classes, ApprovedDigest: podcast.Digest(cuts)}}
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(task)
	if len(body) <= maxHTTPBodyBytes || len(body) > podcast.MaxAdLearningBytes {
		t.Fatalf("fixture size %d", len(body))
	}
	cfg := DefaultWorkerConfig()
	cfg.StateDir = filepath.Join(dir, "jobs")
	cfg.LocalWorkDir = filepath.Join(dir, "work")
	cfg.AllowedRoots = []string{dir}
	cfg.ExternalRoots = nil
	w := NewWorker(cfg)
	w.SetTranscodeSpawner(func(string, string, string) (int, string, error) { return 999999, "", nil })
	w.SetAliveFunc(func(*JobRecord) bool { return true })
	s := httptest.NewServer(NewServer(w, "", "unused", "unused").Handler())
	defer s.Close()
	client, err := transcode.NewHTTPExecutor(transcode.HTTPConfig{BaseURL: s.URL, Token: "unused", PathMappings: []transcode.PathMapping{{Local: dir, Remote: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{Container: "mp3", Podcast: task}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	_, err = client.Submit(context.Background(), transcode.Request{ID: "proof", SourcePath: source, CandidatePath: filepath.Join(dir, "clean.mp3"), Profile: "podcast-v1", SourceSHA256: sha, Plan: plan})
	if err != nil {
		t.Fatalf("valid proof >1MiB rejected: %v", err)
	}
}
