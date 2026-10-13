#!/usr/bin/env bash
# Nightly SQLite backup. app.sh installs this as /usr/local/libexec/playlist-translator-backup and
# playlist-translator-backup.timer runs it as the service user.
set -euo pipefail

db=/var/lib/playlist-translator/playlist-translator.db
dir=/var/backups/playlist-translator
keep_days=14

if [[ ! -f $db ]]; then
  echo "No database yet; nothing to back up"
  exit 0
fi

out=$dir/playlist-translator-$(date +%F).db
tmp=$out.tmp
rm -f "$tmp"
# .backup takes a consistent copy while the app keeps running; .timeout waits out a write in progress.
sqlite3 -bail -cmd '.timeout 10000' "$db" ".backup '$tmp'"
# The copy inherits the app's WAL mode; switch it to a plain single file with no -wal/-shm beside it.
sqlite3 "$tmp" 'PRAGMA journal_mode=DELETE' >/dev/null
rm -f "$tmp-wal" "$tmp-shm"
check=$(sqlite3 "$tmp" 'PRAGMA integrity_check')
if [[ $check != ok ]]; then
  echo "Backup failed its integrity check: $check" >&2
  rm -f "$tmp"
  exit 1
fi
mv "$tmp" "$out"
find "$dir" -name 'playlist-translator-*.db' -mtime +"$keep_days" -delete
echo "Backed up to $out ($(du -h "$out" | cut -f1))"
