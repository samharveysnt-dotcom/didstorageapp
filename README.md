# DIDStorage

Multi-tenant DID hosting / SIP routing platform. Postgres + Redis + Asterisk
built from source. Runs on a single Debian 12 (Bookworm) VM.

## First install — fresh Debian 12 VM

Log in as **root** on a clean Debian 12 Bookworm box (any provider). One
command does everything: install prerequisites, clone the repo, build
Asterisk, set up Postgres/Redis/systemd units, configure ufw, start
services. Takes ~10-15 minutes on a 2 vCPU box (the Asterisk source
compile is the slow bit).

```bash
apt update && apt install -y curl \
  && curl -sSfL https://raw.githubusercontent.com/samharveysnt-dotcom/didstorageapp/main/scripts/install.sh | bash
```

When it finishes you'll see:

```
════════════════════════════════════════════════
INSTALL COMPLETE
════════════════════════════════════════════════

  Open now:  http://<your-VM-ip>/setup
```

Visit that URL in your browser, pick an admin password (min 12
characters), submit — you're signed in. `/setup` disappears the moment
the first admin exists.

## Redeploy new features — no data loss

Use `deploy.sh` when you've pushed new commits (bug fixes, features) and
want to pick them up without touching the database, capture files, or
existing config. **This is the command you want ~95% of the time.**

```bash
cd \
  && rm -rf /root/didstorage-src \
  && git clone https://github.com/samharveysnt-dotcom/didstorageapp.git /root/didstorage-src \
  && cd /root/didstorage-src \
  && PUBLIC_IP=$(hostname -I | awk '{print $1}') SSH_KEY=/root/.ssh/id_ed25519 \
       bash scripts/deploy.sh root@127.0.0.1
```

What this does:

1. Fetches the latest source
2. Rebuilds `didapi` (Go binary) and restarts it
3. Ships updated `pjsip.conf` / `extensions.conf` / AGI scripts and reloads Asterisk
4. Applies any new database migrations forward-only
5. Runs a health check

Does NOT touch:

- Postgres data (admins, suppliers, DIDs, users, orders, CDRs, KYC docs)
- `/etc/didstorage/didapi.env` (DB password, SIPCTL token, PUBLIC_IP)
- `/var/lib/didstorage/` (KYC bundles, uploaded audio, sip-traces / pcaps)
- SSH config, firewall rules
- The Asterisk source build (unless you set `ASTERISK_VERSION` to a new
  release, in which case bootstrap's Asterisk stage is opt-in via a
  separate flag)

If the deploy verify fails, `didapi` doesn't get restarted with the
broken binary — the old one keeps serving.

## Full re-install (destructive — wipes everything)

Only if you truly want a clean slate. Runs bootstrap Stage [02] which
drops the Postgres database, deletes `/etc/didstorage`,
`/var/lib/didstorage`, and removes systemd units — every admin, DID,
supplier, order, CDR, uploaded file, and KYC bundle goes with it.

```bash
cd \
  && rm -rf /opt/didstorage \
  && git clone https://github.com/samharveysnt-dotcom/didstorageapp.git /opt/didstorage \
  && bash /opt/didstorage/scripts/install.sh
```

## SSH hardening (do once, after you've added your public key)

`install.sh` leaves password SSH login enabled so you don't lock
yourself out on a fresh install. Once you've copied a public key from
your workstation into `/root/.ssh/authorized_keys` and verified you can
SSH in with it (from a **second terminal**, so you can't get locked
out), lock the box down:

```bash
bash /root/didstorage-src/scripts/bootstrap.sh root@127.0.0.1 --yes
```

That runs the same bootstrap flow but WITHOUT `--skip-ssh-hardening`,
which enables:

- `PasswordAuthentication no`
- `PermitRootLogin prohibit-password`
- `MaxAuthTries 3`
- Rate limits + verbose sshd logging

Bootstrap refuses to run if `/root/.ssh/authorized_keys` is empty or
missing — you can't accidentally lock yourself out this way.

## Common operations

Check services:

```bash
systemctl status didapi asterisk sip-capture didbill.timer postgresql redis-server
```

Live logs (didapi is the Go monolith serving the GUI + reseller API +
SIP control plane):

```bash
journalctl -u didapi -f
```

Test SIP is listening:

```bash
asterisk -rx 'pjsip show transports'
asterisk -rx 'pjsip show endpoints'
```

Check the SIP + RTP capture is running (backs the trace + call-quality
tabs on `/cdrs/{id}/sip-trace`):

```bash
systemctl status sip-capture
ls -la /var/lib/didstorage/sip-traces/
```

Reload Asterisk config after a manual edit to `/etc/asterisk/*.conf`
(rare — the GUI ships regenerated configs itself):

```bash
asterisk -rx 'core reload'
```

## Config surface — what's where

| Path | Purpose |
|---|---|
| `/opt/didstorage/bin/didapi` | The Go monolith. Handles GUI, reseller API, `/sipctl/*` control plane. |
| `/etc/didstorage/didapi.env` | DB URL, Redis URL, SIPCTL token, PUBLIC_IP. |
| `/etc/asterisk/pjsip.conf` | Base PJSIP config (transports + supplier trunk template). |
| `/etc/asterisk/pjsip_users.conf` | Per-tenant SIP account endpoints (written by didapi). |
| `/etc/asterisk/pjsip_suppliers.conf` | Per-supplier IP identifies (written by didapi). |
| `/etc/asterisk/extensions.conf` | Dialplan — reserved DIDs and the `from-suppliers` context. |
| `/opt/didstorage/scripts/dids-authorize.py` | AGI called on every incoming INVITE; asks didapi via `/sipctl/authorize`. |
| `/opt/didstorage/scripts/dids-cdr.py` | AGI called at hangup; posts CDR to didapi. |
| `/var/lib/didstorage/sip-traces/` | Rolling daily SIP + RTP pcaps (~7 day retention). |
| `/var/lib/didstorage/kyc/` | KYC bundle uploads (private). |
| `/var/lib/asterisk/sounds/didstorage/` | Uploaded audio files for audio-playback routes. |
| `/etc/systemd/system/didapi.service` | didapi systemd unit (User=didstorage, ProtectSystem etc). |
| `/etc/systemd/system/didbill.service` + `.timer` | Nightly billing job. |
| `/etc/systemd/system/sip-capture.service` | tcpdump wrapper feeding the pcaps. |

## Requirements

- Debian 12 Bookworm (installer refuses to run on anything else).
- x86_64 architecture. Anything else — arm64 VMs, RaspberryPi — would
  need a manual Asterisk build with different flags.
- Root shell for the initial install (later steps can run as any user
  with sudo).
- Outbound HTTPS to `deb.debian.org`, `github.com`, `go.dev`,
  `downloads.asterisk.org` during install.
- Inbound: 80 (GUI), 5060 UDP+TCP (SIP), 10000-20000 UDP (RTP media).
  UFW rules are set automatically.

## Troubleshooting

**Trace page says "No SIP packets captured for this call"** —
`sip-capture` service isn't running or the call landed before capture
started. Check `systemctl status sip-capture` and `ls /var/lib/didstorage/sip-traces/`.

**Call Quality tab says "No RTP media"** — the pcap filter isn't
watching Asterisk's RTP port range. This is fixed in current
`sip-capture.service` (installs capture UDP 5060/5061 + 10000-20000).
Reload with `systemctl restart sip-capture`.

**Bootstrap stops silently at "SSH-to-localhost trust"** — sshd or sudo
isn't installed on this fresh netinstall. `install.sh` handles both now;
if you hit this on an older clone, `apt install -y openssh-server sudo`
and re-run install.sh.

**didapi keeps restarting** — check `journalctl -u didapi -n 100`. Most
common causes: DB password mismatch (`/etc/didstorage/didapi.env` vs
Postgres role), Redis not listening on `127.0.0.1:6379`, an unapplied
migration.

## License

Private. Contact the operator.
