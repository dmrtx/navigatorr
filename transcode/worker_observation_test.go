package transcode

import (
	"testing"
	"time"
)

func TestSchedulerFreshnessCannotTrustStaleHealthClaim(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []SchedulerObservation{{Health: "ok"}, {Health: "ok", ObservedAt: now.Add(-time.Minute)}, {Health: "ok", ObservedAt: now, Stale: true}, {Health: "ok", ObservedAt: now.Add(time.Second)}, {Health: "ok", ObservedAt: now.Add(-time.Second), FreshUntil: now.Add(-time.Millisecond)}} {
		got := NormalizeSchedulerObservation(tc, now)
		if got.Health != "unknown" || !got.Stale {
			t.Fatalf("stale worker looked healthy: %+v", got)
		}
	}
	got := NormalizeSchedulerObservation(SchedulerObservation{Health: "degraded", ObservedAt: now.Add(-time.Second), FreshUntil: now.Add(time.Second)}, now)
	if got.Health != "degraded" || got.Stale || got.ObservationAgeSeconds == nil || *got.ObservationAgeSeconds != 1 {
		t.Fatal(got)
	}
}
