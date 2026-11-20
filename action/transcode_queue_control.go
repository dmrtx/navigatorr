package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/jakenesler/navigatorr/store"
)

// Queue control: reading the transcode queue in order, holding and releasing a
// waiting conversion, and moving a conversion or a whole batch to another
// place. Everything here only changes the hold and the key of the conversion
// (or batch) that was asked for, through the queue marks (see
// store/transcode_queue_marks.go); nobody else is renumbered, and a conversion
// that already has a worker is never touched.

// Kinds of a queue reference.
const (
	QueueKindAction = "action" // a transcode_media action waiting for a worker
	QueueKindItem   = "item"   // a queued batch item that has no child yet
	QueueKindBatch  = "batch"  // a transcode_batch: all its waiting entries
)

// QueueRef names one place in the queue: an action or a batch by its id, an
// item by its number (decimal).
type QueueRef struct {
	Kind string
	ID   string
}

// QueueErrorKind classifies a refusal.
type QueueErrorKind int

const (
	QueueNotFound QueueErrorKind = iota + 1 // no such action, item or batch
	QueueConflict                           // it exists but is not in a state this can be done in
	QueueInvalid                            // the request itself is wrong
)

// QueueError is a refusal of a queue change, worded for the caller.
type QueueError struct {
	Kind QueueErrorKind
	Msg  string
}

func (e *QueueError) Error() string { return e.Msg }

func queueErr(kind QueueErrorKind, format string, args ...any) error {
	return &QueueError{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// QueueEntry is one conversion waiting for a worker.
type QueueEntry struct {
	Kind     string // QueueKindAction or QueueKindItem
	ID       string
	BatchID  string
	Position int // 1-based among every waiting entry, held ones included
	// Held is true when the operator holds it or its whole batch is on hold;
	// HeldBy says which ("conversion" or "batch").
	Held             bool
	HeldBy           string
	Path             string
	MediaType        string
	WaitingCondition string
	WaitingReason    string

	key string // the order key, not shown
}

// QueueView is the waiting entries in queue order and the revision they were
// read at.
type QueueView struct {
	Revision int64
	Entries  []QueueEntry
}

// QueueBatch is the summary the queue gives of a batch.
type QueueBatch struct {
	ID            string
	Held          bool
	Waiting       int // its entries waiting in the queue
	FirstPosition int // the position of the first of them, 0 when none
}

// QueueResult is what a hold, a release or a move answers.
type QueueResult struct {
	Revision int64
	Entry    *QueueEntry // set for an action or an item
	Batch    *QueueBatch // set for a batch
}

// staleQueueErr is the refusal for a write the store found stale.
func staleQueueErr(err error) error {
	var stale *store.StaleQueueRevisionError
	if errors.As(err, &stale) {
		return queueErr(QueueConflict, "%s", stale.Error())
	}
	return err
}

// TranscodeQueueView lists the queue in order. It reads only. It waits for any
// operator change in progress (queueMu), so the revision it reports and the
// entries are of the same state.
func (e *Engine) TranscodeQueueView() (QueueView, error) {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	return e.queueViewLocked()
}

// queueViewLocked is TranscodeQueueView for a caller that holds queueMu. The
// revision is read first: a conversion made ready by the engine's own progress
// (which does not move it) can show up under an older revision, and an older
// revision only makes a later change refuse as stale.
func (e *Engine) queueViewLocked() (QueueView, error) {
	if e.deps.Store == nil {
		return QueueView{}, fmt.Errorf("the queue needs the store")
	}
	rev, err := e.deps.Store.QueueRevision()
	if err != nil {
		return QueueView{}, err
	}
	waiters, err := e.deps.Store.ListTranscodeQueueWaitersWithPaused()
	if err != nil {
		return QueueView{}, err
	}
	items, err := e.deps.Store.ListQueuedBatchItemsWithPaused()
	if err != nil {
		return QueueView{}, err
	}
	var entries []QueueEntry
	for _, w := range waiters {
		en := QueueEntry{Kind: QueueKindAction, ID: w.ID, BatchID: w.BatchID, Path: w.Path, MediaType: w.MediaType,
			WaitingCondition: w.WaitingCondition, WaitingReason: w.WaitingReason, key: w.QueueOrder}
		switch {
		case w.Held:
			en.Held, en.HeldBy = true, "conversion"
			en.WaitingCondition, en.WaitingReason = queueHeldCondition, queueHeldReason
		case w.BatchPaused:
			en.Held, en.HeldBy = true, "batch"
			en.WaitingCondition, en.WaitingReason = "parent_paused", "Its batch is on hold; it keeps its place and starts after the batch is released"
		}
		entries = append(entries, en)
	}
	for _, it := range items {
		en := QueueEntry{Kind: QueueKindItem, ID: strconv.FormatInt(it.ItemID, 10), BatchID: it.BatchID, Path: it.FilePath, MediaType: it.MediaType,
			WaitingCondition: "queued", WaitingReason: "Queued in its batch; a conversion is created for it when its turn comes", key: batchItemQueueOrder(it)}
		switch {
		case it.Held:
			en.Held, en.HeldBy = true, "conversion"
			en.WaitingCondition, en.WaitingReason = queueHeldCondition, queueHeldReason
		case it.BatchPaused:
			en.Held, en.HeldBy = true, "batch"
			en.WaitingCondition, en.WaitingReason = "parent_paused", "Its batch is on hold; it keeps its place and starts after the batch is released"
		}
		entries = append(entries, en)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.key != b.key {
			return a.key < b.key
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID < b.ID
	})
	for i := range entries {
		entries[i].Position = i + 1
	}
	return QueueView{Revision: rev, Entries: entries}, nil
}

func (v QueueView) find(kind, id string) *QueueEntry {
	for i := range v.Entries {
		if v.Entries[i].Kind == kind && v.Entries[i].ID == id {
			return &v.Entries[i]
		}
	}
	return nil
}

func (v QueueView) ofBatch(id string) []QueueEntry {
	var out []QueueEntry
	for _, en := range v.Entries {
		if en.BatchID == id {
			out = append(out, en)
		}
	}
	return out
}

// subject resolves a reference to the entries it stands for.
func (e *Engine) queueSubject(v QueueView, ref QueueRef) ([]QueueEntry, error) {
	switch ref.Kind {
	case QueueKindAction:
		if en := v.find(QueueKindAction, ref.ID); en != nil {
			return []QueueEntry{*en}, nil
		}
		inst, err := e.deps.Store.GetActionInstanceIfExists(ref.ID)
		if err != nil {
			return nil, err
		}
		if inst == nil || inst.ActionName != "transcode_media" {
			return nil, queueErr(QueueNotFound, "no such transcode conversion")
		}
		var state map[string]any
		_ = json.Unmarshal([]byte(inst.StateJSON), &state)
		if w := assignedWorker(state); w != "" {
			return nil, queueErr(QueueConflict, "this conversion already has a worker; use cancel to stop it or pause its node instead")
		}
		return nil, queueErr(QueueConflict, "this conversion is not waiting in the queue (its status is %s)", inst.Status)
	case QueueKindItem:
		n, err := strconv.ParseInt(ref.ID, 10, 64)
		if err != nil || n <= 0 {
			return nil, queueErr(QueueInvalid, "an item id is the number of a queued batch item")
		}
		if en := v.find(QueueKindItem, ref.ID); en != nil {
			return []QueueEntry{*en}, nil
		}
		item, err := e.deps.Store.GetTranscodeBatchItemByID(n)
		if err != nil {
			return nil, err
		}
		if item == nil {
			return nil, queueErr(QueueNotFound, "no such batch item")
		}
		if item.ChildActionID != "" {
			return nil, queueErr(QueueConflict, "this item already has a conversion (%s); hold or move that action instead", item.ChildActionID)
		}
		return nil, queueErr(QueueConflict, "this item is not waiting in the queue (its status is %s)", item.Status)
	case QueueKindBatch:
		inst, err := e.deps.Store.GetActionInstanceIfExists(ref.ID)
		if err != nil {
			return nil, err
		}
		if inst == nil || inst.ActionName != "transcode_batch" {
			return nil, queueErr(QueueNotFound, "no such transcode batch")
		}
		if ens := v.ofBatch(ref.ID); len(ens) > 0 {
			return ens, nil
		}
		return nil, queueErr(QueueConflict, "this batch has nothing waiting in the queue")
	}
	return nil, queueErr(QueueInvalid, "a reference is an action, an item or a batch")
}

// checkRevision refuses a change based on a queue that is no longer current.
func checkQueueRevision(v QueueView, want *int64) error {
	if want != nil && *want != v.Revision {
		return queueErr(QueueConflict, "the queue changed since it was read (queue_revision %d, now %d); read it again", *want, v.Revision)
	}
	return nil
}

// queueResult answers a change; the caller holds queueMu.
func (e *Engine) queueResult(ref QueueRef) (QueueResult, error) {
	v, err := e.queueViewLocked()
	if err != nil {
		return QueueResult{}, err
	}
	res := QueueResult{Revision: v.Revision}
	if ref.Kind == QueueKindBatch {
		b := QueueBatch{ID: ref.ID}
		for _, en := range v.ofBatch(ref.ID) {
			b.Waiting++
			if b.FirstPosition == 0 {
				b.FirstPosition = en.Position
			}
		}
		if inst, _ := e.deps.Store.GetActionInstanceIfExists(ref.ID); inst != nil {
			var state map[string]any
			_ = json.Unmarshal([]byte(inst.StateJSON), &state)
			b.Held = getBool(state, "paused")
		}
		res.Batch = &b
		return res, nil
	}
	if en := v.find(ref.Kind, ref.ID); en != nil {
		res.Entry = en
	}
	return res, nil
}

// HoldQueued keeps a waiting conversion in its place without letting it start;
// ReleaseQueued puts it back. A batch is held as a whole by pausing it.
func (e *Engine) HoldQueued(ctx context.Context, ref QueueRef, revision *int64) (QueueResult, error) {
	return e.setQueueHold(ctx, ref, revision, true)
}

func (e *Engine) ReleaseQueued(ctx context.Context, ref QueueRef, revision *int64) (QueueResult, error) {
	return e.setQueueHold(ctx, ref, revision, false)
}

func (e *Engine) setQueueHold(ctx context.Context, ref QueueRef, revision *int64, hold bool) (QueueResult, error) {
	if e.deps.Store == nil {
		return QueueResult{}, fmt.Errorf("the queue needs the store")
	}
	if ref.Kind == QueueKindBatch {
		// Pausing a batch runs its step, which assigns workers under bindMu.
		return e.setBatchHold(ctx, ref, revision, hold)
	}
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	e.bindMu.Lock()
	defer e.bindMu.Unlock()
	v, err := e.queueViewLocked()
	if err != nil {
		return QueueResult{}, err
	}
	if err := checkQueueRevision(v, revision); err != nil {
		return QueueResult{}, err
	}
	if e.afterQueueCheck != nil {
		e.afterQueueCheck()
	}
	subject, err := e.queueSubject(v, ref)
	if err != nil {
		return QueueResult{}, err
	}
	en := subject[0]
	if (en.Held && en.HeldBy == "conversion") == hold {
		return e.queueResult(ref)
	}
	var w store.QueueWrite
	if ref.Kind == QueueKindAction {
		w.ActionHeld = map[string]bool{ref.ID: hold}
	} else {
		n, _ := strconv.ParseInt(ref.ID, 10, 64)
		w.ItemHeld = map[int64]bool{n: hold}
	}
	w.ExpectRevision = revision
	if _, err := e.deps.Store.ApplyQueueWrite(w); err != nil {
		return QueueResult{}, staleQueueErr(err)
	}
	// A hold frees the slot its conversion was next in line for, a release
	// gives it back: either way the others look again now.
	e.WakeReconciler()
	return e.queueResult(ref)
}

// setBatchHold holds a batch by pausing it and releases it by resuming it.
func (e *Engine) setBatchHold(ctx context.Context, ref QueueRef, revision *int64, hold bool) (QueueResult, error) {
	// queueMu is held from the revision check to the revision bump, so no other
	// operator change, and no view, sees the revision of a batch that is half
	// held. Resume runs the batch's step, which takes bindMu (never queueMu).
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	v, err := e.queueViewLocked()
	if err != nil {
		return QueueResult{}, err
	}
	if err := checkQueueRevision(v, revision); err != nil {
		return QueueResult{}, err
	}
	inst, err := e.deps.Store.GetActionInstanceIfExists(ref.ID)
	if err != nil {
		return QueueResult{}, err
	}
	if inst == nil || inst.ActionName != "transcode_batch" {
		return QueueResult{}, queueErr(QueueNotFound, "no such transcode batch")
	}
	switch inst.Status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return QueueResult{}, queueErr(QueueConflict, "this batch has finished (its status is %s); there is nothing to hold or release", inst.Status)
	}
	var state map[string]any
	_ = json.Unmarshal([]byte(inst.StateJSON), &state)
	if getBool(state, "cancel_requested") {
		return QueueResult{}, queueErr(QueueConflict, "this batch is being cancelled; it cannot be held or released")
	}
	if getBool(state, "paused") != hold {
		if e.afterQueueCheck != nil {
			e.afterQueueCheck()
		}
		decision := "resume"
		if hold {
			decision = "pause"
		}
		if _, err := e.Resume(ctx, ref.ID, decision, nil); err != nil {
			return QueueResult{}, queueErr(QueueConflict, "the batch could not be %s now: %v", map[bool]string{true: "held", false: "released"}[hold], err)
		}
		if _, err := e.deps.Store.ApplyQueueWrite(store.QueueWrite{}); err != nil {
			return QueueResult{}, err
		}
		e.WakeReconciler()
	}
	return e.queueResult(ref)
}

// QueueMove says where to put a subject: first, before a reference or after
// one.
type QueueMove struct {
	First  bool
	Before *QueueRef
	After  *QueueRef
}

// MoveQueued gives the subject (a waiting conversion, a queued item or a whole
// batch) a new place in the queue. Only the subject gets a new key. A batch
// moves as one block: its waiting children are restamped and its queued items
// sort under the batch's new key; the ones already running are untouched.
func (e *Engine) MoveQueued(ctx context.Context, ref QueueRef, to QueueMove, revision *int64) (QueueResult, error) {
	if e.deps.Store == nil {
		return QueueResult{}, fmt.Errorf("the queue needs the store")
	}
	n := 0
	if to.First {
		n++
	}
	if to.Before != nil {
		n++
	}
	if to.After != nil {
		n++
	}
	if n != 1 {
		return QueueResult{}, queueErr(QueueInvalid, "say where to move it with exactly one of first, before or after")
	}
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	e.bindMu.Lock()
	defer e.bindMu.Unlock()
	v, err := e.queueViewLocked()
	if err != nil {
		return QueueResult{}, err
	}
	if err := checkQueueRevision(v, revision); err != nil {
		return QueueResult{}, err
	}
	if e.afterQueueCheck != nil {
		e.afterQueueCheck()
	}
	subject, err := e.queueSubject(v, ref)
	if err != nil {
		return QueueResult{}, err
	}
	inSubject := map[string]bool{}
	for _, en := range subject {
		inSubject[en.Kind+"/"+en.ID] = true
	}
	var rest []QueueEntry
	for _, en := range v.Entries {
		if !inSubject[en.Kind+"/"+en.ID] {
			rest = append(rest, en)
		}
	}
	// The bounds: the keys of the entries the subject goes between.
	var lo, hi string
	switch {
	case to.First:
		if len(rest) > 0 {
			hi = rest[0].key
		}
	default:
		target := to.Before
		if to.After != nil {
			target = to.After
		}
		if target.Kind == ref.Kind && target.ID == ref.ID {
			return QueueResult{}, queueErr(QueueInvalid, "it cannot be moved relative to itself")
		}
		tent, err := e.queueSubject(v, *target)
		if err != nil {
			if qe, ok := err.(*QueueError); ok && qe.Kind == QueueNotFound {
				return QueueResult{}, queueErr(QueueNotFound, "the place to move it to: %s", qe.Msg)
			}
			if qe, ok := err.(*QueueError); ok && qe.Kind == QueueConflict {
				return QueueResult{}, queueErr(QueueConflict, "it cannot be moved next to that one: %s", qe.Msg)
			}
			return QueueResult{}, err
		}
		for _, en := range tent {
			if inSubject[en.Kind+"/"+en.ID] {
				return QueueResult{}, queueErr(QueueInvalid, "it cannot be moved relative to something that is part of itself")
			}
		}
		first, last := -1, -1
		for i, en := range rest {
			for _, t := range tent {
				if en.Kind == t.Kind && en.ID == t.ID {
					if first < 0 {
						first = i
					}
					last = i
				}
			}
		}
		if first < 0 {
			return QueueResult{}, queueErr(QueueConflict, "it cannot be moved next to that one: it is not waiting in the queue")
		}
		if to.Before != nil {
			hi = rest[first].key
			if first > 0 {
				lo = rest[first-1].key
			}
		} else {
			lo = rest[last].key
			if last+1 < len(rest) {
				hi = rest[last+1].key
			}
		}
	}
	// Already there: every key of the subject lies between the bounds.
	inPlace := true
	for _, en := range subject {
		if en.key <= lo || (hi != "" && en.key >= hi) {
			inPlace = false
		}
	}
	if inPlace {
		return e.queueResult(ref)
	}
	if hi != "" && lo >= hi {
		return QueueResult{}, queueErr(QueueConflict, "the queue has two entries with the same place; it cannot be moved between them")
	}
	key, err := keyBetween(lo, hi)
	if err != nil {
		return QueueResult{}, queueErr(QueueConflict, "no place can be made there: %v", err)
	}
	var w store.QueueWrite
	switch ref.Kind {
	case QueueKindAction:
		w.ActionOrder = map[string]string{ref.ID: key}
	case QueueKindItem:
		id, _ := strconv.ParseInt(ref.ID, 10, 64)
		w.ItemOrder = map[int64]string{id: key}
	case QueueKindBatch:
		w.BatchOrder = map[string]string{ref.ID: key}
		w.ClearItemOrdersOf = []string{ref.ID}
		w.ActionOrder = map[string]string{}
		for _, en := range subject {
			if en.Kind != QueueKindAction {
				continue
			}
			// Every child needs its own place in the block, so each must be
			// tied to its item: guessing a place would give two children the
			// same key.
			inst, err := e.deps.Store.GetActionInstanceIfExists(en.ID)
			if err != nil {
				return QueueResult{}, err
			}
			if inst == nil {
				return QueueResult{}, queueErr(QueueConflict, "the batch cannot be moved: conversion %s is gone; read the queue again", en.ID)
			}
			var inputs map[string]any
			_ = json.Unmarshal([]byte(inst.InputsJSON), &inputs)
			item, err := e.batchChildItem(en.ID, inputs)
			if err != nil {
				return QueueResult{}, err
			}
			if item == nil {
				return QueueResult{}, queueErr(QueueConflict, "the batch cannot be moved yet: conversion %s is not tied to its batch item", en.ID)
			}
			w.ActionOrder[en.ID] = batchBlockKey(key, item.ID)
		}
	}
	w.ExpectRevision = revision
	if _, err := e.deps.Store.ApplyQueueWrite(w); err != nil {
		return QueueResult{}, staleQueueErr(err)
	}
	// A move can put the subject, or free the place of another, at the head.
	e.WakeReconciler()
	return e.queueResult(ref)
}
