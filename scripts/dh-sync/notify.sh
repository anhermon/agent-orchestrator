#!/usr/bin/env bash
# notify.sh "<text>"
# Always: macOS notification (osascript) and an appended line in the status file
# (<log dir>/status.log). Best effort: Slack DM to Angel via claude -p; reported as
# notify=sent only when the session's output confirms the send (SLACK_SENT), otherwise
# notify=local-only. Prints that status as its last line. Note: "sent" is the model's own
# confirmation of the tool call; it cannot be verified from here.
set -u

text="${1:?usage: notify.sh <text>}"
LOG_DIR="${AO_SYNC_LOG_DIR:-$HOME/Library/Logs/ao-sync}"
SLACK_TOOL="${AO_SYNC_SLACK_TOOL:-mcp__claude_ai_Slack__slack_send_message}"
mkdir -p "$LOG_DIR"

printf '%s %s\n' "$(date +%FT%T)" "$text" >>"$LOG_DIR/status.log"

osascript -e 'on run argv' -e 'display notification (item 1 of argv) with title "AO sync"' -e 'end run' "$text" >/dev/null 2>&1 \
  || echo "WARN osascript notification failed" >&2

status=local-only
if command -v claude >/dev/null 2>&1; then
  out="$(mktemp)"
  claude -p --max-turns 5 --allowedTools "$SLACK_TOOL" \
    "Using the Slack MCP connector, send this exact text as a DM to Angel (at most 2 lines). Then reply with exactly SLACK_SENT if the send succeeded, or SLACK_FAILED if not. Text: $text" \
    >"$out" 2>&1 </dev/null &
  pid=$!
  # The watcher must not hold the caller's stdout open ($(...) would wait for it).
  (sleep 120; kill "$pid" 2>/dev/null) >/dev/null 2>&1 &
  watcher=$!
  wait "$pid"
  kill "$watcher" 2>/dev/null
  pkill -P "$watcher" 2>/dev/null
  if grep -qx 'SLACK_SENT' "$out"; then
    status=sent
  else
    echo "WARN slack DM not confirmed" >&2
  fi
  rm -f "$out"
fi
echo "notify=$status"
