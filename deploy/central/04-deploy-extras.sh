#!/bin/bash
# Deploy fail2ban + ensure pjsip_users.conf seed file exists so Asterisk's
# #include doesn't break.
set -euo pipefail
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

BUNDLE=/tmp/didstorage-bundle

log "ensure pjsip_users.conf seed exists"
touch /etc/asterisk/pjsip_users.conf
chown asterisk:asterisk /etc/asterisk/pjsip_users.conf
chmod 0640 /etc/asterisk/pjsip_users.conf

log "install fail2ban (idempotent)"
apt-get install -y --no-install-recommends fail2ban iptables >/dev/null 2>&1

log "deploy fail2ban filter + jail"
install -m 0644 "$BUNDLE/fail2ban/filter.d/asterisk-didstorage.conf" \
                /etc/fail2ban/filter.d/asterisk-didstorage.conf
install -m 0644 "$BUNDLE/fail2ban/jail.d/asterisk-didstorage.conf" \
                /etc/fail2ban/jail.d/asterisk-didstorage.conf

systemctl enable --now fail2ban
systemctl restart fail2ban
sleep 1

log "fail2ban-client status"
fail2ban-client status 2>&1 | head -10
fail2ban-client status asterisk-didstorage 2>&1 | head -10
log "DONE"
