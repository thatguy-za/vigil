#!/usr/bin/env bash
# Install Docker, Portainer and Vigil on a 64-bit Raspberry Pi OS.
# Run as the pi user, without sudo:  bash install.sh
#
# Data goes in $APPDATA (default /home/pi/AppData):
#   config/       settings, users, event database
#   recordings/   video; put it on a USB SSD with
#                 RECORDINGS=/mnt/ssd/recordings bash install.sh
set -euo pipefail

APPDATA="${APPDATA:-/home/pi/AppData}"
CONFIG_DIR="${CONFIG_DIR:-$APPDATA/config}"
RECORDINGS="${RECORDINGS:-$APPDATA/recordings}"
VIGIL_IMAGE="${VIGIL_IMAGE:-ghcr.io/thatguy-za/vigil:latest}"
TZ_NAME="${TZ_NAME:-$(cat /etc/timezone 2>/dev/null || echo Etc/UTC)}"

say() { printf '\n%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -un)" = "pi" ] || die "run this as the pi user (you are $(id -un))"
[ "$(id -u)" -ne 0 ]   || die "don't run this with sudo; it calls sudo where needed"
[ "$(uname -m)" = "aarch64" ] || die "needs a 64-bit OS, this one is $(uname -m)"
command -v sudo >/dev/null || die "sudo is required"
sudo -v

# Docker
if command -v docker >/dev/null 2>&1; then
  say "Docker is already installed ($(docker --version))"
else
  say "Installing Docker"
  sudo apt-get update -y
  sudo apt-get install -y curl ca-certificates
  curl -fsSL https://get.docker.com | sudo sh
fi
sudo systemctl enable --now docker
if ! id -nG pi | tr ' ' '\n' | grep -qx docker; then
  sudo usermod -aG docker pi
fi

# The new group doesn't apply to this shell yet, so use sudo for docker here.
D="docker"
docker info >/dev/null 2>&1 || D="sudo docker"

# Folders, owned by pi
say "Creating $CONFIG_DIR and $RECORDINGS"
for d in "$CONFIG_DIR" "$RECORDINGS"; do
  sudo mkdir -p "$d"
  sudo chown pi:pi "$d"
done
PUID="$(id -u pi)"; PGID="$(id -g pi)"

# Portainer
say "Starting Portainer"
$D volume create portainer_data >/dev/null
$D rm -f portainer >/dev/null 2>&1 || true
$D run -d --name portainer --restart unless-stopped \
  -p 9443:9443 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v portainer_data:/data \
  portainer/portainer-ce:latest >/dev/null

# A fresh Portainer prints a one-time setup token and asks for it on first visit.
PORTAINER_TOKEN=""
for _ in $(seq 1 30); do
  PORTAINER_TOKEN="$($D logs portainer 2>&1 | grep -o 'setup_token=[0-9a-f]\+' | head -1 | cut -d= -f2 || true)"
  [ -n "$PORTAINER_TOKEN" ] && break
  sleep 1
done

# Vigil
say "Starting Vigil"
# The GPU video encoder/decoder nodes exist on the Pi 3B+ and 4, not the Pi 5.
DEVICES=()
for n in 10 11 12; do
  [ -e "/dev/video$n" ] && DEVICES+=(--device "/dev/video$n:/dev/video$n")
done

$D pull "$VIGIL_IMAGE" || die "could not pull $VIGIL_IMAGE. If the package is private, log in first:
  echo <github-token> | docker login ghcr.io -u <github-user> --password-stdin"
$D rm -f vigil >/dev/null 2>&1 || true
$D run -d --name vigil --restart unless-stopped \
  --network host \
  -e PUID="$PUID" -e PGID="$PGID" -e TZ="$TZ_NAME" \
  -v "$CONFIG_DIR":/config \
  -v "$RECORDINGS":/recordings \
  --tmpfs /tmp:size=128m \
  "${DEVICES[@]}" \
  "$VIGIL_IMAGE" >/dev/null

IP="$(hostname -I | awk '{print $1}')"
if [ -n "$PORTAINER_TOKEN" ]; then
  PORTAINER_NOTE="Portainer setup token: $PORTAINER_TOKEN"
else
  PORTAINER_NOTE="No Portainer setup token in the logs (already set up?). To check: docker logs portainer 2>&1 | grep setup_token"
fi

cat <<EOF

Vigil       http://$IP:8080
Portainer   https://$IP:9443
$PORTAINER_NOTE

Config      $CONFIG_DIR
Recordings  $RECORDINGS

Log out and back in once so docker works without sudo.
EOF
