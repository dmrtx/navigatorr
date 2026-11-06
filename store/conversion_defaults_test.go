package store

import (
	"path/filepath"
	"testing"
)

func TestConversionDefaultsFallBackWhenTheStoredValueNoLongerReads(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "defaults.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.ConversionDefaults()
	if err != nil || got != DefaultConversionDefaults() {
		t.Fatalf("empty store: %+v %v", got, err)
	}
	want := ConversionDefaults{HDR: "keep", MaxHeight: 1080, Encoder: "small", PreferStereo: true}
	if err := st.SetConversionDefaults(want); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ConversionDefaults(); err != nil || got != want {
		t.Fatalf("saved: %+v %v", got, err)
	}
	if err := st.SetConversionDefaults(ConversionDefaults{HDR: "dolby", MaxHeight: 1080, Encoder: "small"}); err == nil {
		t.Fatal("an unknown HDR choice was saved")
	}
	for _, raw := range []string{`not json`, `{"hdr":"dolby","max_height":1080,"encoder":"fast"}`} {
		if _, err := st.db.Exec(`UPDATE replace_settings SET value=? WHERE key=?`, raw, ConversionDefaultsKey); err != nil {
			t.Fatal(err)
		}
		if got, err := st.ConversionDefaults(); err != nil || got != DefaultConversionDefaults() {
			t.Fatalf("%s: %+v %v", raw, got, err)
		}
	}
}
