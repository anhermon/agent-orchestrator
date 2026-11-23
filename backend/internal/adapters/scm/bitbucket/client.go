package bitbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// maxResponseBytes bounds how much of a response body is read.
	maxResponseBytes = 16 << 20
	// maxErrorBodyBytes bounds the response body kept on an APIError.
	maxErrorBodyBytes = 512
	// maxPages guards paginate against a server that never stops sending next.
	maxPages = 1000
)

// APIError is a non-success Bitbucket response that has no dedicated sentinel
// (401 and 403 map to ErrAuthFailed and ports.ErrSCMForbidden). It satisfies
// the observer's rate-limit contract through GetRetryAfter and GetResetAt.
// Bitbucket sends only Retry-After (seconds), so ResetAt is normally zero.
type APIError struct {
	Status     int
	RetryAfter time.Duration
	ResetAt    time.Time
	Body       string
}

// Error formats the response status and a truncated body. It never includes
// request headers, so credentials cannot leak through it.
func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("bitbucket scm: HTTP %d", e.Status)
	}
	return fmt.Sprintf("bitbucket scm: HTTP %d: %s", e.Status, e.Body)
}

// GetRetryAfter exposes the Retry-After hint for the observer's cooldown helper.
func (e *APIError) GetRetryAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.RetryAfter
}

// GetResetAt exposes the reset-time hint for the observer's cooldown helper.
func (e *APIError) GetResetAt() time.Time {
	if e == nil {
		return time.Time{}
	}
	return e.ResetAt
}

type clientOptions struct {
	httpClient *http.Client
	creds      CredentialSource
	apiBase    string
	userAgent  string
	logger     *slog.Logger
}

type client struct {
	http      *http.Client
	creds     CredentialSource
	apiBase   string
	baseURL   *url.URL
	userAgent string
	logger    *slog.Logger
}

func newClient(opts clientOptions) *client {
	hc := opts.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	base := strings.TrimRight(opts.apiBase, "/")
	if base == "" {
		base = defaultAPIBase
	}
	ua := opts.userAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	logger := opts.logger
	if logger == nil {
		logger = slog.Default()
	}
	baseURL, _ := url.Parse(base) // an unparseable base fails every request below
	return &client{http: hc, creds: opts.creds, apiBase: base, baseURL: baseURL, userAgent: ua, logger: logger}
}

// do sends one request to APIBase+path with basic auth. A non-nil body is sent
// as JSON; a non-nil out receives the decoded JSON of a 2xx response. The
// returned response has its body already consumed and closed.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body any, out any) (*http.Response, error) {
	full := c.apiBase + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	return c.doURL(ctx, method, full, body, out)
}

func (c *client) doURL(ctx context.Context, method, fullURL string, body any, out any) (*http.Response, error) {
	creds, err := c.creds.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("bitbucket scm: encode request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, fmt.Errorf("bitbucket scm: build request: %w", err)
	}
	req.SetBasicAuth(creds.Username, creds.Token)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.logger.Debug("bitbucket request", "method", method, "path", req.URL.Path)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitbucket scm: %s %s: %w", method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp, fmt.Errorf("bitbucket scm: read response: %w", err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return resp, fmt.Errorf("bitbucket scm: decode response: %w", err)
			}
		}
		return resp, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return resp, fmt.Errorf("bitbucket scm: %s %s: %w", method, req.URL.Path, ErrAuthFailed)
	case resp.StatusCode == http.StatusForbidden:
		return resp, fmt.Errorf("bitbucket scm: %s %s: %w", method, req.URL.Path, ports.ErrSCMForbidden)
	default:
		return resp, newAPIError(resp, raw)
	}
}

func newAPIError(resp *http.Response, raw []byte) *APIError {
	e := &APIError{Status: resp.StatusCode}
	if len(raw) > maxErrorBodyBytes {
		raw = raw[:maxErrorBodyBytes]
	}
	e.Body = strings.TrimSpace(string(raw))
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if sec, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && sec >= 0 {
			e.RetryAfter = time.Duration(sec) * time.Second
		}
	}
	return e
}

// paginate GETs path and follows Bitbucket's "next" links, calling each for
// every element of "values". It refuses a next URL whose scheme or host differs
// from APIBase so credentials are never sent to another host.
func (c *client) paginate(ctx context.Context, path string, query url.Values, each func(raw json.RawMessage) error) error {
	next := c.apiBase + path
	if len(query) > 0 {
		next += "?" + query.Encode()
	}
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return fmt.Errorf("bitbucket scm: pagination exceeded %d pages for %s", maxPages, path)
		}
		var body struct {
			Values []json.RawMessage `json:"values"`
			Next   string            `json:"next"`
		}
		if _, err := c.doURL(ctx, http.MethodGet, next, nil, &body); err != nil {
			return err
		}
		for _, raw := range body.Values {
			if err := each(raw); err != nil {
				return err
			}
		}
		if body.Next == "" {
			return nil
		}
		if err := c.checkSameHost(body.Next); err != nil {
			return err
		}
		next = body.Next
	}
	return nil
}

func (c *client) checkSameHost(next string) error {
	u, err := url.Parse(next)
	if err != nil || c.baseURL == nil {
		return errors.New("bitbucket scm: refusing unparseable next URL")
	}
	if !strings.EqualFold(u.Scheme, c.baseURL.Scheme) || !strings.EqualFold(u.Host, c.baseURL.Host) {
		return fmt.Errorf("bitbucket scm: refusing next URL on host %q (API host is %q)", u.Host, c.baseURL.Host)
	}
	return nil
}
