package transcode

// Canonicalization proof for DigestTranscodeExecutionSpec: only plan CONTENT
// influences the digest. Empty, valid, and stale/tampered PlanDigest values
// on otherwise identical plans hash identically; a real content change alters
// the digest.

import (
	"strings"
	"testing"
)

func canonicalTestPlan(t *testing.T) *Plan {
	t.Helper()
	p := &Plan{
		Container: "mkv", VideoCodec: "hevc_videotoolbox", Quality: 65,
		AudioMode: "copy", SubtitleMode: "preserve",
		PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true,
		RecipeVersion: "test-1.0.0",
		RecipeDigest:  "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Resilience:    ResiliencePlan{MaxAttempts: 1},
	}
	d, err := DigestPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	p.PlanDigest = d
	return p
}

func TestDigestTranscodeExecutionSpec_PlanDigestInvariant(t *testing.T) {
	base := canonicalTestPlan(t)

	empty := *base
	empty.PlanDigest = ""
	stale := *base
	stale.PlanDigest = "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	tampered := *base
	tampered.PlanDigest = "not-even-a-digest"

	dValid, err := DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "hevc-vt", base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		plan *Plan
	}{
		{"empty digest", &empty},
		{"stale digest", &stale},
		{"tampered digest", &tampered},
	} {
		got, err := DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "hevc-vt", tc.plan)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != dValid {
			t.Errorf("%s: digest %s != canonical %s (PlanDigest alone must not alter the digest)", tc.name, got, dValid)
		}
	}

	// Real content change must alter the digest.
	changed := *base
	changed.Quality = 70
	cp := changed
	cp.PlanDigest = ""
	d2, err := DigestPlan(&cp)
	if err != nil {
		t.Fatal(err)
	}
	changed.PlanDigest = d2
	dChanged, err := DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "hevc-vt", &changed)
	if err != nil {
		t.Fatal(err)
	}
	if dChanged == dValid {
		t.Error("content change (quality) must alter the execution-spec digest")
	}

	// Path and profile sensitivity retained.
	for _, tc := range []struct {
		name string
		src  string
		cand string
		prof string
	}{
		{"source", "/Volumes/media/other.mkv", "/Volumes/media/b.mkv", "hevc-vt"},
		{"candidate", "/Volumes/media/a.mkv", "/Volumes/media/other.mkv", "hevc-vt"},
		{"profile", "/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "other-profile"},
	} {
		got, err := DigestTranscodeExecutionSpec(tc.src, tc.cand, tc.prof, base)
		if err != nil {
			t.Fatal(err)
		}
		if got == dValid {
			t.Errorf("%s change must alter the digest", tc.name)
		}
	}

	// Profile normalization retained.
	dNorm, err := DigestTranscodeExecutionSpec("/Volumes/media/a.mkv", "/Volumes/media/b.mkv", "  HEVC-VT ", base)
	if err != nil {
		t.Fatal(err)
	}
	if dNorm != dValid {
		t.Error("profile case/whitespace must normalize to the same digest")
	}
	if !strings.HasPrefix(dValid, "sha256:") {
		t.Errorf("digest must carry sha256: prefix, got %q", dValid)
	}
}
