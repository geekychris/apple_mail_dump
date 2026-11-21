#!/bin/bash
# Install or upgrade mailbridge as a launchd user agent (no sudo).
#
#   ./install.sh [path/to/mailbridge] [extra serve flags...]
#
# The binary is installed at ~/Applications/MailBridge/mailbridge, a stable
# path to grant Full Disk Access to. It is only replaced when it differs from
# the installed copy: macOS ties the grant to the binary's code signature, so
# a NEW binary needs the grant again (System Settings → Privacy & Security →
# Full Disk Access: remove the old entry and add the new file).
set -euo pipefail

SRC="${1:-$(dirname "$0")/mailbridge}"
shift || true
EXTRA=("$@")

LABEL="com.geekychris.mailbridge"
APP_DIR="$HOME/Applications/MailBridge"
BIN="$APP_DIR/mailbridge"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME/Library/Logs/mailbridge.log"
STATE="$HOME/Library/Application Support/mailbridge"

[ -f "$SRC" ] || { echo "no binary at $SRC" >&2; exit 1; }
mkdir -p "$APP_DIR" "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"

replaced=no
if [ ! -f "$BIN" ] || ! cmp -s "$SRC" "$BIN"; then
  # Write to a temp name and rename so a running copy is never half-written.
  install -m 0755 "$SRC" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
  replaced=yes
fi
xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true

"$BIN" init --dir "$STATE"

args="    <string>$BIN</string>
    <string>serve</string>
    <string>--dir</string>
    <string>$STATE</string>"
for a in "${EXTRA[@]+"${EXTRA[@]}"}"; do
  args="$args
    <string>$a</string>"
done

cat > "$PLIST.tmp" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
$args
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$LOG</string>
  <key>StandardErrorPath</key><string>$LOG</string>
</dict>
</plist>
PL
plutil -lint "$PLIST.tmp" >/dev/null
mv -f "$PLIST.tmp" "$PLIST"

uid=$(id -u)
launchctl bootout "gui/$uid/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$uid" "$PLIST"
launchctl kickstart -k "gui/$uid/$LABEL" >/dev/null 2>&1 || true

echo
echo "installed: $BIN (binary replaced: $replaced)"
echo "agent:     $PLIST"
echo "log:       $LOG"
echo
echo "Full Disk Access: System Settings → Privacy & Security → Full Disk Access → + →"
echo "  press Cmd-Shift-G, paste $BIN, Open, and make sure its switch is on."
if [ "$replaced" = yes ]; then
  echo "  (This is a new binary: if an older mailbridge entry is listed, remove it with − first.)"
fi
echo "Then restart the agent:  launchctl kickstart -k gui/$uid/$LABEL"
