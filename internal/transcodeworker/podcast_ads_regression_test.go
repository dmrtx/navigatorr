package transcodeworker

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/podcast/acoustic"
	"github.com/jakenesler/navigatorr/transcode"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRevokedAdTerminalRecoveryBranches(t *testing.T) {
	for _, state := range []FinalizationState{FinalizationStateNotRequired, FinalizationStateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			cfg := DefaultWorkerConfig()
			cfg.StateDir = filepath.Join(base, "jobs")
			cfg.LocalWorkDir = filepath.Join(base, "work")
			w := NewWorker(cfg)
			source := "sha256:" + strings.Repeat("a", 64)
			r := adRecording{TextDigest: source, Algorithm: podcast.AdAlgorithm, Scope: "show", Label: "paid_ad", SourceHash: source, CutsDigest: source, FirstID: "u1", LastID: "u13", EndMS: 8000, PCM: make([]byte, acoustic.Rate*8*2)}
			ref := recordingReference(r)
			library := w.adDir("show")
			podcast.WriteJSON(adFile(library, ref.ID), r)
			podcast.WriteJSON(adFile(library, ref.ID)+".meta", ref)
			catalog := podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: "show", References: []podcast.AdReference{ref}}
			report := podcast.AdMatchReport{Algorithm: podcast.AdAlgorithm, SourceHash: source, Catalog: catalog, DurationMS: 20000, Matches: []podcast.AdMatch{{ReferenceID: ref.ID, Label: ref.Label, StartMS: 1000, EndMS: 9000, MinCorrelation: 1}}}
			matchDir := filepath.Join(cfg.StateDir, "review-match")
			podcast.WriteJSON(filepath.Join(matchDir, "podcast-matches.json"), report)
			SaveJobAtomic(filepath.Join(matchDir, "job.json"), &JobRecord{ID: "review-match", Status: "completed", Plan: &transcode.Plan{Podcast: &podcast.Task{Version: 1, Operation: "match_ads", Catalog: &catalog}}, Podcast: &podcast.Result{Operation: "match_ads", SourceHash: source, MatchDigest: podcast.Digest(report)}})
			p := podcast.DefaultPolicy()
			p.KnownAdsFirstPass = true
			evidence := &podcast.AdEvidence{ReferenceID: ref.ID, MatchDigest: podcast.Digest(report)}
			task := &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: "review-asr", MatchJobID: "review-match", Learning: &podcast.AdLearning{Scope: "show", Policy: p, Classifications: map[string]podcast.Classification{"b1": {Decisions: []podcast.Decision{{FirstID: "u1", LastID: "u13", Label: "paid_ad", Evidence: evidence}}}}}}
			candidate := filepath.Join(base, "candidate.mp3")
			os.WriteFile(candidate, []byte("validated candidate"), 0600)
			h, _ := hashLocalFileSHA256(ctx, candidate)
			info, _ := os.Stat(candidate)
			job := &JobRecord{ID: "review-render", Status: "running", Candidate: candidate, SourceSHA256: strings.TrimPrefix(source, "sha256:"), Plan: &transcode.Plan{Podcast: task}, EncodeComplete: true, CandidateSHA256: h, CandidateSizeBytes: info.Size(), FinalizationState: string(state)}
			dir := filepath.Join(cfg.StateDir, job.ID)
			file := filepath.Join(dir, "job.json")
			SaveJobAtomic(file, job)
			if err := w.revokeAd("show", ref.ID); err != nil {
				t.Fatal(err)
			}
			_ = w.finalizeOperational(ctx, dir, file, job, &resolvedOperational{localCandidate: candidate, destination: candidate, finalization: state})
			got, err := LoadJob(file)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status == "completed" {
				t.Fatalf("revoked pending render completed through recovery %s", state)
			}
			if !strings.Contains(got.Error, "revoked") {
				t.Fatalf("recovery failed for a reason other than revocation: %s", got.Error)
			}
		})
	}
}

func TestLearningPreservesNativeEndTolerance(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	base := t.TempDir()
	input := filepath.Join(base, "original.mp3")
	if b, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "anoisesrc=color=pink:amplitude=0.2:sample_rate=44100:duration=40:seed=13", "-c:a", "libmp3lame", "-q:a", "2", input).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s %v", b, err)
	}
	cfg := DefaultWorkerConfig()
	cfg.StateDir = filepath.Join(base, "jobs")
	w := NewWorker(cfg)
	h, _ := hashLocalFileSHA256(t.Context(), input)
	tr := podcast.Transcript{SchemaVersion: 1, SourceHash: "sha256:" + h, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", DurationMS: 40000}
	for i := 0; i < 80; i++ {
		end := int64(i*500 + 480)
		if i == 79 {
			end = tr.DurationMS + 100
		}
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i), StartMS: int64(i * 500), EndMS: end, Text: "native spoken unit", Timing: "native_result"})
	}
	p := podcast.DefaultPolicy()
	p.KnownAdsFirstPass = true
	p.MaxRemovedFraction = .5
	bs, _ := podcast.Blocks(tr, p)
	b := bs[0]
	classes := map[string]podcast.Classification{b.ID: {BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "luna", Decisions: []podcast.Decision{{FirstID: tr.Units[0].ID, LastID: tr.Units[55].ID, Label: "content", Reason: "episode"}, {FirstID: tr.Units[56].ID, LastID: tr.Units[79].ID, Label: "paid_ad", Reason: "confirmed tail ad"}}}}
	cuts, err := podcast.Plan(tr, p, classes)
	if err != nil {
		t.Fatal(err)
	}
	task := &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: "native", Cuts: &cuts, Learning: &podcast.AdLearning{Scope: "show", Policy: p, Classifications: classes, ApprovedDigest: podcast.Digest(cuts)}}
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.StateDir, "review-learn")
	file := filepath.Join(dir, "job.json")
	SaveJobAtomic(file, &JobRecord{ID: "review-learn", Status: "running", Plan: &transcode.Plan{Podcast: task}})
	if _, err := w.learnAds(t.Context(), dir, file, input, tr, task); err != nil {
		t.Fatalf("valid native timing tolerance failed learning: %v", err)
	}
}

func TestAdLibraryFreshDedupAndMetadataIdentity(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	base := t.TempDir()
	cfg := DefaultWorkerConfig()
	cfg.StateDir = filepath.Join(base, "jobs")
	w := NewWorker(cfg)
	one := make([]int16, 12*acoustic.Rate)
	state := uint32(91)
	for i := range one {
		state = state*1664525 + 1013904223
		one[i] = int16(int32(state>>16)-32768) / 2
	}
	samples := append([]int16{}, one...)
	samples = append(samples, make([]int16, 2*acoustic.Rate)...)
	samples = append(samples, one...)
	samples = append(samples, make([]int16, 34*acoustic.Rate)...)
	raw := make([]byte, 44+len(samples)*2)
	copy(raw, "RIFF")
	binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	copy(raw[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(raw[16:], 16)
	binary.LittleEndian.PutUint16(raw[20:], 1)
	binary.LittleEndian.PutUint16(raw[22:], 1)
	binary.LittleEndian.PutUint32(raw[24:], acoustic.Rate)
	binary.LittleEndian.PutUint32(raw[28:], acoustic.Rate*2)
	binary.LittleEndian.PutUint16(raw[32:], 2)
	binary.LittleEndian.PutUint16(raw[34:], 16)
	copy(raw[36:], "data")
	binary.LittleEndian.PutUint32(raw[40:], uint32(len(samples)*2))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(raw[44+i*2:], uint16(v))
	}
	source := filepath.Join(base, "original.wav")
	os.WriteFile(source, raw, 0600)
	sha, _ := hashLocalFileSHA256(t.Context(), source)
	tr := podcast.Transcript{SchemaVersion: 1, SourceHash: "sha256:" + sha, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", DurationMS: 60000}
	for i := 0; i < 120; i++ {
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i), StartMS: int64(i * 500), EndMS: int64(i*500 + 480), Text: "native spoken unit", Timing: "native_result"})
	}
	p := podcast.DefaultPolicy()
	p.KnownAdsFirstPass = true
	p.MaxRemovedFraction = .5
	bs, _ := podcast.Blocks(tr, p)
	b := bs[0]
	classes := map[string]podcast.Classification{b.ID: {BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "luna", Decisions: []podcast.Decision{{FirstID: tr.Units[0].ID, LastID: tr.Units[23].ID, Label: "paid_ad", Reason: "first copy"}, {FirstID: tr.Units[24].ID, LastID: tr.Units[27].ID, Label: "content", Reason: "transition"}, {FirstID: tr.Units[28].ID, LastID: tr.Units[51].ID, Label: "paid_ad", Reason: "second copy"}, {FirstID: tr.Units[52].ID, LastID: tr.Units[119].ID, Label: "content", Reason: "transition"}}}}
	cuts, err := podcast.Plan(tr, p, classes)
	if err != nil {
		t.Fatal(err)
	}
	task := &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: "native", Cuts: &cuts, Learning: &podcast.AdLearning{Scope: "show", Policy: p, Classifications: classes, ApprovedDigest: podcast.Digest(cuts)}}
	dir := filepath.Join(cfg.StateDir, "review-fresh")
	file := filepath.Join(dir, "job.json")
	SaveJobAtomic(file, &JobRecord{ID: "review-fresh", Status: "running", Plan: &transcode.Plan{Podcast: task}})
	learned, err := w.learnAds(t.Context(), dir, file, source, tr, task)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := w.adCatalog("show")
	if err != nil {
		t.Fatal(err)
	}
	if learned != 1 || len(catalog.References) != 1 {
		t.Fatalf("fresh repeated recording not deduped: learned=%d refs=%d", learned, len(catalog.References))
	}
	ref := catalog.References[0]
	ref.Label = "cross_promo"
	if err := podcast.WriteJSON(adFile(w.adDir("show"), ref.ID)+".meta", ref); err != nil {
		t.Fatal(err)
	}
	forged, err := w.adCatalog("show")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.matchAds(t.Context(), t.TempDir(), source, tr.SourceHash, forged)
	if err == nil || !strings.Contains(err.Error(), "digest differs") {
		t.Fatalf("metadata alteration was not rejected against PCM recording: %v", err)
	}
}
