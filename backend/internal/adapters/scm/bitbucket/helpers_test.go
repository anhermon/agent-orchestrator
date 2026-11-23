package bitbucket

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

const (
	testUser  = "angel"
	testToken = "s3cr3t-token-value"
)

// newTestProvider builds a Provider with static credentials. A nil handler
// yields a provider on the default API base (no server); otherwise requests go
// to an httptest server running handler.
func newTestProvider(t *testing.T, handler http.Handler) *Provider {
	t.Helper()
	opts := ProviderOptions{
		Credentials: StaticCredentialSource{Username: testUser, Token: testToken},
		UserAgent:   "ao-test",
	}
	if handler != nil {
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		opts.APIBase = srv.URL + "/2.0"
	}
	p, err := NewProvider(opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
