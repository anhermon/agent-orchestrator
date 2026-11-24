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
Daily job in `scripts/dh-sync/` (full detail, safety model and residual guard gaps: `scripts/dh-sync/README.md`).
- Fast-forwards `origin/main` from `upstream/main`, then merges it into `dh/main` in a sibling worktree (`../agent-orchestrator-sync`) and runs the checks (`checks.sh`).
- Clean merge and green checks: fast-forwards `origin/dh/main` (exit 0). Nothing new upstream is also exit 0.
- Conflicts: a restricted Claude Code session (file tools only, no Bash) resolves them in the worktree. The script verifies the result (no conflict markers, test-weakening guard, checks) and pushes it only to `sync/upstream-<UTC yyyymmdd-HHMM>`, never to `dh/main`. A resolved merge needs a verified land into `dh/main` (validator-agent PASS, then the harness `land --into dh/main`); exit 10.
- Exit codes: 0 landed or nothing to do, 10 resolved and pushed to a `sync/upstream-*` branch awaiting land, 20 unresolved or needs a human (config-file guard tripped, checks not run), 30 precondition failure (lock held, `main` not fast-forwardable, push rejected, fetch failed).
- Install: `scripts/dh-sync/install.sh` (copies the scripts to `~/Library/Application Support/ao-sync` and installs a LaunchAgent, daily 07:00). Remove: `scripts/dh-sync/install.sh --uninstall`. Manual run: `sync.sh [--repo PATH] [--dry-run] [--debug]`.
- Logs: `~/Library/Logs/ao-sync/` (`sync.log` one line per run, `<timestamp>.log` full output, `<timestamp>-SUMMARY.md`, `<timestamp>-conflict.diff`). Every notification is also appended to `status.log` there. Files older than 30 days are pruned.
- golangci-lint: if it is not installed, `checks.sh` prints `SKIPPED` and the result is "green with skips: golangci-lint", recorded as `skipped=golangci-lint` in `sync.log`. It is never reported as plain green, and a clean landing with skipped checks still notifies (once per day).
- Residual gaps in the test-weakening guard (for example `if got != want {` style assertions, early returns, `__tests__` naming) are listed in the README. Review the diff of any `sync/upstream-*` branch before landing.

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
| backend/internal/gitdefault/resolver.go | Resolve, Inspect, resolveRemoteless, localDefaultBranch | Repos with no remote at all resolve the default locally (init.defaultBranch if it exists, single main/master, then checked-out branch) and record it best-effort; a failed write never fails resolution; repos with a remote are unchanged | 6 |

### Known GitHub-only (left untouched)
reviewgateway/gateway.go, cli/pr_ref.go, cli/triage_*, githubpat, importer, telemetry, copilot reviewer, trackerintake (beyond the empty-provider guard), cli/spawn.go --tracker-provider, service/session/claim_pr.go sameRenamedGitHubRepo (GitHub rename lookup only), lifecycle/review_done.go (deliberately skipped although spec 3b lists it: the review anchor is a GitHub-only feature; Bitbucket gets the "Review ID" fallback), service/session/pr_summary.go (the "github" default stays; the Bitbucket observer sets Provider).

## Bitbucket setup
The Bitbucket Cloud provider is configured by credentials only; there is no config file entry.
- Create an Atlassian API token (Bitbucket Cloud, "API token with scopes"). Scopes the adapter needs: `read:pullrequest:bitbucket` and `write:pullrequest:bitbucket` (list/get PRs, comments, diffstat, merge, approve, request changes, decline), `read:repository:bitbucket` (repository and commit statuses), `read:pipeline:bitbucket` (pipeline steps behind a failing check). Authentication is HTTP basic: username is the Atlassian account email, password is the API token.
- Environment variables (read as pairs, first complete pair wins): `AO_BITBUCKET_USERNAME` with `AO_BITBUCKET_API_TOKEN`, then fallback `BITBUCKET_USERNAME` with `BITBUCKET_API_TOKEN`. A username from one pair is never combined with a token from the other.
- macOS Keychain (tried after env): service `ao-bitbucket`, account = the username. The username must still come from `AO_BITBUCKET_USERNAME` / `BITBUCKET_USERNAME` in the daemon environment. Store the token with `security add-generic-password -s ao-bitbucket -a <username> -w` (prompts for the token).
- No credentials configured is silent: the daemon logs `scm provider not configured` at Debug and the Bitbucket provider is simply not registered. A rejected token (401) or other setup failure logs a Warn (`scm provider disabled`).

## Repositories with no remote
A repo with no remote at all (for example a local-only home repo) has no remote HEAD to read, so the default branch is resolved locally, in this order:
1. A branch already recorded in git config `ao.defaultBranch` (repo-local), which always wins.
2. `init.defaultBranch`, if that branch exists.
3. `main` or `master`, if exactly one of them exists.
4. The currently checked-out branch.
The choice is recorded in `ao.defaultBranch` so later checkouts do not move it. Recording is best effort; a failed write never fails resolution. Repos with a remote are unchanged. Override at any time: `git -C <repo> config --local ao.defaultBranch <branch>`.

## Known limitations
- Bitbucket Cloud only (`bitbucket.org`, REST 2.0). No Bitbucket Server or Data Center, and no custom ports: a bitbucket remote URL with a port other than the scheme default is rejected.
- Diffs: Bitbucket PR diffs from a fork source repository stay unavailable locally (the source branch is fetched from origin only), as does a PR whose source branch was already deleted.
- `RequestReview` is unsupported (Bitbucket adds reviewers by account UUID); it returns `ErrSCMUnsupported`. So are merge queue, reopening a declined PR, draft state changes and comment attachments.
- Merge is asynchronous: when Bitbucket accepts a merge but the PR is not yet `MERGED` after the poll budget, the adapter returns `ErrMergePending` and the SCM observer reports the merge only when it sees state `MERGED`. Treat pending as in progress, not as failure.
- No Bitbucket tracker: issue-tracker features do not apply to `bitbucket.org` repos (the trackers are skipped).
- GitHub-only code paths left untouched: see "Known GitHub-only (left untouched)" above, including `sameRenamedGitHubRepo` (GitHub rename lookup) and the `lifecycle/review_done.go` review-anchor wording (Bitbucket gets the "Review ID" fallback).
