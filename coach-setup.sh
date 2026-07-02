#!/bin/bash
# Arena Coach — one-time setup for a workstation.
# Run this ONCE from your host (not the sandbox) to install the coach.
set -e

OS=$(uname -s)
COACH_DIR="$HOME/coach"

if [ "$OS" = "Darwin" ] && [ "$1" != "--force" ]; then
    echo "╔══════════════════════════════════════════════════════════════╗"
    echo "║  macOS detected.                                            ║"
    echo "║                                                              ║"
    echo "║  This script installs a Linux systemd service. On macOS:    ║"
    echo "║                                                              ║"
    echo "║  1. Build the coach binary:                                 ║"
    echo "║       cd ~/dev/agent/othello/arena                                  ║"
    echo "║       go build -o $COACH_DIR/bin/coach ./cmd/coach          ║"
    echo "║                                                              ║"
    echo "║  2. Build engines: run ~/bin/coach-update.sh (after creating   ║"
    echo "║     the update script manually or from a Linux machine).     ║"
    echo "║                                                              ║"
    echo "║  3. Run the coach manually:                                 ║"
    echo "║       $COACH_DIR/bin/coach -config $COACH_DIR/coach.yaml    ║"
    echo "║                                                              ║"
    echo "║  4. To auto-start on login, create a launchd plist:         ║"
    echo "║       ~/Library/LaunchAgents/org.arena.coach.plist         ║"
    echo "║                                                              ║"
    echo "║  Or run with --force to proceed anyway (no systemd).        ║"
    echo "╚══════════════════════════════════════════════════════════════╝"
    exit 0
fi
echo "=== Arena Coach Setup ==="
echo "Installing to: $COACH_DIR"
echo ""

# 1. Create directories
echo "1. Creating $COACH_DIR/{bin,engines,players.d} ..."
mkdir -p "$COACH_DIR"/{bin,engines,players.d}

# 2. Build stable coach
echo "2. Building stable coach binary..."
cd ~/dev/agent/othello/arena
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$COACH_DIR/bin/coach" ./cmd/coach
echo "   -> $COACH_DIR/bin/coach"

# 3. Build engines into their own ID-based directories
echo "3. Building engines..."
bash "$COACH_DIR/bin/coach-build-engines" 2>/dev/null || {
    # Inline build if script doesn't exist yet
    echo "   Building neursi..."
    cd ~/dev/agent/othello/neursi/engine
    cargo build --release 2>&1 | tail -1
    NEURSI_HASH=$(sha256sum target/release/neursi | cut -c1-16)
    NEURSI_DIR="$COACH_DIR/engines/$NEURSI_HASH"
    mkdir -p "$NEURSI_DIR"
    cp target/release/neursi "$NEURSI_DIR/neursi"
    echo "$NEURSI_HASH" > "$NEURSI_DIR/engine_id"
    echo "neursi $(grep '^version' Cargo.toml | head -1 | sed 's/.*"\(.*\)"/\1/')" > "$NEURSI_DIR/manifest.txt"
    echo "Binary: neursi" >> "$NEURSI_DIR/manifest.txt"
    echo "Size: $(stat -c%s "$NEURSI_DIR/neursi" 2>/dev/null || stat -f%z "$NEURSI_DIR/neursi")" >> "$NEURSI_DIR/manifest.txt"
    echo "Built: $(date -Iseconds)" >> "$NEURSI_DIR/manifest.txt"
    echo "   -> $NEURSI_DIR/"

    echo "   Building darwersi-gtp..."
    cd ~/dev/agent/darwersi/Arena
    make CFLAGS="-O3 -march=native -ffast-math -fomit-frame-pointer -pipe" 2>&1 | tail -1
    DARW_HASH=$(sha256sum darwersi-gtp | cut -c1-16)
    DARW_DIR="$COACH_DIR/engines/$DARW_HASH"
    mkdir -p "$DARW_DIR"
    cp darwersi-gtp "$DARW_DIR/darwersi-gtp"
    # Copy companion data
    for f in ~/dev/agent/darwersi/Lib/default.brn; do
        [ -f "$f" ] && cp "$f" "$DARW_DIR/"
    done
    for f in ~/dev/agent/darwersi/Database/*.raw; do
        [ -f "$f" ] && cp "$f" "$DARW_DIR/"
    done
    echo "$DARW_HASH" > "$DARW_DIR/engine_id"
    echo "darwersi-gtp 1.0" > "$DARW_DIR/manifest.txt"
    echo "Binary: darwersi-gtp" >> "$DARW_DIR/manifest.txt"
    echo "Companion: default.brn" >> "$DARW_DIR/manifest.txt"
    echo "Built: $(date -Iseconds)" >> "$DARW_DIR/manifest.txt"
    echo "   -> $DARW_DIR/"
}

# 4. Install player configs (pointing to engine_id directories)
echo "4. Installing player configs..."
for f in ~/dev/agent/othello/arena/players.d/*.yaml; do
    [ -f "$f" ] || continue
    cp "$f" "$COACH_DIR/players.d/$(basename $f)"
    echo "   -> $COACH_DIR/players.d/$(basename $f)"
done
# If no players.d/ exists yet, create from coach.d/
if [ -z "$(ls -A "$COACH_DIR/players.d/" 2>/dev/null)" ]; then
    echo "   No player configs found. Create them in ~/dev/agent/othello/arena/players.d/"
    echo "   (see coach-setup.sh for examples)"
fi

# 5. Create coach.yaml
cat > "$COACH_DIR/coach.yaml" << YAMLEOF
coach_id: "workstation"
label: "Workstation (darwersi + neursi AIs)"
arena_url: "https://arena.arsac.org"
max_cores: 6
max_ram_mb: 8192
engines_dir: "$HOME/coach/engines"
YAMLEOF
echo "5. Created $COACH_DIR/coach.yaml"

# 6. Install user systemd unit
echo "6. Installing user systemd unit..."
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/arena-coach.service << UNITEOF
[Unit]
Description=Arena Coach
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$COACH_DIR
Environment="ARENA_TOKEN=CHANGE_ME"
ExecStart=$COACH_DIR/bin/coach -config $COACH_DIR/coach.yaml
Restart=always
RestartSec=10
ExecReload=/bin/kill -HUP \$MAINPID

[Install]
WantedBy=default.target
UNITEOF
echo "   -> ~/.config/systemd/user/arena-coach.service"

# 7. Create coach-update.sh (rebuild engines to new ID dirs)
cat > "$COACH_DIR/coach-update.sh" << 'UPDEOF'
#!/bin/bash
# Arena Coach — rebuild engines into new ID-based directories.
set -e
COACH_DIR="$HOME/coach"
DRY_RUN=false; RELOAD_ONLY=false
for a in "$@"; do case "$a" in --dry-run) DRY_RUN=true ;; --reload) RELOAD_ONLY=true ;; -h|--help) echo "Usage: coach-update.sh [--dry-run] [--reload] [-h]"; exit 0 ;; esac; done

reload() {
    if systemctl --user is-active arena-coach >/dev/null 2>&1; then
        $DRY_RUN && echo "[DRY RUN] Would reload" || { systemctl --user reload arena-coach; echo "SIGHUP sent"; }
    else echo "Coach not running."; fi
}
$RELOAD_ONLY && { echo "=== Reload config ==="; reload; exit 0; }

echo "=== Arena Coach Update ==="
# 1. Coach binary
echo "1. Coach binary..."
cd ~/dev/agent/othello/arena
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$COACH_DIR/bin/coach.new" ./cmd/coach
$DRY_RUN && rm "$COACH_DIR/bin/coach.new" || mv "$COACH_DIR/bin/coach.new" "$COACH_DIR/bin/coach"
echo "   done"

# 2. Build neursi to engine_id directory
echo "2. neursi engine (from ~/dev/agent/othello/neursi/engine)..."
cd ~/dev/agent/othello/neursi/engine
cargo build --release 2>&1 | tail -1
HASH=$(sha256sum target/release/neursi | cut -c1-16)
DIR="$COACH_DIR/engines/$HASH"
if $DRY_RUN; then echo "   [DRY RUN] Would create $DIR"; else
    mkdir -p "$DIR"; cp target/release/neursi "$DIR/neursi"
    echo "$HASH" > "$DIR/engine_id"
    echo "neursi $(grep '^version' Cargo.toml | head -1 | sed 's/.*\"\(.*\)\"/\1/')" > "$DIR/manifest.txt"
    echo "Built: $(date -Iseconds)" >> "$DIR/manifest.txt"
    echo "   -> $DIR/"
fi

# 3. Build darwersi-gtp to engine_id directory
echo "3. darwersi-gtp..."
cd ~/dev/agent/darwersi/Arena
make CFLAGS="-O3 -march=native -ffast-math -fomit-frame-pointer -pipe" 2>&1 | tail -1
HASH=$(sha256sum darwersi-gtp | cut -c1-16)
DIR="$COACH_DIR/engines/$HASH"
if $DRY_RUN; then echo "   [DRY RUN] Would create $DIR"; else
    mkdir -p "$DIR"; cp darwersi-gtp "$DIR/darwersi-gtp"
    for f in ~/dev/agent/darwersi/Lib/default.brn; do [ -f "$f" ] && cp "$f" "$DIR/"; done
    for f in ~/dev/agent/darwersi/Database/*.raw; do [ -f "$f" ] && cp "$f" "$DIR/"; done
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
UPDEOF
chmod +x "$COACH_DIR/coach-update.sh"
mkdir -p ~/bin
ln -sf "$COACH_DIR/coach-update.sh" ~/bin/coach-update
echo "7. Created $COACH_DIR/coach-update.sh (symlinked to ~/bin/coach-update)"

# 8. Instructions
echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  Setup complete!"
echo ""
echo "  Directory layout:"
echo "    ~/coach/bin/coach          — coach binary"
echo "    ~/coach/engines/<id>/      — one dir per engine build"
echo "    ~/coach/builds.d/          — engine build definitions"
echo "    ~/coach/coach.yaml         — coach settings"
echo ""
echo "  Manual steps:"
echo "  1. Get a token from https://arena.arsac.org/admin"
echo "  2. systemctl --user edit arena-coach  (set ARENA_TOKEN)"
echo "  3. systemctl --user daemon-reload"
echo "  4. systemctl --user enable --now arena-coach"
echo "  5. sudo loginctl enable-linger \$USER"
echo ""
echo "  Update engines:  ~/bin/coach-update.sh"
echo "  Reload players:  systemctl --user reload arena-coach"
echo "═══════════════════════════════════════════════════════════"
