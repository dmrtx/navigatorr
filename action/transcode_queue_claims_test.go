package action

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func claimOn(workers ...int) queueClaim {
	c := queueClaim{reach: make([]bool, 2), usable: make([]bool, 2)}
	for _, w := range workers {
		c.reach[w], c.usable[w] = true, true
	}
	return c
}

func twoWorkerSlots(freeFirst, freeSecond int) queueSlots {
	return queueSlots{
		workers: []transcode.NamedWorker{{Name: "w0"}, {Name: "w1"}},
		ready:   []bool{true, true},
		free:    map[string]int{"w0": freeFirst, "w1": freeSecond},
	}
}

func TestPlacedGivesEachClaimASlotOnAWorkerItCanUse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims []queueClaim
		free   []int
		want   int
	}{
		{"nothing ahead", nil, []int{1, 1}, 0},
		{"no slot", []queueClaim{claimOn(0, 1)}, []int{0, 0}, 0},
		{"each on its own worker", []queueClaim{claimOn(0), claimOn(1)}, []int{1, 1}, 2},
		{"two for the one worker with a slot", []queueClaim{claimOn(0), claimOn(0)}, []int{1, 1}, 1},
		{"a claim moves aside for one that has a single choice", []queueClaim{claimOn(0, 1), claimOn(0)}, []int{1, 1}, 2},
		{"more claims than slots", []queueClaim{claimOn(0, 1), claimOn(0, 1), claimOn(0, 1)}, []int{1, 1}, 2},
		{"a claim that reaches no worker places nowhere", []queueClaim{{reach: make([]bool, 2), usable: make([]bool, 2)}, claimOn(0)}, []int{1, 1}, 1},
		{"a worker with two slots", []queueClaim{claimOn(0), claimOn(0), claimOn(0)}, []int{2, 1}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := placed(tc.claims, tc.free); got != tc.want {
				t.Fatalf("placed = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSafeWorkersAreThoseWhoseSlotNoClaimAheadNeeds(t *testing.T) {
	q := twoWorkerSlots(1, 1)
	for _, tc := range []struct {
		name  string
		ahead []queueClaim
		cur   queueClaim
		want  map[string]bool
	}{
		{"nobody ahead", nil, claimOn(0, 1), map[string]bool{"w0": true, "w1": true}},
		{"one ahead that can use either", []queueClaim{claimOn(0, 1)}, claimOn(0, 1), map[string]bool{"w0": true, "w1": true}},
		{"one ahead that only w0 can hold", []queueClaim{claimOn(0)}, claimOn(0, 1), map[string]bool{"w1": true}},
		{"two ahead that can use either leave nothing", []queueClaim{claimOn(0, 1), claimOn(0, 1)}, claimOn(0, 1), map[string]bool{}},
		{"ahead needs w0, the next one needs w1", []queueClaim{claimOn(0)}, claimOn(1), map[string]bool{"w1": true}},
		{"the claim can only use the slot the one ahead needs", []queueClaim{claimOn(0)}, claimOn(0), map[string]bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := q.safeWorkers(tc.cur, tc.ahead)
			if len(got) != len(tc.want) {
				t.Fatalf("safe = %v, want %v", got, tc.want)
			}
			for name := range tc.want {
				if !got[name] {
					t.Fatalf("safe = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSlotsLeftAfterTheClaimsAhead(t *testing.T) {
	q := twoWorkerSlots(1, 1)
	for name, tc := range map[string]struct {
		ahead []queueClaim
		want  int
	}{
		"nobody ahead":                  {nil, 2},
		"one that can use either":       {[]queueClaim{claimOn(0, 1)}, 1},
		"two ahead":                     {[]queueClaim{claimOn(0, 1), claimOn(0, 1)}, 0},
		"two that want the same worker": {[]queueClaim{claimOn(0), claimOn(0)}, 1},
		"more ahead than slots":         {[]queueClaim{claimOn(0, 1), claimOn(0, 1), claimOn(0, 1)}, 0},
	} {
		if got := q.slotsLeftAfter(tc.ahead); got != tc.want {
			t.Errorf("%s: slotsLeftAfter = %d, want %d", name, got, tc.want)
		}
	}
}
