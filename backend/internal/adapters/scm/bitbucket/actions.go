package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	_ ports.SCMMerger          = (*Provider)(nil)
	_ ports.SCMCommenter       = (*Provider)(nil)
	_ ports.SCMPRStateChanger  = (*Provider)(nil)
	_ ports.SCMReviewResolver  = (*Provider)(nil)
	_ ports.SCMReviewRequester = (*Provider)(nil)
	_ ports.SCMMergeQueuer     = (*Provider)(nil)
	_ ports.SCMAttachmentStore = (*Provider)(nil)
	_ providerExtras           = (*Provider)(nil)
)

// providerExtras are the mutations the provider offers beyond any port, so a
// signature drift is caught at compile time.
type providerExtras interface {
	ApprovePullRequest(ctx context.Context, pr ports.SCMPRRef) error
	RequestPullRequestChanges(ctx context.Context, pr ports.SCMPRRef) error
	UnresolveReviewThread(ctx context.Context, request ports.SCMReviewResolveRequest) error
}

// ErrMergePending reports that Bitbucket accepted a merge but it had not
// completed when polling stopped. The caller must not record a merge; the
// observer confirms the state later.
var ErrMergePending = errors.New("bitbucket scm: merge still pending")

const (
	// mergePollBudget bounds the total time spent polling an async merge.
	mergePollBudget = 10 * time.Second
	// mergePollInterval is the wait between task-status polls.
	mergePollInterval = 500 * time.Millisecond
)

// headSHAPattern accepts a full or abbreviated commit hash.
var headSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// mergeStrategy maps a port merge method to Bitbucket's merge_strategy value.
// Bitbucket's rebase_fast_forward replays the branch commits onto the
// destination, the closest match to a rebase merge.
func mergeStrategy(m ports.SCMMergeMethod) (string, error) {
	switch m {
	case "":
		return "", nil
	case ports.SCMMergeSquash:
		return "squash", nil
	case ports.SCMMergeMerge:
		return "merge_commit", nil
	case ports.SCMMergeRebase:
		return "rebase_fast_forward", nil
	default:
		return "", fmt.Errorf("bitbucket scm: unsupported merge method %q", m)
	}
}

func validPRRef(pr ports.SCMPRRef) error {
	if pr.Number <= 0 {
		return fmt.Errorf("bitbucket scm: invalid pull request number %d", pr.Number)
	}
	if strings.TrimSpace(pr.Repo.Owner) == "" || strings.TrimSpace(pr.Repo.Name) == "" {
		return errors.New("bitbucket scm: invalid repository owner/name")
	}
	return nil
}

func prAPIPath(pr ports.SCMPRRef) string {
	return repoAPIPath(pr.Repo) + "/pullrequests/" + strconv.Itoa(pr.Number)
}

// commentID validates a Bitbucket numeric comment id before it is placed in a
// URL path.
func commentID(raw string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("bitbucket scm: invalid comment id %q", raw)
	}
	return id, nil
}

// mutationError maps a failed mutation to the provider-neutral sentinels. 403
// already carries ports.ErrSCMForbidden from the client; 404 becomes
// ErrSCMNotFound; 400, 409 and 422 become ErrSCMRejected. Other errors
// (401, 429, 5xx, transport) pass through unchanged.
func mutationError(err error) error {
	if err == nil {
		return nil
	}
	err = notFound(err)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %w", ports.ErrSCMRejected, err)
		}
	}
	return err
}

// headMatches reports whether the live source commit satisfies the expected
// head. Bitbucket reports the PR source commit abbreviated, so either side may
// be a prefix of the other.
func headMatches(live, expected string) bool {
	live, expected = strings.ToLower(live), strings.ToLower(expected)
	return len(live) >= 7 && (strings.HasPrefix(expected, live) || strings.HasPrefix(live, expected))
}

// MergePullRequest merges the PR. Bitbucket's merge endpoint has no head SHA
// precondition, so the PR is read first and the merge is refused without a
// merge call when its source commit no longer matches ExpectedHeadSHA. A small
// window remains between the read and the merge.
func (p *Provider) MergePullRequest(ctx context.Context, request ports.SCMMergeRequest) (ports.SCMMergeResult, error) {
	if p == nil || p.client == nil {
		return ports.SCMMergeResult{}, errors.New("bitbucket scm: merge provider is not configured")
	}
	if err := validPRRef(request.PR); err != nil {
		return ports.SCMMergeResult{}, err
	}
	expected := strings.TrimSpace(request.ExpectedHeadSHA)
	if !headSHAPattern.MatchString(expected) {
		return ports.SCMMergeResult{}, errors.New("bitbucket scm: invalid expected head sha")
	}
	strategy, err := mergeStrategy(request.Method)
	if err != nil {
		return ports.SCMMergeResult{}, err
	}

	path := prAPIPath(request.PR)
	var pr struct {
		State  string `json:"state"`
		Source struct {
			Commit struct {
				Hash string `json:"hash"`
			} `json:"commit"`
		} `json:"source"`
	}
	q := url.Values{"fields": {"state,source.commit.hash"}}
	if _, err := p.client.do(ctx, http.MethodGet, path, q, nil, &pr); err != nil {
		return ports.SCMMergeResult{}, mutationError(err)
	}
	if !strings.EqualFold(pr.State, "OPEN") {
		return ports.SCMMergeResult{}, fmt.Errorf("%w: pull request state is %q, want OPEN", ports.ErrSCMRejected, pr.State)
	}
	if !headMatches(pr.Source.Commit.Hash, expected) {
		p.client.logger.Debug("bitbucket merge refused: head changed", "repo", request.PR.Repo.Repo, "number", request.PR.Number)
		return ports.SCMMergeResult{}, fmt.Errorf("%w: pull request source commit is %q, expected %q", ports.ErrSCMHeadChanged, pr.Source.Commit.Hash, expected)
	}

	var body map[string]any
	if strategy != "" {
		body = map[string]any{"merge_strategy": strategy}
	}
	var merged struct {
		MergeCommit struct {
			Hash string `json:"hash"`
		} `json:"merge_commit"`
	}
	resp, err := p.client.do(ctx, http.MethodPost, path+"/merge", nil, body, &merged)
	if err != nil {
		return ports.SCMMergeResult{}, mergeError(err)
	}
	if resp != nil && resp.StatusCode == http.StatusAccepted {
		// Long merges complete asynchronously behind a task-status URL.
		p.client.logger.Debug("bitbucket merge accepted asynchronously", "repo", request.PR.Repo.Repo, "number", request.PR.Number)
		if err := p.awaitMergeTask(ctx, resp.Header.Get("Location")); err != nil {
			return ports.SCMMergeResult{}, err
		}
		return p.confirmMerged(ctx, path, true)
	}
	if merged.MergeCommit.Hash != "" {
		return ports.SCMMergeResult{MergeCommitSHA: merged.MergeCommit.Hash}, nil
	}
	// A 2xx without a commit (for example a fast-forward) means the merge
	// happened; confirm it rather than report failure for a merged PR.
	return p.confirmMerged(ctx, path, false)
}

// awaitMergeTask polls the task-status URL until it reports a result, the
// budget is spent (ErrMergePending) or ctx ends. An empty location skips
// polling so the caller re-reads the PR once.
func (p *Provider) awaitMergeTask(ctx context.Context, location string) error {
	if strings.TrimSpace(location) == "" {
		return nil
	}
	ref, err := url.Parse(location)
	if err != nil {
		return fmt.Errorf("bitbucket scm: invalid merge task location: %w", err)
	}
	taskURL := p.client.baseURL.ResolveReference(ref).String()
	if err := p.client.checkSameHost(taskURL); err != nil {
		return err
	}
	budget := mergePollBudget
	if p.mergeBudget > 0 {
		budget = p.mergeBudget
	}
	// The deadline bounds wall time, including a task request that hangs.
	pollCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	budgetSpent := func(err error) error {
		if ctx.Err() == nil && pollCtx.Err() != nil {
			return fmt.Errorf("%w: task not finished within %s", ErrMergePending, budget)
		}
		return err
	}
	for waited := time.Duration(0); ; waited += mergePollInterval {
		var task struct {
			Status string `json:"task_status"`
		}
		if _, err := p.client.doURL(pollCtx, http.MethodGet, taskURL, nil, &task); err != nil {
			return budgetSpent(mutationError(err))
		}
		switch strings.ToUpper(task.Status) {
		case "SUCCESS":
			return nil
		case "FAILED", "ERROR":
			return fmt.Errorf("%w: merge task %s", ports.ErrSCMRejected, task.Status)
		}
		if waited+mergePollInterval > budget {
			return fmt.Errorf("%w: task still %q after %s", ErrMergePending, task.Status, budget)
		}
		if err := p.sleepFor(pollCtx, mergePollInterval); err != nil {
			return budgetSpent(err)
		}
	}
}

func (p *Provider) sleepFor(ctx context.Context, d time.Duration) error {
	if p.sleep != nil {
		return p.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// confirmMerged re-reads the PR after an accepted merge. MERGED succeeds (the
// commit hash may be empty); anything else is an error, ErrMergePending when
// the merge was asynchronous.
func (p *Provider) confirmMerged(ctx context.Context, path string, async bool) (ports.SCMMergeResult, error) {
	var pr struct {
		State       string `json:"state"`
		MergeCommit struct {
			Hash string `json:"hash"`
		} `json:"merge_commit"`
	}
	q := url.Values{"fields": {"state,merge_commit.hash"}}
	if _, err := p.client.do(ctx, http.MethodGet, path, q, nil, &pr); err != nil {
		return ports.SCMMergeResult{}, mutationError(err)
	}
	if strings.EqualFold(pr.State, "MERGED") {
		return ports.SCMMergeResult{MergeCommitSHA: pr.MergeCommit.Hash}, nil
	}
	if async {
		return ports.SCMMergeResult{}, fmt.Errorf("%w: pull request state is %q", ErrMergePending, pr.State)
	}
	return ports.SCMMergeResult{}, fmt.Errorf("bitbucket scm: merge accepted but pull request state is %q", pr.State)
}

// mergeError classifies a failed merge call: a conflict (409, or 400 whose
// body mentions one) is ErrSCMNotMergeable; anything else follows
// mutationError.
func mergeError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		conflict := strings.Contains(strings.ToLower(apiErr.Body), "conflict")
		if apiErr.Status == http.StatusConflict || (apiErr.Status == http.StatusBadRequest && conflict) {
			return fmt.Errorf("%w: %w", ports.ErrSCMNotMergeable, err)
		}
	}
	return mutationError(err)
}

// PostComment posts a top-level comment, or a reply when ThreadID names the
// thread's root comment.
func (p *Provider) PostComment(ctx context.Context, request ports.SCMCommentRequest) (ports.SCMCommentResult, error) {
	if p == nil || p.client == nil {
		return ports.SCMCommentResult{}, errors.New("bitbucket scm: commenter is not configured")
	}
	if err := validPRRef(request.PR); err != nil {
		return ports.SCMCommentResult{}, err
	}
	if strings.TrimSpace(request.Body) == "" {
		return ports.SCMCommentResult{}, errors.New("bitbucket scm: comment body is required")
	}
	payload := map[string]any{"content": map[string]string{"raw": request.Body}}
	if strings.TrimSpace(request.ThreadID) != "" {
		parent, err := commentID(request.ThreadID)
		if err != nil {
			return ports.SCMCommentResult{}, err
		}
		payload["parent"] = map[string]int{"id": parent}
	}
	var created bbComment
	if _, err := p.client.do(ctx, http.MethodPost, prAPIPath(request.PR)+"/comments", nil, payload, &created); err != nil {
		return ports.SCMCommentResult{}, mutationError(err)
	}
	obs := commentObservation(&created)
	return ports.SCMCommentResult{
		ID:        obs.ID,
		URL:       obs.URL,
		Author:    obs.Author,
		IsBot:     obs.IsBot,
		Body:      obs.Body,
		CreatedAt: obs.CreatedAt,
	}, nil
}

// ApprovePullRequest approves the PR as the authenticated account. It is not
// part of a provider-neutral port yet.
func (p *Provider) ApprovePullRequest(ctx context.Context, pr ports.SCMPRRef) error {
	return p.postPRAction(ctx, pr, "/approve")
}

// RequestPullRequestChanges marks the PR as needing changes from the
// authenticated account. It is not part of a provider-neutral port yet.
func (p *Provider) RequestPullRequestChanges(ctx context.Context, pr ports.SCMPRRef) error {
	return p.postPRAction(ctx, pr, "/request-changes")
}

func (p *Provider) postPRAction(ctx context.Context, pr ports.SCMPRRef, suffix string) error {
	if p == nil || p.client == nil {
		return errors.New("bitbucket scm: provider is not configured")
	}
	if err := validPRRef(pr); err != nil {
		return err
	}
	_, err := p.client.do(ctx, http.MethodPost, prAPIPath(pr)+suffix, nil, nil, nil)
	return mutationError(err)
}

// ChangePullRequestState declines the PR for SCMPRClose. Bitbucket cannot
// reopen a declined PR and has no draft state, so the other actions return
// ErrSCMUnsupported.
func (p *Provider) ChangePullRequestState(ctx context.Context, request ports.SCMPRStateRequest) error {
	switch request.Action {
	case ports.SCMPRClose:
		return p.postPRAction(ctx, request.PR, "/decline")
	case ports.SCMPRReopen:
		return fmt.Errorf("%w: bitbucket cannot reopen a declined pull request", ports.ErrSCMUnsupported)
	case ports.SCMPRReady, ports.SCMPRDraft:
		return fmt.Errorf("%w: bitbucket draft state change %q is not implemented", ports.ErrSCMUnsupported, request.Action)
	default:
		return fmt.Errorf("bitbucket scm: unknown pull request state action %q", request.Action)
	}
}

// ResolveReviewThread resolves the comment thread rooted at ThreadID.
func (p *Provider) ResolveReviewThread(ctx context.Context, request ports.SCMReviewResolveRequest) error {
	return p.setThreadResolved(ctx, request, http.MethodPost)
}

// UnresolveReviewThread reopens a resolved comment thread.
func (p *Provider) UnresolveReviewThread(ctx context.Context, request ports.SCMReviewResolveRequest) error {
	return p.setThreadResolved(ctx, request, http.MethodDelete)
}

func (p *Provider) setThreadResolved(ctx context.Context, request ports.SCMReviewResolveRequest, method string) error {
	if p == nil || p.client == nil {
		return errors.New("bitbucket scm: review resolver is not configured")
	}
	if err := validPRRef(request.PR); err != nil {
		return err
	}
	id, err := commentID(request.ThreadID)
	if err != nil {
		return err
	}
	_, err = p.client.do(ctx, method, prAPIPath(request.PR)+"/comments/"+strconv.Itoa(id)+"/resolve", nil, nil, nil)
	return mutationError(err)
}

// RequestReview is unsupported: Bitbucket adds reviewers by account UUID
// through a full pull-request update, which the port's login cannot address.
func (p *Provider) RequestReview(context.Context, ports.SCMReviewRequest) error {
	return fmt.Errorf("%w: bitbucket review requests", ports.ErrSCMUnsupported)
}

// EnqueuePullRequest is unsupported: Bitbucket Cloud has no merge queue.
func (p *Provider) EnqueuePullRequest(context.Context, ports.SCMEnqueueRequest) (domain.MergeQueueEntry, error) {
	return domain.MergeQueueEntry{}, fmt.Errorf("%w: bitbucket merge queue", ports.ErrSCMUnsupported)
}

// DequeuePullRequest is unsupported: Bitbucket Cloud has no merge queue.
func (p *Provider) DequeuePullRequest(context.Context, ports.SCMDequeueRequest) error {
	return fmt.Errorf("%w: bitbucket merge queue", ports.ErrSCMUnsupported)
}

// AttachmentAccess reports that comment attachments are unsupported.
func (p *Provider) AttachmentAccess(context.Context, ports.SCMPRRef) (ports.SCMAttachmentAccess, error) {
	return ports.SCMAttachmentAccess{}, fmt.Errorf("%w: bitbucket comment attachments", ports.ErrSCMUnsupported)
}

// StoreAttachments is unsupported: Bitbucket's API has no comment attachment upload.
func (p *Provider) StoreAttachments(context.Context, ports.SCMAttachmentBatch) (ports.SCMStoredAttachments, error) {
	return ports.SCMStoredAttachments{}, fmt.Errorf("%w: bitbucket comment attachments", ports.ErrSCMUnsupported)
}

// DiscardAttachments is unsupported; nothing is ever stored.
func (p *Provider) DiscardAttachments(context.Context, ports.SCMPRRef, string) error {
	return fmt.Errorf("%w: bitbucket comment attachments", ports.ErrSCMUnsupported)
}
