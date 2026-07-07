#!/usr/bin/env bash
# scripts/bootstrap.sh — single-file Debian 12 fresh-install bootstrap.
#
# Use this ONCE per server, on a clean Debian 12 box, to bring DIDStorage up
# from nothing. It is intentionally DESTRUCTIVE — if a previous DIDStorage
# install is present it will be wiped (services stopped, DB dropped, dirs
# removed) before the new one lands. Do NOT run it against a server you want
# to preserve data on.
#
# After bootstrap, use scripts/deploy.sh for ongoing incremental deploys
# (new code, new migrations, asterisk config refresh). Bootstrap calls into
# deploy.sh internally to perform the first build-and-ship.
#
# Usage:
#   PUBLIC_IP=203.0.113.10 scripts/bootstrap.sh root@HOST [flags]
#
# Required env:
#   PUBLIC_IP            Public IPv4 the server uses for SIP. No auto-detect:
#                        a wrong IP here silently breaks NAT traversal, so we
#                        force the operator to be explicit.
#
# Optional env:
#   SSH_KEY              SSH key path                 (default: ~/.ssh/didstorage_ed25519)
#   DB_PASSWORD          Postgres password for the didstorage role
#                        (default: 32 random alnum chars)
#   SIPCTL_TOKEN         Token Asterisk sends as X-DIDS-Auth on /sipctl/*
#                        (default: 48 random alnum chars)
#   SESSION_SECRET       Reserved for future use; not consumed by didapi yet
#                        (default: 64 random hex chars)
#   ADMIN_EMAIL          Bootstrap admin email used by the seed insert
#                        (default: admin@example.com)
#   ADMIN_PASSWORD       Bootstrap admin password
#                        (default: 16 random alnum chars, printed once)
#   LISTEN_ADDR          Bind for didapi                (default: :80)
#   GO_VERSION           Local Go toolchain to require  (default: 1.23)
#   ASTERISK_VERSION     Asterisk tarball to build      (default: 20.19.0)
#                        Set to "20-current" or "22-current" for the latest
#                        in a major; pinning is recommended for repeatability.
#
# Flags (mostly forwarded to deploy.sh once bootstrap is done):
#   --skip-firewall      Don't touch ufw / iptables.
#   --skip-asterisk      Don't ship pjsip.conf / extensions.conf / AGI scripts.
#   --dry-run            Print every remote command instead of executing it.
#   --yes                Don't prompt for "this is destructive" confirmation.
#   --help               Show this help.
#
# Exit codes match deploy.sh's (0 ok, 2 bad args, 3 precondition, 4 build,
# 5 transfer, 6 remote, 7 verify). 8 is reserved for "user declined wipe".
#
# What it installs:
#   * postgresql-15, redis-server, ffmpeg, tcpdump, sngrep, ufw, jq, rsync,
#     ca-certificates, curl from Debian repos
#   * Asterisk built from source (Debian 12 dropped the asterisk package
#     entirely; building from source matches prod). Defaults to 20.19.0
#     tarball from downloads.asterisk.org. ~12 min on a 2-vCPU box.
#   * /opt/didstorage/{bin,migrations,scripts}
#   * /etc/didstorage/didapi.env  (0640 root:didstorage)
#   * /etc/systemd/system/{didapi,didbill,sip-capture}.service
#   * /etc/asterisk/pjsip.conf + extensions.conf (with PUBLIC_IP substituted)
#   * /var/lib/didstorage/{sip-traces,kyc} and /var/lib/asterisk/sounds/didstorage
#   * Postgres role `didstorage` + database `didstorage`
#   * Firewall opens: 22/tcp (kept), 80/tcp, 443/tcp, 5060/udp+tcp, 10000-20000/udp
#
# What it does NOT do:
#   * TLS certs (use `certbot` after deploy; the app supports SNI per site_domains row).
#   * DNS — point your A record at PUBLIC_IP yourself.
#   * Backups — `pg_dump` cron job is left for the operator to add.

set -euo pipefail

# ─────────────────────────────────────────────────────────────
# Args + defaults
# ─────────────────────────────────────────────────────────────

TARGET=""
SSH_KEY="${SSH_KEY:-$HOME/.ssh/didstorage_ed25519}"
LISTEN_ADDR="${LISTEN_ADDR:-:80}"

DRY_RUN=0
ASSUME_YES=0
SKIP_FIREWALL=0
SKIP_ASTERISK=0
SKIP_SSH_HARDENING=0

# Pass-through flags for deploy.sh.
DEPLOY_FORWARD=()

usage() {
  sed -n '2,55p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-firewall)      SKIP_FIREWALL=1 ;;
    --skip-asterisk)      SKIP_ASTERISK=1; DEPLOY_FORWARD+=(--skip-asterisk) ;;
    --skip-ssh-hardening) SKIP_SSH_HARDENING=1 ;;
    --dry-run)            DRY_RUN=1;       DEPLOY_FORWARD+=(--dry-run) ;;
    --yes|-y)             ASSUME_YES=1 ;;
    --help|-h)            usage ;;
    --)                   shift; break ;;
    -*)                   echo "unknown flag: $1" >&2; usage ;;
    *)                    if [[ -z "$TARGET" ]]; then TARGET="$1"; else echo "unexpected arg: $1" >&2; usage; fi ;;
  esac
  shift
done

[[ -z "$TARGET" ]] && { echo "missing TARGET (user@host)" >&2; usage; }
[[ -z "${PUBLIC_IP:-}" ]] && { echo "PUBLIC_IP env var is required (e.g. PUBLIC_IP=1.2.3.4)" >&2; exit 2; }
if ! [[ "$PUBLIC_IP" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
  echo "PUBLIC_IP must be an IPv4 address (got: $PUBLIC_IP)" >&2
  exit 2
fi

# Generate secrets where the operator didn't pin one. `head -c N` closes the
# pipe early and SIGPIPEs `tr`, which under `set -o pipefail` propagates as a
# 141 exit and kills the whole script. Run each generator in a subshell with
# pipefail disabled so the noise stays contained.
randstr() { ( set +o pipefail; LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c "$1" ); }
randhex() { ( set +o pipefail; LC_ALL=C tr -dc 'a-f0-9'   </dev/urandom | head -c "$1" ); }

DB_PASSWORD="${DB_PASSWORD:-$(randstr 32)}"
SIPCTL_TOKEN="${SIPCTL_TOKEN:-$(randstr 48)}"
SESSION_SECRET="${SESSION_SECRET:-$(randhex 64)}"
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@example.com}"
ADMIN_PASSWORD_PROVIDED=1
if [[ -z "${ADMIN_PASSWORD:-}" ]]; then
  ADMIN_PASSWORD="$(randstr 16)"
  ADMIN_PASSWORD_PROVIDED=0
fi

# Asterisk source build pin. 20.x is the LTS; override with the env var
# for a newer point release. downloads.asterisk.org keeps only the current
# tarball at the main path — older ones get rotated to old-releases/, so
# the fetch below tries both. "20-current" / "22-current" use the moving
# Asterisk-published "latest" tarball, which is fine for greenfield but
# breaks reproducibility.
ASTERISK_VERSION="${ASTERISK_VERSION:-20.20.1}"

# ─────────────────────────────────────────────────────────────
# Output + trap machinery (mirrors deploy.sh styling)
# ─────────────────────────────────────────────────────────────

if [[ -t 1 ]] && command -v tput >/dev/null 2>&1 && [[ "${TERM:-}" != "dumb" ]]; then
  BOLD=$(tput bold); DIM=$(tput dim); RESET=$(tput sgr0)
  RED=$(tput setaf 1); GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3); BLUE=$(tput setaf 4)
else
  BOLD=""; DIM=""; RESET=""; RED=""; GREEN=""; YELLOW=""; BLUE=""
fi

CURRENT_STAGE="(starting)"
STAGE_NUM=0
STARTED_AT=$(date +%s)

stage()  { STAGE_NUM=$((STAGE_NUM + 1)); CURRENT_STAGE="$*"
           printf "\n%s>>> [%02d] %s%s\n" "$BLUE$BOLD" "$STAGE_NUM" "$CURRENT_STAGE" "$RESET"; }
note()   { printf "    %s· %s%s\n" "$DIM" "$*" "$RESET"; }
ok()     { printf "    %s✓ %s%s\n" "$GREEN" "$*" "$RESET"; }
warn()   { printf "    %s! %s%s\n" "$YELLOW" "$*" "$RESET"; }
errln()  { printf "    %s✗ %s%s\n" "$RED" "$*" "$RESET" >&2; }

on_error() {
  local exit_code=$?
  local line=${BASH_LINENO[0]:-?}
  echo
  echo "${RED}${BOLD}════════════════════════════════════════════════${RESET}" >&2
  echo "${RED}${BOLD}BOOTSTRAP FAILED${RESET}" >&2
  echo "${RED}stage  : [${STAGE_NUM}] ${CURRENT_STAGE}${RESET}" >&2
  echo "${RED}line   : ${line}${RESET}" >&2
  echo "${RED}command: ${BASH_COMMAND}${RESET}" >&2
  echo "${RED}exit   : ${exit_code}${RESET}" >&2
  echo "${RED}elapsed: $(( $(date +%s) - STARTED_AT ))s${RESET}" >&2
  echo "${RED}${BOLD}════════════════════════════════════════════════${RESET}" >&2
  exit "$exit_code"
}
trap on_error ERR

# ─────────────────────────────────────────────────────────────
# Helpers
# ─────────────────────────────────────────────────────────────

SSH=(ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
SCP=(scp -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)

remote() {
  if (( DRY_RUN )); then
    printf '%s[dry-run] ssh %s "%s"%s\n' "$DIM" "$TARGET" "$*" "$RESET" >&2
    return 0
  fi
  "${SSH[@]}" "$TARGET" "$@"
}

# remote_script <<'SH'  …  SH   — pipes the heredoc to bash -s on the remote.
remote_script() {
  if (( DRY_RUN )); then
    printf '%s[dry-run] piped script to %s%s\n' "$DIM" "$TARGET" "$RESET" >&2
    cat >/dev/null
    return 0
  fi
  "${SSH[@]}" "$TARGET" 'sudo bash -s' "$@"
}

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

# ─────────────────────────────────────────────────────────────
# Stage 1 — Preconditions + destructive confirm
# ─────────────────────────────────────────────────────────────

stage "Preconditions"

for t in go ssh scp; do
  command -v "$t" >/dev/null 2>&1 || { errln "missing local tool: $t"; exit 3; }
done
ok "local tools present: go, ssh, scp"

if [[ ! -f "$SSH_KEY" ]]; then
  errln "SSH key not found at $SSH_KEY (override with SSH_KEY=...)"; exit 3
fi
ok "SSH key: $SSH_KEY"

if ! remote 'echo ok' >/dev/null 2>&1; then
  errln "cannot SSH to $TARGET with key $SSH_KEY"; exit 3
fi
ok "SSH $TARGET reachable"

remote_os=$(remote 'cat /etc/os-release 2>/dev/null | sed -n "s/^PRETTY_NAME=\"\(.*\)\"$/\1/p"' || echo unknown)
note "remote OS: ${remote_os:-unknown}"
if ! echo "$remote_os" | grep -qi debian; then
  warn "remote is not Debian ($remote_os) — bootstrap may not work"
fi

# Destructive confirm.
if (( ! ASSUME_YES )) && (( ! DRY_RUN )); then
  echo
  echo "${YELLOW}${BOLD}!! DESTRUCTIVE !!${RESET}"
  echo "${YELLOW}This will WIPE any existing DIDStorage state on ${BOLD}$TARGET${RESET}${YELLOW}:${RESET}"
  echo "${YELLOW}  - stop didapi.service, asterisk.service"
  echo "${YELLOW}  - drop the 'didstorage' Postgres database AND role"
  echo "${YELLOW}  - rm -rf /opt/didstorage  /etc/didstorage  /var/lib/didstorage${RESET}"
  echo
  read -rp "Type the literal word ${BOLD}WIPE${RESET} to continue: " confirm
  if [[ "$confirm" != "WIPE" ]]; then
    echo "${RED}aborted by user${RESET}" >&2
    exit 8
  fi
fi

# ─────────────────────────────────────────────────────────────
# Stage 2 — Wipe any prior DIDStorage state
# ─────────────────────────────────────────────────────────────

stage "Wipe prior DIDStorage state on $TARGET"

remote_script <<'WIPE_EOF'
set -e
export DEBIAN_FRONTEND=noninteractive
for svc in didapi didbill asterisk sip-capture nginx apache2 lighttpd caddy; do
  systemctl disable --now "$svc.service" 2>/dev/null || true
  systemctl reset-failed "$svc.service" 2>/dev/null || true
done
# Some cloud images ship nginx / apache2 by default and they squat on :80.
# Purge them so didapi can bind to port 80 cleanly. Wildcards catch
# nginx-common, nginx-light, etc.
apt-get -y purge "nginx*" "apache2*" "lighttpd*" "caddy*" 2>/dev/null | tail -3 || true
apt-get -y autoremove --purge 2>/dev/null | tail -1 || true
rm -rf /etc/nginx /etc/apache2 /var/www /var/log/nginx /var/log/apache2

# Drop DB + role if Postgres is installed.
if command -v psql >/dev/null 2>&1 && systemctl is-active postgresql >/dev/null 2>&1; then
  sudo -u postgres psql -d postgres -c "DROP DATABASE IF EXISTS didstorage;"     >/dev/null 2>&1 || true
  sudo -u postgres psql -d postgres -c "DROP ROLE     IF EXISTS didstorage;"     >/dev/null 2>&1 || true
fi
# Remove all DIDStorage paths. /var/lib/asterisk/sounds/didstorage is taken too
# so the new install starts with a clean audio library.
rm -rf /opt/didstorage /etc/didstorage /var/lib/didstorage \
       /var/lib/asterisk/sounds/didstorage \
       /etc/systemd/system/didapi.service \
       /etc/systemd/system/didbill.service \
       /etc/systemd/system/sip-capture.service
# Asterisk config is replaced wholesale; back up first in case the operator
# put something custom in /etc/asterisk we want to recover.
if [ -d /etc/asterisk ]; then
  ts=$(date -u +%Y%m%d-%H%M%S)
  tar -czf "/root/etc-asterisk.backup-$ts.tgz" -C / etc/asterisk 2>/dev/null || true
fi
systemctl daemon-reload
echo "wipe done"
WIPE_EOF
ok "prior state cleared"

# ─────────────────────────────────────────────────────────────
# Stage 3 — Install packages
# ─────────────────────────────────────────────────────────────

stage "Install Debian packages"

remote_script <<'PKG_EOF'
set -e
export DEBIAN_FRONTEND=noninteractive
# tshark asks a debconf question ("Should non-superusers be able to capture
# packets?") that would hang an unattended install. Answer "no" up front —
# we run tshark as root inside the didapi process, non-root capture isn't
# needed.
echo 'wireshark-common wireshark-common/install-setuid boolean false' | debconf-set-selections
apt-get update -qq
# postgresql-15 is the default in bookworm. Asterisk is NOT in Debian 12
# repos any more — we build it from source in the next stage. Everything
# else (incl. asterisk's runtime + build deps) goes in here.
apt-get install -y --no-install-recommends \
  ca-certificates curl wget gnupg lsb-release \
  postgresql postgresql-contrib \
  redis-server \
  ffmpeg sox \
  tcpdump tshark sngrep iproute2 \
  jq rsync ufw \
  vim less \
  build-essential pkg-config autoconf libtool subversion \
  libssl-dev libncurses-dev libxml2-dev libsqlite3-dev libedit-dev \
  libjansson-dev libsrtp2-dev uuid-dev libcurl4-openssl-dev \
  libnewt-dev libpopt-dev libical-dev libspandsp-dev libgsm1-dev \
  libvorbis-dev libogg-dev libresample1-dev unixodbc-dev libneon27-dev
systemctl enable --now postgresql redis-server >/dev/null
echo "packages installed"
PKG_EOF
ok "core packages installed (postgres, redis, ffmpeg, ufw, asterisk build deps)"

# ─────────────────────────────────────────────────────────────
# Stage 3b — Build Asterisk from source
# ─────────────────────────────────────────────────────────────

stage "Build & install Asterisk ${ASTERISK_VERSION} from source (~10 min)"

# Idempotency: skip the build if the same version is already installed
# (e.g. a re-run after the build succeeded but a later stage failed).
ALREADY=$(remote 'asterisk -V 2>/dev/null | head -1' || true)
if echo "$ALREADY" | grep -q "Asterisk ${ASTERISK_VERSION}"; then
  ok "asterisk ${ASTERISK_VERSION} already installed (${ALREADY})"
else
  remote_script <<ASTBUILD_EOF
set -e
mkdir -p /usr/src/asterisk
cd /usr/src/asterisk
TARBALL="asterisk-${ASTERISK_VERSION}.tar.gz"
# Download if missing. downloads.asterisk.org keeps the current point release
# at the main path and rotates older ones into old-releases/. If both 404
# (because upstream rolled to a newer LTS mid-deploy), fall back to the
# 20-current symlink so the deploy doesn't wedge.
if [ ! -f "\$TARBALL" ]; then
  curl -fsSL -o "\$TARBALL" "https://downloads.asterisk.org/pub/telephony/asterisk/asterisk-${ASTERISK_VERSION}.tar.gz" \\
   || curl -fsSL -o "\$TARBALL" "https://downloads.asterisk.org/pub/telephony/asterisk/old-releases/asterisk-${ASTERISK_VERSION}.tar.gz" \\
   || { echo "pinned asterisk-${ASTERISK_VERSION} unavailable, falling back to 20-current"; \\
        curl -fsSL -o "\$TARBALL" "https://downloads.asterisk.org/pub/telephony/asterisk/asterisk-20-current.tar.gz"; }
fi
rm -rf asterisk-*/  # clear any prior extraction
tar -xzf "\$TARBALL"
# 20-current unpacks to whatever the actual version is, so discover it.
SRCDIR="\$(tar -tzf "\$TARBALL" | head -1 | cut -d/ -f1)"
cd "\$SRCDIR"

# pjproject is bundled in-tree — using --with-pjproject-bundled avoids
# Debian's libpjproject (which may be too old). Same for jansson when the
# system version is < what asterisk wants.
./configure --with-pjproject-bundled --with-jansson-bundled >/tmp/asterisk-configure.log 2>&1

# Take the default menuselect (Sample / 'menuselect.makeopts' with stock
# selections). app_voicemail + chan_pjsip + res_pjsip_* are all default-on.
make menuselect.makeopts >/dev/null
make -j"\$(nproc)" >/tmp/asterisk-build.log 2>&1
make install        >/tmp/asterisk-install.log 2>&1
make samples        >/dev/null 2>&1
make config         >/dev/null 2>&1   # installs /etc/init.d/asterisk and the systemd unit
ldconfig

# Patch asterisk.conf so the control socket is group-rw'able. By default
# /var/run/asterisk/asterisk.ctl lands 0755 owned asterisk:asterisk, which
# means didapi (running as 'didstorage', member of 'asterisk' group) can
# CONNECT to read but can't issue commands — every '/live' Hangup / Warn /
# Redirect, every pjsip reload after a supplier-IP edit, every CDR-time
# reload, fails with "Unable to connect to remote asterisk". The fix is:
#  - uncomment the [files] section header (samples ship it commented)
#  - set astctlpermissions = 0660
#  - set astctlgroup = asterisk
# Each sed is idempotent and tolerant of the line being already-correct.
sed -i 's|^;\\[files\\]|[files]|'                       /etc/asterisk/asterisk.conf
sed -i 's|^;astctlpermissions = 0660|astctlpermissions = 0660|' /etc/asterisk/asterisk.conf
sed -i 's|^;astctlgroup = apache|astctlgroup = asterisk|'       /etc/asterisk/asterisk.conf

# Logrotate for asterisk's chatty /var/log/asterisk/*.log files. Without
# this, a long-running instance fills disk in ~7 days (saw 11 GB on the
# May 2026 168.119.33.30 box — drove Postgres into "could not write init
# file" and locked out /login). Three rotations × 500 MB max per file
# is plenty for debugging without ever threatening disk capacity.
cat > /etc/logrotate.d/asterisk <<'LR_EOF'
/var/log/asterisk/*.log /var/log/asterisk/messages /var/log/asterisk/queue_log {
    daily
    rotate 3
    maxsize 500M
    compress
    delaycompress
    missingok
    notifempty
    sharedscripts
    postrotate
        /usr/sbin/asterisk -rx "logger reload" >/dev/null 2>&1 || true
    endscript
}
LR_EOF

# Create the asterisk system user if the package never landed (it usually
# would have, but we don't depend on that any more).
if ! id -u asterisk >/dev/null 2>&1; then
  useradd --system --home /var/lib/asterisk --shell /usr/sbin/nologin asterisk
fi
install -d -o asterisk -g asterisk /var/lib/asterisk /var/log/asterisk \\
                                   /var/spool/asterisk /var/run/asterisk /usr/lib/asterisk
chown -R asterisk:asterisk /etc/asterisk /var/lib/asterisk /var/log/asterisk \\
                          /var/spool/asterisk /var/run/asterisk /usr/lib/asterisk

# 'make config' from Asterisk 20 ships an init.d script *and* attempts a
# systemd unit; some build trees ship neither. Provide a minimal unit so
# Stage 4+ chowns work and asterisk can be brought up. Bootstrap's own
# /etc/asterisk overwrite happens in Stage 8.
cat >/etc/systemd/system/asterisk.service <<UNIT
[Unit]
Description=Asterisk PBX (DIDStorage)
After=network.target postgresql.service redis-server.service didapi.service
Wants=didapi.service

[Service]
Type=simple
User=asterisk
Group=asterisk
ExecStart=/usr/sbin/asterisk -f -U asterisk -G asterisk -vvvg -C /etc/asterisk/asterisk.conf
ExecReload=/usr/sbin/asterisk -rx 'core reload'
Restart=always
RestartSec=4
LimitCORE=infinity
LimitNOFILE=65536
LimitNPROC=infinity
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_NET_RAW
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
echo "asterisk \$(asterisk -V 2>/dev/null) installed from source"
ASTBUILD_EOF
  ok "asterisk ${ASTERISK_VERSION} built + installed from source"
fi

# ─────────────────────────────────────────────────────────────
# Stage 4 — Create system user + dirs
# ─────────────────────────────────────────────────────────────

stage "Create didstorage system user + directories"

remote_script <<'USR_EOF'
set -e
# didstorage system user — non-login, home at /opt/didstorage so envs and
# state live in one tree the operator can grep.
if ! id -u didstorage >/dev/null 2>&1; then
  useradd --system --home /opt/didstorage --shell /usr/sbin/nologin didstorage
fi

# The 'asterisk' user/group is created by the asterisk package. Add
# didstorage to it so didapi can read/write /etc/asterisk/pjsip_*.conf and
# /var/lib/asterisk/sounds/didstorage without being root.
usermod -aG asterisk didstorage || true

install -d -o didstorage -g didstorage /opt/didstorage /opt/didstorage/bin \
        /opt/didstorage/migrations /opt/didstorage/scripts
install -d -o didstorage -g didstorage /var/lib/didstorage \
        /var/lib/didstorage/sip-traces /var/lib/didstorage/kyc
install -d /etc/didstorage
# 0755 (NOT 0750) so the asterisk user can traverse to /etc/didstorage/
# auth_token, which the AGI scripts read. Each file inside stays 0640
# (root:didstorage for didapi.env, root:asterisk for auth_token) so the
# secrets themselves remain readable only by the right service account.
chmod 755 /etc/didstorage
chown root:didstorage /etc/didstorage

# Audio dir — didapi writes (as didstorage), Asterisk reads (as asterisk).
# 2775 = setgid + group-writable so files dropped here inherit the asterisk
# group automatically (audio.Convert sets 0644 on the file itself).
install -d -o didstorage -g asterisk -m 2775 /var/lib/asterisk/sounds/didstorage

# /etc/asterisk needs to be group-writable so didapi can write
# pjsip_users.conf and pjsip_suppliers.conf alongside operator-managed configs.
if [ -d /etc/asterisk ]; then
  chgrp asterisk /etc/asterisk
  chmod g+w /etc/asterisk
fi
echo "user+dirs done"
USR_EOF
ok "user + directories created"

# ─────────────────────────────────────────────────────────────
# Stage 5 — Postgres role + DB
# ─────────────────────────────────────────────────────────────

stage "Postgres role + database"

# Heredoc to remote, with the DB password expanded LOCALLY into the script.
# The remote run keeps the password out of any shell history.
remote_script <<PG_EOF
set -e
sudo -u postgres psql -v ON_ERROR_STOP=1 -d postgres <<SQL
  CREATE ROLE didstorage WITH LOGIN PASSWORD '${DB_PASSWORD}';
  CREATE DATABASE didstorage OWNER didstorage;
SQL
# deploy.sh applies migrations as the postgres superuser, which means
# every table created by a migration is owned by postgres and the
# didstorage role can't even SELECT from it. Grant access on both the
# (currently empty) public schema AND on future objects, so the next
# migration doesn't reintroduce the problem.
sudo -u postgres psql -v ON_ERROR_STOP=1 -d didstorage <<SQL
  GRANT ALL ON SCHEMA public TO didstorage;
  GRANT ALL ON ALL TABLES    IN SCHEMA public TO didstorage;
  GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO didstorage;
  GRANT ALL ON ALL FUNCTIONS IN SCHEMA public TO didstorage;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES    TO didstorage;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO didstorage;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON FUNCTIONS TO didstorage;
SQL
echo "postgres role + db created + grants applied"
PG_EOF
ok "Postgres role didstorage + db didstorage ready (with public-schema grants)"

# ─────────────────────────────────────────────────────────────
# Stage 6 — Generate /etc/didstorage/didapi.env
# ─────────────────────────────────────────────────────────────

stage "Generate /etc/didstorage/didapi.env"

# Build the env file content LOCALLY (so secrets pass through the SSH pipe
# rather than being computed twice), then drop it on the server with the
# tight 0640 mode.
ENV_BODY=$(cat <<EOF
# DIDStorage runtime config — generated by scripts/bootstrap.sh on $(date -u +"%Y-%m-%dT%H:%M:%SZ").
# Owned by root:didstorage, mode 0640. Read by /etc/systemd/system/didapi.service.

LISTEN_ADDR=${LISTEN_ADDR}
DATABASE_URL=postgres://didstorage:${DB_PASSWORD}@127.0.0.1:5432/didstorage?sslmode=disable
REDIS_URL=redis://127.0.0.1:6379/0
KAMAILIO_AUTH_TOKEN=${SIPCTL_TOKEN}
PUBLIC_IP=${PUBLIC_IP}
MIN_AUTH_SECONDS=6

# Reserved for future use by didapi (session cookie signer, etc.).
SESSION_SECRET=${SESSION_SECRET}
EOF
)

remote_script <<DROP_ENV_EOF
set -e
cat > /etc/didstorage/didapi.env <<'ENVBODY'
${ENV_BODY}
ENVBODY
chown root:didstorage /etc/didstorage/didapi.env
chmod 640 /etc/didstorage/didapi.env
# Stash the SIPCTL/auth token in a separate file too — the asterisk AGI
# scripts read it from there so they don't need to parse the env file.
echo -n '${SIPCTL_TOKEN}' > /etc/didstorage/auth_token
chown root:asterisk /etc/didstorage/auth_token
chmod 640 /etc/didstorage/auth_token
echo "env file written"
DROP_ENV_EOF
ok "didapi.env written (DB password, SIPCTL token, PUBLIC_IP=$PUBLIC_IP)"

# ─────────────────────────────────────────────────────────────
# Stage 7 — Systemd units
# ─────────────────────────────────────────────────────────────

stage "Install systemd units"

# Ship all three units in one tar stream. The bundle is local in deploy/central/systemd/.
if (( DRY_RUN )); then
  note "[dry-run] would tar-pipe didapi.service, didbill.service, sip-capture.service"
else
  # Also ship the didbill.timer so the billing job runs on schedule.
  tar -C deploy/central/systemd -czf - \
      didapi.service didbill.service didbill.timer sip-capture.service \
    | "${SSH[@]}" "$TARGET" 'sudo tar -C /etc/systemd/system -xzf - && \
                             chmod 0644 /etc/systemd/system/{didapi,didbill,sip-capture}.service /etc/systemd/system/didbill.timer && \
                             systemctl daemon-reload'
  # Enable everything so it survives reboot. sip-capture starts now (writes
  # the pcaps that back the CDR sip-trace UI); didapi and didbill.timer are
  # marked enabled but not started here — deploy.sh (Stage [08]) starts
  # didapi after the binary is in place, and didbill.timer needs no
  # explicit start (systemd runs it on schedule once enabled).
  remote 'sudo systemctl enable didapi didbill.timer sip-capture && sudo systemctl start sip-capture' \
    >/dev/null 2>&1 || warn "could not enable one of didapi / didbill.timer / sip-capture"
fi
ok "didapi.service + didbill.service + didbill.timer + sip-capture.service installed and enabled"

# ─────────────────────────────────────────────────────────────
# Stage 8 — Base Asterisk configs
# ─────────────────────────────────────────────────────────────

if (( SKIP_ASTERISK )); then
  stage "Asterisk base configs (skipped via --skip-asterisk)"
else
  stage "Install base Asterisk configs (pjsip.conf + extensions.conf + AGI)"

  if (( DRY_RUN )); then
    note "[dry-run] would scp asterisk/pjsip.conf, asterisk/extensions.conf, asterisk/scripts/*.py"
  else
    # Stage dir on the remote
    remote 'sudo install -d -o root -g root -m 0755 /tmp/dids-bootstrap/asterisk \
                                                    /tmp/dids-bootstrap/scripts'
    "${SCP[@]}" -q asterisk/pjsip.conf      "$TARGET:/tmp/dids-bootstrap/asterisk/pjsip.conf"
    "${SCP[@]}" -q asterisk/extensions.conf "$TARGET:/tmp/dids-bootstrap/asterisk/extensions.conf"
    "${SCP[@]}" -q asterisk/scripts/*.py    "$TARGET:/tmp/dids-bootstrap/scripts/"

    remote_script <<AST_EOF
set -e
# pjsip.conf — substitute @PUBLIC_IP@ on the way in.
sed -i "s|@PUBLIC_IP@|${PUBLIC_IP}|g" /tmp/dids-bootstrap/asterisk/pjsip.conf
install -m 0640 -o root -g asterisk /tmp/dids-bootstrap/asterisk/pjsip.conf      /etc/asterisk/pjsip.conf
install -m 0640 -o root -g asterisk /tmp/dids-bootstrap/asterisk/extensions.conf /etc/asterisk/extensions.conf

# The pjsip.conf #includes these — touch them so Asterisk doesn't refuse to
# load the file. didapi rewrites them at first SIP-account or supplier-IP edit.
touch /etc/asterisk/pjsip_users.conf /etc/asterisk/pjsip_suppliers.conf
chown didstorage:asterisk /etc/asterisk/pjsip_users.conf /etc/asterisk/pjsip_suppliers.conf
chmod 0640 /etc/asterisk/pjsip_users.conf /etc/asterisk/pjsip_suppliers.conf

# AGI scripts.
install -d -o asterisk -g asterisk /opt/didstorage/scripts
install -m 0755 -o asterisk -g asterisk /tmp/dids-bootstrap/scripts/dids-authorize.py /opt/didstorage/scripts/
install -m 0755 -o asterisk -g asterisk /tmp/dids-bootstrap/scripts/dids-cdr.py       /opt/didstorage/scripts/

rm -rf /tmp/dids-bootstrap
echo "asterisk configs installed"
AST_EOF
  fi
  ok "Asterisk pjsip.conf + extensions.conf + AGI scripts installed (PUBLIC_IP=$PUBLIC_IP)"
fi

# ─────────────────────────────────────────────────────────────
# Stage 9 — Firewall
# ─────────────────────────────────────────────────────────────

if (( SKIP_FIREWALL )); then
  stage "Firewall (skipped via --skip-firewall)"
else
  stage "Configure ufw firewall (SSH preserved)"

  # Whole stage is best-effort. Some cloud kernels (notably Hetzner's stock
  # cloud image) ship without the nf_conntrack / multiport / addrtype
  # netfilter modules ufw needs, so `ufw --force enable` fails with
  # "iptables-restore: Could not fetch rule set generation id". When that
  # happens we WARN and continue — the operator can configure a Hetzner-
  # side / cloud-provider firewall instead.
  if remote_script <<'FW_EOF'
set -e
# Keep SSH alive while we flip default deny. Without this you can lock
# yourself out of the box.
ufw allow 22/tcp >/dev/null
ufw default deny incoming   >/dev/null
ufw default allow outgoing  >/dev/null
ufw allow 80/tcp            >/dev/null
ufw allow 443/tcp           >/dev/null
ufw allow 5060/udp          >/dev/null
ufw allow 5060/tcp          >/dev/null
# RTP port range. Asterisk's default is 10000-20000.
ufw allow 10000:20000/udp   >/dev/null
ufw --force enable          >/dev/null
ufw status numbered | head -20
FW_EOF
  then
    ok "firewall up: 22/tcp, 80+443/tcp, 5060/udp+tcp, 10000-20000/udp"
  else
    warn "ufw failed — likely a cloud kernel missing netfilter modules. Configure firewall externally (Hetzner/AWS console)."
  fi
fi

# ─────────────────────────────────────────────────────────────
# Stage 9b — Lock SSH to key-only auth (no password auth)
#
# We're already SSHed in via key (every step above used the same key), so
# disabling password auth here can't lock us out. Background context: on
# the May 2026 168.119.33.30 box, SSH logged ~37,000 failed password
# attempts per day from bot-net brute force; one wrong move on a weak
# distro default and that's a foothold. Patching this from day one means
# the password-attack vector is shut at install time, not after-the-fact.
#
# The drop-in lives at /etc/ssh/sshd_config.d/00-didstorage-hardening.conf
# (00- prefix so it sorts BEFORE distro defaults; first-match-wins in
# sshd_config.d). We verify config syntax via `sshd -t` before reloading
# so a malformed write doesn't take SSH down.
#
# Refuses to apply if /root/.ssh/authorized_keys is missing or empty
# (would mean the operator's key was never deployed; flipping password
# auth off in that state locks the box). Skip with --skip-firewall (same
# flag — both are "hardening" steps an offline test wants to bypass).
# ─────────────────────────────────────────────────────────────

if (( SKIP_FIREWALL || SKIP_SSH_HARDENING )); then
  if (( SKIP_SSH_HARDENING )); then
    stage "SSH key-only auth (skipped via --skip-ssh-hardening)"
    warn "Password auth remains ENABLED. Add your public key to /root/.ssh/authorized_keys"
    warn "then run:  rm /etc/ssh/sshd_config.d/00-didstorage-hardening.conf 2>/dev/null; sshd -t && systemctl reload ssh"
    warn "or re-run bootstrap.sh without --skip-ssh-hardening."
  else
    stage "SSH key-only auth (skipped via --skip-firewall)"
  fi
else
  stage "Lock SSH to key-only auth"
  if remote_script <<'SSH_EOF'
set -e
# Bail loudly rather than lock the operator out.
if [ ! -s /root/.ssh/authorized_keys ]; then
  echo "REFUSING: /root/.ssh/authorized_keys missing or empty — would lock you out" >&2
  exit 1
fi
mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/00-didstorage-hardening.conf <<'CFG'
# Written by scripts/bootstrap.sh. See header comment in that script for
# rationale (May 2026 vory-via-xmrig compromise + 37k failed-password
# attempts/day). Drop-ins in sshd_config.d/ are sourced before
# /etc/ssh/sshd_config; FIRST occurrence of any directive wins.
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PermitRootLogin prohibit-password
PermitEmptyPasswords no
LogLevel VERBOSE
LoginGraceTime 20
MaxAuthTries 3
MaxStartups 10:30:60
CFG
# Validate syntax before reload — a broken sshd_config that survives a
# reload locks every future ssh-in.
sshd -t
systemctl reload ssh
echo "effective auth methods:"
sshd -T 2>/dev/null | grep -iE "^(passwordauth|pubkeyauth|permitrootlogin|kbdinteractive)" | sort
SSH_EOF
  then
    ok "SSH locked: key-only, root via key only"
  else
    errln "SSH hardening failed — leaving password auth as-is so you don't get locked out"
    exit 6
  fi
fi

# ─────────────────────────────────────────────────────────────
# Stage 10 — First-time deploy (build, ship, migrate, start)
# ─────────────────────────────────────────────────────────────

stage "Hand off to scripts/deploy.sh for first build + migration pass"

if (( DRY_RUN )); then
  note "[dry-run] would exec: PUBLIC_IP=$PUBLIC_IP scripts/deploy.sh $TARGET ${DEPLOY_FORWARD[*]:-}"
  ok "dry-run complete — re-run without --dry-run to actually bootstrap"
else
  # deploy.sh wants PUBLIC_IP either in the env file (which we just wrote) or
  # in its own env. Pass it through explicitly so the first run doesn't have
  # to ssh-grep the env file.
  PUBLIC_IP="$PUBLIC_IP" SSH_KEY="$SSH_KEY" \
    bash "$PROJECT_ROOT/scripts/deploy.sh" "$TARGET" "${DEPLOY_FORWARD[@]}"
fi

# ─────────────────────────────────────────────────────────────
# Stage 10b — Start asterisk (deploy.sh only RELOADS if already up)
# ─────────────────────────────────────────────────────────────

stage "Start asterisk + verify"

remote_script <<'AST_START_EOF'
set -e
systemctl enable --now asterisk
sleep 2
systemctl is-active asterisk
asterisk -rx 'core show version' 2>/dev/null | head -1 || true
AST_START_EOF
ok "asterisk active"

# ─────────────────────────────────────────────────────────────
# Stage 11 — Admin account: NOT seeded here
#
# On the first HTTP request after bootstrap, didapi redirects /login to
# /setup while the admins table is empty. The operator picks the password
# in-browser and is auto-signed-in.
#
# Why not seed here: shipping a bootstrap-generated password back to the
# operator's terminal + persisting it in `journalctl -u didapi` + risking
# it being in the deploy log is a much bigger threat surface than a
# 60-second /setup window with tos=cs3 TLS in front of it. If you want a
# pre-seeded install for automation, set ADMIN_PASSWORD before running
# bootstrap and add --seed-admin — the check below opts in to the old
# path.
# ─────────────────────────────────────────────────────────────

stage "Admin account (first-run /setup)"

if (( DRY_RUN )); then
  warn "skipped admin bootstrap (dry-run)"
elif [[ "${SEED_ADMIN:-0}" == "1" && -n "${ADMIN_PASSWORD:-}" ]]; then
  # Opt-in: pre-seed via env when the operator explicitly wants automated
  # provisioning (e.g. CI-provisioned dev boxes). Requires htpasswd.
  if ! command -v htpasswd >/dev/null 2>&1; then
    errln "SEED_ADMIN=1 set but htpasswd not in PATH; install apache2-utils or unset SEED_ADMIN"
    exit 6
  fi
  BCRYPT_HASH=$(htpasswd -bnBC 12 "" "$ADMIN_PASSWORD" | tr -d ':\n' | sed 's/^\$2y/\$2a/')
  remote_script <<ADMIN_EOF
set -e
sudo -u postgres psql -v ON_ERROR_STOP=1 -d didstorage <<SQL
  INSERT INTO admins (email, password_hash)
  VALUES ('${ADMIN_EMAIL}', '${BCRYPT_HASH}')
  ON CONFLICT (email) DO UPDATE SET password_hash = EXCLUDED.password_hash;
SQL
ADMIN_EOF
  ok "pre-seeded admin: $ADMIN_EMAIL (via SEED_ADMIN=1)"
else
  ok "admin not seeded — first visit to http://${PUBLIC_IP}/login will redirect to /setup"
fi

# ─────────────────────────────────────────────────────────────
# Done — print credentials summary ONCE
# ─────────────────────────────────────────────────────────────

elapsed=$(( $(date +%s) - STARTED_AT ))
echo
echo "${GREEN}${BOLD}════════════════════════════════════════════════${RESET}"
echo "${GREEN}${BOLD}BOOTSTRAP OK${RESET} — ${elapsed}s · ${TARGET}"
echo "${GREEN}${BOLD}════════════════════════════════════════════════${RESET}"
echo
echo "${BOLD}Endpoints${RESET}"
echo "  Admin GUI         http://${PUBLIC_IP}/login    (fresh install: auto-redirects to /setup)"
echo "  First-run setup   http://${PUBLIC_IP}/setup    ← visit this first"
echo "  Reseller API      http://${PUBLIC_IP}/api/v1/*  (Bearer token from /settings/api-keys)"
echo "  Asterisk SIP      ${PUBLIC_IP}:5060  (UDP+TCP)"
echo
echo "${BOLD}Credentials (save these — they are NOT stored anywhere else by this script)${RESET}"
echo "  Postgres DB pass  ${DB_PASSWORD}"
echo "  X-DIDS-Auth token ${SIPCTL_TOKEN}"
if [[ "${SEED_ADMIN:-0}" == "1" && -n "${ADMIN_PASSWORD:-}" && "${ADMIN_PASSWORD_PROVIDED:-1}" == "0" ]]; then
  # Only echo if we generated it AND SEED_ADMIN opted in. In the default
  # flow the operator picks their own password via /setup, so nothing
  # to echo.
  echo "  Admin email       ${ADMIN_EMAIL}"
  echo "  Admin password    ${ADMIN_PASSWORD}"
fi
echo
echo "${BOLD}Next steps${RESET}"
echo "  1. Open  ${YELLOW}http://${PUBLIC_IP}/setup${RESET}  in a browser NOW and create the admin password"
echo "     (the /setup page is only reachable while no admin exists; it disappears after)"
echo "  2. Point your DNS A record at ${PUBLIC_IP}"
echo "  3. Add suppliers + DIDs through the GUI"
echo "  4. Ongoing deploys:  scripts/deploy.sh ${TARGET}"
echo
