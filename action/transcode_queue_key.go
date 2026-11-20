package action

import "fmt"

// Fractional queue keys.
//
// A queue key is a string compared bytewise. To put one conversion between two
// others without renumbering anyone, keyBetween builds a key that sorts
// strictly between them. It reads the keys as fractions over the printable
// bytes '!' (0x21, the zero digit) to '~' (0x7e), the way fractional indexing
// does, with one change: the key is never a prefix of the upper bound. A batch
// moved to a place keeps its not-yet-started items as one block by appending
// "|" + item id to the key it was given; that stays below the upper bound only
// when the key itself is not a prefix of it. A key built here does not end in
// the zero digit either, so there is always room below it for the next one.
const (
	keyDigitMin  = '!'
	keyDigitMax  = '~'
	keyDigitBase = int(keyDigitMax-keyDigitMin) + 1
)

func keyDigit(b byte) int { return int(b - keyDigitMin) }

func keyChar(d int) byte { return byte(d) + keyDigitMin }

// keyBetween returns a key strictly between lo and hi that is not a prefix of
// hi. An empty lo has no lower bound and an empty hi no upper bound. It fails
// when lo is not below hi, or a key holds a byte outside '!'..'~' or ends in
// '!' (the zero digit).
func keyBetween(lo, hi string) (string, error) {
	for _, k := range []string{lo, hi} {
		for i := 0; i < len(k); i++ {
			if k[i] < keyDigitMin || k[i] > keyDigitMax {
				return "", fmt.Errorf("queue key %q has a character a position cannot be found between", k)
			}
		}
		if k != "" && k[len(k)-1] == keyDigitMin {
			return "", fmt.Errorf("queue key %q cannot have a key placed below it", k)
		}
	}
	if hi != "" && lo >= hi {
		return "", fmt.Errorf("queue key %q is not below %q", lo, hi)
	}
	return keyMid(lo, hi), nil
}

// keyMid is keyBetween for valid bounds; hi == "" is no upper bound.
func keyMid(lo, hi string) string {
	if hi != "" {
		// The common prefix, with lo read as followed by zero digits.
		n := 0
		for n < len(hi) {
			var lc byte = keyDigitMin
			if n < len(lo) {
				lc = lo[n]
			}
			if lc != hi[n] {
				break
			}
			n++
		}
		if n > 0 {
			rest := ""
			if n < len(lo) {
				rest = lo[n:]
			}
			return hi[:n] + keyMid(rest, hi[n:])
		}
	}
	dl := 0
	if lo != "" {
		dl = keyDigit(lo[0])
	}
	dh := keyDigitBase
	if hi != "" {
		dh = keyDigit(hi[0])
	}
	if dh-dl > 1 {
		return string(keyChar((dl + dh + 1) / 2))
	}
	// No digit fits between the two: keep lo's digit, which is below hi's, and
	// choose the rest with no upper bound. (Taking hi's digit, as the classic
	// algorithm does, would make the key a prefix of hi.)
	rest := ""
	if len(lo) > 1 {
		rest = lo[1:]
	}
	return string(keyChar(dl)) + keyMid(rest, "")
}
