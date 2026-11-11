package transcodeworker

import (
	"errors"
	"fmt"
)

// ErrContentNotPreserved is the error of a candidate refused for not keeping
// what the source has (a track, its packets or frames, a flag, a title, an
// attachment). Its text says so; it still counts as ErrSourceInvalid, the
// sentinel these checks used before, so every caller that turns that one into
// preservation_not_met and no retry keeps doing so.
var ErrContentNotPreserved error = contentNotPreserved{}

type contentNotPreserved struct{}

func (contentNotPreserved) Error() string { return "the converted file lost content" }

func (contentNotPreserved) Is(target error) bool { return target == ErrSourceInvalid }

// preservationCheckError names the candidate check that refused the encode
// (video completeness, audio packets, subtitle events, attachments, a track's
// flags or title...), in front of the check's own numbers. The job's error
// carries it to the coordinator, which tells the owner what was not kept
// instead of a bare "not preserved". It wraps the check's error unchanged, so
// errors.Is(err, ErrSourceInvalid) and the failure class still hold.
type preservationCheckError struct {
	check string
	err   error
}

func (e *preservationCheckError) Error() string {
	return fmt.Sprintf("%s check failed: %s", e.check, e.err.Error())
}

func (e *preservationCheckError) Unwrap() error { return e.err }

// failedCheck labels err with the check that produced it. An error already
// labelled by a narrower check (a track's disposition inside the audio track
// checks) keeps its label.
func failedCheck(check string, err error) error {
	if err == nil {
		return nil
	}
	var labelled *preservationCheckError
	if errors.As(err, &labelled) {
		return err
	}
	return &preservationCheckError{check: check, err: err}
}

// checkInvalid is candidateInvalid labelled with its check.
func checkInvalid(check string, err error) error {
	return failedCheck(check, candidateInvalid(err))
}
