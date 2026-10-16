#!/bin/bash
# Install kv-chunk-fs as a host service. Run with sudo after ./build.sh.
#
#   ./build.sh && sudo deploy/install.sh
#
# Settings (environment variables, with their defaults):
#   KVCFS_USER=$SUDO_USER            the user the service runs as (owns the files)
#   KVCFS_PARENT=/srv/kv             plain parent directory; the mount is $KVCFS_PARENT/cache
#   KVCFS_BACKING=/srv/kv-cache.backing
#
# - binary to /opt/kv-chunk-fs/kv-chunk-fs-<version>, symlink kv-chunk-fs
# - user_allow_other in /etc/fuse.conf (the Docker daemon must reach the mount)
# - /etc/kv-chunk-fs.env (settings; only if missing), kv-chunk-fs.service, and a
#   drop-in ordering llama-swap after it
# - enables and (re)starts the service, then waits for status "healthy"
#
# Layout: the mount lives inside a plain parent directory, which the containers
# bind with propagation rslave (a bind of the mount itself would not survive a
# restart of the service). The mount point must be empty or hold nothing worth
# keeping: the mount hides it. To move from a plain snapshot directory, stop the
# llama.cpp models first and move it aside (it is a cache; nothing is imported).
set -euo pipefail
cd "$(dirname "$0")/.."
[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
BIN=bin/kv-chunk-fs
[ -x "$BIN" ] || { echo "$BIN missing - run ./build.sh first" >&2; exit 1; }
VERSION="$(git describe --always --dirty 2>/dev/null || echo dev)"
SVC_USER="${KVCFS_USER:-${SUDO_USER:-}}"
[ -n "$SVC_USER" ] && [ "$SVC_USER" != root ] || { echo "set KVCFS_USER (the non-root user the service runs as)" >&2; exit 1; }
PARENT="${KVCFS_PARENT:-/srv/kv}"
MNT="$PARENT/cache"
BACKING="${KVCFS_BACKING:-/srv/kv-cache.backing}"

install -d -o root -g root -m 755 /opt/kv-chunk-fs
install -o root -g root -m 755 "$BIN" "/opt/kv-chunk-fs/kv-chunk-fs-$VERSION"
ln -sfn "kv-chunk-fs-$VERSION" /opt/kv-chunk-fs/kv-chunk-fs

if ! grep -q '^user_allow_other' /etc/fuse.conf; then
  if grep -q '^#user_allow_other' /etc/fuse.conf; then
    sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
  else
    echo user_allow_other >> /etc/fuse.conf
  fi
fi

install -d -o "$SVC_USER" -g "$SVC_USER" -m 775 "$PARENT" "$MNT"
install -d -o "$SVC_USER" -g "$SVC_USER" -m 700 "$BACKING"
if [ ! -e /etc/kv-chunk-fs.env ]; then   # never overwrite local settings
  sed -e "s|^KVCFS_BACKING=.*|KVCFS_BACKING=$BACKING|" \
      -e "s|^KVCFS_MOUNT=.*|KVCFS_MOUNT=$MNT|" \
      deploy/kv-chunk-fs.env > /etc/kv-chunk-fs.env
  chown root:root /etc/kv-chunk-fs.env && chmod 644 /etc/kv-chunk-fs.env
fi
sed -e "s/^User=.*/User=$SVC_USER/" -e "s/^Group=.*/Group=$SVC_USER/" \
    -e "s|fusermount3 -uz .*|fusermount3 -uz $MNT|" \
    deploy/kv-chunk-fs.service > /etc/systemd/system/kv-chunk-fs.service
chown root:root /etc/systemd/system/kv-chunk-fs.service && chmod 644 /etc/systemd/system/kv-chunk-fs.service
install -d -m 755 /etc/systemd/system/llama-swap.service.d
install -o root -g root -m 644 deploy/llama-swap-after-kv-chunk-fs.conf \
  /etc/systemd/system/llama-swap.service.d/kv-chunk-fs.conf
systemctl daemon-reload
systemctl enable kv-chunk-fs.service >/dev/null
systemctl restart kv-chunk-fs.service

for _ in $(seq 60); do
  [ "$(head -1 "$MNT/status" 2>/dev/null)" = healthy ] && break
  sleep 1
done
echo "[install] kv-chunk-fs $VERSION: $(head -1 "$MNT/status" 2>/dev/null || echo 'no status - see journalctl -u kv-chunk-fs')"
