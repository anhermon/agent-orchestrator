#!/usr/bin/env bash
# Tests for sync.sh against temporary bare repos. No network, no real remotes.
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYNC="$HERE/../sync.sh"
ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

pass=0
fail=0
check() { # check <description> <command...>
  local desc="$1"
  shift
  if "$@"; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $desc"
  fi
}

g() { git -c init.defaultBranch=main "$@"; }

# Builds $T/{upstream.git,fork.git,clone,work,bin}; sets T.
new_fixture() {
  T="$(mktemp -d "$ROOT/case.XXXXXX")"
  g init -q --bare "$T/upstream.git"
  g init -q --bare "$T/fork.git"
  g clone -q "$T/upstream.git" "$T/work" 2>/dev/null
  printf 'l1\nl2\nl3\n' >"$T/work/a.txt"
  g -C "$T/work" add a.txt
  g -C "$T/work" commit -qm init
  g -C "$T/work" push -q origin HEAD:main
  g -C "$T/work" push -q "$T/fork.git" HEAD:main HEAD:refs/heads/dh/main
  g clone -q "$T/fork.git" "$T/clone" 2>/dev/null
  g -C "$T/clone" remote add upstream "$T/upstream.git"
  g -C "$T/clone" remote set-url --push upstream DISABLE
  g -C "$T/clone" fetch -q upstream
  # stubs
  mkdir "$T/bin"
  printf '#!/bin/bash\n[ -e BROKEN ] && exit 1\nexit 0\n' >"$T/bin/checks"
  # shellcheck disable=SC2016 # $1 belongs to the generated stub
  printf '#!/bin/bash\necho "$1" >>%q\n' "$T/notify.log" >"$T/bin/notify"
  printf '#!/bin/bash\nexit 1\n' >"$T/bin/giveup"
  printf '#!/bin/bash\ngit checkout --theirs a.txt && git add a.txt && git commit -q --no-edit\n' >"$T/bin/resolve"
  printf '#!/bin/bash\ngit rm -q BROKEN && git commit -qm fix\n' >"$T/bin/fixbroken"
  chmod +x "$T"/bin/*
  : >"$T/notify.log"
  export AO_SYNC_LOG_DIR="$T/logs"
}

upstream_commit() { # upstream_commit <file> <content> <msg>
  g -C "$T/work" pull -q origin main 2>/dev/null
  printf '%s\n' "$2" >"$T/work/$1"
  g -C "$T/work" add "$1"
  g -C "$T/work" commit -qm "$3"
  g -C "$T/work" push -q origin HEAD:main
}

fork_commit() { # fork_commit <branch> <file> <content>
  g -C "$T/work" fetch -q "$T/fork.git" "$1"
  g -C "$T/work" checkout -q -B tmp FETCH_HEAD
  printf '%s\n' "$3" >"$T/work/$2"
  g -C "$T/work" add "$2"
  g -C "$T/work" commit -qm "fork $2"
  g -C "$T/work" push -q "$T/fork.git" "tmp:refs/heads/$1"
  g -C "$T/work" checkout -q -B main origin/main 2>/dev/null
}

run_sync() { # run_sync <resolver> [checks]
  "$SYNC" --repo "$T/clone" --checks "$T/bin/${2:-checks}" --resolver "$T/bin/$1" \
    --notify "$T/bin/notify" >"$T/out.log" 2>&1
  echo $?
}

fork_ref() { g -C "$T/fork.git" rev-parse "$1"; }
upstream_main() { g -C "$T/upstream.git" rev-parse main; }
last_log() { tail -n 1 "$T/logs/sync.log"; }
contains() { g -C "$T/fork.git" merge-base --is-ancestor "$1" "$2"; }
notify_count() { wc -l <"$T/notify.log" | tr -d ' '; }

# (a) noop
new_fixture
rc="$(run_sync giveup)"
check "a: exit 0" test "$rc" = 0
check "a: result=noop" grep -q 'result=noop' <<<"$(last_log)"

# (b) clean upstream commit, dh work in different file
new_fixture
fork_commit dh/main dh.txt dh
upstream_commit b.txt b "upstream b"
rc="$(run_sync giveup)"
check "b: exit 0" test "$rc" = 0
check "b: main fast-forwarded" test "$(fork_ref main)" = "$(upstream_main)"
check "b: dh/main contains upstream" contains "$(upstream_main)" dh/main
check "b: dh/main keeps fork commit" test -n "$(g -C "$T/fork.git" show dh/main:dh.txt)"
check "b: result=landed" grep -q 'result=landed' <<<"$(last_log)"
check "b: no notify" test "$(notify_count)" = 0
check "b: worktree removed" test ! -e "$T/clone-sync"

# (c) conflict, resolver resolves
new_fixture
fork_commit dh/main a.txt dhline
upstream_commit a.txt upline "upstream a"
before="$(fork_ref dh/main)"
rc="$(run_sync resolve)"
check "c: exit 10" test "$rc" = 10
check "c: dh/main updated" test "$(fork_ref dh/main)" != "$before"
check "c: dh/main contains upstream" contains "$(upstream_main)" dh/main
check "c: result=resolved" grep -q 'result=resolved' <<<"$(last_log)"

# (d) conflict, resolver gives up
new_fixture
fork_commit dh/main a.txt dhline
upstream_commit a.txt upline "upstream a"
before="$(fork_ref dh/main)"
rc="$(run_sync giveup)"
check "d: exit 20" test "$rc" = 20
check "d: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "d: notified once" test "$(notify_count)" = 1
check "d: notify names branch" grep -q 'sync/' "$T/notify.log"
check "d: result=unresolved" grep -q 'result=unresolved' <<<"$(last_log)"
check "d: worktree removed" test ! -e "$T/clone-sync"

# (e) clean merge, checks red, resolver fixes
new_fixture
upstream_commit BROKEN x "upstream breaks"
rc="$(run_sync fixbroken)"
check "e: exit 10" test "$rc" = 10
check "e: dh/main contains upstream" contains "$(upstream_main)" dh/main
check "e: fix landed" test -z "$(g -C "$T/fork.git" ls-tree dh/main -- BROKEN)"

# (e2) checks stay red after resolver -> unresolved, sync branch pushed, dh/main unchanged
new_fixture
upstream_commit BROKEN x "upstream breaks"
before="$(fork_ref dh/main)"
rc="$(run_sync giveup)"
check "e2: exit 20" test "$rc" = 20
check "e2: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "e2: sync branch pushed" test -n "$(g -C "$T/fork.git" for-each-ref 'refs/heads/sync/*')"

# (f) someone committed to fork main
new_fixture
fork_commit main stray.txt stray
upstream_commit b.txt b "upstream b"
before="$(fork_ref main)"
rc="$(run_sync giveup)"
check "f: exit 30" test "$rc" = 30
check "f: main untouched" test "$(fork_ref main)" = "$before"
check "f: notified" test "$(notify_count)" = 1
check "f: result=precondition" grep -q 'result=precondition' <<<"$(last_log)"

# (g) lock held
new_fixture
upstream_commit b.txt b "upstream b"
mkdir "$T/clone/.git/ao-sync.lock"
before_main="$(fork_ref main)"
before_dh="$(fork_ref dh/main)"
rc="$(run_sync giveup)"
check "g: exit 30" test "$rc" = 30
check "g: no git changes" test "$(fork_ref main)$(fork_ref dh/main)" = "$before_main$before_dh"

# (h) upstream is never pushed to; pushurl DISABLE does not break the run
new_fixture
upstream_commit b.txt b "upstream b"
up_before="$(g -C "$T/upstream.git" for-each-ref)"
rc="$(run_sync giveup)"
check "h: exit 0" test "$rc" = 0
check "h: upstream refs untouched" test "$(g -C "$T/upstream.git" for-each-ref)" = "$up_before"

# (i) dh/main absent: mirror main only, warn, exit 0
new_fixture
g -C "$T/fork.git" update-ref -d refs/heads/dh/main
upstream_commit b.txt b "upstream b"
rc="$(run_sync giveup)"
check "i: exit 0" test "$rc" = 0
check "i: main fast-forwarded" test "$(fork_ref main)" = "$(upstream_main)"
check "i: warns" grep -q 'WARN' "$T/logs/"2*.log

echo "passed=$pass failed=$fail"
test "$fail" = 0
