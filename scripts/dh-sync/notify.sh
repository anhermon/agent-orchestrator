#!/usr/bin/env bash
# notify.sh "<text>"
# Always: a line in the status file (<log dir>/status.log) and a macOS notification (osascript).
# Best effort: Slack DM to Angel via claude -p. Prints a status as its last line:
#   notify=sent                 Slack DM confirmed by the session (SLACK_SENT) and banner shown
#   notify=sent-no-banner       DM confirmed but osascript failed
#   notify=local-only           banner shown, no confirmed DM
#   notify=osascript-failed     nothing but the status file
# "sent" is the model's own confirmation of its tool call; it cannot be verified from here.
# The Slack session loads user settings only (no project/local settings) and needs the claude.ai
# Slack connector, so --strict-mcp-config cannot be used (it also loads the user's other MCP servers).
# The text may contain resolver- or upstream-authored strings (file names, check output), so the
# prompt gets a sanitised copy (A-Za-z0-9 and . _ : / = ( ) , -; at most 400 chars) and the session
# is denied every tool except the Slack send (--disallowedTools; UNVERIFIED offline: confirm the
# flag form in a live run). The banner and status file keep the original text.
# No --max-turns (not in claude --help): the session is bounded by a 120 s watcher.
set -u

text="${1:?usage: notify.sh <text>}"
LOG_DIR="${AO_SYNC_LOG_DIR:-$HOME/Library/Logs/ao-sync}"
SLACK_TOOL="${AO_SYNC_SLACK_TOOL:-mcp__claude_ai_Slack__slack_send_message}"
mkdir -p "$LOG_DIR"

printf '%s %s\n' "$(date +%FT%T)" "$text" >>"$LOG_DIR/status.log"

banner=1
if ! osascript -e 'on run argv' -e 'display notification (item 1 of argv) with title "AO sync"' -e 'end run' "$text" >/dev/null 2>&1; then
  echo "WARN osascript notification failed" >&2
  banner=0
  printf '%s %s\n' "$(date +%FT%T)" "WARN osascript notification failed" >>"$LOG_DIR/status.log"
fi

slack=0
safe="$(printf '%s' "$text" | tr -c 'A-Za-z0-9 ._:/=(),-' ' ' | tr -s ' ' | cut -c1-400)"
if command -v claude >/dev/null 2>&1; then
  out="$(mktemp)"
  set -m
  claude -p --setting-sources user --allowedTools "$SLACK_TOOL" \
    --disallowedTools "Read,Grep,Glob,Bash,Edit,Write,WebFetch,WebSearch" \
    "Using the Slack MCP connector, send this exact text as a DM to Angel (at most 2 lines). Then reply with exactly SLACK_SENT if the send succeeded, or SLACK_FAILED if not. Text: $safe" \
    >"$out" 2>&1 </dev/null &
  pid=$!
  # The watcher must not hold the caller's stdout open ($(...) would wait for it).
  (sleep 120; kill -TERM -- "-$pid" 2>/dev/null) >/dev/null 2>&1 &
  watcher=$!
  set +m
  wait "$pid"
  rc=$?
  kill -TERM -- "-$watcher" 2>/dev/null
  kill -TERM -- "-$pid" 2>/dev/null
  if grep -qx 'SLACK_SENT' "$out"; then
    slack=1
  elif [ "$rc" != 0 ] && grep -qiE 'unknown option|unrecognized (option|argument)|invalid (option|argument)|^usage:|error: (unknown|too many|required)' "$out"; then
    echo "WARN slack DM failed: claude rejected its arguments" >&2
    printf '%s %s\n' "$(date +%FT%T)" "WARN slack DM failed: claude rejected its arguments" >>"$LOG_DIR/status.log"
  else
    echo "WARN slack DM not confirmed" >&2
  fi
  rm -f "$out"
fi

if [ "$slack" = 1 ] && [ "$banner" = 1 ]; then
  echo "notify=sent"
elif [ "$slack" = 1 ]; then
  echo "notify=sent-no-banner"
elif [ "$banner" = 1 ]; then
  echo "notify=local-only"
else
  echo "notify=osascript-failed"
fi
