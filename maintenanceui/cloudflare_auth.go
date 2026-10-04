package maintenanceui

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Cloudflare's assertion is verified cryptographically, never accepted based on
// the presence of its header, a forwarded email, or a locally supplied cookie.
type cloudflareAccessVerifier struct {
	issuer, audience   string
	client             *http.Client
	mu                 sync.Mutex
	keys               map[string]*rsa.PublicKey
	expires, nextFetch time.Time
}

func newCloudflareVerifier(issuer, audience string) *cloudflareAccessVerifier {
	return &cloudflareAccessVerifier{issuer: issuer, audience: audience, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, keys: map[string]*rsa.PublicKey{}}
}

func (v *cloudflareAccessVerifier) verify(ctx context.Context, token string) error {
	if len(token) == 0 || len(token) > 16<<10 {
		return fmt.Errorf("invalid assertion size")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[0]) > 2048 {
		return fmt.Errorf("invalid assertion encoding")
	}
	decode := base64.RawURLEncoding.Strict().DecodeString
	header, err := decode(parts[0])
	if err != nil {
		return err
	}
	var h struct {
		Alg      string   `json:"alg"`
		Kid      string   `json:"kid"`
		Critical []string `json:"crit"`
		Base64   *bool    `json:"b64"`
	}
	if json.Unmarshal(header, &h) != nil || h.Alg != "RS256" || h.Kid == "" || len(h.Kid) > 256 || len(h.Critical) != 0 || h.Base64 != nil && !*h.Base64 {
		return fmt.Errorf("unsupported assertion signing header")
	}
	body, err := decode(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Issuer    string          `json:"iss"`
		Audience  json.RawMessage `json:"aud"`
		Expires   int64           `json:"exp"`
		NotBefore int64           `json:"nbf"`
		IssuedAt  int64           `json:"iat"`
	}
	if json.Unmarshal(body, &claims) != nil {
		return fmt.Errorf("invalid assertion claims")
	}
	now := time.Now().Unix()
	if claims.Issuer != v.issuer || claims.Expires <= now || claims.NotBefore > now || claims.IssuedAt > now {
		return fmt.Errorf("invalid assertion issuer or lifetime")
	}
	var audiences []string
	if json.Unmarshal(claims.Audience, &audiences) != nil {
		var single string
		if json.Unmarshal(claims.Audience, &single) != nil {
			return fmt.Errorf("invalid assertion audience")
		}
		audiences = []string{single}
	}
	match := false
	for _, aud := range audiences {
		if aud == v.audience {
			match = true
		}
	}
	if !match {
		return fmt.Errorf("wrong assertion audience")
	}
	signature, err := decode(parts[2])
	if err != nil {
		return err
	}
	key, err := v.key(ctx, h.Kid)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature)
}

func (v *cloudflareAccessVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if key := v.keys[kid]; key != nil && now.Before(v.expires) {
		return key, nil
	}
	// Unknown kids can rotate the cache, but cannot force one outbound call per
	// request. A failed fetch never makes an expired cache authoritative again.
	if now.Before(v.nextFetch) {
		return nil, fmt.Errorf("Access signing key unavailable")
	}
	v.nextFetch = now.Add(30 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.issuer+"/cdn-cgi/access/certs", nil)
	if err != nil {
		return nil, err
	}
	response, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Access key retrieval failed")
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(payload) > 1<<20 {
		return nil, fmt.Errorf("Access key response exceeds limit")
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if json.Unmarshal(payload, &jwks) != nil || len(jwks.Keys) == 0 || len(jwks.Keys) > 16 {
		return nil, fmt.Errorf("invalid Access key set")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jwk := range jwks.Keys {
		if jwk.Kty != "RSA" || jwk.Alg != "RS256" || jwk.Use != "sig" || jwk.Kid == "" || len(jwk.Kid) > 256 {
			continue
		}
		n, err := base64.RawURLEncoding.Strict().DecodeString(jwk.N)
		if err != nil || len(n) > 512 {
			continue
		}
		e, err := base64.RawURLEncoding.Strict().DecodeString(jwk.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 || modulus.BitLen() > 4096 {
			continue
		}
		exponent := int64(0)
		for _, b := range e {
			exponent = exponent<<8 + int64(b)
		}
		if exponent < 3 || exponent > 2147483647 || exponent%2 == 0 {
			continue
		}
		if keys[jwk.Kid] != nil {
			return nil, fmt.Errorf("ambiguous Access key set")
		}
		keys[jwk.Kid] = &rsa.PublicKey{N: modulus, E: int(exponent)}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no supported Access signing keys")
	}
	v.keys = keys
	v.expires = now.Add(5 * time.Minute)
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("unknown Access signing key")
}
