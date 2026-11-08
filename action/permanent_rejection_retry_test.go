package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

// A file job rejected for what the file is, against its goal, is not retried:
// the same goal on the same file fails the same way. That holds for a failure
// at the worker and for one at the plan step, which never had a job id.
func TestRetryRefusesAPermanentSourceRejection(t *testing.T) {
	e, st, media, _ := setupProbeTestEnv(t, &mockTranscodeExecutor{}, standard8BitProbeJSON)
	for id, failure := range map[string]string{
		"chroma":   `{"error":"Transcode failed (worker_setup_failed): quality_source_class_unsupported: native SDR 8/10-bit 4:2:0 evidence required"}`,
		"subtitle": `{"error":"resolving transcode profile \"x\": unsupported subtitle codec \"dvb_teletext\" in stream 4 for container mkv: no recipe-authorized safe conversion (fail closed)"}`,
		"range":    `{"error":"Transcode failed (worker_setup_failed): quality_source_class_unsupported: compact plan: compact_sdr_requires_limited_range"}`,
	} {
		if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_media", Status: StatusFailed, InputsJSON: toJSON(map[string]any{"path": media, "mode": "size"}), StateJSON: "{}", OutputsJSON: "{}", ErrorJSON: failure}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Retry(context.Background(), id); err == nil || !strings.Contains(err.Error(), "retry blocked") {
			t.Fatalf("%s: retry %v", id, err)
		}
		if inst, _ := st.GetActionInstance(id); inst.Status != StatusFailed {
			t.Fatalf("%s: status %s", id, inst.Status)
		}
	}
}

func TestPermanentSourceRejectionLeavesPassingFailuresAlone(t *testing.T) {
	for _, msg := range []string{
		"quality_source_class_unsupported: native SDR",
		"UNSUPPORTED SUBTITLE CODEC \"x\"",
		"cover art stream 3 has unsupported mimetype \"image/gif\" (fail closed)",
		"hdr_single_video_stream_required",
	} {
		if _, ok := PermanentSourceRejection(msg); !ok {
			t.Errorf("not permanent: %q", msg)
		}
	}
	for _, msg := range []string{
		"compact plan does not match source",
		"compact_frame_evidence_unavailable",
		"quality_vmaf_unavailable",
		"hfr_quality_unavailable",
		"Transcode failed (worker_setup_failed): copying source: connection reset",
		"Transcode failed (ffmpeg_input_corrupt): ffmpeg_input_corrupt: packet scan failed",
	} {
		if code, ok := PermanentSourceRejection(msg); ok {
			t.Errorf("%q matched %s", msg, code)
		}
	}
}
