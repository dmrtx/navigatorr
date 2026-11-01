package arrservice

import (
	"context"
	"net/http"
)

// DoRequestOnce sends a mutation exactly once. Workflows which persist an
// intent and reconcile uncertain effects must not use the pool's HTTP retries:
// even an upstream 5xx can follow a successfully queued command.
func (s *Service) DoRequestOnce(ctx context.Context, method, path string, query map[string]string, body []byte) ([]byte, int, error) {
	client := *httpClient
	// 307/308 preserve POST and its body. Following either would violate the
	// caller's one-attempt journal even though no explicit retry ran here.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	data, code, err := s.doRequest(ctx, &client, method, path, query, body, "")
	if s.Snapshots != nil && method != "GET" {
		s.Snapshots.Invalidate(s.Name)
	}
	return data, code, err
}
