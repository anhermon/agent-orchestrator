#!/usr/bin/env bash
# Fork-wide checks, run from the root of the tree under test (the sync worktree).
# A skipped item is never reported as plain green: it is printed as CHECKS_SKIPPED=<items>
# (parsed by sync.sh into sync.log) and in the final line.
set -eu

ROOT="$(pwd)"
skipped=""

echo "== gofmt"
unformatted="$(cd "$ROOT/backend" && gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "unformatted files:" "$unformatted"
  exit 1
fi

echo "== go build"
(cd "$ROOT/backend" && go build ./...)
echo "== go vet"
(cd "$ROOT/backend" && go vet ./...)
echo "== go test -race"
(cd "$ROOT/backend" && go test -race ./...)

echo "== golangci-lint"
if command -v golangci-lint >/dev/null 2>&1; then
  (cd "$ROOT/backend" && golangci-lint run ./...)
else
  echo "SKIPPED: golangci-lint is not installed (this is not a pass)"
  skipped="golangci-lint"
fi

echo "== frontend"
(
  cd "$ROOT/frontend"
  npm ci --no-audit --no-fund
  npm run typecheck
  npm test
)

if [ -n "$skipped" ]; then
  echo "CHECKS_SKIPPED=$skipped"
  echo "checks: green with skips: $skipped"
else
  echo "checks: all green"
fi
