package transcodeworker

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// tripCtx is a context that ends, with the search budget's cause, on the
// first Err call after it has been told to allow n more. It lets a test put
// the budget's stop at every checkpoint the search passes after a stage,
// without naming them.
type tripCtx struct {
	context.Context
	cancel  context.CancelCauseFunc
	mu      sync.Mutex
	armed   bool
	allow   int
	tripped bool
}

func (c *tripCtx) arm(allow int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.allow = true, allow
}

func (c *tripCtx) Err() error {
	c.mu.Lock()
	if c.armed && !c.tripped {
		if c.allow > 0 {
			c.allow--
		} else {
			c.tripped = true
			c.cancel(errSearchBudgetStopped)
		}
	}
	c.mu.Unlock()
	return c.Context.Err()
}

// When the time limit ends a verify-until-exhausted search, wherever it falls
// (a stage, the merge of its evidence, a selection, a fallback), the search
// ends in a result, not an error, records the time cap, and keeps a setting it
// had already verified.
func TestVerifyUntilExhaustedKeepsAVerifiedWinnerWhereverTheStopFalls(t *testing.T) {
	settings := gridSettings(map[string]replaySetting{
		replayKey("medium", 30): failAt(replaySetting{bytes: 15500, discovery: replayScore{88, 90}}, 70, 2),
	})
	sawTrip, sawKept := false, false
	for allow := 0; allow <= 14; allow++ {
		t.Run("after "+strconv.Itoa(allow)+" checks", func(t *testing.T) {
			var trip *tripCtx
			stops := 0
			// runReplayConfigured fails the test on any error from the search.
			res := runReplayConfigured(t, replayConfig{settings: settings, sourceBytes: gridSourceBytes, rules: gridReplayRules(), ladder: replayLadder(true), duration: gridReplayDuration,
				ctx: func() (context.Context, context.CancelCauseFunc) {
					parent, cancel := context.WithCancelCause(context.Background())
					trip = &tripCtx{Context: parent, cancel: cancel}
					return trip, cancel
				},
				before: func(stage string, _ []validatedCandidate, _ context.CancelCauseFunc) {
					// The second verification attempt (crf22) is the first to pass.
					if stage == phaseVerifyingSamples {
						stops++
						if stops == 2 {
							trip.arm(allow)
						}
					}
				}})
			v := res.evidence.Automatic.Verification
			if trip.tripped {
				sawTrip = true
				if v.StoppedBy != transcode.VerificationStoppedTimeCap {
					t.Fatalf("the stop was not recorded: %+v", v)
				}
			} else if v.StoppedBy != "" {
				t.Fatalf("time cap recorded for a search that was not stopped: %+v", v)
			}
			// Each verified setting is smaller than the one before, so the last
			// one to pass is what the search must report.
			lastPassed := ""
			for _, a := range v.Attempts {
				if a.Outcome == transcode.VerificationPassed {
					lastPassed = a.CandidateID
				}
			}
			if lastPassed != "" {
				sawKept = true
				if res.winner != lastPassed {
					t.Fatalf("a verified setting was dropped by the stop: winner %q, last verified %q, %s", res.winner, lastPassed, res.summary())
				}
			}
		})
	}
	if !sawTrip || !sawKept {
		t.Fatalf("the sweep did not exercise both a stop (%v) and a kept winner (%v)", sawTrip, sawKept)
	}
}
