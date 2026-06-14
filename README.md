# Othello Arena — Distributed Match Framework

Arena is a Go-based match system for Othello engines. It runs matches across
contributor machines, tracks Elo ratings, and provides a web dashboard.

## Components

| Component | Description |
|-----------|-------------|
| `cmd/server` | REST API + SQLite + web dashboard |
| `cmd/coach` | Distributed play agent (runs on contributor machines) |
| `cmd/match_runner` | Local engine-vs-engine match runner |
| `internal/matchmaker` | Priority-based pair scheduling across coaches |

## Engine Protocol: GTP

All engines speak the [Go Text Protocol](https://www.gnu.org/software/gnugo/gnugo_19.html)
over stdin/stdout with arena extensions: `game_time`, `stats`, `final_score`.

Any engine can participate as long as it speaks GTP. Non-GTP engines
(like Darwersi) use a thin adapter. Adapters stay with their engines.

## Coach — Distributed Play

The coach runs on contributor machines. It manages engine lifecycle:
registers available players with the arena, polls for match assignments,
launches engines as subprocesses, and bridges stdin/stdout to a WebSocket
GTP relay.

### Engine Build System

The coach doesn't know how to build engines. Each engine source repo provides
a standardized entry point. The coach just calls it, hashes the result, and
registers the engine.

**Engine source convention** — one of:
- `make coach-build` (Makefile target)
- `./coach-build.sh` (shell script)

This command builds the optimized binary and copies it plus all companion
data (brain files, pattern tables, weights) into a `coach-engine/` directory:

```
coach-engine/
  darwersi-gtp          # the binary
  default.brn           # companion data
  *.raw                 # pattern files
  players.d/            # player declarations
    dw-amoeba.yaml
    dw-rodent.yaml
```

The coach hashes the entire directory to produce a content-addressed
`engine_id` — a SHA-256 fingerprint of the exact build. Same source +
same data = same ID.

### Player Declarations

Players live in `players.d/*.yaml` inside the engine source tree. They
declare engine + runtime arguments:

```yaml
name: "dw-rodent"
version: "d8-es44"
binary: "darwersi-gtp"
args: "--name dw-rodent --max-depth 8 --end-search 44"
cores: 1
memory_mb: 8
max_concurrency: 4
```

Multiple players can share the same engine binary with different parameters.

### Setup

```bash
# One-time setup
~/dev/agent/arena/coach-setup.sh

# Edit ~/coach/builds.d/*.yaml to point to your engine sources
# Edit ~/coach/coach.yaml for your machine's resources

# Systemd commands
systemctl --user daemon-reload
systemctl --user enable --now arena-coach
sudo loginctl enable-linger $USER

# Rebuild engines after changes
~/bin/coach-update

# Reload config without rebuild
systemctl --user reload arena-coach
```

### Engine Sources (`builds.d/`)

```yaml
# ~/coach/builds.d/darwersi.yaml
source: "~/dev/agent/darwersi/Arena"

# ~/coach/builds.d/neursi.yaml
source: "~/dev/agent/neursi/engine"
```

Just a `source:` path. The coach calls `make coach-build` there.

### Building Historical Versions

```bash
# Clone a specific git tag, build it, register it:
cd ~/dev/agent/darwersi/Arena
git checkout v1.0
make coach-build
# The resulting coach-engine/ is the old version — the coach picks it up on next scan
git checkout main
```

## Identity Model

| Level | Definition | Identifier |
|-------|-----------|------------|
| **Engine** | Software build (binary + companion data) | `engine_id` = SHA-256 of `coach-engine/` contents |
| **Player** | Engine + runtime arguments | `players.d/*.yaml` declaration |
| **Ranking** | Player at a time control | Version string + time suffix (e.g., `d8-es44-60s`) |

## API

See `docs/` for the full API reference. Key endpoints:

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/api/coach/register` | Coach registers players |
| GET | `/api/coach/tasks` | Poll for match assignments |
| POST | `/api/matches` | Submit match results |
| GET | `/api/elo` | Current Elo rankings |

## Web Dashboard

`/` Rankings | `/charts` Charts | `/players` Players | `/matches` Matches | `/games` Games | `/coaches` Coaches | `/health` Health | `/admin` Token management

## Deployment

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o arena-server ./cmd/server
scp arena-server root@arena.arsac.org:/opt/arena/
ssh root@arena.arsac.org systemctl restart arena
```
