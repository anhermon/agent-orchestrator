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
	// maxRetryAfter caps a server-supplied Retry-After so a bogus value cannot
	// park the observer indefinitely.
	maxRetryAfter = time.Hour
)

// APIError is a non-success Bitbucket response that has no dedicated sentinel
// (401 and 403 carry ErrAuthFailed and ports.ErrSCMForbidden as the unwrapped
// cause). It satisfies the observer's rate-limit contract through GetRetryAfter
// and GetResetAt. Bitbucket sends only Retry-After, so ResetAt is normally zero.
type APIError struct {
	Status     int
	RetryAfter time.Duration
	ResetAt    time.Time
	Body       string
	// cause is the sentinel for 401/403, exposed through Unwrap.
	cause error
}

// Unwrap exposes the sentinel (ErrAuthFailed or ports.ErrSCMForbidden), if any.
func (e *APIError) Unwrap() error { return e.cause }

// Error formats the response status and a truncated body. It never includes
// request headers, so credentials cannot leak through it.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("bitbucket scm: HTTP %d", e.Status)
	if e.cause != nil {
		// The sentinels carry their own "bitbucket scm: " prefix.
		msg += ": " + strings.TrimPrefix(e.cause.Error(), "bitbucket scm: ")
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// RateLimitError is an APIError the server sent to throttle the client: HTTP
// 429, or 403 carrying Retry-After. Only this type satisfies the observer's
// rate-limit contract (GetRetryAfter/GetResetAt), so a 404, 5xx or plain 403
// never puts the provider into cooldown. It unwraps to the *APIError.
type RateLimitError struct {
	Err *APIError
}

func (e *RateLimitError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying *APIError (and through it any 403 sentinel).
func (e *RateLimitError) Unwrap() error { return e.Err }

// GetRetryAfter exposes the Retry-After hint for the observer's cooldown helper.
func (e *RateLimitError) GetRetryAfter() time.Duration { return e.Err.RetryAfter }

// GetResetAt exposes the reset-time hint for the observer's cooldown helper.
func (e *RateLimitError) GetResetAt() time.Time { return e.Err.ResetAt }

// throttleError wraps e as a *RateLimitError when the response is a throttle
// (429, or 403 with Retry-After); otherwise it returns e unchanged.
func throttleError(e *APIError) error {
	if e.Status == http.StatusTooManyRequests || (e.Status == http.StatusForbidden && e.RetryAfter > 0) {
		return &RateLimitError{Err: e}
	}
	return e
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

func newClient(opts clientOptions) (*client, error) {
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
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("bitbucket scm: invalid API base URL: %w", err)
	}
	if baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("bitbucket scm: invalid API base URL %q: scheme and host are required", base)
	}
	return &client{http: hc, creds: opts.creds, apiBase: base, baseURL: baseURL, userAgent: ua, logger: logger}, nil
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
		return resp, fmt.Errorf("%s %s: %w", method, req.URL.Path, newAPIError(resp, raw, ErrAuthFailed))
	case resp.StatusCode == http.StatusForbidden:
		return resp, fmt.Errorf("%s %s: %w", method, req.URL.Path, throttleError(newAPIError(resp, raw, ports.ErrSCMForbidden)))
	default:
		return resp, throttleError(newAPIError(resp, raw, nil))
	}
}

func newAPIError(resp *http.Response, raw []byte, cause error) *APIError {
	if len(raw) > maxErrorBodyBytes {
		raw = raw[:maxErrorBodyBytes]
	}
	return &APIError{
		Status:     resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		// ToValidUTF8 drops a rune cut in half by the byte truncation.
		Body:  strings.TrimSpace(strings.ToValidUTF8(string(raw), "")),
		cause: cause,
	}
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// parseRetryAfter accepts delay-seconds or an HTTP-date, caps the result at
// maxRetryAfter, and returns 0 for an empty, invalid or already-past value.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var d time.Duration
	if sec, err := strconv.ParseInt(value, 10, 64); err == nil {
		if sec <= 0 {
			return 0
		}
		if sec > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		d = time.Duration(sec) * time.Second
	} else if isAllDigits(value) {
		// Digits that overflow int64 are still a (huge) delay, not garbage.
		return maxRetryAfter
	} else if at, err := http.ParseTime(value); err == nil {
		d = at.Sub(now)
	}
	if d <= 0 {
		return 0
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// paginate GETs path and follows Bitbucket's "next" links, calling each for
// every element of "values". It refuses a next URL whose scheme or host differs
// from APIBase so credentials are never sent to another host.
func (c *client) paginate(ctx context.Context, path string, query url.Values, each func(raw json.RawMessage) error) error {
	next := c.apiBase + path
	if len(query) > 0 {
		next += "?" + query.Encode()
	}
	visited := map[string]struct{}{}
	for page := 0; next != ""; page++ {
		if _, seen := visited[next]; seen {
			return fmt.Errorf("bitbucket scm: pagination repeated next URL for %s", path)
		}
		visited[next] = struct{}{}
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
