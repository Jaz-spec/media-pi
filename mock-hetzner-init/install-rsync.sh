#!/bin/sh
# Runs on mock-hetzner container startup (linuxserver's /custom-cont-init.d/).
# Installs rsync so the container matches real Hetzner Storage Box, which
# supports rsync over SSH. Without this, `upload.sh` fails with
# "rsync: command not found" when the local rsync tries to exec the remote half.
set -e
if ! command -v rsync >/dev/null 2>&1; then
  echo "[install-rsync.sh] installing rsync via apk"
  apk add --no-cache rsync
else
  echo "[install-rsync.sh] rsync already present"
fi
