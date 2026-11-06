package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ConversionDefaultsKey names the setting that holds the goal form's defaults.
const ConversionDefaultsKey = "conversion_defaults"

// ConversionDefaults are the choices the goal form starts from. They only fill
// the form: a job freezes what it was sent.
type ConversionDefaults struct {
	HDR            string `json:"hdr"`
	MaxHeight      int    `json:"max_height"`
	Encoder        string `json:"encoder"`
	DropCommentary bool   `json:"drop_commentary"`
	PreferStereo   bool   `json:"prefer_stereo"`
}

// DefaultConversionDefaults holds until the owner saves others: SDR at the
// file's resolution, without commentary, every channel layout kept.
func DefaultConversionDefaults() ConversionDefaults {
	return ConversionDefaults{HDR: "sdr", MaxHeight: 2160, Encoder: "fast", DropCommentary: true}
}

// Validate checks values the form can show.
func (d ConversionDefaults) Validate() error {
	switch {
	case d.HDR != "sdr" && d.HDR != "keep":
		return fmt.Errorf("hdr must be sdr or keep")
	case d.MaxHeight != 2160 && d.MaxHeight != 1080:
		return fmt.Errorf("max_height must be 2160 or 1080")
	case d.Encoder != "fast" && d.Encoder != "small":
		return fmt.Errorf("encoder must be fast or small")
	}
	return nil
}

// ConversionDefaults reads the saved defaults. A stored value that no longer
// reads gives the built-in ones.
func (s *Store) ConversionDefaults() (ConversionDefaults, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var raw string
	err := s.db.QueryRow(`SELECT value FROM replace_settings WHERE key=?`, ConversionDefaultsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultConversionDefaults(), nil
	}
	if err != nil {
		return ConversionDefaults{}, err
	}
	var d ConversionDefaults
	if json.Unmarshal([]byte(raw), &d) != nil || d.Validate() != nil {
		return DefaultConversionDefaults(), nil
	}
	return d, nil
}

// SetConversionDefaults stores the defaults.
func (s *Store) SetConversionDefaults(d ConversionDefaults) error {
	if err := d.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`INSERT INTO replace_settings(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, ConversionDefaultsKey, string(raw), nowStr())
	return err
}
