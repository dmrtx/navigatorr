package action

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jakenesler/navigatorr/store"
)

// Build output tail. A build's heartbeat may carry the new lines of its output;
// the engine keeps the last ones per build in memory only. Output can hold
// secrets, so it never reaches the database, a backup, the MCP tools or the
// worker activity, and a restart forgets it. A buffer lives while its build
// runs and BuildLogRetention after it ended.
const (
	// BuildLogMaxBatch is how many lines one heartbeat may add.
	BuildLogMaxBatch = 64
	// BuildLogMaxLine is the longest line, in characters.
	BuildLogMaxLine = 300
	// BuildLogKeep is how many lines a build keeps.
	BuildLogKeep = 200
	// BuildLogRetention is how long a build's lines stay readable once it ended.
	BuildLogRetention = 30 * time.Minute
	// buildLogMaxBuilds bounds the buffers held at once.
	buildLogMaxBuilds = 64
)

type buildLogBuffer struct {
	lines     []string
	updatedAt time.Time
}

// buildLogs is the in-memory store of build output tails.
type buildLogs struct {
	mu   sync.Mutex
	bufs map[string]*buildLogBuffer
	// evicted remembers the builds whose buffer the cap pushed out, so a reader
	// can tell "dropped" from "never sent anything". Pruned with the buffers.
	evicted map[string]struct{}
}

// checkBuildLog validates the lines of a heartbeat the way progress is checked:
// single lines, no NUL.
func checkBuildLog(lines []string) error {
	if len(lines) > BuildLogMaxBatch {
		return invalidBuild("log must have at most %d lines per heartbeat", BuildLogMaxBatch)
	}
	for i, l := range lines {
		switch {
		case utf8.RuneCountInString(l) > BuildLogMaxLine:
			return invalidBuild("log line %d must be at most %d characters", i+1, BuildLogMaxLine)
		case strings.ContainsAny(l, "\r\n\x00"):
			return invalidBuild("log line %d must be a single line", i+1)
		}
	}
	return nil
}

// append adds lines to a build's buffer. When the cap of buffers is reached the
// buffer of an ended build goes first (oldest update first), and only when none
// has ended the least recently updated one of a running build.
func (l *buildLogs) append(id string, lines []string, now time.Time, ended func(id string) bool) {
	if len(lines) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bufs == nil {
		l.bufs = map[string]*buildLogBuffer{}
	}
	buf := l.bufs[id]
	if buf == nil {
		if len(l.bufs) >= buildLogMaxBuilds {
			oldest, oldestEnded := "", false
			for k, v := range l.bufs {
				e := ended != nil && ended(k)
				switch {
				case oldest == "",
					e && !oldestEnded,
					e == oldestEnded && v.updatedAt.Before(l.bufs[oldest].updatedAt):
					oldest, oldestEnded = k, e
				}
			}
			delete(l.bufs, oldest)
			if l.evicted == nil {
				l.evicted = map[string]struct{}{}
			}
			l.evicted[oldest] = struct{}{}
		}
		delete(l.evicted, id)
		buf = &buildLogBuffer{}
		l.bufs[id] = buf
	}
	buf.lines = append(buf.lines, lines...)
	if over := len(buf.lines) - BuildLogKeep; over > 0 {
		// Copy so the dropped head is released.
		buf.lines = append([]string(nil), buf.lines[over:]...)
	}
	buf.updatedAt = now
}

func (l *buildLogs) get(id string) ([]string, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	buf := l.bufs[id]
	if buf == nil {
		return nil, time.Time{}
	}
	return append([]string(nil), buf.lines...), buf.updatedAt
}

func (l *buildLogs) drop(id string) {
	l.mu.Lock()
	delete(l.bufs, id)
	delete(l.evicted, id)
	l.mu.Unlock()
}

// wasEvicted is whether the cap pushed the build's buffer out.
func (l *buildLogs) wasEvicted(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.evicted[id]
	return ok
}

func (l *buildLogs) evictedIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.evicted))
	for k := range l.evicted {
		out = append(out, k)
	}
	return out
}

func (l *buildLogs) ids() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.bufs))
	for k := range l.bufs {
		out = append(out, k)
	}
	return out
}

// buildLogExpired is whether the build ended long enough ago for its output to
// be forgotten.
func buildLogExpired(b store.Build, now time.Time) bool {
	if b.EndedAt == "" {
		return false
	}
	end, err := time.Parse(time.RFC3339, b.EndedAt)
	return err == nil && now.Sub(end) >= BuildLogRetention
}

// buildEnded is whether a build is over or gone. A store error that says
// nothing about the build counts as not ended, so a hiccup never makes a live
// build the first buffer to go.
func (e *Engine) buildEnded(id string) bool {
	b, err := e.deps.Store.GetBuild(id)
	if err != nil {
		return errorsIsBuildNotFound(err)
	}
	return b.Status != store.BuildRunning && b.Status != store.BuildWaiting
}

// BuildOutput is what a reader gets of a build's output.
type BuildOutput struct {
	Build   store.Build
	Lines   []string
	Updated time.Time
	// Expired is true only when the output was dropped on purpose: its
	// retention passed or the cap on buffers pushed it out. A build that never
	// sent output, or whose output a restart forgot, is not expired.
	Expired bool
}

// BuildLog is a build's output tail: at most BuildLogKeep lines, oldest first.
// updated is the zero time when nothing was logged. An unknown build gives
// store.ErrBuildNotFound; a build whose retention passed gives no lines.
func (e *Engine) BuildLog(id string) (build store.Build, lines []string, updated time.Time, err error) {
	out, err := e.BuildOutput(id)
	return out.Build, out.Lines, out.Updated, err
}

// BuildOutput is BuildLog with the reason a buffer is empty. Only a build the
// store says is unknown loses its buffer here; any other store error is
// returned and the buffer stays for the next read.
func (e *Engine) BuildOutput(id string) (BuildOutput, error) {
	if e.deps.Store == nil {
		return BuildOutput{}, errStoreForBuilds
	}
	b, err := e.deps.Store.GetBuild(id)
	if err != nil {
		if errorsIsBuildNotFound(err) {
			e.buildLogs.drop(id)
		}
		return BuildOutput{}, err
	}
	if buildLogExpired(b, e.now()) {
		e.buildLogs.drop(id)
		return BuildOutput{Build: b, Lines: []string{}, Expired: true}, nil
	}
	lines, updated := e.buildLogs.get(id)
	if lines == nil {
		lines = []string{}
	}
	return BuildOutput{Build: b, Lines: lines, Updated: updated, Expired: len(lines) == 0 && e.buildLogs.wasEvicted(id)}, nil
}

// pruneBuildLogs drops the buffers of builds that are gone or whose output
// retention passed. The reconcile tick calls it.
func (e *Engine) pruneBuildLogs() {
	if e.deps.Store == nil {
		return
	}
	now := e.now()
	for _, id := range append(e.buildLogs.ids(), e.buildLogs.evictedIDs()...) {
		b, err := e.deps.Store.GetBuild(id)
		if err != nil && !errorsIsBuildNotFound(err) {
			continue
		}
		if err != nil || buildLogExpired(b, now) {
			e.buildLogs.drop(id)
		}
	}
}
