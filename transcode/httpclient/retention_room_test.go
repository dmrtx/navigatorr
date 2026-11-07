package httpclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// The room a worker reports to retain candidates reaches the coordinator in the
// scheduler observation, and a worker that reports none (older, or with no
// budget) leaves it unset.
func TestSchedulerCarriesTheRetentionRoomTheWorkerReports(t *testing.T) {
	now := time.Now().UTC()
	body := func(retention string) string {
		return fmt.Sprintf(`{"ok":true,"scheduler":{"scheduler_health":"ok","observed_at":%q,"fresh_until":%q%s}}`,
			now.Format(time.RFC3339Nano), now.Add(time.Minute).Format(time.RFC3339Nano), retention)
	}
	for name, tc := range map[string]struct {
		retention string
		want      *transcode.RetentionRoom
	}{
		"reported": {
			retention: fmt.Sprintf(`,"retention":{"observed_at":%q,"capacity_bytes":32212254720,"room_bytes":11811160064}`, now.Format(time.RFC3339Nano)),
			want:      &transcode.RetentionRoom{ObservedAt: now, CapacityBytes: 32212254720, RoomBytes: 11811160064},
		},
		"an older worker reports none": {},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/health" {
					http.NotFound(w, r)
					return
				}
				fmt.Fprintln(w, body(tc.retention))
			}))
			defer srv.Close()
			obs, err := newTestHTTPExecutor(t, srv).Scheduler(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if obs.Health != "ok" {
				t.Fatalf("scheduler = %+v", obs)
			}
			switch {
			case tc.want == nil && obs.Retention != nil:
				t.Fatalf("retention = %+v, want none", obs.Retention)
			case tc.want != nil && (obs.Retention == nil || !obs.Retention.ObservedAt.Equal(tc.want.ObservedAt) || obs.Retention.CapacityBytes != tc.want.CapacityBytes || obs.Retention.RoomBytes != tc.want.RoomBytes):
				t.Fatalf("retention = %+v, want %+v", obs.Retention, tc.want)
			}
		})
	}
}
