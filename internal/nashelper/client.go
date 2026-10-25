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
	"time"
)

const (
	maxResponseBytes = 1 << 20
	// requestTimeout bounds one request that does not wait on a hash slot. Sample
	// may queue behind hashes, so its caller's context decides how long it waits.
	requestTimeout = 30 * time.Second
	// sampleTimeout caps a sample that waits for a hash slot; the caller's own
	// deadline, if shorter, wins.
	sampleTimeout = 5 * time.Minute
	pollMin       = 250 * time.Millisecond
	pollMax       = 2 * time.Second
	// maxPollFailures is how many polls in a row may fail before the hash is given
	// up (and cancelled on the helper).
	maxPollFailures = 3
)

// ErrUnavailable means the helper could not give a usable answer: unreachable,
// timed out, refused, not its protocol, or unable to serve this path. The
// caller's answer is to read the file the old way and record that it did.
var ErrUnavailable = errors.New("nas helper unavailable")

// ErrHelperCancelled means the helper itself ended a job nobody here cancelled:
// it is shutting down, or it judged the job abandoned (the caller's machine
// slept past its patience). Asking again is reasonable.
var ErrHelperCancelled = fmt.Errorf("%w: the hash was cancelled on the helper", ErrUnavailable)

// ErrRestarted means the helper restarted while a job was running, so the job
// belongs to a process that no longer exists. It is an unavailability too.
var ErrRestarted = fmt.Errorf("%w: the helper restarted", ErrUnavailable)

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
	timeout  time.Duration
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
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("nas helper url must be a base http(s) URL with a host and no credentials, path or query")
	}
	if len(cfg.Token) < minTokenLength {
		return nil, fmt.Errorf("nas helper token must be at least %d characters", minTokenLength)
	}
	if len(cfg.Mappings) == 0 {
		return nil, errors.New("nas helper needs at least one path mapping")
	}
	for _, m := range cfg.Mappings {
		for _, p := range []string{m.Local, m.Remote} {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
				return nil, fmt.Errorf("nas helper path mapping %q -> %q must be clean absolute directories other than /", m.Local, m.Remote)
			}
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
		timeout:  requestTimeout,
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

// Health asks the helper who it is.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	if err := c.do(ctx, c.timeout, http.MethodGet, "/v1/health", nil, &h); err != nil {
		return Health{}, err
	}
	if h.InstanceID == "" {
		return Health{}, fmt.Errorf("%w: health answered without an instance", ErrUnavailable)
	}
	return h, nil
}

// Identity reports what the library's filesystem says about the file, with the
// instance that said it: a caller that keeps the identity to compare later keeps
// the instance with it.
func (c *Client) Identity(ctx context.Context, local string) (IdentityResponse, error) {
	var resp IdentityResponse
	if err := c.pathCall(ctx, c.timeout, "/v1/identity", local, &resp); err != nil {
		return IdentityResponse{}, err
	}
	if resp.InstanceID == "" {
		return IdentityResponse{}, fmt.Errorf("%w: answer without an instance", ErrUnavailable)
	}
	return resp, nil
}

// Sample returns the sampled-range digest of the file and the identity it was
// taken under. It may wait behind running hashes, so it is bounded by the
// caller's deadline or sampleTimeout, whichever is shorter.
func (c *Client) Sample(ctx context.Context, local string) (SampleResponse, error) {
	var resp SampleResponse
	if err := c.pathCall(ctx, sampleTimeout, "/v1/sample", local, &resp); err != nil {
		return SampleResponse{}, err
	}
	if resp.InstanceID == "" {
		return SampleResponse{}, fmt.Errorf("%w: answer without an instance", ErrUnavailable)
	}
	return resp, nil
}

// HashProgress receives the bytes read so far and the total, roughly every poll.
type HashProgress func(read, total int64)

// HashResponse is a finished hash and the instance that took it.
type HashResponse struct {
	InstanceID string
	HashResult
}

// Hash has the helper read the whole file locally and returns the digest with the
// identity that held steady while it was read. It has no time limit of its own: a
// full read of a large file takes minutes, so the caller's context decides. On
// every way out before a result, the job is cancelled on the helper.
func (c *Client) Hash(ctx context.Context, local string, progress HashProgress) (HashResponse, error) {
	var job Job
	if err := c.pathCall(ctx, c.timeout, "/v1/hash", local, &job); err != nil {
		return HashResponse{}, err
	}
	if job.ID == "" {
		return HashResponse{}, fmt.Errorf("%w: hash started without a job id", ErrUnavailable)
	}
	// From here the job exists on the helper: every way out cancels it.
	finished := false
	defer func() {
		if !finished {
			c.cancelJob(job.ID)
		}
	}()
	if job.InstanceID == "" {
		return HashResponse{}, fmt.Errorf("%w: hash started without an instance", ErrUnavailable)
	}
	instance := job.InstanceID
	wait := pollMin
	failures := 0
	for {
		switch job.State {
		case JobDone:
			if job.Result == nil {
				return HashResponse{}, fmt.Errorf("%w: a finished hash carried no result", ErrUnavailable)
			}
			finished = true
			return HashResponse{InstanceID: instance, HashResult: *job.Result}, nil
		case JobFailed:
			finished = true
			if job.Error != nil {
				return HashResponse{}, job.Error
			}
			return HashResponse{}, &Error{Code: CodeInternal, Message: "the hash failed"}
		case JobCancelled:
			// Nobody here asked for that: the helper is shutting down.
			finished = true
			return HashResponse{}, ErrHelperCancelled
		case JobQueued, JobRunning:
		default:
			return HashResponse{}, fmt.Errorf("%w: unknown job state %q", ErrUnavailable, job.State)
		}
		if progress != nil {
			progress(job.BytesRead, job.TotalBytes)
		}
		select {
		case <-ctx.Done():
			return HashResponse{}, ctx.Err()
		case <-time.After(wait):
		}
		if wait < pollMax {
			wait = min(wait*2, pollMax)
		}
		var next Job
		err := c.do(ctx, c.timeout, http.MethodGet, "/v1/jobs/"+url.PathEscape(job.ID), nil, &next)
		if err != nil {
			var apiErr *Error
			switch {
			case ctx.Err() != nil:
				return HashResponse{}, ctx.Err()
			case errors.As(err, &apiErr) && apiErr.Code == CodeNotFound:
				// The helper forgot the job: it restarted, or the job aged out.
				return HashResponse{}, fmt.Errorf("%w: the hash job is gone", ErrRestarted)
			case IsAnswer(err):
				finished = true
				return HashResponse{}, err
			default:
				// Dropped connection, a 5xx, a busy or refused answer: give the
				// helper a couple of chances before reading the file the old way.
				if failures++; failures >= maxPollFailures {
					return HashResponse{}, err
				}
				continue
			}
		}
		failures = 0
		if next.InstanceID != instance {
			return HashResponse{}, ErrRestarted
		}
		job = next
	}
}

// cancelJob is best effort: it uses its own short context because the caller's
// has usually ended.
func (c *Client) cancelJob(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.do(ctx, 5*time.Second, http.MethodDelete, "/v1/jobs/"+url.PathEscape(id), nil, nil)
}

// ErrNotMapped means no path mapping covers the path, so the helper cannot be
// asked about it. It is an unavailability: the file is read the old way.
var ErrNotMapped = fmt.Errorf("%w: no path mapping covers this path", ErrUnavailable)

func (c *Client) pathCall(ctx context.Context, timeout time.Duration, route, local string, out any) error {
	remote, ok := c.Remote(local)
	if !ok {
		return ErrNotMapped
	}
	return c.do(ctx, timeout, http.MethodPost, route, PathRequest{Path: remote}, out)
}

// do runs one request bounded by timeout and the caller's context. When the
// caller's context ends, its error is returned as it is; when only the timeout
// ran out, the helper is unavailable.
func (c *Client) do(parent context.Context, timeout time.Duration, method, route string, body, out any) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	fail := func(what string, err error) error {
		if parent.Err() != nil {
			return parent.Err()
		}
		// A transport error names the URL, never a header, so the token cannot
		// leak through it.
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, what, err)
	}
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
		return fail("request", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fail("reading the answer", err)
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

// IsAnswer reports that the helper looked at the file and said something about
// it: it is missing, not a regular file, or changed while it was read. Every
// other failure (unreachable, timed out, refused, restarted, a path it does not
// serve, a code it does not know) means the helper could not help, and the caller
// reads the file the old way and records that it did. The caller's own context
// ending is neither.
//
// A not_found is only as good as the path mapping behind it: a caller about to
// act destructively on it confirms the absence on the mount first.
func IsAnswer(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case CodeNotFound, CodeNotFile, CodeChanged:
		return true
	}
	return false
}

// ShouldFallBack reports whether the caller should read the file through the
// mount instead: the helper could not answer, and the caller did not give up.
func ShouldFallBack(err error) bool {
	if err == nil || IsAnswer(err) {
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
