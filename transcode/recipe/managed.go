package recipe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	managedRegistrySchemaVersion = 1
	managedRegistryFilename      = "managed-profiles.json"
	maxManagedHistoryPerProfile  = 50
)

var managedProfileNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ManagedProfileRecord is a centrally managed profile stored by Navigatorr.
// Generation is monotonic per profile name; Digest identifies the normalized
// profile content independently of its name or descriptive metadata.
// SourceActionID is unverified reference metadata until a future
// promote-from-action flow validates provenance.
type ManagedProfileRecord struct {
	Name           string    `json:"name"`
	Profile        Profile   `json:"profile"`
	Digest         string    `json:"digest"`
	Generation     int64     `json:"generation"`
	Description    string    `json:"description,omitempty"`
	SourceActionID string    `json:"source_action_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ManagedProfileHistoryEntry records every save/delete transition. The stored
// profile is the exact normalized content that was active at that generation.
type ManagedProfileHistoryEntry struct {
	Event          string    `json:"event"`
	Name           string    `json:"name"`
	Profile        Profile   `json:"profile"`
	Digest         string    `json:"digest"`
	Generation     int64     `json:"generation"`
	Description    string    `json:"description,omitempty"`
	SourceActionID string    `json:"source_action_id,omitempty"`
	At             time.Time `json:"at"`
}

type managedRegistryFile struct {
	SchemaVersion int                                     `json:"schema_version"`
	Revision      int64                                   `json:"revision"`
	Profiles      map[string]ManagedProfileRecord         `json:"profiles"`
	History       map[string][]ManagedProfileHistoryEntry `json:"history,omitempty"`
}

// DecodeProfileStrict decodes exactly one Profile JSON document, rejects
// unknown fields, normalizes typed values/defaults and returns its stable
// content digest. It is shared by ephemeral action input and managed CRUD.
func DecodeProfileStrict(name string, data []byte) (Profile, string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return Profile{}, "", fmt.Errorf("decoding profile: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Profile{}, "", fmt.Errorf("profile must contain exactly one JSON document")
	}
	return NormalizeAndDigestProfile(name, p)
}

// NormalizeAndDigestProfile returns a detached, normalized and validated copy
// suitable for durable storage or immutable plan resolution.
func NormalizeAndDigestProfile(name string, in Profile) (Profile, string, error) {
	if !managedProfileNamePattern.MatchString(strings.TrimSpace(name)) {
		return Profile{}, "", fmt.Errorf("invalid profile name %q (allowed: letters, numbers, dash, underscore)", name)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return Profile{}, "", fmt.Errorf("copying profile: %w", err)
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return Profile{}, "", fmt.Errorf("copying profile: %w", err)
	}
	p.Container = strings.ToLower(strings.TrimSpace(p.Container))
	p.Video.Codec = strings.ToLower(strings.TrimSpace(p.Video.Codec))
	p.Video.Preset = strings.ToLower(strings.TrimSpace(p.Video.Preset))
	p.Video.Tune = strings.ToLower(strings.TrimSpace(p.Video.Tune))
	p.Video.Profile = strings.ToLower(strings.TrimSpace(p.Video.Profile))
	p.Video.PixelFormat = strings.ToLower(strings.TrimSpace(p.Video.PixelFormat))
	p.Audio.Mode = strings.ToLower(strings.TrimSpace(p.Audio.Mode))
	p.Subtitles.Mode = strings.ToLower(strings.TrimSpace(p.Subtitles.Mode))
	for i := range p.Resilience.Fallbacks {
		p.Resilience.Fallbacks[i].When = strings.ToLower(strings.TrimSpace(p.Resilience.Fallbacks[i].When))
		p.Resilience.Fallbacks[i].Action = strings.ToLower(strings.TrimSpace(p.Resilience.Fallbacks[i].Action))
	}
	if p.Optimization != nil && p.Optimization.Enabled {
		NormalizeOptimizationPolicy(p.Optimization)
	}
	if err := ValidateProfile(name, p); err != nil {
		return Profile{}, "", err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return Profile{}, "", fmt.Errorf("serializing normalized profile: %w", err)
	}
	sum := sha256.Sum256(b)
	return p, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func newManagedRegistry() managedRegistryFile {
	return managedRegistryFile{
		SchemaVersion: managedRegistrySchemaVersion,
		Profiles:      map[string]ManagedProfileRecord{},
		History:       map[string][]ManagedProfileHistoryEntry{},
	}
}

func (m *Manager) managedRegistryPath() (string, error) {
	if strings.TrimSpace(m.cacheDir) == "" {
		return "", fmt.Errorf("recipe cache is disabled; managed profiles require a persistent cache_dir")
	}
	return filepath.Join(m.cacheDir, managedRegistryFilename), nil
}

func (m *Manager) readManagedRegistryLocked() (managedRegistryFile, error) {
	path, err := m.managedRegistryPath()
	if err != nil {
		return managedRegistryFile{}, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newManagedRegistry(), nil
	}
	if err != nil {
		return managedRegistryFile{}, fmt.Errorf("reading managed profile registry: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var reg managedRegistryFile
	if err := dec.Decode(&reg); err != nil {
		return managedRegistryFile{}, fmt.Errorf("decoding managed profile registry: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return managedRegistryFile{}, fmt.Errorf("managed profile registry must contain exactly one JSON document")
	}
	if reg.SchemaVersion != managedRegistrySchemaVersion {
		return managedRegistryFile{}, fmt.Errorf("unsupported managed profile registry schema_version %d", reg.SchemaVersion)
	}
	if reg.Profiles == nil {
		reg.Profiles = map[string]ManagedProfileRecord{}
	}
	if reg.History == nil {
		reg.History = map[string][]ManagedProfileHistoryEntry{}
	}
	for name, rec := range reg.Profiles {
		norm, digest, err := NormalizeAndDigestProfile(name, rec.Profile)
		if err != nil {
			return managedRegistryFile{}, fmt.Errorf("managed profile %q is invalid: %w", name, err)
		}
		if rec.Name != "" && rec.Name != name {
			return managedRegistryFile{}, fmt.Errorf("managed profile key/name mismatch for %q", name)
		}
		if rec.Digest != "" && rec.Digest != digest {
			return managedRegistryFile{}, fmt.Errorf("managed profile %q digest mismatch", name)
		}
		rec.Name = name
		rec.Profile = norm
		rec.Digest = digest
		reg.Profiles[name] = rec
	}
	return reg, nil
}

func (m *Manager) writeManagedRegistryLocked(reg managedRegistryFile) error {
	path, err := m.managedRegistryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating managed profile registry directory: %w", err)
	}
	reg.SchemaVersion = managedRegistrySchemaVersion
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("serializing managed profile registry: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".managed-profiles-*.tmp")
	if err != nil {
		return fmt.Errorf("creating managed profile registry temp file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0644); err != nil {
		return fmt.Errorf("setting managed profile registry permissions: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		return fmt.Errorf("writing managed profile registry: %w", err)
	}
	if _, err := tmp.Write([]byte("\n")); err != nil {
		return fmt.Errorf("writing managed profile registry newline: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing managed profile registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing managed profile registry: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("activating managed profile registry: %w", err)
	}
	ok = true
	return nil
}

// ListManagedProfiles returns managed profiles in stable name order.
func (m *Manager) ListManagedProfiles() ([]ManagedProfileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reg, err := m.readManagedRegistryLocked()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(reg.Profiles))
	for name := range reg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ManagedProfileRecord, 0, len(names))
	for _, name := range names {
		out = append(out, reg.Profiles[name])
	}
	return out, nil
}

// GetManagedProfile returns a managed profile by name.
func (m *Manager) GetManagedProfile(name string) (ManagedProfileRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reg, err := m.readManagedRegistryLocked()
	if err != nil {
		return ManagedProfileRecord{}, false, err
	}
	rec, ok := reg.Profiles[strings.TrimSpace(name)]
	return rec, ok, nil
}

// SaveManagedProfile creates a managed profile or replaces an existing one.
// New names omit expectedGeneration/expectedDigest. Replacing an existing
// profile requires both the active generation and digest so metadata-only
// changes and ABA profile-content cycles cannot satisfy a stale CAS.
// Generation remains monotonic per profile name across delete/recreate cycles.
func (m *Manager) SaveManagedProfile(name string, profile Profile, description, sourceActionID string, expectedGeneration int64, expectedDigest string) (ManagedProfileRecord, error) {
	name = strings.TrimSpace(name)
	norm, digest, err := NormalizeAndDigestProfile(name, profile)
	if err != nil {
		return ManagedProfileRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	reg, err := m.readManagedRegistryLocked()
	if err != nil {
		return ManagedProfileRecord{}, err
	}
	current, exists := reg.Profiles[name]
	expectedDigest = strings.TrimSpace(expectedDigest)
	if exists {
		if expectedGeneration <= 0 {
			return ManagedProfileRecord{}, fmt.Errorf("managed profile %q already exists; expected_generation is required to update it", name)
		}
		if expectedDigest == "" {
			return ManagedProfileRecord{}, fmt.Errorf("managed profile %q already exists; expected_digest is required to update it", name)
		}
		if current.Generation != expectedGeneration {
			return ManagedProfileRecord{}, fmt.Errorf("managed profile %q changed: expected generation %d, current %d", name, expectedGeneration, current.Generation)
		}
		if current.Digest != expectedDigest {
			return ManagedProfileRecord{}, fmt.Errorf("managed profile %q changed: expected digest %s, current %s", name, expectedDigest, current.Digest)
		}
	} else if expectedGeneration != 0 || expectedDigest != "" {
		return ManagedProfileRecord{}, fmt.Errorf("managed profile %q does not exist; expected_generation/expected_digest cannot create it", name)
	}

	nextGeneration := int64(1)
	for _, entry := range reg.History[name] {
		if entry.Generation >= nextGeneration {
			nextGeneration = entry.Generation + 1
		}
	}
	if exists && current.Generation >= nextGeneration {
		nextGeneration = current.Generation + 1
	}

	now := time.Now().UTC()
	rec := ManagedProfileRecord{
		Name:           name,
		Profile:        norm,
		Digest:         digest,
		Generation:     nextGeneration,
		Description:    strings.TrimSpace(description),
		SourceActionID: strings.TrimSpace(sourceActionID),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if exists {
		rec.CreatedAt = current.CreatedAt
		if rec.Description == "" {
			rec.Description = current.Description
		}
	}
	reg.Revision++
	reg.Profiles[name] = rec
	entry := ManagedProfileHistoryEntry{
		Event:          "saved",
		Name:           name,
		Profile:        rec.Profile,
		Digest:         rec.Digest,
		Generation:     rec.Generation,
		Description:    rec.Description,
		SourceActionID: rec.SourceActionID,
		At:             now,
	}
	reg.History[name] = append(reg.History[name], entry)
	if len(reg.History[name]) > maxManagedHistoryPerProfile {
		reg.History[name] = append([]ManagedProfileHistoryEntry(nil), reg.History[name][len(reg.History[name])-maxManagedHistoryPerProfile:]...)
	}
	if err := m.writeManagedRegistryLocked(reg); err != nil {
		return ManagedProfileRecord{}, err
	}
	return rec, nil
}

// DeleteManagedProfile removes the active managed override while preserving an
// audit entry. expectedGeneration and expectedDigest are mandatory so deletes
// cannot race metadata-only changes, content changes, or ABA content cycles.
// Running actions are unaffected because they already hold an immutable plan.
func (m *Manager) DeleteManagedProfile(name string, expectedGeneration int64, expectedDigest string) (ManagedProfileHistoryEntry, error) {
	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	reg, err := m.readManagedRegistryLocked()
	if err != nil {
		return ManagedProfileHistoryEntry{}, err
	}
	rec, exists := reg.Profiles[name]
	if !exists {
		return ManagedProfileHistoryEntry{}, fmt.Errorf("managed profile %q not found", name)
	}
	expectedDigest = strings.TrimSpace(expectedDigest)
	if expectedGeneration <= 0 {
		return ManagedProfileHistoryEntry{}, fmt.Errorf("expected_generation is required to delete managed profile %q", name)
	}
	if expectedDigest == "" {
		return ManagedProfileHistoryEntry{}, fmt.Errorf("expected_digest is required to delete managed profile %q", name)
	}
	if rec.Generation != expectedGeneration {
		return ManagedProfileHistoryEntry{}, fmt.Errorf("managed profile %q changed: expected generation %d, current %d", name, expectedGeneration, rec.Generation)
	}
	if rec.Digest != expectedDigest {
		return ManagedProfileHistoryEntry{}, fmt.Errorf("managed profile %q changed: expected digest %s, current %s", name, expectedDigest, rec.Digest)
	}
	entry := ManagedProfileHistoryEntry{
		Event:          "deleted",
		Name:           name,
		Profile:        rec.Profile,
		Digest:         rec.Digest,
		Generation:     rec.Generation,
		Description:    rec.Description,
		SourceActionID: rec.SourceActionID,
		At:             time.Now().UTC(),
	}
	delete(reg.Profiles, name)
	reg.Revision++
	reg.History[name] = append(reg.History[name], entry)
	if len(reg.History[name]) > maxManagedHistoryPerProfile {
		reg.History[name] = append([]ManagedProfileHistoryEntry(nil), reg.History[name][len(reg.History[name])-maxManagedHistoryPerProfile:]...)
	}
	if err := m.writeManagedRegistryLocked(reg); err != nil {
		return ManagedProfileHistoryEntry{}, err
	}
	return entry, nil
}

// ManagedProfileHistory returns the bounded audit trail for a profile name.
func (m *Manager) ManagedProfileHistory(name string) ([]ManagedProfileHistoryEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reg, err := m.readManagedRegistryLocked()
	if err != nil {
		return nil, err
	}
	h := reg.History[strings.TrimSpace(name)]
	return append([]ManagedProfileHistoryEntry(nil), h...), nil
}
