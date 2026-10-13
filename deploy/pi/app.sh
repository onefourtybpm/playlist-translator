#!/usr/bin/env bash
# App setup for the Raspberry Pi host: service user, folders, secrets, systemd units. Safe to re-run.
# Inputs, copied to your home folder on the Pi first (each is optional on a re-run):
#   ~/spotify.env                       SPOTIFY_CLIENT_ID, SPOTIFY_CLIENT_SECRET
#   ~/apple.env + ~/AuthKey_<KeyID>.p8  APPLE_TEAM_ID, APPLE_KEY_ID, and the MusicKit private key
#   ~/signin-apple.env + ~/AuthKey_<KeyID>.p8
#                                       SIGN_IN_WITH_APPLE_SERVICES_ID, SIGN_IN_WITH_APPLE_KEY_ID, and that key
#   ~/systemd/                          the unit files from deploy/pi/systemd
#   ~/backup-db.sh                      the nightly backup script
# Settings without a new input file are kept from the existing env file. Input files are deleted once used.
# Run on the Pi as root: sudo bash app.sh
set -euo pipefail

deployer=${SUDO_USER:?run this with sudo from your own account}
home=$(getent passwd "$deployer" | cut -d: -f6)
conf=/etc/playlist-translator
env_file=$conf/env

echo "=== Service user 'playlist' (no login, no home folder)"
id playlist &>/dev/null || useradd --system --user-group --no-create-home --shell /usr/sbin/nologin playlist

echo "=== App folder /opt/playlist-translator/app (owned by $deployer so deploys don't need sudo)"
install -d -o root -g root -m 755 /opt/playlist-translator
install -d -o "$deployer" -g "$deployer" -m 755 /opt/playlist-translator/app /opt/playlist-translator/app/web

echo "=== Secrets in $conf"
# The folder is readable by the service's group so it can open the Apple key; the env file stays root-only.
install -d -o root -g playlist -m 750 "$conf"
keep() { [[ -f $env_file ]] && grep -E "^($1)=" "$env_file" || true; }

if [[ -f $home/spotify.env ]]; then
  spotify=$(grep -E '^SPOTIFY_CLIENT_(ID|SECRET)=' "$home/spotify.env")
else
  spotify=$(keep 'SPOTIFY_CLIENT_(ID|SECRET)')
fi
[[ -n $spotify ]] || { echo "Missing ~/spotify.env and no existing Spotify settings" >&2; exit 1; }

if [[ -f $home/apple.env ]]; then
  key_id=$(sed -n 's/^APPLE_KEY_ID=//p' "$home/apple.env")
  key_src=$home/AuthKey_$key_id.p8
  [[ -n $key_id && -f $key_src ]] || { echo "~/apple.env needs APPLE_KEY_ID and a matching ~/AuthKey_<KeyID>.p8" >&2; exit 1; }
  key_dst=$conf/AuthKey_$key_id.p8
  install -o root -g playlist -m 640 "$key_src" "$key_dst"
  # The Mac's apple.env has a relative dev path for the key, so the Pi path replaces it.
  apple=$(grep -E '^APPLE_(TEAM_ID|KEY_ID)=' "$home/apple.env"; echo "APPLE_PRIVATE_KEY_PATH=$key_dst")
else
  apple=$(keep 'APPLE_(TEAM_ID|KEY_ID|PRIVATE_KEY_PATH)')
fi

if [[ -f $home/signin-apple.env ]]; then
  siwa_key_id=$(sed -n 's/^SIGN_IN_WITH_APPLE_KEY_ID=//p' "$home/signin-apple.env")
  siwa_src=$home/AuthKey_$siwa_key_id.p8
  [[ -n $siwa_key_id && -f $siwa_src ]] || { echo "~/signin-apple.env needs SIGN_IN_WITH_APPLE_KEY_ID and a matching ~/AuthKey_<KeyID>.p8" >&2; exit 1; }
  siwa_dst=$conf/AuthKey_$siwa_key_id.p8
  install -o root -g playlist -m 640 "$siwa_src" "$siwa_dst"
  siwa=$(grep -E '^SIGN_IN_WITH_APPLE_(SERVICES_ID|KEY_ID)=' "$home/signin-apple.env"; echo "SIGN_IN_WITH_APPLE_KEY_PATH=$siwa_dst")
else
  siwa=$(keep 'SIGN_IN_WITH_APPLE_(SERVICES_ID|KEY_ID|KEY_PATH)')
fi

base_url="https://$(tailscale status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')"
(
  umask 077
  { echo "$spotify"; for part in "$apple" "$siwa"; do if [[ -n $part ]]; then echo "$part"; fi; done
    echo "BASE_URL=$base_url"; } > "$env_file.new"
)
mv "$env_file.new" "$env_file"
# Keys the settings no longer point at are old ones; remove them.
for key in "$conf"/AuthKey_*.p8; do
  if [[ -e $key ]] && ! grep -qxF "$key" <(sed -n 's/^[A-Z_]*_KEY_PATH=//p' "$env_file"); then
    rm -f "$key"
  fi
done
rm -f "$home/spotify.env" "$home/apple.env" "$home/signin-apple.env" "$home"/AuthKey_*.p8
echo "Wrote $env_file with: $(cut -d= -f1 "$env_file" | paste -sd' ' -)"

echo "=== Backup folder /var/backups/playlist-translator (readable by $deployer to copy off the Pi)"
# setgid, so backups the service user writes belong to the deployer's group.
install -d -o playlist -g "$deployer" -m 2750 /var/backups/playlist-translator
if [[ -f $home/backup-db.sh ]]; then
  install -o root -g root -m 755 "$home/backup-db.sh" /usr/local/libexec/playlist-translator-backup
  rm -f "$home/backup-db.sh"
fi

if [[ -d $home/systemd ]]; then
  echo "=== systemd units"
  install -o root -g root -m 644 "$home"/systemd/playlist-translator* /etc/systemd/system/
  systemctl daemon-reload
  # Starts at boot; on a fresh Pi the first deploy starts it (the binary doesn't exist yet).
  systemctl enable playlist-translator.service
  systemctl enable --now playlist-translator-restart.path playlist-translator-backup.timer
  rm -rf "$home/systemd"
fi

# Pick up the new settings if the app is already running.
systemctl try-restart playlist-translator.service

echo "=== Test backup"
systemctl start playlist-translator-backup.service
journalctl -u playlist-translator-backup --no-pager -n 3 -o cat
echo "=== Done"
