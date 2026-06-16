# Arena — Claude Code Guidance

## Edit vs Write

**If a file needs 3+ changes, Write it entirely instead of patching with Edit.**
Consecutive Edit attempts on the same file frequently fail on whitespace mismatches
(tabs vs spaces are invisible in diffs). This produces partial corruption that
requires more fixes. The most failure-prone files:

- `cmd/coach/main.go` — deeply nested, mixed whitespace in closures
- `internal/db/db.go` — SQL strings with multi-line backtick literals
- `internal/coach/api.go` — tab-indented Go with JSON/SQL strings
- `internal/web/web.go` — raw HTML strings inside Go, mixed indent
- `coach-update.sh` — bash, some lines indented, some not

For a single targeted change, Edit is fine. For a function rewrite or multiple
insertions, use Read + Write.

## Scripts

```bash
./arena-deploy.sh          # build, deploy to VPS, clean logs, health check
./arena-clear-db.sh        # clear all game data from VPS DB (keeps tokens+sessions)
./arena-check.sh [--watch] # quick server health check
./arena-logs.sh            # pull server/caddy/journal logs to local log/
~/bin/coach-update.sh      # rebuild all engines + coach on host (run on host)
```

## Deploy

```bash
./arena-deploy.sh          # builds server, scp to VPS, clears old logs, health check
```
All logs end up in `log/` — shared between host and sandbox at the same path.

The health check hits `/health` which requires login (303 redirect = success).

## Common Pitfalls

### SQLite ALTER TABLE
New columns must be added via migration (`internal/db/db.go` migrate list), not
just in CREATE TABLE. Existing databases need ALTER TABLE to match the schema.
Use `db.Exec(stmt)` with comment "ignore errors — column may already exist".

### Go SQL parameter counting
When adding columns, count `?` placeholders against arguments. SQLite error
"missing argument with index N" means the N-th placeholder has no matching arg.
Use `Exec(... , engineID, engineManifest, now)` — trailing args are easy to miss.

### Coach scanning logic
The coach scans `engines_dir/*/players.d/*.yaml` (glob, not flat ReadDir).
Config field `engines_dir` from `coach.yaml` takes priority over `-players` flag.
Default is `~/coach/engines`. Binary paths are resolved relative to engine dir.

### Coach registration flow
- `loadAndRegister()` — scans filesystem, populates `cfg.AIs`, calls `register()`
- `register()` — POSTs cfg.AIs to `/api/coach/register`
- `heartbeatLoop` — detects server restarts via `server_gen` field, calls `loadAndRegister()` on change
- SIGHUP — also triggers `loadAndRegister()`

### Web auth model
- Web dashboard: `RequireLogin` middleware (session cookie)
- API endpoints: `requireToken` (Bearer token) or `checkAuthOrOpen` (coach endpoints, open if no token configured)
- The API `/api/engines` etc. require token — these are for match_runner, not web pages
- Web pages query the database directly via internal handlers

### systemd unit
Environment lines MUST have matching double quotes:
```
Environment="ARENA_TOKEN=the-token-value"
```
A missing closing quote swallows the next line and the variable is silently ignored.

## Player YAML — `%game_time%` substitution

The coach substitutes `%game_time%` in the player YAML `args` field with the
matchmaker's chosen time control in seconds. This lets engines that need a CLI
flag (like edax's `-t`) receive the time control at launch rather than via GTP.

```yaml
# edax — uses -t flag for time-per-game
args: "-gtp -t %game_time% -l 5"

# neursi — uses -t flag for game time
args: "-t %game_time%"

# darwersi — uses --time flag for game time
args: "--name dw-rodent --max-depth 8 --end-search 44 --time %game_time%"
```

The substitution is a simple `strings.Replace`. If the placeholder is absent,
nothing changes. The engine process is per-match, so the value is always correct
for the current time control.

### Coach-side time enforcement

The coach now tracks genmove wall-clock time via GTP-aware bridge goroutines.
If accumulated thinking time exceeds `gameTimeSec * 1.05` (5% margin) per game,
the engine is killed and the assignment is marked as failed. A watchdog also
fires if the engine doesn't respond at all within 2x the per-game budget.

This replaces the arena's `game_time` GTP command. Time enforcement is coach-side
where wall-clock measurement is reliable.

### GTP protocol — standard only

The arena matchmaker only sends standard GTP commands: `boardsize`, `clear_board`,
`play`, `genmove`, `quit`. The arena-specific extensions (`game_time`, `final_score`,
`stats`) have been removed:
- `game_time` → coach-side CLI flag substitution
- `final_score` → computed from result
- `stats` → optional, ignored if engine doesn't support it

## Key files

| File | Purpose |
|------|---------|
| `cmd/server/main.go` | Arena server entry point |
| `cmd/coach/main.go` | Coach binary (contributor machines) |
| `internal/db/db.go` | Schema + migrations |
| `internal/db/coach.go` | Coach/coach_ais/match_assignments queries |
| `internal/coach/api.go` | Coach REST API handlers |
| `internal/web/web.go` | Web dashboard handlers |
| `internal/matchmaker/mm.go` | Match scheduling |
| `deploy.sh` | Build + deploy to VPS |
| `coach-update.sh` | Build engines + coach binary on host |

## Web UI

Pages use HTMX (`unpkg.com/htmx.org@2.0.4`) for auto-refresh on dynamic content
(rankings, matches, coaches). No custom JS required — declarative attributes on
container elements (`hx-get`, `hx-trigger="every 30s"`, `hx-swap="outerHTML"`).

Game detail page has tabbed charts (Time/Nodes/NpS) with dark green background,
black bars for black moves, white bars for white moves, and proper display of
parity inversions (when a player moves twice because the opponent has no legal
moves). Visited links use a darker shade (`--link-visited`) to distinguish from
unvisited links.

## Documentation

- `README.md` — Arena overview, coach setup, identity model
- `../neursi/docs/gtp-protocol.md` — GTP spec with arena extensions
- `../neursi/docs/arena-design.md` — Architecture, API table, DB schema
