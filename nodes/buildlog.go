package nodes

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	// buildLogMaxDropped bounds the ids remembered as "had output, dropped".
	buildLogMaxDropped = 256
)

type buildLogBuffer struct {
	lines []string
	// total is how many lines the build ever sent, kept or not: the number of
	// the last kept line. The first kept line is number total-len(lines)+1.
	total     int
	updatedAt time.Time
	// gen names this buffer: random, new each time a buffer is created for a
	// build (first output, after an eviction or retention drop, after a
	// restart). Line numbers only mean something within one generation.
	gen string
}

func newLogGeneration() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(raw)
}

// buildLogs is the in-memory store of build output tails.
type buildLogs struct {
	mu   sync.Mutex
	bufs map[string]*buildLogBuffer
	// dropped remembers the builds whose buffer retention or the cap actually
	// dropped, so a reader can tell "dropped" from "never sent anything". It
	// keeps the last buildLogMaxDropped ids, oldest out (droppedOrder).
	dropped      map[string]struct{}
	droppedOrder []string
}

// markDroppedLocked records that a build's buffer was dropped.
func (l *buildLogs) markDroppedLocked(id string) {
	if l.dropped == nil {
		l.dropped = map[string]struct{}{}
	}
	if _, ok := l.dropped[id]; ok {
		return
	}
	l.dropped[id] = struct{}{}
	l.droppedOrder = append(l.droppedOrder, id)
	if over := len(l.droppedOrder) - buildLogMaxDropped; over > 0 {
		for _, old := range l.droppedOrder[:over] {
			delete(l.dropped, old)
		}
		l.droppedOrder = append([]string(nil), l.droppedOrder[over:]...)
	}
}

// forgetDroppedLocked removes an id from the dropped set.
func (l *buildLogs) forgetDroppedLocked(id string) {
	if _, ok := l.dropped[id]; !ok {
		return
	}
	delete(l.dropped, id)
	for i, v := range l.droppedOrder {
		if v == id {
			l.droppedOrder = append(l.droppedOrder[:i:i], l.droppedOrder[i+1:]...)
			break
		}
	}
}

// checkBuildLog validates the lines of a heartbeat the way progress is checked:
// single lines, no NUL.
func checkBuildLog(lines []string) error {
	if len(lines) > BuildLogMaxBatch {
		return invalid("log must have at most %d lines per heartbeat", BuildLogMaxBatch)
	}
	for i, l := range lines {
		switch {
		case utf8.RuneCountInString(l) > BuildLogMaxLine:
			return invalid("log line %d must be at most %d characters", i+1, BuildLogMaxLine)
		case strings.ContainsAny(l, "\r\n\x00"):
			return invalid("log line %d must be a single line", i+1)
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
			l.markDroppedLocked(oldest)
		}
		l.forgetDroppedLocked(id)
		buf = &buildLogBuffer{gen: newLogGeneration()}
		l.bufs[id] = buf
	}
	buf.lines = append(buf.lines, lines...)
	buf.total += len(lines)
	if over := len(buf.lines) - BuildLogKeep; over > 0 {
		// Copy so the dropped head is released.
		buf.lines = append([]string(nil), buf.lines[over:]...)
	}
	buf.updatedAt = now
}

func (l *buildLogs) get(id string) (lines []string, updated time.Time, total int) {
	lines, updated, total, _ = l.getGen(id)
	return lines, updated, total
}

// getGen is get with the generation of the buffer ("" when there is none).
func (l *buildLogs) getGen(id string) (lines []string, updated time.Time, total int, gen string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	buf := l.bufs[id]
	if buf == nil {
		return nil, time.Time{}, 0, ""
	}
	return append([]string(nil), buf.lines...), buf.updatedAt, buf.total, buf.gen
}

// drop forgets a build altogether (its record is gone).
func (l *buildLogs) drop(id string) {
	l.mu.Lock()
	delete(l.bufs, id)
	l.forgetDroppedLocked(id)
	l.mu.Unlock()
}

// expire drops a build's buffer because its retention passed, and remembers it
// did. A build with no buffer is left alone: nothing was dropped.
func (l *buildLogs) expire(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.bufs[id]; ok {
		delete(l.bufs, id)
		l.markDroppedLocked(id)
	}
}

// wasDropped is whether retention or the cap dropped the build's buffer.
func (l *buildLogs) wasDropped(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.dropped[id]
	return ok
}

func (l *buildLogs) droppedIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.droppedOrder...)
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
func buildLogExpired(b Build, now time.Time) bool {
	if b.EndedAt == "" {
		return false
	}
	end, err := time.Parse(time.RFC3339, b.EndedAt)
	return err == nil && now.Sub(end) >= BuildLogRetention
}

// buildEnded is whether a build is over or gone. A store error that says
// nothing about the build counts as not ended, so a hiccup never makes a live
// build the first buffer to go.
func (s *Service) buildEnded(id string) bool {
	b, err := s.reg.get(id)
	if err != nil {
		return errors.Is(err, ErrBuildNotFound)
	}
	return !b.Active()
}

// BuildOutput is what a reader gets of a build's output.
type BuildOutput struct {
	Build   Build
	Lines   []string
	Updated time.Time
	// Total is how many lines the build sent while its buffer lived (the
	// number of the last line in Lines). First is the number of Lines[0]
	// (Total-len(Lines)+1; 0 when there are no lines).
	Total int
	First int
	// Generation names the buffer the numbers belong to ("" when the build has
	// no buffer). It changes whenever the buffer is recreated, which restarts
	// the numbering at 1: a restart, an eviction or the retention dropping it.
	Generation string
	// Expired is true only when a buffer this build had was dropped: its
	// retention passed or the cap on buffers pushed it out. A build that never
	// sent output, or whose output a restart forgot, is not expired.
	Expired bool
}

// BuildLog is a build's output tail: at most BuildLogKeep lines, oldest first.
// updated is the zero time when nothing was logged. An unknown build gives
// ErrBuildNotFound; a build whose retention passed gives no lines.
func (s *Service) BuildLog(id string) (build Build, lines []string, updated time.Time, err error) {
	out, err := s.BuildOutput(id)
	return out.Build, out.Lines, out.Updated, err
}

// BuildOutput is BuildLog with the reason a buffer is empty. Only a build the
// store says is unknown loses its buffer here; any other store error is
// returned and the buffer stays for the next read.
func (s *Service) BuildOutput(id string) (BuildOutput, error) {
	b, err := s.reg.get(id)
	if err != nil {
		if errors.Is(err, ErrBuildNotFound) {
			s.logs.drop(id)
		}
		return BuildOutput{}, err
	}
	if buildLogExpired(b, s.now()) {
		s.logs.expire(id)
	}
	lines, updated, total, gen := s.logs.getGen(id)
	if lines == nil {
		lines = []string{}
	}
	first := 0
	if len(lines) > 0 {
		first = total - len(lines) + 1
	}
	return BuildOutput{Build: b, Lines: lines, Updated: updated, Total: total, First: first, Generation: gen, Expired: len(lines) == 0 && s.logs.wasDropped(id)}, nil
}

// pruneBuildLogs drops the buffers of builds that are gone or whose output
// retention passed. The reconcile tick calls it.
func (s *Service) pruneBuildLogs() {
	now := s.now()
	for _, id := range append(s.logs.ids(), s.logs.droppedIDs()...) {
		b, err := s.reg.get(id)
		switch {
		case err != nil && !errors.Is(err, ErrBuildNotFound):
		case err != nil:
			s.logs.drop(id)
		case buildLogExpired(b, now):
			s.logs.expire(id)
		}
	}
}
