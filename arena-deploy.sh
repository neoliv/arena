#!/bin/bash
# arena-deploy.sh — Build the arena-server binary and push it to the VPS.
# Usage: ./arena-deploy.sh [vps-host] [--clear-db]
set -euo pipefail

VPS="arena.arsac.org"
VPS_USER="${VPS_USER:-root}"
BINARY="arena-server"
CLEAR_DB=false
for arg in "$@"; do
    case "$arg" in
        --clear-db) CLEAR_DB=true ;;
        --*) ;;
        *) VPS="$arg" ;;
    esac
done

echo "=== Arena Deploy ==="
echo "Target: ${VPS_USER}@${VPS}"
$CLEAR_DB && echo "DB clear: YES" || echo "DB clear: no (use --clear-db to wipe)"
echo ""

# ── Build ────────────────────────────────────────────────────────────────

echo "--- Building ${BINARY} ---"
cd "$(dirname "$0")"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "${BINARY}" ./cmd/server

SIZE=$(du -h "${BINARY}" | cut -f1)
echo "Binary: ${BINARY} (${SIZE})"

# ── Stop service ─────────────────────────────────────────────────────────

echo "--- Stopping arena service ---"
ssh "${VPS_USER}@${VPS}" "systemctl stop arena"

# Wait for the process to actually exit (up to 10 seconds).
echo "--- Waiting for process to exit ---"
for i in $(seq 1 10); do
    if ! ssh "${VPS_USER}@${VPS}" "pgrep -f arena-server" >/dev/null 2>&1; then
        echo "  stopped after ${i}s"
        break
    fi
    sleep 1
done

# ── Clean old logs ───────────────────────────────────────────────────────

echo "--- Cleaning old server logs ---"
ssh "${VPS_USER}@${VPS}" "journalctl --vacuum-time=1h 2>/dev/null || true"
ssh "${VPS_USER}@${VPS}" "truncate -s 0 /var/log/caddy/arena.log 2>/dev/null || true"
	ssh "${VPS_USER}@${VPS}" "truncate -s 0 /var/log/caddy/access.log 2>/dev/null || true"
ssh "${VPS_USER}@${VPS}" "truncate -s 0 /var/log/arena/server.log"
echo "  logs reset"

# ── Clear DB ─────────────────────────────────────────────────────────────

if $CLEAR_DB; then
    echo "--- Clearing DB (delegating to arena-clear-db.sh --no-restart) ---"
    # Single source of truth for which tables to clear and how to preserve
    # tokens + sessions. Do NOT inline DELETE statements here.
    bash "$(dirname "$0")/arena-clear-db.sh" "$VPS" --no-restart
fi

# ── Copy binary ──────────────────────────────────────────────────────────

echo "--- Copying binary ---"
scp "${BINARY}" "${VPS_USER}@${VPS}:/opt/arena/${BINARY}"
ssh "${VPS_USER}@${VPS}" "chown arena:arena /opt/arena/${BINARY} && chmod 755 /opt/arena/${BINARY}"

# ── Start service ────────────────────────────────────────────────────────

echo "--- Starting arena service ---"
ssh "${VPS_USER}@${VPS}" "systemctl start arena"

# ── Health check ─────────────────────────────────────────────────────────

sleep 2
echo "--- Checking health ---"
HTTP_CODE=$(ssh "${VPS_USER}@${VPS}" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8500/" 2>/dev/null || echo "000")

if [ "$HTTP_CODE" != "000" ]; then
    echo "✓ Server is healthy (HTTP ${HTTP_CODE})"
else
    echo "✗ Health check failed (no response)"
    echo "  Logs: ssh ${VPS_USER}@${VPS} journalctl -u arena -n 20"
    exit 1
fi

echo ""
echo "=== Deploy complete ==="
echo "Dashboard: https://${VPS}"
