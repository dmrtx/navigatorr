package mediainspect

import (
	"fmt"
	"math"
	"testing"
)

// Holes counts the time between consecutive packets that no packet covers: a
// millisecond of timestamp rounding is not one, a missing stretch is, and a
// packet with no duration (TrueHD in Matroska) only makes one over a tenth of
// a second.
func TestAudioPacketHoles(t *testing.T) {
	type pkt struct{ pts, dur float64 }
	run := func(pkts []pkt) AudioPacketSpan {
		t.Helper()
		var acc AudioPacketAccumulator
		for _, p := range pkts {
			dur := "N/A"
			if p.dur > 0 {
				dur = fmt.Sprintf("%.6f", p.dur)
			}
			if err := acc.Add(map[string]string{"stream_index": "1", "pts_time": fmt.Sprintf("%.6f", p.pts), "duration_time": dur, "size": "100"}); err != nil {
				t.Fatal(err)
			}
		}
		spans, err := acc.Spans()
		if err != nil {
			t.Fatal(err)
		}
		return spans[1]
	}
	steady := func(from, n int, step, dur float64) []pkt {
		var out []pkt
		for i := from; i < from+n; i++ {
			out = append(out, pkt{float64(i) * step, dur})
		}
		return out
	}
	for _, c := range []struct {
		name string
		pkts []pkt
		want float64
	}{
		{"back to back", steady(0, 500, 0.032, 0.032), 0},
		{"rounded to the millisecond", steady(0, 500, 0.02133, 0.021), 0},
		{"a stretch of 100 packets missing", append(steady(0, 150, 0.032, 0.032), steady(250, 150, 0.032, 0.032)...), 3.2},
		{"a single packet missing", append(steady(0, 150, 0.032, 0.032), steady(151, 150, 0.032, 0.032)...), 0.032},
		{"two stretches add up", append(append(steady(0, 10, 0.032, 0.032), steady(20, 10, 0.032, 0.032)...), steady(40, 10, 0.032, 0.032)...), 0.64},
		{"no duration, packets a millisecond apart", steady(0, 5000, 0.001, 0), 0},
		{"no duration, two seconds missing", append(steady(0, 1000, 0.001, 0), steady(3000, 1000, 0.001, 0)...), 2.001},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := run(c.pkts).Holes; math.Abs(got-c.want) > 0.002 {
				t.Fatalf("holes %.4f, want %.4f", got, c.want)
			}
		})
	}
}
