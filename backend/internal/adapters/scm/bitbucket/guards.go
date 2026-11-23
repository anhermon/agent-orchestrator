package bitbucket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// emptyPRListETag is the synthetic ETag of a repository with no pull requests.
const emptyPRListETag = "bb:empty"

// repoAPIPath returns the API path prefix of repo.
func repoAPIPath(repo ports.SCMRepo) string {
	return "/repositories/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
}

func allStatesQuery() url.Values {
	return url.Values{"state": append([]string(nil), allPRStates...)}
}

// RepoPRListGuard returns a synthetic ETag for the repository's PR list.
// Bitbucket Cloud has no conditional requests, so the guard fetches the single
// most recently updated PR (any state) and encodes its updated_on and id; any
// PR update, creation or state change moves the newest updated_on.
func (p *Provider) RepoPRListGuard(ctx context.Context, repo ports.SCMRepo, etag string) (ports.SCMGuardResult, error) {
	q := allStatesQuery()
	q.Set("sort", "-updated_on")
	q.Set("pagelen", "1")
	q.Set("fields", "values.updated_on,values.id")
	var body struct {
		Values []struct {
			ID        int    `json:"id"`
			UpdatedOn string `json:"updated_on"`
		} `json:"values"`
	}
	if _, err := p.client.do(ctx, "GET", repoAPIPath(repo)+"/pullrequests", q, nil, &body); err != nil {
		return ports.SCMGuardResult{}, notFound(err)
	}
	newETag := emptyPRListETag
	if len(body.Values) > 0 {
		newETag = fmt.Sprintf("bb:%s:%d", body.Values[0].UpdatedOn, body.Values[0].ID)
	}
	return ports.SCMGuardResult{ETag: newETag, NotModified: etag != "" && etag == newETag}, nil
}

// CommitChecksGuard returns a hash-based guard for the latest commit status of
// headSHA. A 403 on the statuses endpoint (a token without the repository
// scope) degrades to "not modified" with the previous ETag and one Warn per
// repo, so the observer keeps polling instead of failing the repo every tick.
func (p *Provider) CommitChecksGuard(ctx context.Context, repo ports.SCMRepo, headSHA, etag string) (ports.SCMGuardResult, error) {
	if headSHA == "" {
		return ports.SCMGuardResult{}, fmt.Errorf("bitbucket scm: empty head SHA: %w", ports.ErrSCMNotFound)
	}
	q := url.Values{"sort": {"-updated_on"}, "pagelen": {"1"}}
	var raw json.RawMessage
	_, err := p.client.do(ctx, "GET", repoAPIPath(repo)+"/commit/"+url.PathEscape(headSHA)+"/statuses", q, nil, &raw)
	if err != nil {
		if errors.Is(err, ports.ErrSCMForbidden) {
			p.warnStatusesForbidden(repo)
			return ports.SCMGuardResult{ETag: etag, NotModified: true}, nil
		}
		return ports.SCMGuardResult{}, notFound(err)
	}
	sum := sha256.Sum256(raw)
	newETag := hex.EncodeToString(sum[:])
	return ports.SCMGuardResult{ETag: newETag, NotModified: etag == newETag}, nil
}

// warnStatusesForbidden logs the commit-statuses 403 once per repo.
func (p *Provider) warnStatusesForbidden(repo ports.SCMRepo) {
	if _, loaded := p.forbiddenWarned.LoadOrStore(repo.Repo, struct{}{}); loaded {
		return
	}
	p.client.logger.Warn("bitbucket scm: commit statuses forbidden; CI state will be unknown for this repo (token needs repository read access)", "repo", repo.Repo)
}
