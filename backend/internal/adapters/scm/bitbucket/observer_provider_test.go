package bitbucket

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ scmobserve.Provider = (*Provider)(nil)

const repoPath = "/2.0/repositories/acme/repo"

var testRepo = ports.SCMRepo{Provider: "bitbucket", Host: "bitbucket.org", Owner: "acme", Name: "repo", Repo: "acme/repo"}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// route is one fake endpoint: a fixture body or a status code.
type route struct {
	status int
	body   string
	// header is set on the response when non-empty.
	header map[string]string
}

// fakeBitbucket serves routes keyed by request path and records every request.
type fakeBitbucket struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]route
	reqs   []*http.Request
	srv    *httptest.Server
}

func newFake(t *testing.T, routes map[string]route) (*fakeBitbucket, *Provider) {
	return newFakeWithLogger(t, routes, nil)
}

func newFakeWithLogger(t *testing.T, routes map[string]route, logger *slog.Logger) (*fakeBitbucket, *Provider) {
	t.Helper()
	f := &fakeBitbucket{t: t, routes: routes}
	f.srv = newTestServer(t, f)
	p, err := NewProvider(ProviderOptions{
		Credentials: StaticCredentialSource{Username: testUser, Token: testToken},
		APIBase:     f.srv.URL + "/2.0",
		Logger:      logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}

func (f *fakeBitbucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	rt, ok := f.routes[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	for k, v := range rt.header {
		w.Header().Set(k, v)
	}
	if rt.status != 0 {
		w.WriteHeader(rt.status)
	}
	w.Write([]byte(strings.ReplaceAll(rt.body, "__NEXT__", f.srv.URL+r.URL.Path+"?page=2")))
}

// next returns the first recorded request for path.
func (f *fakeBitbucket) request(path string) *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.URL.Path == path {
			return r
		}
	}
	return nil
}

func ok(body string) route { return route{body: body} }

func TestRepoPRListGuardSyntheticETag(t *testing.T) {
	f, p := newFake(t, map[string]route{
		repoPath + "/pullrequests": ok(`{"values":[{"id":7,"updated_on":"2026-11-02T12:00:00.123456+00:00"}]}`),
	})
	res, err := p.RepoPRListGuard(context.Background(), testRepo, "")
	if err != nil {
		t.Fatal(err)
	}
	const want = "bb:2026-11-02T12:00:00.123456+00:00:7"
	if res.ETag != want || res.NotModified {
		t.Fatalf("first guard = %+v, want modified with etag %q", res, want)
	}
	q := f.request(repoPath + "/pullrequests").URL.Query()
	if got := q["state"]; strings.Join(got, ",") != "OPEN,MERGED,DECLINED,SUPERSEDED" {
		t.Fatalf("state params = %v", got)
	}
	if q.Get("sort") != "-updated_on" || q.Get("pagelen") != "1" || q.Get("fields") != "values.updated_on,values.id" {
		t.Fatalf("query = %v", q)
	}
	// Auth is attached.
	if u, pw, _ := f.request(repoPath + "/pullrequests").BasicAuth(); u != testUser || pw != testToken {
		t.Fatal("basic auth not sent")
	}

	res, err = p.RepoPRListGuard(context.Background(), testRepo, want)
	if err != nil || !res.NotModified || res.ETag != want {
		t.Fatalf("unchanged guard = %+v, %v; want NotModified", res, err)
	}
}

func TestRepoPRListGuardChangedAndEmpty(t *testing.T) {
	_, p := newFake(t, map[string]route{
		repoPath + "/pullrequests": ok(`{"values":[{"id":8,"updated_on":"2026-11-03T00:00:00.000000+00:00"}]}`),
	})
	res, err := p.RepoPRListGuard(context.Background(), testRepo, "bb:2026-11-02T12:00:00.123456+00:00:7")
	if err != nil || res.NotModified || res.ETag != "bb:2026-11-03T00:00:00.000000+00:00:8" {
		t.Fatalf("changed guard = %+v, %v", res, err)
	}

	_, empty := newFake(t, map[string]route{repoPath + "/pullrequests": ok(`{"values":[]}`)})
	res, err = empty.RepoPRListGuard(context.Background(), testRepo, "bb:2026-11-02T12:00:00.123456+00:00:7")
	if err != nil || res.NotModified {
		t.Fatalf("empty list vs non-empty etag = %+v, %v; want modified", res, err)
	}
}

func TestListPRsByRepoPaginatesAllStates(t *testing.T) {
	// The second page is selected by the page=2 query on the same path, so use a
	// handler instead of the static route map.
	f := &fakeBitbucket{t: t}
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(context.Background()))
		f.mu.Unlock()
		if r.URL.Query().Get("page") == "2" {
			w.Write([]byte(fixture(t, "pr_list_page2.json")))
			return
		}
		w.Write([]byte(strings.ReplaceAll(fixture(t, "pr_list_page1.json"), "__NEXT__", "http://"+r.Host+r.URL.Path+"?page=2")))
	}))
	p, err := NewProvider(ProviderOptions{Credentials: StaticCredentialSource{Username: testUser, Token: testToken}, APIBase: srv.URL + "/2.0"})
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got, err := p.ListPRsByRepo(context.Background(), testRepo, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d PRs, want 4 across two pages", len(got))
	}
	wantStates := map[int]string{7: "open", 6: "merged", 5: "closed", 4: "closed"}
	for _, pr := range got {
		if pr.State != wantStates[pr.Number] {
			t.Errorf("PR %d state = %q, want %q", pr.Number, pr.State, wantStates[pr.Number])
		}
	}
	first := f.reqs[0].URL.Query()
	if first.Get("q") != `updated_on > "2026-11-01T00:00:00Z"` {
		t.Fatalf("q = %q", first.Get("q"))
	}
	if len(first["state"]) != 4 {
		t.Fatalf("state params = %v, want all four", first["state"])
	}
	pr7 := got[0]
	if pr7.URL != "https://bitbucket.org/acme/repo/pull-requests/7" || pr7.SourceBranch != "feature/x" || pr7.TargetBranch != "main" ||
		pr7.HeadSHA != strings.Repeat("a", 40) || pr7.Author != "ann" || pr7.BaseRepo != "acme/repo" || pr7.HeadRepo != "acme/repo" || pr7.HeadFromFork {
		t.Fatalf("pr7 = %+v", pr7)
	}
	if pr7.ProviderID != "r-1#7" || pr7.UpdatedAtProvider.IsZero() {
		t.Fatalf("pr7 id/updated = %q %v", pr7.ProviderID, pr7.UpdatedAtProvider)
	}
	if !got[1].Merged || !got[2].Closed || got[2].Merged {
		t.Fatalf("merged/closed flags wrong: %+v %+v", got[1], got[2])
	}
}

func TestListPRsByRepoZeroCursorOmitsQuery(t *testing.T) {
	f, p := newFake(t, map[string]route{repoPath + "/pullrequests": ok(`{"values":[]}`)})
	if _, err := p.ListPRsByRepo(context.Background(), testRepo, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if q := f.request(repoPath + "/pullrequests").URL.Query(); q.Has("q") {
		t.Fatalf("zero cursor sent q=%q", q.Get("q"))
	}
}

func TestListPRsByRepoNotFound(t *testing.T) {
	_, p := newFake(t, map[string]route{})
	_, err := p.ListPRsByRepo(context.Background(), testRepo, time.Time{})
	if !errors.Is(err, ports.ErrSCMNotFound) {
		t.Fatalf("err = %v, want ErrSCMNotFound", err)
	}
}

func TestCommitChecksGuardHashesBody(t *testing.T) {
	sha := strings.Repeat("a", 40)
	f, p := newFake(t, map[string]route{repoPath + "/commit/" + sha + "/statuses": ok(fixture(t, "statuses_guard.json"))})
	res, err := p.CommitChecksGuard(context.Background(), testRepo, sha, "")
	if err != nil || res.NotModified || len(res.ETag) != 64 {
		t.Fatalf("guard = %+v, %v", res, err)
	}
	q := f.request(repoPath + "/commit/" + sha + "/statuses").URL.Query()
	if q.Get("sort") != "-updated_on" || q.Get("pagelen") != "1" {
		t.Fatalf("query = %v", q)
	}
	again, err := p.CommitChecksGuard(context.Background(), testRepo, sha, res.ETag)
	if err != nil || !again.NotModified || again.ETag != res.ETag {
		t.Fatalf("second guard = %+v, %v; want NotModified", again, err)
	}
}

func TestCommitChecksGuardForbiddenDegradesAndWarnsOnce(t *testing.T) {
	sha := strings.Repeat("a", 40)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, p := newFakeWithLogger(t, map[string]route{
		repoPath + "/commit/" + sha + "/statuses": {status: http.StatusForbidden, body: `{"error":{"message":"no"}}`},
	}, logger)
	for i := 0; i < 2; i++ {
		res, err := p.CommitChecksGuard(context.Background(), testRepo, sha, "prev")
		if err != nil {
			t.Fatalf("call %d: err = %v, want degrade without error", i, err)
		}
		if !res.NotModified || res.ETag != "prev" {
			t.Fatalf("call %d: res = %+v, want NotModified with the previous etag", i, res)
		}
	}
	warns := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "level=WARN") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("got %d WARN records, want exactly 1:\n%s", warns, buf.String())
	}
	if strings.Contains(buf.String(), testToken) {
		t.Fatal("log leaked the token")
	}
}

func TestCommitChecksGuardThrottled403PropagatesWithoutScopeWarn(t *testing.T) {
	sha := strings.Repeat("a", 40)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, p := newFakeWithLogger(t, map[string]route{
		repoPath + "/commit/" + sha + "/statuses": {status: http.StatusForbidden, body: `{}`, header: map[string]string{"Retry-After": "30"}},
	}, logger)
	_, err := p.CommitChecksGuard(context.Background(), testRepo, sha, "prev")
	var rl interface{ GetRetryAfter() time.Duration }
	if !errors.As(err, &rl) || rl.GetRetryAfter() != 30*time.Second {
		t.Fatalf("err = %v, want a rate-limit error with Retry-After 30s", err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("a throttle was logged as a token-scope problem:\n%s", buf.String())
	}
}

func TestFetchCIThrottled403Propagates(t *testing.T) {
	sha := strings.Repeat("a", 40)
	_, p := newFake(t, map[string]route{
		repoPath + "/commit/" + sha + "/statuses": {status: http.StatusForbidden, body: `{}`, header: map[string]string{"Retry-After": "30"}},
	})
	_, err := p.fetchCI(context.Background(), testRepo, sha)
	var rl interface{ GetRetryAfter() time.Duration }
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want a rate-limit error", err)
	}
}

func TestCommitChecksGuardEmptySHA(t *testing.T) {
	_, p := newFake(t, nil)
	if _, err := p.CommitChecksGuard(context.Background(), testRepo, "", ""); !errors.Is(err, ports.ErrSCMNotFound) {
		t.Fatalf("err = %v, want ErrSCMNotFound", err)
	}
}

func prRoutes(t *testing.T, id string, detail, diffstat string) map[string]route {
	sha := strings.Repeat("a", 40)
	return map[string]route{
		repoPath + "/pullrequests/" + id:               ok(fixture(t, detail)),
		repoPath + "/pullrequests/" + id + "/diffstat": ok(fixture(t, diffstat)),
		repoPath + "/commit/" + sha + "/statuses":      ok(fixture(t, "statuses_mixed.json")),
	}
}

func TestFetchPullRequestsAlignedWithMissing(t *testing.T) {
	routes := prRoutes(t, "7", "pr_detail_open_approved.json", "diffstat_clean.json")
	for k, v := range prRoutes(t, "6", "pr_detail_merged.json", "diffstat_clean.json") {
		routes[k] = v
	}
	_, p := newFake(t, routes)
	refs := []ports.SCMPRRef{
		{Repo: testRepo, Number: 7, URL: "https://bitbucket.org/acme/repo/pull-requests/7"},
		{Repo: testRepo, Number: 404, URL: "https://bitbucket.org/acme/repo/pull-requests/404"},
		{Repo: testRepo, Number: 6},
	}
	got, err := p.FetchPullRequests(context.Background(), refs)
	if err != nil {
		t.Fatalf("err = %v, want nil so the other observations survive", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	if !got[0].Fetched || got[0].PR.Number != 7 || !got[2].Fetched || got[2].PR.Number != 6 {
		t.Fatalf("results 0/2 misaligned: %+v / %+v", got[0].PR, got[2].PR)
	}
	mid := got[1]
	if mid.Fetched || !errors.Is(mid.Error, ports.ErrSCMNotFound) || mid.PR.Number != 404 || mid.Provider != "bitbucket" || mid.Repo != "acme/repo" {
		t.Fatalf("middle = %+v, want Fetched=false placeholder with ErrSCMNotFound", mid)
	}
	if !got[2].PR.Merged || got[2].PR.MergeCommitSHA != strings.Repeat("c", 40) {
		t.Fatalf("merged PR = %+v", got[2].PR)
	}
}

func TestFetchPullRequestsOpenApprovedWithCI(t *testing.T) {
	_, p := newFake(t, prRoutes(t, "7", "pr_detail_open_approved.json", "diffstat_clean.json"))
	got, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: testRepo, Number: 7}})
	if err != nil {
		t.Fatal(err)
	}
	o := got[0]
	if o.Review.Decision != string(domain.ReviewApproved) {
		t.Errorf("review decision = %q, want approved", o.Review.Decision)
	}
	if o.Mergeability.State != string(domain.MergeUnknown) || o.Mergeability.Conflict {
		t.Errorf("mergeability = %+v, want unknown", o.Mergeability)
	}
	if o.PR.Additions != 13 || o.PR.Deletions != 1 || o.PR.ChangedFiles != 2 {
		t.Errorf("diffstat totals = +%d -%d files %d", o.PR.Additions, o.PR.Deletions, o.PR.ChangedFiles)
	}
	if o.CI.Summary != string(domain.CIFailing) || o.CI.HeadSHA != strings.Repeat("a", 40) {
		t.Errorf("CI = %+v, want failing", o.CI)
	}
	if len(o.CI.Checks) != 4 {
		t.Fatalf("checks = %d, want 4 (latest per key, stale step-2 dropped)", len(o.CI.Checks))
	}
	byName := map[string]ports.SCMCheckObservation{}
	for _, c := range o.CI.Checks {
		byName[c.Name] = c
	}
	for name, want := range map[string]domain.PRCheckStatus{
		"build":      domain.PRCheckPassed,
		"unit tests": domain.PRCheckFailed,
		"lint":       domain.PRCheckInProgress,
		"deploy":     domain.PRCheckCancelled,
	} {
		if got := byName[name].Status; got != string(want) {
			t.Errorf("check %q status = %q, want %q", name, got, want)
		}
	}
	if byName["unit tests"].Conclusion != "FAILED" || byName["unit tests"].ProviderID != "step-2" {
		t.Errorf("unit tests = %+v", byName["unit tests"])
	}
	if len(o.CI.FailedChecks) != 1 || o.CI.FailedChecks[0].Name != "unit tests" || o.CI.FailedFingerprint == "" {
		t.Errorf("failed checks = %+v fp=%q", o.CI.FailedChecks, o.CI.FailedFingerprint)
	}
}

func TestFetchPullRequestsMergeConflictAndChangesRequested(t *testing.T) {
	_, p := newFake(t, prRoutes(t, "8", "pr_detail_changes_requested.json", "diffstat_conflict.json"))
	got, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: testRepo, Number: 8}})
	if err != nil {
		t.Fatal(err)
	}
	o := got[0]
	if o.Mergeability.State != string(domain.MergeConflicting) || !o.Mergeability.Conflict {
		t.Errorf("mergeability = %+v, want conflicting", o.Mergeability)
	}
	if o.Review.Decision != string(domain.ReviewChangesRequest) {
		t.Errorf("review decision = %q, want changes_requested", o.Review.Decision)
	}
}

func TestFetchPullRequestsStatuses403DegradesToUnknownCI(t *testing.T) {
	routes := prRoutes(t, "7", "pr_detail_open_approved.json", "diffstat_clean.json")
	routes[repoPath+"/commit/"+strings.Repeat("a", 40)+"/statuses"] = route{status: http.StatusForbidden}
	_, p := newFake(t, routes)
	got, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: testRepo, Number: 7}})
	if err != nil || !got[0].Fetched {
		t.Fatalf("got %+v, %v; a 403 on statuses must not fail the PR", got[0], err)
	}
	if got[0].CI.Summary != string(domain.CIUnknown) {
		t.Errorf("CI summary = %q, want unknown", got[0].CI.Summary)
	}
}

func TestFetchPullRequestsTransientErrorIsPerObservation(t *testing.T) {
	routes := prRoutes(t, "7", "pr_detail_open_approved.json", "diffstat_clean.json")
	routes[repoPath+"/pullrequests/9"] = route{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "30"}}
	_, p := newFake(t, routes)
	got, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: testRepo, Number: 9}, {Repo: testRepo, Number: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Fetched || got[0].Error == nil || errors.Is(got[0].Error, ports.ErrSCMNotFound) {
		t.Fatalf("rate-limited result = %+v", got[0])
	}
	var apiErr *APIError
	if !errors.As(got[0].Error, &apiErr) || apiErr.RetryAfter != 30*time.Second {
		t.Fatalf("error = %v, want APIError carrying Retry-After", got[0].Error)
	}
	if !got[1].Fetched {
		t.Fatal("second ref should still be fetched")
	}
}

func TestFetchFailedCheckLogTail(t *testing.T) {
	f, p := newFake(t, map[string]route{
		repoPath + "/pipelines/{pl-1}/steps/":            ok(fixture(t, "pipeline_steps.json")),
		repoPath + "/pipelines/{pl-1}/steps/{s-bad}/log": ok(fixture(t, "step_log.txt")),
	})
	check := ports.SCMCheckObservation{
		Name: "unit tests", ProviderID: "step-2",
		URL: "https://bitbucket.org/acme/repo/addon/pipelines/home#!/results/{pl-1}",
	}
	got, err := p.FetchFailedCheckLogTail(context.Background(), testRepo, check)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 200 || lines[0] != "log line 101" || lines[199] != "log line 300" {
		t.Fatalf("tail has %d lines, first %q last %q", len(lines), lines[0], lines[len(lines)-1])
	}
	if r := f.request(repoPath + "/pipelines/{pl-1}/steps/{s-bad}/log"); r == nil || r.Header.Get("Range") != "bytes=-65536" {
		t.Fatalf("log request missing Range header: %+v", r)
	}
}

func TestFetchFailedCheckLogTailNonPipelines(t *testing.T) {
	f, p := newFake(t, nil)
	check := ports.SCMCheckObservation{Name: "jenkins", URL: "https://ci.example.com/job/42"}
	got, err := p.FetchFailedCheckLogTail(context.Background(), testRepo, check)
	if got != "" || !errors.Is(err, ports.ErrSCMUnsupported) || !strings.Contains(err.Error(), "https://ci.example.com/job/42") {
		t.Fatalf("got %q, %v; want empty and ErrSCMUnsupported naming the URL", got, err)
	}
	if len(f.reqs) != 0 {
		t.Fatal("non-pipelines CI must not hit the API")
	}
}

func TestFetchReviewThreads(t *testing.T) {
	routes := map[string]route{
		repoPath + "/pullrequests/7": ok(fixture(t, "pr_detail_open_approved.json")),
	}
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case repoPath + "/pullrequests/7/comments":
			if r.URL.Query().Get("page") == "2" {
				w.Write([]byte(fixture(t, "comments_threads_page2.json")))
				return
			}
			w.Write([]byte(strings.ReplaceAll(fixture(t, "comments_threads_page1.json"), "__NEXT__", "http://"+r.Host+r.URL.Path+"?page=2")))
		case repoPath + "/pullrequests/7":
			w.Write([]byte(routes[r.URL.Path].body))
		default:
			http.NotFound(w, r)
		}
	}))
	p, err := NewProvider(ProviderOptions{Credentials: StaticCredentialSource{Username: testUser, Token: testToken}, APIBase: srv.URL + "/2.0"})
	if err != nil {
		t.Fatal(err)
	}
	ref := ports.SCMPRRef{Repo: testRepo, Number: 7, URL: "https://bitbucket.org/acme/repo/pull-requests/7"}
	got, err := p.FetchReviewThreads(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Threads) != 3 {
		t.Fatalf("threads = %d, want 3 (inline, general, resolved; deleted skipped)", len(got.Threads))
	}
	byID := map[string]ports.SCMReviewThreadObservation{}
	for _, th := range got.Threads {
		byID[th.ID] = th
	}
	inline := byID["1"]
	if inline.Path != "src/a.go" || inline.Line != 12 || inline.Resolved || len(inline.Comments) != 2 {
		t.Fatalf("inline thread = %+v", inline)
	}
	if inline.Comments[0].Body != "inline root" || inline.Comments[1].Body != "reply to inline" || !inline.Comments[1].IsBot {
		t.Fatalf("inline comments = %+v", inline.Comments)
	}
	if inline.Comments[0].Author != "bob" || inline.Comments[0].URL == "" || inline.Comments[0].CreatedAt.IsZero() {
		t.Fatalf("root comment = %+v", inline.Comments[0])
	}
	general := byID["2"]
	if general.Path != "" || general.Line != 0 || len(general.Comments) != 2 || general.Resolved {
		t.Fatalf("general thread = %+v", general)
	}
	resolved := byID["4"]
	if !resolved.Resolved || resolved.Path != "src/b.go" || resolved.Line != 7 {
		t.Fatalf("resolved thread = %+v", resolved)
	}
	if _, ok := byID["5"]; ok {
		t.Fatal("deleted comment became a thread")
	}
	if got.Decision != string(domain.ReviewApproved) || len(got.Reviews) != 1 || got.Reviews[0].Author != "bob" {
		t.Fatalf("decision/reviews = %q %+v", got.Decision, got.Reviews)
	}
}

func TestCheckStateMapping(t *testing.T) {
	for in, want := range map[string]domain.PRCheckStatus{
		"SUCCESSFUL": domain.PRCheckPassed,
		"FAILED":     domain.PRCheckFailed,
		"INPROGRESS": domain.PRCheckInProgress,
		"STOPPED":    domain.PRCheckCancelled,
		"WHATEVER":   domain.PRCheckUnknown,
	} {
		if got := checkStatus(in); got != want {
			t.Errorf("checkStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func providerFor(t *testing.T, h http.HandlerFunc) *Provider {
	t.Helper()
	srv := newTestServer(t, h)
	p, err := NewProvider(ProviderOptions{Credentials: StaticCredentialSource{Username: testUser, Token: testToken}, APIBase: srv.URL + "/2.0"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type retryHinter interface{ GetRetryAfter() time.Duration }

func TestOnlyThrottleErrorsSatisfyRateLimitContract(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		header   map[string]string
		throttle bool
	}{
		{"404", 404, nil, false},
		{"500", 500, nil, false},
		{"400", 400, nil, false},
		{"401", 401, nil, false},
		{"403 plain", 403, nil, false},
		{"429", 429, map[string]string{"Retry-After": "5"}, true},
		{"429 without header", 429, nil, true},
		{"403 with Retry-After", 403, map[string]string{"Retry-After": "5"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, p := newFake(t, map[string]route{repoPath + "/pullrequests": {status: c.status, header: c.header, body: "x"}})
			_, err := p.ListPRsByRepo(context.Background(), testRepo, time.Time{})
			var rl retryHinter
			if got := errors.As(err, &rl); got != c.throttle {
				t.Fatalf("errors.As(rateLimited) = %v, want %v (err=%v)", got, c.throttle, err)
			}
		})
	}
}

func TestFetchPullRequestsDiffstatFailureDegrades(t *testing.T) {
	routes := prRoutes(t, "7", "pr_detail_open_approved.json", "diffstat_clean.json")
	routes[repoPath+"/pullrequests/7/diffstat"] = route{status: http.StatusInternalServerError, body: "boom"}
	_, p := newFake(t, routes)
	got, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: testRepo, Number: 7}})
	if err != nil || !got[0].Fetched {
		t.Fatalf("got %+v, %v; a diffstat failure must not fail the PR", got[0], err)
	}
	o := got[0]
	if o.PR.Additions != 0 || o.PR.ChangedFiles != 0 || o.Mergeability.State != string(domain.MergeUnknown) {
		t.Errorf("diffstat not left unknown: %+v %+v", o.PR, o.Mergeability)
	}
}

func TestFetchPullRequestsDiffstatThrottlePropagates(t *testing.T) {
	routes := prRoutes(t, "7", "pr_detail_open_approved.json", "diffstat_clean.json")
	routes[repoPath+"/pullrequests/7/diffstat"] = route{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "9"}}
	_, p := newFake(t, routes)
	got, _ := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: testRepo, Number: 7}})
	var rl retryHinter
	if got[0].Fetched || !errors.As(got[0].Error, &rl) {
		t.Fatalf("got %+v; a throttled diffstat must surface for cooldown", got[0])
	}
}

func TestFetchPullRequestsSkipsDiffstatForNonOpen(t *testing.T) {
	routes := prRoutes(t, "6", "pr_detail_merged.json", "diffstat_clean.json")
	routes[repoPath+"/pullrequests/6/diffstat"] = route{status: http.StatusInternalServerError}
	f, p := newFake(t, routes)
	got, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: testRepo, Number: 6}})
	if err != nil || !got[0].Fetched || !got[0].PR.Merged {
		t.Fatalf("got %+v, %v", got[0], err)
	}
	if f.request(repoPath+"/pullrequests/6/diffstat") != nil {
		t.Fatal("diffstat requested for a merged PR")
	}
}

func TestFetchFailedCheckLogTailPaginatesStepsAndAcceptsError(t *testing.T) {
	stepsPath := repoPath + "/pipelines/{pl-1}/steps/"
	var logPath string
	p := providerFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case stepsPath:
			if r.URL.Query().Get("page") == "2" {
				w.Write([]byte(`{"values":[{"uuid":"{s-err}","state":{"result":{"name":"ERROR"}}}]}`))
				return
			}
			w.Write([]byte(`{"values":[{"uuid":"{s-ok}","state":{"result":{"name":"SUCCESSFUL"}}}],"next":"http://` + r.Host + stepsPath + `?page=2"}`))
		default:
			logPath = r.URL.Path
			w.Write([]byte("a\nb\n"))
		}
	})
	check := ports.SCMCheckObservation{URL: "https://bitbucket.org/acme/repo/addon/pipelines/home#!/results/{pl-1}"}
	got, err := p.FetchFailedCheckLogTail(context.Background(), testRepo, check)
	if err != nil || got != "a\nb" {
		t.Fatalf("got %q, %v", got, err)
	}
	if logPath != stepsPath+"{s-err}/log" {
		t.Fatalf("log fetched from %q, want the ERROR step on page 2", logPath)
	}
}

func TestListPRsByRepoDedupesKeepingLatest(t *testing.T) {
	entry := func(id int, updated string) string {
		return fmt.Sprintf(`{"id":%d,"state":"OPEN","title":"t%s","updated_on":"%s"}`, id, updated[:10], updated)
	}
	p := providerFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.Write([]byte(`{"values":[` + entry(7, "2026-11-05T00:00:00.000000+00:00") + `,` + entry(3, "2026-11-01T00:00:00.000000+00:00") + `]}`))
			return
		}
		w.Write([]byte(`{"values":[` + entry(7, "2026-11-02T00:00:00.000000+00:00") + `,` + entry(5, "2026-11-04T00:00:00.000000+00:00") + `],"next":"http://` + r.Host + r.URL.Path + `?page=2"}`))
	})
	got, err := p.ListPRsByRepo(context.Background(), testRepo, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Number != 7 || got[1].Number != 5 || got[2].Number != 3 {
		t.Fatalf("got %+v, want 7,5,3 once each", got)
	}
	if got[0].Title != "t2026-11-05" {
		t.Fatalf("PR 7 kept the stale entry: %q", got[0].Title)
	}
}

func TestCIObservationPicksLatestPerKeyRegardlessOfOrder(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 2, h, 0, 0, 0, time.UTC) }
	// Oldest first: a server ignoring sort must not make the stale FAILED win.
	ci := ciObservation("sha", []bbStatus{
		{Key: "k", Name: "unit", State: "FAILED", UpdatedOn: at(1)},
		{Key: "k", Name: "unit", State: "SUCCESSFUL", UpdatedOn: at(2)},
	})
	if ci.Summary != string(domain.CIPassing) || len(ci.Checks) != 1 || ci.Checks[0].Conclusion != "SUCCESSFUL" || len(ci.FailedChecks) != 0 {
		t.Fatalf("ci = %+v", ci)
	}
}

func TestFetchReviewThreadsRequestsCreatedOrderAndKeepsRepliesOfDeletedRoot(t *testing.T) {
	var sort string
	p := providerFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case repoPath + "/pullrequests/7/comments":
			sort = r.URL.Query().Get("sort")
			w.Write([]byte(`{"values":[
				{"id":1,"deleted":true,"content":{"raw":"gone"},"user":{"nickname":"bob"},"inline":{"path":"a.go","to":4}},
				{"id":2,"deleted":false,"content":{"raw":"still here"},"user":{"nickname":"ann"},"parent":{"id":1}},
				{"id":3,"deleted":false,"content":{"raw":"orphan"},"user":{"nickname":"ann"},"parent":{"id":99}}]}`))
		case repoPath + "/pullrequests/7":
			w.Write([]byte(fixture(t, "pr_detail_open_approved.json")))
		default:
			http.NotFound(w, r)
		}
	})
	got, err := p.FetchReviewThreads(context.Background(), ports.SCMPRRef{Repo: testRepo, Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	if sort != "created_on" {
		t.Fatalf("sort = %q, want created_on", sort)
	}
	if len(got.Threads) != 1 {
		t.Fatalf("threads = %+v, want only the deleted-root thread", got.Threads)
	}
	th := got.Threads[0]
	if th.ID != "1" || th.Path != "a.go" || th.Line != 4 || len(th.Comments) != 1 || th.Comments[0].Body != "still here" {
		t.Fatalf("thread = %+v", th)
	}
}

func TestFetchReviewThreadsPRNotFound(t *testing.T) {
	_, p := newFake(t, map[string]route{repoPath + "/pullrequests/7/comments": ok(`{"values":[]}`)})
	_, err := p.FetchReviewThreads(context.Background(), ports.SCMPRRef{Repo: testRepo, Number: 7})
	if !errors.Is(err, ports.ErrSCMNotFound) {
		t.Fatalf("err = %v, want ErrSCMNotFound", err)
	}
}
