#!/bin/bash
# setup-vps.sh — Install all requirements for the neursi Arena on Ubuntu 24.04.
# The arena uses SQLite (pure Go) — no PostgreSQL needed.
#
# Usage: ssh root@arena.arsac.org 'bash -s' < setup-vps.sh

set -euo pipefail

echo "=== neursi Arena VPS Setup ==="
echo "Target: Ubuntu 24.04"

# ── Caddy ────────────────────────────────────────────────────────────────

if command -v caddy &>/dev/null; then
    echo "--- Caddy already installed: $(caddy version | head -1) ---"
else
    echo "--- Installing Caddy ---"
    apt-get update -qq
    apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https
    if [ ! -f /usr/share/keyrings/caddy-stable-archive-keyring.gpg ]; then
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | \
            gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    fi
    if [ ! -f /etc/apt/sources.list.d/caddy-stable.list ]; then
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | \
            tee /etc/apt/sources.list.d/caddy-stable.list
        apt-get update -qq
    fi
    apt-get install -y -qq caddy
fi

# ── System packages ──────────────────────────────────────────────────────

echo "--- Installing system packages ---"
apt-get install -y -qq ufw git curl rsync

# ── Firewall ─────────────────────────────────────────────────────────────

echo "--- Configuring firewall ---"
ufw allow 22/tcp 2>/dev/null || true
ufw allow 80/tcp 2>/dev/null || true
ufw allow 443/tcp 2>/dev/null || true
ufw --force enable 2>/dev/null || true

# ── Arena user and directories ──────────────────────────────────────────

echo "--- Setting up arena user and directories ---"
id arena &>/dev/null || useradd -r -s /bin/false arena
mkdir -p /opt/arena /var/log/arena
chown arena:arena /opt/arena /var/log/arena

# ── Caddy reverse proxy ─────────────────────────────────────────────────

echo "--- Configuring Caddy ---"
CADDYFILE="/etc/caddy/Caddyfile"
ARENA_BLOCK='
arena.arsac.org {
    reverse_proxy 127.0.0.1:8500
    request_body {
        max_size 10MB
    }
    log {
        output file /var/log/caddy/arena.log
    }
}'

if [ -f "$CADDYFILE" ] && grep -q 'arena\.arsac\.org' "$CADDYFILE" 2>/dev/null; then
    echo "  arena.arsac.org block already present in Caddyfile, skipping"
elif [ -f "$CADDYFILE" ]; then
    echo "  appending arena.arsac.org block to existing Caddyfile"
    printf '%s\n' "$ARENA_BLOCK" >> "$CADDYFILE"
else
    echo "  creating Caddyfile with arena.arsac.org block"
    printf '%s\n' "$ARENA_BLOCK" > "$CADDYFILE"
fi

systemctl reload caddy

# ── systemd service ──────────────────────────────────────────────────────

echo "--- Installing systemd service ---"
cat > /etc/systemd/system/arena.service <<SYSTEMD
[Unit]
Description=neursi Arena Server
After=network.target caddy.service

[Service]
Type=simple
User=arena
Environment="ARENA_DB=/opt/arena/arena.db"
Environment="LISTEN_ADDR=127.0.0.1:8500"
Environment="ARENA_TOKEN="
ExecStart=/opt/arena/arena-server
Restart=always
RestartSec=5
StandardOutput=append:/var/log/arena/server.log
StandardError=append:/var/log/arena/server.log

[Install]
WantedBy=multi-user.target
SYSTEMD

systemctl daemon-reload

# ── Log rotation ────────────────────────────────────────────────────────

cat > /etc/logrotate.d/arena <<'LOGROTATE'
/var/log/arena/*.log {
    daily; rotate 30; compress; delaycompress; missingok; notifempty; copytruncate
}
LOGROTATE

# ── Done ─────────────────────────────────────────────────────────────────

echo ""
echo "=== Setup complete ==="
echo ""
echo "Next steps:"
echo "1. Set up DNS: arena.arsac.org → $(curl -s ifconfig.me)"
echo "2. Deploy: ./deploy.sh"
echo "3. Set ARENA_TOKEN in /etc/systemd/system/arena.service"
echo "   (openssl rand -hex 32)"
echo "4. systemctl enable --now arena"
echo ""
echo "Database: /opt/arena/arena.db (SQLite, pure Go)"
echo "Backup:  scp vps:/opt/arena/arena.db ."
echo "Caddy auto-obtains TLS on first request."
