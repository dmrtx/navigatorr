package recipe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type Candidate struct {
	Data     []byte
	Source   string
	Manifest *Manifest
}
type Provider interface {
	Fetch(context.Context) (Candidate, error)
	Name() string
}

type BuiltinProvider struct{}

func (BuiltinProvider) Name() string { return "builtin" }
func (BuiltinProvider) Fetch(context.Context) (Candidate, error) {
	return Candidate{Data: EmbeddedBytes(), Source: "embedded"}, nil
}

type FileProvider struct{ Path string }

func (p FileProvider) Name() string { return "file" }
func (p FileProvider) Fetch(context.Context) (Candidate, error) {
	if strings.TrimSpace(p.Path) == "" {
		return Candidate{}, fmt.Errorf("recipe file path is required")
	}
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return Candidate{}, fmt.Errorf("reading recipe file: %w", err)
	}
	return Candidate{Data: b, Source: p.Path}, nil
}

type HTTPManifestProvider struct {
	ManifestURL       string
	Client            *http.Client
	NavigatorrVersion string
	NameValue         string
}

func (p *HTTPManifestProvider) Name() string {
	if p.NameValue != "" {
		return p.NameValue
	}
	return "https"
}
func (p *HTTPManifestProvider) Fetch(ctx context.Context) (Candidate, error) {
	manifestURL, err := strictHTTPS(p.ManifestURL)
	if err != nil {
		return Candidate{}, fmt.Errorf("invalid recipe manifest URL: %w", err)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	manifestBytes, err := getBounded(ctx, client, manifestURL, 1<<20)
	if err != nil {
		return Candidate{}, fmt.Errorf("fetching recipe manifest: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(manifestBytes))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Candidate{}, fmt.Errorf("parsing recipe manifest: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Candidate{}, fmt.Errorf("recipe manifest must contain exactly one JSON document")
	}
	if m.SchemaVersion < MinSchemaVersion || m.SchemaVersion > LatestSchemaVersion {
		return Candidate{}, fmt.Errorf("unsupported manifest schema_version %d (supported: %d-%d)", m.SchemaVersion, MinSchemaVersion, LatestSchemaVersion)
	}
	if !safeToken.MatchString(m.BundleVersion) {
		return Candidate{}, fmt.Errorf("invalid manifest bundle_version %q", m.BundleVersion)
	}
	digest := strings.ToLower(strings.TrimSpace(m.BundleSHA256))
	digest = strings.TrimPrefix(digest, "sha256:")
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(digest) {
		return Candidate{}, fmt.Errorf("invalid manifest bundle_sha256")
	}
	if m.MinimumNavigatorrVersion != "" && p.NavigatorrVersion != "" && compareNumericVersions(p.NavigatorrVersion, m.MinimumNavigatorrVersion) < 0 {
		return Candidate{}, fmt.Errorf("recipe requires navigatorr >= %s (running %s)", m.MinimumNavigatorrVersion, p.NavigatorrVersion)
	}
	downloadURL, err := strictHTTPS(m.DownloadURL)
	if err != nil {
		return Candidate{}, fmt.Errorf("invalid recipe download URL: %w", err)
	}
	manifestParsed, _ := url.Parse(manifestURL)
	downloadParsed, _ := url.Parse(downloadURL)
	if p.Name() == "github" {
		if !strings.EqualFold(downloadParsed.Hostname(), "raw.githubusercontent.com") {
			return Candidate{}, fmt.Errorf("github recipe bundle download must use raw.githubusercontent.com")
		}
	} else if !strings.EqualFold(downloadParsed.Hostname(), manifestParsed.Hostname()) {
		return Candidate{}, fmt.Errorf("recipe bundle download must use the same host as its manifest")
	}
	data, err := getBounded(ctx, client, downloadURL, 4<<20)
	if err != nil {
		return Candidate{}, fmt.Errorf("fetching recipe bundle: %w", err)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != digest {
		return Candidate{}, fmt.Errorf("recipe bundle sha256 mismatch: expected %s got %s", digest, got)
	}
	snap, err := Parse(data)
	if err != nil {
		return Candidate{}, err
	}
	if snap.Identity.Version != m.BundleVersion {
		return Candidate{}, fmt.Errorf("manifest bundle_version %q does not match bundle %q", m.BundleVersion, snap.Identity.Version)
	}
	return Candidate{Data: data, Source: downloadURL, Manifest: &m}, nil
}

func NewGitHubProvider(repository, channel, revision, navigatorrVersion string) *HTTPManifestProvider {
	ref := strings.TrimSpace(revision)
	name := "github"
	if ref == "" {
		ref = strings.TrimSpace(channel)
	}
	return &HTTPManifestProvider{ManifestURL: fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/manifest.json", strings.TrimSpace(repository), url.PathEscape(ref)), NavigatorrVersion: navigatorrVersion, NameValue: name}
}

func strictHTTPS(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("only absolute https URLs without userinfo are allowed")
	}
	return u.String(), nil
}
func getBounded(ctx context.Context, c *http.Client, raw string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.Scheme != "https" {
		return nil, fmt.Errorf("redirected recipe request is not https")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	r := io.LimitReader(resp.Body, max+1)
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return b, nil
}
func compareNumericVersions(a, b string) int {
	parse := func(s string) []int {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		var out []int
		for _, p := range strings.Split(s, ".") {
			n := 0
			for _, r := range p {
				if r < '0' || r > '9' {
					break
				}
				n = n*10 + int(r-'0')
			}
			out = append(out, n)
		}
		return out
	}
	x, y := parse(a), parse(b)
	n := len(x)
	if len(y) > n {
		n = len(y)
	}
	for i := 0; i < n; i++ {
		var xv, yv int
		if i < len(x) {
			xv = x[i]
		}
		if i < len(y) {
			yv = y[i]
		}
		if xv < yv {
			return -1
		}
		if xv > yv {
			return 1
		}
	}
	return 0
}
