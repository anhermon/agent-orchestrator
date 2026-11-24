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

### What it does
- Fast-forwards `origin/main` from `upstream/main`, then merges it into `dh/main` in a sibling worktree (`../agent-orchestrator-sync`) and runs the checks (`checks.sh`). A red day leaves `main` ahead of `dh/main`; the next run recovers.
- Clean merge and green checks: fast-forwards `origin/dh/main` (exit 0). Nothing new upstream is also exit 0.
- Conflicts, or red checks after a clean merge: a restricted Claude Code session (file tools only, no Bash) works in the worktree. The script then verifies the result (no conflict markers, test-weakening guard, no resolver-authored build/test config, checks green) and pushes it only to `sync/upstream-<UTC yyyymmdd-HHMM>`, never to `dh/main`. A resolved merge needs a verified land into `dh/main` (validator-agent PASS, then the harness `land --into dh/main`); exit 10.
- It never pushes to `upstream`, never force-pushes and never touches the live checkout.
- Residual gaps in the test-weakening guard (for example `if got != want {` assertions, early returns, `__tests__` naming) are listed in the README. Review the diff of any `sync/upstream-*` branch before landing.

### Exit codes
- 0: landed on `dh/main`, or nothing to do (including "`origin/dh/main` does not exist, only `main` was mirrored"). Some exit-0 runs still notify (see below).
- 2: bad command-line usage.
- 10: resolved and pushed to a `sync/upstream-*` branch, awaiting land.
- 20: unresolved: conflict left, resolver made no changes or moved HEAD, conflict markers remain, test-weakening guard tripped, checks still red after the resolver, `claude` rejected its arguments, or resolver-authored build/test config ("NEEDS HUMAN", checks not run, branch pushed for review). `dh/main` is untouched.
- 30: precondition failure: lock held, not a git repo, `origin`/`upstream` remote missing or the same URL, fetch failed, `main` not fast-forwardable, a push rejected or not matching, worktree could not be created or HEAD moved.

### Notifications
Every notification is appended to `status.log` and shown as a macOS banner; a Slack DM is best effort. Slack "sent" is the Claude session's own SLACK_SENT confirmation and cannot be verified; the banner plus `status.log` are the reliable record (`sync.log` records `notify=sent|sent-no-banner|local-only|osascript-failed`).

| Message | Exit | Meaning | What to do |
| --- | --- | --- | --- |
| `upstream <sha> merged on origin/sync/upstream-<ts> ... land the branch` | 10 | Resolver result passed all checks, `dh/main` not updated | Review the branch and `<ts>-SUMMARY.md`, then land it into `dh/main` with the harness |
| `needs attention: ... unresolved (<reason>); <where>` | 20 | Nothing landed; conflict diff kept in the log dir, or a red clean merge pushed to the sync branch for inspection | Read the log and fix or merge by hand on a feature branch |
| `NEEDS HUMAN: resolver changed build/test config` | 20 | Checks were not run; branch pushed to `sync/upstream-*` | Review the config change by hand before running anything from that branch |
| `precondition failed: <reason>` | 30 | A precondition failed (fetch, push rejection, worktree, HEAD moved, ...); nothing landed | Read the log, fix the named condition; the next run retries |
| `another run holds the lock` | 30 | A previous run is alive or stuck. Once per day | Check the process and the lock file named in the message |
| `<repo> is not a git repository`, `remote <name> is not configured`, `origin and upstream point at the same URL` | 30 | Misconfiguration. Once per day each | Fix the repo path or remotes |
| `upstream <sha> landed on dh/main with checks skipped: ...` | 0 | Landed, but a check (normally golangci-lint) was skipped. Once per day | Install the missing tool |
| `origin/dh/main does not exist; only main was mirrored` | 0 | Nothing merged. Once per day | Create `dh/main` |

### Enable the daily job
1. Run `scripts/dh-sync/install.sh`. It copies the scripts to `~/Library/Application Support/ao-sync` and installs the LaunchAgent `io.anhermon.ao-sync` (daily 07:00). Re-running it redeploys script changes.
2. Verify: `launchctl print gui/$(id -u)/io.anhermon.ao-sync`. The job runs only while the user is logged in.
3. The repo path is hard-coded: `REPO=/Users/angelhermon/workspace/agent-orchestrator` at the top of `scripts/dh-sync/sync.sh`. The LaunchAgent passes no `--repo`; change the variable and re-run `install.sh` (manual runs can pass `--repo PATH`).
4. PATH: launchd gives the job only the PATH set in the plist (`~/.local/bin`, `~/go/bin`, Homebrew, `/usr/local/bin`, system dirs). `claude`, `git`, `go` and `npm` must be reachable there, or the job fails (a missing `golangci-lint` is only SKIPPED).
5. Manual run: `~/Library/Application Support/ao-sync/sync.sh [--dry-run] [--debug]`.
6. Remove: `scripts/dh-sync/install.sh --uninstall` removes the LaunchAgent and the copied scripts. Logs stay.

### Logs
`~/Library/Logs/ao-sync/`: `sync.log` (one line per run, with `skipped=<items>`), `<timestamp>.log` (full output), `<timestamp>-SUMMARY.md`, `<timestamp>-conflict.diff`, `status.log` (every notification), `launchd.out.log` and `launchd.err.log` (launchd stdout and stderr). Run files older than 30 days are pruned.

### golangci-lint
If it is not installed, `checks.sh` prints `SKIPPED` and the result is "green with skips: golangci-lint", recorded as `skipped=golangci-lint` in `sync.log`. It is never reported as plain green.

## Upstream files we modify
Production changes first (by task), then test-only rows.
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
| backend/internal/service/session/pr_files.go | ensurePRRevisionObjects, prHeadRefspec | Bitbucket has no refs/pull/N/head; fetch the PR source branch instead | 5 |
| backend/internal/gitdefault/resolver.go | Resolve, Inspect, resolveRemoteless, localDefaultBranch | Repos with no remote at all resolve the default locally (init.defaultBranch if it exists, single main/master, then checked-out branch) and record it best-effort; a failed write never fails resolution; repos with a remote are unchanged | 6 |
| backend/internal/adapters/workspace/gitworktree/workspace_integration_test.go | TestWorkspaceIntegrationRemotelessRepoWithConfiguredBranchCreatesWorktree | Test only: remoteless repo with a configured default branch creates and destroys a worktree; no production change needed | 6 |
| backend/internal/observe/scm/observer_test.go | TestPoll_RemotelessProjectIsSilentlySkipped | Test only: remoteless project causes no provider calls and no Info+ logs; no production change needed | 6 |

### Known GitHub-only (left untouched)
reviewgateway/gateway.go, cli/pr_ref.go, cli/triage_*, githubpat, importer, telemetry, copilot reviewer, trackerintake (beyond the empty-provider guard), cli/spawn.go --tracker-provider, service/session/claim_pr.go sameRenamedGitHubRepo (GitHub rename lookup only), lifecycle/review_done.go (deliberately skipped although spec 3b lists it: the review anchor is a GitHub-only feature; Bitbucket gets the "Review ID" fallback), service/session/pr_summary.go (the "github" default stays; the Bitbucket observer sets Provider).

## Bitbucket setup
The Bitbucket Cloud provider is configured by credentials only; there is no config file entry.
- Create an Atlassian API token with scopes. The scopes below are derived from the endpoints the adapter calls and are unverified against live Bitbucket (verified in Task 9). If a call returns 403, check scopes first. Believed needed: `read:pullrequest:bitbucket` and `write:pullrequest:bitbucket` (list/get PRs, comments, diffstat, merge, approve, request changes, decline), `read:repository:bitbucket` (repository and commit statuses), `read:pipeline:bitbucket` (pipeline steps behind a failing check). The per-scope mapping is an inference. Authentication is HTTP basic: username is the Atlassian account email, password is the API token.
- Environment variables (read as pairs, first complete pair wins): `AO_BITBUCKET_USERNAME` with `AO_BITBUCKET_API_TOKEN`, then fallback `BITBUCKET_USERNAME` with `BITBUCKET_API_TOKEN`. A username from one pair is never combined with a token from the other.
- macOS Keychain (tried after env): service `ao-bitbucket`, account = the username. Store the token with `security add-generic-password -s ao-bitbucket -a <username> -w` (the tool prompts for the password; not verified here). The username must still be set as `AO_BITBUCKET_USERNAME` (or `BITBUCKET_USERNAME`) in the daemon's own environment. A daemon started from the GUI or launchd does not see shell exports: start the daemon from a shell where it is exported, or set it for the GUI session with `launchctl setenv AO_BITBUCKET_USERNAME <email>` and restart the daemon.
- Nothing configured is silent: the daemon logs `scm provider not configured` at Debug and the Bitbucket provider is not registered. A half-set configuration (a username without a token, a token without a username, or mismatched `AO_`/plain names) is not silent: it fails the startup probe and logs the Warn `scm provider disabled: setup failed`. A rejected token (401) logs the Warn `no usable token`.

## Repositories with no remote
A repo with no remote at all (for example a local-only home repo) has no remote HEAD to read, so the default branch is resolved locally, in this order:
1. A branch already recorded in git config `ao.defaultBranch` (repo-local) is used as is. It is not re-derived.
2. `init.defaultBranch`, if that branch exists.
3. `main` or `master`, if exactly one of them exists.
4. The currently checked-out branch (it must exist; a detached HEAD does not count).
The choice is recorded in `ao.defaultBranch` so later checkouts do not move it. Recording is best effort; a failed write never fails resolution. Repos with a remote are unchanged.
Failure cases: if the recorded `ao.defaultBranch` names an invalid or deleted branch, resolution fails as unresolved rather than falling through; if nothing is recorded and steps 2 to 4 all fail (for example a detached HEAD with no `main`, `master` or `init.defaultBranch` branch), it is also unresolved. Fix or override: `git -C <repo> config --local ao.defaultBranch <branch>` (the branch must exist).

## Known limitations
- Bitbucket Cloud only (`bitbucket.org`, REST 2.0). No Bitbucket Server or Data Center, and no custom ports: a bitbucket remote URL with a port other than the scheme default is rejected.
- Diffs: Bitbucket PR diffs from a fork source repository stay unavailable locally (the source branch is fetched from origin only), as does a PR whose source branch was already deleted.
- `RequestReview` is unsupported (Bitbucket adds reviewers by account UUID); it returns `ErrSCMUnsupported`. So are merge queue, reopening a declined PR, draft state changes and comment attachments.
- Failed-check log tails work only for Bitbucket Pipelines; a status from any other CI returns `ErrSCMUnsupported`.
- Merge is asynchronous: when Bitbucket accepts a merge but the PR is not `MERGED` within the poll budget (10 seconds), the adapter returns `ErrMergePending`. Callers see an ordinary error; nothing is recorded as merged. The SCM observer later sees state `MERGED` through normal polling. Do not retry blindly; check the PR first.
- No Bitbucket tracker: issue-tracker features do not apply to `bitbucket.org` repos (the trackers are skipped).
- GitHub-only code paths left untouched: see "Known GitHub-only (left untouched)" above, including `sameRenamedGitHubRepo` (GitHub rename lookup) and the `lifecycle/review_done.go` review-anchor wording (Bitbucket gets the "Review ID" fallback).
