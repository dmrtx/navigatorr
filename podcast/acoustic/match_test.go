package acoustic

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/jakenesler/navigatorr/podcast"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommonBedChangedVoiceRequiresIndependentTranscriptAgreement(t *testing.T) {
	bed, a, b := signal(12, 101), signal(12, 202), signal(12, 303)
	ref := make([]int16, len(bed))
	changed := make([]int16, len(bed))
	for i := range bed {
		ref[i] = int16(float64(bed[i]) + .12*float64(a[i]))
		changed[i] = int16(float64(bed[i]) + .12*float64(b[i]))
	}
	prefix := append(signal(2, 404), make([]int16, 384)...)
	source := append(prefix, changed...)
	source = append(source, signal(2, 505)...)
	matches := find(t, source, ref)
	if len(matches) == 0 {
		t.Fatal("fixture must demonstrate misleading common-bed acoustic similarity")
	}
	seedUnits := []podcast.Unit{}
	for i := 0; i < 24; i++ {
		seedUnits = append(seedUnits, podcast.Unit{ID: fmt.Sprintf("ad%d", i), StartMS: int64(i * 500), EndMS: int64(i*500 + 480), Text: "confirmed advertisement", Timing: "native_result"})
	}
	sourceHash := "sha256:" + strings.Repeat("a", 64)
	reference := podcast.AdReference{ID: sourceHash, Digest: sourceHash, SourceHash: sourceHash, CutsDigest: sourceHash, TextDigest: podcast.NativeAdTextDigest(seedUnits), DurationMS: 12000, Label: "paid_ad"}
	report := podcast.AdMatchReport{Algorithm: podcast.AdAlgorithm, SourceHash: sourceHash, Catalog: podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: "show", References: []podcast.AdReference{reference}}, DurationMS: 16048}
	for _, m := range matches {
		start := m.StartSample * 1000 / Rate
		report.Matches = append(report.Matches, podcast.AdMatch{ReferenceID: reference.ID, Label: reference.Label, StartMS: start, EndMS: start + 12000, MinCorrelation: m.MinCorrelation})
	}
	tr := podcast.Transcript{SchemaVersion: 1, Provider: "apple_speech", ProviderVersion: "fixture", SourceHash: sourceHash, Language: "en_US", DurationMS: 16048}
	tr.Units = append(tr.Units, podcast.Unit{ID: "intro", StartMS: 0, EndMS: 2000, Text: "episode discussion", Timing: "native_result"})
	for i, u := range seedUnits {
		u.StartMS += 2048
		u.EndMS += 2048
		if i == 12 {
			u.Text = "a different spoken offer"
		}
		tr.Units = append(tr.Units, u)
	}
	tr.Units = append(tr.Units, podcast.Unit{ID: "end", StartMS: 14048, EndMS: 16000, Text: "episode discussion", Timing: "native_result"})
	known, err := podcast.KnownAdUnits(tr, report, podcast.DefaultPolicy())
	if err != nil || len(known) != 0 {
		t.Fatalf("different voice under common bed was auto-labelled: %+v %v", known, err)
	}
}

func pcmBytes(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}
func signal(seconds, seed int) []int16 {
	r := rand.New(rand.NewSource(int64(seed)))
	s := make([]int16, seconds*Rate)
	var prev float64
	for i := range s {
		amp := .2 + .8*math.Abs(math.Sin(float64(i)/float64(Rate)*3.7))
		prev = .8*prev + .2*(r.Float64()*2-1)
		s[i] = int16(amp * (8000*prev + 2000*math.Sin(float64(i)*(.08+.015*math.Sin(float64(i)/Rate*2)))))
	}
	return s
}
func find(t *testing.T, source, ref []int16) []Match {
	t.Helper()
	p, err := Prepare(context.Background(), bytes.NewReader(pcmBytes(source)), int64(len(source)))
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.Find(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestCompleteRecordingAtMultipleOffsets(t *testing.T) {
	ref := signal(12, 1)
	source := signal(5, 2)
	offset := len(source) + 83
	source = append(source, make([]int16, 83)...)
	source = append(source, ref...)
	source = append(source, signal(4, 3)...)
	offset2 := len(source)
	for _, v := range ref {
		source = append(source, int16(float64(v)*.65))
	}
	source = append(source, signal(3, 4)...)
	m := find(t, source, ref)
	if len(m) != 2 || abs(m[0].StartSample-int64(offset)) > 1 || abs(m[1].StartSample-int64(offset2)) > 1 {
		t.Fatalf("matches=%+v expected %d,%d", m, offset, offset2)
	}
}
func TestQuietTailIsVerifiedWithoutBeingAnAlignmentProbe(t *testing.T) {
	ref := signal(12, 18)
	for i := len(ref) - Rate; i < len(ref); i++ {
		ref[i] = 5
	}
	source := append(signal(2, 19), ref...)
	source = append(source, signal(2, 20)...)
	if m := find(t, source, ref); len(m) != 1 {
		t.Fatalf("exact quiet-tail recording missed: %+v", m)
	}
	copy(source[2*Rate+len(ref)-Rate:2*Rate+len(ref)], signal(1, 22))
	if m := find(t, source, ref); len(m) != 0 {
		t.Fatal("quiet tail replaced by different audio accepted")
	}
}
func TestRejectChangedTailPartialAndSimilarBed(t *testing.T) {
	ref := signal(12, 1)
	for _, variant := range []string{"changed_tail", "partial", "same_bed_different_voice", "silence", "tone"} {
		t.Run(variant, func(t *testing.T) {
			s := append([]int16{}, ref...)
			switch variant {
			case "changed_tail":
				copy(s[len(s)-Rate:], signal(1, 40))
			case "partial":
				s = s[:len(s)-Rate*2]
			case "same_bed_different_voice":
				s = signal(12, 44)
			case "silence":
				s = make([]int16, len(s))
			case "tone":
				for i := range s {
					s[i] = int16(2000 * math.Sin(float64(i)*.3))
				}
			}
			s = append(signal(2, 32), s...)
			s = append(s, signal(3, 87)...)
			if m := find(t, s, ref); len(m) != 0 {
				t.Fatalf("unsafe partial/similar recording matched: %+v", m)
			}
		})
	}
}
func TestMP3RecodingAndGain(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg unavailable")
	}
	dir := t.TempDir()
	ref := signal(12, 8)
	source := append(signal(3, 9), ref...)
	source = append(source, signal(2, 10)...)
	input := filepath.Join(dir, "input.pcm")
	mp3 := filepath.Join(dir, "audio.mp3")
	output := filepath.Join(dir, "output.pcm")
	os.WriteFile(input, pcmBytes(source), 0600)
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "s16le", "-ar", "8000", "-ac", "1", "-i", input, "-af", "volume=0.7", "-ar", "44100", "-c:a", "libmp3lame", "-q:a", "2", mp3)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v", b, err)
	}
	cmd = exec.Command("ffmpeg", "-v", "error", "-i", mp3, "-ar", "8000", "-ac", "1", "-f", "s16le", output)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v", b, err)
	}
	raw, _ := os.ReadFile(output)
	decoded := make([]int16, len(raw)/2)
	for i := range decoded {
		decoded[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	m := find(t, decoded, ref)
	if len(m) != 1 || abs(m[0].StartSample-int64(3*Rate)) > 8 {
		t.Fatalf("recoded recording not recognized: %+v", m)
	}
}

// Opt-in real-data benchmark. The reference is a reviewed native timed ad;
// held-out sources can independently demonstrate repeated creative matches.
func TestRealRecordingBenchmark(t *testing.T) {
	source := os.Getenv("NAV_AD_BENCH_SOURCE")
	reference := os.Getenv("NAV_AD_BENCH_REFERENCE")
	if source == "" || reference == "" {
		t.Skip("opt-in original/confirmed-ad PCM benchmark")
	}
	f, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	raw, err := os.ReadFile(reference)
	if err != nil {
		t.Fatal(err)
	}
	ref := make([]int16, len(raw)/2)
	for i := range ref {
		ref[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	p, err := Prepare(t.Context(), f, info.Size()/2)
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.Find(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("source seconds=%.3f reference seconds=%.3f matches=%+v", float64(info.Size()/2)/Rate, float64(len(ref))/Rate, m)
	if os.Getenv("NAV_AD_BENCH_EXPECT_MATCH") == "1" && len(m) == 0 {
		t.Fatal("expected known complete recording")
	}
}
