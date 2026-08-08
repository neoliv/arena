# AGENTS.md — OpenCode Guidance

## Build

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o arena-server ./cmd/server
```

CGO is disabled. Only two deps beyond stdlib: `nhooyr.io/websocket` and `modernc.org/sqlite`.

## No tests

There are zero `_test.go` files. Verify changes by building (`go build ./...`) and deploying.

## Edit vs Write threshold

If a file needs 3+ changes, read it and Write the whole file. Edit often fails on
whitespace mismatches in these files: `cmd/coach/main.go`, `internal/db/db.go`,
`internal/coach/api.go`, `internal/web/web.go`, `internal/matchmaker/mm.go`,
`internal/matchmaker/game.go`, `coach-update.sh`.

## SQLite ALTER TABLE

New columns go through migrations in `internal/db/db.go`, not via CREATE TABLE.
Use `db.Exec(stmt)` with `"ignore errors"` comment — column may already exist on
existing DBs. Count `?` placeholders against arguments carefully.

## Web pages

All pages MUST use `sharedCSS` (dark-mode CSS custom properties), `pageHead`, and
`pageFoot` from `internal/web/web.go`. Never write inline `<style>` blocks.

CDN: use `cdn.jsdelivr.net/npm/<pkg>@<version>` with pinned versions. `unpkg.com`
redirects; bare version specifiers return 400.

## Deploy

Always use scripts — never deploy manually:
- `./arena-deploy.sh [--clear-db]` — build, stop, scp, start, health check
- `./arena-clear-db.sh` — wipe game data, keeps tokens + sessions
- `./arena-check.sh [--watch]` — health check

Use `--clear-db` when changing framework code (game loop, board, scoring, matchmaker).
Not needed for web-only changes.

## Architecture

Single Go module. Four binaries:
- `cmd/server` — REST API + SQLite + web dashboard (the arena)
- `cmd/coach` — distributed play agent on contributor machines
- `cmd/match_runner` — local GTP match runner
- `cmd/sprt` — SPRT regression testing tool

Shared packages: `internal/game/` (GTP, board, book), `internal/sprt/`, `internal/db/`,
`internal/web/`, `internal/matchmaker/`, `internal/coach/`.

Coach ↔ server protocol is push-based WebSocket (`wss://arena.arsac.org/api/coach/ws`).
Key files: `cmd/coach/wsloop.go`, `internal/matchmaker/wscoach.go`, `internal/coach/wsproto.go`.

## OTHELLO_HOME

Scripts auto-detect `OTHELLO_HOME` from `$SCRIPT_DIR/..`. Env var overrides.
Coach dir defaults to `$HOME/coach`. No hardcoded paths.

## More detail

See `CLAUDE.md` for coach registration flow, `%game_time%` substitution,
coach-side time enforcement, session persistence, and coach log paths.
