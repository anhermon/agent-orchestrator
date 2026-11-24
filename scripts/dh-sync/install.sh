#!/usr/bin/env bash
# install.sh [--uninstall]: install (or remove) the daily launchd job for sync.sh.
# The job runs a copy of scripts/dh-sync under ~/Library/Application Support/ao-sync so
# it does not depend on which branch the main checkout has out.
set -eu

LABEL=io.anhermon.ao-sync
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST="$HOME/Library/Application Support/ao-sync"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
DOMAIN="gui/$(id -u)"

if [ "${1:-}" = "--uninstall" ]; then
  launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
  rm -f "$PLIST"
  rm -rf "$DEST"
  echo "uninstalled $LABEL"
  exit 0
fi

mkdir -p "$DEST" "$HOME/Library/LaunchAgents" "$HOME/Library/Logs/ao-sync"
cp "$HERE"/sync.sh "$HERE"/checks.sh "$HERE"/notify.sh "$HERE"/resolve-prompt.md "$DEST"/
chmod +x "$DEST"/*.sh
sed -e "s|__SCRIPT_DIR__|$DEST|g" -e "s|__HOME__|$HOME|g" "$HERE/$LABEL.plist" >"$PLIST"
plutil -lint "$PLIST" >/dev/null

launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
launchctl bootstrap "$DOMAIN" "$PLIST"
echo "installed $LABEL (daily 07:00); verify: launchctl print $DOMAIN/$LABEL"
