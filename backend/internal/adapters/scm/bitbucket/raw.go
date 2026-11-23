package bitbucket

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// doRaw GETs path and returns the raw body of a 2xx response, for endpoints
// that serve plain text (pipeline step logs). headers are added to the request.
// Errors follow the same classification as do.
func (c *client) doRaw(ctx context.Context, path string, headers map[string]string) ([]byte, error) {
	creds, err := c.creds.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+path, nil)
	if err != nil {
		return nil, fmt.Errorf("bitbucket scm: build request: %w", err)
	}
	req.SetBasicAuth(creds.Username, creds.Token)
	req.Header.Set("User-Agent", c.userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	c.logger.Debug("bitbucket request", "method", http.MethodGet, "path", req.URL.Path)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitbucket scm: GET %s: %w", req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("bitbucket scm: read response: %w", err)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return raw, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("bitbucket scm: GET %s: %w", req.URL.Path, newAPIError(resp, raw, ErrAuthFailed))
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("bitbucket scm: GET %s: %w", req.URL.Path, newAPIError(resp, raw, ports.ErrSCMForbidden))
	default:
		return nil, newAPIError(resp, raw, nil)
	}
}

// tailLines returns the last n lines of s, ignoring one trailing newline.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
