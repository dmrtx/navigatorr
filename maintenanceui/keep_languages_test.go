package maintenanceui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func decodeKeep(t *testing.T, body string) keepLanguagesView {
	t.Helper()
	var v keepLanguagesView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err, body)
	}
	return v
}

func TestKeepLanguagesDefaultAndAuthentication(t *testing.T) {
	_, h := testUI(t)
	if request(h, "GET", "/api/maintenance/keep-languages", "", false).Code != 401 {
		t.Fatal("reading the setting needs authentication")
	}
	if request(h, "PUT", "/api/maintenance/keep-languages", `{"languages":["eng"]}`, false).Code != 401 {
		t.Fatal("changing the setting needs authentication")
	}
	w := request(h, "GET", "/api/maintenance/keep-languages", "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	v := decodeKeep(t, w.Body.String())
	want := []string{"eng", "jpn", "spa"}
	if !reflect.DeepEqual(v.Languages, want) || !reflect.DeepEqual(v.Default, want) || !v.IsDefault || v.Revision != 0 {
		t.Fatalf("default view: %+v", v)
	}
}

func TestKeepLanguagesPutStoresCanonicalList(t *testing.T) {
	s, h := testUI(t)
	w := request(h, "PUT", "/api/maintenance/keep-languages", `{"languages":["SPA","ger","deu","fre","jpn"]}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	v := decodeKeep(t, w.Body.String())
	if !reflect.DeepEqual(v.Languages, []string{"deu", "fra", "jpn", "spa"}) || v.IsDefault || v.Revision != 1 || v.UpdatedAt == "" {
		t.Fatalf("view after save: %+v", v)
	}
	if got := s.engine.KeepLanguages(); !reflect.DeepEqual(got, v.Languages) {
		t.Fatalf("the engine reads the saved list: %v", got)
	}
	w = request(h, "PUT", "/api/maintenance/keep-languages", `{"languages":["eng"]}`, true)
	if v = decodeKeep(t, w.Body.String()); v.Revision != 2 || !reflect.DeepEqual(v.Languages, []string{"eng"}) {
		t.Fatalf("second save: %+v", v)
	}
	// An empty list keeps every track and is a saved choice, not the default.
	w = request(h, "PUT", "/api/maintenance/keep-languages", `{"languages":[]}`, true)
	v = decodeKeep(t, w.Body.String())
	if w.Code != 200 || v.Languages == nil || len(v.Languages) != 0 || v.IsDefault {
		t.Fatalf("empty list: %d %+v", w.Code, v)
	}
	if got := s.engine.KeepLanguages(); len(got) != 0 {
		t.Fatalf("an empty saved list means every track is kept, got %v", got)
	}
}

func TestKeepLanguagesPutValidates(t *testing.T) {
	s, h := testUI(t)
	for _, body := range []string{
		`{"languages":["eng","xx"]}`,
		`{"languages":["english"]}`,
		`{"languages":["en"]}`,
		`{"languages":[""]}`,
		`{}`,
		`{"languages":null}`,
		`{"languages":"eng"}`,
		`{"languages":["eng"],"extra":1}`,
		`not json`,
	} {
		w := request(h, "PUT", "/api/maintenance/keep-languages", body, true)
		if w.Code != 400 {
			t.Errorf("%s: status %d, want 400 (%s)", body, w.Code, w.Body.String())
		}
	}
	w := request(h, "PUT", "/api/maintenance/keep-languages", `{"languages":["eng","xx"]}`, true)
	if !strings.Contains(w.Body.String(), `"field":"languages"`) || !strings.Contains(w.Body.String(), "ISO 639-2") {
		t.Errorf("the error names the field and the standard: %s", w.Body.String())
	}
	if row, _ := s.engine.Deps().Store.GetKeepLanguages(); row != nil {
		t.Fatalf("a rejected save stored %+v", row)
	}
}
