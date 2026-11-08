package action

import (
	"context"
	"path/filepath"
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
		"chroma":   `{"error":"Transcode failed (worker_setup_failed): quality_source_class_unsupported: native SDR 8/10-bit 4:2:0, 4:2:2 or 4:4:4 evidence at a measurable frame rate"}`,
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
		"quality_source_class_unsupported: native SDR 8/10-bit 4:2:0, 4:2:2 or 4:4:4 evidence",
		"Transcode failed (worker_setup_failed): quality_source_class_unsupported: source/candidate measurement class is unsupported",
		"UNSUPPORTED SUBTITLE CODEC \"x\"",
		"cover art stream 3 has unsupported mimetype \"image/gif\" (fail closed)",
		"hdr_single_video_stream_required",
	} {
		if _, ok := PermanentSourceRejection(msg, nil, nil); !ok {
			t.Errorf("not permanent: %q", msg)
		}
	}
	for _, msg := range []string{
		"compact plan does not match source",
		"Transcode failed (worker_setup_failed): quality_source_class_unsupported: compact plan does not match source: range changed",
		"Transcode failed (worker_setup_failed): quality_source_class_unsupported: ffprobe timed out",
		"failed to probe original media /media/unsupported_source.mkv (fail closed): timeout",
		"failed to probe original media /media/hdr_requires_limited_range/a.mkv (fail closed): timeout",
		"compact_frame_evidence_unavailable",
		"quality_vmaf_unavailable",
		"hfr_quality_unavailable",
		"Transcode failed (worker_setup_failed): copying source: connection reset",
		"Transcode failed (ffmpeg_input_corrupt): ffmpeg_input_corrupt: packet scan failed",
	} {
		if code, ok := PermanentSourceRejection(msg, nil, nil); ok {
			t.Errorf("%q matched %s", msg, code)
		}
	}
	// A file named after a code, between brackets or spaces, does not make
	// its timeout permanent, wherever the path shows up.
	for _, name := range []string{"/media/Show [unsupported_source].mkv", "/media/Movie (unsupported subtitle codec) x.mkv"} {
		inputs := map[string]any{"path": name}
		state := map[string]any{"resolved_path": "/mnt/nas" + name}
		for _, msg := range []string{
			"failed to probe original media " + name + " (fail closed): timeout",
			"Transcode failed (worker_setup_failed): copying /mnt/nas" + name + ": connection reset",
			"Transcode failed (worker_setup_failed): reading /scratch/job-1/" + filepath.Base(name) + ": timeout",
		} {
			if code, ok := PermanentSourceRejection(msg, inputs, state); ok {
				t.Errorf("%q matched %s", msg, code)
			}
		}
		if _, ok := PermanentSourceRejection("resolving: unsupported subtitle codec \"x\" for "+name, inputs, state); !ok {
			t.Errorf("a real rejection of %s was missed", name)
		}
	}
}
