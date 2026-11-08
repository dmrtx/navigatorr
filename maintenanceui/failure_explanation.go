package maintenanceui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// failedFileJob is a file job that failed, not one whose candidate was
// rejected or one that missed the old minimum saving: those explain
// themselves.
func failedFileJob(r operationRecord) bool {
	return r.inst.ActionName == "transcode_media" && r.inst.Status == store.ActionStatusFailed &&
		!strings.Contains(r.inst.ErrorJSON, "transcode candidate rejected by user decision") && !savingsFailurePattern.MatchString(r.inst.ErrorJSON)
}

// recheckOffered is whether the worker's report of a damaged file is still
// open to Retry's read. A file of a batch is not: the batch runs it again by
// skipping a file blamed this way, and a batch file has no Retry of its own on
// the page.
func recheckOffered(r operationRecord) bool {
	return action.CanRecheckSource(r.inst.ErrorJSON, r.state) && operationString(r.inputs["parent_action_id"]) == ""
}

// failureText is the job's stored error message.
func failureText(r operationRecord) string {
	text := strings.TrimSpace(r.inst.ErrorJSON)
	var stored struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(text), &stored) == nil && stored.Error != "" {
		text = stored.Error
	}
	return text
}

var failureClassPattern = regexp.MustCompile(`failed \(([a-z_]+)\)`)

// failureClass is the class the job's final error names, else the one it
// recorded, else the one its message reads as. The final error comes first: a
// class recorded earlier (a probe, a poll) can outlive a rerun that failed
// elsewhere.
func failureClass(r operationRecord, text string) string {
	if m := failureClassPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	if class := operationString(operationValue(r, "failure_classification")); class != "" {
		return class
	}
	return string(resilience.Classify(text))
}

// Failures of the worker's or the NAS's connection, not of the file: running
// it again can pass.
var reachFailures = map[string]bool{
	string(resilience.WorkerUnreachable): true, string(resilience.SSHTransient): true,
	string(resilience.StorageIOTransient): true, string(resilience.StorageIOError): true, string(resilience.SMBTransportError): true,
	string(resilience.SMBSessionInvalid): true,
}

// Failures that need something outside the job fixed first; running it again
// before that fails the same way (and Retry refuses while the worker still
// holds the job).
var fixFirstFailures = map[string]string{
	"storage_permission_denied":      "Navigatorr was not allowed to read or write the file",
	"smb_auth_failed":                "The NAS refused Navigatorr's credentials",
	"storage_full":                   "The disk ran out of space",
	"encoder_capability_unsupported": "The worker's encoder cannot produce what this goal asks for",
}

// Failures of Navigatorr's own measuring of the new file, not of the picture.
var measureFailures = map[string]bool{
	string(resilience.BenchmarkEvidenceIncomplete): true, string(resilience.InternalInvariant): true,
}

// explainFailure says why a file job failed and whether running it again can
// change anything. blocked is why Retry cannot help, "" when it can; steps
// replace Retry when it cannot. The text says "running it again", not
// "Retry": a file of a batch runs again with "Encode again".
func explainFailure(r operationRecord) (e resultExplanation, blocked string, steps []nextStep) {
	text := failureText(r)
	lower := strings.ToLower(text)
	class := failureClass(r, text)
	chooseGoal := nextStep{ID: "tracks", Label: "Choose a goal", Detail: "Start a goal for the file as it is now."}
	switch {
	// The worker's scan blamed the file and Navigatorr has not found it damaged
	// itself: Retry reads the file here first.
	case recheckOffered(r):
		e.Happened = "The worker reported the file as damaged: its own scan of the video found errors in the container. Navigatorr has not confirmed it."
		e.Learned = []string{"The worker reads the file through its own copy or network path, which can fail where a read here does not. Retry reads the file here first: if it reads whole and clean, the job runs again; if it is damaged here too, nothing runs and Retry says what the read found."}
		return e, "", nil
	case action.SourceDamaged(text):
		why := "ffmpeg found broken data while reading it"
		if strings.Contains(lower, "moov atom not found") {
			why = "its MP4 index (the moov atom) is missing, which usually means the download or copy never finished"
		}
		e.Happened = fmt.Sprintf("The file itself is damaged: %s.", why)
		e.Learned = []string{"Every run of this file fails the same way. Replace it, for example by downloading it again, then choose a goal for the new file."}
		return e, "This file is damaged; running it again fails the same way.", []nextStep{{ID: "tracks", Label: "Choose a goal", Detail: "Once the file is replaced, start a goal for the new one."}}
	case permanentRejection(text):
		e.Happened = "This file can't be encoded with this goal: its picture, colour range or tracks are not ones this goal supports."
		e.Learned = []string{"Running it again fails the same way. Choosing a goal for the file as it is now checks it against the current rules, and a goal that cannot take it says so before anything is encoded."}
		return e, "This file can't be encoded with this goal; running it again fails the same way.", []nextStep{chooseGoal}
	case class == string(resilience.WorkerSourceReadFault):
		e.Happened = "The worker reported the file as damaged, but Navigatorr read the same file end to end and found nothing wrong: the worker's own read of it failed."
		e.Learned = []string{"The file is fine. Running it again is safe, and can pass if the worker reads it correctly this time; if it keeps failing on one worker, that worker's copy or network path needs a look."}
		return e, "", nil
	case strings.Contains(text, "choosing the tracks to keep"):
		_, detail, _ := strings.Cut(text, "choosing the tracks to keep: ")
		e.Happened = shortTrackMismatch(detail)
		return e, "Running it again picks the same tracks.", []nextStep{{ID: "tracks", Label: "Choose tracks", Detail: "Pick the audio and subtitle tracks this file has, then choose a goal."}}
	case class == string(resilience.Cancelled):
		e.Happened = "The job was cancelled on the worker."
		e.Learned = []string{"Running it again starts it over."}
	case class == string(resilience.SourceChanged):
		e.Happened = "The file changed while the job ran, for example because Sonarr or Radarr replaced it."
		e.Learned = []string{"Running it again checks the file as it is now. If Retry refuses, choose a goal for the new file instead."}
		return e, "", []nextStep{chooseGoal}
	case fixFirstFailures[class] != "":
		e.Happened = fixFirstFailures[class] + "."
		e.Learned = []string{"Running it again fails the same way until that is fixed."}
	case reachFailures[class]:
		e.Happened = "The job stopped because the worker or the NAS could not be reached; nothing about the file caused it."
		e.Learned = []string{"Running it again can pass once both are reachable."}
	case class == string(resilience.RunnerKilled):
		e.Happened = "The encoder was stopped before it finished, by a worker restart or because it ran out of memory."
		e.Learned = []string{"Running it again can pass. If it stops again on this file, the file may need more memory than the worker has."}
	case class == string(resilience.EncoderTemporarilyUnavailable):
		e.Happened = "The worker's encoder was busy or unavailable."
		e.Learned = []string{"Running it again can pass."}
	case class == string(resilience.SourceUnreachable):
		e.Happened = "Navigatorr could not find or open the file at its path."
		e.Learned = []string{"If the NAS was unreachable, running it again can pass. If the file moved or was deleted, choose a goal for it where it is now."}
	case measureFailures[class] || strings.Contains(text, "quality_final_sample_misaligned"):
		e.Happened = "The new file could not be checked: Navigatorr's own quality measurement failed, not the picture."
		e.Learned = []string{"Running it again repeats the same check, so it usually fails the same way until Navigatorr is fixed."}
	case class == string(resilience.CandidateInvalid):
		e.Happened = "The new file failed Navigatorr's checks (for example a missing stream or an empty encode); the original is untouched."
		e.Learned = []string{"Running it again encodes a new file."}
	case class == "worker_setup_failed":
		e.Happened = "The worker could not prepare the job: copying, reading or setting up the file failed."
		e.Learned = []string{"This is often passing; running it again can work."}
	case strings.HasPrefix(text, "failed to probe original media"):
		e.Happened = "The original could not be read."
		if strings.Contains(lower, "permission denied") {
			e.Happened = "The original could not be read: Navigatorr was not allowed to open it."
			e.Learned = []string{"Running it again fails the same way until the file's permissions are fixed."}
			break
		}
		e.Learned = []string{"This can pass, for a file still being copied. If running it again fails the same way, the file is probably damaged."}
	case class != "" && class != string(resilience.Unclassified):
		e.Happened = fmt.Sprintf("The job failed (%s).", strings.ReplaceAll(class, "_", " "))
	default:
		e.Happened = "The job failed; the error below says where."
	}
	return e, "", nil
}

func permanentRejection(text string) bool {
	_, ok := action.PermanentSourceRejection(text)
	return ok
}
