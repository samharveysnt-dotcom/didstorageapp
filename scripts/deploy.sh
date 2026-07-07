#!/usr/bin/env bash
# scripts/deploy.sh — single-file Debian 12 deploy for DIDStorage.
#
# Usage:
#   scripts/deploy.sh root@HOST [flags]
#
# Flags:
#   --skip-build         Skip cross-compiling the binary (use existing /tmp/didapi-linux-amd64)
#   --skip-migrations    Skip the schema migration pass
#   --skip-asterisk      Skip shipping pjsip.conf / extensions.conf / AGI scripts
#   --skip-verify        Skip the post-deploy smoke tests
#   --baseline           Mark every local migration as applied without running it.
#                        Use ONCE on an existing server that pre-dates _migrations_log
#                        so subsequent deploys only run NEW migrations.
#   --dry-run            Print every remote command instead of executing it.
#   --help               Show this help.
#
# Env overrides:
#   SSH_KEY              SSH key path                 (default: ~/.ssh/didstorage_ed25519)
#   SSH_USER             User for SSH login           (parsed from argv if user@host given)
#   PUBLIC_IP            Public IP for pjsip.conf     (auto-discovered from /etc/didstorage/didapi.env)
#   BUILD_OUT            Local path for the built binary  (default: /tmp/didapi-linux-amd64)
#   REMOTE_STAGE         Remote staging dir           (default: /tmp/didstorage-deploy)
#
# Exit codes:
#   0  success
#   2  bad usage / args
#   3  precondition failed (missing tool, ssh unreachable, etc.)
#   4  build failed
#   5  transfer failed
#   6  remote setup / migration / reload failed
#   7  post-deploy verification failed
#
# Design notes:
#   * `set -euo pipefail` everywhere — single failure stops the run.
#   * A trap prints the stage label, line, and command that broke before exiting.
#   * Every migration is applied inside a transaction; success records the
#     filename in _migrations_log so the next deploy is idempotent. Existing
#     servers should run with `--baseline` once to backfill the log.
#   * The binary swap is atomic (mv). If anything later fails, the previous
#     binary is preserved as /opt/didstorage/bin/didapi.previous-<timestamp>.
#   * Asterisk configs are backed up alongside the live file before swap.
#   * Cold-boot dirs / files are created with the conventional ownership
#     and modes on every run; idempotent.

set -euo pipefail

# ─────────────────────────────────────────────────────────────
# Args + defaults
# ─────────────────────────────────────────────────────────────

TARGET=""
SSH_KEY="${SSH_KEY:-$HOME/.ssh/didstorage_ed25519}"
BUILD_OUT="${BUILD_OUT:-/tmp/didapi-linux-amd64}"
REMOTE_STAGE="${REMOTE_STAGE:-/tmp/didstorage-deploy}"
PUBLIC_IP="${PUBLIC_IP:-}"

SKIP_BUILD=0
SKIP_MIGRATIONS=0
SKIP_ASTERISK=0
SKIP_VERIFY=0
BASELINE=0
DRY_RUN=0

usage() {
  sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-build)       SKIP_BUILD=1 ;;
    --skip-migrations)  SKIP_MIGRATIONS=1 ;;
    --skip-asterisk)    SKIP_ASTERISK=1 ;;
    --skip-verify)      SKIP_VERIFY=1 ;;
    --baseline)         BASELINE=1 ;;
    --dry-run)          DRY_RUN=1 ;;
    --help|-h)          usage ;;
    --)                 shift; break ;;
    -*)                 echo "unknown flag: $1" >&2; usage ;;
    *)                  if [[ -z "$TARGET" ]]; then TARGET="$1"; else echo "unexpected arg: $1" >&2; usage; fi ;;
  esac
  shift
done

[[ -z "$TARGET" ]] && { echo "missing TARGET (user@host)" >&2; usage; }

# ─────────────────────────────────────────────────────────────
# Pretty output + stage/trap machinery
# ─────────────────────────────────────────────────────────────

if [[ -t 1 ]] && command -v tput >/dev/null 2>&1 && [[ "${TERM:-}" != "dumb" ]]; then
  BOLD=$(tput bold); DIM=$(tput dim); RESET=$(tput sgr0)
  RED=$(tput setaf 1); GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3); BLUE=$(tput setaf 4); CYAN=$(tput setaf 6)
else
  BOLD=""; DIM=""; RESET=""; RED=""; GREEN=""; YELLOW=""; BLUE=""; CYAN=""
fi

CURRENT_STAGE="(starting)"
STAGE_NUM=0
STARTED_AT=$(date +%s)

stage() {
  STAGE_NUM=$((STAGE_NUM + 1))
  CURRENT_STAGE="$*"
  printf "\n%s>>> [%02d] %s%s\n" "$BLUE$BOLD" "$STAGE_NUM" "$CURRENT_STAGE" "$RESET"
}
note()   { printf "    %s· %s%s\n" "$DIM" "$*" "$RESET"; }
ok()     { printf "    %s✓ %s%s\n" "$GREEN" "$*" "$RESET"; }
warn()   { printf "    %s! %s%s\n" "$YELLOW" "$*" "$RESET"; }
errln()  { printf "    %s✗ %s%s\n" "$RED" "$*" "$RESET" >&2; }

on_error() {
  local exit_code=$?
  local line=${BASH_LINENO[0]:-?}
  echo
  echo "${RED}${BOLD}════════════════════════════════════════════════${RESET}" >&2
  echo "${RED}${BOLD}DEPLOY FAILED${RESET}" >&2
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
    # Print the dry-run banner to STDERR so $(remote ...) captures the
    # natural empty stdout. Real runs forward the remote command's stdout
    # transparently as you'd expect.
    printf '%s[dry-run] ssh %s "%s"%s\n' "$DIM" "$TARGET" "$*" "$RESET" >&2
    return 0
  fi
  "${SSH[@]}" "$TARGET" "$@"
}

remote_pipe_in() {
  # remote_pipe_in <remote command> — stdin is sent as the remote command's stdin.
  if (( DRY_RUN )); then
    printf '%s[dry-run] ssh %s "%s" < <stdin>%s\n' "$DIM" "$TARGET" "$*" "$RESET" >&2
    cat >/dev/null
    return 0
  fi
  "${SSH[@]}" "$TARGET" "$@"
}

copy_to_stage() {
  # copy_to_stage <local> [<local>...] — uploads to $REMOTE_STAGE
  if (( DRY_RUN )); then
    printf '%s[dry-run] scp %s -> %s:%s%s\n' "$DIM" "$*" "$TARGET" "$REMOTE_STAGE" "$RESET" >&2
    return 0
  fi
  "${SCP[@]}" "$@" "$TARGET:$REMOTE_STAGE/"
}

# psql with a here-doc; runs as the postgres superuser so DDL works.
# Stops at the first error and rolls back the implicit transaction.
psql_remote() {
  remote "sudo -u postgres psql -d didstorage -v ON_ERROR_STOP=1 -X -q -A -t" \
    2> >(grep -v 'could not change directory to' >&2 || true)
}

# Find the project root (the dir containing this script's parent).
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

# ─────────────────────────────────────────────────────────────
# Stage 1 — Preconditions
# ─────────────────────────────────────────────────────────────

stage "Preconditions"

REQUIRED_TOOLS=(go ssh scp)
MISSING=()
for t in "${REQUIRED_TOOLS[@]}"; do
  command -v "$t" >/dev/null 2>&1 || MISSING+=("$t")
done
if (( ${#MISSING[@]} > 0 )); then
  errln "missing required local tools: ${MISSING[*]}"
  exit 3
fi
ok "local tools present: ${REQUIRED_TOOLS[*]}"

if [[ ! -f "$SSH_KEY" ]]; then
  errln "SSH key not found at $SSH_KEY (override with SSH_KEY=...)"
  exit 3
fi
ok "SSH key: $SSH_KEY"

if ! remote 'echo ok' >/dev/null 2>&1; then
  errln "cannot SSH to $TARGET with key $SSH_KEY"
  exit 3
fi
ok "SSH ${TARGET} reachable"

remote_os=$(remote 'cat /etc/os-release 2>/dev/null | sed -n "s/^PRETTY_NAME=\"\(.*\)\"$/\1/p"' || echo unknown)
note "remote OS: ${remote_os:-unknown}"

# ─────────────────────────────────────────────────────────────
# Stage 2 — Resolve PUBLIC_IP
# ─────────────────────────────────────────────────────────────

stage "Resolve PUBLIC_IP for pjsip.conf"

if [[ -z "$PUBLIC_IP" ]]; then
  PUBLIC_IP=$(remote 'grep -E "^PUBLIC_IP=" /etc/didstorage/didapi.env 2>/dev/null | head -1 | cut -d= -f2- | tr -d "\"\r"' || true)
fi
if [[ -z "$PUBLIC_IP" ]]; then
  PUBLIC_IP=$(remote 'grep -oE "external_(media_address|signaling_address)=[0-9.]+" /etc/asterisk/pjsip.conf 2>/dev/null | head -1 | cut -d= -f2' || true)
fi
if [[ -z "$PUBLIC_IP" ]]; then
  # Final fallback: parse the host part of TARGET when it looks like an IPv4.
  host="${TARGET#*@}"
  if [[ "$host" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    PUBLIC_IP="$host"
    warn "PUBLIC_IP not configured anywhere — falling back to SSH host $host"
  fi
fi
if [[ -z "$PUBLIC_IP" ]]; then
  errln "cannot determine PUBLIC_IP; set it via env (PUBLIC_IP=1.2.3.4) or in /etc/didstorage/didapi.env"
  exit 3
fi
ok "PUBLIC_IP: $PUBLIC_IP"

# ─────────────────────────────────────────────────────────────
# Stage 3 — Build the binary
# ─────────────────────────────────────────────────────────────

if (( SKIP_BUILD )); then
  stage "Build (skipped — using $BUILD_OUT as-is)"
  [[ -f "$BUILD_OUT" ]] || { errln "$BUILD_OUT missing — re-run without --skip-build"; exit 4; }
  note "binary size: $(stat -c%s "$BUILD_OUT" 2>/dev/null || stat -f%z "$BUILD_OUT") bytes"
else
  stage "Build didapi for linux/amd64"
  if (( DRY_RUN )); then
    note "[dry-run] would build $BUILD_OUT"
  else
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
      go build -trimpath -ldflags='-s -w' \
      -o "$BUILD_OUT" ./cmd/didapi
    ok "binary built: $BUILD_OUT ($(stat -c%s "$BUILD_OUT" 2>/dev/null || stat -f%z "$BUILD_OUT") bytes)"
  fi
fi

# ─────────────────────────────────────────────────────────────
# Stage 4 — Cold-boot prep (idempotent)
# ─────────────────────────────────────────────────────────────

stage "Cold-boot prep — dirs, perms, conventional ownership"

remote 'set -e
mkdir -p /opt/didstorage/bin /opt/didstorage/migrations /opt/didstorage/scripts \
         /etc/didstorage \
         /var/lib/didstorage/sip-traces /var/lib/didstorage/kyc \
         /var/lib/asterisk/sounds/didstorage \
         '"$REMOTE_STAGE"'

# /etc/asterisk needs to be group-writable so didapi can write
# pjsip_users.conf and pjsip_suppliers.conf alongside the operator-managed
# pjsip.conf. Convention: didstorage:asterisk 750 on the dir.
if [ -d /etc/asterisk ]; then
  chown didstorage:asterisk /etc/asterisk 2>/dev/null || true
  chmod g+w /etc/asterisk
fi

# pjsip_suppliers.conf — auto-regenerated; readable by Asterisk (group asterisk).
touch /etc/asterisk/pjsip_suppliers.conf
chown didstorage:asterisk /etc/asterisk/pjsip_suppliers.conf
chmod 640 /etc/asterisk/pjsip_suppliers.conf

# Audio sounds dir — didapi writes, asterisk reads. 2775 = setgid + group write
# so files dropped here inherit the asterisk group, allowing Playback() to
# read them with the 0644 mode audio.Convert sets at write time.
chown didstorage:asterisk /var/lib/asterisk/sounds/didstorage
chmod 2775 /var/lib/asterisk/sounds/didstorage

# KYC + trace dirs owned by didstorage (didapi process user).
chown -R didstorage:didstorage /var/lib/didstorage/sip-traces /var/lib/didstorage/kyc 2>/dev/null || true
'
ok "cold-boot dirs and perms verified"

# ─────────────────────────────────────────────────────────────
# Stage 5 — Ship files to a stage dir
# ─────────────────────────────────────────────────────────────

stage "Ship migrations + asterisk configs + AGI scripts to $REMOTE_STAGE"

remote "rm -rf $REMOTE_STAGE && mkdir -p $REMOTE_STAGE/migrations $REMOTE_STAGE/asterisk $REMOTE_STAGE/scripts"

# Migrations — always shipped, even if --skip-migrations, so the staging
# dir mirrors source and a future operator can inspect them.
if [[ -d migrations ]] && compgen -G "migrations/*.sql" >/dev/null; then
  if (( DRY_RUN )); then
    note "[dry-run] would scp migrations/*.sql"
  else
    "${SCP[@]}" -q migrations/*.sql "$TARGET:$REMOTE_STAGE/migrations/"
  fi
  ok "uploaded $(ls -1 migrations/*.sql 2>/dev/null | wc -l) migration files"
else
  warn "no migrations/*.sql found locally"
fi

# Binary
if (( DRY_RUN )); then
  note "[dry-run] would scp $BUILD_OUT"
else
  "${SCP[@]}" -q "$BUILD_OUT" "$TARGET:$REMOTE_STAGE/didapi.new"
fi
ok "uploaded didapi binary"

# Asterisk configs + AGI scripts
if (( SKIP_ASTERISK == 0 )); then
  if (( DRY_RUN )); then
    note "[dry-run] would scp asterisk/{pjsip,extensions}.conf and asterisk/scripts/*.py"
  else
    [[ -f asterisk/pjsip.conf      ]] && "${SCP[@]}" -q asterisk/pjsip.conf      "$TARGET:$REMOTE_STAGE/asterisk/pjsip.conf"
    [[ -f asterisk/extensions.conf ]] && "${SCP[@]}" -q asterisk/extensions.conf "$TARGET:$REMOTE_STAGE/asterisk/extensions.conf"
    if compgen -G "asterisk/scripts/*.py" >/dev/null; then
      "${SCP[@]}" -q asterisk/scripts/*.py "$TARGET:$REMOTE_STAGE/scripts/"
    fi
  fi
  ok "uploaded asterisk configs + AGI scripts"
else
  note "asterisk stage skipped (--skip-asterisk)"
fi

# Always copy migrations into the canonical location so the runbook lookups
# match what's on disk after deploy (the actual apply uses the staged copy).
remote "rsync -a --delete $REMOTE_STAGE/migrations/ /opt/didstorage/migrations/ 2>/dev/null \
        || cp -f $REMOTE_STAGE/migrations/*.sql /opt/didstorage/migrations/ 2>/dev/null || true
        chown -R root:root /opt/didstorage/migrations 2>/dev/null || true
        chmod 644 /opt/didstorage/migrations/*.sql 2>/dev/null || true"
ok "migrations copied to /opt/didstorage/migrations/"

# ─────────────────────────────────────────────────────────────
# Stage 6 — Migrations
# ─────────────────────────────────────────────────────────────

if (( SKIP_MIGRATIONS )); then
  stage "Migrations (skipped)"
  note "skipped via --skip-migrations"
else
  stage "Migrations"

  note "ensuring _migrations_log meta-table exists"
  printf '%s\n' "CREATE TABLE IF NOT EXISTS _migrations_log (
    filename    TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_by  TEXT NOT NULL DEFAULT current_user
  );" | psql_remote >/dev/null

  # If the log is empty but the user table already exists, this server
  # predates the tracking table. Auto-baseline (or --baseline forces it).
  log_count=$(printf '%s\n' "SELECT count(*) FROM _migrations_log;" | psql_remote | tr -d '[:space:]')
  has_users=$(printf '%s\n' "SELECT to_regclass('public.users') IS NOT NULL;" | psql_remote | tr -d '[:space:]')

  if (( BASELINE )) || { [[ "$log_count" == "0" ]] && [[ "$has_users" == "t" ]]; }; then
    if (( BASELINE )); then
      note "--baseline given — marking every local migration as applied"
    else
      note "log empty + 'users' table present → auto-baselining existing schema"
    fi
    # shellcheck disable=SC2012
    for mig in $(ls migrations/*.up.sql 2>/dev/null | sort); do
      fname=$(basename "$mig")
      printf "INSERT INTO _migrations_log (filename) VALUES ('%s') ON CONFLICT DO NOTHING;\n" "$fname" | psql_remote >/dev/null
      note "baseline: $fname"
    done
    log_count=$(printf '%s\n' "SELECT count(*) FROM _migrations_log;" | psql_remote | tr -d '[:space:]')
    ok "baseline complete — log now holds $log_count migrations"
  fi

  # Apply any migration whose filename isn't in the log yet.
  applied=0
  # shellcheck disable=SC2012
  for mig in $(ls migrations/*.up.sql 2>/dev/null | sort); do
    fname=$(basename "$mig")
    found=$(printf "SELECT 1 FROM _migrations_log WHERE filename='%s';\n" "$fname" | psql_remote | tr -d '[:space:]')
    if [[ "$found" == "1" ]]; then
      continue
    fi
    note "applying $fname"
    if (( DRY_RUN )); then
      note "[dry-run] would apply $fname"
      continue
    fi
    # Run inside a single transaction so a syntax error doesn't half-apply.
    # If the migration ITSELF wraps BEGIN/COMMIT, that's fine — Postgres
    # tolerates the no-op outer transaction.
    remote "sudo -u postgres psql -d didstorage -v ON_ERROR_STOP=1 -1 -f $REMOTE_STAGE/migrations/$fname" \
      2> >(grep -v 'could not change directory to' >&2 || true) | sed 's/^/      /' || {
        errln "migration $fname failed — see psql output above. Server schema is in a half-applied state."
        exit 6
      }
    printf "INSERT INTO _migrations_log (filename) VALUES ('%s');\n" "$fname" | psql_remote >/dev/null
    applied=$((applied+1))
    ok "applied $fname"
  done

  if (( applied == 0 )); then
    ok "no new migrations to apply"
  else
    ok "$applied migration(s) applied"
  fi
fi

# ─────────────────────────────────────────────────────────────
# Stage 7 — Asterisk configs + AGI scripts (idempotent)
# ─────────────────────────────────────────────────────────────

if (( SKIP_ASTERISK )); then
  stage "Asterisk + AGI (skipped)"
else
  stage "Asterisk configs + AGI scripts"

  remote "set -e
    if [ -f $REMOTE_STAGE/asterisk/pjsip.conf ]; then
      sed -i 's|@PUBLIC_IP@|$PUBLIC_IP|g' $REMOTE_STAGE/asterisk/pjsip.conf
      if [ -f /etc/asterisk/pjsip.conf ]; then
        cp -a /etc/asterisk/pjsip.conf /etc/asterisk/pjsip.conf.backup-\$(date -u +%Y%m%d-%H%M%S)
      fi
      mv $REMOTE_STAGE/asterisk/pjsip.conf /etc/asterisk/pjsip.conf
      chown root:asterisk /etc/asterisk/pjsip.conf
      chmod 640 /etc/asterisk/pjsip.conf
    fi
    if [ -f $REMOTE_STAGE/asterisk/extensions.conf ]; then
      if [ -f /etc/asterisk/extensions.conf ]; then
        cp -a /etc/asterisk/extensions.conf /etc/asterisk/extensions.conf.backup-\$(date -u +%Y%m%d-%H%M%S)
      fi
      mv $REMOTE_STAGE/asterisk/extensions.conf /etc/asterisk/extensions.conf
      chown root:asterisk /etc/asterisk/extensions.conf
      chmod 640 /etc/asterisk/extensions.conf
    fi
    if ls $REMOTE_STAGE/scripts/*.py >/dev/null 2>&1; then
      mkdir -p /opt/didstorage/scripts
      cp -f $REMOTE_STAGE/scripts/*.py /opt/didstorage/scripts/
      chown asterisk:asterisk /opt/didstorage/scripts/*.py
      chmod 755 /opt/didstorage/scripts/*.py
    fi
  "
  ok "pjsip.conf + extensions.conf staged with PUBLIC_IP=$PUBLIC_IP"

  # Reload Asterisk if it's running; if not, leave it for the operator.
  if remote 'systemctl is-active asterisk' >/dev/null 2>&1; then
    remote 'asterisk -rx "core reload" >/dev/null' && ok "asterisk core reload done" || warn "asterisk reload returned non-zero (check 'asterisk -rx \"core show channels\"')"
  else
    warn "asterisk service inactive — not reloading"
  fi
fi

# ─────────────────────────────────────────────────────────────
# Stage 8 — Atomic binary swap + restart didapi
# ─────────────────────────────────────────────────────────────

stage "Swap didapi binary + restart"

remote "set -e
  if [ -f /opt/didstorage/bin/didapi ]; then
    cp -a /opt/didstorage/bin/didapi /opt/didstorage/bin/didapi.previous-\$(date -u +%Y%m%d-%H%M%S)
  fi
  chown didstorage:didstorage $REMOTE_STAGE/didapi.new
  chmod 755 $REMOTE_STAGE/didapi.new
  mv $REMOTE_STAGE/didapi.new /opt/didstorage/bin/didapi
  systemctl restart didapi
"
sleep 2
state=$(remote 'systemctl is-active didapi' || true)
if [[ "$state" == "active" ]]; then
  ok "didapi active"
else
  errln "didapi failed to start (state=$state)"
  remote 'journalctl -u didapi -n 30 --no-pager' >&2 || true
  exit 6
fi

# ─────────────────────────────────────────────────────────────
# Stage 9 — Smoke verification
# ─────────────────────────────────────────────────────────────

if (( SKIP_VERIFY )); then
  stage "Verify (skipped)"
else
  stage "Verify"

  http=$(remote 'curl -s -o /dev/null -w "%{http_code}" http://localhost/login' || echo 000)
  if [[ "$http" == "200" ]]; then
    ok "GET /login → 200"
  else
    errln "GET /login → $http (expected 200)"; exit 7
  fi

  if remote 'systemctl is-active asterisk >/dev/null'; then
    # pjsip show transports format varies between Asterisk versions; some
    # leave a leading space, some don't. Drop the anchor and just grep
    # the literal keyword.
    transports=$(remote 'asterisk -rx "pjsip show transports" 2>/dev/null | grep -c "Transport:"' || echo 0)
    note "pjsip transports loaded: $transports"
    suppliers=$(remote 'wc -l < /etc/asterisk/pjsip_suppliers.conf 2>/dev/null' || echo 0)
    note "pjsip_suppliers.conf lines: $suppliers"
    identifies=$(remote 'asterisk -rx "pjsip show identifies" 2>/dev/null | grep -c "Identify:"' || echo 0)
    note "pjsip identifies registered: $identifies"
  else
    warn "asterisk inactive — skipping pjsip checks"
  fi

  audio_dir=$(remote 'stat -c "%U:%G %a" /var/lib/asterisk/sounds/didstorage 2>/dev/null' || echo missing)
  if [[ "$audio_dir" == "didstorage:asterisk 2775" ]]; then
    ok "audio sounds dir: $audio_dir"
  else
    warn "audio sounds dir is '$audio_dir' (expected 'didstorage:asterisk 2775')"
  fi

  ffmpeg_path=$(remote 'command -v ffmpeg 2>/dev/null' || true)
  if [[ -n "$ffmpeg_path" ]]; then
    ok "ffmpeg: $ffmpeg_path"
  else
    warn "ffmpeg not in PATH on server — audio uploads will fail. Run: apt-get install -y ffmpeg"
  fi

  total_migrations=$(printf '%s\n' "SELECT count(*) FROM _migrations_log;" | psql_remote 2>/dev/null | tr -d '[:space:]' || echo ?)
  ok "_migrations_log holds $total_migrations entries"
fi

# ─────────────────────────────────────────────────────────────
# Done
# ─────────────────────────────────────────────────────────────

elapsed=$(( $(date +%s) - STARTED_AT ))
echo
echo "${GREEN}${BOLD}════════════════════════════════════════════════${RESET}"
echo "${GREEN}${BOLD}DEPLOY OK${RESET} — ${elapsed}s · ${TARGET}"
echo "${GREEN}${BOLD}════════════════════════════════════════════════${RESET}"
