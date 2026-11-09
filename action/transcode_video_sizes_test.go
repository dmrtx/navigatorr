package action

import (
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestAddVideoSizes(t *testing.T) {
	orig := map[string]any{"video": []mediainspect.DetailedStream{
		{Kind: "video", Codec: "mjpeg", Width: 600, Height: 900, Disposition: map[string]int{"attached_pic": 1}},
		{Kind: "video", Codec: "hevc", Width: 4096, Height: 1716},
	}}
	sizes := func(r map[string]any) [4]any {
		return [4]any{r["source_width"], r["source_height"], r["candidate_width"], r["candidate_height"]}
	}

	// A plan that does not resize: the candidate keeps the programme video's size.
	r := map[string]any{}
	addVideoSizes(r, orig, &transcode.Plan{}, 0, 0)
	if sizes(r) != [4]any{4096, 1716, 4096, 1716} || r["resized"] != nil {
		t.Errorf("plain plan: %v", r)
	}

	// A compact plan that keeps a source no taller than max_height.
	d := mediainspect.DynamicRange{Class: mediainspect.DynamicRangeHDR10, Primaries: "bt2020", Matrix: "bt2020nc"}
	keep, err := transcode.NewCompactPlan(d, mediainspect.DetailedStream{Kind: "video", Width: 4096, Height: 1716}, 2160)
	if err != nil {
		t.Fatal(err)
	}
	r = map[string]any{}
	addVideoSizes(r, orig, &transcode.Plan{Compact: keep}, 0, 0)
	if sizes(r) != [4]any{4096, 1716, 4096, 1716} || r["resized"] != false {
		t.Errorf("kept compact: %v", r)
	}

	// A compact plan that shrinks a taller source, with the probed output.
	shrink, err := transcode.NewCompactPlan(d, mediainspect.DetailedStream{Kind: "video", Width: 4096, Height: 1716}, 1080)
	if err != nil {
		t.Fatal(err)
	}
	r = map[string]any{}
	addVideoSizes(r, orig, &transcode.Plan{Compact: shrink}, 1920, 804)
	if sizes(r) != [4]any{4096, 1716, 1920, 804} || r["resized"] != true {
		t.Errorf("shrunk compact: %v", r)
	}

	// Without a source report nothing is guessed.
	r = map[string]any{}
	addVideoSizes(r, nil, &transcode.Plan{}, 0, 0)
	if len(r) != 0 {
		t.Errorf("no source: %v", r)
	}
}
