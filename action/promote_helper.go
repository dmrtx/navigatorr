package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/internal/nashelper"
	"github.com/jakenesler/navigatorr/internal/sampledigest"
)

// The NAS helper hashes a whole media file where it lives, so the bytes never
// cross the network (docs/nas-helper.md, "Stage 4 design"). promotionHash keeps
// every metadata check it always made against the mount and asks the helper only
// for the byte loop; anything the helper cannot answer is read through the mount
// as before, and the integrity record says which way it went.

// helperStallAfter is how long a helper hash may go without a byte of progress
// (queued behind other hashes, or starved of disk by idle I/O priority) before
// the read goes through the mount instead. Tests shorten it.
var helperStallAfter = 5 * time.Minute

const (
	// helperDownFor is how long a helper that could not be reached is left alone,
	// so a down helper costs one timeout, not one per file.
	helperDownFor = 2 * time.Minute
	// helperBusyRetries and helperAskAgain bound the patience for a helper that
	// answers "busy" (a rename in flight) or ended the job itself.
	helperBusyRetries = 3
	helperAskAgain    = 1
)

// promotionHelperDisagreeError is the helper and the mount describing different
// bytes under one path: almost always a path mapping that points at the wrong
// place. It is an integrity failure, never a reason to read through the mount.
type promotionHelperDisagreeError struct{ path, what string }

func (e *promotionHelperDisagreeError) Error() string {
	return fmt.Sprintf("the NAS helper and the mount do not agree about %s (%s); check nas_helper.path_mappings", e.path, e.what)
}

// helperBreaker leaves an unreachable helper alone for a while.
type helperBreaker struct {
	mu   sync.Mutex
	down time.Time
}

func (b *helperBreaker) open(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.Before(b.down)
}

func (b *helperBreaker) trip(now time.Time) {
	b.mu.Lock()
	b.down = now.Add(helperDownFor)
	b.mu.Unlock()
}

func (b *helperBreaker) reset() {
	b.mu.Lock()
	b.down = time.Time{}
	b.mu.Unlock()
}

type promotionExecKey struct{}

// promotionRecordRead notes, in the execution's durable state, how a whole-file
// read was done. Unlike the verified-file cache it survives
// withoutPromotionVerifiedFiles, because the reads that authorise removing
// recovery data are the ones whose path matters most.
func promotionRecordRead(ctx context.Context, path, via, class string, complete bool) {
	ec, _ := ctx.Value(promotionExecKey{}).(*ExecutionContext)
	if ec == nil || ec.State == nil {
		return
	}
	reads, _ := ec.State["integrity_reads"].(map[string]any)
	if reads == nil {
		reads = map[string]any{}
		ec.State["integrity_reads"] = reads
	}
	rec := map[string]any{"via": via, "at": time.Now().UTC().Format(time.RFC3339Nano)}
	if class != "" {
		rec["reason"] = class
	}
	if via == "nas_helper" {
		rec["identity_complete"] = complete
	}
	reads[path] = rec
}

// helperProgress turns the helper's progress into the engine's observer calls,
// remembers an observer failure (which must abort the read, as it does for a
// mount read) and notices a hash that has stopped advancing.
type helperProgress struct {
	path     string
	total    int64
	observer promotionHashProgress
	cancel   context.CancelFunc

	mu        sync.Mutex
	last      int64
	advanced  time.Time
	observed  error
	stalled   bool
	finishedC chan struct{}
}

func (h *helperProgress) report(read, _ int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if read > h.last {
		h.last, h.advanced = read, time.Now()
	}
	if h.observer == nil || h.observed != nil {
		return
	}
	if err := h.observer(h.path, h.last, h.total); err != nil {
		h.observed = err
		h.cancel()
	}
}

func (h *helperProgress) watch(ctx context.Context, after time.Duration) {
	t := time.NewTicker(max(after/10, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.finishedC:
			return
		case <-t.C:
			h.mu.Lock()
			if time.Since(h.advanced) > after {
				h.stalled = true
				h.mu.Unlock()
				h.cancel()
				return
			}
			h.mu.Unlock()
		}
	}
}

func (h *helperProgress) state() (observed error, stalled bool, read int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.observed, h.stalled, h.last
}

func sameHelperIdentity(a, b nashelper.Identity) bool {
	a.Complete, b.Complete = false, false
	return a == b
}

// promotionHelperHash hashes path through the NAS helper while f, already open
// through the mount, stays the reference for what the mount serves. handled is
// false when the caller must read through the mount itself; any other return is
// the answer or the error of the read.
//
// The helper hash is bracketed so that one identity covers it: the mount's
// descriptor is opened first, the helper hashes (identity A), the mount's sampled
// ranges are read from the same descriptor, and the helper samples the file again
// and must report identity A and the same sample digest.
func (e *Engine) promotionHelperHash(ctx context.Context, path, resolved string, f *os.File, opened os.FileInfo, observer promotionHashProgress) (sum string, n int64, proof *helperProof, handled bool, err error) {
	client := e.deps.NASHelper
	if client == nil {
		return "", 0, nil, false, nil
	}
	fallback := func(class string, cause error) (string, int64, *helperProof, bool, error) {
		log.Printf("promotion: the NAS helper could not hash %s (%s: %v); reading it through the mount", path, class, cause)
		promotionRecordRead(ctx, path, "smb_fallback", class, false)
		return "", 0, nil, false, nil
	}
	if _, mapped := client.Remote(resolved); !mapped {
		return "", 0, nil, false, nil
	}
	if e.helperBreaker.open(time.Now()) {
		return fallback("helper_down", errors.New("recently unreachable"))
	}
	// Queueing at the helper must not hold a dispatcher slot, or live jobs stop
	// being polled while a hash waits its turn.
	yieldReconcileSlot(ctx)

	var res nashelper.HashResponse
	asked, busy := 0, 0
	for {
		hashCtx, cancel := context.WithCancel(ctx)
		prog := &helperProgress{path: path, total: opened.Size(), observer: observer, cancel: cancel, advanced: time.Now(), finishedC: make(chan struct{})}
		if observer != nil {
			if oerr := observer(path, 0, opened.Size()); oerr != nil {
				cancel()
				return "", 0, nil, true, oerr
			}
		}
		go prog.watch(hashCtx, helperStallAfter)
		res, err = client.Hash(hashCtx, resolved, prog.report)
		close(prog.finishedC)
		cancel()
		observed, stalled, readSoFar := prog.state()
		switch {
		case observed != nil:
			return "", 0, nil, true, observed
		case ctx.Err() != nil:
			return "", 0, nil, true, ctx.Err()
		case err == nil:
			// A hash that finished is good even if the watchdog fired in the same
			// instant.
		case stalled:
			// A job that was reading and stopped means a helper in trouble: leave
			// it alone for a while. One still queued is only waiting its turn.
			if readSoFar > 0 {
				e.helperBreaker.trip(time.Now())
			}
			return fallback("stalled", errors.New("no progress"))
		case nashelper.IsAnswer(err):
			var apiErr *nashelper.Error
			errors.As(err, &apiErr)
			if apiErr.Code == nashelper.CodeChanged {
				return "", 0, nil, true, &promotionFileChangedError{path: path, phase: "hashing"}
			}
			// The mount opened this file a moment ago, so a helper that says it is
			// not there is looking somewhere else: say so and read it ourselves.
			return fallback("helper_disagrees", err)
		case errors.Is(err, nashelper.ErrRestarted) || errors.Is(err, nashelper.ErrHelperCancelled):
			if asked < helperAskAgain {
				asked++
				continue
			}
			return fallback("helper_restarted", err)
		case isHelperBusy(err):
			if busy < helperBusyRetries {
				busy++
				select {
				case <-time.After(time.Duration(busy) * 200 * time.Millisecond):
					continue
				case <-ctx.Done():
					return "", 0, nil, true, ctx.Err()
				}
			}
			return fallback("helper_busy", err)
		default:
			if nashelper.ShouldFallBack(err) {
				if errors.Is(err, nashelper.ErrUnavailable) && !errors.Is(err, nashelper.ErrNotMapped) {
					e.helperBreaker.trip(time.Now())
				}
				return fallback("helper_unavailable", err)
			}
			return "", 0, nil, true, err
		}
		break
	}
	e.helperBreaker.reset()

	// The mount and the helper must describe the same bytes. The mount side is
	// read from the descriptor the mount checks already vouch for.
	mountSample, serr := sampledigest.Digest(ctx, f, opened.Size())
	if serr != nil {
		if ctx.Err() != nil {
			return "", 0, nil, true, ctx.Err()
		}
		return "", 0, nil, true, serr
	}
	helperSample, herr := client.Sample(ctx, resolved)
	if herr != nil {
		if ctx.Err() != nil {
			return "", 0, nil, true, ctx.Err()
		}
		if nashelper.IsAnswer(herr) {
			return "", 0, nil, true, &promotionFileChangedError{path: path, phase: "hashing"}
		}
		return fallback("helper_unavailable", herr)
	}
	if helperSample.InstanceID != res.InstanceID || !sameHelperIdentity(helperSample.Identity, res.Identity) {
		// The file moved between the hash and the sample: the stable loop reads again.
		return "", 0, nil, true, &promotionFileChangedError{path: path, phase: "hashing"}
	}
	if res.Bytes != opened.Size() {
		// The helper's view held steady across the hash and the sample, so a size
		// that differs from the mount's descriptor is a different file under this
		// path, not one being written. Re-reading would only repeat it.
		return "", 0, nil, true, &promotionHelperDisagreeError{path: path, what: "sizes differ"}
	}
	if helperSample.Digest != mountSample {
		return "", 0, nil, true, &promotionHelperDisagreeError{path: path, what: "sampled ranges differ"}
	}
	if observer != nil {
		if oerr := observer(path, res.Bytes, res.Bytes); oerr != nil {
			return "", 0, nil, true, oerr
		}
	}
	promotionRecordRead(ctx, path, "nas_helper", "", res.Identity.Complete)
	if res.Identity.Complete {
		proof = &helperProof{Identity: res.Identity, Sample: helperSample.Digest}
	}
	return res.SHA256, res.Bytes, proof, true, nil
}

func isHelperBusy(err error) bool {
	var apiErr *nashelper.Error
	return errors.As(err, &apiErr) && apiErr.Code == nashelper.CodeBusy
}
