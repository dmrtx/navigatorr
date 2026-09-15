package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var leaseTestStates = []LeaseState{
	LeaseHealthy,
	LeaseDegraded,
	LeaseRepairPending,
	LeaseRepairing,
	LeaseFailed,
}

func TestLeaseKeyDeterministicAndPathSafe(t *testing.T) {
	resources := []string{
		"/mnt/nas/media/../etc/passwd",
		"../../../../etc/shadow",
		"relative/path",
		"resource with spaces",
		"unicode-资源-Ω",
		strings.Repeat("a", 4096),
	}
	stateDir := t.TempDir()
	cleanStateDir := filepath.Clean(stateDir)
	seen := map[string]string{}
	for _, res := range resources {
		k1, err := LeaseKey(res)
		if err != nil {
			t.Fatalf("LeaseKey(%q) error: %v", res, err)
		}
		k2, err := LeaseKey(res)
		if err != nil {
			t.Fatalf("LeaseKey(%q) second call error: %v", res, err)
		}
		if k1 != k2 {
			t.Fatalf("LeaseKey(%q) not deterministic: %q != %q", res, k1, k2)
		}
		if len(k1) != leaseKeyHexLen || !isSafeLeaseKey(k1) {
			t.Fatalf("LeaseKey(%q)=%q is not a safe hex key", res, k1)
		}
		if strings.ContainsAny(k1, `/\`) {
			t.Fatalf("LeaseKey(%q)=%q contains a path separator", res, k1)
		}
		if prev, ok := seen[k1]; ok && prev != res {
			t.Fatalf("key collision between %q and %q", prev, res)
		}
		seen[k1] = res

		path, err := LeasePath(stateDir, res)
		if err != nil {
			t.Fatalf("LeasePath(%q) error: %v", res, err)
		}
		if filepath.Dir(path) != filepath.Join(cleanStateDir, LeaseDirName) {
			t.Fatalf("LeasePath(%q)=%q not under _leases", res, path)
		}
		if filepath.Base(path) != k1+".json" {
			t.Fatalf("LeasePath(%q)=%q filename not derived from safe key", res, path)
		}
		if !strings.HasPrefix(path, cleanStateDir+string(os.PathSeparator)) {
			t.Fatalf("LeasePath(%q)=%q escapes state dir %q", res, path, cleanStateDir)
		}
	}

	for _, blank := range []string{"", "   ", "\t\n", " \u00a0 "} {
		if _, err := LeaseKey(blank); err == nil {
			t.Fatalf("LeaseKey(%q) must fail for blank resource", blank)
		}
		if _, err := LeasePath(stateDir, blank); err == nil {
			t.Fatalf("LeasePath(%q) must fail for blank resource", blank)
		}
	}
	if _, err := LeasePath("", "/mnt/nas/x"); err == nil {
		t.Fatal("LeasePath must fail for blank state dir")
	}
}

func TestLeaseSaveLoadAllStates(t *testing.T) {
	stateDir := t.TempDir()
	resource := "/mnt/nas/media/show/season01"
	key, err := LeaseKey(resource)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range leaseTestStates {
		updated := time.Now().UTC().Truncate(time.Second)
		rec := &LeaseRecord{
			Resource:  resource,
			Key:       key,
			State:     state,
			UpdatedAt: updated,
			LastError: "synthetic " + string(state),
		}
		if err := SaveLeaseAtomic(stateDir, rec); err != nil {
			t.Fatalf("SaveLeaseAtomic(%s): %v", state, err)
		}
		got, err := LoadLease(stateDir, resource)
		if err != nil {
			t.Fatalf("LoadLease(%s): %v", state, err)
		}
		if got.Resource != resource || got.Key != key || got.State != state {
			t.Fatalf("round trip mismatch: got %+v", got)
		}
		if !got.UpdatedAt.Equal(updated) {
			t.Fatalf("updated_at mismatch: got %v want %v", got.UpdatedAt, updated)
		}
		if got.LastError != rec.LastError {
			t.Fatalf("last_error mismatch: got %q want %q", got.LastError, rec.LastError)
		}
	}
}

func TestLeaseLoadFailsClosed(t *testing.T) {
	stateDir := t.TempDir()
	resource := "/mnt/nas/media/id-a"
	key, err := LeaseKey(resource)
	if err != nil {
		t.Fatal(err)
	}
	path, err := LeasePath(stateDir, resource)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	writeRaw := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		content string
	}{
		{"malformed json", "{not-json"},
		{"json null", "null"},
		{"blank resource", fmt.Sprintf(`{"resource":"","key":%q,"state":"healthy"}`, key)},
		{"blank key", fmt.Sprintf(`{"resource":%q,"key":"","state":"healthy"}`, resource)},
		{"missing state", fmt.Sprintf(`{"resource":%q,"key":%q}`, resource, key)},
		{"unknown state", fmt.Sprintf(`{"resource":%q,"key":%q,"state":"weird"}`, resource, key)},
		{"key mismatch", fmt.Sprintf(`{"resource":%q,"key":%q,"state":"healthy"}`, resource, strings.Repeat("0", 64))},
		{"path mismatch", fmt.Sprintf(`{"resource":"/mnt/nas/media/id-b","key":%q,"state":"healthy"}`, key)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeRaw(tc.content)
			if _, err := LoadLease(stateDir, resource); err == nil {
				t.Fatalf("LoadLease must fail closed for %s", tc.name)
			}
		})
	}

	if _, err := LoadLease(stateDir, "/mnt/nas/media/missing"); err == nil {
		t.Fatal("LoadLease must fail for a missing record")
	}
}

func TestLeaseSaveFailsClosed(t *testing.T) {
	stateDir := t.TempDir()
	resource := "/mnt/nas/media/id"
	key, _ := LeaseKey(resource)
	valid := &LeaseRecord{Resource: resource, Key: key, State: LeaseHealthy, UpdatedAt: time.Now()}

	if err := SaveLeaseAtomic(stateDir, nil); err == nil {
		t.Fatal("nil record must fail")
	}
	if err := SaveLeaseAtomic(stateDir, &LeaseRecord{Key: key, State: LeaseHealthy}); err == nil {
		t.Fatal("blank resource must fail")
	}
	if err := SaveLeaseAtomic(stateDir, &LeaseRecord{Resource: resource, Key: strings.Repeat("0", 64), State: LeaseHealthy}); err == nil {
		t.Fatal("nondeterministic key must fail")
	}
	if err := SaveLeaseAtomic(stateDir, &LeaseRecord{Resource: resource, Key: key, State: "bogus"}); err == nil {
		t.Fatal("unknown state must fail")
	}
	if err := SaveLeaseAtomic("", valid); err == nil {
		t.Fatal("blank state dir must fail")
	}
}

func TestLeaseManagerHealthySkipsRepair(t *testing.T) {
	stateDir := t.TempDir()
	var repairCalls int32
	m, err := NewLeaseManager(stateDir,
		func(ctx context.Context, resource string) error { return nil },
		func(ctx context.Context, resource string) error {
			atomic.AddInt32(&repairCalls, 1)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := m.EnsureHealthy(context.Background(), "/mnt/nas/media/ok")
	if err != nil {
		t.Fatalf("EnsureHealthy: %v", err)
	}
	if rec == nil || rec.State != LeaseHealthy {
		t.Fatalf("expected healthy, got %+v", rec)
	}
	if got := atomic.LoadInt32(&repairCalls); got != 0 {
		t.Fatalf("repair must not run for a healthy resource, got %d calls", got)
	}
	loaded, err := LoadLease(stateDir, "/mnt/nas/media/ok")
	if err != nil || loaded.State != LeaseHealthy {
		t.Fatalf("persisted state: %+v err=%v", loaded, err)
	}
}

func TestLeaseManagerUnhealthyRepairSuccess(t *testing.T) {
	stateDir := t.TempDir()
	resource := "/mnt/nas/media/flaky"

	var mu sync.Mutex
	healthCalls := 0
	health := func(ctx context.Context, r string) error {
		mu.Lock()
		healthCalls++
		n := healthCalls
		mu.Unlock()
		if n == 1 {
			return errors.New("share unavailable")
		}
		return nil
	}

	var repairCalls int32
	var sawRepairing bool
	repair := func(ctx context.Context, r string) error {
		atomic.AddInt32(&repairCalls, 1)
		rec, err := LoadLease(stateDir, r)
		if err != nil {
			return fmt.Errorf("repair cannot read persisted state: %w", err)
		}
		if rec.State != LeaseRepairing {
			return fmt.Errorf("repair observed state %q, want repairing", rec.State)
		}
		sawRepairing = true
		return nil
	}

	m, err := NewLeaseManager(stateDir, health, repair)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := m.EnsureHealthy(context.Background(), resource)
	if err != nil {
		t.Fatalf("EnsureHealthy: %v", err)
	}
	if rec == nil || rec.State != LeaseHealthy {
		t.Fatalf("expected healthy, got %+v", rec)
	}
	if got := atomic.LoadInt32(&repairCalls); got != 1 {
		t.Fatalf("repair calls=%d want 1", got)
	}
	if !sawRepairing {
		t.Fatal("repair must observe the persisted repairing state")
	}
	if healthCalls != 2 {
		t.Fatalf("health calls=%d want 2 (pre + post repair)", healthCalls)
	}
	loaded, err := LoadLease(stateDir, resource)
	if err != nil || loaded.State != LeaseHealthy || loaded.LastError != "" {
		t.Fatalf("final persisted state: %+v err=%v", loaded, err)
	}
}

func TestLeaseManagerRepairErrorFailed(t *testing.T) {
	stateDir := t.TempDir()
	resource := "/mnt/nas/media/broken"
	repairErr := errors.New("repair exploded")

	m, err := NewLeaseManager(stateDir,
		func(ctx context.Context, r string) error { return errors.New("unhealthy") },
		func(ctx context.Context, r string) error { return repairErr })
	if err != nil {
		t.Fatal(err)
	}
	rec, err := m.EnsureHealthy(context.Background(), resource)
	if err == nil {
		t.Fatal("EnsureHealthy must fail when repair fails")
	}
	if !errors.Is(err, repairErr) {
		t.Fatalf("error must wrap repair error, got %v", err)
	}
	if rec == nil || rec.State != LeaseFailed {
		t.Fatalf("expected failed record, got %+v", rec)
	}
	loaded, lerr := LoadLease(stateDir, resource)
	if lerr != nil || loaded.State != LeaseFailed {
		t.Fatalf("persisted state: %+v err=%v", loaded, lerr)
	}
	if !strings.Contains(loaded.LastError, repairErr.Error()) {
		t.Fatalf("last_error %q must contain repair error", loaded.LastError)
	}
}

func TestLeaseManagerPostRepairCheckErrorFailed(t *testing.T) {
	stateDir := t.TempDir()
	resource := "/mnt/nas/media/still-bad"

	var mu sync.Mutex
	healthCalls := 0
	health := func(ctx context.Context, r string) error {
		mu.Lock()
		defer mu.Unlock()
		healthCalls++
		return fmt.Errorf("health failure %d", healthCalls)
	}
	var repairCalls int32
	m, err := NewLeaseManager(stateDir, health, func(ctx context.Context, r string) error {
		atomic.AddInt32(&repairCalls, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := m.EnsureHealthy(context.Background(), resource)
	if err == nil {
		t.Fatal("EnsureHealthy must fail when post-repair health check fails")
	}
	if rec == nil || rec.State != LeaseFailed {
		t.Fatalf("expected failed record, got %+v", rec)
	}
	if got := atomic.LoadInt32(&repairCalls); got != 1 {
		t.Fatalf("repair calls=%d want 1", got)
	}
	loaded, lerr := LoadLease(stateDir, resource)
	if lerr != nil || loaded.State != LeaseFailed {
		t.Fatalf("persisted state: %+v err=%v", loaded, lerr)
	}
}

func TestLeaseManagerConcurrentSameResourceRepairsOnce(t *testing.T) {
	stateDir := t.TempDir()
	resource := "/mnt/nas/media/shared"

	var mu sync.Mutex
	repaired := false
	health := func(ctx context.Context, r string) error {
		mu.Lock()
		defer mu.Unlock()
		if repaired {
			return nil
		}
		return errors.New("unhealthy")
	}

	repairStarted := make(chan struct{})
	release := make(chan struct{})
	var repairCalls int32
	repair := func(ctx context.Context, r string) error {
		if atomic.AddInt32(&repairCalls, 1) == 1 {
			close(repairStarted)
		}
		<-release
		mu.Lock()
		repaired = true
		mu.Unlock()
		return nil
	}

	m, err := NewLeaseManager(stateDir, health, repair)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	recs := make([]*LeaseRecord, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i], errs[i] = m.EnsureHealthy(context.Background(), resource)
		}(i)
	}
	close(start)

	select {
	case <-repairStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("repair never started")
	}
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&repairCalls); got != 1 {
		t.Fatalf("repair calls while leader in flight=%d want 1", got)
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&repairCalls); got != 1 {
		t.Fatalf("total repair calls=%d want 1", got)
	}
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d error: %v", i, errs[i])
		}
		if recs[i] == nil || recs[i].State != LeaseHealthy {
			t.Fatalf("caller %d state=%+v want healthy", i, recs[i])
		}
	}
	loaded, err := LoadLease(stateDir, resource)
	if err != nil || loaded.State != LeaseHealthy {
		t.Fatalf("final persisted state: %+v err=%v", loaded, err)
	}
}

func TestLeaseManagerDifferentResourcesRepairConcurrently(t *testing.T) {
	stateDir := t.TempDir()
	resources := []string{"/mnt/nas/media/a", "/mnt/nas/media/b"}

	var mu sync.Mutex
	repaired := map[string]bool{}
	health := func(ctx context.Context, r string) error {
		mu.Lock()
		defer mu.Unlock()
		if repaired[r] {
			return nil
		}
		return errors.New("unhealthy " + r)
	}

	var entered int
	both := make(chan struct{})
	var once sync.Once
	repair := func(ctx context.Context, r string) error {
		mu.Lock()
		entered++
		if entered == 2 {
			once.Do(func() { close(both) })
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(5 * time.Second):
			return errors.New("repairs did not run concurrently (shared bottleneck)")
		}
		mu.Lock()
		repaired[r] = true
		mu.Unlock()
		return nil
	}

	m, err := NewLeaseManager(stateDir, health, repair)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(resources))
	for i, res := range resources {
		wg.Add(1)
		go func(i int, res string) {
			defer wg.Done()
			_, errs[i] = m.EnsureHealthy(context.Background(), res)
		}(i, res)
	}
	wg.Wait()

	for i, res := range resources {
		if errs[i] != nil {
			t.Fatalf("resource %s error: %v", res, errs[i])
		}
		loaded, lerr := LoadLease(stateDir, res)
		if lerr != nil || loaded.State != LeaseHealthy {
			t.Fatalf("resource %s persisted state: %+v err=%v", res, loaded, lerr)
		}
	}
}

func TestLeaseManagerCancellationNeverHealthy(t *testing.T) {
	t.Run("cancel during unhealthy check", func(t *testing.T) {
		stateDir := t.TempDir()
		resource := "/mnt/nas/media/cancel1"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var repairCalls int32
		health := func(ctx context.Context, r string) error {
			cancel()
			return errors.New("unhealthy")
		}
		m, err := NewLeaseManager(stateDir, health, func(ctx context.Context, r string) error {
			atomic.AddInt32(&repairCalls, 1)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		rec, err := m.EnsureHealthy(ctx, resource)
		if err == nil {
			t.Fatal("cancelled EnsureHealthy must not succeed")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error must be context.Canceled, got %v", err)
		}
		if got := atomic.LoadInt32(&repairCalls); got != 0 {
			t.Fatalf("repair must not start after cancellation, got %d", got)
		}
		if rec != nil && rec.State == LeaseHealthy {
			t.Fatalf("cancelled flow must not report healthy: %+v", rec)
		}
		loaded, lerr := LoadLease(stateDir, resource)
		if lerr != nil {
			t.Fatalf("cancellation must persist a safe non-healthy state: %v", lerr)
		}
		if loaded.State == LeaseHealthy {
			t.Fatalf("persisted state must not be healthy, got %+v", loaded)
		}
		if loaded.State != LeaseRepairPending {
			t.Fatalf("persisted state=%q want repair_pending", loaded.State)
		}
	})

	t.Run("pre-cancelled context with healthy probe", func(t *testing.T) {
		stateDir := t.TempDir()
		resource := "/mnt/nas/media/cancel2"
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var repairCalls int32
		m, err := NewLeaseManager(stateDir,
			func(ctx context.Context, r string) error { return nil },
			func(ctx context.Context, r string) error {
				atomic.AddInt32(&repairCalls, 1)
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}

		rec, err := m.EnsureHealthy(ctx, resource)
		if err == nil {
			t.Fatal("cancelled EnsureHealthy must not succeed")
		}
		if got := atomic.LoadInt32(&repairCalls); got != 0 {
			t.Fatalf("repair must not start for a cancelled call, got %d", got)
		}
		if rec != nil && rec.State == LeaseHealthy {
			t.Fatalf("cancelled flow must not report healthy: %+v", rec)
		}
		loaded, lerr := LoadLease(stateDir, resource)
		if lerr != nil {
			t.Fatalf("cancellation must persist a safe non-healthy state: %v", lerr)
		}
		if loaded.State == LeaseHealthy {
			t.Fatalf("persisted state must not be healthy, got %+v", loaded)
		}
	})
}

func TestLeaseManagerPersistenceErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(stateDir, 0700) }()

	m, err := NewLeaseManager(stateDir,
		func(ctx context.Context, r string) error { return nil },
		func(ctx context.Context, r string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	rec, err := m.EnsureHealthy(context.Background(), "/mnt/nas/media/read-only")
	if err == nil {
		t.Fatal("persistence failure must fail closed")
	}
	if rec != nil && rec.State == LeaseHealthy {
		t.Fatalf("must not report healthy on persistence failure: %+v", rec)
	}
}

func TestNewLeaseManagerValidation(t *testing.T) {
	if _, err := NewLeaseManager("", func(context.Context, string) error { return nil }, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("blank state dir must fail")
	}
	if _, err := NewLeaseManager(t.TempDir(), nil, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("nil health check must fail")
	}
	if _, err := NewLeaseManager(t.TempDir(), func(context.Context, string) error { return nil }, nil); err == nil {
		t.Fatal("nil repair must fail")
	}
}

func TestBoundLeaseError(t *testing.T) {
	short := "small failure"
	if got := boundLeaseError(short); got != short {
		t.Fatalf("short error changed: %q", got)
	}
	long := strings.Repeat("x", MaxLeaseLastErrorBytes*2)
	got := boundLeaseError(long)
	if len(got) != MaxLeaseLastErrorBytes {
		t.Fatalf("bounded length=%d want %d", len(got), MaxLeaseLastErrorBytes)
	}
	if !strings.HasSuffix(got, leaseErrorTruncatedMark) {
		t.Fatalf("bounded error missing truncation marker: %q", got)
	}
}
