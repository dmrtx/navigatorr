package transcodeworker

import (
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
