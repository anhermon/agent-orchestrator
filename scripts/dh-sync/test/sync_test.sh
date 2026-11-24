#!/usr/bin/env bash
# Tests for sync.sh/notify.sh/checks.sh against temporary bare repos. No network, no real
# remotes, no real claude or osascript (fakes are first on PATH).
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DHSYNC="$HERE/.."
SYNC="$DHSYNC/sync.sh"
ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT
TICKET=INT-20261123-ao-bitbucket

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

# Fakes first on PATH so no test can reach the real claude or pop a real notification.
FAKEBIN="$ROOT/fakebin"
mkdir "$FAKEBIN"
cat >"$FAKEBIN/osascript" <<'EOF'
#!/bin/bash
echo "osascript $*" >>"${FAKE_OSA_REC:-/dev/null}"
EOF
cat >"$FAKEBIN/claude" <<'EOF'
#!/bin/bash
{
  echo "---"
  echo "pwd=$(pwd -P)"
  for a in "$@"; do echo "arg=$a"; done
  echo "stdin<<"
  [ -t 0 ] || cat
} >>"${FAKE_CLAUDE_REC:-/dev/null}"
[ -n "${FAKE_CLAUDE_OUT:-}" ] && printf '%s\n' "$FAKE_CLAUDE_OUT"
exit 0
EOF
chmod +x "$FAKEBIN"/*
export PATH="$FAKEBIN:$PATH"

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
  printf 'package x\nfunc TestX(t *testing.T) {\n\tassert.Equal(t, 1, 1)\n}\n' >"$T/work/x_test.go"
  g -C "$T/work" add a.txt x_test.go
  g -C "$T/work" commit -qm init
  g -C "$T/work" push -q origin HEAD:main
  g -C "$T/work" push -q "$T/fork.git" HEAD:main HEAD:refs/heads/dh/main
  g clone -q "$T/fork.git" "$T/clone" 2>/dev/null
  g -C "$T/clone" remote add upstream "$T/upstream.git"
  g -C "$T/clone" remote set-url --push upstream DISABLE
  g -C "$T/clone" fetch -q upstream
  mkdir "$T/bin"
  stub checks '[ -e BROKEN ] && exit 1; exit 0'
  stub checks_skip 'echo CHECKS_SKIPPED=golangci-lint; exit 0'
  stub notify "echo \"\$1\" >>'$T/notify.log'"
  stub giveup 'exit 1'
  stub resolve "printf 'merged\n' >a.txt"
  stub fixbroken 'rm BROKEN'
  stub slow "touch '$T/resolver.started'; sleep 30"
  stub del_test "printf 'merged\n' >a.txt; rm x_test.go"
  stub skip_test "printf 'merged\n' >a.txt; printf '\tt.Skip(\"x\")\n' >>x_test.go"
  stub drop_assert "printf 'merged\n' >a.txt; grep -v assert x_test.go >x.tmp; mv x.tmp x_test.go"
  stub detach "git checkout -q --detach; rm BROKEN; git add -A; git commit -qm fix"
  : >"$T/notify.log"
  export AO_SYNC_LOG_DIR="$T/logs"
  export FAKE_OSA_REC="$T/osa.log"
  export FAKE_CLAUDE_REC="$T/claude.log"
}

stub() { # stub <name> <body>
  printf '#!/bin/bash\n%s\n' "$2" >"$T/bin/$1"
  chmod +x "$T/bin/$1"
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
    --notify "$T/bin/notify" >"$T/out.log" 2>&1 </dev/null
  echo $?
}

fork_ref() { g -C "$T/fork.git" rev-parse "$1"; }
upstream_main() { g -C "$T/upstream.git" rev-parse main; }
last_log() { tail -n 1 "$T/logs/sync.log"; }
contains() { g -C "$T/fork.git" merge-base --is-ancestor "$1" "$2"; }
notify_count() { wc -l <"$T/notify.log" | tr -d ' '; }
sync_branches() { g -C "$T/fork.git" for-each-ref --format='%(refname)' 'refs/heads/sync/upstream-*'; }
conflict_setup() {
  fork_commit dh/main a.txt dhline
  upstream_commit a.txt upline "upstream a"
}

# (a) noop
new_fixture
rc="$(run_sync giveup)"
check "a: exit 0" test "$rc" = 0
check "a: result=noop" grep -q 'result=noop' <<<"$(last_log)"

# (b) clean upstream commit lands on dh/main
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
check "b: no sync branch pushed" test -z "$(sync_branches)"
check "b: no spurious timeout warning" bash -c "! grep -q 'timeout after' '$T/logs/'2*.log"
check "b: lock released" test ! -e "$T/clone/.git/ao-sync.lock"

# (c) conflict, resolver resolves: pushed to sync/upstream-*, dh/main NOT updated
new_fixture
conflict_setup
before="$(fork_ref dh/main)"
rc="$(run_sync resolve)"
check "c: exit 10" test "$rc" = 10
check "c: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "c: sync branch pushed" test -n "$(sync_branches)"
check "c: sync branch has upstream" contains "$(upstream_main)" "$(sync_branches)"
check "c: sync branch has dh/main" contains "$before" "$(sync_branches)"
check "c: result=resolved" grep -q 'result=resolved' <<<"$(last_log)"
check "c: notified once, names branch" grep -q "$(sync_branches | sed 's|refs/heads/||')" "$T/notify.log"
check "c: no conflict markers committed" test -z "$(g -C "$T/fork.git" grep -l '^<<<<<<<' "$(sync_branches)" 2>/dev/null)"

# (d) conflict, resolver gives up
new_fixture
conflict_setup
before="$(fork_ref dh/main)"
rc="$(run_sync giveup)"
check "d: exit 20" test "$rc" = 20
check "d: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "d: nothing pushed" test -z "$(sync_branches)"
check "d: notified once" test "$(notify_count)" = 1
check "d: notify is accurate" grep -q 'nothing pushed' "$T/notify.log"
check "d: conflict diff kept" test -n "$(ls "$T/logs"/*-conflict.diff 2>/dev/null)"
check "d: result=unresolved" grep -q 'result=unresolved' <<<"$(last_log)"
check "d: worktree removed" test ! -e "$T/clone-sync"
check "d: local branch removed" test -z "$(g -C "$T/clone" for-each-ref 'refs/heads/sync/*')"

# (e) clean merge, checks red, resolver fixes: sync branch only
new_fixture
upstream_commit BROKEN x "upstream breaks"
before="$(fork_ref dh/main)"
rc="$(run_sync fixbroken)"
check "e: exit 10" test "$rc" = 10
check "e: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "e: fix on sync branch" test -z "$(g -C "$T/fork.git" ls-tree "$(sync_branches)" -- BROKEN)"
check "e: sync branch has upstream" contains "$(upstream_main)" "$(sync_branches)"

# (e2) checks stay red -> unresolved, merge preserved on sync branch, dh/main unchanged
new_fixture
upstream_commit BROKEN x "upstream breaks"
before="$(fork_ref dh/main)"
rc="$(run_sync giveup)"
check "e2: exit 20" test "$rc" = 20
check "e2: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "e2: sync branch pushed" test -n "$(sync_branches)"
check "e2: notify names the pushed branch" grep -q "$(sync_branches | sed 's|refs/heads/||')" "$T/notify.log"

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

# (g) lock held (live pid): exit 30, notify once per day, no git changes
new_fixture
upstream_commit b.txt b "upstream b"
mkdir "$T/clone/.git/ao-sync.lock"
before_main="$(fork_ref main)"
before_dh="$(fork_ref dh/main)"
rc="$(run_sync giveup)"
check "g: exit 30" test "$rc" = 30
check "g: no git changes" test "$(fork_ref main)$(fork_ref dh/main)" = "$before_main$before_dh"
check "g: notified" test "$(notify_count)" = 1
rc="$(run_sync giveup)"
check "g: second run exit 30" test "$rc" = 30
check "g: notified once per day" test "$(notify_count)" = 1
check "g: foreign lock left alone" test -d "$T/clone/.git/ao-sync.lock"

# (g2) lock of a dead pid is recovered
new_fixture
upstream_commit b.txt b "upstream b"
dead="$(bash -c 'echo $$')"
mkdir "$T/clone/.git/ao-sync.lock"
echo "$dead" >"$T/clone/.git/ao-sync.lock/pid"
rc="$(run_sync giveup)"
check "g2: exit 0" test "$rc" = 0
check "g2: landed" grep -q 'result=landed' <<<"$(last_log)"

# (h) upstream is never pushed to; pushurl DISABLE does not break the run
new_fixture
upstream_commit b.txt b "upstream b"
up_before="$(g -C "$T/upstream.git" for-each-ref)"
rc="$(run_sync giveup)"
check "h: exit 0" test "$rc" = 0
check "h: upstream refs untouched" test "$(g -C "$T/upstream.git" for-each-ref)" = "$up_before"

# (i) dh/main absent: mirror main only, warn, notify
new_fixture
g -C "$T/fork.git" update-ref -d refs/heads/dh/main
upstream_commit b.txt b "upstream b"
rc="$(run_sync giveup)"
check "i: exit 0" test "$rc" = 0
check "i: main fast-forwarded" test "$(fork_ref main)" = "$(upstream_main)"
check "i: warns" grep -q 'WARN' "$T/logs/"2*.log
check "i: notified" test "$(notify_count)" = 1

# (j) F1: install layout in a directory with a space, default checks and notify
new_fixture
upstream_commit b.txt b "upstream b"
INST="$T/App Support/ao-sync"
mkdir -p "$INST"
cp "$DHSYNC/sync.sh" "$DHSYNC/notify.sh" "$DHSYNC/resolve-prompt.md" "$INST/"
printf '#!/bin/bash\ntouch "%s"\n' "$T/checks.ran" >"$INST/checks.sh"
chmod +x "$INST"/*.sh
bash "$INST/sync.sh" --repo "$T/clone" --resolver "$T/bin/giveup" >"$T/out.log" 2>&1 </dev/null
rc=$?
check "j: exit 0" test "$rc" = 0
check "j: default checks ran" test -e "$T/checks.ran"
check "j: landed" grep -q 'result=landed' <<<"$(last_log)"
new_fixture
conflict_setup
INST="$T/App Support/ao-sync"
mkdir -p "$INST"
cp "$DHSYNC/sync.sh" "$DHSYNC/notify.sh" "$DHSYNC/resolve-prompt.md" "$DHSYNC/checks.sh" "$INST/"
bash "$INST/sync.sh" --repo "$T/clone" --resolver "$T/bin/giveup" >"$T/out.log" 2>&1 </dev/null
rc=$?
check "j: unresolved exit 20" test "$rc" = 20
check "j: default notify ran osascript" grep -q 'osascript' "$T/osa.log"
check "j: default notify wrote status file" grep -q 'needs attention' "$T/logs/status.log"
check "j: status is local-only (fake claude sent nothing)" grep -q 'notify=local-only' <<<"$(last_log)"

# (k) F2/F5: built-in resolver contract, via fake claude on PATH
new_fixture
conflict_setup
"$SYNC" --repo "$T/clone" --checks "$T/bin/checks" --notify "$T/bin/notify" >"$T/out.log" 2>&1 </dev/null
rc=$?
check "k: exit 20 (fake claude edited nothing)" test "$rc" = 20
rec="$T/claude.log"
check "k: cwd is the sync worktree" grep -qx "pwd=$(cd "$T" && pwd -P)/clone-sync" "$rec"
check "k: allowedTools passed" grep -qx 'arg=--allowedTools' "$rec"
check "k: disallowedTools passed" grep -qx 'arg=--disallowedTools' "$rec"
check "k: tools restricted" grep -qx 'arg=--tools' "$rec"
check "k: strict mcp" grep -qx 'arg=--strict-mcp-config' "$rec"
check "k: project settings ignored" grep -qx 'arg=local' "$rec"
check "k: no web tools allowed" bash -c "! grep '^arg=Read' '$rec' | grep -qE 'Web'"
check "k: git commit/push denied" bash -c "grep -q 'Bash(git commit:\*)' '$rec' && grep -q 'Bash(git push:\*)' '$rec'"
check "k: no bypass flags" bash -c "! grep -qiE 'dangerously|bypass|skip-permissions' '$rec'"
check "k: prompt has real sha" grep -q "$(upstream_main)" "$rec"
check "k: prompt has branch" grep -q 'sync/upstream-' "$rec"
check "k: prompt has ticket" grep -q "$TICKET" "$rec"
check "k: prompt fully substituted" bash -c "! grep -q '@[A-Z]*@' '$rec'"
check "k: prompt omits upstream commit message and content" bash -c "! grep -qE 'upstream a|upline' '$rec'"

# (l) F3: notify.sh with a claude that exits 0 without sending must not report sent
new_fixture
out="$("$DHSYNC/notify.sh" "hello there" 2>&1 </dev/null)"
check "l: local-only when nothing confirmed" test "$(tail -n 1 <<<"$out")" = "notify=local-only"
check "l: osascript always called" grep -q 'hello there' "$T/osa.log"
check "l: status file written" grep -q 'hello there' "$T/logs/status.log"
export FAKE_CLAUDE_OUT=SLACK_SENT
out="$("$DHSYNC/notify.sh" "second" 2>&1 </dev/null)"
unset FAKE_CLAUDE_OUT
check "l: sent only when confirmed" test "$(tail -n 1 <<<"$out")" = "notify=sent"
check "l: slack tool allow-listed" grep -q 'arg=mcp__' "$T/claude.log"

# (m) F4: resolver detaches HEAD and commits: fails safely and notifies
new_fixture
upstream_commit BROKEN x "upstream breaks"
before="$(fork_ref dh/main)"
rc="$(run_sync detach)"
check "m: exit 20" test "$rc" = 20
check "m: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "m: notified" test "$(notify_count)" = 1
check "m: detached commit not pushed" test -z "$(g -C "$T/fork.git" log --all --oneline --grep=fix)"

# (n) F6: test-weakening guard
for variant in del_test skip_test drop_assert; do
  new_fixture
  conflict_setup
  before="$(fork_ref dh/main)"
  rc="$(run_sync "$variant")"
  check "n: $variant exit 20" test "$rc" = 20
  check "n: $variant dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
  check "n: $variant nothing pushed" test -z "$(sync_branches)"
  check "n: $variant guard logged" grep -q 'guard:' "$T/logs/"2*.log
done

# (o) F7: SIGTERM releases lock and worktree; leftover worktree is cleaned; timeout
new_fixture
conflict_setup
"$SYNC" --repo "$T/clone" --checks "$T/bin/checks" --resolver "$T/bin/slow" --notify "$T/bin/notify" \
  >"$T/out.log" 2>&1 </dev/null &
spid=$!
for _ in $(seq 1 100); do
  [ -e "$T/resolver.started" ] && break
  sleep 0.1
done
check "o: resolver started" test -e "$T/resolver.started"
kill -TERM "$spid"
wait "$spid"
rc=$?
check "o: exit 143" test "$rc" = 143
check "o: lock released" test ! -e "$T/clone/.git/ao-sync.lock"
check "o: worktree removed" test ! -e "$T/clone-sync"
check "o: result=aborted" grep -q 'result=aborted' <<<"$(last_log)"

new_fixture
upstream_commit b.txt b "upstream b"
g -C "$T/clone" worktree add -q -b old/leftover "$T/clone-sync" origin/dh/main
rc="$(run_sync giveup)"
check "o2: leftover worktree recovered" test "$rc" = 0
check "o2: landed" grep -q 'result=landed' <<<"$(last_log)"

new_fixture
conflict_setup
export AO_SYNC_RESOLVER_TIMEOUT=1
rc="$(run_sync slow)"
unset AO_SYNC_RESOLVER_TIMEOUT
check "o3: resolver timeout -> exit 20" test "$rc" = 20
check "o3: timeout logged" grep -q 'timeout after 1s' "$T/logs/"2*.log

# (p) F8: skipped checks are recorded in sync.log
new_fixture
upstream_commit b.txt b "upstream b"
rc="$(run_sync giveup checks_skip)"
check "p: exit 0" test "$rc" = 0
check "p: skip recorded" grep -q 'skipped=golangci-lint' <<<"$(last_log)"

# (p2) checks.sh itself reports skips, with fake toolchain and no golangci-lint
new_fixture
TREE="$T/tree"
mkdir -p "$TREE/backend" "$TREE/frontend" "$T/tools"
for tool in gofmt go npm; do printf '#!/bin/bash\nexit 0\n' >"$T/tools/$tool"; done
chmod +x "$T/tools"/*
out="$(cd "$TREE" && PATH="$T/tools:/usr/bin:/bin" bash "$DHSYNC/checks.sh" 2>&1)"
rc=$?
check "p2: checks.sh exit 0" test "$rc" = 0
check "p2: CHECKS_SKIPPED line" grep -qx 'CHECKS_SKIPPED=golangci-lint' <<<"$out"
check "p2: summary says green with skips" grep -q 'green with skips: golangci-lint' <<<"$out"
check "p2: not plain green" bash -c "! grep -q 'all green' <<<'$out'"

# (q) F11: old per-run logs are pruned, sync.log is kept
new_fixture
mkdir -p "$T/logs"
touch -t 202001010000 "$T/logs/20200101T000000.log" "$T/logs/20200101T000000-SUMMARY.md" "$T/logs/sync.log"
rc="$(run_sync giveup)"
check "q: old run log pruned" test ! -e "$T/logs/20200101T000000.log"
check "q: old summary pruned" test ! -e "$T/logs/20200101T000000-SUMMARY.md"
check "q: sync.log kept" test -e "$T/logs/sync.log"

# (r) F9: SYNC_SUMMARY.md kept in the log dir
new_fixture
conflict_setup
stub summary "printf 'merged\n' >a.txt; echo 'summary text' >SYNC_SUMMARY.md"
rc="$(run_sync summary)"
check "r: exit 10" test "$rc" = 10
check "r: summary kept" grep -q 'summary text' "$T/logs/"*-SUMMARY.md
check "r: summary not committed" test -z "$(g -C "$T/fork.git" ls-tree "$(sync_branches)" -- SYNC_SUMMARY.md)"

echo "passed=$pass failed=$fail"
test "$fail" = 0
