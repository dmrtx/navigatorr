package resilience

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Standard error categories
const (
	CategoryDatabaseLocked      = "upstream_database_locked"
	CategoryUpstreamUnavailable = "upstream_unavailable"
	CategoryUpstreamRateLimited = "upstream_rate_limited"
	CategoryUpstreamError       = "upstream_error"
	CategoryDestructiveDisabled = "destructive_disabled"
	CategorySafetyViolation     = "safety_violation"
	CategoryNotFound            = "not_found"
	CategoryTimeout             = "timeout"
	CategoryInvalidInput        = "invalid_input"
)

// StructuredError represents a structured, machine-readable error for LLM workflows.
type StructuredError struct {
	Category   string `json:"category"`
	Service    string `json:"service,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	Retryable  bool   `json:"retryable"`
	Message    string `json:"message"`
	Detail     string `json:"detail,omitempty"`
}

func (e *StructuredError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("[%s] %s (%s)", e.Category, e.Message, e.Detail)
	}
	return fmt.Sprintf("[%s] %s", e.Category, e.Message)
}

func (e *StructuredError) JSON() string {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf(`{"category":%q,"message":%q,"retryable":%v}`, e.Category, e.Message, e.Retryable)
	}
	return string(b)
}

// IsDatabaseLocked checks if an HTTP response indicates SQLite database lock.
func IsDatabaseLocked(statusCode int, body []byte) bool {
	if statusCode != 500 && statusCode != 503 {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "database is locked") ||
		(strings.Contains(s, "sqlite") && strings.Contains(s, "busy")) ||
		strings.Contains(s, "sqlite_busy") ||
		strings.Contains(s, "sqlite error 5")
}

// ClassifyError converts a status code and response body into a StructuredError.
func ClassifyError(service string, statusCode int, body []byte) *StructuredError {
	if IsDatabaseLocked(statusCode, body) {
		return &StructuredError{
			Category:   CategoryDatabaseLocked,
			Service:    service,
			StatusCode: statusCode,
			Retryable:  true,
			Message:    fmt.Sprintf("%s database is currently locked by another operation", service),
			Detail:     string(body),
		}
	}
	if statusCode == 429 {
		return &StructuredError{
			Category:   CategoryUpstreamRateLimited,
			Service:    service,
			StatusCode: statusCode,
			Retryable:  true,
			Message:    fmt.Sprintf("%s rate limit exceeded", service),
			Detail:     string(body),
		}
	}
	if statusCode == 502 || statusCode == 503 || statusCode == 504 {
		return &StructuredError{
			Category:   CategoryUpstreamUnavailable,
			Service:    service,
			StatusCode: statusCode,
			Retryable:  true,
			Message:    fmt.Sprintf("%s is temporarily unavailable (HTTP %d)", service, statusCode),
			Detail:     string(body),
		}
	}
	if statusCode == 404 {
		return &StructuredError{
			Category:   CategoryNotFound,
			Service:    service,
			StatusCode: statusCode,
			Retryable:  false,
			Message:    fmt.Sprintf("resource not found in %s", service),
			Detail:     string(body),
		}
	}
	return &StructuredError{
		Category:   CategoryUpstreamError,
		Service:    service,
		StatusCode: statusCode,
		Retryable:  false,
		Message:    fmt.Sprintf("%s returned HTTP %d", service, statusCode),
		Detail:     string(body),
	}
}

// DestructiveDisabledError creates a structured error when destructive operations are disabled.
func DestructiveDisabledError(action string) *StructuredError {
	return &StructuredError{
		Category:  CategoryDestructiveDisabled,
		Retryable: false,
		Message:   "Destructive operations are disabled by policy",
		Detail:    fmt.Sprintf("Action %q requires allow_destructive: true in config.yaml", action),
	}
}

// ServicePool manages concurrency and rate-limiting across services.
type ServicePool struct {
	mu         sync.Mutex
	maxPerSvc  int
	semaphores map[string]chan struct{}
	mediaSem   chan struct{}
}

// NewServicePool initializes bounded concurrency limits.
func NewServicePool(maxPerSvc, maxMedia int) *ServicePool {
	if maxPerSvc <= 0 {
		maxPerSvc = 3
	}
	if maxMedia <= 0 {
		maxMedia = 2
	}
	return &ServicePool{
		maxPerSvc:  maxPerSvc,
		semaphores: make(map[string]chan struct{}),
		mediaSem:   make(chan struct{}, maxMedia),
	}
}

func (p *ServicePool) getServiceSem(svc string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	sem, ok := p.semaphores[svc]
	if !ok {
		sem = make(chan struct{}, p.maxPerSvc)
		p.semaphores[svc] = sem
	}
	return sem
}

// AcquireService acquires a concurrency token for the specified service.
func (p *ServicePool) AcquireService(ctx context.Context, svc string) error {
	sem := p.getServiceSem(svc)
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseService releases a concurrency token for the service.
func (p *ServicePool) ReleaseService(svc string) {
	sem := p.getServiceSem(svc)
	select {
	case <-sem:
	default:
	}
}

// AcquireMedia acquires a concurrency token for media inspection (ffprobe).
func (p *ServicePool) AcquireMedia(ctx context.Context) error {
	select {
	case p.mediaSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseMedia releases a concurrency token for media inspection.
func (p *ServicePool) ReleaseMedia() {
	select {
	case <-p.mediaSem:
	default:
	}
}

// RetryConfig holds exponential backoff parameters.
type RetryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

// DefaultRetryConfig returns standard retry settings.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		BaseDelay:  150 * time.Millisecond,
		MaxDelay:   2 * time.Second,
	}
}

// ExecuteWithRetry executes an HTTP request operation with bounded concurrency and retry.
func (p *ServicePool) ExecuteWithRetry(ctx context.Context, svc string, cfg RetryConfig, fn func() ([]byte, int, error)) ([]byte, int, error) {
	if cfg.MaxRetries <= 0 {
		cfg = DefaultRetryConfig()
	}

	var lastBody []byte
	var lastStatus int
	var lastErr error

	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			// Calculate backoff with full jitter
			delay := cfg.BaseDelay * (1 << (attempt - 1))
			if delay > cfg.MaxDelay {
				delay = cfg.MaxDelay
			}
			jitter := time.Duration(rand.Int63n(int64(delay / 2)))
			sleepTime := delay/2 + jitter

			select {
			case <-time.After(sleepTime):
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
		}

		// Acquire concurrency slot
		if err := p.AcquireService(ctx, svc); err != nil {
			return nil, 0, err
		}

		body, statusCode, err := fn()
		p.ReleaseService(svc)

		if err != nil {
			lastErr = err
			lastBody = body
			lastStatus = statusCode
			continue
		}

		// Check if error is transient and retryable
		if IsDatabaseLocked(statusCode, body) || statusCode == 429 || statusCode == 502 || statusCode == 503 || statusCode == 504 {
			lastBody = body
			lastStatus = statusCode
			lastErr = ClassifyError(svc, statusCode, body)
			continue
		}

		// Success or non-retryable status
		return body, statusCode, nil
	}

	if lastErr != nil {
		return lastBody, lastStatus, lastErr
	}
	return lastBody, lastStatus, ClassifyError(svc, lastStatus, lastBody)
}
