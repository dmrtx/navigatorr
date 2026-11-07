package transcode

import (
	"testing"
	"time"
)

func TestRetentionRoomFit(t *testing.T) {
	const gib = int64(1) << 30
	now := time.Now().UTC()
	fresh := func(capacity, room int64) *RetentionRoom {
		return &RetentionRoom{ObservedAt: now.Add(-time.Second), CapacityBytes: capacity, RoomBytes: room}
	}
	for _, tc := range []struct {
		name string
		room *RetentionRoom
		need int64
		want RetentionFit
	}{
		{"no report is taken to have room", nil, 100 * gib, RetentionFits},
		{"nothing to retain", fresh(30*gib, 0), 0, RetentionFits},
		{"a worker with no budget reports none", fresh(0, 0), 100 * gib, RetentionFits},
		{"fits the room left", fresh(30*gib, 11*gib), 11 * gib, RetentionFits},
		{"one byte over the room left", fresh(30*gib, 11*gib), 11*gib + 1, RetentionFull},
		{"fits the whole budget but not what is left", fresh(30*gib, 11*gib), 29 * gib, RetentionFull},
		{"fits the whole budget exactly", fresh(30*gib, 30*gib), 30 * gib, RetentionFits},
		{"larger than the whole budget", fresh(30*gib, 30*gib), 30*gib + 1, RetentionTooLarge},
		{"larger than the whole budget with none left", fresh(30*gib, 0), 40 * gib, RetentionTooLarge},
		{"an old report says nothing", &RetentionRoom{ObservedAt: now.Add(-2 * RetentionRoomMaxAge), CapacityBytes: 30 * gib, RoomBytes: 0}, 29 * gib, RetentionFits},
		{"a report at the limit is still trusted", &RetentionRoom{ObservedAt: now.Add(-RetentionRoomMaxAge), CapacityBytes: 30 * gib, RoomBytes: 0}, 29 * gib, RetentionFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.room.Fit(tc.need, now); got != tc.want {
				t.Fatalf("Fit(%d) = %d, want %d", tc.need, got, tc.want)
			}
		})
	}
}
