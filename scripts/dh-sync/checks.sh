#!/usr/bin/env bash
# Fork-wide checks, run from the root of the tree under test (the sync worktree).
set -eu

ROOT="$(pwd)"

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
fi

echo "== frontend"
(
  cd "$ROOT/frontend"
  npm ci --no-audit --no-fund
  npm run typecheck
  npm test
)
echo "checks: all green"
