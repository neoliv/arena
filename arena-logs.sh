#!/bin/bash
# arena-logs.sh — pull Arena server logs from VPS to local log/ directory.
# Usage: ./arena-logs.sh [vps-host]
set -euo pipefail

VPS="${1:-arena.arsac.org}"
VPS_USER="${VPS_USER:-root}"
LOG_DIR="$(cd "$(dirname "$0")" && pwd)/log"
mkdir -p "$LOG_DIR"

echo "=== Arena Logs ==="
echo "Pulling from ${VPS_USER}@${VPS} to ${LOG_DIR}/"
echo ""

# Server logs
scp "${VPS_USER}@${VPS}:/var/log/arena/server.log" "$LOG_DIR/server.log" 2>/dev/null || echo "  server.log: not found"
scp "${VPS_USER}@${VPS}:/var/log/caddy/arena.log" "$LOG_DIR/caddy.log" 2>/dev/null || echo "  caddy.log: not found"

# Last 200 lines of systemd journal
ssh "${VPS_USER}@${VPS}" "journalctl -u arena --no-pager -n 200" > "$LOG_DIR/journal.log" 2>/dev/null || echo "  journal.log: failed"

echo ""
echo "=== Summary ==="
echo "Server log:   $LOG_DIR/server.log ($(wc -l < "$LOG_DIR/server.log" 2>/dev/null || echo 0) lines)"
echo "Caddy log:    $LOG_DIR/caddy.log ($(wc -l < "$LOG_DIR/caddy.log" 2>/dev/null || echo 0) lines)"
echo "Journal:      $LOG_DIR/journal.log ($(wc -l < "$LOG_DIR/journal.log" 2>/dev/null || echo 0) lines)"
echo "Coach log:    $LOG_DIR/coach.log ($(wc -l < "$LOG_DIR/coach.log" 2>/dev/null || echo 0) lines)"
echo "Coach build:  $LOG_DIR/coach-update.log ($(wc -l < "$LOG_DIR/coach-update.log" 2>/dev/null || echo 0) lines)"
engines=$(ls -1 "$LOG_DIR/engines/" 2>/dev/null | wc -l || echo 0)
echo "Engine errs:  $engines files"
echo ""
echo "All logs in: $LOG_DIR/"
