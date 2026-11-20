package action

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func checkBetween(t *testing.T, lo, hi string) string {
	t.Helper()
	k, err := keyBetween(lo, hi)
	if err != nil {
		t.Fatalf("keyBetween(%q, %q): %v", lo, hi, err)
	}
	if k == "" || k <= lo {
		t.Fatalf("keyBetween(%q, %q) = %q: not above lo", lo, hi, k)
	}
	if hi != "" {
		if k >= hi {
			t.Fatalf("keyBetween(%q, %q) = %q: not below hi", lo, hi, k)
		}
		if strings.HasPrefix(hi, k) {
			t.Fatalf("keyBetween(%q, %q) = %q: a prefix of hi", lo, hi, k)
		}
		// What a batch block appends stays inside the bounds.
		if s := k + "|00000000000000000099"; s >= hi || s <= lo {
			t.Fatalf("keyBetween(%q, %q) = %q: its block key %q leaves the bounds", lo, hi, k, s)
		}
	}
	if k[len(k)-1] == keyDigitMin {
		t.Fatalf("keyBetween(%q, %q) = %q ends in the zero digit", lo, hi, k)
	}
	return k
}

func TestKeyBetweenBounds(t *testing.T) {
	real1 := queueOrder(queueRankPlain, parseStoreTime("2026-01-02T03:04:05Z"), "act-transcode-media-aa", 0)
	real2 := queueOrder(queueRankPlain, parseStoreTime("2026-01-02T03:04:06Z"), "act-transcode-media-bb", 0)
	for _, c := range [][2]string{
		{"", ""}, {"", "a"}, {"a", ""}, {"a", "b"}, {"a", "a1"}, {"a", "a2"}, {"a", "a|0000000000000000001"},
		{real1, real2}, {"", real1}, {real2, ""},
		{"0|x", "0~|x"}, {"0~|x", "1|x"}, {"a", "aa"}, {"a", "a\"1"}, {"~", ""}, {"", "\"1"},
	} {
		checkBetween(t, c[0], c[1])
	}
	if _, err := keyBetween("b", "a"); err == nil {
		t.Fatal("lo above hi must fail")
	}
	if _, err := keyBetween("a", "a"); err == nil {
		t.Fatal("equal bounds must fail")
	}
	if _, err := keyBetween("a ", "b"); err == nil {
		t.Fatal("a space is not an orderable character")
	}
	if _, err := keyBetween("a!", "b"); err == nil {
		t.Fatal("a key ending in the zero digit has no room below it")
	}
}

// Always inserting first never runs out of room.
func TestKeyBetweenRepeatedFirstInserts(t *testing.T) {
	hi := queueOrder(queueRankPlain, parseStoreTime("2026-01-02T03:04:05Z"), "act-transcode-media-aa", 0)
	for i := 0; i < 500; i++ {
		hi = checkBetween(t, "", hi)
	}
	if len(hi) > 400 {
		t.Fatalf("500 first inserts grew the key to %d characters", len(hi))
	}
}

// Always inserting after the last, or always right before the same key, keeps
// working, and the keys stay in the order they were placed.
func TestKeyBetweenRepeatedInsertsKeepTheirOrder(t *testing.T) {
	lo, hi := "1|a", "1|b"
	var before []string
	for i := 0; i < 300; i++ {
		k := checkBetween(t, lo, hi)
		before = append(before, k)
		hi = k
	}
	for i := 1; i < len(before); i++ {
		if before[i] >= before[i-1] {
			t.Fatalf("insert %d (%q) is not before insert %d (%q)", i, before[i], i-1, before[i-1])
		}
	}
	var after []string
	lo, hi = "1|a", "1|b"
	for i := 0; i < 300; i++ {
		k := checkBetween(t, lo, hi)
		after = append(after, k)
		lo = k
	}
	if !sort.StringsAreSorted(after) {
		t.Fatal("inserting after the last did not keep ascending order")
	}
	var last string
	for i := 0; i < 300; i++ {
		last = checkBetween(t, last, "")
	}
}

// A property-style loop: random inserts into a growing list always land at the
// chosen place and leave every other key alone.
func TestKeyBetweenRandomInsertsLandWhereAsked(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	keys := []string{"1|a", "1|b", "1|c"}
	for i := 0; i < 3000; i++ {
		pos := rng.Intn(len(keys) + 1) // insert at index pos
		lo, hi := "", ""
		if pos > 0 {
			lo = keys[pos-1]
		}
		if pos < len(keys) {
			hi = keys[pos]
		}
		k := checkBetween(t, lo, hi)
		keys = append(keys, "")
		copy(keys[pos+1:], keys[pos:])
		keys[pos] = k
		if i%100 == 0 && !sort.StringsAreSorted(keys) {
			t.Fatalf("list not sorted after %d inserts", i)
		}
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatal("list not sorted")
	}
}
