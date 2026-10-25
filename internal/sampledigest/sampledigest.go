// Package sampledigest is the digest over a few ranges of a file that stands in
// for a full read where a full read is the cost being avoided. The coordinator
// and the NAS helper share it, so both sides compute the same value for the same
// bytes.
package sampledigest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Chunk is the size of each sampled range.
const Chunk = 1 << 20

// Ranges is how many interior ranges are sampled besides the first and the
// last. A file this many chunks long or shorter is read whole.
const Ranges = 6

// Offsets are the starts of the ranges sampled from a file of the given size:
// all of it when short, else the first, the last and evenly spaced ranges
// between, aligned to 4 KiB.
func Offsets(size int64) []int64 {
	if size <= Chunk*(Ranges+2) {
		var offsets []int64
		for off := int64(0); off < size; off += Chunk {
			offsets = append(offsets, off)
		}
		return offsets
	}
	last := size - Chunk
	offsets := []int64{0}
	for i := int64(1); i <= Ranges; i++ {
		offsets = append(offsets, (last*i/(Ranges+1))&^4095)
	}
	return append(offsets, last)
}

// Digest hashes size and the sampled ranges of r, which must hold exactly size
// bytes. ctx is checked between ranges.
func Digest(ctx context.Context, r io.ReaderAt, size int64) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n", size)
	buf := make([]byte, Chunk)
	for _, off := range Offsets(size) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n := int64(len(buf))
		if rest := size - off; rest < n {
			n = rest
		}
		if _, err := r.ReadAt(buf[:n], off); err != nil {
			return "", err
		}
		h.Write(buf[:n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
