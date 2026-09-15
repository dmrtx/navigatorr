package main

// Phase 6B2 startup-order tests: after binding, the serve lifecycle must
// reconcile persisted state, then synchronously resume post-encode
// finalization, and only then start the queued scheduler. Any earlier failure
// must fail startup closed without starting the scheduler. Exercised through
// the injected seam so no real worker, process, or network is required.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubResumer struct {
	calls      int
	err        error
	gotCtx     context.Context
	gotSelfExe string
	gotConfig  string
	order      *[]string
}

func (s *stubResumer) ResumePostEncode(ctx context.Context, selfExe, configPath string) (int, error) {
	s.calls++
	s.gotCtx = ctx
	s.gotSelfExe = selfExe
	s.gotConfig = configPath
	if s.order != nil {
		*s.order = append(*s.order, "resume")
	}
	return 0, s.err
}

func TestServeStartupReconcileResumeSchedulingOrder(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "serve")

	var order []string
	rec := &stubReconciler{order: &order}
	res := &stubResumer{order: &order}
	scheduled := 0

	err := startServeAfterReconcileAndResume(ctx, rec, res, "/self/exe", "/cfg.yaml", func() {
		order = append(order, "schedule")
		scheduled++
	})
	if err != nil {
		t.Fatalf("successful startup must not error: %v", err)
	}
	if rec.calls != 1 || res.calls != 1 || scheduled != 1 {
		t.Fatalf("calls reconcile=%d resume=%d schedule=%d, want all 1", rec.calls, res.calls, scheduled)
	}
	if rec.gotCtx != ctx || res.gotCtx != ctx {
		t.Fatalf("reconcile/resume must receive the serve context")
	}
	if res.gotSelfExe != "/self/exe" || res.gotConfig != "/cfg.yaml" {
		t.Fatalf("resume must receive self exe/config, got %q/%q", res.gotSelfExe, res.gotConfig)
	}
	want := []string{"reconcile", "resume", "schedule"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestServeStartupResumeFailurePreventsScheduling(t *testing.T) {
	rec := &stubReconciler{}
	res := &stubResumer{err: errors.New("resume boom")}
	scheduled := false

	err := startServeAfterReconcileAndResume(context.Background(), rec, res, "self", "cfg", func() {
		scheduled = true
	})
	if err == nil {
		t.Fatal("resume failure must fail startup closed")
	}
	if !strings.Contains(err.Error(), "resume boom") {
		t.Fatalf("error must be contextual and wrap the cause, got %v", err)
	}
	if rec.calls != 1 || res.calls != 1 {
		t.Fatalf("reconcile/resume must still run once each, got %d/%d", rec.calls, res.calls)
	}
	if scheduled {
		t.Fatal("scheduler must never start when post-encode resume fails")
	}
}

func TestServeStartupReconcileFailurePreventsResumeAndScheduling(t *testing.T) {
	rec := &stubReconciler{err: errors.New("reconcile boom")}
	res := &stubResumer{}
	scheduled := false

	err := startServeAfterReconcileAndResume(context.Background(), rec, res, "self", "cfg", func() {
		scheduled = true
	})
	if err == nil || !strings.Contains(err.Error(), "reconcile boom") {
		t.Fatalf("reconcile failure must fail startup with cause, got %v", err)
	}
	if res.calls != 0 {
		t.Fatalf("resume must not run when reconciliation fails, got %d calls", res.calls)
	}
	if scheduled {
		t.Fatal("scheduler must never start when reconciliation fails")
	}
}

func TestServeStartupNilResumerFailsClosed(t *testing.T) {
	rec := &stubReconciler{}
	scheduled := false

	err := startServeAfterReconcileAndResume(context.Background(), rec, nil, "self", "cfg", func() {
		scheduled = true
	})
	if err == nil {
		t.Fatal("nil resumer must fail closed")
	}
	if rec.calls != 1 {
		t.Fatalf("reconcile must run before the nil-resumer check, got %d", rec.calls)
	}
	if scheduled {
		t.Fatal("scheduler must not start without a resumer")
	}
}
