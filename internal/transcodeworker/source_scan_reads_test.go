package transcodeworker

import "testing"

func TestSourceScanFullReads(t *testing.T) {
	if got := sourceScanFullReads(SourceProbe{}); got != 1 {
		t.Errorf("no subtitles = %d, want 1", got)
	}
	probe := SourceProbe{subtitlePackets: map[int]subtitlePackets{2: {}}}
	if got := sourceScanFullReads(probe); got != 1 {
		t.Errorf("subtitles scanned, none counted = %d, want 1", got)
	}
	probe.subtitlePackets[3] = subtitlePackets{counted: true}
	if got := sourceScanFullReads(probe); got != 2 {
		t.Errorf("a converted subtitle counted = %d, want 2", got)
	}
}
