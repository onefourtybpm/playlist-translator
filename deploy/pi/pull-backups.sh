#!/usr/bin/env bash
# Copy the Pi's nightly database backups to the Mac, into ../shh/pi-backups (outside the repo).
# They contain everyone's Spotify and Apple Music tokens, so they're kept private to you.
# Run on the Mac from anywhere: deploy/pi/pull-backups.sh   (PI_HOST overrides the source)
set -euo pipefail

host=${PI_HOST:-garrett-dev@playlist-translator-pi.local}
repo=$(cd "$(dirname "$0")/../.." && pwd)
dest=$repo/../shh/pi-backups

mkdir -p "$dest"
chmod 700 "$dest"
rsync -t "$host:/var/backups/playlist-translator/playlist-translator-*.db" "$dest/"
chmod 600 "$dest"/*.db
echo "Backups in $(cd "$dest" && pwd):"
ls -lh "$dest" | tail -n +2
