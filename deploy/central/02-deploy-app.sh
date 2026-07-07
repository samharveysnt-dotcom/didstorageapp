#!/bin/bash
# Deploy the didapi binary, run migrations, configure Kamailio + RTPengine,
# install systemd units, open firewall, and start everything.
#
# Run AFTER 01-install-base.sh has succeeded.
# Inputs (env or args): PUBLIC_IP (auto-detected if unset), DIDAPI_URL.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

PUBLIC_IP="${PUBLIC_IP:-$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -1)}"
DIDAPI_URL="${DIDAPI_URL:-http://127.0.0.1:8080}"

if [ -z "$PUBLIC_IP" ]; then
  echo "could not determine PUBLIC_IP" >&2
  exit 1
fi

log "PUBLIC_IP=$PUBLIC_IP"

# --- system user
if ! id -u didstorage >/dev/null 2>&1; then
  useradd --system --home /opt/didstorage --shell /usr/sbin/nologin didstorage
fi

# --- layout
install -d -o didstorage -g didstorage /opt/didstorage /opt/didstorage/bin /opt/didstorage/migrations
install -d /etc/didstorage
install -d /var/log/didstorage
chown didstorage:didstorage /var/log/didstorage

# --- secrets
if [ ! -s /etc/didstorage/auth_token ]; then
  tr -dc 'A-Za-z0-9' </dev/urandom | head -c 48 > /etc/didstorage/auth_token
  chmod 640 /etc/didstorage/auth_token
  chown root:didstorage /etc/didstorage/auth_token
fi
AUTH_TOKEN=$(cat /etc/didstorage/auth_token)
PG_PASS=$(cat /root/.pg_didstorage_password)

# --- env file consumed by didapi.service
cat > /etc/didstorage/didapi.env <<EOF
LISTEN_ADDR=127.0.0.1:8080
DATABASE_URL=postgres://didstorage:${PG_PASS}@127.0.0.1:5432/didstorage?sslmode=disable
REDIS_URL=redis://127.0.0.1:6379/0
KAMAILIO_AUTH_TOKEN=${AUTH_TOKEN}
PUBLIC_IP=${PUBLIC_IP}
MIN_AUTH_SECONDS=6
EOF
chmod 640 /etc/didstorage/didapi.env
chown root:didstorage /etc/didstorage/didapi.env

# --- copy binary + migrations (expected pre-staged at /tmp/didstorage-bundle/)
BUNDLE=/tmp/didstorage-bundle
test -d "$BUNDLE" || { echo "missing $BUNDLE — push your build artifact first" >&2; exit 1; }

install -o didstorage -g didstorage -m 0755 "$BUNDLE/didapi-linux-amd64" /opt/didstorage/bin/didapi
cp -r "$BUNDLE/migrations/." /opt/didstorage/migrations/
chown -R didstorage:didstorage /opt/didstorage/migrations

# --- run migrations using psql (no migrate binary needed for v1)
log "running migrations"
export PGPASSWORD="$PG_PASS"
for f in /opt/didstorage/migrations/*.up.sql; do
  log "  applying $(basename "$f")"
  psql -h 127.0.0.1 -U didstorage -d didstorage -v ON_ERROR_STOP=1 -f "$f" >/dev/null
done
unset PGPASSWORD

# --- systemd units
install -m 0644 "$BUNDLE/systemd/didapi.service" /etc/systemd/system/didapi.service
sed -e "s#@PUBLIC_IP@#${PUBLIC_IP}#g" \
    "$BUNDLE/systemd/rtpengine.service" \
    > /etc/systemd/system/rtpengine.service
chmod 0644 /etc/systemd/system/rtpengine.service

# --- kamailio config substitution
RTPENGINE_NS="udp:127.0.0.1:2223"
sed -e "s#@PUBLIC_IP@#${PUBLIC_IP}#g" \
    -e "s#@AUTH_TOKEN@#${AUTH_TOKEN}#g" \
    -e "s#@DIDAPI_URL@#${DIDAPI_URL}#g" \
    -e "s#@RTPENGINE_NS@#${RTPENGINE_NS}#g" \
    "$BUNDLE/kamailio/central.cfg" > /etc/kamailio/kamailio.cfg
chmod 0644 /etc/kamailio/kamailio.cfg

# tell the kamailio default config NOT to override ours
if [ -f /etc/default/kamailio ]; then
  sed -i 's/^#\?RUN_KAMAILIO=.*/RUN_KAMAILIO=yes/' /etc/default/kamailio
  sed -i 's|^#\?CFGFILE=.*|CFGFILE=/etc/kamailio/kamailio.cfg|' /etc/default/kamailio
fi

# --- firewall
log "configuring firewall"
firewall-cmd --permanent --add-port=80/tcp        2>/dev/null || true
firewall-cmd --permanent --add-port=443/tcp       2>/dev/null || true
firewall-cmd --permanent --add-port=5060/udp      2>/dev/null || true
firewall-cmd --permanent --add-port=5060/tcp      2>/dev/null || true
firewall-cmd --permanent --add-port=30000-40000/udp 2>/dev/null || true
firewall-cmd --reload 2>&1 | tail -3

# --- enable + start everything
systemctl daemon-reload
systemctl enable --now rtpengine.service
systemctl enable --now didapi.service
systemctl enable --now kamailio.service

sleep 2
log "=== STATUS ==="
systemctl is-active rtpengine.service && echo "rtpengine ACTIVE"  || echo "rtpengine NOT active"
systemctl is-active didapi.service    && echo "didapi    ACTIVE"  || echo "didapi    NOT active"
systemctl is-active kamailio.service  && echo "kamailio  ACTIVE"  || echo "kamailio  NOT active"

log "=== LISTENING PORTS ==="
ss -tulnp | grep -E ':(80|443|5060|8080|2223|6379|5432)\b' || echo "no listeners on expected ports"
log "=== DEPLOY COMPLETE ==="
