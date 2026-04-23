#!/usr/bin/env bash
# install.sh — idempotent post-`git pull` installer for facpi on the Pi.
#
# Steps: vuln-scan dependencies → build → install systemd unit → restart.
# Safe to run repeatedly.
#
# Prerequisites:
#   - go (1.22+) in PATH
#   - sudo privileges for systemctl
#   - a working .env (copied from .env.example)
#
# Environment overrides:
#   FACPI_SKIP_VULNCHECK=1   skip the govulncheck step (e.g. offline Pi)

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

if [[ ! -f .env ]]; then
  echo "install.sh: .env not found. Copy .env.example → .env and edit it first." >&2
  exit 1
fi

# -----------------------------------------------------------------------------
# 1. Supply-chain vuln scan (optional but recommended).
#
#    govulncheck cross-references every module we depend on against Google's
#    Go vulnerability database (pkg.go.dev/vuln) and — critically — only
#    reports CVEs whose affected functions are actually called by our code.
#    False positives from "the module has a CVE but we don't call that
#    function" are filtered out automatically.
#
#    Blocks the build on any real hit. To override (e.g. on an offline Pi),
#    set FACPI_SKIP_VULNCHECK=1.
# -----------------------------------------------------------------------------
if [[ "${FACPI_SKIP_VULNCHECK:-}" == "1" ]]; then
  echo "install.sh: skipping govulncheck (FACPI_SKIP_VULNCHECK=1)"
elif command -v govulncheck >/dev/null 2>&1; then
  echo "install.sh: running govulncheck"
  govulncheck ./...
else
  cat >&2 <<EOF
install.sh: govulncheck not installed — skipping supply-chain scan.
           To enable (recommended once, then it'll be available):
               go install golang.org/x/vuln/cmd/govulncheck@latest
           Or skip this warning permanently with FACPI_SKIP_VULNCHECK=1.
EOF
fi

# -----------------------------------------------------------------------------
# 2. Build
# -----------------------------------------------------------------------------
echo "install.sh: building facpi"
mkdir -p bin logs recordings
go build -o bin/facpi ./cmd/facpi

# -----------------------------------------------------------------------------
# 3. Seed the DB (runs migrations) without starting the daemon, so a first-run
#    operator can poke around with `facpi list` immediately.
# -----------------------------------------------------------------------------
./bin/facpi list >/dev/null 2>&1 || true

# -----------------------------------------------------------------------------
# 4. systemd unit
# -----------------------------------------------------------------------------
UNIT_SRC="$REPO_ROOT/deploy/facpi.service"
UNIT_DST="/etc/systemd/system/facpi.service"

echo "install.sh: installing systemd unit at $UNIT_DST"
sudo cp "$UNIT_SRC" "$UNIT_DST"
sudo systemctl daemon-reload

if systemctl is-enabled --quiet facpi 2>/dev/null; then
  echo "install.sh: facpi already enabled; restarting"
  sudo systemctl restart facpi
else
  echo "install.sh: enabling + starting facpi"
  sudo systemctl enable --now facpi
fi

echo
echo "install.sh: done. Follow the daemon with:"
echo "    journalctl -u facpi -f"
echo
echo "Open the TUI with:"
echo "    ./bin/facpi tui"
