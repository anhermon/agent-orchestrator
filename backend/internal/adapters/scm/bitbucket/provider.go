package bitbucket

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	defaultAPIBase   = "https://api.bitbucket.org/2.0"
	defaultUserAgent = "agent-orchestrator-bitbucket"
	defaultTimeout   = 30 * time.Second
)

// ProviderOptions configures the Bitbucket Cloud SCM provider.
type ProviderOptions struct {
	HTTPClient  *http.Client
	Credentials CredentialSource
	// APIBase defaults to https://api.bitbucket.org/2.0. Tests point it at an
	// httptest server.
	APIBase   string
	UserAgent string
	Logger    *slog.Logger
}

// Provider implements the provider-neutral scm.Provider interface for
// Bitbucket Cloud.
type Provider struct {
	client *client
	creds  CredentialSource
	// forbiddenWarned records the repos whose commit statuses already produced
	// the "forbidden" warning, so a polling loop warns once per repo.
	forbiddenWarned sync.Map
	// sleep waits between async-merge polls; tests replace it. Nil uses a timer.
	sleep func(ctx context.Context, d time.Duration) error
	// mergeBudget overrides mergePollBudget; tests set it. Zero uses the default.
	mergeBudget time.Duration
}

// NewProvider creates a Bitbucket Cloud SCM provider. Credentials are resolved
// lazily on each request, so construction never fails for missing credentials
// and never touches the network.
func NewProvider(opts ProviderOptions) (*Provider, error) {
	creds := opts.Credentials
	if creds == nil {
		creds = DefaultCredentialSource()
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	c, err := newClient(clientOptions{
		httpClient: opts.HTTPClient,
		creds:      creds,
		apiBase:    opts.APIBase,
		userAgent:  opts.UserAgent,
		logger:     logger,
	})
	if err != nil {
		return nil, err
	}
	return &Provider{client: c, creds: creds}, nil
}

// SCMCredentialsAvailable reports whether usable Bitbucket credentials exist.
// Missing credentials are not an error: it returns false, nil so the observer
// can stay disabled. Any other source failure is propagated.
func (p *Provider) SCMCredentialsAvailable(ctx context.Context) (bool, error) {
	_, err := p.creds.Credentials(ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNoCredentials) {
		return false, nil
	}
	return false, err
}
