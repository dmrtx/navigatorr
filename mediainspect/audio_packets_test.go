package mediainspect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakePacketProbe(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The packets give each stream's span and bytes; a stream whose packets carry
// no duration ends one packet gap after its last packet, as the worker counts.
func TestMeasureAudioPacketsSpansAndBytes(t *testing.T) {
	ffprobe := fakePacketProbe(t, `cat <<'P'
stream_index=1|pts_time=0.000000|duration_time=0.021333|size=400
stream_index=2|pts_time=0.010000|duration_time=N/A|size=100
stream_index=1|pts_time=19.978667|duration_time=0.021333|size=600
stream_index=2|pts_time=0.020000|duration_time=N/A|size=100
P
`)
	spans, err := MeasureAudioPackets(context.Background(), ffprobe, "candidate.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if s := spans[1]; s.Start != 0 || s.End != 20 || s.Bytes != 1000 || s.BitsPerSecond() != 400 {
		t.Fatalf("stream 1 %+v", s)
	}
	if s := spans[2]; s.Start != 0.01 || s.End < 0.0299 || s.End > 0.0301 || s.Bytes != 200 {
		t.Fatalf("stream 2 %+v", s)
	}
}

// A scan that cannot be read, or fails, is a ProbeError: nothing was measured.
func TestMeasureAudioPacketsFailures(t *testing.T) {
	for name, body := range map[string]string{
		"unparseable": "echo 'stream_index=1|pts_time=N/A|duration_time=N/A|size=0'\n",
		"exit":        "echo broken >&2; exit 1\n",
		"open end":    "echo 'stream_index=1|pts_time=0.0|duration_time=N/A|size=10'\n",
	} {
		_, err := MeasureAudioPackets(context.Background(), fakePacketProbe(t, body), "candidate.mkv")
		if _, ok := AsProbeError(err); !ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A scan that stops reading packets is killed, not waited for.
func TestMeasureAudioPacketsStall(t *testing.T) {
	old := audioPacketStallTimeout
	audioPacketStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { audioPacketStallTimeout = old })
	start := time.Now()
	_, err := MeasureAudioPackets(context.Background(), fakePacketProbe(t, "exec sleep 30\n"), "candidate.mkv")
	pe, ok := AsProbeError(err)
	if !ok || pe.Kind != ProbeTimeout || time.Since(start) > 10*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}
