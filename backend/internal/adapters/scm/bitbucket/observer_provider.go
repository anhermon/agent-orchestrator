package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	ciFailureLogTailLines = 200
	// ciFailureLogTailBytes bounds the log read: the last 64 KiB of the step.
	ciFailureLogTailBytes = 64 << 10
	listPageLen           = "50"
	fetchConcurrency      = 5
)

// ListPRsByRepo lists pull requests in every state, optionally only those
// updated after updatedAfter (zero means a full listing), following next links.
// Listing all states means merged and closed PRs surface here, so no terminal
// reconcile pass is needed.
func (p *Provider) ListPRsByRepo(ctx context.Context, repo ports.SCMRepo, updatedAfter time.Time) ([]ports.SCMPRObservation, error) {
	q := allStatesQuery()
	q.Set("sort", "-updated_on")
	q.Set("pagelen", listPageLen)
	if !updatedAfter.IsZero() {
		q.Set("q", fmt.Sprintf(`updated_on > "%s"`, updatedAfter.UTC().Format(time.RFC3339Nano)))
	}
	// A PR updated while paging can show up on two pages; keep the entry with
	// the latest updated_on, at its first position.
	var result []ports.SCMPRObservation
	index := map[int]int{}
	err := p.client.paginate(ctx, repoAPIPath(repo)+"/pullrequests", q, func(raw jsonRaw) error {
		var pr bbPullRequest
		if err := unmarshal(raw, &pr); err != nil {
			return fmt.Errorf("bitbucket scm: decode PR list entry: %w", err)
		}
		obs := prToObservation(repo, &pr)
		if i, dup := index[pr.ID]; dup {
			if obs.UpdatedAtProvider.After(result[i].UpdatedAtProvider) {
				result[i] = obs
			}
			return nil
		}
		index[pr.ID] = len(result)
		result = append(result, obs)
		return nil
	})
	if err != nil {
		return nil, notFound(err)
	}
	return result, nil
}

// FetchPullRequests fetches PR, CI, review and mergeability facts for refs. The
// result is positionally aligned with refs. A failure on one ref (missing PR,
// rate limit, 5xx) becomes a Fetched=false placeholder carrying Error and does
// not fail the batch, so the other refs' observations still land; a missing PR
// carries ports.ErrSCMNotFound.
func (p *Provider) FetchPullRequests(ctx context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	results := make([]ports.SCMObservation, len(refs))
	sem := make(chan struct{}, fetchConcurrency)
	done := make(chan struct{})
	for i, ref := range refs {
		go func(idx int, ref ports.SCMPRRef) {
			defer func() { done <- struct{}{} }()
			sem <- struct{}{}
			defer func() { <-sem }()
			obs, err := p.fetchSinglePR(ctx, ref)
			if err != nil {
				p.client.logger.Debug("bitbucket scm: fetch PR failed", "repo", ref.Repo.Repo, "pr", ref.Number, "err", err)
				results[idx] = ports.SCMObservation{
					Provider: providerName,
					Host:     ref.Repo.Host,
					Repo:     ref.Repo.Repo,
					PR:       ports.SCMPRObservation{Number: ref.Number, URL: ref.URL},
					Error:    err,
				}
				return
			}
			results[idx] = obs
		}(i, ref)
	}
	for range refs {
		<-done
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (p *Provider) fetchPR(ctx context.Context, repo ports.SCMRepo, number int) (*bbPullRequest, error) {
	var pr bbPullRequest
	if _, err := p.client.do(ctx, "GET", repoAPIPath(repo)+"/pullrequests/"+strconv.Itoa(number), nil, nil, &pr); err != nil {
		return nil, notFound(err)
	}
	return &pr, nil
}

func (p *Provider) fetchSinglePR(ctx context.Context, ref ports.SCMPRRef) (ports.SCMObservation, error) {
	repo := ref.Repo
	pr, err := p.fetchPR(ctx, repo, ref.Number)
	if err != nil {
		return ports.SCMObservation{}, err
	}
	prObs := prToObservation(repo, pr)
	if requested := strings.TrimSpace(ref.URL); requested != "" && requested != strings.TrimSpace(prObs.URL) {
		prObs.URLAlias = requested
	}

	// The diffstat only feeds line counts and the conflict flag, and only an
	// open PR can conflict, so it is skipped for merged/declined PRs and its
	// failure degrades to unknown instead of hiding the PR's state. A throttle
	// still propagates so the observer backs off.
	conflict := false
	if pr.State == prStateOpen {
		entries, err := p.fetchDiffstat(ctx, repo, ref.Number)
		var rl *RateLimitError
		switch {
		case errors.As(err, &rl):
			return ports.SCMObservation{}, fmt.Errorf("bitbucket scm: fetch diffstat: %w", err)
		case err != nil:
			p.client.logger.Debug("bitbucket scm: diffstat unavailable; leaving diff stats and mergeability unknown", "repo", repo.Repo, "pr", ref.Number, "err", err)
		default:
			var added, removed int
			added, removed, conflict = diffstatSummary(entries)
			prObs.Additions, prObs.Deletions, prObs.ChangedFiles = added, removed, len(entries)
		}
	}

	ci, err := p.fetchCI(ctx, repo, prObs.HeadSHA)
	if err != nil {
		return ports.SCMObservation{}, fmt.Errorf("bitbucket scm: fetch CI: %w", err)
	}
	decision := reviewDecision(pr.Participants)
	return ports.SCMObservation{
		Fetched:      true,
		ObservedAt:   time.Now(),
		Provider:     providerName,
		Host:         repo.Host,
		Repo:         repo.Repo,
		PR:           prObs,
		CI:           ci,
		Review:       ports.SCMReviewObservation{Decision: string(decision)},
		Mergeability: mergeability(conflict, pr.State),
	}, nil
}

func (p *Provider) fetchDiffstat(ctx context.Context, repo ports.SCMRepo, number int) ([]bbDiffstatEntry, error) {
	var entries []bbDiffstatEntry
	err := p.client.paginate(ctx, repoAPIPath(repo)+"/pullrequests/"+strconv.Itoa(number)+"/diffstat", url.Values{"pagelen": {"500"}}, func(raw jsonRaw) error {
		var e bbDiffstatEntry
		if err := unmarshal(raw, &e); err != nil {
			return fmt.Errorf("bitbucket scm: decode diffstat entry: %w", err)
		}
		entries = append(entries, e)
		return nil
	})
	return entries, err
}

// fetchCI reads the commit statuses of headSHA. A 403 degrades to unknown CI
// (warned once per repo) instead of failing the PR; any other error propagates
// so the observer keeps the last durable CI state.
func (p *Provider) fetchCI(ctx context.Context, repo ports.SCMRepo, headSHA string) (ports.SCMCIObservation, error) {
	if headSHA == "" {
		return ciObservation("", nil), nil
	}
	q := url.Values{"sort": {"-updated_on"}, "pagelen": {"100"}}
	var statuses []bbStatus
	err := p.client.paginate(ctx, repoAPIPath(repo)+"/commit/"+url.PathEscape(headSHA)+"/statuses", q, func(raw jsonRaw) error {
		var s bbStatus
		if err := unmarshal(raw, &s); err != nil {
			return fmt.Errorf("bitbucket scm: decode status: %w", err)
		}
		statuses = append(statuses, s)
		return nil
	})
	if err != nil {
		if errors.Is(err, ports.ErrSCMForbidden) {
			p.warnStatusesForbidden(repo)
			return ciObservation(headSHA, nil), nil
		}
		return ports.SCMCIObservation{}, notFound(err)
	}
	return ciObservation(headSHA, statuses), nil
}

// FetchFailedCheckLogTail returns the last ciFailureLogTailLines lines of the
// failed step of the Bitbucket Pipelines run the check's URL points at. A
// check from any other CI returns "" and ports.ErrSCMUnsupported naming the URL.
func (p *Provider) FetchFailedCheckLogTail(ctx context.Context, repo ports.SCMRepo, check ports.SCMCheckObservation) (string, error) {
	pipelineID, ok := pipelineIDFromURL(repo, check.URL)
	if !ok {
		return "", fmt.Errorf("%w: bitbucket log tail for non-Pipelines status %q", ports.ErrSCMUnsupported, check.URL)
	}
	stepsPath := repoAPIPath(repo) + "/pipelines/" + url.PathEscape(pipelineID) + "/steps/"
	stepUUID := ""
	errStop := errors.New("failed step found")
	err := p.client.paginate(ctx, strings.TrimSuffix(stepsPath, "/")+"/", url.Values{"pagelen": {"100"}}, func(raw jsonRaw) error {
		var s struct {
			UUID  string `json:"uuid"`
			State struct {
				Result struct {
					Name string `json:"name"`
				} `json:"result"`
			} `json:"state"`
		}
		if err := unmarshal(raw, &s); err != nil {
			return fmt.Errorf("bitbucket scm: decode pipeline step: %w", err)
		}
		// A step that errored (setup or infrastructure failure) has no
		// FAILED result but still explains a red pipeline.
		if r := s.State.Result.Name; r == "FAILED" || r == "ERROR" {
			stepUUID = s.UUID
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return "", notFound(err)
	}
	if stepUUID == "" {
		return "", fmt.Errorf("%w: bitbucket pipeline %s has no failed step", ports.ErrSCMNotFound, pipelineID)
	}
	body, err := p.client.doRaw(ctx, stepsPath+url.PathEscape(stepUUID)+"/log", map[string]string{
		"Range": "bytes=-" + strconv.Itoa(ciFailureLogTailBytes),
	})
	if err != nil {
		return "", notFound(err)
	}
	return tailLines(string(body), ciFailureLogTailLines), nil
}

// pipelineIDFromURL extracts the pipeline identifier from a Bitbucket Pipelines
// result URL (".../addon/pipelines/home#!/results/{id}" or ".../pipelines/results/{id}").
// It accepts only the repo's own host, so a status pointing at another CI is
// never sent credentials.
func pipelineIDFromURL(repo ports.SCMRepo, raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Hostname(), repo.Host) {
		return "", false
	}
	for _, s := range []string{u.Fragment, u.Path} {
		if !strings.Contains(s, "pipelines") && !strings.Contains(u.Path, "pipelines") {
			continue
		}
		if _, after, found := strings.Cut(s, "/results/"); found {
			id, _, _ := strings.Cut(after, "/")
			if id != "" {
				return id, true
			}
		}
	}
	return "", false
}

// FetchReviewThreads returns the PR's comment threads plus its review decision
// and per-reviewer summaries.
func (p *Provider) FetchReviewThreads(ctx context.Context, ref ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	repo := ref.Repo
	var comments []bbComment
	err := p.client.paginate(ctx, repoAPIPath(repo)+"/pullrequests/"+strconv.Itoa(ref.Number)+"/comments", url.Values{"pagelen": {"100"}, "sort": {"created_on"}}, func(raw jsonRaw) error {
		var c bbComment
		if err := unmarshal(raw, &c); err != nil {
			return fmt.Errorf("bitbucket scm: decode comment: %w", err)
		}
		comments = append(comments, c)
		return nil
	})
	if err != nil {
		return ports.SCMReviewObservation{}, notFound(err)
	}
	pr, err := p.fetchPR(ctx, repo, ref.Number)
	if err != nil {
		return ports.SCMReviewObservation{}, err
	}
	return ports.SCMReviewObservation{
		Decision: string(reviewDecision(pr.Participants)),
		Reviews:  reviewSummaries(pr.Participants),
		Threads:  buildThreads(comments),
	}, nil
}
