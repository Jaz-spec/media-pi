#!/usr/bin/env bash
# upload-cdn.sh — register a draft, PUT to MinIO, confirm, clean up local.
#
#   upload-cdn.sh <file>
#
# Flow (all HTTP, no rsync):
#   1. Sanity-check the file exists, is non-zero, and isn't currently being
#      written by record.sh.
#   2. POST a GraphQL watch_ingest_register mutation → { video_id, object_key,
#      upload_url }. Authenticates via Bearer $FAC_API_KEY.
#   3. HTTP PUT the file at upload_url. Retry with exponential backoff on
#      transient network errors.
#   4. POST watch_ingest_confirm(video_id, object_key) → the DRAFT row in
#      watch.videos gets its video_url populated.
#   5. Delete local file.
#
# Exit codes:
#   0 success (file uploaded, confirmed, local deleted)
#   1 usage / config error
#   2 file missing / empty / still being written
#   3 PUT exhausted retries
#   4 register failed (API unreachable, auth rejected, bad ext, ...)
#   5 confirm failed (PUT completed — row still has video_url=NULL on server;
#     the local file is retained so confirm can be re-run by hand)

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

if [[ ! -f .env ]]; then
  echo "upload-cdn.sh: .env not found — copy .env.example first" >&2
  exit 1
fi
set -a; source .env; set +a

: "${FAC_API_URL:?FAC_API_URL not set in .env}"
: "${FAC_API_KEY:?FAC_API_KEY not set in .env}"
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
  echo "upload-cdn.sh: file not found: $FILE" >&2
  exit 2
fi
if [[ ! -s "$FILE" ]]; then
  echo "upload-cdn.sh: file is empty: $FILE" >&2
  exit 2
fi

# If record.sh is still writing this file, refuse.
if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  current_file=$(cat "${PID_FILE}.file" 2>/dev/null || true)
  if [[ "$current_file" == "$FILE" ]]; then
    echo "upload-cdn.sh: $FILE is currently being recorded — stop first" >&2
    exit 2
  fi
fi

mkdir -p "$LOG_DIR"
LOGFILE="${LOG_DIR%/}/upload_$(basename "${FILE%.*}").log"

log() {
  echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*" | tee -a "$LOGFILE" >&2
}

EXT="${FILE##*.}"
EXT_LC=$(echo "$EXT" | tr '[:upper:]' '[:lower:]')

# --- 1. register ------------------------------------------------------------
log "register: POST $FAC_API_URL (ext=$EXT_LC)"

REGISTER_BODY=$(jq -nc --arg ext "$EXT_LC" '{
  source: "mutation WatchIngestRegister($ext:String!){ watch_ingest_register(ext:$ext) }",
  variableValues: { ext: $ext }
}')

REG=$(curl -fsS -X POST "$FAC_API_URL" \
  -H "Authorization: Bearer $FAC_API_KEY" \
  -H "Content-Type: application/json" \
  --data "$REGISTER_BODY" 2>>"$LOGFILE") || {
    log "register: curl failed"
    exit 4
  }

# The /g endpoint strips the GraphQL envelope (see fac-cra api/app.ts), so the
# resolver's { video_id, object_key, upload_url } arrives at the top level.
UPLOAD_URL=$(echo "$REG" | jq -r '.upload_url // empty')
VIDEO_ID=$(echo "$REG"  | jq -r '.video_id // empty')
OBJECT_KEY=$(echo "$REG" | jq -r '.object_key // empty')

if [[ -z "$UPLOAD_URL" || -z "$VIDEO_ID" || -z "$OBJECT_KEY" ]]; then
  log "register: bad response — $REG"
  exit 4
fi
log "register: video_id=$VIDEO_ID object_key=$OBJECT_KEY"

# --- 2. PUT with retry ------------------------------------------------------
attempt=1
backoff=$UPLOAD_BACKOFF_START_SECONDS
while true; do
  log "put attempt $attempt"
  rc=0
  curl -fsS --retry 0 -X PUT -T "$FILE" \
    -H "Content-Type: video/$EXT_LC" \
    "$UPLOAD_URL" >>"$LOGFILE" 2>&1 || rc=$?
  if (( rc == 0 )); then
    log "put attempt $attempt: ok"
    break
  fi
  log "put attempt $attempt failed (curl $rc)"
  if (( attempt >= UPLOAD_MAX_ATTEMPTS )); then
    log "put exhausted $UPLOAD_MAX_ATTEMPTS attempts"
    echo "${FILE}.failed: PUT exhausted retries at $(date -u +%Y-%m-%dT%H:%M:%SZ) video_id=$VIDEO_ID object_key=$OBJECT_KEY" > "${FILE}.failed"
    exit 3
  fi
  log "backing off ${backoff}s"
  sleep "$backoff"
  backoff=$((backoff * 2))
  attempt=$((attempt + 1))
done

# --- 3. confirm -------------------------------------------------------------
log "confirm: POST $FAC_API_URL"

CONFIRM_BODY=$(jq -nc --arg id "$VIDEO_ID" --arg k "$OBJECT_KEY" '{
  source: "mutation WatchIngestConfirm($id:ID!,$k:String!){ watch_ingest_confirm(video_id:$id,object_key:$k) }",
  variableValues: { id: $id, k: $k }
}')

CONFIRM=$(curl -fsS -X POST "$FAC_API_URL" \
  -H "Authorization: Bearer $FAC_API_KEY" \
  -H "Content-Type: application/json" \
  --data "$CONFIRM_BODY" 2>>"$LOGFILE") || {
    log "confirm: curl failed — leaving $FILE in place for manual retry"
    echo "${FILE}.failed: confirm failed at $(date -u +%Y-%m-%dT%H:%M:%SZ) video_id=$VIDEO_ID object_key=$OBJECT_KEY" > "${FILE}.failed"
    exit 5
  }

log "confirm: ok ($CONFIRM) — deleting local $FILE"
rm -f "$FILE"
log "done"
