#!/usr/bin/env bash
# notify.sh "<text>": macOS notification plus a Slack DM to Angel through claude -p.
# Prints notify=<ok|local-only> as its last line.
set -u

text="${1:?usage: notify.sh <text>}"

osascript -e 'on run argv' -e 'display notification (item 1 of argv) with title "AO sync"' -e 'end run' "$text" >/dev/null 2>&1 \
  || echo "WARN osascript notification failed" >&2

if command -v claude >/dev/null 2>&1; then
  if claude -p --max-turns 5 \
    "Using the Slack MCP connector, send this exact text as a DM to Angel (at most 2 lines): $text" >/dev/null 2>&1; then
    echo "notify=ok"
    exit 0
  fi
  echo "WARN slack DM via claude failed" >&2
fi
echo "notify=local-only"
