package daemon

import (
	"context"
	"log/slog"
	"time"

	scmbitbucket "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/bitbucket"
)

// bitbucketCredentialProbeTimeout bounds the startup credential check, which
// may shell out to the macOS Keychain.
const bitbucketCredentialProbeTimeout = 10 * time.Second

// newBitbucketSCMProvider builds the Bitbucket Cloud provider. The provider
// itself resolves credentials lazily and never fails for missing ones, so this
// probes the credential chain once and returns ErrNoCredentials when nothing
// is configured; that keeps Bitbucket out of the multi-provider (and the
// observer disabled) for GitHub/GitLab-only users.
func newBitbucketSCMProvider(logger *slog.Logger) (*scmbitbucket.Provider, error) {
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
		return nil, scmbitbucket.ErrNoCredentials
	}
	return p, nil
}
