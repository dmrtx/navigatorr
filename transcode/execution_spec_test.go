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

// TestDigestTranscodeExecutionSpec_SourceSHA256Identity proves that source
// content identity is part of the canonical full-transcode execution-spec
// digest when available, so a changed source at the same path cannot reuse an
// existing job, while empty (legacy) SHA-256 values are byte-for-byte
// unchanged.
func TestDigestTranscodeExecutionSpec_SourceSHA256Identity(t *testing.T) {
	base := canonicalTestPlan(t)
	const src = "/Volumes/media/a.mkv"
	const cand = "/Volumes/media/b.mkv"
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)

	dA, err := DigestTranscodeExecutionSpecWithSourceSHA(src, cand, "hevc-vt", shaA, base)
	if err != nil {
		t.Fatal(err)
	}
	dA2, err := DigestTranscodeExecutionSpecWithSourceSHA(src, cand, "hevc-vt", shaA, base)
	if err != nil {
		t.Fatal(err)
	}
	if dA != dA2 {
		t.Fatalf("same source SHA-256 must produce the same digest: %s != %s", dA, dA2)
	}

	dB, err := DigestTranscodeExecutionSpecWithSourceSHA(src, cand, "hevc-vt", shaB, base)
	if err != nil {
		t.Fatal(err)
	}
	if dA == dB {
		t.Fatal("different source SHA-256 must alter the execution-spec digest")
	}

	// Legacy equivalence: an absent SHA-256 hashes exactly like the legacy
	// no-identity function, preserving historical persisted digests.
	legacy, err := DigestTranscodeExecutionSpec(src, cand, "hevc-vt", base)
	if err != nil {
		t.Fatal(err)
	}
	legacyEmpty, err := DigestTranscodeExecutionSpecWithSourceSHA(src, cand, "hevc-vt", "", base)
	if err != nil {
		t.Fatal(err)
	}
	if legacy != legacyEmpty {
		t.Fatalf("empty source SHA-256 must be byte-for-byte legacy-equivalent: %s != %s", legacy, legacyEmpty)
	}
	if dA == legacy {
		t.Fatal("a present source SHA-256 must differ from the legacy digest")
	}

	// Prefix/case normalization.
	dPrefixed, err := DigestTranscodeExecutionSpecWithSourceSHA(src, cand, "hevc-vt", "SHA256:"+strings.ToUpper(shaA), base)
	if err != nil {
		t.Fatal(err)
	}
	if dPrefixed != dA {
		t.Fatalf("sha256: prefix / uppercase must normalize to the same digest: %s != %s", dPrefixed, dA)
	}

	// Malformed digests fail closed rather than being silently ignored.
	if _, err := DigestTranscodeExecutionSpecWithSourceSHA(src, cand, "hevc-vt", "not-a-digest", base); err == nil {
		t.Fatal("malformed source SHA-256 must fail closed")
	}
}
