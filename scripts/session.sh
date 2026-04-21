#!/usr/bin/env bash
# session.sh — run a full recording session: record → wait → stop → upload.
#
#   ./scripts/session.sh
#
# Starts ffmpeg via record.sh, then blocks until you Ctrl-C (SIGINT) or send
# SIGTERM. On either signal, it tells record.sh to stop (which finalises the
# mp4), then hands the file to upload.sh. If upload succeeds, the local file
# is gone. If it fails, the file stays on disk with a .failed marker next to
# it for retry via `./scripts/upload.sh <file>`.
#
# This is the Phase 1 "done when" entry point — also what Phase 2's FastAPI
# app will eventually invoke (either by shelling out, or by calling record.sh
# + upload.sh directly).

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

RECORD="$SCRIPT_DIR/record.sh"
UPLOAD="$SCRIPT_DIR/upload-cdn.sh"

finished=0

on_signal() {
  # Guard against double-invocation if the user mashes Ctrl-C.
  (( finished )) && return
  finished=1

  echo ""  # newline after "^C"
  echo "session: signal received, stopping..."

  local file
  # record.sh stop echoes the finalised filename on stdout.
  file=$("$RECORD" stop) || {
    echo "session: record.sh stop failed" >&2
    exit 1
  }

  if [[ -z "$file" || ! -f "$file" ]]; then
    echo "session: no file to upload (stop returned '$file')" >&2
    exit 1
  fi

  echo "session: uploading $file"
  if "$UPLOAD" "$file"; then
    echo "session: done — $file uploaded and cleaned up"
    exit 0
  else
    rc=$?
    echo "session: upload.sh exited $rc — file retained at $file" >&2
    exit "$rc"
  fi
}

trap on_signal INT TERM

"$RECORD" start

echo "session: recording. Ctrl-C to stop and upload."

# Block until the ffmpeg process exits (either via our signal handler above,
# or externally — e.g. crash, or someone running `./scripts/record.sh stop`
# from another shell). Polling at 1Hz is cheap and simple; a PID-watch with
# `wait` isn't possible here because ffmpeg is a grandchild (spawned by
# record.sh, not by this shell).
while "$RECORD" status | grep -q '^recording'; do
  sleep 1
done

# If we get here, ffmpeg died without us signalling — maybe crashed. Try to
# upload whatever file record.sh last knew about, so we don't lose footage.
if (( ! finished )); then
  echo "session: ffmpeg exited unexpectedly — attempting salvage upload" >&2
  file=$("$RECORD" last 2>/dev/null || echo "")
  if [[ -n "$file" && -f "$file" ]]; then
    "$UPLOAD" "$file" || exit $?
  else
    echo "session: no recoverable file" >&2
    exit 1
  fi
fi
