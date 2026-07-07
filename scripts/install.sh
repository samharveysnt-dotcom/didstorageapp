#!/usr/bin/env bash
# DIDStorage on-box installer, git-clone edition.
#
# Usage — on a fresh Debian 12 (Bookworm) VM, as root:
#
#   # Public repo:
#   curl -sSfL https://raw.githubusercontent.com/YOUR_GH_USER/didstorageapp/main/scripts/install.sh | bash
#
#   # Private repo (embed a PAT with `repo` scope):
#   REPO_URL="https://ghp_XXXXX@github.com/YOUR_GH_USER/didstorageapp.git" \
#     curl -sSfL https://raw.githubusercontent.com/YOUR_GH_USER/didstorageapp/main/scripts/install.sh | bash
#
#   # Or after cloning the repo yourself:
#   git clone https://github.com/YOUR_GH_USER/didstorageapp.git /opt/didstorage
#   sudo bash /opt/didstorage/scripts/install.sh
#
# Env overrides:
#   REPO_URL=https://...   Where to git-clone from (default: nexgenvoip repo)
#   BRANCH=main            Which branch to clone (default: main)
#   PUBLIC_IP=1.2.3.4      Force the public IP (default: auto-detected)
#   INSTALL_DIR=/opt/x     Where to clone to (default: /opt/didstorage)

set -euo pipefail

# Change these two to match your GitHub org/repo when you fork.
REPO_URL="${REPO_URL:-https://github.com/samharveysnt-dotcom/didstorageapp.git}"
BRANCH="${BRANCH:-main}"
INSTALL_DIR="${INSTALL_DIR:-/opt/didstorage}"

# ─────────────────────────────────────────────────────────────
# Colour helpers
# ─────────────────────────────────────────────────────────────
if [[ -t 1 ]] && command -v tput >/dev/null 2>&1 && [[ "${TERM:-}" != "dumb" ]]; then
  BOLD=$(tput bold); DIM=$(tput dim); RESET=$(tput sgr0)
  RED=$(tput setaf 1); GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3); BLUE=$(tput setaf 4)
else
  BOLD=""; DIM=""; RESET=""; RED=""; GREEN=""; YELLOW=""; BLUE=""
fi
step() { printf "\n%s>>> %s%s\n" "$BLUE$BOLD" "$*" "$RESET"; }
ok()   { printf "    %s✓ %s%s\n" "$GREEN" "$*" "$RESET"; }
warn() { printf "    %s! %s%s\n" "$YELLOW" "$*" "$RESET"; }
die()  { printf "    %s✗ %s%s\n" "$RED"    "$*" "$RESET" >&2; exit 1; }

# ─────────────────────────────────────────────────────────────
# Step 1 — Preflight
# ─────────────────────────────────────────────────────────────
step "Preflight — Debian 12 + root check"

[[ $EUID -eq 0 ]] || die "Must run as root. Try:  sudo bash $0   (or pipe curl through sudo)"

[[ -f /etc/os-release ]] || die "no /etc/os-release; can't identify OS"
. /etc/os-release
[[ "${ID:-}" == "debian" && "${VERSION_ID:-}" == "12" ]] \
  || die "This installer targets Debian 12 Bookworm. Detected: ${PRETTY_NAME:-unknown}"
ok "root on ${PRETTY_NAME}"

# ─────────────────────────────────────────────────────────────
# Step 2 — Detect public IP
# ─────────────────────────────────────────────────────────────
step "Detect public IP"

if [[ -z "${PUBLIC_IP:-}" ]]; then
  PUBLIC_IP="$(curl -s --max-time 5 https://ipv4.icanhazip.com 2>/dev/null || true)"
fi
if [[ -z "${PUBLIC_IP:-}" ]]; then
  PUBLIC_IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '/src/{for(i=1;i<=NF;i++)if($i=="src")print $(i+1)}' | head -1)"
fi
[[ -n "${PUBLIC_IP:-}" ]] || die "Could not detect public IP. Set PUBLIC_IP=... and re-run."
ok "PUBLIC_IP=${PUBLIC_IP}"

# ─────────────────────────────────────────────────────────────
# Step 3 — Install prerequisites
#
# git, sudo, openssh-server:  bootstrap.sh is an SSH-based deployer
#     that shells out via sudo; minimal netinstalls of Debian 12
#     ship with none of them.
# tar, ca-certificates, curl: fetching the Go tarball.
# Go 1.22:  Debian 12's apt golang-go is 1.19, which lacks log/slog
#     and slices (both Go 1.21+). We fetch a pinned upstream tarball
#     to /usr/local/go and PATH it in.
# ─────────────────────────────────────────────────────────────
step "Install prerequisites: git, tar, ca-certificates, curl, sudo, openssh-server"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq git tar ca-certificates curl sudo openssh-server >/dev/null
systemctl enable --now ssh >/dev/null 2>&1 || systemctl enable --now sshd >/dev/null 2>&1 || true
ok "$(git --version)"
ok "$(sudo --version | head -1)"
ok "sshd active: $(systemctl is-active ssh 2>/dev/null || systemctl is-active sshd 2>/dev/null || echo unknown)"

step "Install Go 1.23 to /usr/local/go (apt's golang-go is 1.19, too old for log/slog)"

# Pin to 1.23 because go.mod declares `toolchain go1.23.0`. Installing 1.22
# here would trigger an auto-download of the 1.23 toolchain from
# proxy.golang.org at build time — which is unreliable on VMs where
# outbound to Google is slow or blocked.
GO_VERSION="${GO_VERSION:-1.23.4}"
GO_ARCH="linux-amd64"
GO_TARBALL="go${GO_VERSION}.${GO_ARCH}.tar.gz"

if [[ -x /usr/local/go/bin/go ]] && /usr/local/go/bin/go version | grep -q "go${GO_VERSION}"; then
  ok "go ${GO_VERSION} already installed at /usr/local/go"
else
  rm -rf /usr/local/go
  curl -fsSL -o "/tmp/${GO_TARBALL}" "https://go.dev/dl/${GO_TARBALL}"
  tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
  rm -f "/tmp/${GO_TARBALL}"
  ok "installed $(/usr/local/go/bin/go version)"
fi

# Make /usr/local/go/bin available to this script AND to bootstrap.sh's
# ssh session below (SendEnv PATH doesn't work with a default sshd config,
# so we prepend to PATH here and rely on the fact that install.sh invokes
# bootstrap.sh in the same shell — bootstrap.sh then uses `go` locally
# during its Stage [03] build).
export PATH="/usr/local/go/bin:${PATH}"
# Persist for future logins too — helps when the operator sshs back in
# and wants to run `go` or re-run bootstrap manually.
if ! grep -q '/usr/local/go/bin' /etc/profile.d/go.sh 2>/dev/null; then
  echo 'export PATH="/usr/local/go/bin:$PATH"' > /etc/profile.d/go.sh
  chmod 0644 /etc/profile.d/go.sh
fi
# Make sure the ssh-to-localhost session also picks up /usr/local/go/bin.
# The default sshd on Debian sources /etc/environment, so drop it there.
if ! grep -q '/usr/local/go/bin' /etc/environment 2>/dev/null; then
  # /etc/environment expects KEY="VALUE" lines with no `export`.
  if grep -q '^PATH=' /etc/environment 2>/dev/null; then
    sed -i 's|^PATH="\(.*\)"$|PATH="/usr/local/go/bin:\1"|' /etc/environment
  else
    echo 'PATH="/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"' >> /etc/environment
  fi
fi

# ─────────────────────────────────────────────────────────────
# Step 4 — Get the source (git clone or reuse existing checkout)
# ─────────────────────────────────────────────────────────────
step "Fetch DIDStorage source from ${REPO_URL}"

# If this installer file itself lives inside a checked-out repo (e.g.
# operator ran `git clone && bash scripts/install.sh`), reuse that.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd 2>/dev/null || true)"
if [[ -n "$HERE" && -f "$HERE/bootstrap.sh" && -f "$HERE/../go.mod" ]]; then
  SRC_DIR="$(cd "$HERE/.." && pwd)"
  ok "using local checkout at ${SRC_DIR}"
elif [[ -d "$INSTALL_DIR/.git" ]]; then
  cd "$INSTALL_DIR"
  git fetch --depth 1 origin "$BRANCH"
  git reset --hard "origin/$BRANCH"
  SRC_DIR="$INSTALL_DIR"
  ok "refreshed existing checkout at ${SRC_DIR}"
else
  # Fresh clone. --depth 1 keeps it small; --branch pins to what you asked for.
  git clone --depth 1 --branch "$BRANCH" "$REPO_URL" "$INSTALL_DIR"
  SRC_DIR="$INSTALL_DIR"
  ok "cloned into ${SRC_DIR}"
fi

# ─────────────────────────────────────────────────────────────
# Step 5 — SSH-to-localhost so bootstrap.sh's remote plumbing works
# ─────────────────────────────────────────────────────────────
step "SSH-to-localhost trust (so bootstrap.sh can drive install stages)"

# Print progress on every substep so a silent exit points at a specific line.
set +e  # temporarily lift `set -e` so we can inspect return codes ourselves
mkdir -p /root/.ssh && chmod 700 /root/.ssh
ok "mkdir /root/.ssh (rc=$?)"

if [[ ! -f /root/.ssh/id_ed25519 ]]; then
  ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519 -C "didstorage-install@$(hostname)"
  ok "generated root SSH key (rc=$?)"
else
  ok "reused existing /root/.ssh/id_ed25519"
fi

# Ensure the pub file exists — if only the private key survived from an
# earlier run, ssh-keygen -y rebuilds the pub from it.
if [[ ! -f /root/.ssh/id_ed25519.pub ]]; then
  ssh-keygen -y -f /root/.ssh/id_ed25519 > /root/.ssh/id_ed25519.pub
  chmod 600 /root/.ssh/id_ed25519.pub
  ok "recreated /root/.ssh/id_ed25519.pub from private key"
fi

touch /root/.ssh/authorized_keys
if ! grep -qxFf /root/.ssh/id_ed25519.pub /root/.ssh/authorized_keys 2>/dev/null; then
  cat /root/.ssh/id_ed25519.pub >> /root/.ssh/authorized_keys
  ok "authorised own key on localhost"
else
  ok "own key already in authorized_keys"
fi
chmod 600 /root/.ssh/authorized_keys
set -e  # restore

touch /root/.ssh/known_hosts
chmod 600 /root/.ssh/known_hosts
ok "touch/chmod known_hosts done"

# Skip ssh-keyscan entirely — we use StrictHostKeyChecking=accept-new on
# the probe which achieves the same "trust on first use" outcome without
# a fragile pipeline that can pipefail-exit the whole installer silently.
ok "will accept host key on first connect (StrictHostKeyChecking=accept-new)"

ok "about to test ssh root@127.0.0.1 …"

# Redirect stdin from /dev/null so ssh doesn't try to consume the piped
# install.sh stream (curl|bash leaves the script bytes on stdin — ssh
# would forward them to the remote or hang). set +e so a failed probe
# doesn't silently kill the installer; we handle rc ourselves.
set +e
ssh -i /root/.ssh/id_ed25519 \
    -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=5 \
    -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/root/.ssh/known_hosts \
    root@127.0.0.1 'echo ok' </dev/null >/tmp/ssh-probe.out 2>/tmp/ssh-probe.err
rc=$?
set -e
if [[ $rc -ne 0 ]]; then
  warn "first ssh probe failed (rc=$rc), waiting 3s and retrying with verbose output"
  cat /tmp/ssh-probe.err >&2 || true
  systemctl is-active ssh >/dev/null 2>&1 || systemctl start ssh 2>/dev/null || true
  sleep 3
  set +e
  ssh -v -i /root/.ssh/id_ed25519 \
      -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=5 \
      -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/root/.ssh/known_hosts \
      root@127.0.0.1 'echo ok' </dev/null
  rc2=$?
  set -e
  if [[ $rc2 -ne 0 ]]; then
    die "ssh root@127.0.0.1 still failing (rc=$rc2). See verbose output above."
  fi
fi
ok "ssh root@127.0.0.1 works"

# ─────────────────────────────────────────────────────────────
# Step 6 — Stage source outside the wipe target
#
# bootstrap.sh's Stage [02] (Wipe prior DIDStorage state) does
# `rm -rf /opt/didstorage` on the remote — which IS 127.0.0.1 in
# the on-box install. If bootstrap.sh runs with cwd inside
# /opt/didstorage, that rm pulls its cwd out from under it and
# later stages using relative paths (deploy/central/systemd,
# asterisk/pjsip.conf) hit "getcwd() failed" / "No such file".
#
# Copy the source to /root/didstorage-build/ (untouched by the
# wipe) and run bootstrap.sh from there.
# ─────────────────────────────────────────────────────────────
step "Stage source at /root/didstorage-build (outside the wipe path)"

BUILD_DIR="/root/didstorage-build"
# If the operator invoked install.sh from inside BUILD_DIR itself, SRC_DIR
# equals BUILD_DIR — an `rm -rf $BUILD_DIR` here would nuke the source we're
# about to copy from. Skip re-staging in that case; the tree is already in
# place.
if [[ "$(cd "$SRC_DIR" && pwd)" == "$BUILD_DIR" ]]; then
  ok "already running from ${BUILD_DIR}; skipping re-stage"
else
  rm -rf "$BUILD_DIR"
  mkdir -p "$BUILD_DIR"
  # Copy everything except .git — bootstrap doesn't need history and this
  # keeps the copy small.
  tar -C "$SRC_DIR" --exclude='.git' -cf - . | tar -C "$BUILD_DIR" -xf -
  ok "source staged at ${BUILD_DIR}"
fi

# ─────────────────────────────────────────────────────────────
# Step 7 — Hand off to bootstrap.sh
# ─────────────────────────────────────────────────────────────
step "Running bootstrap.sh against 127.0.0.1 (~10-15 min, mostly the Asterisk source compile)"

cd "$BUILD_DIR"
export PUBLIC_IP
export SSH_KEY=/root/.ssh/id_ed25519
# --skip-ssh-hardening keeps password auth ON. Reason: the on-box installer
# has no way to know the operator's real public key (they might be running
# it from a hosting-panel console, not SSH), so locking to key-only would
# risk permanently locking them out. The final banner nudges them to
# harden manually once they've added their key.
bash scripts/bootstrap.sh root@127.0.0.1 --yes --skip-ssh-hardening

# ─────────────────────────────────────────────────────────────
# Step 7 — Final banner
# ─────────────────────────────────────────────────────────────
echo
echo "${GREEN}${BOLD}════════════════════════════════════════════════${RESET}"
echo "${GREEN}${BOLD}INSTALL COMPLETE${RESET}"
echo "${GREEN}${BOLD}════════════════════════════════════════════════${RESET}"
echo
echo "  ${BOLD}Open now:${RESET}  ${YELLOW}http://${PUBLIC_IP}/setup${RESET}"
echo
echo "  Pick the admin password there — you'll be signed in on submit."
echo "  /setup disappears the moment the first admin exists."
echo
echo "  ${DIM}Source lives at:  ${SRC_DIR}${RESET}"
echo "  ${DIM}Redeploy new commits:  cd ${SRC_DIR} && git pull && bash scripts/install.sh${RESET}"
echo
echo "  ${YELLOW}${BOLD}Security note:${RESET} SSH still allows password auth (so you"
echo "  don't get locked out from a fresh install). Once you've copied your"
echo "  public key into /root/.ssh/authorized_keys and verified you can"
echo "  ssh in with it, lock the box down with:"
echo
echo "    ${BOLD}bash ${SRC_DIR}/scripts/bootstrap.sh root@127.0.0.1 --yes${RESET}"
echo
echo "  (same bootstrap, but this time without --skip-ssh-hardening — it"
echo "  refuses to run if /root/.ssh/authorized_keys is empty, so it can't"
echo "  lock you out.)"
echo
