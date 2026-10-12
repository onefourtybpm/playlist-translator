#!/usr/bin/env bash
# Build the server for the Pi plus the frontend, upload both, and restart the service.
# Run on the Mac from anywhere: deploy/pi/deploy.sh   (PI_HOST overrides the target)
set -euo pipefail

host=${PI_HOST:-garrett-dev@playlist-translator-pi.local}
repo=$(cd "$(dirname "$0")/../.." && pwd)
app=/opt/playlist-translator/app
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

echo "=== Building server (linux/arm64)"
(cd "$repo/server" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$out/playlist-translator" .)

echo "=== Building frontend"
(cd "$repo/web" && npm run build)

echo "=== Uploading to $host"
rsync -rtz --delete "$repo/web/dist/" "$host:$app/web/"
rsync -tz "$out/playlist-translator" "$host:$app/playlist-translator.new"
# The service user needs to read everything. Swapping in the new binary and writing .deployed
# triggers playlist-translator-restart.path.
ssh "$host" "chmod -R u=rwX,go=rX $app/web && chmod 755 $app/playlist-translator.new && \
  mv $app/playlist-translator.new $app/playlist-translator && date > $app/.deployed"

echo "=== Checking the service"
for _ in {1..10}; do
  sleep 1
  if ssh "$host" 'curl -fsS -o /dev/null http://127.0.0.1:8080/'; then
    ssh "$host" 'systemctl status playlist-translator --no-pager --lines=0 | head -3'
    echo "=== Deployed"
    exit 0
  fi
done
echo "The service didn't respond. Recent logs:" >&2
ssh "$host" 'journalctl -u playlist-translator --no-pager -n 30' >&2
exit 1
