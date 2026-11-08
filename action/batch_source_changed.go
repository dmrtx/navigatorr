package action

import (
	"fmt"

	"github.com/jakenesler/navigatorr/store"
)

// A batch plans each file at one moment and starts its encode later, sometimes
// days later while the files ahead of it wait for a slot. In between, the file
// can be replaced: another request converted it, or a better release was
// imported. The plan was made for the old bytes, so starting its encode now is
// a full encode for a result nobody asked for. An item whose file is no longer
// the size it was planned at is skipped without starting its encode; a new
// batch plans the file as it is now.

// sourceChangeTolerance is the fraction of the planned size (one tenth) the
// file's size may drift by before it counts as another file: a re-tag or a
// rewritten header moves it by far less, a conversion or a replaced release by
// far more.
const sourceChangeTolerance = 10

// sourceChangedSincePlanned reports whether a file of current bytes is no
// longer the one planned at planned bytes. Either size unknown (0) is not a
// change: the item then starts as it always did.
func sourceChangedSincePlanned(planned, current int64) bool {
	if planned <= 0 || current <= 0 {
		return false
	}
	diff := current - planned
	if diff < 0 {
		diff = -diff
	}
	// diff > planned/10 in whole bytes is exactly diff*10 > planned, without
	// the multiplication that could overflow.
	return diff > planned/sourceChangeTolerance
}

// batchItemSourceChanged reports whether the item's file changed since the
// batch planned it, with the reason to show when it did. Only an item that has
// no encode yet is judged: a child already started has judged its file in its
// own preflight. An item the user asked to encode again (new settings) is
// judged like any other: the batch only takes new settings for the originals
// it planned and has not replaced, so the planned size is still the one to
// compare with, and a file another request converted in between is not one to
// encode again.
func (e *Engine) batchItemSourceChanged(item store.TranscodeBatchItem) (string, bool) {
	if item.ChildActionID != "" || item.SourceBytes <= 0 {
		return "", false
	}
	current := e.boundedFileSize(item.FilePath)
	if !sourceChangedSincePlanned(item.SourceBytes, current) {
		return "", false
	}
	return fmt.Sprintf("Not converted: the file is no longer the one this batch planned (%s then, %s now)", formatBudgetBytes(item.SourceBytes), formatBudgetBytes(current)), true
}
