package bitbucket

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Bitbucket PR states. SUPERSEDED is a PR replaced by a newer one; AO treats it
// as closed.
const (
	prStateOpen       = "OPEN"
	prStateMerged     = "MERGED"
	prStateDeclined   = "DECLINED"
	prStateSuperseded = "SUPERSEDED"
)

// allPRStates is requested on every PR listing so merged and closed PRs are
// observed too and no separate terminal reconcile is needed.
var allPRStates = []string{prStateOpen, prStateMerged, prStateDeclined, prStateSuperseded}

type bbUser struct {
	DisplayName string `json:"display_name"`
	Nickname    string `json:"nickname"`
	// Type is "user", or "app_user" for a bot.
	Type  string `json:"type"`
	Links struct {
		Avatar struct {
			Href string `json:"href"`
		} `json:"avatar"`
	} `json:"links"`
}

// login is the stable human-readable name of a user.
func (u bbUser) login() string {
	if u.Nickname != "" {
		return u.Nickname
	}
	return u.DisplayName
}

func (u bbUser) isBot() bool { return u.Type == "app_user" }

type bbEndpoint struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
	Commit struct {
		Hash string `json:"hash"`
	} `json:"commit"`
	Repository struct {
		FullName string `json:"full_name"`
		UUID     string `json:"uuid"`
	} `json:"repository"`
}

type bbParticipant struct {
	Role           string    `json:"role"`
	Approved       bool      `json:"approved"`
	State          string    `json:"state"`
	ParticipatedOn time.Time `json:"participated_on"`
	User           bbUser    `json:"user"`
}

type bbPullRequest struct {
	ID          int        `json:"id"`
	Title       string     `json:"title"`
	State       string     `json:"state"`
	Draft       bool       `json:"draft"`
	Author      bbUser     `json:"author"`
	Source      bbEndpoint `json:"source"`
	Destination bbEndpoint `json:"destination"`
	Links       struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	MergeCommit *struct {
		Hash string `json:"hash"`
	} `json:"merge_commit"`
	CreatedOn    time.Time       `json:"created_on"`
	UpdatedOn    time.Time       `json:"updated_on"`
	Participants []bbParticipant `json:"participants"`
}

func prToObservation(repo ports.SCMRepo, pr *bbPullRequest) ports.SCMPRObservation {
	merged := pr.State == prStateMerged
	closed := pr.State == prStateDeclined || pr.State == prStateSuperseded
	headRepo := strings.ToLower(pr.Source.Repository.FullName)
	fork := headRepo != "" && !strings.EqualFold(headRepo, repo.Repo)
	if headRepo == "" {
		headRepo = repo.Repo
	}
	obs := ports.SCMPRObservation{
		ProviderID:        providerID(repo, pr),
		URL:               pr.Links.HTML.Href,
		HTMLURL:           pr.Links.HTML.Href,
		Number:            pr.ID,
		State:             string(normalizePRState(pr)),
		Draft:             pr.Draft && pr.State == prStateOpen,
		Merged:            merged,
		Closed:            closed,
		SourceBranch:      pr.Source.Branch.Name,
		HeadRepo:          headRepo,
		BaseRepo:          repo.Repo,
		HeadFromFork:      fork,
		TargetBranch:      pr.Destination.Branch.Name,
		HeadSHA:           pr.Source.Commit.Hash,
		Title:             pr.Title,
		Author:            pr.Author.login(),
		AuthorAvatarURL:   pr.Author.Links.Avatar.Href,
		BaseSHA:           pr.Destination.Commit.Hash,
		ProviderState:     pr.State,
		CreatedAtProvider: pr.CreatedOn,
		UpdatedAtProvider: pr.UpdatedOn,
	}
	if pr.MergeCommit != nil {
		obs.MergeCommitSHA = pr.MergeCommit.Hash
	}
	return obs
}

// providerID scopes the repo-local PR id by the destination repository uuid,
// which survives renames; it falls back to the repo name when no uuid is sent.
func providerID(repo ports.SCMRepo, pr *bbPullRequest) string {
	if pr.ID <= 0 {
		return ""
	}
	scope := strings.Trim(pr.Destination.Repository.UUID, "{}")
	if scope == "" {
		scope = repo.Repo
	}
	return scope + "#" + strconv.Itoa(pr.ID)
}

func normalizePRState(pr *bbPullRequest) domain.PRState {
	switch pr.State {
	case prStateOpen:
		if pr.Draft {
			return domain.PRStateDraft
		}
		return domain.PRStateOpen
	case prStateMerged:
		return domain.PRStateMerged
	default:
		return domain.PRStateClosed
	}
}

// reviewDecision derives the decision from REVIEWER participants only: any
// changes_requested wins, then any approval. Bitbucket exposes no required
// approval count, so a pending reviewer yields no decision.
func reviewDecision(participants []bbParticipant) domain.ReviewDecision {
	approved := false
	for _, p := range participants {
		if p.Role != "REVIEWER" {
			continue
		}
		switch {
		case p.State == "changes_requested":
			return domain.ReviewChangesRequest
		case p.Approved || p.State == "approved":
			approved = true
		}
	}
	if approved {
		return domain.ReviewApproved
	}
	return domain.ReviewNone
}

func reviewSummaries(participants []bbParticipant) []ports.SCMReviewSummaryObservation {
	var out []ports.SCMReviewSummaryObservation
	for _, p := range participants {
		if p.Role != "REVIEWER" {
			continue
		}
		var state domain.ReviewDecision
		switch {
		case p.State == "changes_requested":
			state = domain.ReviewChangesRequest
		case p.Approved || p.State == "approved":
			state = domain.ReviewApproved
		default:
			continue
		}
		out = append(out, ports.SCMReviewSummaryObservation{
			ID:          "participant:" + p.User.login(),
			Author:      p.User.login(),
			State:       string(state),
			IsBot:       p.User.isBot(),
			SubmittedAt: p.ParticipatedOn,
		})
	}
	return out
}

type bbDiffstatEntry struct {
	Status       string `json:"status"`
	LinesAdded   int    `json:"lines_added"`
	LinesRemoved int    `json:"lines_removed"`
}

// diffstatSummary totals the diffstat and reports whether any entry is a merge
// conflict.
func diffstatSummary(entries []bbDiffstatEntry) (added, removed int, conflict bool) {
	for _, e := range entries {
		added += e.LinesAdded
		removed += e.LinesRemoved
		if e.Status == "merge conflict" {
			conflict = true
		}
	}
	return added, removed, conflict
}

// mergeability is "unknown" unless the diffstat shows a conflict: Bitbucket
// Cloud exposes no mergeable flag on the PR itself.
func mergeability(conflict bool, state string) ports.SCMMergeabilityObservation {
	if conflict && state == prStateOpen {
		return ports.SCMMergeabilityObservation{
			State:    string(domain.MergeConflicting),
			Conflict: true,
			Blockers: []string{"conflicts"},
		}
	}
	return ports.SCMMergeabilityObservation{State: string(domain.MergeUnknown)}
}

type bbStatus struct {
	Key       string    `json:"key"`
	State     string    `json:"state"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	UpdatedOn time.Time `json:"updated_on"`
}

// checkStatus maps a Bitbucket commit status state to AO's check status.
func checkStatus(state string) domain.PRCheckStatus {
	switch state {
	case "SUCCESSFUL":
		return domain.PRCheckPassed
	case "FAILED":
		return domain.PRCheckFailed
	case "INPROGRESS":
		return domain.PRCheckInProgress
	case "STOPPED":
		return domain.PRCheckCancelled
	default:
		return domain.PRCheckUnknown
	}
}

// ciObservation builds the CI rollup from statuses in any order. Bitbucket
// keeps every update of a status key, so only the latest per key counts.
func ciObservation(headSHA string, statuses []bbStatus) ports.SCMCIObservation {
	if len(statuses) == 0 {
		return ports.SCMCIObservation{Summary: string(domain.CIUnknown), HeadSHA: headSHA}
	}
	// Pick the latest update per key by updated_on rather than trusting the
	// server's sort; keys stay in first-seen order and ties keep the first.
	latest := map[string]bbStatus{}
	var keys []string
	for _, s := range statuses {
		cur, seen := latest[s.Key]
		if !seen {
			keys = append(keys, s.Key)
		}
		if !seen || s.UpdatedOn.After(cur.UpdatedOn) {
			latest[s.Key] = s
		}
	}
	var checks, failed []ports.SCMCheckObservation
	allPassed, anyFailed, anyPending := true, false, false
	for _, key := range keys {
		s := latest[key]
		status := checkStatus(s.State)
		name := s.Name
		if name == "" {
			name = s.Key
		}
		check := ports.SCMCheckObservation{
			Name:       name,
			Status:     string(status),
			Conclusion: s.State,
			URL:        s.URL,
			ProviderID: s.Key,
		}
		checks = append(checks, check)
		switch status {
		case domain.PRCheckFailed:
			anyFailed = true
			failed = append(failed, check)
		case domain.PRCheckInProgress:
			anyPending = true
		}
		if status != domain.PRCheckPassed {
			allPassed = false
		}
	}
	summary := domain.CIUnknown
	switch {
	case anyFailed:
		summary = domain.CIFailing
	case anyPending:
		summary = domain.CIPending
	case allPassed:
		summary = domain.CIPassing
	}
	return ports.SCMCIObservation{
		Summary:           string(summary),
		HeadSHA:           headSHA,
		FailedFingerprint: failedFingerprint(headSHA, failed),
		Checks:            checks,
		FailedChecks:      failed,
	}
}

func failedFingerprint(headSHA string, checks []ports.SCMCheckObservation) string {
	if len(checks) == 0 {
		return ""
	}
	parts := make([]string, len(checks))
	for i, c := range checks {
		parts[i] = headSHA + "\x00" + c.Name + "\x00" + c.Status + "\x00" + c.Conclusion + "\x00" + c.URL + "\x00" + c.ProviderID
	}
	sort.Strings(parts)
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return hex.EncodeToString(h[:])
}

type bbComment struct {
	ID      int `json:"id"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
	User      bbUser    `json:"user"`
	CreatedOn time.Time `json:"created_on"`
	Deleted   bool      `json:"deleted"`
	Parent    *struct {
		ID int `json:"id"`
	} `json:"parent"`
	Inline *struct {
		Path string `json:"path"`
		From *int   `json:"from"`
		To   *int   `json:"to"`
	} `json:"inline"`
	Resolution *struct{} `json:"resolution"`
	Links      struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func commentObservation(c *bbComment) ports.SCMReviewCommentObservation {
	return ports.SCMReviewCommentObservation{
		ID:        strconv.Itoa(c.ID),
		Author:    c.User.login(),
		Body:      c.Content.Raw,
		URL:       c.Links.HTML.Href,
		IsBot:     c.User.isBot(),
		CreatedAt: c.CreatedOn,
	}
}

// buildThreads groups comments into threads: a root is a comment without a
// parent and replies attach to their root, in API order. Deleted comments are
// never emitted. A live reply whose root was deleted is kept: the deleted root
// stays as a placeholder that supplies the thread's id, path, line and
// resolution but contributes no comment. Replies whose root is absent from the
// listing are dropped.
func buildThreads(comments []bbComment) []ports.SCMReviewThreadObservation {
	byID := make(map[int]*bbComment, len(comments))
	for i := range comments {
		byID[comments[i].ID] = &comments[i]
	}
	rootOf := func(c *bbComment) *bbComment {
		// Bitbucket threads are one level deep; the loop tolerates deeper chains
		// and is bounded so a malformed cycle cannot spin.
		for i := 0; c.Parent != nil && i < len(comments); i++ {
			parent, ok := byID[c.Parent.ID]
			if !ok {
				return nil
			}
			c = parent
		}
		if c.Parent != nil {
			return nil
		}
		return c
	}

	threads := map[int]*ports.SCMReviewThreadObservation{}
	var order []int
	for i := range comments {
		c := &comments[i]
		if c.Deleted {
			continue
		}
		root := rootOf(c)
		if root == nil {
			continue
		}
		th, ok := threads[root.ID]
		if !ok {
			th = &ports.SCMReviewThreadObservation{ID: strconv.Itoa(root.ID), Resolved: root.Resolution != nil, IsBot: true}
			if root.Inline != nil {
				th.Path = root.Inline.Path
				switch {
				case root.Inline.To != nil:
					th.Line = *root.Inline.To
				case root.Inline.From != nil:
					th.Line = *root.Inline.From
				}
			}
			threads[root.ID] = th
			order = append(order, root.ID)
		}
		if !c.User.isBot() {
			th.IsBot = false
		}
		th.Comments = append(th.Comments, commentObservation(c))
	}
	out := make([]ports.SCMReviewThreadObservation, 0, len(order))
	for _, id := range order {
		out = append(out, *threads[id])
	}
	return out
}

type jsonRaw = json.RawMessage

func unmarshal(raw jsonRaw, v any) error { return json.Unmarshal(raw, v) }

// notFound maps a Bitbucket 404 to the provider-neutral ports.ErrSCMNotFound,
// keeping the original error in the chain; any other error passes through.
func notFound(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && !errors.Is(err, ports.ErrSCMNotFound) {
		return fmt.Errorf("%w: %w", ports.ErrSCMNotFound, err)
	}
	return err
}
