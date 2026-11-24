# dh-sync: upstream sync job

Daily job that keeps the fork current with OrchestratorInc/agent-orchestrator.

1. Fast-forwards `origin/main` from `upstream/main`. A red day leaves `main` ahead of `dh/main`; the next run recovers.
2. Merges upstream into `dh/main` on a local branch in a sibling worktree (`../agent-orchestrator-sync`).
3. Conflict-free merge with green checks: fast-forwards `origin/dh/main` (exit 0).
4. Conflicts or red checks: escalation (below). A resolved merge never reaches `dh/main` automatically.

It never pushes to `upstream`, never force-pushes, never touches the live checkout, and never resolves a conflict by itself.
If `origin/dh/main` does not exist yet, it only mirrors `main`, logs a warning and notifies (once per day).

## Escalation and safety model

- The resolver is a Claude Code session started in the worktree with an explicit tool set: `--tools Read,Edit,Write,Grep,Glob,Bash`, allowed `Read,Edit,Write,Grep,Glob,Bash(git diff:*),Bash(git status:*)`, denied web tools and git add/commit/push/checkout/switch/reset/merge/remote, `--strict-mcp-config`, `--setting-sources local` (upstream-controlled project settings are ignored), `--permission-mode acceptEdits` (edits outside the worktree are not auto-approved and are denied in `-p` mode). No bypass flags. The prompt (`resolve-prompt.md`) is a template with three controlled values (SHA, branch, ticket); upstream commit messages and file contents are never put in it. Residual risk: the session reads untrusted upstream files (prompt injection); containment is the tool restriction plus the script's verification below.
- The script then verifies: HEAD attached to the sync branch, merge still in progress (resolver did not commit), no conflict markers, **test-weakening guard**, checks green. Then the script commits and pushes only to `sync/upstream-<UTC yyyymmdd-HHMM>` (explicit `HEAD:refs/heads/<branch>`, verified with ls-remote) and notifies. Exit 10: land that branch with the harness.
- Guard heuristic (compares the staged result with both parents, so one-sided upstream or fork changes are legitimate): fails if a test file (`*_test.go`, `*.test.ts|tsx|js|jsx|mjs`, `*.spec.*`) present in both parents is deleted, a skip/only marker (`t.Skip`, `.skip(`, `xit(`, `xdescribe(`, `xtest(`, `.only(`) is added, or an assertion line (`expect(`, `assert*(`, `require.X(`, `t.Error/Fatal`) is removed. It does not cover renamed files, paths with spaces, or edits to package.json scripts.
- Clean merge with red checks and an unresolved resolver: the committed merge is pushed to its sync branch for inspection; dh/main is untouched.
- Unresolved conflict: nothing is pushed; the conflict diff and SYNC_SUMMARY.md (if any) are kept in the log directory.

## Operation

Exit codes: 0 landed or noop, 10 resolved and pushed to a sync branch (awaiting land), 20 unresolved, 30 precondition failure (lock held, main not fast-forwardable, push rejected, fetch failed).
The lock holds the PID (liveness check), INT/TERM/EXIT clean up the lock, worktree and local branch, a leftover worktree from a killed run is removed, the resolver is limited to 45 min (`AO_SYNC_RESOLVER_TIMEOUT`, seconds) and the checks to 60 min (`AO_SYNC_CHECKS_TIMEOUT`).

Logs: `~/Library/Logs/ao-sync/sync.log` (one line per run, includes `skipped=<items>` when checks skipped something), `<timestamp>.log` (full output), `<timestamp>-SUMMARY.md`, `<timestamp>-conflict.diff`, `status.log` (every notification). Files older than 30 days are pruned. `--debug` adds DEBUG lines.

Notifications (`notify.sh`): always a macOS notification plus a line in `status.log`. A Slack DM through `claude -p` is best effort and reported as `notify=sent` only when the session prints SLACK_SENT (the model's own confirmation); otherwise `notify=local-only`. The Slack tool name is `AO_SYNC_SLACK_TOOL` (default `mcp__claude_ai_Slack__slack_send_message`, unverified). Lock-held and missing-dh/main notifications are limited to once per day.

Options: `sync.sh [--repo PATH] [--checks PROGRAM] [--resolver PROGRAM] [--notify PROGRAM] [--dry-run] [--debug]` (PROGRAM is a single executable path; paths with spaces are fine).

Checks (`checks.sh`): gofmt, go build, go vet, go test -race, golangci-lint, frontend `npm ci`, typecheck, vitest. A missing golangci-lint is printed as SKIPPED and the result is "green with skips: golangci-lint", never plain green.

Install: `scripts/dh-sync/install.sh` (copies the scripts to `~/Library/Application Support/ao-sync`, installs the LaunchAgent, daily 07:00). Remove: `scripts/dh-sync/install.sh --uninstall`.

Tests: `scripts/dh-sync/test/sync_test.sh` (temporary bare repos, fake claude/osascript on PATH, no network).
