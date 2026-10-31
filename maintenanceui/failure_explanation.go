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
// rejected: a rejection is the owner's decision and explains itself.
func failedFileJob(r operationRecord) bool {
	return r.inst.ActionName == "transcode_media" && r.inst.Status == store.ActionStatusFailed &&
		!strings.Contains(r.inst.ErrorJSON, "transcode candidate rejected by user decision")
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

// failureClass is the class the job recorded, else the one its message names.
func failureClass(r operationRecord, text string) string {
	if class := operationString(operationValue(r, "failure_classification")); class != "" {
		return class
	}
	if m := failureClassPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return string(resilience.Classify(text))
}

// Failures of the worker or the storage, not of the file: Retry can pass.
var reachFailures = map[string]bool{
	string(resilience.WorkerUnreachable): true, string(resilience.RunnerKilled): true, string(resilience.SSHTransient): true,
	string(resilience.StorageIOTransient): true, string(resilience.StorageIOError): true, string(resilience.SMBTransportError): true,
	string(resilience.SMBSessionInvalid): true, string(resilience.SourceUnreachable): true, string(resilience.EncoderTemporarilyUnavailable): true,
}

// Failures that Retry refuses until something outside the job is fixed (see
// rejectUnchangedPermanentRetry); a new goal starts a new job once it is.
var fixFirstFailures = map[string]string{
	"storage_permission_denied":      "Navigatorr was not allowed to read or write the file",
	"permission_denied":              "Navigatorr was not allowed to read or write the file",
	"smb_auth_failed":                "The NAS refused Navigatorr's credentials",
	"storage_full":                   "The disk ran out of space",
	"codec_unsupported":              "The worker cannot decode or encode one of this file's streams",
	"encoder_capability_unsupported": "The worker's encoder cannot produce what this goal asks for",
}

// Failures of Navigatorr's own measuring of the new file, not of the picture.
var checkFailures = map[string]bool{
	string(resilience.CandidateInvalid): true, string(resilience.BenchmarkEvidenceIncomplete): true,
	string(resilience.InternalInvariant): true, "worker_setup_failed": true,
}

// explainFailure says why a file job failed and whether running it again can
// change anything. blocked is why Retry cannot help, "" when it can; steps
// replace Retry when it cannot.
func explainFailure(r operationRecord) (e resultExplanation, blocked string, steps []nextStep) {
	text := failureText(r)
	class := failureClass(r, text)
	chooseGoal := nextStep{ID: "tracks", Label: "Choose a goal", Detail: "Start a goal for the file as it is now."}
	switch {
	case action.SourceDamaged(text):
		why := "ffmpeg found broken data while reading it"
		if strings.Contains(strings.ToLower(text), "moov atom not found") {
			why = "its MP4 index (the moov atom) is missing, which usually means the download or copy never finished"
		}
		e.Happened = fmt.Sprintf("The file itself is damaged: %s.", why)
		e.Learned = []string{"Every run of this file fails the same way. Replace it, for example by downloading it again, then choose a goal for the new file."}
		return e, "This file is damaged; running it again fails the same way.", nil
	case strings.Contains(text, "choosing the tracks to keep"):
		_, detail, _ := strings.Cut(text, "choosing the tracks to keep: ")
		e.Happened = "The tracks chosen for this job do not match the file: " + strings.TrimSuffix(detail, ".") + "."
		return e, "Running it again picks the same tracks.", []nextStep{{ID: "tracks", Label: "Choose tracks", Detail: "Pick the audio and subtitle tracks this file has, then choose a goal."}}
	case class == string(resilience.SourceChanged):
		e.Happened = "The file changed while the job ran, for example because Sonarr or Radarr replaced it."
		return e, "The original changed, so this job's checks no longer match it.", []nextStep{chooseGoal}
	case fixFirstFailures[class] != "":
		e.Happened = fixFirstFailures[class] + "."
		e.Learned = []string{"Fix that first, then choose a goal to start a new job; Retry refuses until then."}
		return e, fixFirstFailures[class] + ".", []nextStep{chooseGoal}
	case reachFailures[class]:
		e.Happened = "The job stopped because the worker or the NAS could not be reached; nothing about the file caused it."
		e.Learned = []string{"Retry runs it again once both are reachable."}
	case checkFailures[class] || strings.Contains(text, "quality_final_sample_misaligned"):
		e.Happened = "The new file could not be checked: Navigatorr's own quality measurement failed, not the picture."
		e.Learned = []string{"Retry repeats the same check, so it fails the same way until Navigatorr is fixed."}
	case strings.HasPrefix(text, "failed to probe original media"):
		e.Happened = "The original could not be read."
		e.Learned = []string{"This can pass, for a file still being copied. If Retry fails the same way, the file is probably damaged."}
	case class != "" && class != string(resilience.Unclassified):
		e.Happened = fmt.Sprintf("The job failed (%s).", strings.ReplaceAll(class, "_", " "))
	default:
		e.Happened = "The job failed; the error below says where."
	}
	return e, "", nil
}
