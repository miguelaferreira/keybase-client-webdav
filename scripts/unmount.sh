#!/bin/bash
# Usage: scripts/unmount.sh [mountpoint] (default /Volumes/Keybase)
set -euo pipefail
umount "${1:-/Volumes/Keybase}"
