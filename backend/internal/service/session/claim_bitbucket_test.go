package session

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestClaimPRBitbucket(t *testing.T) {
	const bbPR = "https://bitbucket.org/dealhubio/aura-sre/pull-requests/12"
	for _, tc := range []struct {
		name, origin, ref string
		wantErr           error
	}{
		{"ssh origin, pr url", "git@bitbucket.org:dealhubio/aura-sre.git", bbPR, nil},
		{"https origin, pr url", "https://bitbucket.org/dealhubio/aura-sre", bbPR + "/diff", nil},
		{"https origin, bare number", "https://bitbucket.org/dealhubio/aura-sre", "12", nil},
		{"github origin rejects bitbucket pr", "https://github.com/dealhubio/aura-sre.git", bbPR, ErrProjectMismatch},
		{"bitbucket origin rejects github pr", "git@bitbucket.org:dealhubio/aura-sre.git", "https://github.com/dealhubio/aura-sre/pull/12", ErrProjectMismatch},
		{"other bitbucket repo", "git@bitbucket.org:dealhubio/other.git", bbPR, ErrProjectMismatch},
		{"mixed-case pr url", "git@bitbucket.org:dealhubio/aura-sre.git", "https://bitbucket.org/DealHubIO/Aura-SRE/pull-requests/12", nil},
		{"ssh origin with default port", "ssh://git@bitbucket.org:22/dealhubio/aura-sre.git", bbPR, nil},
		{"https origin with default port", "https://bitbucket.org:443/dealhubio/aura-sre", bbPR, nil},
		{"ssh origin with bitbucket server port", "ssh://git@bitbucket.org:7999/dealhubio/aura-sre.git", bbPR, ErrInvalidPRRef},
		{"https origin with other port", "https://bitbucket.org:8443/dealhubio/aura-sre", bbPR, ErrInvalidPRRef},
		{"bad number", "git@bitbucket.org:dealhubio/aura-sre.git", "https://bitbucket.org/dealhubio/aura-sre/pull-requests/abc", ErrInvalidPRRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.sessions["demo-1"] = domain.SessionRecord{ID: "demo-1", ProjectID: "demo", Kind: domain.KindWorker, Metadata: domain.SessionMetadata{WorkspacePath: "/ws"}}
			st.projects["demo"] = domain.ProjectRecord{ID: "demo", RepoOriginURL: tc.origin}
			scm := &claimTargetSCM{}
			svc := NewWithDeps(Deps{Store: st, SCM: scm, PRClaimer: &fakePRClaimer{}})
			_, err := svc.ClaimPR(context.Background(), "demo-1", tc.ref, ClaimPROptions{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if len(scm.refs) != 1 {
					t.Fatalf("refs = %+v", scm.refs)
				}
				r := scm.refs[0]
				if r.Repo.Provider != "bitbucket" || r.Repo.Host != "bitbucket.org" || r.Repo.Repo != "dealhubio/aura-sre" || r.Number != 12 || r.URL != "https://bitbucket.org/dealhubio/aura-sre/pull-requests/12" {
					t.Fatalf("ref = %+v", r)
				}
			}
		})
	}
}

func TestScmRepoForClaimBitbucket(t *testing.T) {
	repo, err := scmRepoForClaim("https://bitbucket.org/dealhubio/aura-sre/pull-requests/12")
	if err != nil {
		t.Fatal(err)
	}
	want := ports.SCMRepo{Provider: "bitbucket", Host: "bitbucket.org", Owner: "dealhubio", Name: "aura-sre", Repo: "dealhubio/aura-sre"}
	if repo != want {
		t.Fatalf("repo = %+v, want %+v", repo, want)
	}
}

func TestScmRepoForClaimBitbucketLowercases(t *testing.T) {
	repo, err := scmRepoForClaim("https://bitbucket.org/DealHubIO/Aura-SRE/pull-requests/12")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Owner != "dealhubio" || repo.Name != "aura-sre" || repo.Repo != "dealhubio/aura-sre" {
		t.Fatalf("repo = %+v", repo)
	}
}

func TestSummarizePRKeepsBitbucketProvider(t *testing.T) {
	got := summarizePR(domain.PullRequest{Provider: "bitbucket", URL: "https://bitbucket.org/dealhubio/aura-sre/pull-requests/12", Number: 12}, nil, nil, nil, nil, true)
	if got.Provider != "bitbucket" {
		t.Fatalf("provider = %q", got.Provider)
	}
}

func TestPRHeadRefspecBitbucket(t *testing.T) {
	const prefix = "refs/ao/pr/12"
	for _, tc := range []struct {
		name string
		pr   domain.PullRequest
		want string
	}{
		{"github", domain.PullRequest{Provider: "github", Number: 12}, "+refs/pull/12/head:refs/ao/pr/12/head"},
		{"gitlab", domain.PullRequest{Provider: "gitlab", Number: 12}, "+refs/merge-requests/12/head:refs/ao/pr/12/head"},
		{"bitbucket uses source branch", domain.PullRequest{Provider: "bitbucket", Number: 12, SourceBranch: "feature/x"}, "+refs/heads/feature/x:refs/ao/pr/12/head"},
		{"bitbucket without source branch", domain.PullRequest{Provider: "bitbucket", Number: 12}, ""},
	} {
		if got := prHeadRefspec(tc.pr, prefix); got != tc.want {
			t.Fatalf("%s: refspec = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPullRequestAliasKeyBitbucket(t *testing.T) {
	const u = "https://bitbucket.org/DealHubIO/Aura-SRE/pull-requests/12"
	if got, want := pullRequestAliasKey(u, 12, "", "", ""), "bitbucket|bitbucket.org|dealhubio|aura-sre|12"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if got := pullRequestAliasKey(u, 13, "", "", ""); got != "" {
		t.Fatalf("mismatched number key = %q", got)
	}
}
