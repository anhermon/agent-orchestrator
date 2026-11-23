package bitbucket

import (
	"context"
	"errors"
	"os"
	"strings"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Credentials is a Bitbucket Cloud username and API token, sent as HTTP basic
// auth.
type Credentials struct {
	Username string
	Token    string
}

// CredentialSource yields Bitbucket credentials on demand. Production wires
// DefaultCredentialSource; tests inject StaticCredentialSource.
type CredentialSource interface {
	Credentials(ctx context.Context) (Credentials, error)
}

// ErrNoCredentials is returned when no source could yield a username and token.
var ErrNoCredentials = errors.New("bitbucket scm: no credentials configured")

// ErrAuthFailed is returned when Bitbucket rejects the supplied credentials
// (401).
var ErrAuthFailed = errors.New("bitbucket scm: authentication failed")

// Env var names, in precedence order (project-scoped AO_ names win).
var (
	defaultUserVars  = []string{"AO_BITBUCKET_USERNAME", "BITBUCKET_USERNAME"}
	defaultTokenVars = []string{"AO_BITBUCKET_API_TOKEN", "BITBUCKET_API_TOKEN"}
)

const defaultKeychainService = "ao-bitbucket"

// StaticCredentialSource is a literal username and token, typically used in
// tests.
type StaticCredentialSource struct {
	Username string
	Token    string
}

// Credentials returns the literal values, or ErrNoCredentials when either is
// empty.
func (s StaticCredentialSource) Credentials(context.Context) (Credentials, error) {
	u, t := strings.TrimSpace(s.Username), strings.TrimSpace(s.Token)
	if u == "" || t == "" {
		return Credentials{}, ErrNoCredentials
	}
	return Credentials{Username: u, Token: t}, nil
}

// EnvCredentialSource reads a username and token as a pair: UserVars[i] and
// TokenVars[i] belong together, and the first index where both are set wins.
// Order matters: list the AO_-prefixed variables first. A username from one
// pair is never combined with a token from another.
type EnvCredentialSource struct {
	UserVars  []string
	TokenVars []string
}

// DefaultEnvCredentialSource reads AO_BITBUCKET_* then BITBUCKET_* variables.
func DefaultEnvCredentialSource() EnvCredentialSource {
	return EnvCredentialSource{UserVars: defaultUserVars, TokenVars: defaultTokenVars}
}

// Credentials returns the env credentials, or ErrNoCredentials when the
// username or the token is missing.
func (s EnvCredentialSource) Credentials(context.Context) (Credentials, error) {
	for i := 0; i < len(s.UserVars) && i < len(s.TokenVars); i++ {
		u := strings.TrimSpace(os.Getenv(s.UserVars[i]))
		t := strings.TrimSpace(os.Getenv(s.TokenVars[i]))
		if u != "" && t != "" {
			return Credentials{Username: u, Token: t}, nil
		}
	}
	return Credentials{}, ErrNoCredentials
}

func firstEnv(names []string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// KeychainCredentialSource reads the token from a macOS Keychain generic
// password (service Service, account = username). The username comes from the
// AO_BITBUCKET_USERNAME / BITBUCKET_USERNAME env vars because the Keychain
// lookup is keyed by it.
type KeychainCredentialSource struct {
	Service string

	// run executes an external command; nil uses the real process runner.
	// Injectable so tests never touch the real Keychain.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Credentials looks the token up in the Keychain. Any lookup failure (not
// macOS, item missing, access denied) yields ErrNoCredentials so the source
// is silently skipped.
func (s KeychainCredentialSource) Credentials(ctx context.Context) (Credentials, error) {
	user := firstEnv(defaultUserVars)
	if user == "" {
		return Credentials{}, ErrNoCredentials
	}
	service := s.Service
	if service == "" {
		service = defaultKeychainService
	}
	run := s.run
	if run == nil {
		run = runCommand
	}
	out, err := run(ctx, "security", "find-generic-password", "-s", service, "-a", user, "-w")
	if err != nil {
		return Credentials{}, ErrNoCredentials
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return Credentials{}, ErrNoCredentials
	}
	return Credentials{Username: user, Token: token}, nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return aoprocess.CommandContext(ctx, name, args...).Output()
}

// FallbackCredentialSource tries each source in order and returns the first
// success.
type FallbackCredentialSource []CredentialSource

// Credentials returns the first successful result. ErrNoCredentials from a
// source moves on to the next; any other error is remembered and returned only
// if no later source succeeds.
func (s FallbackCredentialSource) Credentials(ctx context.Context) (Credentials, error) {
	var firstErr error
	for _, src := range s {
		if src == nil {
			continue
		}
		creds, err := src.Credentials(ctx)
		if err == nil {
			return creds, nil
		}
		if errors.Is(err, ErrNoCredentials) {
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return Credentials{}, firstErr
	}
	return Credentials{}, ErrNoCredentials
}

// DefaultCredentialSource is the production chain: env vars, then Keychain.
func DefaultCredentialSource() CredentialSource {
	return FallbackCredentialSource{
		DefaultEnvCredentialSource(),
		KeychainCredentialSource{Service: defaultKeychainService},
	}
}
