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
[ -z "${FAKE_OSA_FAIL:-}" ]
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
if [ -n "${FAKE_CLAUDE_REJECT:-}" ]; then
  printf "error: unknown option '--x'\nUsage: claude [options]\n" >&2
  exit 2
fi
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
  printf '{\n"n1": 1,\n"n2": 2,\n"n3": 3,\n"n4": 4,\n"n5": 5,\n"n6": 6,\n"n7": 7\n}\n' >"$T/work/package.json"
  g -C "$T/work" add a.txt x_test.go package.json
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
  stub rename_test "printf 'merged\n' >a.txt; mv x_test.go x_test.go.off"
  stub drop_fork 'printf "package x\nfunc TestX(t *testing.T) {\n\tassert.Equal(t, 1, 1)\n\tassert.Equal(t, 3, 3)\n}\n" >x_test.go'
  stub cfg_change 'printf "{\"scripts\":{\"test\":\"true\"}}\n" >package.json'
  stub checks_marker "touch '$T/checks.ran'"
  stub checks_move 'git commit --allow-empty -qm moved'
  stub leaky 'sleep 3141.59 & printf "merged\n" >a.txt'
  stub leaky_term "( trap '' TERM; exec sleep 3141.59 ) & printf 'merged\n' >a.txt"
  stub leaky_slow 'sleep 3141.59 & wait'
  stub slow_term "touch '$T/resolver.started'; ( trap '' TERM; exec sleep 2719.1 ) & sleep 30"
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
check "k: Bash not in tools" bash -c "! awk '/^arg=--tools\$/{getline; print}' '$rec' | grep -q Bash"
check "k: Bash and web denied" grep -qx 'arg=WebFetch,WebSearch,Bash' "$rec"
check "k: no max-turns" bash -c "! grep -q 'max-turns' '$rec'"
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
check "l: slack session uses user settings only" grep -qx 'arg=user' "$T/claude.log"
check "l: no max-turns in notify" bash -c "! grep -q 'max-turns' '$T/claude.log'"
export FAKE_CLAUDE_REJECT=1
out="$("$DHSYNC/notify.sh" "third" 2>&1 </dev/null)"
unset FAKE_CLAUDE_REJECT
check "l: rejected args: local-only" test "$(tail -n 1 <<<"$out")" = "notify=local-only"
check "l: rejected args recorded" grep -q 'claude rejected its arguments' "$T/logs/status.log"
export FAKE_OSA_FAIL=1
out="$("$DHSYNC/notify.sh" "fourth" 2>&1 </dev/null)"
check "l: osascript failure reflected" test "$(tail -n 1 <<<"$out")" = "notify=osascript-failed"
export FAKE_CLAUDE_OUT=SLACK_SENT
out="$("$DHSYNC/notify.sh" "fifth" 2>&1 </dev/null)"
unset FAKE_CLAUDE_OUT FAKE_OSA_FAIL
check "l: sent without banner" test "$(tail -n 1 <<<"$out")" = "notify=sent-no-banner"

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
for variant in del_test skip_test drop_assert rename_test; do
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
export AO_SYNC_RESOLVER_TIMEOUT=2718
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
unset AO_SYNC_RESOLVER_TIMEOUT
# R1: the timeout watcher (a subshell sharing sync.sh's command line) and its sleep are gone too
mine() { pgrep -f -- "--repo $T/clone" | wc -l | tr -d ' '; }
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [ "$(mine)" = 0 ] && [ "$(pgrep -f 'sleep 2718$' | wc -l | tr -d ' ')" = 0 ] && break
  sleep 0.5
done
check "o: no leftover watcher after SIGTERM" test "$(mine)" = 0
check "o: no leftover watcher sleep after SIGTERM" test "$(pgrep -f 'sleep 2718$' | wc -l | tr -d ' ')" = 0

# (o2) SIGTERM with a TERM-ignoring grandchild: the group is escalated to KILL (bounded)
new_fixture
conflict_setup
export AO_SYNC_RESOLVER_TIMEOUT=2718
"$SYNC" --repo "$T/clone" --checks "$T/bin/checks" --resolver "$T/bin/slow_term" --notify "$T/bin/notify" \
  >"$T/out.log" 2>&1 </dev/null &
spid=$!
for _ in $(seq 1 100); do
  [ -e "$T/resolver.started" ] && break
  sleep 0.1
done
check "o2: resolver started" test -e "$T/resolver.started"
kill -TERM "$spid"
wait "$spid"
rc=$?
check "o2: exit 143" test "$rc" = 143
unset AO_SYNC_RESOLVER_TIMEOUT
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [ "$(pgrep -f 'sleep 2719.1$' | wc -l | tr -d ' ')" = 0 ] && break
  sleep 0.5
done
check "o2: TERM-ignoring grandchild killed after SIGTERM" test "$(pgrep -f 'sleep 2719.1$' | wc -l | tr -d ' ')" = 0
pkill -KILL -f 'sleep 2719.1$' 2>/dev/null

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
check "p: skip on landing is notified" grep -q 'skipped: golangci-lint' "$T/notify.log"

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

# (s) N1: rejected claude arguments are detected and reported (resolver)
new_fixture
conflict_setup
export FAKE_CLAUDE_REJECT=1
"$SYNC" --repo "$T/clone" --checks "$T/bin/checks" --notify "$T/bin/notify" >"$T/out.log" 2>&1 </dev/null
rc=$?
unset FAKE_CLAUDE_REJECT
check "s: exit 20" test "$rc" = 20
check "s: notify says claude rejected its arguments" grep -q 'rejected its arguments' "$T/notify.log"

# (t) N2: fork-side assertion dropped in a conflicted test file; resolver-authored config change
new_fixture
fork_commit dh/main x_test.go $'package x\nfunc TestX(t *testing.T) {\n\tassert.Equal(t, 1, 1)\n\tassert.Equal(t, 2, 2)\n}'
upstream_commit x_test.go $'package x\nfunc TestX(t *testing.T) {\n\tassert.Equal(t, 1, 1)\n\tassert.Equal(t, 3, 3)\n}' "upstream x_test"
before="$(fork_ref dh/main)"
rc="$(run_sync drop_fork)"
check "t: dropped fork-side assertion -> exit 20" test "$rc" = 20
check "t: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "t: nothing pushed" test -z "$(sync_branches)"
check "t: guard names fork-side" grep -q 'fork-side' "$T/logs/"2*.log

new_fixture
fork_commit dh/main package.json '{"scripts":{"test":"a"}}'
upstream_commit package.json '{"scripts":{"test":"b"}}' "upstream pkg"
before="$(fork_ref dh/main)"
rc="$(run_sync cfg_change checks_marker)"
check "t2: config change -> exit 20" test "$rc" = 20
check "t2: checks were not run" test ! -e "$T/checks.ran"
check "t2: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
check "t2: sync branch pushed for review" test -n "$(sync_branches)"
check "t2: notified needs human" grep -q 'NEEDS HUMAN' "$T/notify.log"
check "t2: result=needs-human" grep -q 'result=needs-human' <<<"$(last_log)"

# (u) N3: a plain directory at <repo>-sync is never removed
new_fixture
upstream_commit b.txt b "upstream b"
mkdir "$T/clone-sync"
echo keep >"$T/clone-sync/file"
rc="$(run_sync giveup)"
check "u: exit 30" test "$rc" = 30
check "u: directory survives" test -e "$T/clone-sync/file"
check "u: notified" test "$(notify_count)" = 1

# (v) N8: HEAD moved by the checks -> refuse to land; identical remote URLs -> refuse to run
new_fixture
upstream_commit b.txt b "upstream b"
before="$(fork_ref dh/main)"
rc="$(run_sync giveup checks_move)"
check "v: exit 30" test "$rc" = 30
check "v: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"

new_fixture
upstream_commit b.txt b "upstream b"
g -C "$T/clone" remote set-url upstream "$T/fork.git"
before="$(fork_ref main)$(fork_ref dh/main)"
rc="$(run_sync giveup)"
check "v2: same URLs exit 30" test "$rc" = 30
check "v2: nothing changed" test "$(fork_ref main)$(fork_ref dh/main)" = "$before"

# (w) N4: no leaked processes (unique sleep durations; pre-existing sleeps are never touched)
procs() { pgrep -f -c 'sleep (3141\.59|2718|3141)$' 2>/dev/null || pgrep -f 'sleep (3141\.59|2718|3141)$' | wc -l | tr -d ' '; }
settle() { for _ in 1 2 3 4 5 6 7 8 9 10; do [ "$(procs)" = 0 ] && return 0; sleep 0.5; done; return 1; }
new_fixture
conflict_setup
export AO_SYNC_RESOLVER_TIMEOUT=2718 AO_SYNC_CHECKS_TIMEOUT=3141
rc="$(run_sync leaky)"
check "w: leaky resolver run ok (exit 10)" test "$rc" = 10
check "w: no leftover processes after normal run" settle
new_fixture
conflict_setup
export AO_SYNC_RESOLVER_TIMEOUT=1
rc="$(run_sync leaky_slow)"
unset AO_SYNC_RESOLVER_TIMEOUT AO_SYNC_CHECKS_TIMEOUT
check "w: timeout run exit 20" test "$rc" = 20
check "w: no leftover processes after timeout" settle

# (x) notify_once suppression for dh/main missing; dry-run; stale pid-less lock
new_fixture
g -C "$T/fork.git" update-ref -d refs/heads/dh/main
rc="$(run_sync giveup)"
rc="$(run_sync giveup)"
check "x: dh/main-missing notified once per day" test "$(notify_count)" = 1

new_fixture
upstream_commit b.txt b "upstream b"
before="$(fork_ref main)"
"$SYNC" --repo "$T/clone" --dry-run --notify "$T/bin/notify" >"$T/out.log" 2>&1 </dev/null
check "x2: dry-run pushes nothing" test "$(fork_ref main)" = "$before"
check "x2: dry-run result noop" grep -q 'result=noop' <<<"$(last_log)"

new_fixture
upstream_commit b.txt b "upstream b"
mkdir "$T/clone/.git/ao-sync.lock"
touch -t 202001010000 "$T/clone/.git/ao-sync.lock"
rc="$(run_sync giveup)"
check "x3: stale pid-less lock recovered" test "$rc" = 0

# (y) R6: a grandchild ignoring TERM is escalated to KILL after the leader exits
new_fixture
conflict_setup
rc="$(run_sync leaky_term)"
check "y: run ok (exit 10)" test "$rc" = 10
check "y: TERM-ignoring grandchild killed" settle

# (z) R2: config files auto-merged cleanly are not flagged; only resolver-authored changes are
new_fixture
fork_commit dh/main package.json $'{\n"n1": 10,\n"n2": 2,\n"n3": 3,\n"n4": 4,\n"n5": 5,\n"n6": 6,\n"n7": 7\n}'
upstream_commit package.json $'{\n"n1": 1,\n"n2": 2,\n"n3": 3,\n"n4": 4,\n"n5": 5,\n"n6": 6,\n"n7": 70\n}' "upstream pkg"
conflict_setup
rc="$(run_sync resolve)"
check "z: auto-merged package.json not flagged (exit 10)" test "$rc" = 10
check "z: sync branch pushed" test -n "$(sync_branches)"
check "z: not needs-human" bash -c "! grep -q 'needs-human' '$T/logs/sync.log'"
check "z: merged package.json has both edits" bash -c "g() { git -C '$T/fork.git' show \"\$1:package.json\"; }; b=\"$(sync_branches)\"; g \"\$b\" | grep -q 'n1\": 10' && g \"\$b\" | grep -q 'n7\": 70'"
new_fixture
fork_commit dh/main package.json $'{\n"n1": 10,\n"n2": 2,\n"n3": 3,\n"n4": 4,\n"n5": 5,\n"n6": 6,\n"n7": 7\n}'
upstream_commit package.json $'{\n"n1": 1,\n"n2": 2,\n"n3": 3,\n"n4": 4,\n"n5": 5,\n"n6": 6,\n"n7": 70\n}' "upstream pkg"
conflict_setup
stub touch_pkg "printf 'merged\n' >a.txt; printf '{\"x\":1}\n' >package.json"
rc="$(run_sync touch_pkg)"
check "z2: resolver-authored package.json change flagged (exit 20)" test "$rc" = 20
check "z2: result=needs-human" grep -q 'result=needs-human' <<<"$(last_log)"

# (aa) R4: registered leftover worktree and plain directory under a path with spaces
new_fixture
upstream_commit b.txt b "upstream b"
mkdir "$T/my space"
mv "$T/clone" "$T/my space/clone"
CL="$T/my space/clone"
g -C "$CL" worktree add -q -b old/left "$CL-sync" origin/dh/main
"$SYNC" --repo "$CL" --checks "$T/bin/checks" --resolver "$T/bin/giveup" --notify "$T/bin/notify" >"$T/out.log" 2>&1 </dev/null
rc=$?
check "aa: registered leftover with spaces recovered" test "$rc" = 0
check "aa: landed" grep -q 'result=landed' <<<"$(last_log)"
new_fixture
upstream_commit b.txt b "upstream b"
mkdir "$T/my space"
mv "$T/clone" "$T/my space/clone"
CL="$T/my space/clone"
mkdir "$CL-sync"
echo keep >"$CL-sync/file"
"$SYNC" --repo "$CL" --checks "$T/bin/checks" --resolver "$T/bin/giveup" --notify "$T/bin/notify" >"$T/out.log" 2>&1 </dev/null
rc=$?
check "aa: plain dir with spaces refused (exit 30)" test "$rc" = 30
check "aa: plain dir with spaces survives" test -e "$CL-sync/file"

# (ab) R7: a missing remote is named, not reported as identical URLs
new_fixture
upstream_commit b.txt b "upstream b"
g -C "$T/clone" remote remove upstream
rc="$(run_sync giveup)"
check "ab: missing remote exit 30" test "$rc" = 30
check "ab: names the remote" grep -q 'remote upstream is not configured' "$T/logs/"2*.log

echo "passed=$pass failed=$fail"
test "$fail" = 0
