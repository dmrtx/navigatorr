package transcodeworker

import (
	"errors"
	"fmt"

	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// classedError attaches a failure class to an error without changing its text
// or what it wraps, so errors.Is(err, ErrSourceInvalid) and the like keep
// working. resilience.ClassifyError reads the class before it reads any text,
// so the class survives wrapping on its way to failJobTerminal and from there
// to the coordinator, whatever the message says about paths.
type classedError struct {
	class resilience.FailureClass
	err   error
}

func (e *classedError) Error() string        { return e.err.Error() }
func (e *classedError) Unwrap() error        { return e.err }
func (e *classedError) FailureClass() string { return string(e.class) }

// sourceChanged marks err as the source differing from the one the plan, the
// expected digest or the evidence was built on. The coordinator then refuses
// an unchanged manual retry, which would copy and hash the whole file again to
// fail the same way.
func sourceChanged(err error) error {
	if err == nil {
		return nil
	}
	return &classedError{class: resilience.SourceChanged, err: err}
}

// candidateInvalid marks err as the worker-local candidate failing its
// integrity or preservation checks. It is not a statement about the source and
// not an ffmpeg failure; it is never retried automatically, and a manual retry
// re-encodes.
func candidateInvalid(err error) error {
	if err == nil {
		return nil
	}
	return &classedError{class: resilience.CandidateInvalid, err: err}
}

// localFileChangedError is a local file whose identity, size or modification
// time moved while it was being hashed. Which file that was is the caller's
// to say: a staged source is a source change, a candidate is an invalid
// candidate, so this error carries no class itself.
type localFileChangedError struct{ path string }

func (e *localFileChangedError) Error() string {
	return fmt.Sprintf("%s: local file changed while hashing %s", ErrSourceInvalid, e.path)
}

func (e *localFileChangedError) Unwrap() error { return ErrSourceInvalid }

// sourceDigestFailure classes a failed digest check of the staged source.
func sourceDigestFailure(err error) error {
	var changed *localFileChangedError
	if errors.As(err, &changed) {
		return sourceChanged(err)
	}
	return err
}

// candidateHashFailure classes a failed hash of the local candidate.
func candidateHashFailure(err error) error {
	var changed *localFileChangedError
	if errors.As(err, &changed) {
		return candidateInvalid(err)
	}
	return err
}
