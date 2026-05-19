#!/usr/bin/env bash

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
SESSION_STATE="${PID_FILE}.session"
START_STATE="${PID_FILE}.started_at"
RECORDINGS_DIR="${RECORDINGS_DIR:-./recordings}"
LOG_DIR="${LOG_DIR:-./logs}"
DISK_SPACE_MIN_MB="${DISK_SPACE_MIN_MB:-500}"
SEGMENT_TIME_SECONDS="${SEGMENT_TIME_SECONDS:-800}"

is_running() {
  [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null
}

clear_state() {
  rm -f "$PID_FILE" "$SESSION_STATE" "$START_STATE"
}

cmd_start() {
  # Optional positional duration in seconds (0 / unset = no limit).
  local duration="${1:-0}"

  if is_running; then
    echo "record.sh: already recording (pid $(cat "$PID_FILE"), session $(cat "$SESSION_STATE" 2>/dev/null))" >&2
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

  local ts session_dir pattern logfile
  ts=$(date +%Y%m%d_%H%M%S)
  session_dir="${RECORDINGS_DIR%/}/session_${ts}"
  pattern="${session_dir}/part_%03d.mp4"
  logfile="${LOG_DIR%/}/session_${ts}.log"
  mkdir -p "$session_dir"

  # Optional total-duration cap. -t exits ffmpeg cleanly after N seconds; the
  # segment muxer still finalises the in-flight chunk.
  local duration_args=()
  if [[ -n "$duration" && "$duration" -gt 0 ]]; then
    duration_args=(-t "$duration")
  fi

  # ffmpeg with the segment muxer:
  #   -force_key_frames pins a keyframe at every segment boundary so cuts are
  #     clean (no held frame at the start of part_NNN+1).
  #   -reset_timestamps 1 makes each part start at PTS 0 — needed for players
  #     that expect each mp4 to be standalone.
  #   -segment_format mp4 + the encoder/audio chain mirrors what we used to
  #     write to a single mp4. The segment muxer keeps one continuous encode
  #     across all parts, so there's no AAC priming silence at boundaries.
  # We do NOT quote $FFMPEG_INPUT_ARGS — it contains multiple tokens that must
  # be split into separate argv entries.
  # shellcheck disable=SC2086
  nohup ffmpeg -hide_banner -nostdin -y \
    $FFMPEG_INPUT_ARGS \
    -c:v libx264 -preset veryfast -crf 23 \
    -force_key_frames "expr:gte(t,n_forced*${SEGMENT_TIME_SECONDS})" \
    -c:a aac \
    -f segment \
    -segment_time "$SEGMENT_TIME_SECONDS" \
    -segment_format mp4 \
    -reset_timestamps 1 \
    "${duration_args[@]}" \
    "$pattern" >/dev/null 2>"$logfile" &

  local pid=$!
  echo "$pid"         > "$PID_FILE"
  echo "$session_dir" > "$SESSION_STATE"
  date -u +%s         > "$START_STATE"

  # Give ffmpeg a moment to fail fast (bad device, perms, etc) so we surface
  # the error instead of reporting "recording" for a process that just died.
  sleep 0.5
  if ! is_running; then
    echo "record.sh: ffmpeg exited immediately — see $logfile" >&2
    tail -5 "$logfile" >&2 || true
    clear_state
    exit 4
  fi

  echo "recording pid=$pid session=$session_dir"
}

cmd_stop() {
  if ! is_running; then
    echo "record.sh: not currently recording" >&2
    # Still echo the last session dir if we have one.
    [[ -f "$SESSION_STATE" ]] && cat "$SESSION_STATE"
    clear_state
    exit 1
  fi

  local pid session
  pid=$(cat "$PID_FILE")
  session=$(cat "$SESSION_STATE")

  # SIGINT (not SIGKILL). ffmpeg uses this to finalise the in-flight segment;
  # without it, the last part_NNN.mp4 is unplayable (missing moov atom).
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
  echo "$session"
}

cmd_status() {
  if is_running; then
    local pid session started_at
    pid=$(cat "$PID_FILE")
    session=$(cat "$SESSION_STATE" 2>/dev/null || echo "<unknown>")
    started_at=$(cat "$START_STATE" 2>/dev/null || echo "0")
    local elapsed=$(( $(date -u +%s) - started_at ))
    echo "recording pid=$pid session=$session elapsed=${elapsed}s"
  else
    # If PID file exists but process died, clean up so `start` works next.
    [[ -f "$PID_FILE" ]] && clear_state
    echo "idle"
  fi
}

cmd_last() {
  if [[ -f "$SESSION_STATE" ]]; then
    cat "$SESSION_STATE"
  else
    echo "record.sh: no session state — nothing to report" >&2
    exit 1
  fi
}

case "${1:-}" in
  start)  shift; cmd_start  "$@" ;;
  stop)   cmd_stop   ;;
  status) cmd_status ;;
  last)   cmd_last   ;;
  *)
    echo "usage: $0 {start [duration_seconds]|stop|status|last}" >&2
    exit 2
    ;;
esac
