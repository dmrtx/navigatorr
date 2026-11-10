package transcodeworker

import (
	"context"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// prescanSource is the scan the job runs on the source before the encode
// (scanSourcePackets): the candidate checks compare against its measures.
func prescanSource(t *testing.T, w *Worker, plan *transcode.Plan, probe SourceProbe, path string) SourceProbe {
	t.Helper()
	out, err := w.scanSourcePackets(context.Background(), plan, probe, path)
	if err != nil {
		t.Fatalf("source packet scan: %v", err)
	}
	return out
}

// audioPacketsCheck runs the audio packet check the candidate validation
// runs: the source's spans as measured before the encode (measured here when
// src carries none), against the candidate's.
func audioPacketsCheck(t *testing.T, w *Worker, plan *transcode.Plan, source, candidate string, src, dst SourceProbe) error {
	t.Helper()
	if src.audioSpans == nil {
		spans, err := w.audioPacketSpansForJob(context.Background(), source)
		if err != nil {
			return err
		}
		src.audioSpans = spans
	}
	cand, err := w.audioPacketSpansForJob(context.Background(), candidate)
	if err != nil {
		return err
	}
	return validateAudioPackets(plan, src, dst, cand)
}
