package transcode

import (
	"reflect"
	"testing"
)

// The worker writes an audio track's flags whenever the probe knows them,
// and a subtitle's only when the plan moved the default onto it. Flags not
// known (nil or an empty map, which a JSON round trip swaps) write nothing.
func TestWrittenDisposition(t *testing.T) {
	none := map[string]int{"default": 0, "forced": 0}
	def := map[string]int{"default": 1, "forced": 0}
	for _, tc := range []struct {
		name            string
		kind            string
		planned, source map[string]int
		want            map[string]int
	}{
		{"audio, flags known", "audio", def, def, def},
		{"audio, none set is known", "audio", none, none, none},
		{"audio, nil", "audio", nil, def, nil},
		{"audio, empty map", "audio", map[string]int{}, def, nil},
		{"subtitle carried over", "subtitle", def, def, nil},
		{"subtitle without default", "subtitle", none, none, nil},
		{"subtitle the plan made default", "subtitle", def, none, def},
		{"subtitle, empty map", "subtitle", map[string]int{}, none, nil},
		{"video", "video", def, none, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WrittenDisposition(tc.kind, tc.planned, tc.source); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ffmpeg marks a track default on its own only when the command sets no
// flags, only the first of two or more tracks of a kind, and only when none
// of them is default.
func TestInferredDefault(t *testing.T) {
	none := map[string]int{"default": 0}
	def := map[string]int{"default": 1}
	two := []map[string]int{none, none}
	for _, tc := range []struct {
		name    string
		i       int
		planned []map[string]int
		written bool
		want    bool
	}{
		{"first of two, nothing written", 0, two, false, true},
		{"flags written", 0, two, true, false},
		{"second track", 1, two, false, false},
		{"a single track", 0, []map[string]int{none}, false, false},
		{"one already default", 0, []map[string]int{none, def}, false, false},
		{"flags not known", 0, []map[string]int{nil, {}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InferredDefault(tc.i, tc.planned, tc.written); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A default ffmpeg inferred passes; any other added or lost flag is the
// first difference.
func TestDispositionChange(t *testing.T) {
	sdh := map[string]int{"hearing_impaired": 1}
	sdhDef := map[string]int{"hearing_impaired": 1, "default": 1}
	if got := DispositionChange(sdh, sdhDef, true); got != "" {
		t.Fatalf("inferred default: %q", got)
	}
	if got := DispositionChange(sdh, sdhDef, false); got != "default" {
		t.Fatalf("added default: %q", got)
	}
	if got := DispositionChange(sdhDef, sdh, true); got != "default" {
		t.Fatalf("lost default: %q", got)
	}
	if got := DispositionChange(sdh, map[string]int{"hearing_impaired": 1, "forced": 1}, true); got != "forced" {
		t.Fatalf("added forced: %q", got)
	}
	if got := DispositionChange(map[string]int{"dub": 1}, nil, false); got != "" {
		t.Fatalf("a flag Matroska cannot store: %q", got)
	}
}
