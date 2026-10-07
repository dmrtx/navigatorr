package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

type adTestExecutor struct {
	*mockTranscodeExecutor
	catalog podcast.AdCatalog
}

func (m *adTestExecutor) AdCatalog(context.Context, string) (podcast.AdCatalog, error) {
	return m.catalog, nil
}
func (m *adTestExecutor) RevokeAd(_ context.Context, _, id string) error {
	for i := range m.catalog.References {
		if m.catalog.References[i].ID == id {
			m.catalog.References[i].Revoked = true
			return nil
		}
	}
	return fmt.Errorf("missing reference")
}
func TestKnownAdFirstPassCachedASRRestartAndMixedCoverage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp3")
	os.WriteFile(source, []byte("original preserved"), 0600)
	sum := sha256.Sum256([]byte("original preserved"))
	hash := hex.EncodeToString(sum[:])
	tr := podcast.Transcript{SchemaVersion: 1, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", SourceHash: "sha256:" + hash, DurationMS: 40000}
	for i := 0; i < 100; i++ {
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i+1), StartMS: int64(i * 400), EndMS: int64(i*400 + 350), Text: "episode word", Timing: "native_result"})
	}
	cached := filepath.Join(dir, "cached.json")
	podcast.WriteJSON(cached, tr)
	ref := podcast.AdReference{TextDigest: podcast.NativeAdTextDigest(tr.Units[3:28]), ID: "sha256:" + strings.Repeat("b", 64), Digest: "sha256:" + strings.Repeat("b", 64), SourceHash: tr.SourceHash, CutsDigest: "sha256:" + strings.Repeat("c", 64), DurationMS: 10000, Label: "paid_ad"}
	tc := &adTestExecutor{catalog: podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: "show", References: []podcast.AdReference{ref}}}
	statuses := map[string]transcode.JobStatus{"cached-asr": {ID: "cached-asr", Status: transcode.StatusCompleted, Podcast: &podcast.Result{Operation: "transcribe", SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr)}}}
	requests := map[string]transcode.Request{}
	lostAck := true
	tc.mockTranscodeExecutor = &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US", AdAlgorithm: podcast.AdAlgorithm}}, nil
	}, statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
		if s, ok := statuses[id]; ok {
			return s, nil
		}
		return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
	}, submitFunc: func(_ context.Context, r transcode.Request) (transcode.Job, error) {
		requests[r.ID] = r
		statuses[r.ID] = transcode.JobStatus{ID: r.ID, Status: transcode.StatusQueued}
		if lostAck {
			lostAck = false
			return transcode.Job{}, &transcode.UncertainError{Op: "submit", Err: fmt.Errorf("lost ACK")}
		}
		return transcode.Job{ID: r.ID}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
	defer st.Close()
	e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"show": {Enabled: true, KnownAdsFirstPass: true}}}
	r, err := e.Run(ctx, "clean_podcast_ads", map[string]any{"path": source, "podcast_id": "show", "output_path": filepath.Join(dir, "clean.mp3"), "cached_transcript_path": cached, "cached_transcript_digest": podcast.Digest(tr), "cached_asr_job_id": "cached-asr"})
	if err != nil || r.Status != StatusWaitingExternal {
		t.Fatalf("run %+v %v", r, err)
	}
	matchID := getString(r.State, "podcast_match_ads_job")
	request := requests[matchID]
	if request.Plan.Podcast.Operation != "match_ads" || len(requests) != 1 {
		t.Fatal("cached ASR bypassed first pass")
	}
	e = NewEngine(e.Deps())
	r, err = e.Resume(ctx, r.ID, "", nil)
	if err != nil || len(requests) != 1 {
		t.Fatal("lost ACK/restart duplicated scan")
	}
	report := podcast.AdMatchReport{Algorithm: podcast.AdAlgorithm, SourceHash: tr.SourceHash, Catalog: *request.Plan.Podcast.Catalog, DurationMS: tr.DurationMS, Matches: []podcast.AdMatch{{ReferenceID: ref.ID, Label: ref.Label, StartMS: 1000, EndMS: 11000, MinCorrelation: .99}}}
	podcast.WriteJSON(request.CandidatePath, report)
	raw, _ := os.ReadFile(request.CandidatePath)
	digest := sha256.Sum256(raw)
	statuses[matchID] = transcode.JobStatus{ID: matchID, Status: transcode.StatusCompleted, CandidatePath: request.CandidatePath, CandidateSHA256: hex.EncodeToString(digest[:]), CandidateSizeBytes: int64(len(raw)), Podcast: &podcast.Result{Operation: "match_ads", SourceHash: tr.SourceHash, MatchDigest: podcast.Digest(report)}}
	r, err = e.Resume(ctx, r.ID, "", nil)
	if err != nil || r.WaitingCondition != "podcast_classification" || len(requests) != 1 {
		t.Fatalf("scan/cached ASR %+v %v", r, err)
	}
	s, err := loadPodcastSession(parseExecutionContextMust(t, e, r.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Known) == 0 || podcast.Digest(s.Transcript) != podcast.Digest(tr) {
		t.Fatal("missing evidence or altered native transcript")
	}
	b := s.Blocks[0]
	c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "luna"}
	for offset := 0; ; {
		page, err := e.PodcastBlock(ctx, r.ID, b.ID, offset, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range page["units"].([]podcast.Unit) {
			if _, ok := s.Known[u.ID]; ok {
				t.Fatal("known unit sent to LLM")
			}
			c.Decisions = append(c.Decisions, podcast.Decision{FirstID: u.ID, LastID: u.ID, Label: "content", Reason: "case discussion"})
		}
		if !page["has_more"].(bool) {
			break
		}
		offset = page["next_offset"].(int)
	}
	s, _ = loadPodcastSession(parseExecutionContextMust(t, e, r.ID))
	for i, u := range tr.Units {
		if _, ok := s.Known[u.ID]; ok && s.Reads[b.ID][i-b.First] {
			t.Fatal("omitted unit received fake read receipt")
		}
	}
	if _, err := e.PodcastClassify(ctx, r.ID, c); err != nil {
		t.Fatal(err)
	}
	e = NewEngine(e.Deps())
	r, err = e.Resume(ctx, r.ID, "plan", nil)
	if err != nil || r.WaitingCondition != "podcast_review" {
		t.Fatalf("plan %+v %v", r, err)
	}
	review, err := e.PodcastReview(ctx, r.ID, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PodcastReview(ctx, r.ID, review["digest"].(string), true, 0); err != nil {
		t.Fatal(err)
	}
	tc.RevokeAd(ctx, "show", ref.ID)
	r, err = e.Resume(ctx, r.ID, "render", nil)
	if err == nil && (r == nil || !strings.Contains(r.Error, "revoked")) {
		t.Fatalf("revoked evidence did not block render %+v %v", r, err)
	}
	if len(requests) != 1 {
		t.Fatal("render queued after revocation")
	}
}

func TestKnownAdEmptyFirstPassDurableClassification(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp3")
	if err := os.WriteFile(source, []byte("original preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("original preserved"))
	hash := hex.EncodeToString(sum[:])
	tr := podcast.Transcript{SchemaVersion: 1, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", SourceHash: "sha256:" + hash, DurationMS: 40000}
	for i := 0; i < 100; i++ {
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i+1), StartMS: int64(i * 400), EndMS: int64(i*400 + 350), Text: "case discussion", Timing: "native_result"})
	}
	cached := filepath.Join(dir, "cached.json")
	if err := podcast.WriteJSON(cached, tr); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]transcode.JobStatus{"cached-asr": {ID: "cached-asr", Status: transcode.StatusCompleted, Podcast: &podcast.Result{Operation: "transcribe", SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr)}}}
	requests := map[string]transcode.Request{}
	tc := &adTestExecutor{catalog: podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: "show"}}
	tc.mockTranscodeExecutor = &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US", AdAlgorithm: podcast.AdAlgorithm}}, nil
	}, statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
		if s, ok := statuses[id]; ok {
			return s, nil
		}
		return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
	}, submitFunc: func(_ context.Context, r transcode.Request) (transcode.Job, error) {
		requests[r.ID] = r
		statuses[r.ID] = transcode.JobStatus{ID: r.ID, Status: transcode.StatusQueued}
		return transcode.Job{ID: r.ID}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
	defer st.Close()
	e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"show": {Enabled: true, KnownAdsFirstPass: true}}}
	r, err := e.Run(ctx, "clean_podcast_ads", map[string]any{"path": source, "podcast_id": "show", "output_path": filepath.Join(dir, "clean.mp3"), "cached_transcript_path": cached, "cached_transcript_digest": podcast.Digest(tr), "cached_asr_job_id": "cached-asr"})
	if err != nil || r.Status != StatusWaitingExternal {
		t.Fatalf("run %+v %v", r, err)
	}
	matchID := getString(r.State, "podcast_match_ads_job")
	request := requests[matchID]
	report := podcast.AdMatchReport{Algorithm: podcast.AdAlgorithm, SourceHash: tr.SourceHash, Catalog: *request.Plan.Podcast.Catalog, DurationMS: tr.DurationMS}
	if err := podcast.WriteJSON(request.CandidatePath, report); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(request.CandidatePath)
	digest := sha256.Sum256(raw)
	statuses[matchID] = transcode.JobStatus{ID: matchID, Status: transcode.StatusCompleted, CandidatePath: request.CandidatePath, CandidateSHA256: hex.EncodeToString(digest[:]), CandidateSizeBytes: int64(len(raw)), Podcast: &podcast.Result{Operation: "match_ads", SourceHash: tr.SourceHash, MatchDigest: podcast.Digest(report)}}
	e = NewEngine(e.Deps())
	r, err = e.Resume(ctx, r.ID, "", nil)
	if err != nil || r.WaitingCondition != "podcast_classification" {
		t.Fatalf("empty scan/cached ASR %+v %v", r, err)
	}
	e = NewEngine(e.Deps())
	s, err := loadPodcastSession(parseExecutionContextMust(t, e, r.ID))
	if err != nil || len(s.Known) != 0 {
		t.Fatalf("empty evidence checkpoint %v %+v", err, s.Known)
	}
	// Recover a durable action left failed by the old digest comparison.
	inst, err := st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	inst.Status = StatusFailed
	inst.ErrorJSON = toJSON(map[string]any{"message": "acoustic checkpoint evidence changed"})
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	e = NewEngine(e.Deps())
	r, err = e.Retry(ctx, r.ID)
	if err != nil || r.WaitingCondition != "podcast_classification" || len(requests) != 1 || getString(r.State, "podcast_transcribe_job") != "cached-asr" {
		t.Fatalf("existing empty checkpoint retry %+v %v requests=%d", r, err, len(requests))
	}
	manifest, err := e.PodcastBlocks(ctx, r.ID, 0)
	if err != nil || manifest["classified_blocks"].(int) != 0 {
		t.Fatalf("unknown blocks were skipped: %v %+v", err, manifest)
	}
	for _, b := range s.Blocks {
		for offset := 0; ; {
			page, err := e.PodcastBlock(ctx, r.ID, b.ID, offset, true)
			if err != nil {
				t.Fatal(err)
			}
			if !page["has_more"].(bool) {
				break
			}
			offset = page["next_offset"].(int)
		}
		c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "luna", Decisions: []podcast.Decision{{FirstID: tr.Units[b.First].ID, LastID: tr.Units[b.Last].ID, Label: "content", Reason: "case discussion"}}}
		if _, err := e.PodcastClassify(ctx, r.ID, c); err != nil {
			t.Fatal(err)
		}
	}
	e = NewEngine(e.Deps())
	r, err = e.Resume(ctx, r.ID, "plan", nil)
	if err != nil || r.WaitingCondition != "podcast_review" || len(requests) != 1 {
		t.Fatalf("empty pass restart plan %+v %v requests=%d", r, err, len(requests))
	}
	ec := parseExecutionContextMust(t, e, r.ID)
	s, err = loadPodcastSession(ec)
	if err != nil || s.Cuts.RemovedMS != 0 {
		t.Fatalf("empty evidence altered cuts: %v %+v", err, s.Cuts)
	}
	s.Known = map[string]podcast.AdKnownUnit{tr.Units[0].ID: {Label: "paid_ad"}}
	if err := savePodcastSession(ec, s); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPodcastSession(ec); err == nil {
		t.Fatal("invented nonempty evidence accepted")
	}
}
