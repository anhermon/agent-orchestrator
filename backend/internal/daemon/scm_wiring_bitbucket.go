package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	scmbitbucket "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/bitbucket"
)

// bitbucketCredentialProbeTimeout bounds the startup credential check, which
// may shell out to the macOS Keychain.
const bitbucketCredentialProbeTimeout = 10 * time.Second

// errBitbucketIncompleteCredentials means some Bitbucket credential env var is
// set but no complete username and token pair could be resolved. Unlike
// scmbitbucket.ErrNoCredentials (not configured, expected for most users) it
// signals a broken setup.
var errBitbucketIncompleteCredentials = errors.New("bitbucket scm: credentials incomplete: a username or token is set but not both")

var bitbucketCredentialEnvVars = []string{
	"AO_BITBUCKET_USERNAME", "AO_BITBUCKET_API_TOKEN",
	"BITBUCKET_USERNAME", "BITBUCKET_API_TOKEN",
}

// bitbucketSCMProbe memoizes the credential probe so the keychain is consulted
// at most once per daemon process, however many construction sites ask.
type bitbucketSCMProbe struct {
	once     sync.Once
	provider *scmbitbucket.Provider
	err      error
}

var bitbucketProbe bitbucketSCMProbe

// newBitbucketSCMProvider returns the process-wide Bitbucket Cloud provider.
// The provider itself resolves credentials lazily and never fails for missing
// ones, so the first call probes the credential chain once and the result
// (provider or error) is reused. It returns ErrNoCredentials when nothing is
// configured, which keeps Bitbucket out of the multi-provider (and the
// observer disabled) for GitHub/GitLab-only users, and
// errBitbucketIncompleteCredentials for a half-set configuration.
func newBitbucketSCMProvider(logger *slog.Logger) (*scmbitbucket.Provider, error) {
	bitbucketProbe.once.Do(func() {
		bitbucketProbe.provider, bitbucketProbe.err = probeBitbucketSCMProvider(logger)
	})
	return bitbucketProbe.provider, bitbucketProbe.err
}

func probeBitbucketSCMProvider(logger *slog.Logger) (*scmbitbucket.Provider, error) {
	creds := scmbitbucket.FallbackCredentialSource{
		scmbitbucket.EnvCredentialSource{
			UserVars:  []string{"AO_BITBUCKET_USERNAME"},
			TokenVars: []string{"AO_BITBUCKET_API_TOKEN"},
		},
		scmbitbucket.EnvCredentialSource{
			UserVars:  []string{"BITBUCKET_USERNAME"},
			TokenVars: []string{"BITBUCKET_API_TOKEN"},
		},
		scmbitbucket.KeychainCredentialSource{Service: "ao-bitbucket"},
	}
	p, err := scmbitbucket.NewProvider(scmbitbucket.ProviderOptions{Credentials: creds, Logger: logger})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), bitbucketCredentialProbeTimeout)
	defer cancel()
	ok, err := p.SCMCredentialsAvailable(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		if bitbucketCredentialEnvPartiallySet() {
			return nil, errBitbucketIncompleteCredentials
		}
		return nil, scmbitbucket.ErrNoCredentials
	}
	return p, nil
}

func bitbucketCredentialEnvPartiallySet() bool {
	for _, name := range bitbucketCredentialEnvVars {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}
