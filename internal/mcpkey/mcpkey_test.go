package mcpkey

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKey is built at run time so secret scanners do not read it as a credential.
var testKey = strings.Repeat("k", MinLength)

func writeKey(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file: want error")
	}
	if _, err := Load(""); err == nil {
		t.Fatal("empty path: want error")
	}
	if _, err := Load(writeKey(t, "")); err == nil {
		t.Fatal("empty file: want error")
	}
	if _, err := Load(writeKey(t, " \n\t\n")); err == nil {
		t.Fatal("whitespace-only file: want error")
	}
	// 31 characters once trimmed, padded to look long before trimming.
	short := "  " + testKey[:31] + "\n\n"
	if _, err := Load(writeKey(t, short)); err == nil {
		t.Fatal("short key after trimming: want error")
	}
	_, err := Load(writeKey(t, testKey[:31]))
	if err == nil || strings.Contains(err.Error(), testKey[:31]) {
		t.Fatalf("short key error must not leak the key: %v", err)
	}
	for name, content := range map[string]string{
		"multibyte, 32 bytes but 16 characters": strings.Repeat("é", 16),
		"inner line break":                      testKey[:16] + "\n" + testKey[16:],
		"inner carriage return":                 testKey[:16] + "\r" + testKey[16:],
		"inner space":                           testKey[:16] + " " + testKey[16:],
		"inner NUL":                             testKey[:16] + "\x00" + testKey[16:],
		"two lines":                             "# comment\n" + testKey,
	} {
		if _, err := Load(writeKey(t, content)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	got, err := Load(writeKey(t, testKey+"\n"))
	if err != nil || got != testKey {
		t.Fatalf("valid key with trailing newline: got %q, %v", got, err)
	}
}

func TestRequire(t *testing.T) {
	h := Require(testKey, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	bad := strings.Repeat("x", len(testKey))
	type hdr struct{ k, v string }
	cases := []struct {
		name    string
		headers []hdr
		want    int
	}{
		{"no key", nil, 401},
		{"wrong X-Api-Key", []hdr{{"X-Api-Key", bad}}, 401},
		{"good X-Api-Key", []hdr{{"X-Api-Key", testKey}}, 418},
		{"empty X-Api-Key", []hdr{{"X-Api-Key", ""}}, 401},
		{"good Bearer", []hdr{{"Authorization", "Bearer " + testKey}}, 418},
		{"lowercase bearer", []hdr{{"Authorization", "bearer " + testKey}}, 418},
		{"mixed case bearer", []hdr{{"Authorization", "BeArEr " + testKey}}, 418},
		{"wrong Bearer", []hdr{{"Authorization", "Bearer " + bad}}, 401},
		{"empty Bearer", []hdr{{"Authorization", "Bearer "}}, 401},
		{"bare Bearer", []hdr{{"Authorization", "Bearer"}}, 401},
		{"Basic is not a key", []hdr{{"Authorization", "Basic " + testKey}}, 401},
		{"Basic beside a good X-Api-Key", []hdr{{"Authorization", "Basic abc"}, {"X-Api-Key", testKey}}, 418},
		{"repeated X-Api-Key", []hdr{{"X-Api-Key", testKey}, {"X-Api-Key", testKey}}, 401},
		{"repeated Authorization", []hdr{{"Authorization", "Bearer " + testKey}, {"Authorization", "Bearer " + testKey}}, 401},
		{"both equal", []hdr{{"X-Api-Key", testKey}, {"Authorization", "Bearer " + testKey}}, 418},
		{"both differ", []hdr{{"X-Api-Key", testKey}, {"Authorization", "Bearer " + bad}}, 401},
		{"both differ, bearer good", []hdr{{"X-Api-Key", bad}, {"Authorization", "Bearer " + testKey}}, 401},
		{"empty X-Api-Key, good Bearer", []hdr{{"X-Api-Key", ""}, {"Authorization", "Bearer " + testKey}}, 401},
		{"good key, empty Bearer", []hdr{{"X-Api-Key", testKey}, {"Authorization", "Bearer "}}, 401},
	}
	for _, c := range cases {
		for _, method := range []string{"POST", "GET", "DELETE", "PUT", "OPTIONS"} {
			for _, path := range []string{"/mcp", "/mcp/x"} {
				r := httptest.NewRequest(method, path, nil)
				for _, h := range c.headers {
					r.Header.Add(h.k, h.v)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != c.want {
					t.Errorf("%s %s %s: got %d, want %d", c.name, method, path, w.Code, c.want)
				}
				if w.Code == 401 && strings.Contains(w.Body.String(), testKey) {
					t.Errorf("%s: 401 body leaks the key", c.name)
				}
			}
		}
	}
}
