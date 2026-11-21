#!/bin/bash
# Remove the mailbridge launchd agent and binary.  --purge also deletes the
# token, certificate and snapshots (consumers then need a new token and pin).
set -euo pipefail
LABEL="com.geekychris.mailbridge"
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
rm -f "$HOME/Library/LaunchAgents/$LABEL.plist"
rm -rf "$HOME/Applications/MailBridge"
if [ "${1:-}" = "--purge" ]; then
  rm -rf "$HOME/Library/Application Support/mailbridge"
  echo "removed state (token, certificate, snapshots)"
else
  rm -rf "$HOME/Library/Application Support/mailbridge/snapshots"
fi
echo "uninstalled. Also remove 'mailbridge' from System Settings → Privacy & Security → Full Disk Access."
