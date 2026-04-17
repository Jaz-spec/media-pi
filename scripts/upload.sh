#!/usr/bin/env bash
# upload.sh — transfer a local recording to the remote, verify, clean up.
#
#   upload.sh <file>
#
# Flow:
#   1. Sanity-check the file exists, is non-zero, and isn't currently being written.
#   2. Compute local sha256.
#   3. rsync to $REMOTE_HOST:$REMOTE_PATH. Retry with exponential backoff on
#      rsync failure (network / transient).
#   4. Compute remote sha256 via `ssh <host> sha256sum <remote-file>`.
#   5. Hashes match → delete local. Hashes differ → leave local, drop .failed
#      marker. (We do NOT retry on mismatch — that's corruption, not transient.)
#
# Exit codes:
#   0 success (file uploaded, verified, local deleted)
#   1 usage error
#   2 file missing / empty / still being written
#   3 rsync exhausted retries
#   4 hash mismatch after upload
#   5 remote hash could not be computed

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

if [[ ! -f .env ]]; then
  echo "upload.sh: .env not found — copy .env.example first" >&2
  exit 1
fi
set -a; source .env; set +a

REMOTE_HOST="${REMOTE_HOST:?REMOTE_HOST not set in .env}"
REMOTE_PATH="${REMOTE_PATH:?REMOTE_PATH not set in .env}"
UPLOAD_MAX_ATTEMPTS="${UPLOAD_MAX_ATTEMPTS:-3}"
UPLOAD_BACKOFF_START_SECONDS="${UPLOAD_BACKOFF_START_SECONDS:-1}"
LOG_DIR="${LOG_DIR:-./logs}"
PID_FILE="${PID_FILE:-/tmp/fac-recorder.pid}"

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <file>" >&2
  exit 1
fi
FILE="$1"

if [[ ! -f "$FILE" ]]; then
  echo "upload.sh: file not found: $FILE" >&2
  exit 2
fi

if [[ ! -s "$FILE" ]]; then
  echo "upload.sh: file is empty: $FILE" >&2
  exit 2
fi

# If record.sh is still writing this file, refuse. Upload of an open file can
# race with mp4 finalisation.
if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  current_file=$(cat "${PID_FILE}.file" 2>/dev/null || true)
  if [[ "$current_file" == "$FILE" ]]; then
    echo "upload.sh: $FILE is currently being recorded — stop first" >&2
    exit 2
  fi
fi

mkdir -p "$LOG_DIR"
LOGFILE="${LOG_DIR%/}/upload_$(basename "${FILE%.*}").log"

log() {
  echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*" | tee -a "$LOGFILE" >&2
}

# sha256 — use the tool present on each platform. macOS has shasum; Linux has
# sha256sum. On the Pi, sha256sum will be used natively.
local_sha256() {
  if command -v sha256sum >/dev/null; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

FILE_BASENAME=$(basename "$FILE")
REMOTE_FILE="${REMOTE_PATH%/}/$FILE_BASENAME"

log "start upload $FILE → $REMOTE_HOST:$REMOTE_FILE"
LOCAL_HASH=$(local_sha256 "$FILE")
log "local sha256 $LOCAL_HASH"

# rsync with exponential backoff.
# --partial keeps the partial file on the remote so a retry can resume it.
# -T (--inplace not used): rsync default (rename from .tmp) is fine; the remote
# side only sees the final filename when the transfer completes.
attempt=1
backoff=$UPLOAD_BACKOFF_START_SECONDS
while true; do
  rc=0
  # `|| rc=$?` captures the real rsync exit code without tripping `set -e`.
  rsync -az --partial --timeout=30 "$FILE" "${REMOTE_HOST}:${REMOTE_PATH%/}/" >>"$LOGFILE" 2>&1 || rc=$?
  if (( rc == 0 )); then
    log "rsync attempt $attempt: ok"
    break
  fi
  log "rsync attempt $attempt failed (exit $rc)"
  if (( attempt >= UPLOAD_MAX_ATTEMPTS )); then
    log "rsync exhausted $UPLOAD_MAX_ATTEMPTS attempts — giving up"
    echo "${FILE}.failed: rsync exhausted retries at $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "${FILE}.failed"
    exit 3
  fi
  log "backing off ${backoff}s"
  sleep "$backoff"
  backoff=$((backoff * 2))
  attempt=$((attempt + 1))
done

# Remote verify. ssh returns the remote sha256sum output; we parse the first field.
# sha256sum is present on the linuxserver/openssh-server image and on any Debian/Pi host.
REMOTE_HASH=$(ssh "$REMOTE_HOST" "sha256sum $REMOTE_FILE 2>/dev/null" | awk '{print $1}' || true)

if [[ -z "$REMOTE_HASH" ]]; then
  log "could not compute remote sha256 — leaving local file in place"
  echo "${FILE}.failed: remote sha256 empty at $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "${FILE}.failed"
  exit 5
fi

log "remote sha256 $REMOTE_HASH"

if [[ "$LOCAL_HASH" != "$REMOTE_HASH" ]]; then
  log "HASH MISMATCH — local=$LOCAL_HASH remote=$REMOTE_HASH"
  echo "${FILE}.failed: hash mismatch local=$LOCAL_HASH remote=$REMOTE_HASH at $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "${FILE}.failed"
  exit 4
fi

log "verified — deleting local $FILE"
rm -f "$FILE"
log "done"
