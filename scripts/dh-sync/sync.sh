#!/usr/bin/env bash
# Upstream sync job for the DealHub fork of agent-orchestrator.
#
# 1. Fast-forwards origin/main from upstream/main.
# 2. Merges upstream into dh/main on a local sync branch in a sibling worktree.
#    Conflict-free merge with green checks: fast-forwards origin/dh/main (exit 0).
# 3. Conflicts or red checks: a resolver (a restricted Claude Code session, edit-only,
#    no git, no network) edits files in the worktree. THIS SCRIPT then verifies (no
#    conflict markers, test-weakening guard, checks green), commits, and pushes ONLY to
#    sync/upstream-<UTC yyyymmdd-HHMM>. dh/main is never changed by a resolved merge:
#    a human/session lands the branch (exit 10).
# Never pushes to upstream, never force-pushes, never touches the live checkout.
#
# Exit codes: 0 landed or nothing to do, 10 resolved and pushed to a sync branch awaiting
#             land, 20 unresolved or needs a human (dh/main untouched, notified), 30 precondition failure.
# Bash 3.2 compatible (macOS default).
# shellcheck disable=SC2329 # functions are invoked indirectly (traps, run_limited)
set -u
umask 077

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO=/Users/angelhermon/workspace/agent-orchestrator
CHECKS=("$SCRIPT_DIR/checks.sh")
NOTIFY=("$SCRIPT_DIR/notify.sh")
RESOLVER_PROGRAM="" # empty: built-in restricted claude resolver
DRY_RUN=0
DEBUG=0
TICKET=INT-20261123-ao-bitbucket
RESOLVER_TIMEOUT="${AO_SYNC_RESOLVER_TIMEOUT:-2700}"
CHECKS_TIMEOUT="${AO_SYNC_CHECKS_TIMEOUT:-3600}"
LOG_RETENTION_DAYS=30

usage() {
  echo "usage: sync.sh [--repo PATH] [--checks PROGRAM] [--resolver PROGRAM] [--notify PROGRAM] [--dry-run] [--debug]"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --checks) CHECKS=("$2"); shift 2 ;;
    --resolver) RESOLVER_PROGRAM="$2"; shift 2 ;;
    --notify) NOTIFY=("$2"); shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --debug) DEBUG=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

LOG_DIR="${AO_SYNC_LOG_DIR:-$HOME/Library/Logs/ao-sync}"
mkdir -p "$LOG_DIR"
chmod 700 "$LOG_DIR"
STAMP="$(date +%Y%m%dT%H%M%S)"
RUN_LOG="$LOG_DIR/$STAMP.log"
exec 3>&1
exec >>"$RUN_LOG" 2>&1

log() { echo "$(date +%FT%T) $1 $2"; }
debug() { [ "$DEBUG" = 1 ] && log DEBUG "$1"; return 0; }
warn() { log WARN "$1"; }
err() { log ERROR "$1"; }

UPSTREAM_SHA=unknown
BRANCH=-
LOCK=""
OWN_LOCK=0
WT=""
WT_CREATED=0
NOTIFY_STATUS=""
SKIPPED=""
FINISHED=0
CHILD=""
WATCHER=""
CHECKS_OUT=""
RESOLVER_OUT=""
PRE_INDEX=""
DH_BASE=""
MERGE_COMMIT=""
CONFLICT=0

git_repo() { git -C "$REPO" "$@"; }

# True only if git registers exactly this path as a worktree of this repo.
wt_registered() { # wt_registered <path>
  local real
  real="$(cd "$(dirname "$1")" 2>/dev/null && pwd -P)/$(basename "$1")"
  git_repo worktree list --porcelain 2>/dev/null | grep -Fxq "worktree $real"
}

# Pushes from the worktree. Refspec must be explicit, never forced. Remote is always origin.
push_wt() { # push_wt <refspec>
  case "$1" in
    +* | "") err "refusing refspec '$1'"; return 1 ;;
  esac
  git -C "$WT" push origin "$1"
}

# Timed jobs run under set -m, so each is its own process group; kill the whole group.
kill_tree() { # kill_tree <pid>
  kill -TERM -- "-$1" 2>/dev/null
  kill -TERM "$1" 2>/dev/null
  return 0
}

# Runs a command in its own process group with a time limit; signals interrupt the wait.
# On timeout, completion or signal the whole group (grandchildren included) is killed.
# The watcher PID is global so signal and exit handlers can reap it; the watcher only signals a
# child group that is still alive, and the main flow escalates to SIGKILL (bounded) after the
# group leader exits so TERM-ignoring grandchildren cannot survive.
run_limited() { # run_limited <seconds> <command...>
  local secs="$1" rc child
  shift
  set -m
  "$@" &
  child=$!
  CHILD=$child
  (
    sleep "$secs"
    kill -0 -- "-$child" 2>/dev/null || exit 0
    log WARN "timeout after ${secs}s: $1"
    kill -TERM -- "-$child" 2>/dev/null
    sleep 10
    kill -0 -- "-$child" 2>/dev/null && kill -KILL -- "-$child" 2>/dev/null
    exit 0
  ) &
  WATCHER=$!
  set +m
  wait "$child"
  rc=$?
  kill_tree "$WATCHER"
  wait "$WATCHER" 2>/dev/null
  WATCHER=""
  kill -TERM -- "-$child" 2>/dev/null
  for _ in 1 2 3 4 5 6; do
    kill -0 -- "-$child" 2>/dev/null || break
    sleep 0.5
  done
  kill -0 -- "-$child" 2>/dev/null && kill -KILL -- "-$child" 2>/dev/null
  CHILD=""
  return "$rc"
}

cleanup() {
  [ -n "$WATCHER" ] && kill_tree "$WATCHER"
  [ -n "$PRE_INDEX" ] && rm -f "$PRE_INDEX"
  [ -n "$CHECKS_OUT" ] && rm -f "$CHECKS_OUT"
  [ -n "$RESOLVER_OUT" ] && rm -f "$RESOLVER_OUT"
  if [ "$WT_CREATED" = 1 ] && wt_registered "$WT"; then
    git_repo worktree remove --force "$WT" >/dev/null 2>&1 || rm -rf "$WT"
    git_repo worktree prune
  fi
  if [ "$WT_CREATED" = 1 ] && [ "$BRANCH" != "-" ]; then
    git_repo branch -D "$BRANCH" >/dev/null 2>&1
  fi
  if [ "$OWN_LOCK" = 1 ]; then
    rm -rf "$LOCK"
  fi
  return 0
}

write_line() { # write_line <result>
  local line
  line="$(date +%FT%T%z) upstream=${UPSTREAM_SHA:0:12} result=$1 branch=$BRANCH${SKIPPED:+ skipped=$SKIPPED}${NOTIFY_STATUS:+ $NOTIFY_STATUS}"
  echo "$line" >>"$LOG_DIR/sync.log"
  echo "$line" >&3
}

on_exit() {
  if [ "$FINISHED" = 0 ]; then
    write_line aborted
  fi
  cleanup
}
on_signal() { # on_signal <exit code>
  warn "received signal, stopping"
  [ -n "$WATCHER" ] && kill_tree "$WATCHER"
  if [ -n "$CHILD" ]; then
    kill_tree "$CHILD"
    # Bounded KILL escalation, as in run_limited: a TERM-ignoring grandchild must not outlive us.
    for _ in 1 2 3 4 5 6; do
      kill -0 -- "-$CHILD" 2>/dev/null || break
      sleep 0.5
    done
    kill -0 -- "-$CHILD" 2>/dev/null && kill -KILL -- "-$CHILD" 2>/dev/null
  fi
  exit "$1"
}
trap on_exit EXIT
trap 'on_signal 143' TERM
trap 'on_signal 130' INT

finish() { # finish <result> <exit code>
  FINISHED=1
  write_line "$1"
  exit "$2"
}

notify() {
  local out
  out="$("${NOTIFY[@]}" "$1" 2>&1 </dev/null)"
  debug "notify output: $out"
  NOTIFY_STATUS="$(grep -o 'notify=[a-z-]*' <<<"$out" | tail -n 1)"
}

# Notifies at most once per UTC day per key (keeps a stuck state from spamming).
notify_once() { # notify_once <key> <text>
  local marker
  marker="$LOG_DIR/.notified-$1-$(date -u +%F)"
  if [ -e "$marker" ]; then
    debug "already notified today: $1"
    return 0
  fi
  : >"$marker"
  notify "$2"
}

precondition() { # precondition <message>
  err "$1"
  notify "AO sync: precondition failed: $1 (log: $RUN_LOG)"
  finish precondition 30
}

prune_logs() {
  find "$LOG_DIR" -maxdepth 1 -type f \( -name '2*' -o -name '.notified-*' \) -mtime +"$LOG_RETENTION_DAYS" -delete 2>/dev/null
  return 0
}
prune_logs

if ! git_repo rev-parse --git-dir >/dev/null 2>&1; then
  err "not a git repo: $REPO"
  notify_once not-a-repo "AO sync: $REPO is not a git repository"
  finish precondition 30
fi

norm_url() { sed -e 's|/*$||' -e 's|\.git$||' <<<"$1"; }
for remote in origin upstream; do
  if [ -z "$(git_repo remote get-url "$remote" 2>/dev/null)" ]; then
    err "remote $remote is not configured"
    notify_once "no-remote-$remote" "AO sync: remote $remote is not configured in $REPO"
    finish precondition 30
  fi
done
if [ "$(norm_url "$(git_repo remote get-url origin)")" = "$(norm_url "$(git_repo remote get-url upstream)")" ]; then
  err "origin and upstream URLs are identical; refusing to run"
  notify_once same-remotes "AO sync: origin and upstream point at the same URL; refusing to run"
  finish precondition 30
fi

# Lock: mkdir is atomic, the PID inside allows a liveness check.
LOCK="$(git_repo rev-parse --path-format=absolute --git-common-dir)/ao-sync.lock"
acquire_lock() {
  local pid
  if mkdir "$LOCK" 2>/dev/null; then
    echo $$ >"$LOCK/pid"
    OWN_LOCK=1
    return 0
  fi
  pid="$(cat "$LOCK/pid" 2>/dev/null)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    return 1
  fi
  if [ -z "$pid" ] && [ -z "$(find "$LOCK" -maxdepth 0 -mmin +360)" ]; then
    return 1 # no pid recorded yet and younger than 6h: assume another run is starting
  fi
  warn "removing stale lock $LOCK (pid=${pid:-none})"
  rm -rf "$LOCK"
  if mkdir "$LOCK" 2>/dev/null; then
    echo $$ >"$LOCK/pid"
    OWN_LOCK=1
    return 0
  fi
  return 1
}
if ! acquire_lock; then
  err "lock held: $LOCK"
  notify_once lock-held "AO sync skipped: another run holds the lock ($LOCK)"
  finish precondition 30
fi

debug "fetching upstream and origin in $REPO"
git_repo fetch --quiet upstream || precondition "fetch upstream failed"
git_repo fetch --quiet --prune origin || precondition "fetch origin failed"

UPSTREAM_SHA="$(git_repo rev-parse upstream/main)"
ORIGIN_MAIN="$(git_repo rev-parse origin/main)"

if ! git_repo merge-base --is-ancestor "$ORIGIN_MAIN" "$UPSTREAM_SHA"; then
  precondition "origin/main is not an ancestor of upstream/main (main must only be fast-forwarded)"
fi

if [ "$DRY_RUN" = 1 ]; then
  log INFO "dry run: would sync upstream ${UPSTREAM_SHA:0:12}"
  finish noop 0
fi

# Mirror main (fast-forward only; a push without + is rejected otherwise).
if [ "$ORIGIN_MAIN" != "$UPSTREAM_SHA" ]; then
  debug "fast-forwarding origin/main to $UPSTREAM_SHA"
  git_repo push origin "$UPSTREAM_SHA:refs/heads/main" || precondition "push of origin/main was rejected"
fi

if ! git_repo rev-parse --verify --quiet origin/dh/main >/dev/null; then
  warn "origin/dh/main does not exist yet; only mirrored main"
  notify_once dh-main-missing "AO sync: origin/dh/main does not exist; only main was mirrored"
  finish noop 0
fi

DH_BASE="$(git_repo rev-parse origin/dh/main)"
if git_repo merge-base --is-ancestor "$UPSTREAM_SHA" "$DH_BASE"; then
  if [ "$ORIGIN_MAIN" = "$UPSTREAM_SHA" ]; then
    finish noop 0
  fi
  finish landed 0
fi

SHORT="${UPSTREAM_SHA:0:12}"
BRANCH="sync/upstream-$(date -u +%Y%m%d-%H%M)"
if git_repo rev-parse --verify --quiet "refs/heads/$BRANCH" >/dev/null ||
  git_repo rev-parse --verify --quiet "refs/remotes/origin/$BRANCH" >/dev/null; then
  BRANCH="$BRANCH-$(date -u +%S)"
fi

# Leftover from a killed run: we hold the lock, so nobody else owns this path.
WT="$(dirname "$REPO")/$(basename "$REPO")-sync"
git_repo worktree prune
if [ -e "$WT" ]; then
  if wt_registered "$WT"; then
    warn "removing leftover sync worktree $WT"
    git_repo worktree remove --force "$WT" >/dev/null 2>&1 || precondition "could not remove leftover worktree $WT"
    git_repo worktree prune
  else
    precondition "$WT exists but is not a worktree of this repo; refusing to remove it"
  fi
fi
git_repo worktree add --quiet -b "$BRANCH" "$WT" "$DH_BASE" || precondition "could not create worktree $WT"
WT_CREATED=1

CHECKS_OUT="$(mktemp "$LOG_DIR/.checks.XXXXXX")"
RESOLVER_OUT="$(mktemp "$LOG_DIR/.resolver.XXXXXX")"
export AO_SYNC_UPSTREAM_SHA="$UPSTREAM_SHA"
export AO_SYNC_BRANCH="$BRANCH"
export AO_SYNC_TICKET="$TICKET"

attached() { [ "$(git -C "$WT" symbolic-ref -q --short HEAD)" = "$BRANCH" ]; }

checks_inner() { (cd "$WT" && "${CHECKS[@]}") >"$CHECKS_OUT" 2>&1; }

# Runs the checks (with timeout); records skipped items so a skip is never reported as plain green.
run_checks() {
  local rc
  run_limited "$CHECKS_TIMEOUT" checks_inner
  rc=$?
  cat "$CHECKS_OUT"
  SKIPPED="$(sed -n 's/^CHECKS_SKIPPED=//p' "$CHECKS_OUT" | tail -n 1)"
  [ -n "$SKIPPED" ] && warn "checks green with skips: $SKIPPED"
  return "$rc"
}

render_prompt() {
  local tpl
  tpl="$(cat "$SCRIPT_DIR/resolve-prompt.md")" || return 1
  tpl="${tpl//@SHA@/$UPSTREAM_SHA}"
  tpl="${tpl//@BRANCH@/$BRANCH}"
  tpl="${tpl//@TICKET@/$TICKET}"
  printf '%s\n' "$tpl"
}

# Edit-only Claude session: file tools only (no Bash, no git, no network, no MCP), project settings
# (upstream-controlled) ignored. Edits outside the worktree are not auto-approved by acceptEdits and
# are denied in -p mode. No --max-turns: it is not documented in claude --help; runtime is bounded
# by run_limited instead. Output is kept so a rejected-arguments failure is recognised.
claude_resolver() {
  cd "$WT" || return 1
  render_prompt | claude -p --permission-mode acceptEdits \
    --setting-sources local --strict-mcp-config \
    --tools "Read,Edit,Write,Grep,Glob" \
    --allowedTools "Read,Edit,Write,Grep,Glob" \
    --disallowedTools "WebFetch,WebSearch,Bash"
}
resolver_inner() {
  if [ -n "$RESOLVER_PROGRAM" ]; then
    (cd "$WT" && "$RESOLVER_PROGRAM") >"$RESOLVER_OUT" 2>&1
  else
    claude_resolver >"$RESOLVER_OUT" 2>&1
  fi
}

# --- Test-weakening guard (heuristic) --------------------------------------------------------
# Compares the staged result with BOTH parents and the merge base, so one-sided upstream or
# fork changes are legitimate and only resolver-authored changes count. Flags:
#  1. a test file present in both parents that is deleted or renamed away,
#  2. skip/only/todo markers added (in neither parent),
#  3. a removed assertion or test-definition line that the other side did not itself remove:
#     lines removed vs dh/main unless upstream removed them, and the reverse.
# Not covered (accepted residual): conditions such as "if got != want {", early returns,
# vitest/jest exclude lists beyond CONFIG_RE, __tests__ naming, paths with spaces.
TEST_RE='(_test\.go|\.test\.(ts|tsx|js|jsx|mjs|mts|cts)|\.spec\.(ts|tsx|js|jsx|mjs|mts))$'
SKIP_RE='(t\.Skip(Now|f)?\(|\.skip\(|\.skipIf\(|\.todo\(|\.fixme\(|(^|[^A-Za-z])x(it|describe|test)\(|\.only\(|go:build ignore|\+build ignore)'
REMOVED_RE='(expect\(|assert[A-Za-z_.]*\(|require\.[A-Za-z]+\(|t\.(Error|Errorf|Fatal|Fatalf|Run)\(|func Test[A-Za-z0-9_]*\(|(^|[^A-Za-z])(it|test|describe)(\.each)?\()'
# Build/test configuration whose resolver-authored change must never auto-land: it runs code when
# checks run (npm scripts, .npmrc, go.work, Makefile/Taskfile) or changes what tests run.
CONFIG_RE='(^|/)(package\.json|\.npmrc|go\.work|Makefile|Taskfile\.ya?ml|vitest\.[a-z.]*config\.[a-z]+|vite\.[a-z.]*config\.[a-z]+|playwright\.config\.[a-z]+|\.golangci\.ya?ml|\.github/workflows/[^/]+)$'

# Test files removed or renamed away relative to a parent (old paths).
gone_tests() { # gone_tests <parent>
  git -C "$WT" diff --cached --find-renames --name-status "$1" |
    awk -F'\t' '$1 ~ /^[DR]/ { print $2 }' | grep -E "$TEST_RE" | sort -u
}

# tl <+|-> <git diff args...>: "path<TAB>line" for added/removed lines in test files (no renames).
tl() {
  local sign="$1"
  shift
  git -C "$WT" diff -U0 --no-renames "$@" | awk -v sign="$sign" -v re="$TEST_RE" '
    /^diff --git / { f = $4; sub(/^b\//, "", f); next }
    /^(\+\+\+|---) / { next }
    substr($0, 1, 1) == sign && f ~ re { print f "\t" substr($0, 2) }'
}

weakening_guard() {
  local out rc=0 mb
  mb="$(git -C "$WT" merge-base "$DH_BASE" "$UPSTREAM_SHA")"
  out="$(comm -12 <(gone_tests "$DH_BASE") <(gone_tests "$UPSTREAM_SHA"))"
  [ -n "$out" ] && { err "guard: resolver deleted or renamed test files: $out"; rc=1; }
  out="$(comm -12 \
    <(tl + --cached "$DH_BASE" | grep -E "$SKIP_RE" | sort -u) \
    <(tl + --cached "$UPSTREAM_SHA" | grep -E "$SKIP_RE" | sort -u))"
  [ -n "$out" ] && { err "guard: resolver added skip markers: $out"; rc=1; }
  out="$(comm -23 \
    <(tl - --cached "$DH_BASE" | grep -E "$REMOVED_RE" | sort -u) \
    <(tl - "$mb" "$UPSTREAM_SHA" | grep -E "$REMOVED_RE" | sort -u))"
  [ -n "$out" ] && { err "guard: resolver dropped fork-side assertions/tests: $out"; rc=1; }
  out="$(comm -23 \
    <(tl - --cached "$UPSTREAM_SHA" | grep -E "$REMOVED_RE" | sort -u) \
    <(tl - "$mb" "$DH_BASE" | grep -E "$REMOVED_RE" | sort -u))"
  [ -n "$out" ] && { err "guard: resolver dropped upstream-side assertions/tests: $out"; rc=1; }
  return "$rc"
}

# Index state before the resolver runs: "<mode> <blob> <stage>\t<path>" (conflicted paths have no stage 0).
snapshot_index() { git -C "$WT" ls-files -s >"$PRE_INDEX"; }

# Files matching CONFIG_RE that the RESOLVER changed: staged content differs from the pre-resolver
# stage-0 entry (so cleanly auto-merged files are not flagged) and equals neither parent
# (taking one side of a conflicted file as is stays allowed).
config_changes() {
  local f cur pre
  { git -C "$WT" diff --cached --name-only "$DH_BASE"; git -C "$WT" diff --cached --name-only "$UPSTREAM_SHA"; } |
    sort -u | grep -E "$CONFIG_RE" | while IFS= read -r f; do
    cur="$(git -C "$WT" rev-parse -q --verify ":$f" 2>/dev/null)"
    pre="$(awk -F'\t' -v p="$f" '{ split($1, a, " "); if (a[3] == 0 && $2 == p) print a[2] }' "$PRE_INDEX")"
    if [ "$cur" != "$pre" ] &&
      [ "$cur" != "$(git -C "$WT" rev-parse -q --verify "$DH_BASE:$f" 2>/dev/null)" ] &&
      [ "$cur" != "$(git -C "$WT" rev-parse -q --verify "$UPSTREAM_SHA:$f" 2>/dev/null)" ]; then
      echo "$f"
    fi
  done
}

save_summary() {
  [ -f "$WT/SYNC_SUMMARY.md" ] && cp "$WT/SYNC_SUMMARY.md" "$LOG_DIR/$STAMP-SUMMARY.md"
  return 0
}

unresolved() { # unresolved <reason>
  local where msg
  warn "unresolved: $1"
  save_summary
  if [ "$CONFLICT" = 1 ]; then
    git -C "$WT" diff >"$LOG_DIR/$STAMP-conflict.diff" 2>/dev/null
  fi
  if git -C "$WT" rev-parse -q --verify MERGE_HEAD >/dev/null 2>&1; then
    git -C "$WT" merge --abort
  fi
  # A committed clean merge with red checks is preserved on the sync branch for inspection.
  if [ -n "$MERGE_COMMIT" ] && git -C "$WT" push origin "$MERGE_COMMIT:refs/heads/$BRANCH" >/dev/null 2>&1; then
    where="merge pushed to origin/$BRANCH for inspection"
  elif [ "$CONFLICT" = 1 ]; then
    where="nothing pushed; conflict diff in $LOG_DIR/$STAMP-conflict.diff"
  else
    where="nothing pushed"
  fi
  msg="AO sync needs attention: upstream $SHORT unresolved ($1); $where; log $RUN_LOG"
  notify "$msg"
  finish unresolved 20
}

land_dh() {
  attached || precondition "worktree HEAD is not on $BRANCH; refusing to push dh/main"
  # The checks run upstream code in this worktree: make sure nothing moved HEAD.
  [ "$(git -C "$WT" rev-parse HEAD)" = "$MERGE_COMMIT" ] ||
    precondition "worktree HEAD moved during the checks; refusing to push dh/main"
  push_wt "HEAD:refs/heads/dh/main" || precondition "push of dh/main was rejected"
  [ "$(git -C "$WT" ls-remote origin refs/heads/dh/main | cut -f1)" = "$(git -C "$WT" rev-parse HEAD)" ] ||
    precondition "origin/dh/main does not match the pushed commit"
  if [ -n "$SKIPPED" ]; then
    notify_once checks-skipped "AO sync: upstream $SHORT landed on dh/main with checks skipped: $SKIPPED"
  fi
  finish landed 0
}

if git -C "$WT" merge --no-ff -q -m "🔀 merge: sync upstream $SHORT" -m "Refs: $TICKET" "$UPSTREAM_SHA"; then
  MERGE_COMMIT="$(git -C "$WT" rev-parse HEAD)"
  debug "clean merge $MERGE_COMMIT; running checks"
  if run_checks; then
    land_dh
  fi
  warn "checks red after clean merge"
else
  warn "merge conflict"
  CONFLICT=1
fi

PRE_INDEX="$(mktemp "$LOG_DIR/.preindex.XXXXXX")"
snapshot_index
debug "escalating to resolver (program: ${RESOLVER_PROGRAM:-built-in claude})"
run_limited "$RESOLVER_TIMEOUT" resolver_inner
rrc=$?
log INFO "resolver exited with $rrc"
cat "$RESOLVER_OUT"
if [ -z "$RESOLVER_PROGRAM" ] && [ "$rrc" != 0 ] &&
  grep -qiE 'unknown option|unrecognized (option|argument)|invalid (option|argument)|^usage:|error: (unknown|too many|required)' "$RESOLVER_OUT"; then
  err "claude rejected its arguments"
  unresolved "claude rejected its arguments; the resolver never ran (check the claude CLI version)"
fi

commit_resolved() {
  if [ "$CONFLICT" = 1 ]; then
    git -C "$WT" commit -q -m "🔀 merge: sync upstream $SHORT" \
      -m "Conflicts resolved by an automated Claude Code session; verified by sync.sh." -m "Refs: $TICKET"
  else
    git -C "$WT" commit -q -m "🐛 fix(sync): adapt fork to upstream $SHORT" \
      -m "Applied by an automated Claude Code session; verified by sync.sh." -m "Refs: $TICKET"
  fi
  attached || precondition "worktree HEAD is not on $BRANCH after commit"
  push_wt "HEAD:refs/heads/$BRANCH" || precondition "push of $BRANCH was rejected"
  [ "$(git -C "$WT" ls-remote origin "refs/heads/$BRANCH" | cut -f1)" = "$(git -C "$WT" rev-parse HEAD)" ] ||
    precondition "origin/$BRANCH does not match the pushed commit"
}

# Never trust the resolver's claim: verify the result ourselves.
attached || unresolved "HEAD detached or switched away from $BRANCH"
if [ "$CONFLICT" = 1 ]; then
  git -C "$WT" rev-parse -q --verify MERGE_HEAD >/dev/null || unresolved "resolver committed or aborted the merge itself"
else
  [ "$(git -C "$WT" rev-parse HEAD)" = "$MERGE_COMMIT" ] || unresolved "resolver moved HEAD"
fi
git -C "$WT" add -A -- . ':!SYNC_SUMMARY.md'
save_summary
if git -C "$WT" diff --cached "$DH_BASE" | grep -qE '^\+(<<<<<<< |>>>>>>> )'; then
  unresolved "conflict markers remain"
fi
weakening_guard || unresolved "test-weakening guard tripped"
if [ "$CONFLICT" = 0 ] && git -C "$WT" diff --cached --quiet; then
  unresolved "resolver made no changes"
fi
# Resolver-authored build/test config is never run or auto-landed: checks would execute it.
cfg="$(config_changes | tr '\n' ' ')"
if [ -n "$cfg" ]; then
  warn "resolver changed build/test config: $cfg"
  commit_resolved
  notify "AO sync NEEDS HUMAN: resolver changed build/test config ($cfg). Checks were NOT run. Pushed origin/$BRANCH for review; dh/main untouched."
  finish needs-human 20
fi
run_checks || unresolved "checks red after resolver"

commit_resolved
notify "AO sync: upstream $SHORT merged on origin/$BRANCH (checks green${SKIPPED:+, skipped: $SKIPPED}). dh/main NOT updated: land the branch. Summary: $LOG_DIR/$STAMP-SUMMARY.md"
finish resolved 10
