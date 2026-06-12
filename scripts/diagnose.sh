#!/usr/bin/env bash
# diagnose.sh — walk the record/upload chain and report PASS/FAIL per link.
#
#   ./scripts/diagnose.sh           # all checks
#   ./scripts/diagnose.sh --no-net  # skip the upload/network checks
#
# Tests run inside-out: Pi health → disk → recorder state → USB → video →
# audio → full capture → upload tooling → API. An early FAIL usually explains
# every FAIL after it, so fix top-down.
#
# Read-only except for: a 3s throwaway capture in /tmp and a 2s mic capture in
# /tmp. The API probe sends a register with an invalid extension on purpose —
# it proves reachability + auth without creating a draft video row.

set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

if [[ ! -f .env ]]; then
  echo "diagnose.sh: .env not found — copy .env.example first" >&2
  exit 2
fi
set -a; source .env; set +a

PID_FILE="${PID_FILE:-/tmp/fac-recorder.pid}"
RECORDINGS_DIR="${RECORDINGS_DIR:-./recordings}"
LOG_DIR="${LOG_DIR:-./logs}"
DISK_SPACE_MIN_MB="${DISK_SPACE_MIN_MB:-500}"
SKIP_NET=0
[[ "${1:-}" == "--no-net" ]] && SKIP_NET=1

PASS=0; FAIL=0; WARN=0
pass() { echo "  PASS  $*"; PASS=$((PASS+1)); }
fail() { echo "  FAIL  $*"; FAIL=$((FAIL+1)); }
warn() { echo "  warn  $*"; WARN=$((WARN+1)); }
section() { echo; echo "── $* ──────────────────────────────"; }

# ---------------------------------------------------------------------------
section "1. Pi health (power / thermal)"
# get_throttled is a bitmask. Bit 0 = undervoltage NOW, bit 16 = undervoltage
# happened since boot. 0x0 means the Pi has had clean power all night.
if command -v vcgencmd >/dev/null; then
  throttled=$(vcgencmd get_throttled | cut -d= -f2)
  if [[ "$throttled" == "0x0" ]]; then
    pass "no undervoltage or throttling since boot ($throttled)"
  else
    fail "power/thermal event recorded: $throttled — check PSU and camera power draw (bit 0=undervolt now, 16=undervolt since boot, 1/17=freq capped, 2/18=throttled)"
  fi
  temp=$(vcgencmd measure_temp | tr -d "temp='C")
  echo "        current temp: ${temp}°C"
else
  warn "vcgencmd not found (not a Pi?) — skipping"
fi

# ---------------------------------------------------------------------------
section "2. Disk space"
avail_mb=$(df -m "$RECORDINGS_DIR" 2>/dev/null | awk 'NR==2 {print $4}')
if [[ -z "${avail_mb:-}" ]]; then
  fail "could not stat $RECORDINGS_DIR"
elif (( avail_mb < DISK_SPACE_MIN_MB )); then
  fail "${avail_mb}MB free — below the ${DISK_SPACE_MIN_MB}MB minimum, record.sh will refuse to start"
  echo "        recordings on disk:"
  du -sh "$RECORDINGS_DIR" 2>/dev/null | sed 's/^/        /'
  echo "        free space by uploading + deleting, or: rm recordings/<uploaded>.mp4"
else
  pass "${avail_mb}MB free (minimum ${DISK_SPACE_MIN_MB}MB)"
fi

# ---------------------------------------------------------------------------
section "3. Recorder state (stale PID / zombie ffmpeg)"
if [[ -f "$PID_FILE" ]]; then
  pid=$(cat "$PID_FILE")
  if kill -0 "$pid" 2>/dev/null; then
    fail "ffmpeg from a previous session is STILL RUNNING (pid $pid) — it holds the camera and blocks 'start'. Finalise it with: ./scripts/record.sh stop"
  else
    warn "stale PID file ($PID_FILE, pid $pid is dead) — record.sh cleans this up itself, but it means the last run crashed rather than stopped"
  fi
else
  pass "no PID file — recorder idle"
fi
# Any ffmpeg we don't know about (started outside record.sh) also holds devices.
others=$(pgrep -a ffmpeg 2>/dev/null || true)
if [[ -n "$others" ]]; then
  warn "ffmpeg process(es) running:"
  echo "$others" | sed 's/^/        /'
fi

# ---------------------------------------------------------------------------
section "4. What killed the last session (ffmpeg stderr)"
last_log=$(ls -t "$LOG_DIR"/session_*.log 2>/dev/null | head -1)
if [[ -n "$last_log" ]]; then
  echo "        $last_log (last 10 lines):"
  tail -10 "$last_log" | sed 's/^/        | /'
else
  warn "no session logs in $LOG_DIR"
fi

# ---------------------------------------------------------------------------
section "5. USB link (camera on the bus, overnight disconnects)"
if command -v lsusb >/dev/null; then
  if lsusb | grep -qi insta; then
    pass "Insta360 present on USB: $(lsusb | grep -i insta)"
  else
    fail "Insta360 NOT on the USB bus — reseat the lead, try the other blue port, then check dmesg below"
  fi
else
  warn "lsusb not found — skipping"
fi
# dmesg keeps a timestamped log of every USB disconnect/reset since boot.
usb_events=$( (dmesg 2>/dev/null || sudo -n dmesg 2>/dev/null) | grep -iE "usb.*(disconnect|reset|enumerat|over-current)" | tail -8 || true)
if [[ -n "$usb_events" ]]; then
  warn "recent USB events (a disconnect overnight = lead/port/power suspect):"
  echo "$usb_events" | sed 's/^/        | /'
else
  echo "        no USB disconnect/reset events readable (or dmesg needs sudo)"
fi

# ---------------------------------------------------------------------------
section "6. Video device (/dev/video0 exists, correct, not busy)"
if [[ -e /dev/video0 ]]; then
  pass "/dev/video0 exists"
  # If the camera re-enumerated it may now be a different node — list them all.
  echo "        all video nodes: $(ls /dev/video* 2>/dev/null | tr '\n' ' ')"
  holder=$(fuser /dev/video0 2>/dev/null || true)
  if [[ -n "$holder" ]]; then
    fail "/dev/video0 is held by pid(s):$holder — ffmpeg can't open a busy device. (Leftover timg preview? Zombie ffmpeg from test 3?)"
  else
    pass "/dev/video0 not in use by another process"
  fi
else
  fail "/dev/video0 missing — camera re-enumerated or off the bus (see tests 4/5)"
fi

# ---------------------------------------------------------------------------
section "7. Video capture (can ffmpeg actually pull frames?)"
# 'Camera works' in a preview doesn't prove ffmpeg can open it with our flags.
if [[ -e /dev/video0 ]]; then
  if ffmpeg -hide_banner -nostdin -f v4l2 -i /dev/video0 -frames:v 30 -f null - </dev/null >/dev/null 2>/tmp/diag_video.log; then
    pass "pulled 30 frames from /dev/video0"
  else
    fail "ffmpeg could not capture video — /tmp/diag_video.log says:"
    tail -5 /tmp/diag_video.log | sed 's/^/        | /'
  fi
else
  warn "skipped (no /dev/video0)"
fi

# ---------------------------------------------------------------------------
section "8. Mic / ALSA (card number drift)"
# .env hard-codes a card number (plughw:N,0). If boot-time enumeration order
# changed, the Insta360 mic may have moved and ffmpeg dies on the audio input
# even though video is fine.
alsa_dev=$(grep -oE '(plug)?hw:[0-9]+,[0-9]+' <<<"${FFMPEG_INPUT_ARGS:-}" | head -1)
if [[ -z "$alsa_dev" ]]; then
  warn "no ALSA device in FFMPEG_INPUT_ARGS — skipping mic checks"
else
  echo "        .env expects mic at: $alsa_dev"
  actual_card=$(arecord -l 2>/dev/null | grep -i insta | grep -oE 'card [0-9]+' | grep -oE '[0-9]+' | head -1)
  expected_card=$(grep -oE '[0-9]+' <<<"$alsa_dev" | head -1)
  if [[ -z "$actual_card" ]]; then
    fail "no Insta360 mic in 'arecord -l' — if test 5 passed, the audio half of the camera didn't enumerate; replug the camera"
  elif [[ "$actual_card" != "$expected_card" ]]; then
    fail "card drift: .env says card $expected_card but the Insta360 mic is card $actual_card — update FFMPEG_INPUT_ARGS to plughw:${actual_card},0"
  else
    pass "Insta360 mic is card $actual_card, matching .env"
    if arecord -D "$alsa_dev" -d 2 -f S16_LE /tmp/diag_mic.wav >/dev/null 2>&1 && [[ $(stat -c%s /tmp/diag_mic.wav 2>/dev/null || echo 0) -gt 1000 ]]; then
      pass "captured 2s of audio from $alsa_dev"
    else
      fail "could not capture audio from $alsa_dev (device busy, or use plughw: not hw: — hw: rejects the mono mic)"
    fi
  fi
fi

# ---------------------------------------------------------------------------
section "9. Full capture chain (exact record.sh input flags, 3s)"
# Same input args record.sh uses, but to /tmp — proves recording works without
# touching PID/session state. If 1–8 pass and this fails, the problem is in
# the flags themselves.
# shellcheck disable=SC2086
if ffmpeg -hide_banner -nostdin -y $FFMPEG_INPUT_ARGS -t 3 \
     -c:v libx264 -preset veryfast -crf 23 -c:a aac \
     /tmp/diag_full.mp4 </dev/null >/dev/null 2>/tmp/diag_full.log; then
  size=$(stat -c%s /tmp/diag_full.mp4 2>/dev/null || stat -f%z /tmp/diag_full.mp4)
  pass "3s test recording OK (/tmp/diag_full.mp4, ${size} bytes) — recording chain is healthy"
else
  fail "full capture failed — /tmp/diag_full.log says:"
  tail -5 /tmp/diag_full.log | sed 's/^/        | /'
fi

# ---------------------------------------------------------------------------
if (( SKIP_NET )); then
  section "10–13. upload/API checks skipped (--no-net)"
else
  section "10. Upload tooling + config (upload-cdn.sh prerequisites)"
  # upload-cdn.sh hard-requires curl + jq, and FAC_API_URL/FAC_API_KEY in .env.
  tools_ok=1
  for bin in curl jq; do
    if command -v "$bin" >/dev/null; then
      pass "$bin installed"
    else
      fail "$bin not installed — upload-cdn.sh needs it (sudo apt install -y $bin)"
      tools_ok=0
    fi
  done
  cfg_ok=1
  if [[ -z "${FAC_API_URL:-}" ]]; then
    fail "FAC_API_URL not set in .env"
    cfg_ok=0
  else
    pass "FAC_API_URL set: $FAC_API_URL"
  fi
  if [[ -z "${FAC_API_KEY:-}" || "${FAC_API_KEY:-}" == "replace-me" ]]; then
    fail "FAC_API_KEY missing or still the .env.example placeholder"
    cfg_ok=0
  else
    pass "FAC_API_KEY set (${#FAC_API_KEY} chars)"
  fi

  section "11. API reachable (DNS / route / TLS)"
  # Unauthenticated poke — ANY http status proves the network path to the API
  # is fine; 000 means DNS failure, no route, or TLS handshake failure.
  if (( tools_ok && cfg_ok )); then
    api_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 8 "$FAC_API_URL" || true)
    if [[ "$api_code" != "000" && -n "$api_code" ]]; then
      pass "API endpoint answered (HTTP $api_code) — wifi/DNS/TLS all fine"
    else
      fail "no HTTP response from $FAC_API_URL — Pi has no route to the API (wifi down? DNS? captive portal?)"
      echo "        isolate with: ping -c2 8.8.8.8 (route) then curl -sI https://google.com (DNS+TLS)"
    fi
  else
    warn "skipped (test 10 failed)"
    api_code="000"
  fi

  section "12. API auth — register probe (no draft row created)"
  # Real register mutation with a deliberately invalid extension. A 401/403
  # means the key is rejected; a validation error means auth WORKS and the
  # whole register path is healthy — without registering a junk draft video.
  if [[ "$api_code" != "000" ]] && (( tools_ok && cfg_ok )); then
    probe_body=$(jq -nc '{
      source: "mutation WatchIngestRegister($ext:String!){ watch_ingest_register(ext:$ext) }",
      variableValues: { ext: "diagprobe" }
    }')
    probe_resp=$(curl -s -w '\n%{http_code}' --connect-timeout 8 -X POST "$FAC_API_URL" \
      -H "Authorization: Bearer $FAC_API_KEY" \
      -H "Content-Type: application/json" \
      --data "$probe_body" || true)
    probe_code=$(tail -1 <<<"$probe_resp")
    probe_json=$(sed '$d' <<<"$probe_resp")
    if [[ "$probe_code" == "401" || "$probe_code" == "403" ]]; then
      fail "API rejected the key (HTTP $probe_code) — check FAC_API_KEY in .env against core.api_keys"
    elif jq -e '.upload_url' <<<"$probe_json" >/dev/null 2>&1; then
      # Server accepted ext=diagprobe — unexpected, and it DID create a draft.
      warn "register unexpectedly succeeded for ext=diagprobe — a draft row was created: $(jq -c '{video_id}' <<<"$probe_json")"
      pass "auth + register path working (HTTP $probe_code)"
    else
      pass "key accepted; register answered HTTP $probe_code (invalid-ext probe rejected as expected)"
      echo "        response: $(head -c 200 <<<"$probe_json")"
    fi
  else
    warn "skipped (API unreachable or missing prerequisites)"
  fi

  section "13. Stranded uploads (.failed markers)"
  markers=$(ls "$RECORDINGS_DIR"/*.failed 2>/dev/null || true)
  if [[ -n "$markers" ]]; then
    warn "failed-upload markers found — retry with ./scripts/upload-cdn.sh <file> once 10–12 pass:"
    for m in $markers; do echo "        | $(cat "$m")"; done
  else
    pass "no .failed markers in $RECORDINGS_DIR"
  fi
fi

# ---------------------------------------------------------------------------
echo
echo "══════════════════════════════════════════"
echo " $PASS passed, $FAIL failed, $WARN warnings"
echo " Fix the FIRST failure above — later failures are usually symptoms of it."
exit $(( FAIL > 0 ? 1 : 0 ))
