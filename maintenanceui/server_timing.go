package maintenanceui

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// serverTiming collects where a slow listing spent its time and sends it as a
// Server-Timing header, which the web console's network panel and the iOS app
// can both read. It carries durations and a cache verdict only.
type serverTiming struct {
	began time.Time
	parts []string
}

func newServerTiming() *serverTiming { return &serverTiming{began: time.Now()} }

// since records a step that began at start.
func (t *serverTiming) since(name string, start time.Time) {
	t.parts = append(t.parts, fmt.Sprintf("%s;dur=%.1f", name, float64(time.Since(start).Microseconds())/1000))
}

// note records a step that took no time of its own, such as a cache verdict.
func (t *serverTiming) note(name, desc string) {
	t.parts = append(t.parts, fmt.Sprintf("%s;dur=0;desc=%q", name, desc))
}

// set must be called before the response is written.
func (t *serverTiming) set(w http.ResponseWriter) {
	total := fmt.Sprintf("total;dur=%.1f", float64(time.Since(t.began).Microseconds())/1000)
	w.Header().Set("Server-Timing", strings.Join(append(t.parts, total), ", "))
}
