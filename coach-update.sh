#!/bin/bash
# Arena Coach — generic engine build system.
#   coach-update.sh              rebuild all engines from builds.d/
#   coach-update.sh --dry-run    show what would happen
#   coach-update.sh --reload     only reload config (SIGHUP)
#   coach-update.sh -h           show help
set -e

# Detect stale symlink (old neursi/arena path)
SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0" 2>/dev/null || echo "$0")")" && pwd)"
if echo "$SCRIPT_DIR" | grep -q "neursi/arena"; then
    echo "ERROR: coach-update.sh is in the old neursi/arena location."
    echo "The arena is now at agent/arena. Fix with:"
    echo "  ln -sf ~/dev/agent/arena/coach-update.sh ~/bin/coach-update"
    exit 1
fi
COACH_DIR="${COACH_DIR:-$HOME/coach}"
BUILDS_DIR="${BUILDS_DIR:-$COACH_DIR/builds.d}"
DRY_RUN=false; RELOAD_ONLY=false
for a in "$@"; do case "$a" in --dry-run) DRY_RUN=true ;; --reload) RELOAD_ONLY=true ;; -h|--help) echo "Usage: coach-update.sh [--dry-run] [--reload] [-h]"; exit 0 ;; esac; done

reload() {
    if [ "$(uname -s)" = "Darwin" ]; then
        pid=$(pgrep -f "coach -config" 2>/dev/null | head -1)
        if [ -n "$pid" ]; then $DRY_RUN && echo "[DRY RUN] Would kill -HUP $pid" || { kill -HUP "$pid"; echo "SIGHUP sent"; }
        else echo "Coach not running."; fi
    elif systemctl --user is-active arena-coach >/dev/null 2>&1; then
        $DRY_RUN && echo "[DRY RUN] Would reload" || { systemctl --user reload arena-coach; echo "SIGHUP sent"; }
    else echo "Coach not running."; fi
}
$RELOAD_ONLY && { echo "=== Reload config ==="; reload; exit 0; }

# Log under agent/arena/log/
mkdir -p "$SCRIPT_DIR/log"
exec > >(tee -a "$SCRIPT_DIR/log/coach-update.log") 2>&1
echo "=== Arena Coach Update === ($(date))"

# 0. Ensure coach tree exists
mkdir -p "$COACH_DIR"/{bin,engines}
if [ ! -f "$COACH_DIR/coach.yaml" ]; then
    cat > "$COACH_DIR/coach.yaml" << 'YAMLEOF'
coach_id: "workstation"
label: "Workstation"
arena_url: "https://arena.arsac.org"
max_cores: 4
max_ram_mb: 4096
engines_dir: "~/coach/engines"
YAMLEOF
    echo "0. Created default $COACH_DIR/coach.yaml — edit it to set your resources and token."
fi
# Warn if no token configured
if ! grep -q "^token:" "$COACH_DIR/coach.yaml" 2>/dev/null && [ -z "$ARENA_TOKEN" ]; then
    echo "╔══════════════════════════════════════════════════════════════╗"
    echo "║  WARNING: No token configured!                              ║"
    echo "║  Add to $COACH_DIR/coach.yaml:                              ║"
    echo "║    token: \"your-token-here\"                                 ║"
    echo "║  Or set ARENA_TOKEN in the systemd unit.                    ║"
    echo "║  Without a token, the coach cannot register players.        ║"
    echo "╚══════════════════════════════════════════════════════════════╝"
fi

# 1. Coach binary
echo "1. Coach binary..."
COACH_UPDATED=false
cd "$SCRIPT_DIR"
git pull --ff-only 2>/dev/null || true
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$COACH_DIR/bin/coach.new" ./cmd/coach
if ! cmp -s "$COACH_DIR/bin/coach.new" "$COACH_DIR/bin/coach" 2>/dev/null; then
    $DRY_RUN && rm "$COACH_DIR/bin/coach.new" || { mv "$COACH_DIR/bin/coach.new" "$COACH_DIR/bin/coach"; COACH_UPDATED=true; }
    echo "   updated (binary changed)"
else
    rm "$COACH_DIR/bin/coach.new"
    echo "   unchanged"
fi

# 2. Build engines from builds.d/
# Auto-discover: arena examples + coach-adapters
if [ ! -d "$BUILDS_DIR" ]; then
    echo "2. Creating $BUILDS_DIR from examples..."
    mkdir -p "$BUILDS_DIR"
    # Copy arena's bundled examples
    if [ -d "$SCRIPT_DIR/builds.d" ]; then
        cp "$SCRIPT_DIR/builds.d"/*.yaml "$BUILDS_DIR/" 2>/dev/null || true
    fi
    # Copy coach-adapters entries (othello-refs sibling)
    ADAPTERS_DIR="$HOME/dev/agent/othello-refs/coach-adapters/builds.d"
    if [ -d "$ADAPTERS_DIR" ]; then
        cp "$ADAPTERS_DIR"/*.yaml "$BUILDS_DIR/" 2>/dev/null || true
        echo "   Included coach-adapters entries"
    fi
    if [ -n "$(ls -A "$BUILDS_DIR" 2>/dev/null)" ]; then
        echo "   Created entries. Edit these to match your machine:"
        for f in "$BUILDS_DIR"/*.yaml; do echo "     $f"; done
    else
        echo "   No examples found. Create .yaml files in $BUILDS_DIR/"
        echo "   Format: source: \"path/to/engine/source\""
    fi
fi

cd "$SCRIPT_DIR"

# Sync: pull in any new entries from arena's own builds.d + coach-adapters
for src_dir in "$SCRIPT_DIR/builds.d" "$HOME/dev/agent/othello-refs/coach-adapters/builds.d"; do
    if [ -d "$src_dir" ]; then
        for f in "$src_dir"/*.yaml; do
            [ -f "$f" ] || continue
            dst="$BUILDS_DIR/$(basename "$f")"
            [ -f "$dst" ] || { cp "$f" "$dst"; echo "   Added new entry: $(basename "$f")"; }
        done
    fi
done

BUILD_ERRORS=0; BUILD_COUNT=0
for f in "$BUILDS_DIR"/*.yaml; do
    [ -f "$f" ] || continue
    # Extract source path (simple YAML: source: "...")
    source=$(grep '^source:' "$f" | sed 's/^source: *"//;s/"$//')
    source=$(eval echo "$source")  # expand ~
    [ -z "$source" ] && { echo "   SKIP: no source in $f"; continue; }
    [ -d "$source" ] || { echo "   SKIP: $source not found"; continue; }

    name=$(basename "$f" .yaml)
    echo ""; echo "─── $name ───"
    echo "   $source"

    cd "$source"
    # Try make coach-build first, then ./coach-build.sh
    make coach-build > "$SCRIPT_DIR/log/.build-${name}.log" 2>&1 || true
    if [ ! -d coach-engine ] && [ -f coach-build.sh ]; then
        bash coach-build.sh >> "$SCRIPT_DIR/log/.build-${name}.log" 2>&1 || true
    fi
    if [ ! -d coach-engine ]; then
        echo "   ERROR: build failed for $name"
        echo "   Last 10 lines:"
        tail -10 "$SCRIPT_DIR/log/.build-${name}.log" 2>/dev/null || true
        echo "   Full log: $SCRIPT_DIR/log/.build-${name}.log"
        BUILD_ERRORS=$((BUILD_ERRORS + 1))
        continue
    fi

    if [ -d coach-engine ]; then
        HASH=$(cd coach-engine && find . -type f -exec sha256sum {} \; | sort -k2 | sha256sum | cut -c1-16)
        ENGINE_DIR="$COACH_DIR/engines/$HASH"
        if $DRY_RUN; then
            echo "   [DRY RUN] Would create $ENGINE_DIR"
        else
            rm -rf "$ENGINE_DIR" 2>/dev/null || true
            mkdir -p "$ENGINE_DIR"
            cp -a coach-engine/* "$ENGINE_DIR/"
            find "$ENGINE_DIR" -type f -exec chmod +x {} +
            echo "$HASH" > "$ENGINE_DIR/engine_id"
            echo "$name $(git rev-parse --short HEAD 2>/dev/null || echo '?')" > "$ENGINE_DIR/manifest.txt"
            echo "Built: $(date -Iseconds)" >> "$ENGINE_DIR/manifest.txt"
            find coach-engine -type f | while read ff; do
                echo "$(sha256sum "$ff" | cut -c1-16)  $(stat -c%s "$ff" 2>/dev/null || stat -f%z "$ff")  ${ff#coach-engine/}" >> "$ENGINE_DIR/manifest.txt"
            done
            echo "   -> $ENGINE_DIR/"
        fi
        BUILD_COUNT=$((BUILD_COUNT + 1))
        rm -rf coach-engine  # cleanup ephemeral dir
    else
        echo "   ERROR: coach-engine/ not created by build"
    fi
done

echo ""; echo "─── Reload ───"
if $COACH_UPDATED; then
    echo "Coach binary was updated — restarting service..."
    if systemctl --user is-active arena-coach >/dev/null 2>&1; then
        $DRY_RUN && echo "[DRY RUN] Would restart arena-coach" || { systemctl --user restart arena-coach; echo "   restarted"; }
    else
        reload  # Darwin / manual mode
    fi
else
    reload
fi
echo ""; echo "=== Done ==="
if [ $BUILD_ERRORS -gt 0 ]; then
    echo "ERROR: $BUILD_ERRORS engine build(s) failed — check the log for details"
else
    echo "All $BUILD_COUNT engines built successfully"
fi
PLAYER_COUNT=$(find "$COACH_DIR/engines" -name '*.yaml' -path '*/players.d/*' 2>/dev/null | wc -l)
echo "Deployed $PLAYER_COUNT players from $COACH_DIR/engines"
echo "Log saved to: $SCRIPT_DIR/log/coach-update.log"
$DRY_RUN && echo "(dry run — no changes made)"
