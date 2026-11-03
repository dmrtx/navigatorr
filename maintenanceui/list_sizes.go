package maintenanceui

import (
	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// Sizes on list rows. Every file job row says how big its original is from
// the moment that is known, and how big its result is once there is one, so a
// list reads "14.2 GB" while a file waits or encodes and "14.2 GB → 6.1 GB"
// once it has a result. Every figure comes from keys the trimmed snapshot
// keeps; a list row never stats a file.
//
// Row fields (bytes; a field is absent while the figure is unknown):
//
//	transcode_media, promote_transcode_candidate:
//	  original_bytes   the original file's size
//	  result_bytes     the new file's size, once the job has one
//	transcode_batch:
//	  original_bytes_total  the sum of its files' original sizes
//	  result_bytes_total    the sum of the result sizes of the files that have one
//	  files_with_result     how many files result_bytes_total counts
//	  files_with_original   how many files original_bytes_total counts

// jobOriginalBytes is the size of the file a job works on: a replacement's
// own checkpoint, else the transcode preflight's record of the original, else
// the stat taken when the job was submitted. 0 while none is known.
func jobOriginalBytes(r operationRecord) int64 {
	if r.inst.ActionName == "promote_transcode_candidate" {
		return int64(operationNumber(operationMap(operationValue(r, "promotion"))["original_bytes"]))
	}
	if n := int64(operationNumber(operationMap(operationValue(r, "original"))["size_bytes"])); n > 0 {
		return n
	}
	return int64(operationNumber(operationValue(r, action.SubmittedSourceBytesKey)))
}

// jobResultBytes is the size of the job's new file. A transcode has one only
// once it completed with a candidate, and keeps showing it after the
// candidate replaced its original; a candidate deleted without replacing
// anything has none. 0 while there is none.
func jobResultBytes(r operationRecord, replaced bool) int64 {
	if r.inst.ActionName == "promote_transcode_candidate" {
		return int64(operationNumber(operationMap(operationValue(r, "promotion"))["candidate_bytes"]))
	}
	if r.inst.ActionName != "transcode_media" || r.inst.Status != store.ActionStatusCompleted || operationValue(r, "skip_transcode") == true || operationValue(r, "skipped") == true {
		return 0
	}
	if operationValue(r, "candidate_removed") == true && !replaced {
		return 0
	}
	if n := int64(operationNumber(operationMap(operationValue(r, "result"))["size_bytes"])); n > 0 {
		return n
	}
	return int64(operationNumber(operationValue(r, "candidate_size_bytes")))
}

// setJobSizes writes original_bytes and result_bytes on a file job's row.
// source is the transcode a replacement replaces with (nil when unknown): a
// replacement still planning has no checkpoint of its own yet.
func setJobSizes(job map[string]any, r operationRecord, source *operationRecord, replaced bool) {
	original, result := jobOriginalBytes(r), jobResultBytes(r, replaced)
	if source != nil {
		if original <= 0 {
			original = jobOriginalBytes(*source)
		}
		if result <= 0 {
			result = jobResultBytes(*source, true)
		}
	}
	if original > 0 {
		job["original_bytes"] = original
	}
	if result > 0 {
		job["result_bytes"] = result
	}
}

// batchFileSizes is a batch file's original and result sizes: its current
// attempt's, else, for the original, the size the batch planned it at. A file
// waiting to be encoded again has no result yet.
func batchFileSizes(item store.TranscodeBatchItem, child *operationRecord, replaced, retryPending bool) (original, result int64) {
	if child != nil {
		original = jobOriginalBytes(*child)
		if !retryPending {
			result = jobResultBytes(*child, replaced)
		}
	}
	if original <= 0 {
		original = item.SourceBytes
	}
	return original, result
}

// setBatchSizeTotals writes a batch row's totals over its files. A file's
// current attempt is its item's child, when that child belongs to this batch
// and this file.
func setBatchSizeTotals(job map[string]any, batch operationRecord, items []store.TranscodeBatchItem, recordsByID map[string]operationRecord, replaced func(operationRecord) bool) {
	pending := operationMap(operationValue(batch, "batch_retry_pending"))
	var originalTotal, resultTotal int64
	withOriginal, withResult := 0, 0
	for _, item := range items {
		var child *operationRecord
		if c, ok := recordsByID[item.ChildActionID]; ok && c.inst.ActionName == "transcode_media" && operationString(c.inputs["parent_action_id"]) == batch.inst.ID && operationString(c.inputs["path"]) == item.FilePath {
			child = &c
		}
		isReplaced := child != nil && replaced(*child)
		original, result := batchFileSizes(item, child, isReplaced, pending[item.ItemKey] == true)
		if original > 0 {
			originalTotal += original
			withOriginal++
		}
		if result > 0 {
			resultTotal += result
			withResult++
		}
	}
	if withOriginal > 0 {
		job["original_bytes_total"] = originalTotal
	}
	job["files_with_original"] = withOriginal
	if withResult > 0 {
		job["result_bytes_total"] = resultTotal
	}
	job["files_with_result"] = withResult
}
