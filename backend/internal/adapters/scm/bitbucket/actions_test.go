package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

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
