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
- `/etc/asterisk/codecs.conf`, `/etc/asterisk/rtp.conf`, and
  `/etc/sysctl.d/99-voip-tuning.conf` — deploy ships the tuned defaults
  from the repo but installs each **only when no on-box file exists**, so
  any operator hand-tune is preserved. The definitive versions live in
  `asterisk/` and `deploy/central/sysctl.d/` in the repo; delete the
  on-box file to opt back in to the repo version.

If the deploy verify fails, `didapi` doesn't get restarted with the
broken binary — the old one keeps serving.

### Automatic pre-deploy backup

Every non-dry-run deploy takes a `pg_dump -Fc` snapshot **before**
running any migration, so a semantically-wrong migration is always
recoverable:

```
/var/backups/didstorage/predeploy-<UTC-timestamp>.dump
```

Retention: newest 14 kept, older pruned automatically. The dump is
verified with `pg_restore -l` before deploy proceeds — if the dump is
unreadable, deploy aborts before touching the DB.

To roll back a bad deploy:

```bash
systemctl stop didapi                          # release the pool connections
sudo -u postgres dropdb didstorage
sudo -u postgres createdb -O didstorage didstorage
sudo -u postgres pg_restore -d didstorage /var/backups/didstorage/predeploy-<TS>.dump
systemctl start didapi
```

To roll back the binary specifically (data intact), the previous three
copies are kept as `/opt/didstorage/bin/didapi.previous-<TS>`. Atomic swap:

```bash
mv /opt/didstorage/bin/didapi.previous-<TS> /opt/didstorage/bin/didapi.rollback
mv /opt/didstorage/bin/didapi /opt/didstorage/bin/didapi.broken
mv /opt/didstorage/bin/didapi.rollback /opt/didstorage/bin/didapi
systemctl restart didapi
```

## Full re-install (destructive — wipes everything)

Only if you truly want a clean slate. Runs bootstrap Stage [02] which
drops the Postgres database, deletes `/etc/didstorage`,
`/var/lib/didstorage`, and removes systemd units — every admin, DID,
supplier, order, CDR, uploaded file, and KYC bundle goes with it.

**Populated-DB guard:** `bootstrap.sh` refuses to run against a server
whose `cdrs` table is not empty. This runs **regardless** of `--yes`,
so automation can't destroy a production DB by pointing at the wrong
host. To override on a test box you're deliberately abandoning:

```bash
I_REALLY_MEAN_IT=1 bash scripts/bootstrap.sh root@<host> --yes
```

On a truly fresh server (no `cdrs` table) the guard passes silently and
install proceeds.

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

Check the two capture services (SIP-signalling and RTP-media are split
so a heavy RTP trace extraction can't OOM didapi during a SIP-trace
lookup):

```bash
systemctl status sip-capture rtp-capture
ls -la /var/lib/didstorage/sip-traces/
```

`sip-*.pcap` files back the SIP trace viewer; `rtp-*.pcap` files back
the Call Quality tab. Both rotate hourly, 168 and 48 files retained
respectively.

Reload Asterisk config after a manual edit to `/etc/asterisk/*.conf`.
Prefer this over `systemctl restart asterisk` — a reload is zero-call-loss:

```bash
asterisk -rx "core show channels count"    # ALWAYS check first
asterisk -rx "module reload res_pjsip.so"  # pjsip.conf changes
asterisk -rx "dialplan reload"             # extensions.conf changes
asterisk -rx "module reload res_rtp_asterisk.so"  # rtp.conf changes
asterisk -rx "core reload"                 # everything (what deploy.sh does)
```

**NEVER on a box carrying traffic:**

- `asterisk -rx "core stop now"` — hangs up every active call
- `asterisk -rx "core restart now"` — same, plus restart downtime
- `systemctl restart asterisk` — same, if channels are active

For a config that requires actual restart (`asterisk.conf`, modules,
systemd drop-in changes):

```bash
asterisk -rx "core stop gracefully"        # refuses NEW calls, drains existing
```

## Audio-quality tuning (Asterisk-side)

The default install includes the packet-loss-concealment interlock
established by the 2026-08-05 audit. All four gates are set:

1. `genericplc => true` (Asterisk built-in default, in `codecs.conf`)
2. `genericplc_on_equal_codecs => true` (`codecs.conf` line 75)
3. `[supplier-trunk]` and `[outbound]` allow only `ulaw`/`alaw` (no G.722)
4. `JITTERBUFFER` set on the called leg via `b(jb-called-trunk^s^1)`
   or `b(jb-called-user^s^1)` in every `Dial()` in `extensions.conf`

Verify all four in one command:

```bash
asterisk -rx "core show settings" | grep -i "Generic PLC"      # both Enabled
asterisk -rx "pjsip show endpoint outbound" | grep "^ allow"   # (ulaw|alaw)
asterisk -rx "dialplan show jb-called-trunk" | head -3         # context exists
```

If you change any of these, verify with a test call and measure with
`tshark -q -z rtp,streams` — target is 50 pps / 20 ms mean delta /
zero gaps > 40 ms on the outbound stream. See
[docs/RTP-TIMESTAMP-INVERSION.md](docs/RTP-TIMESTAMP-INVERSION.md)
for the one residual defect (only fires under packet loss, does not
block audio, requires an Asterisk patch to fully fix).

## Config surface — what's where

| Path | Purpose | Hand-editable? |
|---|---|---|
| `/opt/didstorage/bin/didapi` | The Go monolith. Handles GUI, reseller API, `/sipctl/*` control plane. | No — deployed |
| `/opt/didstorage/bin/didbill` | Nightly billing job (runs via `didbill.timer`). | No — deployed |
| `/etc/didstorage/didapi.env` | DB URL, Redis URL, SIPCTL token, PUBLIC_IP. | Yes — restart didapi |
| `/etc/asterisk/pjsip.conf` | Base PJSIP config (transports + supplier / outbound endpoints). | Mirror in git and re-deploy — deploy overwrites |
| `/etc/asterisk/extensions.conf` | Dialplan — `[from-supplier]`, `[jb-called-*]`, `[admin-actions]`. | Mirror in git and re-deploy — deploy overwrites |
| `/etc/asterisk/pjsip_users.conf` | Per-tenant SIP account endpoints (auto-written by didapi on every user change). | **NEVER** — regenerated at runtime |
| `/etc/asterisk/pjsip_suppliers.conf` | Per-supplier IP identifies (auto-written by didapi on every supplier IP change). | **NEVER** — regenerated at runtime |
| `/etc/asterisk/codecs.conf` | Codec + PLC settings. Line 75 `genericplc_on_equal_codecs`. | Yes — deploy leaves alone |
| `/etc/asterisk/rtp.conf` | RTP transport (strictrtp, rtcpinterval, port range). | Yes — deploy leaves alone |
| `/etc/sysctl.d/99-voip-tuning.conf` | UDP receive-buffer + VM tuning. | Yes — deploy leaves alone |
| `/etc/systemd/journald.conf.d/didstorage.conf` | Journal caps (500 M / 90 d). | Yes — `systemctl restart systemd-journald` |
| `/opt/didstorage/scripts/dids-authorize.py` | AGI called on every incoming INVITE; retries on transport failure. | No — deployed |
| `/opt/didstorage/scripts/dids-cdr.py` | AGI called at hangup; retries 3× on POST failure. | No — deployed |
| `/var/lib/didstorage/sip-traces/` | Hourly `sip-*.pcap` (168 files) + `rtp-*.pcap` (48 files, 200-byte snaplen). | Yes — safe to prune |
| `/var/lib/didstorage/kyc/` | KYC bundle uploads (private). | Yes but back up first |
| `/var/lib/asterisk/sounds/didstorage/` | Uploaded audio files for audio-playback routes. | Yes but back up first |
| `/var/backups/didstorage/` | Auto pg_dump before every deploy. Newest 14 kept. | Yes — safe to add copies |
| `/etc/systemd/system/didapi.service` | didapi unit. `OOMPolicy=continue`, `MemoryMax=1.5G`, `StartLimit`. | Repo has canonical version |
| `/etc/systemd/system/didbill.service` + `.timer` | Nightly billing job (runs `didbill` binary). | Repo has canonical version |
| `/etc/systemd/system/sip-capture.service` | tcpdump for SIP-signalling pcaps. Hourly rotation. | Repo has canonical version |
| `/etc/systemd/system/rtp-capture.service` | tcpdump for RTP-header pcaps. Hourly rotation, 200-byte snaplen. | Repo has canonical version |
| `/etc/systemd/system/asterisk.service` | Asterisk unit. `RuntimeDirectory=asterisk`, `StartLimit`. | Repo has canonical version |

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
migration. The service has `StartLimitBurst=5` / `StartLimitIntervalSec=60`
so a broken build fails visibly as `active (start-limit-hit)` after 10 s
instead of thrashing forever.

**`asterisk -rx` returns "Unable to connect"** — `/var/run/asterisk/`
ownership is wrong. `RuntimeDirectory=asterisk` in the unit fixes this
on next restart, but for a running instance:

```bash
chown asterisk:asterisk /var/run/asterisk
chmod 2755 /var/run/asterisk
systemctl restart asterisk       # ONLY if 0 active channels
```

**Reconciler is silent when it should be sweeping** — probably means
Asterisk is unreachable from didapi. The reconciler explicitly skips
(rather than false-evicts) when `asterisk -rx` fails. Fix the socket
per the previous section and it resumes on the next tick (~1 s).

**Ghost "Active" rows on `/live`** — reconciler should sweep within
1-2 s of the underlying Asterisk channel dying. If a row persists past
that, either the channel really is still alive (check
`asterisk -rx "core show channels" | grep <chan-name>`) or the reconciler
is silent for the reason above.

**`didbill.timer` fails with 203/EXEC** — `/opt/didstorage/bin/didbill`
missing. Fixed by re-running `bash scripts/deploy.sh root@<host>`;
Stage 3 now builds `cmd/didbill` alongside `cmd/didapi`.

**Journal disk usage climbing** — check `/etc/systemd/journald.conf.d/didstorage.conf`
exists (500 M / 90 d cap). If missing, install ships it via bootstrap;
on an existing box, create it manually:

```bash
cat >/etc/systemd/journald.conf.d/didstorage.conf <<EOF
[Journal]
SystemMaxUse=500M
SystemKeepFree=200M
MaxRetentionSec=90day
EOF
systemctl restart systemd-journald
journalctl --vacuum-size=500M    # apply immediately
```

## Related docs

- [docs/RTP-TIMESTAMP-INVERSION.md](docs/RTP-TIMESTAMP-INVERSION.md) —
  §6 residual defect from the 2026-08-05 audit, an Asterisk-level RTP
  timestamp anomaly under packet loss. Not blocking, needs a future
  Asterisk patch or upgrade to fully close.

## License

Private. Contact the operator.
