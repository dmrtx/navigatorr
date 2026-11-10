package action

import "strings"

// preservationFailureKey holds, for a candidate the worker refused for losing
// part of the original (preservation_not_met), which check refused it and its
// numbers, in the owner's words.
const preservationFailureKey = "preservation_failure"

// workerPreservationNoise is wording the worker's preservation errors carry
// for its logs that says nothing the explanation does not already say: the
// sentinel's text (a worker before this one used the source sentinel, whose
// text speaks of a regular file) and the fail-closed markers.
var workerPreservationNoise = []string{
	"the converted file lost content: ",
	"transcode source file is not a regular file: ",
	" (fail closed, never publish)",
	", fail closed, never publish",
	" (fail closed)",
	"; never publish",
}

// preservationFailureDetail turns the worker's error for a refused candidate
// ("audio packets check failed: ... audio stream 1 has 310 packets, the
// source 313 ...") into the detail the explanation shows: the check's name and
// its numbers, without the worker's log wording. It is "" when the worker
// gave no error.
func preservationFailureDetail(workerError string) string {
	msg := workerError
	for _, noise := range workerPreservationNoise {
		msg = strings.ReplaceAll(msg, noise, "")
	}
	msg = strings.ReplaceAll(msg, "local candidate ", "converted ")
	return strings.TrimSpace(msg)
}
