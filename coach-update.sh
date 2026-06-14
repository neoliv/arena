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

# Log everything to arena/coach-update.log
exec > >(tee -a "$SCRIPT_DIR/coach-update.log") 2>&1
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

# 1. Coach binary
echo "1. Coach binary..."
cd "$SCRIPT_DIR"
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$COACH_DIR/bin/coach.new" ./cmd/coach
$DRY_RUN && rm "$COACH_DIR/bin/coach.new" || mv "$COACH_DIR/bin/coach.new" "$COACH_DIR/bin/coach"
echo "   done"

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

# Sync: pull in any new entries from coach-adapters
ADAPTERS_DIR="$HOME/dev/agent/othello-refs/coach-adapters/builds.d"
if [ -d "$ADAPTERS_DIR" ]; then
    for f in "$ADAPTERS_DIR"/*.yaml; do
        [ -f "$f" ] || continue
        dst="$BUILDS_DIR/$(basename "$f")"
        [ -f "$dst" ] || { cp "$f" "$dst"; echo "   Added new entry: $(basename "$f")"; }
    done
fi

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
    make coach-build 2>/dev/null || true
    if [ ! -d coach-engine ] && [ -f coach-build.sh ]; then
        bash coach-build.sh 2>&1 | tail -1 || true
    fi
    if [ ! -d coach-engine ]; then
        echo "   ERROR: no coach-build target found (tried make and coach-build.sh)"
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
            echo "$HASH" > "$ENGINE_DIR/engine_id"
            echo "$name $(git rev-parse --short HEAD 2>/dev/null || echo '?')" > "$ENGINE_DIR/manifest.txt"
            echo "Built: $(date -Iseconds)" >> "$ENGINE_DIR/manifest.txt"
            find coach-engine -type f | while read ff; do
                echo "$(sha256sum "$ff" | cut -c1-16)  $(stat -c%s "$ff" 2>/dev/null || stat -f%z "$ff")  ${ff#coach-engine/}" >> "$ENGINE_DIR/manifest.txt"
            done
            echo "   -> $ENGINE_DIR/"
        fi
        rm -rf coach-engine  # cleanup ephemeral dir
    else
        echo "   ERROR: coach-engine/ not created by build"
    fi
done

echo ""; echo "─── Reload ───"; reload
echo ""; echo "=== Done ==="
echo "Log saved to: $SCRIPT_DIR/coach-update.log"
$DRY_RUN && echo "(dry run — no changes made)"
