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
./arena-check.sh [--watch] # quick server health check
./arena-logs.sh            # pull server/caddy/journal logs to local log/
~/bin/coach-update         # rebuild all engines + coach on host (run on host)
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

## Documentation

- `README.md` — Arena overview, coach setup, identity model
- `../neursi/docs/gtp-protocol.md` — GTP spec with arena extensions
- `../neursi/docs/arena-design.md` — Architecture, API table, DB schema
