package recipe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEmbeddedBundleAndLegacyProfileResolve(t *testing.T) {
	s, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatalf("embedded bundle invalid: %v", err)
	}
	if s.Identity.Version == "" || !strings.HasPrefix(s.Identity.Digest, "sha256:") {
		t.Fatalf("missing identity: %+v", s.Identity)
	}
	for _, name := range []string{"hevc-vt", "hevc-vt-balanced", "hevc-vt-quality", "hevc-vt-space"} {
		p, err := Resolve(s, name, nil, nil)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if p.PlanDigest == "" {
			t.Fatalf("%s missing plan digest", name)
		}
	}
	if s.Bundle.Profiles["hevc-vt-quality"].Video.Quality <= s.Bundle.Profiles["hevc-vt-space"].Video.Quality {
		t.Fatalf("VideoToolbox higher q:v is higher quality; quality preset must be greater than space preset")
	}
}

func TestRecipeParserRejectsUnsafeOrUnknownContent(t *testing.T) {
	base := string(EmbeddedBytes())
	cases := []struct{ name, data, want string }{
		{"unknown field", base + "\nextra_args: ['-c', 'copy']\n", "field extra_args"},
		{"unsupported schema", strings.Replace(base, "schema_version: 2", "schema_version: 99", 1), "unsupported recipe schema_version"},
		{"malformed yaml", "schema_version: [", "parsing recipe bundle"},
		{"unsafe bundle version", strings.Replace(base, `bundle_version: "2026.09.4"`, `bundle_version: ".."`, 1), "invalid bundle_version"},
		{"unsafe video codec", strings.Replace(base, "hevc_videotoolbox", ";rm-rf", 1), "unsupported video codec"},
		{"unsafe conversion target", strings.Replace(base, "target_codec: subrip", "target_codec: libx264", 1), "unsupported subtitle conversion target"},
		{"unsafe fallback action", strings.Replace(base, "action: retry", "action: exec", 1), "unknown fallback action"},
		{"shell fallback token", strings.Replace(base, "action: retry", `action: "; rm -rf /"`, 1), "unknown fallback action"},
		{"raw args impossible", strings.Replace(base, "quality: 65}", "quality: 65, raw_args: '-y'}", 1), "field raw_args"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRecipeDuplicateAndInvalidProfileRejected(t *testing.T) {
	dup := `schema_version: 1
bundle_version: "1.0.0"
containers:
  mkv:
    subtitle_copy: [ass, ass]
    subtitle_conversions: {}
profiles:
  p:
    container: mkv
    video: {codec: hevc_videotoolbox, quality: 65}
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: false}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1, transient_retries: 0, retry_backoff_seconds: [], max_fallbacks: 0, fallbacks: []}
`
	_, err := Parse([]byte(dup))
	if err == nil || !strings.Contains(err.Error(), "duplicate subtitle copy codec") {
		t.Fatalf("unexpected error: %v", err)
	}

	badPreserve := strings.Replace(string(EmbeddedBytes()), "attachments: true", "attachments: false", 1)
	_, err = Parse([]byte(badPreserve))
	if err == nil || !strings.Contains(err.Error(), "must all be preserved") {
		t.Fatalf("unexpected preserve error: %v", err)
	}
}

func TestResolverContainerCompatibilityFromRecipe(t *testing.T) {
	s, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatal(err)
	}
	subs := []SourceSubtitle{{SourceStreamIndex: 2, TypeIndex: 0, Codec: "mov_text"}, {SourceStreamIndex: 3, TypeIndex: 1, Codec: "ass"}, {SourceStreamIndex: 4, TypeIndex: 2, Codec: "ssa"}, {SourceStreamIndex: 5, TypeIndex: 3, Codec: "subrip"}}
	p, err := Resolve(s, "hevc-vt", nil, subs)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.SubtitleActions[0]; got.Operation != "transcode" || got.Codec != "subrip" || got.Reason != "matroska_compatibility" {
		t.Fatalf("mov_text action: %+v", got)
	}
	for i := 1; i < len(p.SubtitleActions); i++ {
		if p.SubtitleActions[i].Operation != "copy" {
			t.Fatalf("subtitle %d should copy: %+v", i, p.SubtitleActions[i])
		}
	}
	if len(p.AppliedFallbacks) != 1 {
		t.Fatalf("fallback should apply once, got %v", p.AppliedFallbacks)
	}
	if p.Resilience.MaxFallbacks != 1 {
		t.Fatalf("unexpected max fallback %d", p.Resilience.MaxFallbacks)
	}
}

func TestResolverFailsClosedForUnknownSubtitle(t *testing.T) {
	s, _ := Parse(EmbeddedBytes())
	_, err := Resolve(s, "hevc-vt", nil, []SourceSubtitle{{SourceStreamIndex: 2, TypeIndex: 0, Codec: "made_up_subtitle"}})
	if err == nil || !strings.Contains(err.Error(), "no recipe-authorized safe conversion") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolverWholeProfileReplacementIsDeterministic(t *testing.T) {
	s, _ := Parse(EmbeddedBytes())
	override := s.Bundle.Profiles["hevc-vt"]
	override.Video.Quality = 71
	p1, err := Resolve(s, "custom", map[string]Profile{"custom": override}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Resolve(s, "custom", map[string]Profile{"custom": override}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Quality != 71 || p1.PlanDigest != p2.PlanDigest {
		t.Fatalf("replacement not deterministic: %+v %+v", p1, p2)
	}
	if _, err := Resolve(s, "missing", nil, nil); err == nil {
		t.Fatal("unknown profile must fail before SSH")
	}
}

type mutableProvider struct {
	mu   sync.Mutex
	data []byte
	err  error
	name string
}

func (p *mutableProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "file"
}
func (p *mutableProvider) Fetch(context.Context) (Candidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return Candidate{}, p.err
	}
	return Candidate{Data: append([]byte(nil), p.data...)}, nil
}
func (p *mutableProvider) set(data []byte, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data = data
	p.err = err
}

func versionedBundle(version string, quality int) []byte {
	s := string(EmbeddedBytes())
	s = strings.Replace(s, `bundle_version: "2026.09.4"`, fmt.Sprintf(`bundle_version: %q`, version), 1)
	s = strings.Replace(s, "quality: 65}", fmt.Sprintf("quality: %d}", quality), 1)
	return []byte(s)
}

func TestManagerActivationLKGAndRollback(t *testing.T) {
	p := &mutableProvider{name: "file", data: versionedBundle("1.0.0", 60)}
	m, err := NewManager(p, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveVersion != "1.0.0" {
		t.Fatalf("active=%s", st.ActiveVersion)
	}
	old := m.Snapshot()
	p.set(versionedBundle("1.1.0", 70), nil)
	st, err = m.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveVersion != "1.1.0" || m.Snapshot().Identity.Version != "1.1.0" {
		t.Fatalf("update did not activate: %+v", st)
	}
	if old.Identity.Version != "1.0.0" || old.Bundle.Profiles["hevc-vt"].Video.Quality != 60 {
		t.Fatalf("running snapshot was mutated: %+v", old)
	}
	p.set([]byte("not: [valid"), nil)
	st, err = m.Update(context.Background())
	if err == nil {
		t.Fatal("invalid update should fail")
	}
	if st.ActiveVersion != "1.1.0" || st.LastUpdateError == "" {
		t.Fatalf("LKG not retained: %+v", st)
	}
	st, err = m.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveVersion != "1.0.0" {
		t.Fatalf("rollback=%+v", st)
	}
}

func TestManagerRejectsImmutableVersionCollision(t *testing.T) {
	p := &mutableProvider{name: "file", data: versionedBundle("2.0.0", 60)}
	m, _ := NewManager(p, t.TempDir(), "", "")
	if _, err := m.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.set(versionedBundle("2.0.0", 80), nil)
	if _, err := m.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable version collision") {
		t.Fatalf("unexpected collision result: %v", err)
	}
	if m.Snapshot().Bundle.Profiles["hevc-vt"].Video.Quality != 60 {
		t.Fatal("collision replaced active bundle")
	}
}

func TestManagerDoesNotRestoreCacheFromDifferentSource(t *testing.T) {
	cache := t.TempDir()
	p1 := &mutableProvider{name: "file", data: versionedBundle("3.0.0", 61)}
	m1, _ := NewManager(p1, cache, "stable", "")
	if _, err := m1.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	p2 := &mutableProvider{name: "github", data: versionedBundle("4.0.0", 62)}
	m2, err := NewManager(p2, cache, "stable", "")
	if err != nil {
		t.Fatal(err)
	}
	if m2.Snapshot().Identity.Version != "2026.09.4" {
		t.Fatalf("cross-source cache restored: %s", m2.Snapshot().Identity.Version)
	}
}

func TestFileProviderLoadsBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipes.yaml")
	if err := os.WriteFile(path, EmbeddedBytes(), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := (FileProvider{Path: path}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(c.Data); err != nil {
		t.Fatal(err)
	}
}

func TestManifestDigestFormatHelper(t *testing.T) {
	b := EmbeddedBytes()
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	if len(digest) != 64 {
		t.Fatal("bad sha helper")
	}
}
