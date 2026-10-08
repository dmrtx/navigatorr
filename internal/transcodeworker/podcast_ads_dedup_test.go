package transcodeworker

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/podcast/acoustic"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestAdLibraryDedupPreservesCategoriesAndTombstones(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg unavailable")
	}
	for _, tc := range []struct {
		name           string
		previous       bool
		revoked        bool
		firstLabel     string
		secondLabel    string
		wantLearned    int
		wantReferences int
		wantMatches    int
		wantKnown      bool
	}{
		{name: "fresh_conflicting_categories", firstLabel: "paid_ad", secondLabel: "cross_promo", wantLearned: 2, wantReferences: 2, wantMatches: 4},
		{name: "previous_conflicting_category", previous: true, firstLabel: "paid_ad", secondLabel: "cross_promo", wantLearned: 1, wantReferences: 2, wantMatches: 4},
		{name: "fresh_same_category", firstLabel: "paid_ad", secondLabel: "paid_ad", wantLearned: 1, wantReferences: 1, wantMatches: 2, wantKnown: true},
		{name: "previous_same_category", previous: true, firstLabel: "paid_ad", secondLabel: "paid_ad", wantReferences: 1, wantMatches: 2, wantKnown: true},
		{name: "revoked_recording_new_category", previous: true, revoked: true, firstLabel: "cross_promo", secondLabel: "cross_promo", wantReferences: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			cfg := DefaultWorkerConfig()
			cfg.StateDir = filepath.Join(base, "jobs")
			w := NewWorker(cfg)
			source, tr := adCategoryDedupFixture(t, base)
			p := podcast.DefaultPolicy()
			p.KnownAdsFirstPass = true
			p.MaxRemovedFraction = .5
			p.Remove = []string{"paid_ad", "cross_promo"}

			learn := func(id, firstLabel, secondLabel string) int {
				t.Helper()
				blocks, err := podcast.Blocks(tr, p)
				if err != nil || len(blocks) != 1 {
					t.Fatalf("fixture blocks: %v %+v", err, blocks)
				}
				b := blocks[0]
				classes := map[string]podcast.Classification{b.ID: {
					BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "luna",
					Decisions: []podcast.Decision{
						{FirstID: tr.Units[0].ID, LastID: tr.Units[23].ID, Label: firstLabel, Reason: "reviewed first recording"},
						{FirstID: tr.Units[24].ID, LastID: tr.Units[27].ID, Label: "content", Reason: "transition"},
						{FirstID: tr.Units[28].ID, LastID: tr.Units[51].ID, Label: secondLabel, Reason: "reviewed second recording"},
						{FirstID: tr.Units[52].ID, LastID: tr.Units[119].ID, Label: "content", Reason: "episode"},
					},
				}}
				cuts, err := podcast.Plan(tr, p, classes)
				if err != nil {
					t.Fatal(err)
				}
				task := &podcast.Task{
					Version: 1, Operation: "render", Language: "en_US", ASRJobID: "native", Cuts: &cuts,
					Learning: &podcast.AdLearning{Scope: "show", Policy: p, Classifications: classes, ApprovedDigest: podcast.Digest(cuts)},
				}
				if err := task.Validate(); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(cfg.StateDir, id)
				file := filepath.Join(dir, "job.json")
				if err := SaveJobAtomic(file, &JobRecord{ID: id, Status: "running", Plan: &transcode.Plan{Podcast: task}}); err != nil {
					t.Fatal(err)
				}
				n, err := w.learnAds(t.Context(), dir, file, source, tr, task)
				if err != nil {
					t.Fatal(err)
				}
				return n
			}

			if tc.previous {
				if n := learn("prior", "paid_ad", "content"); n != 1 {
					t.Fatalf("initial learning=%d, want 1", n)
				}
				if tc.revoked {
					catalog, err := w.adCatalog("show")
					if err != nil {
						t.Fatal(err)
					}
					if err := w.revokeAd("show", catalog.References[0].ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if n := learn("current", tc.firstLabel, tc.secondLabel); n != tc.wantLearned {
				t.Fatalf("learning=%d, want %d", n, tc.wantLearned)
			}
			catalog, err := w.adCatalog("show")
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.References) != tc.wantReferences {
				t.Fatalf("references=%d, want %d", len(catalog.References), tc.wantReferences)
			}
			if tc.revoked && !catalog.References[0].Revoked {
				t.Fatal("learning erased the tombstone")
			}
			if tc.wantReferences == 2 {
				labels := map[string]bool{}
				for _, ref := range catalog.References {
					labels[ref.Label] = true
				}
				if !labels["paid_ad"] || !labels["cross_promo"] {
					t.Fatalf("conflicting reviewed categories were lost: %+v", labels)
				}
			}
			// Read the committed catalog through a new worker, then change policy.
			// Keeping cross promos must not turn their ambiguous copy into paid ads.
			w = NewWorker(cfg)
			catalog, err = w.adCatalog("show")
			if err != nil {
				t.Fatal(err)
			}
			report, err := w.matchAds(t.Context(), t.TempDir(), source, tr.SourceHash, catalog)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Matches) != tc.wantMatches {
				t.Fatalf("matches=%d, want %d", len(report.Matches), tc.wantMatches)
			}
			p.Remove = []string{"paid_ad"}
			known, err := podcast.KnownAdUnits(tr, report, p)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantKnown != (len(known) > 0) {
				t.Fatalf("automatic labels=%d, want known=%v", len(known), tc.wantKnown)
			}
		})
	}
}

// Two identical PCM copies and native timing fixtures isolate category handling
// while exercising actual FFmpeg decoding, reviewed planning and matching.
func adCategoryDedupFixture(t *testing.T, dir string) (string, podcast.Transcript) {
	t.Helper()
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
	source := filepath.Join(dir, "original.wav")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	sha, err := hashLocalFileSHA256(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	tr := podcast.Transcript{SchemaVersion: 1, SourceHash: "sha256:" + sha, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", DurationMS: 60000}
	for i := 0; i < 120; i++ {
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i), StartMS: int64(i * 500), EndMS: int64(i*500 + 480), Text: "native spoken unit", Timing: "native_result"})
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
	return source, tr
}
