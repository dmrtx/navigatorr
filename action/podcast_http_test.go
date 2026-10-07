package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal/transcodeworker"
	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

// Real coordinator -> HTTP -> shared worker queue -> FFmpeg -> publication.
// ASR is a scripted fixture; TestPodcastNativeWorkerRoundtrip separately runs
// the real Apple adapter on a spoken audio fixture.
func TestPodcastHTTPWorkflowEndToEnd(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	source := filepath.Join(dir, "source.mp3")
	if b, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=12.13", "-ac", "2", "-c:a", "libmp3lame", source).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s %v", b, err)
	}
	b, _ := os.ReadFile(source)
	sha := sha256.Sum256(b)
	sourceHash := hex.EncodeToString(sha[:])
	tr := podcast.Transcript{SchemaVersion: 1, Provider: "apple_speech", ProviderVersion: "test", Language: "en_US", SourceHash: "sha256:" + sourceHash, DurationMS: 12130, Units: []podcast.Unit{{ID: "u000001", StartMS: 0, EndMS: 1000, Text: "episode introduction", Timing: "native_result"}, {ID: "u000002", StartMS: 1100, EndMS: 3100, Text: "commercial paid advertisement", Timing: "native_result"}, {ID: "u000003", StartMS: 3200, EndMS: 12000, Text: "episode story", Timing: "native_result"}}}
	fixture := filepath.Join(dir, "fixture.json")
	podcast.WriteJSON(fixture, tr)
	helper := filepath.Join(dir, "asr")
	os.WriteFile(helper, []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = probe ]; then echo '{\"available\":true,\"os_version\":\"test\"}'; else cp '%s' \"$2\"; fi\n", fixture)), 0700)
	workerCfg := transcodeworker.DefaultWorkerConfig()
	workerCfg.FFmpeg = ffmpeg
	workerCfg.FFprobe = ffprobe
	workerCfg.AllowedRoots = []string{dir}
	workerCfg.StateDir = filepath.Join(dir, "jobs")
	workerCfg.LocalWorkDir = filepath.Join(dir, "work")
	workerCfg.AppleSpeechPath = helper
	workerCfg.AppleSpeechProbeAudio = source
	w := transcodeworker.NewWorker(workerCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w.SetAliveFunc(func(*transcodeworker.JobRecord) bool { return true })
	w.SetTranscodeSpawner(func(_, _ string, id string) (int, string, error) {
		go func() { _ = w.InternalRun(ctx, id) }()
		return os.Getpid(), "", nil
	})
	srv := httptest.NewServer(transcodeworker.NewServer(w, "unused", "unused", "").Handler())
	defer srv.Close()
	tc, err := transcode.NewHTTPExecutor(transcode.HTTPConfig{BaseURL: srv.URL, PathMappings: []transcode.PathMapping{{Local: dir, Remote: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	e, store := setupTranscodeEngine(t, tc, ffprobe, []string{dir}, []string{dir}, false)
	defer store.Close()
	e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"genwhy": {Enabled: true}}}
	output := filepath.Join(dir, "clean.mp3")
	r, err := e.Run(ctx, "clean_podcast_ads", map[string]any{"path": source, "output_path": output, "podcast_id": "genwhy", "source_sha256": sourceHash, "feed_id": "existing-feed", "episode_id": "existing-guid"})
	if err != nil {
		caps, probeErr := tc.Capabilities(ctx)
		t.Fatalf("%v; podcast capabilities=%+v probe_error=%v", err, caps.Podcast, probeErr)
	}
	poll := func() {
		for r.Status == StatusWaitingExternal {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
			r, err = e.Resume(ctx, r.ID, "", nil)
			if err != nil {
				t.Fatal(err)
			}
		}
		if r.Status == StatusFailed {
			caps, probeErr := tc.Capabilities(ctx)
			t.Fatalf("%s; podcast capabilities=%+v probe_error=%v", r.Error, caps.Podcast, probeErr)
		}
	}
	poll()
	if r.WaitingCondition != "podcast_classification" {
		t.Fatalf("unexpected status %s", r.Status)
	}
	manifest, err := e.PodcastBlocks(ctx, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	row := manifest["blocks"].([]map[string]any)[0]
	if _, err := e.PodcastBlock(ctx, r.ID, row["id"].(string), 0); err != nil {
		t.Fatal(err)
	}
	c := podcast.Classification{BlockID: row["id"].(string), BlockDigest: row["digest"].(string), TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "orchestrator-test", Decisions: []podcast.Decision{{FirstID: "u000001", LastID: "u000001", Label: "content", Reason: "Introduction"}, {FirstID: "u000002", LastID: "u000002", Label: "paid_ad", Reason: "Commercial sales read"}, {FirstID: "u000003", LastID: "u000003", Label: "content", Reason: "Story"}}}
	if _, err := e.PodcastClassify(ctx, r.ID, c); err != nil {
		t.Fatal(err)
	}
	r, err = e.Resume(ctx, r.ID, "plan", nil)
	if err != nil {
		t.Fatal(err)
	}
	review, err := e.PodcastReview(ctx, r.ID, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PodcastReview(ctx, r.ID, review["digest"].(string), true, 0); err != nil {
		t.Fatal(err)
	}
	r, err = e.Resume(ctx, r.ID, "render", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll()
	if r.Status != StatusCompleted {
		t.Fatalf("final status %s %s", r.Status, r.Error)
	}
	p := r.State["podcast"].(map[string]any)
	if p["feed_id"] != "existing-feed" || p["episode_id"] != "existing-guid" || p["decode_passed"] != true || getInt64(p, "removed_ms") != 2000 {
		t.Fatalf("publication %+v", p)
	}
	b, _ = os.ReadFile(source)
	after := sha256.Sum256(b)
	if after != sha {
		t.Fatal("original changed")
	}
}
