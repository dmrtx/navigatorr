package action

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func TestDetailedStream_JSONRoundTripPersistence(t *testing.T) {
	origStreams := []mediainspect.DetailedStream{
		{
			Index:    0,
			Kind:     "video",
			Codec:    "hevc",
			Language: "eng",
			Title:    "Main Feature",
			Channels: 0,
			Width:    1920,
			Height:   1080,
			BitDepth: 10,
			Tags: map[string]string{
				"title":        "Main Feature",
				"HANDLER_NAME": "VideoHandler",
				"DURATION":     "01:30:00.000",
			},
			Disposition: map[string]int{
				"default": 1,
				"forced":  0,
			},
		},
		{
			Index:    1,
			Kind:     "audio",
			Codec:    "aac",
			Language: "jpn",
			Title:    "Stereo Commentary",
			Channels: 2,
			Width:    0,
			Height:   0,
			BitDepth: 0,
			Tags: map[string]string{
				"language": "jpn",
				"title":    "Stereo Commentary",
			},
			Disposition: map[string]int{
				"default": 0,
				"forced":  0,
			},
		},
	}

	// Persisted action state in SQLite store serializes ec.State (including the "original" inspect map)
	// to JSON and restores it as generic map[string]any / []any structures upon resume or restart.
	state := map[string]any{
		"original": map[string]any{
			"video": []mediainspect.DetailedStream{origStreams[0]},
			"audio": []mediainspect.DetailedStream{origStreams[1]},
		},
		"video": origStreams,
	}

	// Simulate store serialization to JSON and unmarshaling back into generic map[string]any
	rawJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("failed to marshal state: %v", err)
	}

	var roundTripped map[string]any
	if err := json.Unmarshal(rawJSON, &roundTripped); err != nil {
		t.Fatalf("failed to unmarshal state: %v", err)
	}

	origMap, ok := roundTripped["original"].(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any for 'original', got %T", roundTripped["original"])
	}

	restoredVideo := getStreamsList(origMap, "video")
	if len(restoredVideo) != 1 {
		t.Fatalf("expected 1 restored video stream, got %d", len(restoredVideo))
	}

	v := restoredVideo[0]
	origV := origStreams[0]

	// Prove that Width, Height, BitDepth, Tags, and Disposition survive the JSON round trip
	if v.Width != origV.Width {
		t.Errorf("Width did not survive: got %d, want %d", v.Width, origV.Width)
	}
	if v.Height != origV.Height {
		t.Errorf("Height did not survive: got %d, want %d", v.Height, origV.Height)
	}
	if v.BitDepth != origV.BitDepth {
		t.Errorf("BitDepth did not survive: got %d, want %d", v.BitDepth, origV.BitDepth)
	}
	if !reflect.DeepEqual(v.Tags, origV.Tags) {
		t.Errorf("Tags did not survive: got %#v, want %#v", v.Tags, origV.Tags)
	}
	if !reflect.DeepEqual(v.Disposition, origV.Disposition) {
		t.Errorf("Disposition did not survive: got %#v, want %#v", v.Disposition, origV.Disposition)
	}
	if v.Index != origV.Index || v.Kind != origV.Kind || v.Codec != origV.Codec || v.Language != origV.Language || v.Title != origV.Title {
		t.Errorf("Basic stream fields mismatch: got %+v, want %+v", v, origV)
	}

	// Also verify audio stream tags and disposition survive
	restoredAudio := getStreamsList(origMap, "audio")
	if len(restoredAudio) != 1 {
		t.Fatalf("expected 1 restored audio stream, got %d", len(restoredAudio))
	}
	a := restoredAudio[0]
	origA := origStreams[1]
	if a.Channels != origA.Channels {
		t.Errorf("Audio Channels mismatch: got %d, want %d", a.Channels, origA.Channels)
	}
	if !reflect.DeepEqual(a.Tags, origA.Tags) {
		t.Errorf("Audio Tags did not survive: got %#v, want %#v", a.Tags, origA.Tags)
	}
	if !reflect.DeepEqual(a.Disposition, origA.Disposition) {
		t.Errorf("Audio Disposition did not survive: got %#v, want %#v", a.Disposition, origA.Disposition)
	}

	// Verify top-level stream list round-trip preserves all fields across both streams
	restoredTopLevel := getStreamsList(roundTripped, "video")
	if len(restoredTopLevel) != len(origStreams) {
		t.Fatalf("expected %d direct streams, got %d", len(origStreams), len(restoredTopLevel))
	}
	if !reflect.DeepEqual(restoredTopLevel, origStreams) {
		t.Errorf("Top-level restored streams mismatch: got %#v, want %#v", restoredTopLevel, origStreams)
	}
}
