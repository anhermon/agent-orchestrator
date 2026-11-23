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
