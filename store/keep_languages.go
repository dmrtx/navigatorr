package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// KeepLanguageSettings is the operator's global list of languages the size and
// compact goals keep audio and subtitle tracks for. Validation and the
// canonical spelling of the codes belong to the caller (transcode.LanguageList);
// the store keeps what it is given. An empty list is a real choice (keep every
// track), so a missing row, which means "never configured", is told apart from
// it. Revision counts saves, like the per-node slot settings.
type KeepLanguageSettings struct {
	Languages []string `json:"languages"`
	Revision  int64    `json:"revision"`
	UpdatedAt string   `json:"updated_at"`
}

// GetKeepLanguages returns the stored list, or nil when it has never been set.
func (s *Store) GetKeepLanguages() (*KeepLanguageSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var raw string
	out := KeepLanguageSettings{}
	err := s.db.QueryRow(`SELECT languages_json, revision, updated_at FROM keep_language_settings WHERE id=1`).Scan(&raw, &out.Revision, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &out.Languages); err != nil {
		return nil, fmt.Errorf("stored keep-languages list is unreadable: %w", err)
	}
	if out.Languages == nil {
		out.Languages = []string{}
	}
	return &out, nil
}

// SetKeepLanguages stores the list and bumps the revision.
func (s *Store) SetKeepLanguages(languages []string) (KeepLanguageSettings, error) {
	if languages == nil {
		languages = []string{}
	}
	raw, err := json.Marshal(languages)
	if err != nil {
		return KeepLanguageSettings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowStr()
	if _, err := s.db.Exec(`INSERT INTO keep_language_settings(id, languages_json, revision, updated_at) VALUES(1, ?, 1, ?)
		ON CONFLICT(id) DO UPDATE SET languages_json=excluded.languages_json, revision=keep_language_settings.revision+1, updated_at=excluded.updated_at`, string(raw), now); err != nil {
		return KeepLanguageSettings{}, err
	}
	var revision int64
	if err := s.db.QueryRow(`SELECT revision FROM keep_language_settings WHERE id=1`).Scan(&revision); err != nil {
		return KeepLanguageSettings{}, err
	}
	return KeepLanguageSettings{Languages: languages, Revision: revision, UpdatedAt: now}, nil
}
