package daemon

import (
	"errors"
	"log/slog"
	"testing"

	scmbitbucket "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/bitbucket"
)

// clearBitbucketEnv blanks every credential env var the Bitbucket chain reads.
func clearBitbucketEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AO_BITBUCKET_USERNAME", "AO_BITBUCKET_API_TOKEN",
		"BITBUCKET_USERNAME", "BITBUCKET_API_TOKEN",
	} {
		t.Setenv(name, "")
	}
}

func TestSCMWiring_BitbucketRegisteredWhenCredentialsSet(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("AO_BITBUCKET_USERNAME", "u")
	t.Setenv("AO_BITBUCKET_API_TOKEN", "t")

	multi := newMultiSCMProvider(testGitLabConfig(), slog.Default())
	if multi == nil {
		t.Fatal("newMultiSCMProvider = nil, want bitbucket-backed provider")
	}
	repo, ok := multi.ParseRepository("git@bitbucket.org:dealhubio/aura-sre.git")
	if !ok {
		t.Fatal("ParseRepository did not recognise a bitbucket remote")
	}
	if repo.Provider != "bitbucket" {
		t.Fatalf("Provider = %q, want bitbucket", repo.Provider)
	}
}

func TestSCMWiring_BitbucketMergerRegisteredWhenCredentialsSet(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("BITBUCKET_USERNAME", "u")
	t.Setenv("BITBUCKET_API_TOKEN", "t")

	if merger := newMultiSCMMerger(testGitLabConfig(), slog.Default()); merger == nil {
		t.Fatal("newMultiSCMMerger = nil, want bitbucket-backed merger")
	}
}

func TestSCMWiring_BitbucketSkippedWithoutCredentials(t *testing.T) {
	clearBitbucketEnv(t)

	p, err := newBitbucketSCMProvider(slog.Default())
	if !errors.Is(err, scmbitbucket.ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	if p != nil {
		t.Fatalf("provider = %v, want nil", p)
	}
}

func TestSCMWiring_BitbucketNotRegisteredWithoutCredentials(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("AO_GITHUB_TOKEN", "")
	t.Setenv("AO_GITLAB_TOKEN", "")

	multi := newMultiSCMProvider(testGitLabConfig(), slog.Default())
	if multi == nil {
		return // upstream behavior: nothing usable
	}
	repo, ok := multi.ParseRepository("git@bitbucket.org:dealhubio/aura-sre.git")
	if ok && repo.Provider == "bitbucket" {
		t.Fatal("bitbucket registered without credentials")
	}
}
