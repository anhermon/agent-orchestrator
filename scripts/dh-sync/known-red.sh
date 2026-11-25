#!/usr/bin/env bash
# Known-red support for checks.sh: normalises test/typecheck failures to "kind|scope|name" lines
# and filters out the ones listed in known-red.txt, so only NEW failures turn the checks red.
#
#   known-red.sh go <go-test-json-file> [exit-code]   failures from `go test -json`
#   known-red.sh tsc <tsc-output-file> [exit-code]    failures from tsgo/tsc output
#   known-red.sh vitest <vitest-json-file> [exit-code] failures from `vitest --reporter=json`
#   known-red.sh filter <known-red.txt>               stdin failures -> stdout failures NOT listed
#
# Entry format (known-red.txt, one per line, '#' comments): kind|scope|name
#   go|<package>|<TestName>        one failing Go test (top-level test; subtests roll up)
#   go|<package>|*                 the whole package is known red (missing CLI, timeout, build)
#   tsc|<file>|<TScode>            a typecheck error code in a file (line numbers are ignored)
#   vitest|<file>|<full test name> one failing vitest test
#   vitest|<file>|*                the whole vitest file is known red
# A non-zero exit code with nothing parsable is reported as kind|_run|* (never silently green).
set -u

go_failures() { # go_failures <json> <rc>
  awk -v rc="$2" '
    function field(line, key,   m, s) {
      s = line
      if (match(s, "\"" key "\":\"[^\"]*\"")) {
        m = substr(s, RSTART, RLENGTH)
        sub("^\"" key "\":\"", "", m); sub("\"$", "", m)
        return m
      }
      return ""
    }
    /"Action":"fail"/ {
      pkg = field($0, "Package"); test = field($0, "Test")
      if (pkg == "") next
      if (test == "") { pkgfail[pkg] = 1; next }
      if (index(test, "/") == 0) { out["go|" pkg "|" test] = 1; hastest[pkg] = 1 }
      else { hastest[pkg] = 1 }
    }
    END {
      for (p in pkgfail) if (!(p in hastest)) out["go|" p "|*"] = 1
      n = 0; for (k in out) { n++; print k }
      if (n == 0 && rc != 0) print "go|_run|*"
    }' "$1" | sort -u
}

tsc_failures() { # tsc_failures <output> <rc>
  local out
  out="$(sed -n 's/^\(.*\)([0-9]*,[0-9]*): error \(TS[0-9]*\):.*$/tsc|\1|\2/p' "$1" | sort -u)"
  if [ -z "$out" ] && [ "$2" != 0 ]; then
    echo "tsc|_run|*"
  else
    [ -n "$out" ] && echo "$out"
  fi
  return 0
}

vitest_failures() { # vitest_failures <json> <rc>
  local out
  out="$(node -e '
    const fs = require("fs"), path = require("path");
    let j; try { j = JSON.parse(fs.readFileSync(process.argv[1], "utf8")); } catch (e) { process.exit(3); }
    const out = new Set();
    for (const f of j.testResults || []) {
      const file = path.relative(process.cwd(), f.name);
      const failed = (f.assertionResults || []).filter((a) => a.status === "failed");
      for (const a of failed) out.add("vitest|" + file + "|" + (a.fullName || a.title));
      if (f.status === "failed" && failed.length === 0) out.add("vitest|" + file + "|*");
    }
    console.log([...out].sort().join("\n"));
  ' "$1" 2>/dev/null)" || out=""
  out="$(printf '%s\n' "$out" | sed '/^$/d')"
  if [ -z "$out" ] && [ "$2" != 0 ]; then
    echo "vitest|_run|*"
  else
    [ -n "$out" ] && echo "$out"
  fi
  return 0
}

filter_known() { # filter_known <known-red.txt>; stdin failures
  awk -F'|' '
    NR == FNR { if ($0 ~ /^[[:space:]]*(#|$)/) next; known[$0] = 1; next }
    {
      k = $0; kind = $1; scope = $2
      if (k in known || (kind "|" scope "|*") in known) next
      print k
    }' "$1" -
}

case "${1:-}" in
  go) go_failures "$2" "${3:-0}" ;;
  tsc) tsc_failures "$2" "${3:-0}" ;;
  vitest) vitest_failures "$2" "${3:-0}" ;;
  filter) filter_known "$2" ;;
  *) echo "usage: known-red.sh go|tsc|vitest <file> [rc] | filter <known-red.txt>" >&2; exit 2 ;;
esac
