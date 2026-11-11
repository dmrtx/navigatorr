package transcodeworker

import (
	"errors"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func streamWithMatrix(rows ...string) mediainspect.DetailedStream {
	text := "\n"
	for i, r := range rows {
		text += "0000000" + string(rune('0'+i)) + ": " + r + "\n"
	}
	return mediainspect.DetailedStream{SideData: []mediainspect.SideDataRecord{{
		SideDataType: "Display Matrix",
		Data:         map[string]any{"displaymatrix": text},
	}}}
}

func TestDisplayOrientationEquality(t *testing.T) {
	none := mediainspect.DetailedStream{}
	identity := streamWithMatrix("65536 0 0", "0 65536 0", "0 0 1073741824")
	rot90 := streamWithMatrix("0 -65536 0", "65536 0 0", "0 0 1073741824")
	// A phone MOV keeps the picture size as translation.
	rot90Phone := streamWithMatrix("0 -65536 0", "65536 0 0", "70778880 0 1073741824")
	rotMinus90 := streamWithMatrix("0 65536 0", "-65536 0 0", "0 0 1073741824")
	rot270 := streamWithMatrix("0 65536 0", "-65536 0 0", "0 0 1073741824")
	hflip := streamWithMatrix("-65536 0 0", "0 65536 0", "0 0 1073741824")
	rot90Flip := streamWithMatrix("0 -65536 0", "-65536 0 0", "0 0 1073741824")
	unreadable := streamWithMatrix("garbage")

	for _, tc := range []struct {
		name string
		a, b mediainspect.DetailedStream
		same bool
	}{
		{"phone 90 with translation vs canonical 90", rot90Phone, rot90, true},
		{"minus 90 vs 270", rotMinus90, rot270, true},
		{"no side data vs identity", none, identity, true},
		{"90 vs none", rot90, none, false},
		{"hflip vs none", hflip, none, false},
		{"90 plus hflip vs 90", rot90Flip, rot90, false},
		{"unreadable vs none", unreadable, none, false},
		{"unreadable vs itself", unreadable, unreadable, true},
	} {
		if got := displayOrientation(tc.a) == displayOrientation(tc.b); got != tc.same {
			t.Errorf("%s: equal=%v, want %v (%+v vs %+v)", tc.name, got, tc.same, displayOrientation(tc.a), displayOrientation(tc.b))
		}
	}
	if got := displayOrientation(rotMinus90); got.degrees != 270 || got.flipped {
		t.Errorf("minus 90 = %+v, want 270 unflipped", got)
	}
}

func videoWithMatrix(rows ...string) *SourceStream {
	s := &SourceStream{Kind: "video"}
	if len(rows) > 0 {
		s.DisplayMatrix = streamWithMatrix(rows...).SideData
	}
	return s
}

func TestOrientationPreservedForEveryPlan(t *testing.T) {
	identity := []string{"65536 0 0", "0 65536 0", "0 0 1073741824"}
	rot90 := []string{"0 -65536 0", "65536 0 0", "0 0 1073741824"}
	rot90Phone := []string{"0 -65536 0", "65536 0 0", "70778880 0 1073741824"}
	for _, tc := range []struct {
		name      string
		src, cand *SourceStream
		refused   bool
	}{
		{"rotated source, candidate lost the rotation", videoWithMatrix(rot90...), videoWithMatrix(), true},
		{"rotated source, candidate shows identity", videoWithMatrix(rot90...), videoWithMatrix(identity...), true},
		{"same rotation", videoWithMatrix(rot90...), videoWithMatrix(rot90...), false},
		{"phone matrix with translation vs canonical", videoWithMatrix(rot90Phone...), videoWithMatrix(rot90...), false},
		{"no matrix vs identity", videoWithMatrix(), videoWithMatrix(identity...), false},
		{"identity vs no matrix", videoWithMatrix(identity...), videoWithMatrix(), false},
		{"plain source, candidate gained a rotation", videoWithMatrix(), videoWithMatrix(rot90...), true},
		{"no source video", nil, videoWithMatrix(rot90...), false},
	} {
		err := orientationPreserved(tc.src, tc.cand)
		if (err != nil) != tc.refused {
			t.Errorf("%s: err = %v, refused want %v", tc.name, err, tc.refused)
		}
		if err != nil && !errors.Is(err, ErrContentNotPreserved) {
			t.Errorf("%s: %v is not a preservation error", tc.name, err)
		}
	}
}
