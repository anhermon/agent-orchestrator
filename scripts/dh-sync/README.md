# dh-sync: upstream sync job

Daily job that keeps the fork current with OrchestratorInc/agent-orchestrator.

1. Fast-forwards `origin/main` from `upstream/main` (never anything else touches `main`).
2. Merges upstream into `dh/main` on `sync/<date>` in a sibling worktree (`../agent-orchestrator-sync`).
3. On conflicts or red checks it runs the resolver (a Claude Code session, see `resolve-prompt.md`), then re-runs the checks itself.
4. Green: pushes `dh/main` (fast-forward only). Red: pushes only the sync branch, notifies Angel, exit 20.

It never pushes to `upstream`, never force-pushes, and never resolves a conflict on its own.
If `origin/dh/main` does not exist yet, it only mirrors `main` and logs a warning.

Exit codes: 0 landed or noop, 10 escalated and resolved, 20 unresolved, 30 precondition failure (lock held, main not fast-forwardable, push rejected).

Logs: `~/Library/Logs/ao-sync/sync.log` (one line per run) and `<timestamp>.log` (full output). `--debug` adds DEBUG lines.

Options: `sync.sh [--repo PATH] [--checks CMD] [--resolver CMD] [--notify CMD] [--dry-run] [--debug]`.

Checks (`checks.sh`): gofmt, go build, go vet, go test -race, golangci-lint (prints SKIPPED if not installed), frontend `npm ci`, typecheck, vitest.

Install: `scripts/dh-sync/install.sh` (copies the scripts to `~/Library/Application Support/ao-sync`, installs the LaunchAgent, daily 07:00). Remove: `scripts/dh-sync/install.sh --uninstall`.

Tests: `scripts/dh-sync/test/sync_test.sh` (temporary bare repos, no network).
