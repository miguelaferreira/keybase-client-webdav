#!/bin/bash
# Mount the keybase-client-webdav server if it is up and the mount is missing.
# Meant to be run repeatedly by launchd (see launchd/*-mount.plist); silent when there is nothing to do.
#
# This script never touches files under the mountpoint: a WebDAV mount whose
# server has gone away can block any process that reads it, uninterruptibly.
set -u
export PATH=/usr/bin:/bin:/usr/sbin:/sbin

mountpoint=${KBWEBDAV_MOUNTPOINT:-$HOME/Keybase}
addr=${KBWEBDAV_ADDR:-127.0.0.1:8484}
token_file=${KBWEBDAV_TOKEN_FILE:-$HOME/.config/keybase-client-webdav/token}
keybase=${KBWEBDAV_KEYBASE:-/Applications/Keybase.app/Contents/SharedSupport/bin/keybase}
here=$(cd "$(dirname "$0")" && pwd)

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*"; }

# bounded SECONDS CMD...: run CMD, SIGKILL it after SECONDS.
bounded() {
  local secs=$1; shift
  "$@" &
  local pid=$!
  ( sleep "$secs"; kill -9 "$pid" 2>/dev/null ) &
  local killer=$!
  wait "$pid" 2>/dev/null
  local rc=$?
  kill "$killer" 2>/dev/null
  { wait "$killer"; } 2>/dev/null
  return $rc
}

mount | grep -q " on $mountpoint (webdav" && exit 0

[ -r "$token_file" ] || { log "waiting: no token file yet"; exit 0; }
token=$(tr -d '[:space:]' < "$token_file")

curl -s -m 3 -o /dev/null "http://$addr/$token/" || { log "waiting: server not answering on $addr"; exit 0; }

# The Keybase service must be up and logged in, or every request would fail.
bounded 15 sh -c "\"$keybase\" status 2>/dev/null | grep -q 'Logged in:.*yes'" || { log "waiting: keybase not logged in yet"; exit 0; }

log "mounting at $mountpoint"
bounded 30 "$here/mount.sh" "$mountpoint" && log "mounted" || log "mount failed (rc=$?)"
