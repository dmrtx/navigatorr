package sampledigest

import (
	"bytes"
	"context"
	"testing"
)

func fixture(size int64) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i*31 + i/7)
	}
	return buf
}

// The digests were taken from the coordinator's implementation before it moved
// here; a change in them means the two sides of a comparison would disagree.
func TestDigestKeepsItsValues(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want string
	}{
		{0, "9a271f2a916b0b6ee6cecb2426f0b3206ef074578be55d9bc94f6f3fe3ab86aa"},
		{5, "b51ddad871f084117f1e046bf3bfa840d704aa4ad92eaef81cc2303e6f0da61d"},
		{3*Chunk + 5, "f06968b5f8b0e4dc3a334cf9a6b12ee5f3cda7eac83f5f023470c6811c99cd79"},
		{Chunk * 40, "c28d6803a3a2886f0831eeeb278c281f014029cec7437eb8ba497cef0fdecf91"},
		{Chunk*40 + 4097, "fde0c0d20c58d68676c75e8f7edfa910a30fb929ce56c5a69c00076a0d091486"},
	} {
		got, err := Digest(context.Background(), bytes.NewReader(fixture(tc.size)), tc.size)
		if err != nil || got != tc.want {
			t.Errorf("size %d: %s %v, want %s", tc.size, got, err, tc.want)
		}
	}
}

func TestDigestStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Digest(ctx, bytes.NewReader(fixture(Chunk*3)), Chunk*3); err == nil {
		t.Fatal("a cancelled context must stop the read")
	}
}
