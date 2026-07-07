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
REPO_URL="${REPO_URL:-https://github.com/YOUR_GH_USER/didstorageapp.git}"
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
# Step 3 — Install prerequisites (git first so we can clone)
# ─────────────────────────────────────────────────────────────
step "Install prerequisites: git, golang, tar, ca-certificates"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq git golang-go tar ca-certificates >/dev/null
ok "$(go version 2>/dev/null || echo 'go missing')"
ok "$(git --version)"

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

mkdir -p /root/.ssh
chmod 700 /root/.ssh

if [[ ! -f /root/.ssh/id_ed25519 ]]; then
  ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519 -C "didstorage-install@$(hostname)" >/dev/null
  ok "generated root SSH key"
fi

if ! grep -qxFf /root/.ssh/id_ed25519.pub /root/.ssh/authorized_keys 2>/dev/null; then
  cat /root/.ssh/id_ed25519.pub >> /root/.ssh/authorized_keys
  ok "authorised own key on localhost"
fi
chmod 600 /root/.ssh/authorized_keys

touch /root/.ssh/known_hosts
chmod 600 /root/.ssh/known_hosts
for h in localhost 127.0.0.1; do
  ssh-keyscan -H "$h" 2>/dev/null | while read -r line; do
    grep -qxF "$line" /root/.ssh/known_hosts || echo "$line" >> /root/.ssh/known_hosts
  done
done

if ! ssh -i /root/.ssh/id_ed25519 -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=5 \
      root@127.0.0.1 'echo ok' >/dev/null 2>&1; then
  die "ssh root@127.0.0.1 with the just-generated key doesn't work. Check sshd is running."
fi
ok "ssh root@127.0.0.1 works"

# ─────────────────────────────────────────────────────────────
# Step 6 — Hand off to bootstrap.sh
# ─────────────────────────────────────────────────────────────
step "Running bootstrap.sh against 127.0.0.1 (~10-15 min, mostly the Asterisk source compile)"

cd "$SRC_DIR"
export PUBLIC_IP
export SSH_KEY=/root/.ssh/id_ed25519
bash scripts/bootstrap.sh root@127.0.0.1 --yes

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
