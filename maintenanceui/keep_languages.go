package maintenanceui

import (
	"errors"
	"net/http"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// keepLanguagesView is the global "languages to keep" setting the size and
// compact goals read when a job is created. Languages are ISO 639-2 codes in
// canonical form (the terminology spelling, so ger and deu are one language),
// sorted. An empty list turns the selection off: every track is kept.
type keepLanguagesView struct {
	Languages []string `json:"languages"`
	// Default is the list in force until one is saved; IsDefault says whether
	// the saved list is absent.
	Default   []string `json:"default"`
	IsDefault bool     `json:"is_default"`
	Revision  int64    `json:"revision"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// keepLanguages serves GET (the effective list) and PUT (validate, store).
// A saved list applies to jobs created afterwards; a job freezes the list it
// started with, so a change never reaches work already planned.
func (s *Server) keepLanguages(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Deps().Store
	if st == nil {
		fail(w, 500, "settings store unavailable")
		return
	}
	if r.Method == http.MethodPut {
		var body struct {
			Languages *[]string `json:"languages"`
		}
		if decode(w, r, &body) != nil || body.Languages == nil {
			fail(w, 400, `send {"languages": ["eng", "spa", "jpn"]}; an empty list keeps every track`)
			return
		}
		languages, err := transcode.LanguageList(*body.Languages)
		if err != nil {
			var bad *transcode.InvalidLanguageError
			if errors.As(err, &bad) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_params", "field": "languages"})
				return
			}
			fail(w, 400, err.Error())
			return
		}
		if _, err := st.SetKeepLanguages(languages); err != nil {
			fail(w, 500, "languages to keep not saved")
			return
		}
	}
	row, err := st.GetKeepLanguages()
	if err != nil {
		fail(w, 500, "languages to keep unavailable")
		return
	}
	writeJSON(w, 200, keepLanguagesOf(row, s.engine.KeepLanguages()))
}

func keepLanguagesOf(row *store.KeepLanguageSettings, effective []string) keepLanguagesView {
	view := keepLanguagesView{Languages: effective, Default: transcode.DefaultKeepLanguages(), IsDefault: row == nil}
	if row != nil {
		view.Revision, view.UpdatedAt = row.Revision, row.UpdatedAt
	}
	return view
}
