#!/usr/bin/env bash
# Regenerates scripts/dh-sync/known-red.txt from the CURRENT tree (run it from a clean checkout of
# dh/main, after `npm ci` in frontend/). It runs the same suites checks.sh runs and records every
# failure at test level (Go "pkg|Test", vitest "file|test", tsc "file|TScode"). It can take 20+
# minutes because it runs the full Go and vitest suites.
#
#   scripts/dh-sync/regen-known-red.sh [--out PATH]      (default: scripts/dh-sync/known-red.txt)
#
# Package-level entries ("go|pkg|*", "vitest|file|*") are kept from the existing file: they cover
# environment failures (missing CLIs, timeouts, build failures) that cannot be named per test.
# Review the diff before committing; entries should only ever be removed once fixed.
set -eu
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(git rev-parse --show-toplevel)"
OUT="$HERE/known-red.txt"
[ "${1:-}" = "--out" ] && OUT="$2"
KR="$HERE/known-red.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

: >"$tmp/new"
(cd "$ROOT/backend" && go test -race -count=1 -timeout=10m -json ./... >"$tmp/go.json" 2>"$tmp/go.err") && rc=0 || rc=$?
"$KR" go "$tmp/go.json" "$rc" >>"$tmp/new"
for script in typecheck typecheck:e2e; do
  (cd "$ROOT/frontend" && npm run "$script" >"$tmp/tsc.out" 2>&1) && rc=0 || rc=$?
  "$KR" tsc "$tmp/tsc.out" "$rc" >>"$tmp/new"
done
(cd "$ROOT/frontend" && npx vitest run --reporter=json --outputFile="$tmp/vitest.json" >"$tmp/vitest.out" 2>&1) && rc=0 || rc=$?
(cd "$ROOT/frontend" && "$KR" vitest "$tmp/vitest.json" "$rc") >>"$tmp/new"

{
  echo "# Known-red failures on dh/main (see regen-known-red.sh). checks.sh fails only on failures NOT listed here."
  echo "# Format: kind|scope|name  (name '*' = whole package/file). Regenerated $(date -u +%F)."
  { grep -E '^(go|vitest)\|[^|]*\|\*$' "$OUT" 2>/dev/null || true; cat "$tmp/new"; } | sort -u
} >"$tmp/out"
mv "$tmp/out" "$OUT"
echo "wrote $OUT ($(grep -vc '^#' "$OUT") entries)"
