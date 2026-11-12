#!/bin/bash
# Mount the running keybase-client-webdav server with macOS's built-in
# mount_webdav. Usage: scripts/mount.sh [mountpoint] (default /Volumes/Keybase)
set -euo pipefail

mountpoint=${1:-/Volumes/Keybase}
addr=${KBWEBDAV_ADDR:-127.0.0.1:8484}
token_file=${KBWEBDAV_TOKEN_FILE:-$HOME/.config/keybase-client-webdav/token}

[ -r "$token_file" ] || { echo "no token at $token_file; start the server once first" >&2; exit 1; }
token=$(tr -d '[:space:]' < "$token_file")

if mount | grep -q " on $mountpoint (webdav"; then
  echo "already mounted at $mountpoint"
  exit 0
fi

if [ ! -d "$mountpoint" ]; then
  # macOS clears /Volumes at boot, so this may need recreating.
  mkdir -p "$mountpoint" 2>/dev/null || { sudo mkdir -p "$mountpoint" && sudo chown "$USER" "$mountpoint"; }
fi

mount_webdav -S -v Keybase "http://$addr/$token/" "$mountpoint"
echo "mounted at $mountpoint"
