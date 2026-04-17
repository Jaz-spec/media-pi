#!/usr/bin/env bash
# record.sh — manage a single ffmpeg recording via PID file
#
#   record.sh start   → spawn ffmpeg in background, write PID + filename state
#   record.sh stop    → SIGINT the ffmpeg, wait for it to finalise the .mp4
#   record.sh status  → "idle" or "recording <file> since <ts>"
#   record.sh last    → print the last session's filename (set by start)
#
# Intentionally small. No concurrency, no session database. Phase 2's FastAPI
# will call into this; the PID-file + filename pair is the state it reads.

set -euo pipefail

# Resolve repo root so the script works regardless of CWD
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

# Load config. .env is gitignored; .env.example is the template.
if [[ ! -f .env ]]; then
  echo "record.sh: .env not found — copy .env.example first" >&2
  exit 2
fi
set -a; source .env; set +a

PID_FILE="${PID_FILE:-/tmp/fac-recorder.pid}"
FILE_STATE="${PID_FILE}.file"
START_STATE="${PID_FILE}.started_at"
RECORDINGS_DIR="${RECORDINGS_DIR:-./recordings}"
LOG_DIR="${LOG_DIR:-./logs}"
DISK_SPACE_MIN_MB="${DISK_SPACE_MIN_MB:-500}"

is_running() {
  [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null
}

clear_state() {
  rm -f "$PID_FILE" "$FILE_STATE" "$START_STATE"
}

cmd_start() {
  if is_running; then
    echo "record.sh: already recording (pid $(cat "$PID_FILE"), file $(cat "$FILE_STATE" 2>/dev/null))" >&2
    exit 1
  fi
  # Stale state from a crashed run: clean up before checking disk.
  clear_state

  # Create dirs first so disk-full check can always find a valid path.
  mkdir -p "$RECORDINGS_DIR" "$LOG_DIR"

  # Disk-full guard. `|| true` prevents pipefail from killing the script.
  local avail_mb
  avail_mb=$(df -m "$RECORDINGS_DIR" 2>/dev/null | awk 'NR==2 {print $4}' || true)
  if [[ -z "$avail_mb" ]]; then
    avail_mb=$(df -m . | awk 'NR==2 {print $4}' || true)
  fi
  if [[ -n "$avail_mb" ]] && (( avail_mb < DISK_SPACE_MIN_MB )); then
    echo "record.sh: only ${avail_mb}MB free, need ${DISK_SPACE_MIN_MB}MB minimum" >&2
    exit 3
  fi

  local ts filename logfile
  ts=$(date +%Y%m%d_%H%M%S)
  filename="${RECORDINGS_DIR%/}/session_${ts}.mp4"
  logfile="${LOG_DIR%/}/session_${ts}.log"

  # ffmpeg: input flags come from .env, encoding is fixed (H.264 CRF 23 + AAC).
  # We do NOT quote $FFMPEG_INPUT_ARGS — it contains multiple tokens that must
  # be split into separate argv entries.
  # nohup + background + PID capture is the idiomatic way to own a long-running
  # subprocess from a short-lived script.
  # shellcheck disable=SC2086
  nohup ffmpeg -hide_banner -nostdin -y \
    $FFMPEG_INPUT_ARGS \
    -c:v libx264 -preset veryfast -crf 23 \
    -c:a aac \
    -movflags frag_keyframe+empty_moov \
    "$filename" >/dev/null 2>"$logfile" &

  local pid=$!
  echo "$pid"      > "$PID_FILE"
  echo "$filename" > "$FILE_STATE"
  date -u +%s      > "$START_STATE"

  # Give ffmpeg a moment to fail fast (bad device, perms, etc) so we surface
  # the error instead of reporting "recording" for a process that just died.
  sleep 0.5
  if ! is_running; then
    echo "record.sh: ffmpeg exited immediately — see $logfile" >&2
    tail -5 "$logfile" >&2 || true
    clear_state
    exit 4
  fi

  echo "recording pid=$pid file=$filename"
}

cmd_stop() {
  if ! is_running; then
    echo "record.sh: not currently recording" >&2
    # Still echo the last filename if we have one, so callers can pipe to upload
    [[ -f "$FILE_STATE" ]] && cat "$FILE_STATE"
    clear_state
    exit 1
  fi

  local pid file
  pid=$(cat "$PID_FILE")
  file=$(cat "$FILE_STATE")

  # SIGINT (not SIGKILL). ffmpeg uses this to finalise the mp4 — writing the
  # `moov` atom with chunk offsets. Without it, the file is unplayable.
  kill -INT "$pid"

  # Bounded wait for graceful exit. 10s is generous for the finalise step.
  local waited=0
  while kill -0 "$pid" 2>/dev/null; do
    (( waited >= 10 )) && break
    sleep 0.5
    waited=$((waited + 1))
  done

  if kill -0 "$pid" 2>/dev/null; then
    echo "record.sh: ffmpeg did not exit within 10s, escalating to SIGTERM" >&2
    kill -TERM "$pid" || true
    sleep 2
  fi

  clear_state
  echo "$file"
}

cmd_status() {
  if is_running; then
    local pid file started_at
    pid=$(cat "$PID_FILE")
    file=$(cat "$FILE_STATE" 2>/dev/null || echo "<unknown>")
    started_at=$(cat "$START_STATE" 2>/dev/null || echo "0")
    local elapsed=$(( $(date -u +%s) - started_at ))
    echo "recording pid=$pid file=$file elapsed=${elapsed}s"
  else
    # If PID file exists but process died, clean up so `start` works next.
    [[ -f "$PID_FILE" ]] && clear_state
    echo "idle"
  fi
}

cmd_last() {
  if [[ -f "$FILE_STATE" ]]; then
    cat "$FILE_STATE"
  else
    echo "record.sh: no session state — nothing to report" >&2
    exit 1
  fi
}

case "${1:-}" in
  start)  cmd_start  ;;
  stop)   cmd_stop   ;;
  status) cmd_status ;;
  last)   cmd_last   ;;
  *)
    echo "usage: $0 {start|stop|status|last}" >&2
    exit 2
    ;;
esac
