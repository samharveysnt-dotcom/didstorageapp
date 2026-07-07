#!/bin/bash
# Deploy DIDStorage Asterisk configs + AGI scripts. Run AFTER build-asterisk.sh
# has finished installing Asterisk to /usr/sbin/asterisk.
set -euo pipefail
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

BUNDLE=/tmp/didstorage-bundle/asterisk
PUBLIC_IP="${PUBLIC_IP:-$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -1)}"

# AGI scripts location
install -d -o asterisk -g asterisk /opt/didstorage/scripts
install -m 0755 "$BUNDLE/scripts/dids-authorize.py" /opt/didstorage/scripts/dids-authorize.py
install -m 0755 "$BUNDLE/scripts/dids-cdr.py"       /opt/didstorage/scripts/dids-cdr.py

# Configs — substitute @PUBLIC_IP@.
sed -e "s#@PUBLIC_IP@#${PUBLIC_IP}#g" "$BUNDLE/pjsip.conf" > /etc/asterisk/pjsip.conf
install -m 0644 "$BUNDLE/extensions.conf" /etc/asterisk/extensions.conf
chown asterisk:asterisk /etc/asterisk/pjsip.conf /etc/asterisk/extensions.conf

# Make sure asterisk can reach didapi token.
chgrp asterisk /etc/didstorage/auth_token
chmod 640 /etc/didstorage/auth_token

# Make /opt/didstorage/scripts readable by asterisk
chown -R asterisk:asterisk /opt/didstorage/scripts

# Reload asterisk if it's running, else start it
log "ensure firewall has 5060 (already opened earlier) + RTP range"
firewall-cmd --permanent --add-port=10000-20000/udp 2>/dev/null || true
firewall-cmd --reload 2>&1 | tail -3

systemctl daemon-reload
log "starting asterisk"
systemctl enable --now asterisk
sleep 2

log "status"
systemctl is-active asterisk
ss -tulnp | grep -E ":5060\b" | head

log "asterisk version"
asterisk -rx 'core show version' 2>/dev/null | head -3

log "pjsip endpoints"
asterisk -rx 'pjsip show endpoints' 2>/dev/null | head -20

log "DONE"
