package resilience

import (
	"context"
	"testing"
	"time"
)

func TestIsDatabaseLocked(t *testing.T) {
	cases := []struct {
		code int
		body string
		want bool
	}{
		{500, `{"message":"database is locked"}`, true},
		{500, `SqliteFailure: SQLite Error 5: 'database is locked'`, true},
		{500, `sqlite_busy: database is locked`, true},
		{503, `Service Temporarily Busy: sqlite busy`, true},
		{200, `{"status":"ok"}`, false},
		{404, `{"message":"not found"}`, false},
		{500, `{"message":"internal server error"}`, false},
	}

	for _, tc := range cases {
		got := IsDatabaseLocked(tc.code, []byte(tc.body))
		if got != tc.want {
			t.Errorf("IsDatabaseLocked(%d, %q) = %v, want %v", tc.code, tc.body, got, tc.want)
		}
	}
}

func TestClassifyError(t *testing.T) {
	err1 := ClassifyError("radarr", 500, []byte("database is locked"))
	if err1.Category != CategoryDatabaseLocked || !err1.Retryable {
		t.Errorf("expected CategoryDatabaseLocked, got %+v", err1)
	}

	err2 := ClassifyError("sonarr", 429, []byte("too many requests"))
	if err2.Category != CategoryUpstreamRateLimited || !err2.Retryable {
		t.Errorf("expected CategoryUpstreamRateLimited, got %+v", err2)
	}

	err3 := ClassifyError("radarr", 404, []byte("missing movie"))
	if err3.Category != CategoryNotFound || err3.Retryable {
		t.Errorf("expected CategoryNotFound, got %+v", err3)
	}
}

func TestServicePoolConcurrency(t *testing.T) {
	pool := NewServicePool(2, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := pool.AcquireService(ctx, "radarr"); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	if err := pool.AcquireService(ctx, "radarr"); err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}

	// Third acquire should block until release or context timeout
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()
	if err := pool.AcquireService(shortCtx, "radarr"); err == nil {
		t.Error("expected third acquire to timeout")
	}

	pool.ReleaseService("radarr")
	if err := pool.AcquireService(ctx, "radarr"); err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	pool.ReleaseService("radarr")
	pool.ReleaseService("radarr")
}

func TestExecuteWithRetry(t *testing.T) {
	pool := NewServicePool(2, 1)
	ctx := context.Background()
	cfg := RetryConfig{
		MaxRetries: 2,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   50 * time.Millisecond,
	}

	attempts := 0
	body, code, err := pool.ExecuteWithRetry(ctx, "radarr", cfg, func() ([]byte, int, error) {
		attempts++
		if attempts < 2 {
			return []byte("database is locked"), 500, nil
		}
		return []byte(`{"status":"ok"}`), 200, nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Errorf("expected status 200, got %d", code)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
	if string(body) != `{"status":"ok"}` {
		t.Errorf("got body %s", string(body))
	}
}
