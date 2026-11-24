package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	scmbitbucket "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/bitbucket"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

const bitbucketRemote = "git@bitbucket.org:dealhubio/aura-sre.git"

// clearBitbucketEnv blanks every credential env var the Bitbucket chain reads
// and resets the process-wide probe memo, before and after the test.
func clearBitbucketEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AO_BITBUCKET_USERNAME", "AO_BITBUCKET_API_TOKEN",
		"BITBUCKET_USERNAME", "BITBUCKET_API_TOKEN",
	} {
		t.Setenv(name, "")
	}
	bitbucketProbe = bitbucketSCMProbe{}
	t.Cleanup(func() { bitbucketProbe = bitbucketSCMProbe{} })
}

// isolateOtherSCMProviders makes GitHub and GitLab unavailable: no token env
// and an empty PATH so the gh and glab fallbacks cannot run.
func isolateOtherSCMProviders(t *testing.T) {
	t.Helper()
	t.Setenv("AO_GITHUB_TOKEN", "")
	t.Setenv("AO_GITLAB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
}

type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// recordingHandler collects log records so tests can assert on levels.
type recordingHandler struct {
	mu      *sync.Mutex
	records *[]logRecord
}

func newRecordingLogger() (*slog.Logger, func() []logRecord) {
	h := recordingHandler{mu: &sync.Mutex{}, records: &[]logRecord{}}
	return slog.New(h), func() []logRecord {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]logRecord(nil), *h.records...)
	}
}

func (recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, logRecord{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

func bitbucketRecords(records []logRecord) []logRecord {
	var out []logRecord
	for _, r := range records {
		if r.attrs["provider"] == "bitbucket" {
			out = append(out, r)
		}
	}
	return out
}

func TestSCMWiring_BitbucketRegisteredWhenCredentialsSet(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("AO_BITBUCKET_USERNAME", "u")
	t.Setenv("AO_BITBUCKET_API_TOKEN", "t")

	multi := newMultiSCMProvider(testGitLabConfig(), slog.Default())
	if multi == nil {
		t.Fatal("newMultiSCMProvider = nil, want bitbucket-backed provider")
	}
	repo, ok := multi.ParseRepository(bitbucketRemote)
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
	isolateOtherSCMProviders(t)

	// GitHub and GitLab construct lazily, so the multi provider is still
	// built; Bitbucket must simply not be among its routes.
	multi := newMultiSCMProvider(testGitLabConfig(), slog.Default())
	if multi == nil {
		t.Fatal("newMultiSCMProvider = nil, want the lazily built github/gitlab providers")
	}
	if repo, ok := multi.ParseRepository(bitbucketRemote); ok {
		t.Fatalf("bitbucket remote routed to %q without credentials", repo.Provider)
	}
}

func TestSCMWiring_BitbucketUsernameWithoutTokenIsPartialConfig(t *testing.T) {
	clearBitbucketEnv(t)
	isolateOtherSCMProviders(t)
	t.Setenv("AO_BITBUCKET_USERNAME", "u")

	p, err := newBitbucketSCMProvider(slog.Default())
	if err == nil || p != nil {
		t.Fatalf("provider, err = %v, %v; want nil provider and an error", p, err)
	}
	if errors.Is(err, scmbitbucket.ErrNoCredentials) {
		t.Fatalf("err = %v; partial config must be distinct from not configured", err)
	}
	multi := newMultiSCMProvider(testGitLabConfig(), slog.Default())
	if multi == nil {
		t.Fatal("newMultiSCMProvider = nil, want the lazily built github/gitlab providers")
	}
	if repo, ok := multi.ParseRepository(bitbucketRemote); ok {
		t.Fatalf("bitbucket registered with a username but no token (routed to %q)", repo.Provider)
	}
}

func TestSCMWiring_BitbucketProbedOncePerProcess(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("AO_BITBUCKET_USERNAME", "u")
	t.Setenv("AO_BITBUCKET_API_TOKEN", "t")

	first, err := newBitbucketSCMProvider(slog.Default())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Credentials vanish after the first probe; the memoized result stands.
	t.Setenv("AO_BITBUCKET_API_TOKEN", "")
	second, err := newBitbucketSCMProvider(slog.Default())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatal("probe ran again; want one shared provider per process")
	}
}

func TestSCMWiring_LogSCMProviderDisabledLevels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want slog.Level
	}{
		{"not configured", scmbitbucket.ErrNoCredentials, slog.LevelDebug},
		{"partial config", errBitbucketIncompleteCredentials, slog.LevelWarn},
		{"auth failure", scmbitbucket.ErrAuthFailed, slog.LevelWarn},
		{"other failure", errors.New("keychain exploded"), slog.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, records := newRecordingLogger()
			logSCMProviderDisabled(logger, "bitbucket", tc.err)
			got := records()
			if len(got) != 1 || got[0].level != tc.want {
				t.Fatalf("records = %+v, want exactly one at %v", got, tc.want)
			}
		})
	}
}

func TestSCMWiring_GitHubRemoteStillRoutesToGitHubWithAllProviders(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("AO_BITBUCKET_USERNAME", "u")
	t.Setenv("AO_BITBUCKET_API_TOKEN", "t")
	t.Setenv("AO_GITHUB_TOKEN", "gh-token")
	t.Setenv("AO_GITLAB_TOKEN", "gl-token")

	multi := newMultiSCMProvider(testGitLabConfig(), slog.Default())
	if multi == nil {
		t.Fatal("newMultiSCMProvider = nil")
	}
	for remote, want := range map[string]string{
		"git@github.com:dealhubio/aura-sre.git": "github",
		"https://gitlab.com/dealhubio/aura-sre": "gitlab",
		bitbucketRemote:                         "bitbucket",
	} {
		repo, ok := multi.ParseRepository(remote)
		if !ok || repo.Provider != want {
			t.Fatalf("ParseRepository(%q) = %+v, %v; want provider %q", remote, repo, ok, want)
		}
	}
}

func TestSCMWiring_StartSCMObserverBitbucketOnly(t *testing.T) {
	clearBitbucketEnv(t)
	isolateOtherSCMProviders(t)
	t.Setenv("AO_BITBUCKET_USERNAME", "u")
	t.Setenv("AO_BITBUCKET_API_TOKEN", "t")

	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger, records := newRecordingLogger()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the observer must start and stop without polling
	done := startSCMObserver(ctx, store, (*lifecycle.Manager)(nil), nil, testGitLabConfig(), &scmReviewRefresher{}, logger)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("observer did not stop after context cancellation")
	}

	for _, r := range records() {
		if r.msg == "scm observer disabled: no usable SCM provider" {
			t.Fatal("observer disabled although bitbucket has credentials")
		}
		if r.attrs["provider"] == "bitbucket" {
			t.Fatalf("unexpected bitbucket disabled log: %+v", r)
		}
	}
}

func TestSCMWiring_StartSCMObserverLogsBitbucketNotConfiguredAtDebug(t *testing.T) {
	clearBitbucketEnv(t)
	isolateOtherSCMProviders(t)

	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger, records := newRecordingLogger()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // GitHub/GitLab build lazily, so the observer starts; stop it at once
	done := startSCMObserver(ctx, store, (*lifecycle.Manager)(nil), nil, testGitLabConfig(), &scmReviewRefresher{}, logger)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("observer did not stop after context cancellation")
	}

	got := bitbucketRecords(records())
	if len(got) != 1 || got[0].level != slog.LevelDebug {
		t.Fatalf("bitbucket records = %+v, want exactly one Debug", got)
	}
}
