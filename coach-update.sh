#!/bin/bash
# Arena Coach — generic engine build system.
#   coach-update.sh              rebuild all engines from builds.d/
#   coach-update.sh --dry-run    show what would happen
#   coach-update.sh -h           show help
set -e

SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0" 2>/dev/null || echo "$0")")" && pwd)"
# OTHELLO_HOME is the root of the othello project tree (arena/, neursi/, ref/).
# Auto-detect from script location: coach-update.sh is at othello/arena/coach-update.sh.
OTHELLO_HOME="${OTHELLO_HOME:-$(dirname "$SCRIPT_DIR")}"
export OTHELLO_HOME
COACH_DIR="${COACH_DIR:-$HOME/coach}"
BUILDS_DIR="${BUILDS_DIR:-$COACH_DIR/builds.d}"
DRY_RUN=false
for a in "$@"; do case "$a" in --dry-run) DRY_RUN=true ;; -h|--help) echo "Usage: coach-update.sh [--dry-run] [-h]"; exit 0 ;; esac; done

reload() {
    if [ "$(uname -s)" = "Darwin" ]; then
        pid=$(pgrep -f "coach -config" 2>/dev/null | head -1)
        if [ -n "$pid" ]; then $DRY_RUN && echo "[DRY RUN] Would kill -HUP $pid" || { kill -HUP "$pid"; echo "SIGHUP sent"; }
        else echo "Coach not running."; fi
    elif systemctl --user is-active arena-coach >/dev/null 2>&1; then
        $DRY_RUN && echo "[DRY RUN] Would reload" || { systemctl --user reload arena-coach; echo "SIGHUP sent"; }
    else echo "Coach not running."; fi
}

# Log under agent/arena/log/
mkdir -p "$SCRIPT_DIR/log"
exec > >(tee -a "$SCRIPT_DIR/log/coach-update.log") 2>&1
echo "=== Arena Coach Update === ($(date))"

# 0. Ensure coach tree and copy config from source tree.
mkdir -p "$COACH_DIR"/{bin,engines}
cp "$SCRIPT_DIR/coach.yaml" "$COACH_DIR/coach.yaml"
echo "0. coach.yaml copied from arena source"

# Warn if no token configured
if ! grep -q "^token:" "$COACH_DIR/coach.yaml" 2>/dev/null && [ -z "$ARENA_TOKEN" ]; then
    echo "WARNING: No token configured!"
    echo "Add to $COACH_DIR/coach.yaml: token: \"your-token\""
    echo "Or set ARENA_TOKEN in the systemd unit."
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
if [ ! -d "$BUILDS_DIR" ]; then
    echo "2. Creating $BUILDS_DIR from examples..."
    mkdir -p "$BUILDS_DIR"
    if [ -d "$SCRIPT_DIR/builds.d" ]; then
        cp "$SCRIPT_DIR/builds.d"/*.yaml "$BUILDS_DIR/" 2>/dev/null || true
    fi
    ADAPTERS_DIR="$SCRIPT_DIR/coach-adapters/builds.d"
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

for src_dir in "$SCRIPT_DIR/builds.d" "$SCRIPT_DIR/coach-adapters/builds.d"; do
    if [ -d "$src_dir" ]; then
        echo "   Scanning: $src_dir"
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
    source=$(grep '^source:' "$f" | sed 's/^source: *"//;s/"$//')
    source=$(eval echo "$source")
    [ -z "$source" ] && { echo "   SKIP: no source in $f"; continue; }
    [ -d "$source" ] || { echo "   SKIP: $source not found"; continue; }

    name=$(basename "$f" .yaml)
    echo ""; echo "─── $name ───"
    echo "   $source"

    cd "$source"
    make coach-build > "$SCRIPT_DIR/log/.build-${name}.log" 2>&1 || true
    if [ ! -d coach-engine ] && [ -f coach-build.sh ]; then
        bash coach-build.sh >> "$SCRIPT_DIR/log/.build-${name}.log" 2>&1 || true
    fi
    if [ ! -d coach-engine ]; then
        echo "   ERROR: build failed for $name"
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

            # Warm up engine and probe version via GTP (no timeout pressure).
            # This pre-builds any caches (e.g. NNUE .mmap) so the coach
            # doesn't hit a cold-start timeout on first probe.
            if ! $DRY_RUN; then
                # Read binary name from first player YAML instead of guessing.
                BIN=""
                PROBE_ARGS=""
                for y in "$ENGINE_DIR"/players.d/*.yaml; do
                    [ -f "$y" ] || continue
                    BIN_NAME=$(grep '^binary:' "$y" | sed 's/^binary: *"//;s/"$//')
                    ARGS=$(grep '^args:' "$y" | sed 's/^args: *"//;s/"$//')
                    ARGS=$(echo "$ARGS" | sed "s|%share_dir%|$COACH_DIR/share|g; s|%game_time%|60|g")
                    BIN="$ENGINE_DIR/$BIN_NAME"
                    PROBE_ARGS="$ARGS"
                    break
                done
                if [ -n "$BIN" ] && [ -x "$BIN" ]; then
                    GTP_IN="version\nquit\n"
                    VERSION_OUT=$(cd "$ENGINE_DIR" && printf "$GTP_IN" | timeout 10 "$BIN" $PROBE_ARGS 2>/dev/null || true)
                    GTP_VERSION=$(echo "$VERSION_OUT" | grep '^= ' | head -1 | sed 's/^= //')
                    if [ -n "$GTP_VERSION" ]; then
                        echo "   version: $GTP_VERSION (probed via GTP, caches warmed)"
                    else
                        echo "   version: (no GTP response — adapter/engine may not support version probe)"
                    fi
                fi
            fi
        fi
        BUILD_COUNT=$((BUILD_COUNT + 1))
        rm -rf coach-engine
    else
        echo "   ERROR: coach-engine/ not created by build"
    fi
done

echo ""; echo "─── Reload ───"

# 3. Copy neursi NN weight file to coach share dir
NEURSI_WEIGHTS="$OTHELLO_HOME/neursi/data/eval-large.bin"
SHARE_WEIGHTS="$COACH_DIR/share/eval-large.bin"
if [ -f "$NEURSI_WEIGHTS" ]; then
    mkdir -p "$(dirname "$SHARE_WEIGHTS")"
    if [ ! -f "$SHARE_WEIGHTS" ] || [ "$NEURSI_WEIGHTS" -nt "$SHARE_WEIGHTS" ]; then
        if $DRY_RUN; then
            echo "[DRY RUN] Would copy $NEURSI_WEIGHTS → $SHARE_WEIGHTS"
        else
            cp "$NEURSI_WEIGHTS" "$SHARE_WEIGHTS"
            echo "3. NN weights copied → $SHARE_WEIGHTS"
        fi
    else
        echo "3. NN weights up to date ($SHARE_WEIGHTS)"
    fi
else
    echo "3. NN weights NOT FOUND at $NEURSI_WEIGHTS — skipping"
fi
if $COACH_UPDATED; then
    echo "Coach binary was updated — restarting service..."
    if systemctl --user is-active arena-coach >/dev/null 2>&1; then
        $DRY_RUN && echo "[DRY RUN] Would restart arena-coach" || { systemctl --user restart arena-coach; echo "   restarted"; }
    else
        reload
    fi
else
    reload
fi
echo ""; echo "=== Done ==="
PLAYER_COUNT=$(find "$COACH_DIR/engines" -name '*.yaml' -path '*/players.d/*' 2>/dev/null | wc -l)
echo "$PLAYER_COUNT players deployed from $COACH_DIR/engines"
if [ $BUILD_ERRORS -gt 0 ]; then
    echo "ERROR: $BUILD_ERRORS engine build(s) failed"
else
    echo "$BUILD_COUNT engines built successfully"
fi
echo "Log saved to: $SCRIPT_DIR/log/coach-update.log"
$DRY_RUN && echo "(dry run — no changes made)"
