# media-pi

FAC classroom filming pipeline: Insta360 Link 2 → Raspberry Pi → Hetzner Storage Box.

Canonical brief on Notion: **SST: Insta360 Link 2 → Pi → Hetzner Pipeline**.

## Current status

**Phase 1** — manual record + upload CLI. Developed on a Mac against a Docker SSH container standing in for Hetzner (we don't have Pi hardware or Hetzner access yet).

See `docs/decisions.md` for the running log of choices and why.

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
