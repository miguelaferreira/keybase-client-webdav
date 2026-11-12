#!/bin/bash
# Install two per-user launch agents: the server, and a mounter that keeps it mounted.
# Usage: scripts/install-launchd.sh /path/to/keybase-client-webdav [/path/to/keybase]
set -euo pipefail

bin=$(cd "$(dirname "${1:?path to the built binary}")" && pwd)/$(basename "$1")
keybase=${2:-/Applications/Keybase.app/Contents/SharedSupport/bin/keybase}
root=$(cd "$(dirname "$0")/.." && pwd)
label=com.github.miguelaferreira.keybase-client-webdav

install_agent() { # install_agent LABEL
  local l=$1 dest=$HOME/Library/LaunchAgents/$1.plist
  sed -e "s|__BIN__|$bin|" -e "s|__KEYBASE__|$keybase|" -e "s|__HOME__|$HOME|" \
      -e "s|__AUTOMOUNT__|$root/scripts/automount.sh|" "$root/launchd/$l.plist" > "$dest"
  launchctl bootout "gui/$(id -u)/$l" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$dest"
  echo "installed $dest"
}

install_agent "$label"
install_agent "$label-mount"
