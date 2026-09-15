package main

// Phase 5B startup-ordering tests: the serve lifecycle must reconcile
// persisted state synchronously before any queue scheduling, and a
// reconciliation failure must fail startup closed without scheduling. The
// ordering is exercised through the injected seam so no real worker, process,
// or network is required.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubReconciler struct {
	calls  int
	err    error
	gotCtx context.Context
	order  *[]string
}

func (s *stubReconciler) ReconcileStartup(ctx context.Context) error {
	s.calls++
	s.gotCtx = ctx
	if s.order != nil {
		*s.order = append(*s.order, "reconcile")
	}
	return s.err
}

func TestServeStartupReconcileRunsBeforeScheduling(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "serve")

	var order []string
	rec := &stubReconciler{order: &order}
	scheduled := 0

	err := startServeAfterReconcile(ctx, rec, func() {
		order = append(order, "schedule")
		scheduled++
	})
	if err != nil {
		t.Fatalf("successful reconciliation must not error: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("reconcile must run exactly once, got %d", rec.calls)
	}
	if scheduled != 1 {
		t.Fatalf("scheduler must start after reconciliation, got %d starts", scheduled)
	}
	if rec.gotCtx != ctx {
		t.Fatalf("reconcile must receive the serve context")
	}
	if len(order) != 2 || order[0] != "reconcile" || order[1] != "schedule" {
		t.Fatalf("reconcile must precede scheduling, got order %v", order)
	}
}

func TestServeStartupReconcileFailurePreventsScheduling(t *testing.T) {
	rec := &stubReconciler{err: errors.New("reconcile boom")}
	scheduled := false

	err := startServeAfterReconcile(context.Background(), rec, func() {
		scheduled = true
	})
	if err == nil {
		t.Fatal("reconciliation failure must fail startup closed")
	}
	if !strings.Contains(err.Error(), "reconcile boom") {
		t.Fatalf("error must be contextual and wrap the cause, got %v", err)
	}
	if scheduled {
		t.Fatal("scheduler must never start when reconciliation fails")
	}
}

func TestServeStartupNilReconcilerFailsClosed(t *testing.T) {
	scheduled := false
	err := startServeAfterReconcile(context.Background(), nil, func() {
		scheduled = true
	})
	if err == nil {
		t.Fatal("nil reconciler must fail closed")
	}
	if scheduled {
		t.Fatal("scheduler must not start without a reconciler")
	}
}
