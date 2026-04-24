#!/usr/bin/env bash
# studio.sh — launch the facpi TUI with a live camera preview in a tmux split.
#
#   TUI on the left (~60% of the width, full interactive dashboard).
#   timg on the right (~40%, live camera preview via /dev/video0).
#
# Quitting either pane tears down the whole session, so a single `q` exits
# everything cleanly.
#
# Preconditions: tmux + timg installed, ./bin/facpi built, `.env` configured.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

# Dependency checks — bail with clear install instructions rather than
# letting tmux or timg complain mid-attach.
for bin in tmux timg; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "studio.sh: $bin not installed — sudo apt install -y $bin" >&2
    exit 1
  fi
done

if [[ ! -x "$REPO_ROOT/bin/facpi" ]]; then
  echo "studio.sh: $REPO_ROOT/bin/facpi not found. Run ./deploy/install.sh first." >&2
  exit 1
fi

# Reject nested tmux — running this from inside an existing tmux session
# confuses the layout. Operator should detach (Ctrl-B d) and re-run.
if [[ -n "${TMUX:-}" ]]; then
  echo "studio.sh: already inside tmux — detach first (Ctrl-B d) and re-run" >&2
  exit 1
fi

SESSION="facpi-$$"

# Left pane: the TUI. When it exits (q / Ctrl-C / crash), kill the whole
# session so the preview pane doesn't linger as an orphan.
tmux new-session -d -s "$SESSION" -x 200 -y 50 \
  "'$REPO_ROOT/bin/facpi' tui; tmux kill-session -t '$SESSION' 2>/dev/null || true"

# Right pane: timg's live preview. If timg exits (user pressed q, or camera
# isn't available), also kill the session. `-l 40%` sets the new pane's
# width; tmux 3.x syntax — present on Pi OS Bookworm.
tmux split-window -t "$SESSION" -h -l 40% \
  "timg -V /dev/video0; tmux kill-session -t '$SESSION' 2>/dev/null || true"

# Focus the TUI pane so the first keypress goes there.
tmux select-pane -t "${SESSION}:0.0"

# Hand control to the operator. Blocks until the session ends.
tmux attach -t "$SESSION"
