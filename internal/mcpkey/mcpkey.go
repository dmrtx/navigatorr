// Package mcpkey holds the API key that guards the /mcp endpoint, shared by
// the Transcode server (which checks it) and the MCP gateway (which sends it).
//
// The key is read once at start-up from a file. Surrounding whitespace is
// trimmed first, then the key must be visible ASCII (no spaces, line breaks or
// control characters) and at least MinLength characters long; anything
// else is an error, so a missing, empty or short file stops the binary instead
// of leaving /mcp open or unusable.
package mcpkey

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// MinLength is the shortest key accepted after trimming.
const MinLength = 32

// HeaderName is the header the gateway sends the key in. Authorization:
// Bearer is accepted too.
const HeaderName = "X-Api-Key"

// Load reads the key from path. The error never contains the key.
func Load(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("mcp key file path is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read mcp key file: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("mcp key file %s is empty", path)
	}
	// Only visible ASCII: the key travels in an HTTP header, where control
	// characters and line breaks cannot go, and one byte is one character.
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return "", fmt.Errorf("mcp key in %s must be visible ASCII with no spaces or line breaks", path)
		}
	}
	if len(key) < MinLength {
		return "", fmt.Errorf("mcp key in %s is shorter than %d characters", path, MinLength)
	}
	return key, nil
}

// Require wraps next so that every request, whatever its method or path,
// must carry key in X-Api-Key or Authorization: Bearer. It answers 401 when
// the key is missing or wrong, when either header is repeated, when a Bearer
// is empty, or when both headers are present and differ.
func Require(key string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(key))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := presented(r.Header)
		if ok {
			h := sha256.Sum256([]byte(got))
			ok = subtle.ConstantTimeCompare(h[:], want[:]) == 1
		}
		if !ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// presented returns the key a request carries and whether the headers are
// well formed. A non-Bearer Authorization (such as Basic) is not a key and is
// ignored.
func presented(h http.Header) (string, bool) {
	apiKeys := h.Values(HeaderName)
	auths := h.Values("Authorization")
	if len(apiKeys) > 1 || len(auths) > 1 {
		return "", false
	}
	var apiKey, bearer string
	hasAPIKey, hasBearer := len(apiKeys) == 1, false
	if hasAPIKey {
		apiKey = strings.TrimSpace(apiKeys[0])
		if apiKey == "" {
			return "", false
		}
	}
	if len(auths) == 1 {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(auths[0]), " ")
		if strings.EqualFold(scheme, "Bearer") {
			hasBearer = true
			bearer = strings.TrimSpace(rest)
			if bearer == "" {
				return "", false
			}
		}
	}
	switch {
	case hasAPIKey && hasBearer:
		if subtle.ConstantTimeCompare([]byte(apiKey), []byte(bearer)) != 1 {
			return "", false
		}
		return apiKey, true
	case hasAPIKey:
		return apiKey, true
	case hasBearer:
		return bearer, true
	}
	return "", false
}
