#!/usr/bin/env bash
# Fork-wide checks, run from the root of the tree under test (the sync worktree).
#
# Baseline-relative: Go tests, typecheck and vitest fail the run only on failures that are NOT in
# known-red.txt (see known-red.sh and regen-known-red.sh), so environment failures that exist on
# dh/main (missing CLIs, timeouts, unresolved modules) neither block a landing nor wake the resolver.
# Known-red failures are counted: CHECKS_KNOWN_RED=<n>. A skipped item is never reported as plain
# green: it is printed as CHECKS_SKIPPED=<items> (parsed by sync.sh into sync.log).
# Commands mirror CI (.github/workflows/go.yml and frontend.yml): go test -race -count=1
# -timeout=10m, npm run typecheck and typecheck:e2e, npx vitest run, golangci-lint v2.14.0,
# and the sqlc / API codegen drift checks.
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KNOWN_RED="${AO_SYNC_KNOWN_RED:-$HERE/known-red.txt}"
KR="$HERE/known-red.sh"
LINT_VERSION=2.14.0
ROOT="$(pwd)"
skipped=""
new_red=0
known_red=0
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

add_skip() { skipped="${skipped:+$skipped,}$1"; }

# triage <label> <normalised failures file>: counts known-red, prints and counts new failures.
triage() {
  local label="$1" file="$2" new all
  all="$(grep -c . "$file" || true)"
  "$KR" filter "$KNOWN_RED" <"$file" >"$file.new"
  new="$(grep -c . "$file.new" || true)"
  known_red=$((known_red + all - new))
  if [ "$new" != 0 ]; then
    new_red=$((new_red + new))
    echo "NEW FAILURES ($label), not in known-red.txt:"
    sed 's/^/  /' "$file.new"
  elif [ "$all" != 0 ]; then
    echo "$label: $all known-red failure(s), no new ones"
  fi
}

echo "== gofmt"
unformatted="$(cd "$ROOT/backend" && gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "unformatted files:" "$unformatted"
  exit 1
fi

echo "== go build"
(cd "$ROOT/backend" && go build ./...) || exit 1
echo "== go vet"
(cd "$ROOT/backend" && go vet ./...) || exit 1

echo "== go test -race"
(cd "$ROOT/backend" && go test -race -count=1 -timeout=10m -json ./... >"$tmp/go.json" 2>"$tmp/go.err")
rc=$?
"$KR" go "$tmp/go.json" "$rc" >"$tmp/go.fail"
[ -n "${AO_SYNC_LOG_DIR:-}" ] && [ -s "$tmp/go.fail" ] && cp "$tmp/go.json" "$AO_SYNC_LOG_DIR/checks-go-raw.json" 2>/dev/null
triage "go test" "$tmp/go.fail"

echo "== golangci-lint"
if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "SKIPPED: golangci-lint is not installed (this is not a pass)"
  add_skip golangci-lint
elif ! golangci-lint version 2>&1 | grep -q "version v\?$LINT_VERSION"; then
  echo "SKIPPED: golangci-lint is not v$LINT_VERSION, the version CI pins (this is not a pass)"
  add_skip "golangci-lint"
else
  (cd "$ROOT/backend" && golangci-lint run ./...) || { echo "NEW FAILURES (golangci-lint)"; new_red=$((new_red + 1)); }
fi

echo "== frontend"
(cd "$ROOT/frontend" && npm ci --no-audit --no-fund) >"$tmp/npmci.out" 2>&1 || { tail -n 40 "$tmp/npmci.out"; echo "npm ci failed"; exit 1; }
: >"$tmp/tsc.fail"
for script in typecheck typecheck:e2e; do
  (cd "$ROOT/frontend" && npm run "$script") >"$tmp/tsc.out" 2>&1
  rc=$?
  "$KR" tsc "$tmp/tsc.out" "$rc" >>"$tmp/tsc.fail"
done
sort -u -o "$tmp/tsc.fail" "$tmp/tsc.fail"
triage "typecheck" "$tmp/tsc.fail"

(cd "$ROOT/frontend" && npx vitest run --reporter=json --outputFile="$tmp/vitest.json") >"$tmp/vitest.out" 2>&1
rc=$?
(cd "$ROOT/frontend" && "$KR" vitest "$tmp/vitest.json" "$rc") >"$tmp/vitest.fail"
triage "vitest" "$tmp/vitest.fail"

echo "== codegen drift"
GEN_PATHS="backend/internal/storage/sqlite/gen frontend/src/api/schema.ts"
if (cd "$ROOT" && npm ci --no-audit --no-fund && npm run sqlc && npm run api) >"$tmp/gen.out" 2>&1; then
  # shellcheck disable=SC2086
  if ! (cd "$ROOT" && git diff --quiet -- $GEN_PATHS) ||
    [ -n "$(cd "$ROOT" && git ls-files --others --exclude-standard -- $GEN_PATHS)" ]; then
    echo "NEW FAILURES (codegen drift): generated sqlc/schema.ts files differ from the committed ones"
    new_red=$((new_red + 1))
  fi
  # Leave the tree as it was: generated output must never be staged or committed by the sync job.
  # shellcheck disable=SC2086
  (cd "$ROOT" && git checkout -- $GEN_PATHS && git clean -fdq -- $GEN_PATHS)
else
  tail -n 20 "$tmp/gen.out"
  echo "SKIPPED: codegen drift (the generators could not run; this is not a pass)"
  add_skip "codegen-drift"
  # shellcheck disable=SC2086
  (cd "$ROOT" && git checkout -- $GEN_PATHS 2>/dev/null; git clean -fdq -- $GEN_PATHS 2>/dev/null)
fi

[ "$known_red" != 0 ] && echo "CHECKS_KNOWN_RED=$known_red"
if [ "$new_red" != 0 ]; then
  echo "checks: RED, $new_red new failure(s)"
  exit 1
fi
if [ -n "$skipped" ]; then
  echo "CHECKS_SKIPPED=$skipped"
  echo "checks: green with skips: $skipped"
elif [ "$known_red" != 0 ]; then
  echo "checks: green apart from $known_red known-red failure(s)"
else
  echo "checks: all green"
fi
