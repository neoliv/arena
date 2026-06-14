#!/bin/bash
set -e
COACH_DIR="$HOME/coach"
DRY_RUN=false; RELOAD_ONLY=false
for a in "$@"; do case "$a" in --dry-run) DRY_RUN=true ;; --reload) RELOAD_ONLY=true ;; -h|--help) echo "Usage: coach-update.sh [--dry-run] [--reload] [-h]"; exit 0 ;; esac; done

reload() {
    if [ "$(uname -s)" = "Darwin" ]; then
        # macOS: find coach PID and send SIGHUP
        local pid=$(pgrep -f "coach -config" 2>/dev/null | head -1)
        if [ -n "$pid" ]; then
            $DRY_RUN && echo "[DRY RUN] Would kill -HUP $pid" || { kill -HUP "$pid"; echo "SIGHUP sent to pid $pid"; }
        else
            echo "Coach not running. Start manually: $COACH_DIR/bin/coach -config $COACH_DIR/coach.yaml"
        fi
    elif systemctl --user is-active neursi-coach >/dev/null 2>&1; then
        $DRY_RUN && echo "[DRY RUN] Would reload" || { systemctl --user reload neursi-coach; echo "SIGHUP sent"; }
    else
        echo "Coach not running. Start with: systemctl --user start neursi-coach"
    fi
}
$RELOAD_ONLY && { echo "=== Reload config ==="; reload; exit 0; }

echo "=== neursi Coach Update ==="

# 1. Coach binary
echo "1. Coach binary..."
cd ~/dev/agent/neursi/arena
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$COACH_DIR/bin/coach.new" ./cmd/coach
$DRY_RUN && rm "$COACH_DIR/bin/coach.new" || mv "$COACH_DIR/bin/coach.new" "$COACH_DIR/bin/coach"
echo "   done"

# 2. Build neursi
echo "2. neursi..."
cd ~/dev/agent/neursi/engine
cargo build --release 2>&1 | tail -1
HASH=$(sha256sum target/release/neursi | cut -c1-16)
DIR="$COACH_DIR/engines/$HASH"
if $DRY_RUN; then echo "   [DRY RUN] Would create $DIR"; else
    mkdir -p "$DIR/players.d"
    cp target/release/neursi "$DIR/neursi"
    cp players.d/*.yaml "$DIR/players.d/" 2>/dev/null || true
    echo "$HASH" > "$DIR/engine_id"
    echo "neursi $(grep '^version' Cargo.toml | head -1 | sed 's/.*\"\(.*\)\"/\1/')" > "$DIR/manifest.txt"
    echo "Built: $(date -Iseconds)" >> "$DIR/manifest.txt"
    echo "   -> $DIR/"
fi

# 3. Build darwersi-gtp
echo "3. darwersi-gtp..."
cd ~/dev/agent/darwersi/Arena
make CFLAGS="-O3 -march=native -ffast-math -fomit-frame-pointer -pipe" 2>&1 | tail -1
HASH=$(sha256sum darwersi-gtp | cut -c1-16)
DIR="$COACH_DIR/engines/$HASH"
if $DRY_RUN; then echo "   [DRY RUN] Would create $DIR"; else
    mkdir -p "$DIR/players.d"
    cp darwersi-gtp "$DIR/darwersi-gtp"
    for f in ~/dev/agent/darwersi/Lib/default.brn; do [ -f "$f" ] && cp "$f" "$DIR/"; done
    for f in ~/dev/agent/darwersi/Database/*.raw; do [ -f "$f" ] && cp "$f" "$DIR/"; done
    cp players.d/*.yaml "$DIR/players.d/" 2>/dev/null || true
    echo "$HASH" > "$DIR/engine_id"
    echo "darwersi-gtp 1.0" > "$DIR/manifest.txt"
    echo "Built: $(date -Iseconds)" >> "$DIR/manifest.txt"
    echo "   -> $DIR/"
fi

# 4. Reload
echo "4. Reloading..."
reload
echo "=== Done ==="
$DRY_RUN && echo "(dry run)"
