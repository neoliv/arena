#!/bin/bash
# arena-clear-db.sh — Clear all game/match/engine data from the arena DB.
# Keeps api_tokens and web_sessions intact.
# Usage: ./arena-clear-db.sh [vps-host] [--no-restart]
set -euo pipefail

VPS="${1:-arena.arsac.org}"
VPS_USER="${VPS_USER:-root}"
DB="/opt/arena/arena.db"
NO_RESTART=false
[[ "${2:-}" == "--no-restart" ]] && NO_RESTART=true

echo "=== Arena Clear DB ==="
echo "Target: ${VPS_USER}@${VPS}"
echo ""

# Stop the server so no writes happen during cleanup
if ! $NO_RESTART; then
    echo "--- Stopping arena ---"
    ssh "${VPS_USER}@${VPS}" "systemctl stop arena"
fi

# Backup
echo "--- Backing up DB ---"
BACKUP_NAME="${DB}.bak-$(date +%Y%m%d-%H%M%S)"
ssh "${VPS_USER}@${VPS}" "cp '$DB' '$BACKUP_NAME'"
echo "  backup created: $BACKUP_NAME"

# Clear all data tables, keep tokens and sessions
echo "--- Clearing tables ---"
TABLES=(
    bisect_steps bisections coach_ais coaches elo_history engines
    game_moves games match_assignments matches speed_stats
)
for t in "${TABLES[@]}"; do
    count=$(ssh "${VPS_USER}@${VPS}" "sqlite3 '$DB' \"SELECT COUNT(*) FROM $t\"")
    ssh "${VPS_USER}@${VPS}" "sqlite3 '$DB' \"DROP TABLE IF EXISTS $t\""
    echo "  dropped $t ($count rows)"
done

TOKENS=$(ssh "${VPS_USER}@${VPS}" "sqlite3 '$DB' \"SELECT COUNT(*) FROM api_tokens\"")
SESSIONS=$(ssh "${VPS_USER}@${VPS}" "sqlite3 '$DB' \"SELECT COUNT(*) FROM web_sessions\"")
echo "Kept: $TOKENS api_tokens, $SESSIONS web_sessions"

# Reclaim disk space
echo "--- Vacuum ---"
BEFORE=$(ssh "${VPS_USER}@${VPS}" "stat -c%s '$DB'")
ssh "${VPS_USER}@${VPS}" "sqlite3 '$DB' 'VACUUM'"
AFTER=$(ssh "${VPS_USER}@${VPS}" "stat -c%s '$DB'")
echo "  DB: $(numfmt --to=iec $BEFORE) → $(numfmt --to=iec $AFTER)"

# Start the server (skip if called from arena-deploy.sh which handles its own restart)
if $NO_RESTART; then
    echo "--- Skipping restart (called with --no-restart) ---"
else
    echo "--- Starting arena ---"
    ssh "${VPS_USER}@${VPS}" "systemctl start arena"
    sleep 2
    HTTP_CODE=$(ssh "${VPS_USER}@${VPS}" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8500/" 2>/dev/null || echo "000")
    if [ "$HTTP_CODE" != "000" ]; then
        echo "✓ Server healthy (HTTP $HTTP_CODE)"
    else
        echo "✗ Health check failed"
        exit 1
    fi
fi

echo ""
echo "=== Done ==="
