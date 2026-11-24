package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	testHeadSHA  = "0123456789abcdef0123456789abcdef01234567"
	testStaleSHA = "ffffffffffffffffffffffffffffffffffffffff"
)

var testPR = ports.SCMPRRef{Repo: ports.SCMRepo{Owner: "acme", Name: "widgets"}, Number: 7}

type recordedCall struct {
	Method string
	Path   string
	Body   string
}

// actionsServer records every request and answers from routes keyed by
// "METHOD path"; an unknown route is a 404.
type actionsServer struct {
	mu     sync.Mutex
	calls  []recordedCall
	routes map[string]func(w http.ResponseWriter)
}

func (s *actionsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.calls = append(s.calls, recordedCall{r.Method, r.URL.Path, string(body)})
	s.mu.Unlock()
	if h, ok := s.routes[r.Method+" "+r.URL.Path]; ok {
		h(w)
		return
	}
	http.NotFound(w, r)
}

func (s *actionsServer) count(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.Method == method && c.Path == path {
			n++
		}
	}
	return n
}

func (s *actionsServer) last(method, path string) (recordedCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].Method == method && s.calls[i].Path == path {
			return s.calls[i], true
		}
	}
	return recordedCall{}, false
}

func writeJSON(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const (
	prPath = "/2.0/repositories/acme/widgets/pullrequests/7"
	prOpen = `{"id":7,"state":"OPEN","source":{"commit":{"hash":"0123456789ab"}}}`
)

func TestMergeHappyPathPostsOnce(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": writeJSON(200, `{"merge_commit":{"hash":"deadbeef"}}`),
	}}
	p := newTestProvider(t, srv)
	res, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA, Method: ports.SCMMergeSquash})
	if err != nil {
		t.Fatal(err)
	}
	if res.MergeCommitSHA != "deadbeef" {
		t.Fatalf("merge sha = %q", res.MergeCommitSHA)
	}
	if n := srv.count("POST", prPath+"/merge"); n != 1 {
		t.Fatalf("merge posts = %d", n)
	}
	call, _ := srv.last("POST", prPath+"/merge")
	var body map[string]any
	if err := json.Unmarshal([]byte(call.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["merge_strategy"] != "squash" {
		t.Fatalf("merge_strategy = %v", body["merge_strategy"])
	}
}

func TestMergeStrategyMapping(t *testing.T) {
	cases := map[ports.SCMMergeMethod]string{
		ports.SCMMergeSquash: "squash",
		ports.SCMMergeMerge:  "merge_commit",
		ports.SCMMergeRebase: "rebase_fast_forward",
	}
	for method, want := range cases {
		srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
			"GET " + prPath:             writeJSON(200, prOpen),
			"POST " + prPath + "/merge": writeJSON(200, `{"merge_commit":{"hash":"abc"}}`),
		}}
		p := newTestProvider(t, srv)
		if _, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA, Method: method}); err != nil {
			t.Fatal(err)
		}
		call, _ := srv.last("POST", prPath+"/merge")
		if !strings.Contains(call.Body, `"merge_strategy":"`+want+`"`) {
			t.Fatalf("method %s: body = %s, want strategy %s", method, call.Body, want)
		}
	}
}

func TestMergeOmitsStrategyWhenUnset(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": writeJSON(200, `{"merge_commit":{"hash":"abc"}}`),
	}}
	p := newTestProvider(t, srv)
	if _, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA}); err != nil {
		t.Fatal(err)
	}
	call, _ := srv.last("POST", prPath+"/merge")
	if strings.Contains(call.Body, "merge_strategy") {
		t.Fatalf("body = %s, want no merge_strategy", call.Body)
	}
}

func TestMergeStaleHeadDoesNotMerge(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": writeJSON(200, `{}`),
	}}
	p := newTestProvider(t, srv)
	_, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testStaleSHA})
	if !errors.Is(err, ports.ErrSCMHeadChanged) {
		t.Fatalf("err = %v, want ErrSCMHeadChanged", err)
	}
	if n := srv.count("POST", prPath+"/merge"); n != 0 {
		t.Fatalf("merge posts = %d, want 0", n)
	}
}

func TestMergeRequiresExpectedHead(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){}}
	p := newTestProvider(t, srv)
	if _, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR}); err == nil {
		t.Fatal("want error for empty expected head")
	}
	if len(srv.calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(srv.calls))
	}
}

func TestMergeNotOpenIsRejected(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, `{"id":7,"state":"MERGED","source":{"commit":{"hash":"0123456789ab"}}}`),
		"POST " + prPath + "/merge": writeJSON(200, `{}`),
	}}
	p := newTestProvider(t, srv)
	_, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA})
	if !errors.Is(err, ports.ErrSCMRejected) {
		t.Fatalf("err = %v, want ErrSCMRejected", err)
	}
	if n := srv.count("POST", prPath+"/merge"); n != 0 {
		t.Fatalf("merge posts = %d", n)
	}
}

func TestMergeConflictIsNotMergeable(t *testing.T) {
	for _, status := range []int{400, 409} {
		srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
			"GET " + prPath:             writeJSON(200, prOpen),
			"POST " + prPath + "/merge": writeJSON(status, `{"error":{"message":"You can't merge until you resolve all merge conflicts."}}`),
		}}
		p := newTestProvider(t, srv)
		_, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA})
		if !errors.Is(err, ports.ErrSCMNotMergeable) {
			t.Fatalf("status %d: err = %v, want ErrSCMNotMergeable", status, err)
		}
	}
}

func TestMergeOtherBadRequestIsRejected(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": writeJSON(400, `{"error":{"message":"invalid merge_strategy"}}`),
	}}
	p := newTestProvider(t, srv)
	_, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA})
	if !errors.Is(err, ports.ErrSCMRejected) {
		t.Fatalf("err = %v, want ErrSCMRejected", err)
	}
}

func TestMergeForbidden(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": writeJSON(403, `{"error":{"message":"nope"}}`),
	}}
	p := newTestProvider(t, srv)
	_, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA})
	if !errors.Is(err, ports.ErrSCMForbidden) {
		t.Fatalf("err = %v, want ErrSCMForbidden", err)
	}
}

func TestMergeNotFound(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){}}
	p := newTestProvider(t, srv)
	_, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA})
	if !errors.Is(err, ports.ErrSCMNotFound) {
		t.Fatalf("err = %v, want ErrSCMNotFound", err)
	}
}

func TestPostCommentTopLevel(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"POST " + prPath + "/comments": writeJSON(201, `{"id":55,"content":{"raw":"hello"},"user":{"display_name":"Angel","nickname":"angel"},"created_on":"2026-01-02T03:04:05.000000+00:00","links":{"html":{"href":"https://bitbucket.org/acme/widgets/pull-requests/7#comment-55"}}}`),
	}}
	p := newTestProvider(t, srv)
	res, err := p.PostComment(context.Background(), ports.SCMCommentRequest{PR: testPR, Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	call, _ := srv.last("POST", prPath+"/comments")
	if call.Body != `{"content":{"raw":"hello"}}` {
		t.Fatalf("body = %s", call.Body)
	}
	if res.ID != "55" || res.Body != "hello" || !strings.HasSuffix(res.URL, "#comment-55") {
		t.Fatalf("result = %+v", res)
	}
}

func TestPostCommentReplyCarriesParent(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"POST " + prPath + "/comments": writeJSON(201, `{"id":56,"content":{"raw":"re"}}`),
	}}
	p := newTestProvider(t, srv)
	if _, err := p.PostComment(context.Background(), ports.SCMCommentRequest{PR: testPR, Body: "re", ThreadID: "12"}); err != nil {
		t.Fatal(err)
	}
	call, _ := srv.last("POST", prPath+"/comments")
	if call.Body != `{"content":{"raw":"re"},"parent":{"id":12}}` {
		t.Fatalf("body = %s", call.Body)
	}
}

func TestPostCommentRejectsBadInput(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){}}
	p := newTestProvider(t, srv)
	if _, err := p.PostComment(context.Background(), ports.SCMCommentRequest{PR: testPR, Body: "  "}); err == nil {
		t.Fatal("want error for empty body")
	}
	if _, err := p.PostComment(context.Background(), ports.SCMCommentRequest{PR: testPR, Body: "x", ThreadID: "abc"}); err == nil {
		t.Fatal("want error for non-numeric thread id")
	}
	if len(srv.calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(srv.calls))
	}
}

func TestPostCommentForbidden(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"POST " + prPath + "/comments": writeJSON(403, `{}`),
	}}
	p := newTestProvider(t, srv)
	_, err := p.PostComment(context.Background(), ports.SCMCommentRequest{PR: testPR, Body: "x"})
	if !errors.Is(err, ports.ErrSCMForbidden) {
		t.Fatalf("err = %v, want ErrSCMForbidden", err)
	}
}

func TestApproveRequestChangesDecline(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"POST " + prPath + "/approve":         writeJSON(200, `{}`),
		"POST " + prPath + "/request-changes": writeJSON(200, `{}`),
		"POST " + prPath + "/decline":         writeJSON(200, `{}`),
	}}
	p := newTestProvider(t, srv)
	ctx := context.Background()
	if err := p.ApprovePullRequest(ctx, testPR); err != nil {
		t.Fatal(err)
	}
	if err := p.RequestPullRequestChanges(ctx, testPR); err != nil {
		t.Fatal(err)
	}
	if err := p.ChangePullRequestState(ctx, ports.SCMPRStateRequest{PR: testPR, Action: ports.SCMPRClose}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/approve", "/request-changes", "/decline"} {
		if n := srv.count("POST", prPath+suffix); n != 1 {
			t.Fatalf("%s hits = %d", suffix, n)
		}
	}
}

func TestChangeStateUnsupported(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){}}
	p := newTestProvider(t, srv)
	for _, a := range []ports.SCMPRStateAction{ports.SCMPRReopen, ports.SCMPRReady, ports.SCMPRDraft} {
		err := p.ChangePullRequestState(context.Background(), ports.SCMPRStateRequest{PR: testPR, Action: a})
		if !errors.Is(err, ports.ErrSCMUnsupported) {
			t.Fatalf("%s: err = %v, want ErrSCMUnsupported", a, err)
		}
	}
	if len(srv.calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(srv.calls))
	}
}

func TestDeclineForbiddenAndRejected(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"POST " + prPath + "/decline": writeJSON(403, `{}`),
	}}
	p := newTestProvider(t, srv)
	err := p.ChangePullRequestState(context.Background(), ports.SCMPRStateRequest{PR: testPR, Action: ports.SCMPRClose})
	if !errors.Is(err, ports.ErrSCMForbidden) {
		t.Fatalf("err = %v, want ErrSCMForbidden", err)
	}
	srv.routes["POST "+prPath+"/decline"] = writeJSON(400, `{"error":{"message":"already declined"}}`)
	err = p.ChangePullRequestState(context.Background(), ports.SCMPRStateRequest{PR: testPR, Action: ports.SCMPRClose})
	if !errors.Is(err, ports.ErrSCMRejected) {
		t.Fatalf("err = %v, want ErrSCMRejected", err)
	}
}

func TestResolveAndUnresolveThread(t *testing.T) {
	path := prPath + "/comments/12/resolve"
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"POST " + path:   writeJSON(200, `{}`),
		"DELETE " + path: writeJSON(204, ``),
	}}
	p := newTestProvider(t, srv)
	ctx := context.Background()
	req := ports.SCMReviewResolveRequest{PR: testPR, ThreadID: "12"}
	if err := p.ResolveReviewThread(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := p.UnresolveReviewThread(ctx, req); err != nil {
		t.Fatal(err)
	}
	if srv.count("POST", path) != 1 || srv.count("DELETE", path) != 1 {
		t.Fatalf("calls = %+v", srv.calls)
	}
	if err := p.ResolveReviewThread(ctx, ports.SCMReviewResolveRequest{PR: testPR, ThreadID: "x/../y"}); err == nil {
		t.Fatal("want error for non-numeric thread id")
	}
}

func TestUnsupportedOperations(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){}}
	p := newTestProvider(t, srv)
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["enqueue"] = p.EnqueuePullRequest(ctx, ports.SCMEnqueueRequest{PR: testPR})
	checks["dequeue"] = p.DequeuePullRequest(ctx, ports.SCMDequeueRequest{PR: testPR})
	checks["request review"] = p.RequestReview(ctx, ports.SCMReviewRequest{PR: testPR, Reviewer: "bob"})
	_, checks["attachment access"] = p.AttachmentAccess(ctx, testPR)
	_, checks["store attachments"] = p.StoreAttachments(ctx, ports.SCMAttachmentBatch{PR: testPR})
	checks["discard attachments"] = p.DiscardAttachments(ctx, testPR, "h")
	for name, err := range checks {
		if !errors.Is(err, ports.ErrSCMUnsupported) {
			t.Fatalf("%s: err = %v, want ErrSCMUnsupported", name, err)
		}
	}
	if len(srv.calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(srv.calls))
	}
}

// sequence serves responses in order, repeating the last one.
func sequence(responses ...func(http.ResponseWriter)) func(http.ResponseWriter) {
	var mu sync.Mutex
	i := 0
	return func(w http.ResponseWriter) {
		mu.Lock()
		h := responses[min(i, len(responses)-1)]
		i++
		mu.Unlock()
		h(w)
	}
}

func withHeader(key, value string, h func(http.ResponseWriter)) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set(key, value)
		h(w)
	}
}

const (
	taskPath   = "/2.0/repositories/acme/widgets/pullrequests/7/tasks/99"
	prMerged   = `{"id":7,"state":"MERGED","source":{"commit":{"hash":"0123456789ab"}},"merge_commit":{"hash":"cafe01"}}`
	prMergedNo = `{"id":7,"state":"MERGED","source":{"commit":{"hash":"0123456789ab"}}}`
)

// recordSleeps replaces the provider's poll sleep and returns the total slept.
func recordSleeps(p *Provider) *time.Duration {
	var total time.Duration
	p.sleep = func(ctx context.Context, d time.Duration) error {
		total += d
		return ctx.Err()
	}
	return &total
}

func mergeReq() ports.SCMMergeRequest {
	return ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA}
}

func TestMergeAsyncPollsTaskThenConfirmsMerged(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             sequence(writeJSON(200, prOpen), writeJSON(200, prMerged)),
		"POST " + prPath + "/merge": withHeader("Location", taskPath, writeJSON(202, ``)),
		"GET " + taskPath:           sequence(writeJSON(200, `{"task_status":"PENDING"}`), writeJSON(200, `{"task_status":"SUCCESS"}`)),
	}}
	p := newTestProvider(t, srv)
	recordSleeps(p)
	res, err := p.MergePullRequest(context.Background(), mergeReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.MergeCommitSHA != "cafe01" {
		t.Fatalf("sha = %q", res.MergeCommitSHA)
	}
	if srv.count("POST", prPath+"/merge") != 1 || srv.count("GET", taskPath) != 2 {
		t.Fatalf("calls = %+v", srv.calls)
	}
}

func TestMergeAsyncMergedWithoutHashIsSuccess(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             sequence(writeJSON(200, prOpen), writeJSON(200, prMergedNo)),
		"POST " + prPath + "/merge": withHeader("Location", taskPath, writeJSON(202, ``)),
		"GET " + taskPath:           writeJSON(200, `{"task_status":"SUCCESS"}`),
	}}
	p := newTestProvider(t, srv)
	recordSleeps(p)
	res, err := p.MergePullRequest(context.Background(), mergeReq())
	if err != nil || res.MergeCommitSHA != "" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestMergeAsyncStillPendingReturnsErrMergePending(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": withHeader("Location", taskPath, writeJSON(202, ``)),
		"GET " + taskPath:           writeJSON(200, `{"task_status":"PENDING"}`),
	}}
	p := newTestProvider(t, srv)
	slept := recordSleeps(p)
	_, err := p.MergePullRequest(context.Background(), mergeReq())
	if !errors.Is(err, ErrMergePending) {
		t.Fatalf("err = %v, want ErrMergePending", err)
	}
	if *slept > 10*time.Second || *slept == 0 {
		t.Fatalf("slept %v, want 0 < total <= 10s", *slept)
	}
}

func TestMergeAsyncNoLocationAndStillOpenIsPending(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": writeJSON(202, ``),
	}}
	p := newTestProvider(t, srv)
	recordSleeps(p)
	_, err := p.MergePullRequest(context.Background(), mergeReq())
	if !errors.Is(err, ErrMergePending) {
		t.Fatalf("err = %v, want ErrMergePending", err)
	}
}

func TestMergeAsyncTaskFailedIsError(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": withHeader("Location", taskPath, writeJSON(202, ``)),
		"GET " + taskPath:           writeJSON(200, `{"task_status":"FAILED"}`),
	}}
	p := newTestProvider(t, srv)
	recordSleeps(p)
	_, err := p.MergePullRequest(context.Background(), mergeReq())
	if err == nil || errors.Is(err, ErrMergePending) {
		t.Fatalf("err = %v, want non-pending error", err)
	}
}

func TestMergeAsyncHonoursContext(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": withHeader("Location", taskPath, writeJSON(202, ``)),
		"GET " + taskPath:           writeJSON(200, `{"task_status":"PENDING"}`),
	}}
	p := newTestProvider(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	p.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := p.MergePullRequest(ctx, mergeReq())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrMergePending) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestMergeAsyncRefusesTaskLocationOnOtherHost(t *testing.T) {
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits.Add(1)
	}))
	t.Cleanup(evil.Close)
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": withHeader("Location", evil.URL+"/task", writeJSON(202, ``)),
	}}
	p := newTestProvider(t, srv)
	recordSleeps(p)
	_, err := p.MergePullRequest(context.Background(), mergeReq())
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want refusal of the foreign task host", err)
	}
	if n := evilHits.Load(); n != 0 {
		t.Fatalf("foreign host received %d requests, want 0", n)
	}
}

func TestMergeAsyncPollBudgetBoundsHungTask(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": withHeader("Location", taskPath, writeJSON(202, ``)),
	}}
	hang := make(chan struct{})
	srv.routes["GET "+taskPath] = func(w http.ResponseWriter) { <-hang }
	p := newTestProvider(t, srv)
	// Registered after the server's cleanup so it runs first and frees the handler.
	t.Cleanup(func() { close(hang) })
	p.mergeBudget = 100 * time.Millisecond
	start := time.Now()
	_, err := p.MergePullRequest(context.Background(), mergeReq())
	if !errors.Is(err, ErrMergePending) {
		t.Fatalf("err = %v, want ErrMergePending", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v, want the poll bounded by the budget", elapsed)
	}
}

func TestMergeSyncWithoutHashReconfirms(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             sequence(writeJSON(200, prOpen), writeJSON(200, prMergedNo)),
		"POST " + prPath + "/merge": writeJSON(200, `{}`),
	}}
	p := newTestProvider(t, srv)
	res, err := p.MergePullRequest(context.Background(), mergeReq())
	if err != nil || res.MergeCommitSHA != "" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestMergeSyncWithoutHashStillOpenIsError(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": writeJSON(200, `{}`),
	}}
	p := newTestProvider(t, srv)
	_, err := p.MergePullRequest(context.Background(), mergeReq())
	if err == nil || errors.Is(err, ErrMergePending) {
		t.Fatalf("err = %v, want non-pending error", err)
	}
}

func TestHeadMatches(t *testing.T) {
	cases := []struct {
		name, live, expected string
		want                 bool
	}{
		{"full expected, 12-char live", "0123456789ab", testHeadSHA, true},
		{"7-char expected", testHeadSHA[:12], testHeadSHA[:7], true},
		{"uppercase expected", "0123456789ab", strings.ToUpper(testHeadSHA), true},
		{"uppercase live", "0123456789AB", testHeadSHA, true},
		{"different", "ffffffffffff", testHeadSHA, false},
		{"empty live", "", testHeadSHA, false},
		{"live below minimum", "0123", testHeadSHA, false},
		{"live 6 chars", "012345", testHeadSHA, false},
	}
	for _, c := range cases {
		if got := headMatches(c.live, c.expected); got != c.want {
			t.Errorf("%s: headMatches(%q,%q) = %v, want %v", c.name, c.live, c.expected, got, c.want)
		}
	}
}

func TestMergeInvalidExpectedHeadMakesNoRequest(t *testing.T) {
	for _, sha := range []string{"abc123", "zzzzzzzzzzzz", ""} {
		srv := &actionsServer{routes: map[string]func(http.ResponseWriter){}}
		p := newTestProvider(t, srv)
		if _, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: sha}); err == nil {
			t.Fatalf("sha %q: want error", sha)
		}
		if len(srv.calls) != 0 {
			t.Fatalf("sha %q: calls = %d", sha, len(srv.calls))
		}
	}
}

func TestMergeUnknownMethodMakesNoRequest(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){}}
	p := newTestProvider(t, srv)
	_, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: testPR, ExpectedHeadSHA: testHeadSHA, Method: "octopus"})
	if err == nil || len(srv.calls) != 0 {
		t.Fatalf("err = %v, calls = %d", err, len(srv.calls))
	}
}

func TestMergeFailingGetNeverPosts(t *testing.T) {
	for _, status := range []int{404, 500} {
		srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
			"GET " + prPath:             writeJSON(status, `{}`),
			"POST " + prPath + "/merge": writeJSON(200, `{}`),
		}}
		p := newTestProvider(t, srv)
		if _, err := p.MergePullRequest(context.Background(), mergeReq()); err == nil {
			t.Fatalf("status %d: want error", status)
		}
		if n := srv.count("POST", prPath+"/merge"); n != 0 {
			t.Fatalf("status %d: merge posts = %d", status, n)
		}
	}
}

func TestMergeConflictCaseInsensitive(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"GET " + prPath:             writeJSON(200, prOpen),
		"POST " + prPath + "/merge": writeJSON(400, `{"error":{"message":"Merge CONFLICT detected"}}`),
	}}
	p := newTestProvider(t, srv)
	_, err := p.MergePullRequest(context.Background(), mergeReq())
	if !errors.Is(err, ports.ErrSCMNotMergeable) {
		t.Fatalf("err = %v", err)
	}
}

func TestThrottledMutationsKeepRateLimitError(t *testing.T) {
	throttles := map[string]func(http.ResponseWriter){
		"429":            withHeader("Retry-After", "30", writeJSON(429, `{}`)),
		"403+RetryAfter": withHeader("Retry-After", "30", writeJSON(403, `{}`)),
	}
	for name, throttle := range throttles {
		srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
			"GET " + prPath:                writeJSON(200, prOpen),
			"POST " + prPath + "/merge":    throttle,
			"POST " + prPath + "/comments": throttle,
		}}
		p := newTestProvider(t, srv)
		_, mergeErr := p.MergePullRequest(context.Background(), mergeReq())
		_, commentErr := p.PostComment(context.Background(), ports.SCMCommentRequest{PR: testPR, Body: "x"})
		for op, err := range map[string]error{"merge": mergeErr, "comment": commentErr} {
			var rl *RateLimitError
			if !errors.As(err, &rl) || rl.GetRetryAfter() != 30*time.Second {
				t.Fatalf("%s %s: err = %v, want RateLimitError", name, op, err)
			}
			if name == "403+RetryAfter" && !errors.Is(err, ports.ErrSCMForbidden) {
				t.Fatalf("%s %s: want ErrSCMForbidden in chain", name, op)
			}
		}
	}
}

func TestResolveNonNumericThreadMakesNoRequest(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){}}
	p := newTestProvider(t, srv)
	for _, id := range []string{"abc", "", "1/../2"} {
		if err := p.ResolveReviewThread(context.Background(), ports.SCMReviewResolveRequest{PR: testPR, ThreadID: id}); err == nil {
			t.Fatalf("id %q: want error", id)
		}
		if err := p.UnresolveReviewThread(context.Background(), ports.SCMReviewResolveRequest{PR: testPR, ThreadID: id}); err == nil {
			t.Fatalf("id %q: want error", id)
		}
	}
	if len(srv.calls) != 0 {
		t.Fatalf("calls = %d", len(srv.calls))
	}
}

func TestPostCommentWhitespaceThreadIsTopLevel(t *testing.T) {
	srv := &actionsServer{routes: map[string]func(http.ResponseWriter){
		"POST " + prPath + "/comments": writeJSON(201, `{"id":1,"content":{"raw":"x"}}`),
	}}
	p := newTestProvider(t, srv)
	if _, err := p.PostComment(context.Background(), ports.SCMCommentRequest{PR: testPR, Body: "x", ThreadID: "  "}); err != nil {
		t.Fatal(err)
	}
	call, _ := srv.last("POST", prPath+"/comments")
	if call.Body != `{"content":{"raw":"x"}}` {
		t.Fatalf("body = %s", call.Body)
	}
}
