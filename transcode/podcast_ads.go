package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/jakenesler/navigatorr/podcast"
)

// Optional capability keeps existing SSH/video executors compatible.
type PodcastAdExecutor interface {
	AdCatalog(context.Context, string) (podcast.AdCatalog, error)
	RevokeAd(context.Context, string, string) error
}

func (e *HTTPExecutor) AdCatalog(ctx context.Context, scope string) (podcast.AdCatalog, error) {
	var c podcast.AdCatalog
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	path := "/v1/podcast-ad-library?scope=" + url.QueryEscape(scope)
	req, err := e.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return c, err
	}
	b, code, _, err := e.do(req, "ad_catalog", "", false)
	if err != nil {
		return c, err
	}
	if code != 200 {
		return c, &HTTPError{Method: http.MethodGet, URL: redactURL(e.base + path), StatusCode: code, Message: parseErrorMessage(b)}
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Scope != scope {
		return c, fmt.Errorf("ad catalog scope differs")
	}
	return c, c.Validate()
}
func (e *HTTPExecutor) RevokeAd(ctx context.Context, scope, id string) error {
	if !podcast.ValidHash(id) {
		return fmt.Errorf("invalid reference ID")
	}
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	path := "/v1/podcast-ad-library?scope=" + url.QueryEscape(scope) + "&revoke=" + url.QueryEscape(id)
	req, err := e.newRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	b, code, _, err := e.do(req, "revoke_ad", "", true)
	if err != nil {
		return err
	}
	if code != 200 {
		return &HTTPError{Method: http.MethodPost, URL: redactURL(e.base + path), StatusCode: code, Message: parseErrorMessage(b)}
	}
	var ack struct {
		Revoked bool `json:"revoked"`
	}
	if json.Unmarshal(b, &ack) != nil || !ack.Revoked {
		return &UncertainError{Op: "revoke_ad", Err: fmt.Errorf("invalid revocation acknowledgement; inspect catalog before retry")}
	}
	return nil
}
