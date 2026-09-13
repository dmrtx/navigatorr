package recipe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPManifestProvider_SchemaCompatibility(t *testing.T) {
	bundleYAML := []byte(`
schema_version: 1
bundle_version: "v1.0"
containers:
  mkv:
    subtitle_copy: [subrip]
profiles:
  test:
    container: mkv
    video: {codec: hevc_videotoolbox, quality: 65}
    audio: {mode: copy}
    subtitles: {mode: preserve}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
`)
	sum := sha256.Sum256(bundleYAML)
	bundleSHA := hex.EncodeToString(sum[:])

	cases := []struct {
		name          string
		schemaVersion int
		wantErr       bool
		errContains   string
	}{
		{
			name:          "v1 manifest accepted (backward compatibility)",
			schemaVersion: 1,
			wantErr:       false,
		},
		{
			name:          "v2 manifest accepted (current version)",
			schemaVersion: 2,
			wantErr:       false,
		},
		{
			name:          "schema 0 rejected",
			schemaVersion: 0,
			wantErr:       true,
			errContains:   "unsupported manifest schema_version 0",
		},
		{
			name:          "future schema 3 rejected",
			schemaVersion: 3,
			wantErr:       true,
			errContains:   "unsupported manifest schema_version 3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ts *httptest.Server
			ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/manifest.json":
					manifest := Manifest{
						SchemaVersion: tc.schemaVersion,
						BundleVersion: "v1.0",
						BundleSHA256:  bundleSHA,
						DownloadURL:   ts.URL + "/bundle.yaml",
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(manifest)
				case "/bundle.yaml":
					w.Header().Set("Content-Type", "text/yaml")
					_, _ = w.Write(bundleYAML)
				default:
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()

			provider := &HTTPManifestProvider{
				ManifestURL: ts.URL + "/manifest.json",
				Client:      ts.Client(),
			}

			candidate, err := provider.Fetch(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("expected error containing %q, got %v", tc.errContains, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if candidate.Manifest == nil || candidate.Manifest.SchemaVersion != tc.schemaVersion {
					t.Errorf("expected candidate manifest schema %d, got %+v", tc.schemaVersion, candidate.Manifest)
				}
				if len(candidate.Data) == 0 {
					t.Errorf("expected candidate data to be non-empty")
				}
			}
		})
	}
}
