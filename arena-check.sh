#!/bin/bash
# arena-check.sh — quick health check for the Arena server.
# Usage: ./arena-check.sh [--watch]
set -euo pipefail

check() {
    printf "%s  " "$(date +%H:%M:%S)"
    local code
    if ! code=$(ssh -o ConnectTimeout=5 root@arena.arsac.org \
        'curl -s -m 3 -o /dev/null -w "%{http_code}" http://127.0.0.1:8500/' 2>/dev/null); then
        echo "DOWN (SSH failed)"
        return
    fi
    case "$code" in
        303) echo "UP (login redirect)" ;;
        200) echo "UP (HTTP 200)" ;;
        000) echo "DOWN (no response)" ;;
        *)   echo "UP (HTTP $code)" ;;
    esac
    ssh -o ConnectTimeout=5 root@arena.arsac.org \
        'tail -3 /var/log/arena/server.log 2>/dev/null' 2>/dev/null | \
        while read -r line; do printf "       %s\n" "$line"; done
}

check
if [ "${1:-}" = "--watch" ]; then
    while true; do sleep 5; check; done
fi
