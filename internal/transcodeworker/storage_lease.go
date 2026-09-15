package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// LeaseState is the persisted health state of an external (NAS/SMB) storage
// resource. Only these five states are valid; anything else fails closed.
type LeaseState string

const (
	LeaseHealthy       LeaseState = "healthy"
	LeaseDegraded      LeaseState = "degraded"
	LeaseRepairPending LeaseState = "repair_pending"
	LeaseRepairing     LeaseState = "repairing"
	LeaseFailed        LeaseState = "failed"
)

// LeaseDirName is the fixed subdirectory of StateDir holding lease records.
const LeaseDirName = "_leases"

// MaxLeaseLastErrorBytes bounds the persisted last_error field.
const MaxLeaseLastErrorBytes = 2048

const (
	leaseKeyHexLen          = 64
	leaseErrorTruncatedMark = "...(truncated)"
)

func (s LeaseState) valid() bool {
	switch s {
	case LeaseHealthy, LeaseDegraded, LeaseRepairPending, LeaseRepairing, LeaseFailed:
		return true
	default:
		return false
	}
}

// LeaseHealthCheckFunc probes a resource and returns nil when it is healthy.
// Callers inject it so no real mount/SMB command is ever issued.
type LeaseHealthCheckFunc func(ctx context.Context, resource string) error

// LeaseRepairFunc attempts an out-of-band repair of a resource and returns nil
// on success. Callers inject it so no real mount/SMB command is ever issued.
type LeaseRepairFunc func(ctx context.Context, resource string) error

// LeaseRecord is the durable lease state for a single resource. The on-disk
// filename is never the raw resource; it is the deterministic safe key.
type LeaseRecord struct {
	Resource  string     `json:"resource"`
	Key       string     `json:"key"`
	State     LeaseState `json:"state"`
	UpdatedAt time.Time  `json:"updated_at"`
	LastError string     `json:"last_error,omitempty"`
}

// LeaseKey returns the deterministic, filesystem-safe key for a resource. It is
// a hex SHA-256 digest, so a resource path can never influence the filename and
// two distinct resources cannot collide on the raw path text.
func LeaseKey(resource string) (string, error) {
	if strings.TrimSpace(resource) == "" {
		return "", errors.New("lease resource is required")
	}
	sum := sha256.Sum256([]byte(resource))
	return hex.EncodeToString(sum[:]), nil
}

func isSafeLeaseKey(key string) bool {
	if len(key) != leaseKeyHexLen {
		return false
	}
	for _, r := range key {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// LeasePath returns the deterministic record path
// <stateDir>/_leases/<safe-key>.json for a resource.
func LeasePath(stateDir, resource string) (string, error) {
	key, err := LeaseKey(resource)
	if err != nil {
		return "", err
	}
	return leasePathForKey(stateDir, key)
}

func leasePathForKey(stateDir, key string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", errors.New("lease state dir is required")
	}
	if !isSafeLeaseKey(key) {
		return "", fmt.Errorf("invalid lease key %q", key)
	}
	clean := filepath.Clean(stateDir)
	path := filepath.Join(clean, LeaseDirName, key+".json")
	rel, err := filepath.Rel(clean, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("lease path for key %q escapes state dir", key)
	}
	return path, nil
}

// SaveLeaseAtomic validates and atomically persists a lease record using a
// temp file + fsync + close + rename. It fails closed on a nil record, blank
// resource, nondeterministic key, or unknown state.
func SaveLeaseAtomic(stateDir string, rec *LeaseRecord) error {
	if rec == nil {
		return errors.New("saving lease: nil record")
	}
	key, err := LeaseKey(rec.Resource)
	if err != nil {
		return fmt.Errorf("saving lease: %w", err)
	}
	if rec.Key != key {
		return fmt.Errorf("saving lease %s: key mismatch (got %q, want %q)", rec.Resource, rec.Key, key)
	}
	if !rec.State.valid() {
		return fmt.Errorf("saving lease %s: unknown state %q", rec.Resource, rec.State)
	}
	path, err := leasePathForKey(stateDir, key)
	if err != nil {
		return fmt.Errorf("saving lease %s: %w", rec.Resource, err)
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling lease %s: %w", rec.Resource, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating lease directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "lease-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp lease file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp lease file %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing temp lease file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing temp lease file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp lease file %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// LoadLease loads and validates the lease record for a resource. It fails
// closed on a missing/unreadable file, malformed JSON, blank resource or key,
// key mismatch (the record key is not the digest of the resource), resource
// mismatch (record loaded for a different resource), or unknown state.
func LoadLease(stateDir, resource string) (*LeaseRecord, error) {
	key, err := LeaseKey(resource)
	if err != nil {
		return nil, err
	}
	path, err := leasePathForKey(stateDir, key)
	if err != nil {
		return nil, err
	}
	return loadLeaseFile(path, resource, key)
}

func loadLeaseFile(path, resource, key string) (*LeaseRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading lease %s: %w", path, err)
	}
	var rec LeaseRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parsing lease %s: %w", path, err)
	}
	if err := validateLeaseRecord(&rec, resource, key); err != nil {
		return nil, fmt.Errorf("validating lease %s: %w", path, err)
	}
	return &rec, nil
}

func validateLeaseRecord(rec *LeaseRecord, resource, key string) error {
	if rec == nil {
		return errors.New("nil record")
	}
	if strings.TrimSpace(rec.Resource) == "" {
		return errors.New("blank resource")
	}
	if strings.TrimSpace(rec.Key) == "" {
		return errors.New("blank key")
	}
	if rec.Key != key {
		return fmt.Errorf("key mismatch: record key %q does not match expected %q", rec.Key, key)
	}
	if resource != "" && rec.Resource != resource {
		return fmt.Errorf("path mismatch: record resource %q does not match expected %q", rec.Resource, resource)
	}
	if !rec.State.valid() {
		return fmt.Errorf("unknown state %q", rec.State)
	}
	return nil
}

func boundLeaseError(msg string) string {
	if len(msg) <= MaxLeaseLastErrorBytes {
		return msg
	}
	limit := MaxLeaseLastErrorBytes - len(leaseErrorTruncatedMark)
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.RuneStart(msg[limit]) {
		limit--
	}
	return msg[:limit] + leaseErrorTruncatedMark
}

type leaseFlight struct {
	done chan struct{}
	rec  *LeaseRecord
	err  error
}

// LeaseManager coordinates per-resource health checks and repairs. Concurrent
// EnsureHealthy calls for the same resource collapse onto a single in-flight
// flow; different resources run fully independently.
type LeaseManager struct {
	stateDir    string
	healthCheck LeaseHealthCheckFunc
	repair      LeaseRepairFunc
	now         func() time.Time

	mu      sync.Mutex
	flights map[string]*leaseFlight
}

// NewLeaseManager builds a manager with injected health-check and repair hooks.
func NewLeaseManager(stateDir string, healthCheck LeaseHealthCheckFunc, repair LeaseRepairFunc) (*LeaseManager, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("lease manager: state dir is required")
	}
	if healthCheck == nil {
		return nil, errors.New("lease manager: health check is required")
	}
	if repair == nil {
		return nil, errors.New("lease manager: repair is required")
	}
	return &LeaseManager{
		stateDir:    filepath.Clean(stateDir),
		healthCheck: healthCheck,
		repair:      repair,
		now:         time.Now,
		flights:     make(map[string]*leaseFlight),
	}, nil
}

// EnsureHealthy drives a resource to healthy via a deterministic transition
// sequence and persists each transition. It never reports healthy when the
// caller's context is cancelled, and no retry engine is involved.
func (m *LeaseManager) EnsureHealthy(ctx context.Context, resource string) (*LeaseRecord, error) {
	if m == nil {
		return nil, errors.New("lease manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key, err := LeaseKey(resource)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if f, ok := m.flights[key]; ok {
		m.mu.Unlock()
		select {
		case <-f.done:
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return f.rec, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &leaseFlight{done: make(chan struct{})}
	m.flights[key] = f
	m.mu.Unlock()

	rec, flowErr := m.runFlow(ctx, resource, key)
	f.rec = rec
	f.err = flowErr

	m.mu.Lock()
	delete(m.flights, key)
	m.mu.Unlock()
	close(f.done)

	return rec, flowErr
}

func (m *LeaseManager) loadOrInit(resource, key string) (*LeaseRecord, error) {
	rec, err := LoadLease(m.stateDir, resource)
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &LeaseRecord{
		Resource:  resource,
		Key:       key,
		State:     LeaseHealthy,
		UpdatedAt: m.now(),
	}, nil
}

func (m *LeaseManager) setState(rec *LeaseRecord, state LeaseState, lastErr string) error {
	rec.State = state
	rec.LastError = boundLeaseError(lastErr)
	rec.UpdatedAt = m.now()
	if err := SaveLeaseAtomic(m.stateDir, rec); err != nil {
		return fmt.Errorf("persisting lease state %s for %s: %w", state, rec.Resource, err)
	}
	return nil
}

func (m *LeaseManager) runFlow(ctx context.Context, resource, key string) (*LeaseRecord, error) {
	rec, err := m.loadOrInit(resource, key)
	if err != nil {
		return nil, err
	}

	healthErr := m.healthCheck(ctx, resource)
	if healthErr == nil {
		if cerr := ctx.Err(); cerr != nil {
			if err := m.setState(rec, LeaseDegraded, cerr.Error()); err != nil {
				return nil, err
			}
			return rec, cerr
		}
		if err := m.setState(rec, LeaseHealthy, ""); err != nil {
			return nil, err
		}
		return rec, nil
	}

	if err := m.setState(rec, LeaseDegraded, healthErr.Error()); err != nil {
		return nil, err
	}
	if err := m.setState(rec, LeaseRepairPending, healthErr.Error()); err != nil {
		return nil, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return rec, cerr
	}

	if err := m.setState(rec, LeaseRepairing, healthErr.Error()); err != nil {
		return nil, err
	}
	if err := m.repair(ctx, resource); err != nil {
		if serr := m.setState(rec, LeaseFailed, err.Error()); serr != nil {
			return nil, serr
		}
		return rec, fmt.Errorf("lease repair for %s: %w", resource, err)
	}

	if err := m.healthCheck(ctx, resource); err != nil {
		if serr := m.setState(rec, LeaseFailed, err.Error()); serr != nil {
			return nil, serr
		}
		return rec, fmt.Errorf("lease post-repair health check for %s: %w", resource, err)
	}
	if cerr := ctx.Err(); cerr != nil {
		if err := m.setState(rec, LeaseDegraded, cerr.Error()); err != nil {
			return nil, err
		}
		return rec, cerr
	}
	if err := m.setState(rec, LeaseHealthy, ""); err != nil {
		return nil, err
	}
	return rec, nil
}
