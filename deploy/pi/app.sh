#!/usr/bin/env bash
# One-time app setup for the Raspberry Pi host: service user, folders, secrets, systemd units.
# Before running, copy spotify.env to ~/spotify.env on the Pi and the systemd/ folder to ~/systemd.
# Run on the Pi as root: sudo bash app.sh
set -euo pipefail

deployer=${SUDO_USER:?run this with sudo from your own account}
home=$(getent passwd "$deployer" | cut -d: -f6)

echo "=== Service user 'playlist' (no login, no home folder)"
id playlist &>/dev/null || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin playlist

echo "=== App folder /opt/playlist-translator/app (owned by $deployer so deploys don't need sudo)"
install -d -o root -g root -m 755 /opt/playlist-translator
install -d -o "$deployer" -g "$deployer" -m 755 /opt/playlist-translator/app /opt/playlist-translator/app/web

echo "=== Secrets in /etc/playlist-translator/env (root only)"
install -d -o root -g root -m 700 /etc/playlist-translator
if [[ -f "$home/spotify.env" ]]; then
  base_url="https://$(tailscale status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')"
  umask 077
  { grep -E '^SPOTIFY_CLIENT_(ID|SECRET)=' "$home/spotify.env"; echo "BASE_URL=$base_url"; } > /etc/playlist-translator/env
  rm -f "$home/spotify.env"
  echo "Wrote env with BASE_URL=$base_url and removed ~/spotify.env"
elif [[ -f /etc/playlist-translator/env ]]; then
  echo "No ~/spotify.env; keeping the existing env file"
else
  echo "Missing ~/spotify.env and no existing env file" >&2
  exit 1
fi

echo "=== systemd units"
install -o root -g root -m 644 "$home"/systemd/playlist-translator{.service,-restart.path,-restart.service} /etc/systemd/system/
systemctl daemon-reload
# Starts at boot; the first deploy starts it now (the binary doesn't exist yet).
systemctl enable playlist-translator.service
systemctl enable --now playlist-translator-restart.path
rm -rf "$home/systemd"

echo "=== Done. Next: run deploy/pi/deploy.sh on the Mac."
