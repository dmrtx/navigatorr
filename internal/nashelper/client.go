package nashelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxResponseBytes = 1 << 20
	// requestTimeout bounds one request that does not wait on a hash slot. Sample
	// may queue behind hashes, so its caller's context decides how long it waits.
	requestTimeout = 30 * time.Second
	pollMin        = 250 * time.Millisecond
	pollMax        = 2 * time.Second
)

// ErrUnavailable means the helper could not be reached or answered something that
// is not its protocol. The caller's answer is to read the file the old way and
// record that it did.
var ErrUnavailable = errors.New("nas helper unavailable")

// ErrRestarted means the helper restarted since this client first spoke to it, so
// the job or identity the caller holds belongs to a process that no longer exists.
var ErrRestarted = errors.New("nas helper restarted")

// PathMapping translates a directory in the coordinator's namespace into the same
// directory in the helper's.
type PathMapping struct {
	Local  string
	Remote string
}

// ClientConfig configures a Client.
type ClientConfig struct {
	URL      string
	Token    string
	Mappings []PathMapping
	// HTTPClient is used instead of the default, for tests.
	HTTPClient *http.Client
}

// Client talks to the helper. It is safe for concurrent use.
type Client struct {
	base     string
	token    string
	mappings []PathMapping
	http     *http.Client

	mu       sync.Mutex
	instance string
}

// ReadToken reads a bearer token from a file, dropping surrounding whitespace.
func ReadToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read nas helper token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < minTokenLength {
		return "", fmt.Errorf("nas helper token in %s is shorter than %d characters", path, minTokenLength)
	}
	return token, nil
}

// NewClient validates the configuration; it makes no request.
func NewClient(cfg ClientConfig) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("nas helper url must be an http(s) URL with a host")
	}
	if len(cfg.Token) < minTokenLength {
		return nil, fmt.Errorf("nas helper token must be at least %d characters", minTokenLength)
	}
	if len(cfg.Mappings) == 0 {
		return nil, errors.New("nas helper needs at least one path mapping")
	}
	for _, m := range cfg.Mappings {
		if !filepath.IsAbs(m.Local) || !filepath.IsAbs(m.Remote) {
			return nil, fmt.Errorf("nas helper path mapping %q -> %q must be absolute", m.Local, m.Remote)
		}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 0, // per-request contexts decide
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       time.Minute,
		}}
	}
	// The token must never follow a redirect to another host.
	hcCopy := *hc
	hcCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		base:     strings.TrimRight(u.String(), "/"),
		token:    cfg.Token,
		mappings: append([]PathMapping(nil), cfg.Mappings...),
		http:     &hcCopy,
	}, nil
}

// Remote translates a coordinator path to the helper's namespace using the
// longest matching mapping. It reports false when no mapping covers the path, or
// when the path is not clean: the helper never sees a path the coordinator did
// not state exactly.
func (c *Client) Remote(local string) (string, bool) {
	if !filepath.IsAbs(local) || filepath.Clean(local) != local {
		return "", false
	}
	best := -1
	for i, m := range c.mappings {
		prefix := strings.TrimSuffix(m.Local, "/")
		if local != prefix && !strings.HasPrefix(local, prefix+"/") {
			continue
		}
		if best < 0 || len(m.Local) > len(c.mappings[best].Local) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	m := c.mappings[best]
	rest := strings.TrimPrefix(local, strings.TrimSuffix(m.Local, "/"))
	return strings.TrimSuffix(m.Remote, "/") + rest, true
}

// Health asks the helper who it is. It also records the instance, so a later
// restart is noticed.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := c.do(ctx, http.MethodGet, "/v1/health", nil, &h); err != nil {
		return Health{}, err
	}
	if h.InstanceID == "" {
		return Health{}, fmt.Errorf("%w: health answered without an instance", ErrUnavailable)
	}
	if err := c.checkInstance(h.InstanceID); err != nil {
		return Health{}, err
	}
	return h, nil
}

// Identity reports what the library's filesystem says about the file.
func (c *Client) Identity(ctx context.Context, local string) (Identity, error) {
	var resp IdentityResponse
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := c.pathCall(ctx, "/v1/identity", local, &resp); err != nil {
		return Identity{}, err
	}
	if err := c.checkInstance(resp.InstanceID); err != nil {
		return Identity{}, err
	}
	return resp.Identity, nil
}

// Sample returns the sampled-range digest of the file and the identity it was
// taken under. It may wait behind running hashes; ctx bounds that wait.
func (c *Client) Sample(ctx context.Context, local string) (SampleResponse, error) {
	var resp SampleResponse
	if err := c.pathCall(ctx, "/v1/sample", local, &resp); err != nil {
		return SampleResponse{}, err
	}
	if err := c.checkInstance(resp.InstanceID); err != nil {
		return SampleResponse{}, err
	}
	return resp, nil
}

// HashProgress receives the bytes read so far and the total, roughly every poll.
type HashProgress func(read, total int64)

// Hash has the helper read the whole file locally and returns the digest with the
// identity that held steady while it was read. It returns when the hash is done,
// fails, or ctx ends; in the last case the job is cancelled on the helper.
func (c *Client) Hash(ctx context.Context, local string, progress HashProgress) (HashResult, error) {
	var job Job
	startCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	err := c.pathCall(startCtx, "/v1/hash", local, &job)
	cancel()
	if err != nil {
		return HashResult{}, err
	}
	if err := c.checkInstance(job.InstanceID); err != nil {
		return HashResult{}, err
	}
	if job.ID == "" {
		return HashResult{}, fmt.Errorf("%w: hash started without a job id", ErrUnavailable)
	}
	instance := job.InstanceID
	wait := pollMin
	for {
		switch job.State {
		case JobDone:
			if job.Result == nil {
				return HashResult{}, fmt.Errorf("%w: a finished hash carried no result", ErrUnavailable)
			}
			return *job.Result, nil
		case JobFailed, JobCancelled:
			if job.Error != nil {
				return HashResult{}, job.Error
			}
			return HashResult{}, &Error{Code: CodeInternal, Message: "the hash " + job.State}
		}
		if progress != nil {
			progress(job.BytesRead, job.TotalBytes)
		}
		select {
		case <-ctx.Done():
			c.cancelJob(job.ID)
			return HashResult{}, ctx.Err()
		case <-time.After(wait):
		}
		if wait < pollMax {
			wait *= 2
			if wait > pollMax {
				wait = pollMax
			}
		}
		pollCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		var next Job
		err := c.do(pollCtx, http.MethodGet, "/v1/jobs/"+url.PathEscape(job.ID), nil, &next)
		cancel()
		if err != nil {
			var apiErr *Error
			if errors.As(err, &apiErr) && apiErr.Code == CodeNotFound {
				// The helper forgot the job: it restarted, or the job aged out.
				return HashResult{}, fmt.Errorf("%w: the hash job is gone", ErrRestarted)
			}
			if ctx.Err() != nil {
				c.cancelJob(job.ID)
				return HashResult{}, ctx.Err()
			}
			return HashResult{}, err
		}
		if next.InstanceID != instance {
			return HashResult{}, ErrRestarted
		}
		job = next
	}
}

// cancelJob is best effort: it uses its own short context because the caller's
// has usually ended.
func (c *Client) cancelJob(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.do(ctx, http.MethodDelete, "/v1/jobs/"+url.PathEscape(id), nil, nil)
}

func (c *Client) pathCall(ctx context.Context, route, local string, out any) error {
	remote, ok := c.Remote(local)
	if !ok {
		return &Error{Code: CodeOutside, Message: "no helper path mapping covers this path"}
	}
	return c.do(ctx, http.MethodPost, route, PathRequest{Path: remote}, out)
}

// checkInstance pins the first instance seen and reports a different one later.
func (c *Client) checkInstance(id string) error {
	if id == "" {
		return fmt.Errorf("%w: answer without an instance", ErrUnavailable)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.instance == "" {
		c.instance = id
		return nil
	}
	if c.instance != id {
		c.instance = id
		return ErrRestarted
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, route string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+route, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The URL error text carries no header, so the token cannot leak here.
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: reading the answer: %v", ErrUnavailable, err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("%w: answer too large", ErrUnavailable)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: answer is not the helper protocol", ErrUnavailable)
		}
		return nil
	}
	var wrapped struct {
		Error *Error `json:"error"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Error != nil && wrapped.Error.Code != "" {
		return wrapped.Error
	}
	return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
}

// IsUnavailable reports that the helper could not give an answer at all, as
// opposed to answering that the file is missing or changed.
func IsUnavailable(err error) bool {
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRestarted) {
		return true
	}
	var apiErr *Error
	return errors.As(err, &apiErr) && (apiErr.Code == CodeAuth || apiErr.Code == CodeInternal || apiErr.Code == CodeBusy)
}
