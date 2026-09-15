#!/usr/bin/env bash
#
# deploy.sh — install a new Lira binary and restart the systemd service.
#
# Run this ON the Lira host (the ZimaBoard). It stops the service, swaps in the
# new binary (keeping a backup), restarts, and rolls back automatically if the
# new binary fails to come up.
#
# Usage:
#   sudo ./deploy.sh [path-to-binary]        # default: ./lira-linux-amd64
#
# Defaults can be overridden via environment, e.g.:
#   SERVICE=lira INSTALL_DIR=/opt/lira OWNER=lira:lira ./deploy.sh ./lira-linux-amd64
#
# Optional health probe after restart: set HEALTH_URL to the app's URL (export it
# from your deploy env / env file, or pass it inline). It defaults to a LOCAL probe
# so no public hostname is baked into the repo — set it to your real origin if you
# want to verify end-to-end, e.g.:
#   HEALTH_URL=https://lira.example.com/login sudo ./deploy.sh
#
set -euo pipefail

SERVICE="${SERVICE:-lira}"
INSTALL_DIR="${INSTALL_DIR:-/opt/lira}"
BINARY_NAME="${BINARY_NAME:-lira}"
OWNER="${OWNER:-lira:lira}"
# Default to a local probe (the app listens on :8080 on the host); override via env.
HEALTH_URL="${HEALTH_URL:-http://localhost:8080/login}"
SRC="${1:-./lira-linux-amd64}"

TARGET="$INSTALL_DIR/$BINARY_NAME"
BACKUP="$TARGET.bak"

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!  \033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx \033[0m %s\n' "$*" >&2; exit 1; }

# systemctl and writing under /opt/lira need root — re-exec via sudo if needed.
if [ "$(id -u)" -ne 0 ]; then
  exec sudo --preserve-env=SERVICE,INSTALL_DIR,BINARY_NAME,OWNER,HEALTH_URL "$0" "$@"
fi

command -v systemctl >/dev/null || die "systemctl not found — run this on the service host."
[ -f "$SRC" ] || die "binary not found: $SRC  (build it first with 'make linux')"
[ -s "$SRC" ] || die "binary is empty: $SRC"

# Guard against deploying a non-Linux build (e.g. a macOS binary from 'make build').
magic="$(head -c4 "$SRC" | od -An -tx1 | tr -d ' \n')"
[ "$magic" = "7f454c46" ] || die "not a Linux ELF binary (magic=$magic) — build with 'make linux'."

owner_user="${OWNER%%:*}"
owner_group="${OWNER##*:}"

log "Deploying $SRC -> $TARGET   (service: $SERVICE)"

# Keep a copy of the current binary so we can roll back.
if [ -f "$TARGET" ]; then
  cp -p "$TARGET" "$BACKUP"
  log "Backed up current binary -> $BACKUP"
fi

log "Stopping $SERVICE"
systemctl stop "$SERVICE" 2>/dev/null || warn "service was not running"

log "Installing new binary"
install -o "$owner_user" -g "$owner_group" -m 0755 "$SRC" "$TARGET"

log "Starting $SERVICE"
systemctl start "$SERVICE" || true
sleep 2

healthy() {
  systemctl is-active --quiet "$SERVICE" || return 1
  if [ -n "$HEALTH_URL" ] && command -v curl >/dev/null; then
    curl -fsS -o /dev/null --max-time 5 "$HEALTH_URL" || return 1
  fi
  return 0
}

if healthy; then
  log "$SERVICE is up ✔"
  systemctl --no-pager --lines=0 status "$SERVICE" || true
  exit 0
fi

# --- Failure path: roll back to the previous binary if we have one. ---
warn "$SERVICE did not come up cleanly. Recent logs:"
journalctl -u "$SERVICE" -n 20 --no-pager || true

if [ -f "$BACKUP" ]; then
  warn "Rolling back to the previous binary"
  systemctl stop "$SERVICE" 2>/dev/null || true
  install -o "$owner_user" -g "$owner_group" -m 0755 "$BACKUP" "$TARGET"
  systemctl start "$SERVICE" || true
  sleep 2
  if systemctl is-active --quiet "$SERVICE"; then
    die "Deploy failed — rolled back; the previous version is running again."
  fi
fi
die "Deploy failed and the service is down. Investigate: journalctl -u $SERVICE -e"
