package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// The worker keeps its own wording for a failed audio packet scan, and the
// failure is not a media_probe_failed one: that class belongs to a source
// ffprobe could not read, and a packet scan that came out wrong is not that.
func TestAudioPacketSpansKeepTheWorkersWordingAndClass(t *testing.T) {
	dir := t.TempDir()
	probe := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, ffprobe, want string
	}{
		{"unparseable packets", probe("unparseable", "echo 'stream_index=1|pts_time=N/A|duration_time=N/A|size=0'\n"), "audio packet timing/size unavailable"},
		{"tool exit", probe("exit", "echo broken >&2; exit 1\n"), "audio packet probe failed: exit status 1"},
		{"tool missing", filepath.Join(dir, "missing-ffprobe"), "no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := audioPacketSpans(context.Background(), tc.ffprobe, "candidate.mkv")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "mediainspect") {
				t.Fatalf("a probe error's own rendering leaked into the wording: %v", err)
			}
			if got := resilience.ClassifyError(err); got == resilience.MediaProbeFailed {
				t.Fatalf("class %s: a failed packet scan must not read as media_probe_failed (%v)", got, err)
			}
		})
	}
}
