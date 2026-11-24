#!/usr/bin/env bash
# Upstream sync job for the DealHub fork of agent-orchestrator.
#
# Fast-forwards origin/main from upstream/main, then merges upstream into dh/main
# on a sync/<date> branch in a separate worktree. Conflicts or red checks are
# escalated to a resolver (a Claude Code session by default); this script never
# resolves a conflict itself, never pushes to upstream, never force-pushes.
#
# Exit codes: 0 landed or nothing to do, 10 escalated and resolved (landed),
#             20 unresolved (nothing merged, notified), 30 precondition failure.
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO=/Users/angelhermon/workspace/agent-orchestrator
CHECKS="$SCRIPT_DIR/checks.sh"
# shellcheck disable=SC2016 # expanded later by the bash -c that runs the resolver
RESOLVER='claude -p --max-turns 60 --permission-mode acceptEdits "$(cat "$AO_SYNC_PROMPT")"'
NOTIFY="$SCRIPT_DIR/notify.sh"
DRY_RUN=0
DEBUG=0
TICKET=INT-20261123-ao-bitbucket

usage() {
  echo "usage: sync.sh [--repo PATH] [--checks CMD] [--resolver CMD] [--notify CMD] [--dry-run] [--debug]"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --checks) CHECKS="$2"; shift 2 ;;
    --resolver) RESOLVER="$2"; shift 2 ;;
    --notify) NOTIFY="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --debug) DEBUG=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

LOG_DIR="${AO_SYNC_LOG_DIR:-$HOME/Library/Logs/ao-sync}"
mkdir -p "$LOG_DIR"
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
WT=""
NOTIFY_STATUS=""

git_repo() { git -C "$REPO" "$@"; }

# The only push path. Never targets upstream, never forces.
push_origin() { git_repo push origin "$1"; }

finish() { # finish <result> <exit code>
  local line
  line="$(date +%FT%T%z) upstream=${UPSTREAM_SHA:0:12} result=$1 branch=$BRANCH${NOTIFY_STATUS:+ $NOTIFY_STATUS}"
  echo "$line" >>"$LOG_DIR/sync.log"
  echo "$line" >&3
  cleanup
  exit "$2"
}

cleanup() {
  if [ -n "$WT" ] && [ -e "$WT" ]; then
    git_repo worktree remove --force "$WT" >/dev/null 2>&1 || rm -rf "$WT"
    git_repo worktree prune
  fi
  [ -n "$LOCK" ] && rmdir "$LOCK" 2>/dev/null
  return 0
}

notify() {
  local out
  out="$(bash -c "$NOTIFY \"\$1\"" _ "$1" 2>&1)"
  debug "notify output: $out"
  NOTIFY_STATUS="$(grep -o 'notify=[a-z-]*' <<<"$out" | tail -n 1)"
}

precondition() { # precondition <message>
  err "$1"
  notify "AO sync: precondition failed: $1"
  finish precondition 30
}

if [ ! -d "$REPO/.git" ] && ! git_repo rev-parse --git-dir >/dev/null 2>&1; then
  err "not a git repo: $REPO"
  finish precondition 30
fi

# Lock (mkdir is atomic). Stale after 6 hours.
LOCK="$(git_repo rev-parse --path-format=absolute --git-common-dir)/ao-sync.lock"
if [ -d "$LOCK" ] && [ -n "$(find "$LOCK" -maxdepth 0 -mmin +360)" ]; then
  warn "removing stale lock $LOCK"
  rmdir "$LOCK"
fi
if ! mkdir "$LOCK" 2>/dev/null; then
  err "lock held: $LOCK"
  LOCK=""
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

# Mirror main (fast-forward only; push without --force is rejected otherwise).
if [ "$ORIGIN_MAIN" != "$UPSTREAM_SHA" ]; then
  debug "fast-forwarding origin/main to $UPSTREAM_SHA"
  push_origin "$UPSTREAM_SHA:refs/heads/main" || precondition "push of origin/main was rejected"
fi

if ! git_repo rev-parse --verify --quiet origin/dh/main >/dev/null; then
  warn "origin/dh/main does not exist yet; only mirrored main"
  finish noop 0
fi

DH_BASE="$(git_repo rev-parse origin/dh/main)"
if git_repo merge-base --is-ancestor "$UPSTREAM_SHA" "$DH_BASE"; then
  if [ "$ORIGIN_MAIN" = "$UPSTREAM_SHA" ]; then
    finish noop 0
  fi
  finish landed 0
fi

BRANCH="sync/$(date +%F)"
if git_repo rev-parse --verify --quiet "refs/heads/$BRANCH" >/dev/null ||
  git_repo rev-parse --verify --quiet "refs/remotes/origin/$BRANCH" >/dev/null; then
  BRANCH="$BRANCH-$(date +%H%M%S)"
fi
WT="$(dirname "$REPO")/$(basename "$REPO")-sync"
if [ -e "$WT" ]; then
  precondition "worktree path already exists: $WT"
fi
git_repo worktree add --quiet -b "$BRANCH" "$WT" "$DH_BASE" || precondition "could not create worktree $WT"

SHORT="${UPSTREAM_SHA:0:12}"
export AO_SYNC_PROMPT="$SCRIPT_DIR/resolve-prompt.md"
export AO_SYNC_UPSTREAM_SHA="$UPSTREAM_SHA"
export AO_SYNC_BRANCH="$BRANCH"
export AO_SYNC_TICKET="$TICKET"

run_checks() { (cd "$WT" && bash -c "$CHECKS"); }

needs_resolver=0
if git -C "$WT" merge --no-ff -q -m "🔀 merge: sync upstream $SHORT" -m "Refs: $TICKET" "$UPSTREAM_SHA"; then
  debug "clean merge; running checks"
  run_checks || { warn "checks red after clean merge"; needs_resolver=1; }
else
  warn "merge conflict"
  needs_resolver=1
fi

if [ "$needs_resolver" = 0 ]; then
  push_origin "$BRANCH:refs/heads/dh/main" || precondition "push of dh/main was rejected"
  finish landed 0
fi

debug "escalating to resolver: $RESOLVER"
(cd "$WT" && bash -c "$RESOLVER")
log INFO "resolver exited with $?"

# Never trust the resolver's claim: verify the result ourselves.
unresolved() { # unresolved <reason>
  warn "$1"
  if [ -f "$WT/.git" ] && git -C "$WT" rev-parse -q --verify MERGE_HEAD >/dev/null; then
    git -C "$WT" merge --abort
  fi
  # Preserve the attempt on the sync branch only if it is a descendant of dh/main with upstream merged.
  if git -C "$WT" merge-base --is-ancestor "$UPSTREAM_SHA" HEAD 2>/dev/null; then
    push_origin "$BRANCH:refs/heads/$BRANCH" || warn "could not push $BRANCH"
  fi
  notify "AO sync needs attention: upstream $SHORT unresolved ($1). Branch $BRANCH, worktree log $RUN_LOG"
  finish unresolved 20
}

git -C "$WT" rev-parse -q --verify MERGE_HEAD >/dev/null && unresolved "merge still in progress"
git -C "$WT" merge-base --is-ancestor "$UPSTREAM_SHA" HEAD || unresolved "upstream not merged"
git -C "$WT" merge-base --is-ancestor "$DH_BASE" HEAD || unresolved "dh/main history lost"
[ -z "$(git -C "$WT" status --porcelain --untracked-files=no)" ] || unresolved "uncommitted changes left"
if git -C "$WT" grep -qE '^(<<<<<<< |>>>>>>> )' -- . ':!*.md'; then
  unresolved "conflict markers remain"
fi
run_checks || unresolved "checks red after resolver"

push_origin "$BRANCH:refs/heads/dh/main" || unresolved "push of dh/main was rejected"
[ -f "$WT/SYNC_SUMMARY.md" ] && cp "$WT/SYNC_SUMMARY.md" "$LOG_DIR/$STAMP-SUMMARY.md"
notify "AO sync: upstream $SHORT merged into dh/main after conflict resolution. Review $LOG_DIR/$STAMP-SUMMARY.md"
finish resolved 10
