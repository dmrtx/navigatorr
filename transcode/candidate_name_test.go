package transcode

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const testJobID = "job-act-transcode-media-0123456789abcdef"

func TestCandidateFileNameKeepsNamesThatFit(t *testing.T) {
	got := CandidateFileName("Show - S01E01 - Pilot", testJobID, ".mkv")
	if want := "Show - S01E01 - Pilot." + testJobID + ".mkv"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCandidateFileNameFitsWithItsPartialAndKeepsTheTail(t *testing.T) {
	stems := map[string]string{
		"ascii":      strings.Repeat("a", 300),
		"accents":    strings.Repeat("é", 150),
		"japanese":   strings.Repeat("転生したらスライムだった件", 20),
		"emoji":      strings.Repeat("🎬", 90),
		"odd offset": "x" + strings.Repeat("ñ", 200),
	}
	for name, stem := range stems {
		t.Run(name, func(t *testing.T) {
			got := CandidateFileName(stem, testJobID, ".mkv")
			if !utf8.ValidString(got) {
				t.Fatalf("name is not valid UTF-8: %q", got)
			}
			if n := len(got + ".partial." + testJobID); n > MaxFileNameBytes {
				t.Fatalf("partial name is %d bytes, limit %d", n, MaxFileNameBytes)
			}
			if !strings.HasSuffix(got, "."+testJobID+".mkv") {
				t.Fatalf("job id and extension lost: %q", got)
			}
			if !strings.HasPrefix(stem, strings.TrimSuffix(got, "."+testJobID+".mkv")) {
				t.Fatalf("name is not a prefix of the original: %q", got)
			}
		})
	}
}

func TestCandidateFileNameUsesTheWholeBudget(t *testing.T) {
	got := CandidateFileName(strings.Repeat("a", 400), testJobID, ".mkv")
	if n := len(got + ".partial." + testJobID); n != MaxFileNameBytes {
		t.Fatalf("expected the partial to be exactly %d bytes, got %d", MaxFileNameBytes, n)
	}
}

func TestShortenCandidatePath(t *testing.T) {
	long := strings.Repeat("ñ", 100) + " " + strings.Repeat("a", 120)
	dir := "/media/TV/Show/.navigatorr-candidates/"

	fits := dir + "Short." + testJobID + ".mkv"
	if got := ShortenCandidatePath(fits, testJobID); got != fits {
		t.Fatalf("a name that fits changed: %q", got)
	}

	old := dir + long + "." + testJobID + ".mkv"
	got := ShortenCandidatePath(old, testJobID)
	if got != dir+CandidateFileName(long, testJobID, ".mkv") {
		t.Fatalf("an old long name must shorten exactly like a new one: %q", got)
	}
	if again := ShortenCandidatePath(got, testJobID); again != got {
		t.Fatalf("shortening is not idempotent: %q", again)
	}

	foreign := dir + strings.Repeat("b", 300) + ".mkv"
	got = ShortenCandidatePath(foreign, testJobID)
	if base := got[len(dir):]; len(base+".partial."+testJobID) > MaxFileNameBytes || !strings.HasSuffix(base, ".mkv") {
		t.Fatalf("foreign name not bounded: %d bytes %q", len(base), base)
	}
}
