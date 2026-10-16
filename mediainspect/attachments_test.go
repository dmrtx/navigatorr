package mediainspect

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestAttachmentPayloadBytesFromObservedFontProbe(t *testing.T) {
	probe, err := os.ReadFile("testdata/font-attachments-18.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	media := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(media, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := InspectDetailed(context.Background(), createMockFFprobe(t, dir, string(probe)), media)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Probed || len(rep.Attachments) != 18 {
		t.Fatalf("lost streams: %+v", rep)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var reopened DetailedReport
	if err := json.Unmarshal(data, &reopened); err != nil {
		t.Fatal(err)
	}
	got, err := AttachmentPayloadBytes(reopened.Attachments)
	if err != nil || got != 5046808 {
		t.Fatalf("attachments=%d, err=%v; want 5046808", got, err)
	}
}

func TestAttachmentPayloadBytesFallbackAndInvalidSizes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		streams []DetailedStream
		want    int64
		wantErr bool
	}{
		{"absent", nil, 0, false},
		{"extradata wins without double counting", []DetailedStream{{ExtraDataSize: 40, Tags: map[string]string{"NUMBER_OF_BYTES": "100"}}}, 40, false},
		{"legacy tag fallback", []DetailedStream{{Tags: map[string]string{"NUMBER_OF_BYTES": " 100 "}}}, 100, false},
		{"mixed sources", []DetailedStream{{ExtraDataSize: 40}, {Tags: map[string]string{"NUMBER_OF_BYTES": "100"}}}, 140, false},
		{"missing is not zero", []DetailedStream{{Index: 12}}, 0, true},
		{"zero is unknown", []DetailedStream{{Tags: map[string]string{"NUMBER_OF_BYTES": "0"}}}, 0, true},
		{"negative extradata", []DetailedStream{{ExtraDataSize: -1}}, 0, true},
		{"negative tag", []DetailedStream{{Tags: map[string]string{"NUMBER_OF_BYTES": "-1"}}}, 0, true},
		{"invalid tag", []DetailedStream{{Tags: map[string]string{"NUMBER_OF_BYTES": "N/A"}}}, 0, true},
		{"overflow", []DetailedStream{{ExtraDataSize: math.MaxInt64}, {ExtraDataSize: 1}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AttachmentPayloadBytes(tc.streams)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %d, %v; want %d error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
