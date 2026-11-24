# DealHub downstream fork

## Purpose
Permanent downstream fork of OrchestratorInc/agent-orchestrator (Go backend and Electron frontend) that adds Bitbucket Cloud support. Ticket: INT-20261123-ao-bitbucket.

## Branch model
- `main`: pristine mirror of upstream `main`. Agent-protected: agents never commit or push here. Only the sync job updates it (fast-forward).
- `dh/main`: default branch of this fork. Upstream plus our commits. Upstream changes are merged in from `main`.
- `feature/<TICKET>-<slug>`: work branches, cut from `dh/main`. Work lands on `dh/main` only via `python3 ~/.claude/tools/harness/harness.py land <TICKET> --into dh/main`, after a validator-agent PASS. No other route (no raw merge or push to `dh/main`).

## Never upstream
Never push to `upstream`, and never open PRs or issues against OrchestratorInc/agent-orchestrator. The `upstream` remote has its push URL set to `DISABLE`.

## Sync job
TBD: location of the job that fast-forwards `main` from upstream and merges it into `dh/main`.

## Upstream files we modify
| File | Function | Reason | Task |
| --- | --- | --- | --- |
| backend/internal/daemon/scm_wiring.go | startSCMObserver, newMultiSCMProvider, newMultiSCMMerger | Register the bitbucket provider/merger after the gitlab block (one block per site) | 4 |
| backend/internal/daemon/scm_wiring.go | logSCMProviderDisabled | Treat bitbucket ErrNoCredentials/ErrAuthFailed as the "no usable token" warning | 4 |
| backend/internal/domain/repository_identity.go | RepositoryProvider, ParseRepositoryIdentity | bitbucket.org maps to provider "bitbucket"; namespace must be exactly workspace/slug | 5 |
| backend/internal/domain/change_request.go | ParseChangeRequestURL | Parse /ws/slug/pull-requests/N (optional /diff, /commits, /overview) as provider "bitbucket" | 5 |
| backend/internal/domain/tracker.go | InferTrackerProvider | bitbucket.org returns empty provider (no tracker) instead of falling through to gitlab | 5 |
| backend/internal/observe/trackerintake/observer.go | trackerRepo | Empty inferred provider means unsupported tracker; skip the repo | 5 |
| backend/internal/service/session/claim_pr.go | prURLFromParts, parsePRURL, parseBitbucketPRURL | Accept and build Bitbucket PR URLs so a session can claim its PR | 5 |
| backend/internal/service/session/pr_alias.go | pullRequestAliasKey, bitbucketPRAliasKey | Alias key bitbucket|host|ws|slug|n | 5 |
| backend/internal/service/review/review.go | providerFromPRURL | /pull-requests/ URLs resolve to provider "bitbucket" | 5 |
| backend/internal/adapters/workspace/gitworktree/workspace_integration_test.go | TestWorkspaceIntegrationRemotelessRepoWithConfiguredBranchCreatesWorktree | Test only: remoteless repo with a configured default branch creates and destroys a worktree; no production change needed | 6 |
| backend/internal/observe/scm/observer_test.go | TestPoll_RemotelessProjectIsSilentlySkipped | Test only: remoteless project causes no provider calls and no Info+ logs; no production change needed | 6 |
| backend/internal/service/session/pr_files.go | ensurePRRevisionObjects, prHeadRefspec | Bitbucket has no refs/pull/N/head; fetch the PR source branch instead | 5 |
| backend/internal/gitdefault/resolver.go | Resolve, Inspect, resolveRemoteless, localDefaultBranch | Repos with no remote at all resolve the default locally (checked-out branch, init.defaultBranch, single main/master) and record it; repos with a remote are unchanged | 6 |

### Known GitHub-only (left untouched)
reviewgateway/gateway.go, cli/pr_ref.go, cli/triage_*, githubpat, importer, telemetry, copilot reviewer, trackerintake (beyond the empty-provider guard), cli/spawn.go --tracker-provider, service/session/claim_pr.go sameRenamedGitHubRepo (GitHub rename lookup only), lifecycle/review_done.go (deliberately skipped although spec 3b lists it: the review anchor is a GitHub-only feature; Bitbucket gets the "Review ID" fallback), service/session/pr_summary.go (the "github" default stays; the Bitbucket observer sets Provider).


### Known limitations
- Bitbucket PR diffs from a fork source repository stay unavailable locally (the source branch is fetched from origin only), as does a PR whose source branch was already deleted.
