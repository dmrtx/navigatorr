package transcodeworker

import (
	"strings"
	"testing"
)

// A failed alignment check names both extracts' leading timestamps, so a gap
// in the source's timeline, an offset and a duplicated picture read apart from
// the message alone.
func TestVerifySampleAlignmentReportsTimestampsOfBothExtracts(t *testing.T) {
	ref := []float64{0, 0.042, 0.083, 0.125, 0.167, 0.208, 8.4}
	cand := []float64{0, 0.042, 0.083, 0.125, 0.167, 0.208, 0.25, 9.2}
	err := verifySampleAlignment(ref, cand, 23.976)
	if err == nil {
		t.Fatal("different frame counts passed")
	}
	for _, want := range []string{
		"frame count mismatch: 7 versus 8",
		"source timestamps [0.000 0.042 0.083 0.125 0.167 … 8.400]",
		"candidate timestamps [0.000 0.042 0.083 0.125 0.167 … 9.200]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}

	err = verifySampleAlignment([]float64{0, 0.04, 0.08}, []float64{0, 0.04, 0.2}, 25)
	if err == nil {
		t.Fatal("a late picture passed")
	}
	for _, want := range []string{"timestamp mismatch at frame 2", "source 0.080, candidate 0.200", "source timestamps [0.000 0.040 0.080]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
}

func TestLeadingTimestampsIsBounded(t *testing.T) {
	if got := leadingTimestamps(nil); got != "[]" {
		t.Fatalf("empty extract: %q", got)
	}
	long := make([]float64, 240)
	for i := range long {
		long[i] = float64(i) / 24
	}
	if got := leadingTimestamps(long); strings.Count(got, " ") > leadingTimestampCount+1 {
		t.Fatalf("a whole window was printed: %q", got)
	}
}
