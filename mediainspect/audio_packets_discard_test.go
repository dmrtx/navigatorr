package mediainspect

import (
	"strconv"
	"testing"
)

// Packets the container marks discard (an edit list trimming the start) are
// counted apart: a copy may leave them out and still have every packet a
// player plays.
func TestAudioPacketAccumulatorCountsDiscardedPackets(t *testing.T) {
	var acc AudioPacketAccumulator
	for i, flags := range []string{"KD", "KD_", "K__", "K__", "K"} {
		fields := map[string]string{"stream_index": "1", "pts_time": strconv.FormatFloat(float64(i)*0.032, 'f', 3, 64), "duration_time": "0.032", "size": "100", "flags": flags}
		if err := acc.Add(fields); err != nil {
			t.Fatal(err)
		}
	}
	// A scan that does not show the flags counts none.
	if err := acc.Add(map[string]string{"stream_index": "2", "pts_time": "0", "duration_time": "0.032", "size": "100"}); err != nil {
		t.Fatal(err)
	}
	spans, err := acc.Spans()
	if err != nil {
		t.Fatal(err)
	}
	if got := spans[1]; got.Packets != 5 || got.Discarded != 2 || got.Bytes != 500 {
		t.Fatalf("span %+v, want 5 packets, 2 discarded", got)
	}
	if got := spans[2]; got.Packets != 1 || got.Discarded != 0 {
		t.Fatalf("span without flags %+v", got)
	}
	if err := acc.Add(map[string]string{"stream_index": "1", "pts_time": "N/A", "duration_time": "0.032", "size": "100"}); err == nil {
		t.Fatal("a packet without a time was measured")
	}
}
