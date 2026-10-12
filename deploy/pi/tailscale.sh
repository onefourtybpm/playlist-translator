#!/usr/bin/env bash
# One-time Tailscale setup for the Raspberry Pi host.
# Run on the Pi as root: sudo bash tailscale.sh (it prints a login link to open in your browser).
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
codename=$(. /etc/os-release && echo "$VERSION_CODENAME")
operator=${SUDO_USER:?run this with sudo from your own account}

echo "=== Adding Tailscale's package repository (Debian $codename)"
curl -fsSL "https://pkgs.tailscale.com/stable/debian/$codename.noarmor.gpg" \
  -o /usr/share/keyrings/tailscale-archive-keyring.gpg
curl -fsSL "https://pkgs.tailscale.com/stable/debian/$codename.tailscale-keyring.list" \
  -o /etc/apt/sources.list.d/tailscale.list

echo "=== Installing Tailscale"
apt-get update
apt-get -y install tailscale
systemctl enable --now tailscaled

echo "=== Joining your tailnet: open the link below and sign in"
tailscale up

# Let your account run tailscale commands (status, serve, funnel) without sudo, and keep Tailscale updated.
tailscale set --operator="$operator" --auto-update

echo "=== Done"
tailscale status --self --peers=false
