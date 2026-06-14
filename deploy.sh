#!/bin/bash
# deploy.sh — Build the arena-server binary and push it to the VPS.
# Stops the service before copying, waits for the process to exit,
# then starts it again.
#
# Usage: ./deploy.sh [vps-host]
#   Default: arena.arsac.org

set -euo pipefail

VPS="${1:-arena.arsac.org}"
VPS_USER="${VPS_USER:-root}"
BINARY="arena-server"

echo "=== Arena Deploy ==="
echo "Target: ${VPS_USER}@${VPS}"
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
HTTP_CODE=$(ssh "${VPS_USER}@${VPS}" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8500/health" 2>/dev/null || echo "000")

if [ "$HTTP_CODE" = "200" ]; then
    echo "✓ Server is healthy (HTTP 200)"
else
    echo "✗ Health check failed (HTTP ${HTTP_CODE})"
    echo "  Logs: ssh ${VPS_USER}@${VPS} journalctl -u arena -n 20"
    exit 1
fi

echo ""
echo "=== Deploy complete ==="
echo "Dashboard: https://${VPS}"
