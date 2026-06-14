#!/usr/bin/env bash
set -euo pipefail

# Uninstall mirror-relay (handles both old and new installations)
# Old: binary at /usr/local/bin/mirror-relay
# New: binary at /usr/local/bin/relay

SERVICE="mirror-relay"
UNIT_FILE="/etc/systemd/system/${SERVICE}.service"
BIN_OLD="/usr/local/bin/mirror-relay"
BIN_NEW="/usr/local/bin/relay"
CONFIG_DIR="/etc/mirror-relay"
CACHE_DIR="/var/cache/mirror-relay"
USER="mirror-relay"

if [ "$(id -u)" -ne 0 ]; then
  echo "error: must be run as root (use sudo)" >&2
  exit 1
fi

echo "=== mirror-relay uninstall ==="
echo

# 1. Stop and disable service
if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
  echo "Stopping ${SERVICE}..."
  systemctl stop "$SERVICE"
fi

if systemctl is-enabled --quiet "$SERVICE" 2>/dev/null; then
  echo "Disabling ${SERVICE}..."
  systemctl disable "$SERVICE"
fi

# 2. Remove unit file
if [ -f "$UNIT_FILE" ]; then
  echo "Removing ${UNIT_FILE}"
  rm -f "$UNIT_FILE"
  systemctl daemon-reload
fi

# 3. Remove binaries
for bin in "$BIN_OLD" "$BIN_NEW"; do
  if [ -f "$bin" ]; then
    echo "Removing ${bin}"
    rm -f "$bin"
  fi
done

# 4. Remove cache
if [ -d "$CACHE_DIR" ]; then
  echo "Removing ${CACHE_DIR}"
  rm -rf "$CACHE_DIR"
fi

# 5. Remove config (ask)
if [ -d "$CONFIG_DIR" ]; then
  read -rp "Remove config directory ${CONFIG_DIR}? [y/N] " answer
  if [[ "$answer" =~ ^[Yy] ]]; then
    rm -rf "$CONFIG_DIR"
    echo "Removed ${CONFIG_DIR}"
  else
    echo "Kept ${CONFIG_DIR}"
  fi
fi

# 6. Remove user and group
if id "$USER" &>/dev/null; then
  echo "Removing user ${USER}"
  userdel "$USER" 2>/dev/null || true
fi
if getent group "$USER" &>/dev/null; then
  echo "Removing group ${USER}"
  groupdel "$USER" 2>/dev/null || true
fi

echo
echo "Done."
