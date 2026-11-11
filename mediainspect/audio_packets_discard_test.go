package mediainspect

import (
	"strconv"
	"testing"
)

// Packets the container marks discard (an edit list trimming the start) are
// counted apart and never measured: a copy may leave them out and still have
// every packet a player plays, and as long a track.
func TestAudioPacketAccumulatorCountsDiscardedPackets(t *testing.T) {
	var acc AudioPacketAccumulator
	for i, flags := range []string{"KD", "KD_", "K__", "K__", "K"} {
		fields := map[string]string{"stream_index": "1", "pts_time": strconv.FormatFloat(float64(i)*0.5-1, 'f', 3, 64), "duration_time": "0.5", "size": "100", "flags": flags}
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
	// The discarded packets ran from -1 s to 0 s: the played span is 0-1.5 s.
	if got := spans[1]; got.Packets != 5 || got.Discarded != 2 || got.Timed != 3 || got.Bytes != 300 || got.Start != 0 || got.End != 1.5 {
		t.Fatalf("span %+v, want 5 packets, 2 discarded, 0-1.5 s", got)
	}
	if got := spans[2]; got.Packets != 1 || got.Discarded != 0 {
		t.Fatalf("span without flags %+v", got)
	}
}

// A packet without a time, a packet stated to last nothing and an empty
// packet are what real tracks carry: each is counted, none fails the scan,
// and only a timed packet places the span.
func TestAudioPacketAccumulatorUntimedAndEmptyPackets(t *testing.T) {
	var acc AudioPacketAccumulator
	for _, p := range [][3]string{
		{"N/A", "0.032", "100"},      // no time: counted apart
		{"0.000", "0.032", "100"},    // a normal packet
		{"0.032", "0.000000", "0"},   // stated to last nothing, and empty
		{"0.064", "N/A", "100"},      // no duration: ends at the next packet
		{"0.096", "0.000000", "100"}, // the last, lasting nothing: one gap more
	} {
		if err := acc.Add(map[string]string{"stream_index": "1", "pts_time": p[0], "duration_time": p[1], "size": p[2]}); err != nil {
			t.Fatalf("%v: %v", p, err)
		}
	}
	spans, err := acc.Spans()
	if err != nil {
		t.Fatal(err)
	}
	got := spans[1]
	if got.Packets != 5 || got.Untimed != 1 || got.Timed != 4 || got.Bytes != 300 || got.Start != 0 || got.End < 0.1279 || got.End > 0.1281 {
		t.Fatalf("span %+v, want 5 packets, 1 untimed, 0-0.128 s", got)
	}
	for _, bad := range []map[string]string{
		{"stream_index": "1", "pts_time": "0", "duration_time": "-1", "size": "1"},
		{"stream_index": "1", "pts_time": "0", "duration_time": "0.1", "size": "-1"},
		{"stream_index": "x", "pts_time": "0", "duration_time": "0.1", "size": "1"},
		{"stream_index": "1", "pts_time": "zero", "duration_time": "0.1", "size": "1"},
	} {
		if err := acc.Add(bad); err == nil {
			t.Errorf("%v measured", bad)
		}
	}
}
