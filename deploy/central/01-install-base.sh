#!/bin/bash
# DIDStorage central node base install.
# Idempotent: safe to re-run.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

log "=== APT UPDATE ==="
apt-get update -qq

log "=== BASE TOOLS ==="
apt-get install -y --no-install-recommends \
  curl wget ca-certificates gnupg lsb-release apt-transport-https \
  build-essential git pkg-config make pandoc \
  vim nano tmux htop iotop tcpdump sngrep iproute2 \
  jq

log "=== POSTGRES 15 ==="
apt-get install -y postgresql postgresql-contrib

log "=== REDIS ==="
apt-get install -y redis-server

log "=== KAMAILIO REPO (5.8 stable) ==="
mkdir -p /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/kamailio.gpg ]; then
  wget -qO- https://deb.kamailio.org/kamailiodebkey.gpg \
    | gpg --dearmor -o /etc/apt/keyrings/kamailio.gpg
fi
echo "deb [signed-by=/etc/apt/keyrings/kamailio.gpg] http://deb.kamailio.org/kamailio58 bookworm main" \
  > /etc/apt/sources.list.d/kamailio.list
apt-get update -qq

log "=== KAMAILIO ==="
apt-get install -y \
  kamailio \
  kamailio-postgres-modules \
  kamailio-tls-modules \
  kamailio-utils-modules \
  kamailio-json-modules \
  kamailio-extra-modules \
  kamailio-redis-modules
systemctl stop kamailio || true
systemctl disable kamailio || true

log "=== RTPENGINE BUILD DEPS ==="
apt-get install -y --no-install-recommends \
  libavcodec-dev libavfilter-dev libavformat-dev libavutil-dev libswresample-dev \
  libbencode-perl libcrypt-openssl-rsa-perl libcrypt-rijndael-perl \
  libcurl4-openssl-dev libevent-dev libglib2.0-dev libhiredis-dev \
  libjson-glib-dev libmosquitto-dev libpcap-dev libpcre3-dev \
  libspandsp-dev libsystemd-dev libwebsockets-dev libxmlrpc-core-c3-dev \
  libssl-dev libssl3 libio-socket-inet6-perl libsocket6-perl \
  libio-socket-ssl-perl gperf zlib1g-dev nettle-dev \
  default-libmysqlclient-dev libiptc-dev libmnl-dev libnftnl-dev \
  libgcrypt20-dev libsodium-dev libopus-dev xmlrpc-api-utils \
  libjwt-dev libwebsockets-dev libxtables-dev \
  markdown debhelper

log "=== BUILD RTPENGINE (userspace daemon only) ==="
cd /usr/src
if [ ! -d rtpengine ]; then
  git clone --depth 1 -b master https://github.com/sipwise/rtpengine.git
fi
cd rtpengine
git fetch --tags --depth 1 origin || true
# Pin to a known-good release tag; latest 14.x stable as of 2026
git checkout mr14.1.1.8 2>/dev/null || git checkout master
make -C daemon -j"$(nproc)"
install -m 0755 daemon/rtpengine /usr/local/bin/rtpengine
log "rtpengine installed: $(/usr/local/bin/rtpengine -v 2>&1 | head -1)"

log "=== GO TOOLCHAIN ==="
GO_VER=1.23.4
if ! command -v go >/dev/null || ! /usr/local/go/bin/go version 2>/dev/null | grep -q "$GO_VER"; then
  cd /tmp
  wget -q "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "go${GO_VER}.linux-amd64.tar.gz"
  rm -f "/tmp/go${GO_VER}.linux-amd64.tar.gz"
fi
echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
log "go installed: $(go version)"

log "=== POSTGRES SETUP ==="
systemctl enable --now postgresql
# Generate a random db password if not already saved
if [ ! -s /root/.pg_didstorage_password ]; then
  tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 > /root/.pg_didstorage_password
  chmod 600 /root/.pg_didstorage_password
fi
PG_PASS=$(cat /root/.pg_didstorage_password)
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='didstorage'" | grep -q 1 \
  || sudo -u postgres psql -c "CREATE USER didstorage WITH PASSWORD '${PG_PASS}';"
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='didstorage'" | grep -q 1 \
  || sudo -u postgres psql -c "CREATE DATABASE didstorage OWNER didstorage;"
sudo -u postgres psql -c "ALTER USER didstorage WITH PASSWORD '${PG_PASS}';" >/dev/null

log "=== REDIS START ==="
systemctl enable --now redis-server

log "=== VERSIONS ==="
postgres --version 2>/dev/null || /usr/lib/postgresql/15/bin/postgres --version
redis-server --version | head -1
kamailio -V 2>&1 | head -1
/usr/local/bin/rtpengine -v 2>&1 | head -1
go version

log "=== INSTALL COMPLETE ==="
