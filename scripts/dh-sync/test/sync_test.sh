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
  prev=""
  for a in "$@"; do [ "$prev" = "--settings" ] && cp "$a" "${FAKE_CLAUDE_REC:-/dev/null}.settings" 2>/dev/null; prev="$a"; done
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
# Hermetic: never run a real gitleaks from the test PATH (cases that need one set their own).
export AO_SYNC_GITLEAKS=none

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
  mkdir -p "$(dirname "$T/work/$1")"
  printf '%s\n' "$2" >"$T/work/$1"
  g -C "$T/work" add "$1"
  g -C "$T/work" commit -qm "$3"
  g -C "$T/work" push -q origin HEAD:main
}

fork_commit() { # fork_commit <branch> <file> <content>
  g -C "$T/work" fetch -q "$T/fork.git" "$1"
  g -C "$T/work" checkout -q -B tmp FETCH_HEAD
  mkdir -p "$(dirname "$T/work/$2")"
  printf '%s\n' "$3" >"$T/work/$2"
  g -C "$T/work" add "$2"
  g -C "$T/work" commit -qm "fork $2"
  g -C "$T/work" push -q "$T/fork.git" "tmp:refs/heads/$1"
  g -C "$T/work" checkout -q -B main origin/main 2>/dev/null
}

run_sync() { # run_sync <resolver> [checks]
  local chk="${2:-$T/bin/checks}"
  case "$chk" in /*) ;; *) chk="$T/bin/$chk" ;; esac
  "$SYNC" --repo "$T/clone" --checks "$chk" --resolver "$T/bin/$1" \
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
check "k: script-owned settings file passed" grep -qx 'arg=--settings' "$rec"
check "k: no setting sources loaded (not local)" bash -c "! grep -qx 'arg=local' '$rec'"
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
g init -q "$TREE"
g -C "$TREE" commit -q --allow-empty -m init
for tool in gofmt go npm npx; do printf '#!/bin/bash\nexit 0\n' >"$T/tools/$tool"; done
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
check "t2: nothing pushed to origin" test -z "$(sync_branches)"
check "t2: local branch kept" test -n "$(g -C "$T/clone" for-each-ref 'refs/heads/sync/*')"
check "t2: notified needs human, with the local path" grep -q "NEEDS HUMAN.*$T/clone-sync" "$T/notify.log"
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

# --- I1: baseline-relative checks (checks.sh + known-red.sh) ----------------------------------
KR="$DHSYNC/known-red.sh"

# Fixture for checks.sh: a git repo with backend/ and frontend/ and fake go/gofmt/npm/npx/lint on PATH.
checks_fixture() {
  CT="$(mktemp -d "$ROOT/chk.XXXXXX")"
  mkdir -p "$CT/repo/backend" "$CT/repo/frontend/src/api" "$CT/repo/backend/internal/storage/sqlite/gen" "$CT/fk"
  echo base >"$CT/repo/frontend/src/api/schema.ts"
  : >"$CT/repo/backend/internal/storage/sqlite/gen/keep"
  g init -q "$CT/repo"
  g -C "$CT/repo" add -A
  g -C "$CT/repo" commit -qm init
  printf '#!/bin/bash\ncase "$1" in test) cat "${FAKE_GO_JSON:-/dev/null}"; exit "${FAKE_GO_RC:-0}";; esac\nexit 0\n' >"$CT/fk/go"
  printf '#!/bin/bash\nexit 0\n' >"$CT/fk/gofmt"
  cat >"$CT/fk/npm" <<'EOF'
#!/bin/bash
case "$*" in
  "run typecheck:e2e") [ -n "${FAKE_TSCE_OUT:-}" ] && cat "$FAKE_TSCE_OUT"; exit "${FAKE_TSCE_RC:-0}" ;;
  "run typecheck") [ -n "${FAKE_TSC_OUT:-}" ] && cat "$FAKE_TSC_OUT"; exit "${FAKE_TSC_RC:-0}" ;;
  "run api") [ -n "${FAKE_DRIFT:-}" ] && echo drifted >frontend/src/api/schema.ts; exit 0 ;;
  "run sqlc") [ -n "${FAKE_SQLC_FAIL:-}" ] && exit 1; exit 0 ;;
esac
exit 0
EOF
  cat >"$CT/fk/npx" <<'EOF'
#!/bin/bash
for a in "$@"; do case "$a" in --outputFile=*) out="${a#--outputFile=}" ;; esac; done
[ -n "${FAKE_VITEST_JSON:-}" ] && [ -n "${out:-}" ] && cp "$FAKE_VITEST_JSON" "$out"
exit "${FAKE_VITEST_RC:-0}"
EOF
  printf '#!/bin/bash\n[ "$1" = version ] && echo "golangci-lint has version ${FAKE_LINT_VERSION:-2.14.0} built with go1.27"\nexit 0\n' >"$CT/fk/golangci-lint"
  chmod +x "$CT/fk"/*
  FE="$(cd "$CT/repo/frontend" && pwd -P)"
  printf '# test\ngo|p/known|*\ngo|p/mixed|TestKnown\ntsc|a.tsx|TS2307\nvitest|v/known.test.ts|*\nvitest|v/mixed.test.ts|known one\n' >"$CT/known"
}
gofail() { printf '{"Action":"fail","Package":"%s","Test":"%s"}\n{"Action":"fail","Package":"%s"}\n' "$1" "$2" "$1"; }
run_checks_script() {
  (cd "$CT/repo" && PATH="$CT/fk:$PATH" AO_SYNC_KNOWN_RED="$CT/known" "$DHSYNC/checks.sh" >"$CT/out" 2>&1)
  echo $?
}
# all-green environment for the fake tools, overridden per case
green_env() { FAKE_GO_RC=0 FAKE_GO_JSON= FAKE_TSC_RC=0 FAKE_TSC_OUT= FAKE_VITEST_RC=0 FAKE_VITEST_JSON= "$@"; }

# known-red.sh unit behaviour
checks_fixture
printf '%s\n' 'go|p/known|*' 'go|p/mixed|TestKnown' 'go|p/mixed|TestNew' 'tsc|a.tsx|TS9999' 'go|_run|*' >"$CT/in"
check "kr: filter keeps only unlisted failures" test "$("$KR" filter "$CT/known" <"$CT/in" | tr '\n' ' ')" = "go|p/mixed|TestNew tsc|a.tsx|TS9999 go|_run|* "
{ gofail p/mixed TestA; echo '{"Action":"fail","Package":"p/build"}'; echo '{"Action":"fail","Package":"p/mixed","Test":"TestA/sub"}'; } >"$CT/go.json"
check "kr: go extracts test-level and package-level, subtests roll up" test "$("$KR" go "$CT/go.json" 1 | tr '\n' ' ')" = "go|p/build|* go|p/mixed|TestA "
echo garbage >"$CT/go.json"
check "kr: go nonzero rc with nothing parsable is go|_run|*" test "$("$KR" go "$CT/go.json" 2)" = "go|_run|*"
printf 'x/a.tsx(3,4): error TS2307: nope\nx/a.tsx(9,1): error TS2307: again\n' >"$CT/tsc.out"
check "kr: tsc ignores line numbers and dedups" test "$("$KR" tsc "$CT/tsc.out" 2)" = "tsc|x/a.tsx|TS2307"

# checks.sh: only known-red failures -> green
checks_fixture
{ gofail p/known TestAnything; gofail p/mixed TestKnown; } >"$CT/go.json"
printf 'a.tsx(1,1): error TS2307: x\n' >"$CT/tsc.out"
printf '{"testResults":[{"name":"%s/v/known.test.ts","status":"failed","assertionResults":[]},{"name":"%s/v/mixed.test.ts","status":"failed","assertionResults":[{"status":"failed","fullName":"known one","title":"x"}]}]}\n' "$FE" "$FE" >"$CT/vitest.json"
export FAKE_GO_JSON="$CT/go.json" FAKE_GO_RC=1 FAKE_TSC_OUT="$CT/tsc.out" FAKE_TSC_RC=2 FAKE_VITEST_JSON="$CT/vitest.json" FAKE_VITEST_RC=1
rc="$(run_checks_script)"
check "ck: only known-red failures exit 0" test "$rc" = 0
check "ck: reports known-red count" grep -q 'CHECKS_KNOWN_RED=' "$CT/out"
check "ck: known-red is not claimed as all green" bash -c "! grep -q 'checks: all green' '$CT/out'"

# a new Go test failure in a package with a different known test
gofail p/mixed TestBrandNew >"$CT/go.json"
rc="$(run_checks_script)"
check "ck: new go test failure exits non-zero" test "$rc" != 0
check "ck: names the new go failure" grep -q 'go|p/mixed|TestBrandNew' "$CT/out"

# a new typecheck code in a known file
printf 'a.tsx(1,1): error TS2322: x\n' >"$CT/tsc.out"
: >"$CT/go.json"
rc="$(FAKE_GO_RC=0 run_checks_script)"
check "ck: new tsc error exits non-zero" test "$rc" != 0
check "ck: names the new tsc failure" grep -q 'tsc|a.tsx|TS2322' "$CT/out"

# a new vitest test failure
printf 'a.tsx(1,1): error TS2307: x\n' >"$CT/tsc.out"
printf '{"testResults":[{"name":"%s/v/mixed.test.ts","status":"failed","assertionResults":[{"status":"failed","fullName":"fresh two","title":"x"}]}]}\n' "$FE" >"$CT/vitest.json"
rc="$(FAKE_GO_RC=0 run_checks_script)"
check "ck: new vitest failure exits non-zero" test "$rc" != 0
check "ck: names the new vitest failure" grep -q 'vitest|v/mixed.test.ts|fresh two' "$CT/out"

# unparsable failure is never green
echo garbage >"$CT/go.json"
rc="$(FAKE_TSC_RC=0 FAKE_TSC_OUT= FAKE_VITEST_RC=0 FAKE_VITEST_JSON= run_checks_script)"
check "ck: unparsable go failure exits non-zero" test "$rc" != 0
unset FAKE_GO_JSON FAKE_GO_RC FAKE_TSC_OUT FAKE_TSC_RC FAKE_VITEST_JSON FAKE_VITEST_RC

# lint version pin, codegen drift
printf '{"testResults":[]}\n' >"$CT/vitest.json"
rc="$(FAKE_VITEST_JSON="$CT/vitest.json" FAKE_LINT_VERSION=2.1.0 run_checks_script)"
check "ck: wrong lint version exits 0 but is skipped, not green" test "$rc" = 0
check "ck: wrong lint version is listed as skipped" grep -q 'CHECKS_SKIPPED=.*golangci-lint' "$CT/out"
rc="$(FAKE_VITEST_JSON="$CT/vitest.json" FAKE_DRIFT=1 run_checks_script)"
check "ck: schema drift exits non-zero" test "$rc" != 0
check "ck: drift is reverted afterwards" test "$(cat "$CT/repo/frontend/src/api/schema.ts")" = base
rc="$(FAKE_VITEST_JSON="$CT/vitest.json" FAKE_SQLC_FAIL=1 run_checks_script)"
check "ck: failing generator is a skip, not green" grep -q 'CHECKS_SKIPPED=.*codegen' "$CT/out"
rc="$(FAKE_VITEST_JSON="$CT/vitest.json" run_checks_script)"
check "ck: clean run is all green" grep -q 'checks: all green' "$CT/out"

# sync.sh: a clean merge whose only failures are known-red lands without the resolver
new_fixture
checks_fixture
mkdir -p "$T/fk"
cp "$CT/fk"/* "$T/fk/"
gofail p/known TestAnything >"$T/go.json"
printf '{"testResults":[]}\n' >"$T/vitest.json"
printf 'go|p/known|*\n' >"$T/known"
stub resolver_marker "touch '$T/resolver.ran'"
g -C "$T/work" pull -q origin main 2>/dev/null
mkdir -p "$T/work/backend" "$T/work/frontend"
echo go >"$T/work/backend/a.go"
echo ts >"$T/work/frontend/a.ts"
g -C "$T/work" add backend frontend
g -C "$T/work" commit -qm "upstream dirs"
g -C "$T/work" push -q origin HEAD:main
rc="$(PATH="$T/fk:$PATH" FAKE_GO_JSON="$T/go.json" FAKE_GO_RC=1 FAKE_VITEST_JSON="$T/vitest.json" AO_SYNC_KNOWN_RED="$T/known" run_sync resolver_marker "$DHSYNC/checks.sh")"
check "kr-sync: exit 0" test "$rc" = 0
check "kr-sync: landed" grep -q 'result=landed' <<<"$(last_log)"
check "kr-sync: resolver never ran" test ! -e "$T/resolver.ran"
check "kr-sync: dh/main contains upstream" contains "$(upstream_main)" dh/main

# --- I2: resolver containment (no push on needs-human, path scope, secret scan, settings) -------
remote_has_sync_branch() { test -n "$(g -C "$T/fork.git" for-each-ref 'refs/heads/sync/*')"; }
local_sync_branch() { g -C "$T/clone" for-each-ref --format='%(refname:short)' 'refs/heads/sync/*'; }
needs_human_case() { # needs_human_case <label> <rc> : common assertions
  check "$1: exit 20" test "$2" = 20
  check "$1: result=needs-human" grep -q 'result=needs-human' <<<"$(last_log)"
  check "$1: nothing pushed to origin" bash -c "test -z \"\$(git -C '$T/fork.git' for-each-ref 'refs/heads/sync/*')\""
  check "$1: dh/main unchanged" test "$(fork_ref dh/main)" = "$before"
  check "$1: local branch kept" test -n "$(local_sync_branch)"
  check "$1: notification names the local worktree" grep -q "NEEDS HUMAN.*$T/clone-sync" "$T/notify.log"
  check "$1: worktree kept" test -d "$T/clone-sync"
}

# a new untracked file
new_fixture
conflict_setup
before="$(fork_ref dh/main)"
stub newfile "printf 'merged\n' >a.txt; echo stolen >NOTES.txt"
rc="$(run_sync newfile checks_marker)"
needs_human_case "o1" "$rc"
check "o1: checks not run" test ! -e "$T/checks.ran"

# a tracked file outside the conflicted set
new_fixture
conflict_setup
before="$(fork_ref dh/main)"
stub touch_other "printf 'merged\n' >a.txt; echo '// extra' >>x_test.go"
rc="$(run_sync touch_other checks_marker)"
needs_human_case "o2" "$rc"

# red checks after a clean merge: a path outside the merge diff is needs-human
new_fixture
upstream_commit BROKEN x "upstream breaks"
before="$(fork_ref dh/main)"
stub fix_and_stray 'rm BROKEN; echo stray >stray.txt'
rc="$(run_sync fix_and_stray)"
needs_human_case "o3" "$rc"

# secret scan: regex fallback (gitleaks disabled)
new_fixture
conflict_setup
before="$(fork_ref dh/main)"
stub leak "printf 'token=%s%s\n' ghp_ abcdefghijklmnopqrstuvwxyz0123456789 >a.txt"
rc="$(run_sync leak)"
needs_human_case "s1" "$rc"
check "s1: notification names the secret scan" grep -q 'secret scan' "$T/notify.log"

# secret scan: a gitleaks hit (exit 1) blocks the push; a clean gitleaks lets it through
new_fixture
conflict_setup
before="$(fork_ref dh/main)"
stub gl_hit 'cat >/dev/null; exit 1'
stub gl_ok 'cat >/dev/null; exit 0'
rc="$(AO_SYNC_GITLEAKS="$T/bin/gl_hit" run_sync resolve)"
needs_human_case "s2" "$rc"
new_fixture
conflict_setup
rc="$(AO_SYNC_GITLEAKS="$T/bin/gl_ok" run_sync resolve)"
check "s3: clean gitleaks lets the resolved branch through (exit 10)" test "$rc" = 10
check "s3: pushed" remote_has_sync_branch

# a worktree that carries Claude settings is refused before the resolver starts
new_fixture
fork_commit dh/main .claude/settings.json '{"hooks":{}}'
conflict_setup
stub resolver_marker "touch '$T/resolver.ran'"
rc="$(run_sync resolver_marker)"
check "w1: exit 20" test "$rc" = 20
check "w1: resolver never started" test ! -e "$T/resolver.ran"
check "w1: nothing pushed" bash -c "test -z \"\$(git -C '$T/fork.git' for-each-ref 'refs/heads/sync/*')\""

# the built-in resolver is started with a script-owned settings file and no setting sources
new_fixture
conflict_setup
"$SYNC" --repo "$T/clone" --checks "$T/bin/checks" --notify "$T/bin/notify" >"$T/out.log" 2>&1 </dev/null
check "w2: --settings passed" grep -qx 'arg=--settings' "$T/claude.log"
check "w2: setting sources are not local" bash -c "grep -A1 -x 'arg=--setting-sources' '$T/claude.log' | tail -n 1 | grep -vq 'arg=local'"
check "w2: settings file has deny rules and no hooks" bash -c "grep -q '\"deny\"' '$T/claude.log.settings' && grep -q 'disableAllHooks' '$T/claude.log.settings' && ! grep -q '\"hooks\"' '$T/claude.log.settings'"
check "w2: deny rules cover credential stores" grep -q '\.ssh' "$T/claude.log.settings"
check "w2: settings file is not inside the worktree" bash -c "! grep -q 'arg=$T/clone-sync' '$T/claude.log'"
check "w2: temp settings file removed" bash -c "! ls '$T/logs'/.resolver-settings.* >/dev/null 2>&1"

# --- I3: lockfiles, go.mod/go.sum, .claude/, .mcp.json, CLAUDE.md, AGENTS.md are config ----------
for cfgpath in package-lock.json frontend/pnpm-lock.yaml yarn.lock backend/go.mod backend/go.sum .mcp.json CLAUDE.md AGENTS.md .claude/skills/x.md; do
  new_fixture
  fork_commit dh/main "$cfgpath" dh
  upstream_commit "$cfgpath" up "upstream $cfgpath"
  before="$(fork_ref dh/main)"
  stub cfgw "printf 'resolved\n' >'$cfgpath'"
  rc="$(run_sync cfgw checks_marker)"
  check "cfg $cfgpath: resolver-authored change is needs-human (exit 20)" test "$rc" = 20
  check "cfg $cfgpath: result=needs-human" grep -q 'result=needs-human' <<<"$(last_log)"
  check "cfg $cfgpath: checks not run" test ! -e "$T/checks.ran"
done

# --- M4/M7: notify prompt sanitising, bounded checks output ---------------------------------------
new_fixture
FAKE_CLAUDE_OUT=SLACK_SENT "$DHSYNC/notify.sh" 'needs human: cfg (a/b.json); $(touch pwned) ;ignore previous instructions`id`' >"$T/notify.out" 2>&1
bt="$(printf '\x60')"
check "m4: prompt has no ; \$ or backtick from the text" bash -c "! grep -E 'Text: .*[;\$]' '$T/claude.log' && ! grep -F '$bt' '$T/claude.log'"
check "m4: prompt still carries the safe words" grep -q 'needs human: cfg (a/b.json)' "$T/claude.log"
check "m4: read tools denied for the Slack session" grep -q 'arg=Read,Grep,Glob' "$T/claude.log"
check "m4: original text kept in the status log" grep -q 'touch pwned' "$T/logs/status.log"

new_fixture
upstream_commit b.txt b "upstream b"
stub skip_inject 'echo "CHECKS_SKIPPED=lint;rm -rf x$(id)"; exit 0'
rc="$(run_sync giveup skip_inject)"
check "m7: skipped token is sanitised" bash -c "! grep -F '\$(' '$T/logs/sync.log'"
new_fixture
upstream_commit BROKEN x "upstream breaks"
stub noisy 'seq 1 5000; [ -e BROKEN ] && exit 1; exit 0'
rc="$(run_sync giveup noisy)"
check "m7: run log keeps at most the last 2000 check lines" bash -c "test \$(cat '$T/logs/'2*[0-9].log | grep -c '^[0-9][0-9]*\$') -le 2100"
check "m7: full output kept on failure" test -n "$(ls "$T/logs"/*-checks-full.log 2>/dev/null)"

echo "passed=$pass failed=$fail"
test "$fail" = 0
