package transcode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateSlotSplitX265(t *testing.T) {
	for _, tc := range []struct {
		name    string
		split   SlotSplit
		ceiling int
		ok      bool
	}{
		{"no cap, no split", SlotSplit{}, 2, true},
		{"cap one on a shared ceiling of two", SlotSplit{X265Slots: 1}, 2, true},
		{"cap equal to the shared ceiling", SlotSplit{X265Slots: 2}, 2, true},
		{"cap over the shared ceiling", SlotSplit{X265Slots: 3}, 2, false},
		{"negative cap", SlotSplit{X265Slots: -1}, 2, false},
		{"cap within the video slots", SlotSplit{VideoSlots: 2, PodcastSlots: 1, X265Slots: 2}, 3, true},
		{"cap over the video slots", SlotSplit{VideoSlots: 1, PodcastSlots: 2, X265Slots: 2}, 3, false},
	} {
		_, err := ValidateSlotSplit(tc.split, tc.ceiling, 0)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	if _, err := ValidateSlotSplit(SlotSplit{X265Slots: 3}, 2, 0); err == nil || !strings.Contains(err.Error(), "x265_slots") {
		t.Errorf("the error names the field: %v", err)
	}
}

func TestValidateSlotSplitWarnsAboutPoolsUnlessX265IsCapped(t *testing.T) {
	split := SlotSplit{VideoSlots: 2, PodcastSlots: 1}
	if warnings, err := ValidateSlotSplit(split, 3, 6); err != nil || len(warnings) != 1 {
		t.Fatalf("two video slots on 6 CPUs: %v %v", warnings, err)
	}
	split.X265Slots = 1
	if warnings, err := ValidateSlotSplit(split, 3, 6); err != nil || len(warnings) != 0 {
		t.Fatalf("one x265 at a time never partitions: %v %v", warnings, err)
	}
	// A cap of two on 8 CPUs partitions into 4 threads each: nothing to warn about.
	split = SlotSplit{VideoSlots: 4, PodcastSlots: 1, X265Slots: 2}
	if warnings, err := ValidateSlotSplit(split, 5, 8); err != nil || len(warnings) != 0 {
		t.Fatalf("two x265 encodes on 8 CPUs: %v %v", warnings, err)
	}
	// ...but four video slots without a cap would run four.
	split.X265Slots = 0
	if warnings, err := ValidateSlotSplit(split, 5, 8); err != nil || len(warnings) != 1 {
		t.Fatalf("four video slots on 8 CPUs: %v %v", warnings, err)
	}
	// The warning counts the x265 encodes that can run together.
	split = SlotSplit{VideoSlots: 4, PodcastSlots: 1, X265Slots: 2}
	if warnings, err := ValidateSlotSplit(split, 5, 7); err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "2 x265 slots") {
		t.Fatalf("two x265 encodes on 7 CPUs: %v %v", warnings, err)
	}
}

func TestFitSlotSplitX265(t *testing.T) {
	// A cap that fits the shared ceiling is kept.
	if got, notes := FitSlotSplit(SlotSplit{X265Slots: 1, Ceiling: 2}, 2); got.X265Slots != 1 || len(notes) != 0 {
		t.Fatalf("%+v %v", got, notes)
	}
	// A lowered ceiling clears a cap that no longer fits.
	if got, notes := FitSlotSplit(SlotSplit{X265Slots: 3, Ceiling: 2}, 2); got.X265Slots != 0 || len(notes) != 1 {
		t.Fatalf("%+v %v", got, notes)
	}
	// A split that stops fitting returns to shared and keeps a cap that fits the ceiling.
	got, notes := FitSlotSplit(SlotSplit{VideoSlots: 2, PodcastSlots: 2, X265Slots: 2, Ceiling: 3}, 3)
	if !got.Shared() || got.X265Slots != 2 || got.Ceiling != 3 || len(notes) != 1 {
		t.Fatalf("%+v %v", got, notes)
	}
	// A split that fits keeps its cap, or loses one over its video slots.
	if got, _ := FitSlotSplit(SlotSplit{VideoSlots: 2, PodcastSlots: 1, X265Slots: 2}, 3); got.X265Slots != 2 {
		t.Fatalf("%+v", got)
	}
	if got, notes := FitSlotSplit(SlotSplit{VideoSlots: 1, PodcastSlots: 1, X265Slots: 2}, 3); got.X265Slots != 0 || len(notes) != 1 {
		t.Fatalf("%+v %v", got, notes)
	}
}

func TestSlotSplitJSONOmitsAnUnsetX265Cap(t *testing.T) {
	// A worker that predates the cap refuses the unknown field, so it must be
	// absent unless set.
	data, _ := json.Marshal(SlotSplit{VideoSlots: 1, PodcastSlots: 1})
	if strings.Contains(string(data), "x265") {
		t.Fatalf("%s", data)
	}
	data, _ = json.Marshal(SlotSplit{X265Slots: 1})
	if !strings.Contains(string(data), `"x265_slots":1`) {
		t.Fatalf("%s", data)
	}
	var old WorkerSlots
	if err := json.Unmarshal([]byte(`{"ceiling":2,"split":false,"video":{"total":2,"used":0},"podcast":{"total":2,"used":0}}`), &old); err != nil || old.X265Capable() {
		t.Fatalf("a worker without the pool is not x265 capable: %+v %v", old, err)
	}
	var cur WorkerSlots
	if err := json.Unmarshal([]byte(`{"ceiling":2,"x265":{"total":0,"used":0}}`), &cur); err != nil || !cur.X265Capable() {
		t.Fatalf("a worker with the pool is x265 capable: %+v %v", cur, err)
	}
}
