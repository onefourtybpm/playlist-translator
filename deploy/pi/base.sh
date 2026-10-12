#!/usr/bin/env bash
# One-time base setup for the Raspberry Pi host: updates, automatic security updates, key-only SSH.
# Run on the Pi as root: sudo bash base.sh (it reboots at the end).
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt_opts=(-y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold)

echo "=== Updating packages (this can take several minutes)"
apt-get update
apt-get "${apt_opts[@]}" full-upgrade
apt-get "${apt_opts[@]}" install unattended-upgrades sqlite3 rsync

echo "=== Turning on automatic security updates"
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
systemctl enable --now unattended-upgrades

echo "=== SSH: keys only, no root login"
# sshd keeps the first value it reads, and this file sorts before 50-cloud-init.conf.
cat > /etc/ssh/sshd_config.d/10-hardening.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
EOF
sshd -t
systemctl reload ssh
sshd -T | grep -E '^(pubkeyauthentication|passwordauthentication|kbdinteractiveauthentication|permitrootlogin) '

echo "=== Base setup done. Rebooting in 5 seconds; the connection will close."
sleep 5
systemctl reboot
