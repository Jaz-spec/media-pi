# media-pi

FAC classroom filming pipeline: Insta360 Link 2 → Raspberry Pi → Hetzner Storage Box.

Canonical brief on Notion: **SST: Insta360 Link 2 → Pi → Hetzner Pipeline**.

## Current status

**Phase 2 — `facpi`**: Go daemon + Bubble Tea TUI sitting on top of the existing bash scripts. Persistent SQLite queue, scheduled auto-recording from platform events, interlock when you manually start near a scheduled session.

Bash scripts (`record.sh`, `upload-cdn.sh`) remain the execution primitives; `facpi` owns state, orchestration, and UX. The only retired bash script is `session.sh` — its record→upload coupling is replaced by the Go orchestrator (which decouples the two so you can start a new recording while the previous one is still uploading).

See `docs/decisions.md` for the running log of choices and why.

## facpi quick start

```bash
# 1. Build (native on Pi; go 1.22+ required)
go build -o bin/facpi ./cmd/facpi

# 2. Configure
cp .env.example .env
# edit .env: set FFMPEG_INPUT_ARGS for your platform, FAC_API_URL, FAC_API_KEY, FAC_PI_ID, FAC_API_BASE_URL

# 3. Start the daemon (foreground — systemd unit is provided in deploy/ for production)
./bin/facpi daemon

# 4. In another shell (or over SSH), open the TUI
./bin/facpi tui
```

### TUI keys

| Key | Action |
|---|---|
| `r` | Start recording (prompts interlock if a scheduled event is within 30 min) |
| `s` | Stop active recording — file is enqueued for upload immediately |
| `R` | Retry the selected **failed** upload |
| `f` | Suspend the TUI and show a live camera preview as terminal block characters (requires `timg`; press `q` inside timg to return) |
| `↑` / `↓` | Select an upload in the queue |
| `enter` | Refresh the log pane for the selected upload |
| `y` / `n` | Answer an interlock prompt |
| `q` / `Ctrl-C` | Quit the TUI (daemon keeps running) |

**Optional dependency for `f`:** `sudo apt install timg` on the Pi. The install script warns if it's missing.

### CLI equivalents

```bash
./bin/facpi record start                # same as pressing `r`
./bin/facpi record stop                 # same as pressing `s`
./bin/facpi enqueue <file>              # add an arbitrary file to the queue
./bin/facpi list                        # show the queue as a table
./bin/facpi retry <id>                  # same as `R`
```

### Deployment on the Pi

```bash
ssh pi@<host>
cd media-pi
git pull
./deploy/install.sh        # idempotent: builds, installs systemd unit, restarts service
journalctl -u facpi -f     # follow daemon logs
```

## Phase 1 quick start

```bash
# 0. One-time setup
cp .env.example .env
ssh-keygen -t ed25519 -f ~/.ssh/fac_mock_hetzner_ed25519 -C "fac-mock-hetzner"
#   → then add a 'Host mock-hetzner' block to ~/.ssh/config (see setup notes below)
docker compose up -d

# 1. Confirm the mock is reachable
ssh mock-hetzner 'echo ok'

# 2. Record a session — Ctrl-C stops → auto-uploads → local cleaned up
./scripts/session.sh
```

## `~/.ssh/config` entry

```
Host mock-hetzner
  HostName localhost
  Port 2222
  User fac
  IdentityFile ~/.ssh/fac_mock_hetzner_ed25519
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
```

Host-key verification is off for this alias only — the container regenerates its host key on every `docker compose up`, so pinning it would force re-acceptance constantly. Safe because the "network" is localhost and the container is local. Real Hetzner will use `StrictHostKeyChecking accept-new` and a normal known_hosts entry.
