# Deploying DIDStorage

DIDStorage is a single Go binary (`didapi`) plus Postgres 15, Redis 6, and
Asterisk 20. Every deploy is driven from your dev machine over SSH; the
server only needs SSH access and a public IPv4.

There are exactly two scripts you need:

| Script | When | What it does |
|---|---|---|
| `scripts/bootstrap.sh` | **Once per new server.** First install, or after wiping. | Destructively rebuilds the box: installs packages, creates users, sets up Postgres, generates `/etc/didstorage/didapi.env` with fresh secrets, installs systemd units, lays down base Asterisk config, opens the firewall, then calls `deploy.sh` to ship the first binary + run all migrations. |
| `scripts/deploy.sh` | **Every code change.** Daily driver. | Cross-compiles `didapi` for `linux/amd64`, ships it + new migration files + asterisk configs, applies any unapplied migrations transactionally, atomically swaps the binary, and restarts `didapi.service`. Idempotent. |

You never SSH in to do a deploy manually. The scripts cover everything.

---

## Prerequisites (your dev machine)

- Go 1.23+ in `PATH` (needed to cross-compile)
- `ssh` + `scp`
- An SSH key the target server's `root` user accepts. Default path is
  `~/.ssh/didstorage_ed25519` — override with `SSH_KEY=/path/to/key`.
- `htpasswd` (Debian: `apt install apache2-utils`) — only needed by
  bootstrap, to bcrypt the seeded admin password. Optional; without it the
  admin user is not auto-seeded and you create one via `/admins/new`.

The scripts run on macOS, Linux, and Git-Bash / WSL on Windows. They do
**not** require Ansible, Docker, or any agent on the server.

## Prerequisites (the server)

- Fresh **Debian 12** (Bookworm). Other Debian-likes may work but are not
  tested. Specifically you need apt + systemd + the `asterisk` package
  available from the default repos.
- Root SSH access by key (no password-auth).
- A reachable public IPv4. Your DNS A record should point at it.
- At least 2 GB RAM, 2 vCPU, 20 GB disk. Modest.
- Open inbound 22/tcp on the firewall side (ufw is configured for you).

## First install on a brand-new server

```sh
PUBLIC_IP=203.0.113.10 scripts/bootstrap.sh root@new.example.com
```

This is destructive — it wipes any prior DIDStorage state on the box
before installing — so you'll be prompted to type **WIPE** to confirm. Pass
`--yes` to skip the prompt in automation.

After ~3–5 minutes the script prints a credentials block with the freshly
generated DB password, the `X-DIDS-Auth` (SIPCTL) token, and an admin
email/password. **Save these — they are not recoverable** (the env file on
the box has the DB password and token, but the admin password is bcrypted
in `admins.password_hash`).

Open `http://203.0.113.10/login`, sign in, change the admin password.

### What `bootstrap.sh` does, in order

1. **Wipe** — `systemctl disable --now` for didapi, didbill, asterisk,
   sip-capture; `DROP DATABASE didstorage`; `DROP ROLE didstorage`;
   `rm -rf /opt/didstorage /etc/didstorage /var/lib/didstorage`. Existing
   `/etc/asterisk` is tar-backed up to `/root/etc-asterisk.backup-<ts>.tgz`.
2. **Install packages** — postgresql-15, redis-server, asterisk 20,
   ffmpeg, sox, tcpdump, sngrep, jq, rsync, ufw.
3. **System user** — creates the `didstorage` user (non-login, home
   `/opt/didstorage`), adds it to the `asterisk` group so didapi can
   write `/etc/asterisk/pjsip_users.conf` and read audio files.
4. **Directories** — `/opt/didstorage/{bin,migrations,scripts}`,
   `/etc/didstorage`, `/var/lib/didstorage/{sip-traces,kyc}`, and
   `/var/lib/asterisk/sounds/didstorage` (setgid, group `asterisk`).
5. **Postgres** — creates the `didstorage` role with a random 32-char
   password and the `didstorage` database owned by it.
6. **Env file** — writes `/etc/didstorage/didapi.env` (mode `0640`, owner
   `root:didstorage`) with `DATABASE_URL`, `REDIS_URL`,
   `KAMAILIO_AUTH_TOKEN` (the `X-DIDS-Auth` shared secret),
   `PUBLIC_IP`, `LISTEN_ADDR`, `MIN_AUTH_SECONDS`. Drops a copy of just
   the SIPCTL token to `/etc/didstorage/auth_token` for the Asterisk AGI
   scripts.
7. **Systemd units** — installs `didapi.service`, `didbill.service`, and
   `sip-capture.service` from `deploy/central/systemd/`.
8. **Asterisk** — installs `asterisk/pjsip.conf` and `extensions.conf`
   (with `@PUBLIC_IP@` substituted), the two AGI scripts to
   `/opt/didstorage/scripts/`, and touches the auto-generated
   `pjsip_users.conf` + `pjsip_suppliers.conf` so the `#include` lines
   don't fail.
9. **Firewall** — ufw allow 22, 80, 443, 5060/udp+tcp, 10000-20000/udp.
   Default deny incoming; SSH is allowed before the policy flips so you
   can't lock yourself out.
10. **Deploy** — calls `scripts/deploy.sh` to do the first cross-compile,
    ship, migrate-from-scratch (every `0001_*` through the latest), and
    start `didapi.service`.
11. **Seed admin** — bcrypts `ADMIN_PASSWORD` and inserts a row into
    `admins`. If `htpasswd` isn't available locally, this step is skipped
    and you create the admin via the `/admins/new` page after first login.

## Ongoing deploys

After bootstrap, every code change ships with:

```sh
scripts/deploy.sh root@hostname
```

`deploy.sh` is **idempotent and non-destructive**:

- It cross-compiles `cmd/didapi` for `linux/amd64` (`-trimpath -ldflags='-s -w'`).
- It applies only migrations whose filenames aren't in `_migrations_log`,
  each inside a single transaction. A failing migration leaves the log
  alone so the next run retries.
- The binary swap is atomic (`mv`). The previous binary is preserved as
  `/opt/didstorage/bin/didapi.previous-<utc-timestamp>` so you can
  manually roll back with `mv` if a release misbehaves.
- Asterisk configs are backed up to `/etc/asterisk/<name>.backup-<utc-ts>`
  before being overwritten, then `asterisk -rx "core reload"` is issued.
- Post-deploy it verifies `GET /login → 200`, PJSIP transport count,
  Asterisk supplier identifies count, audio-dir perms, ffmpeg presence,
  and the `_migrations_log` row count.

### Useful flags

```
--skip-build         Don't rebuild; reuse /tmp/didapi-linux-amd64.
--skip-migrations    Don't touch the schema.
--skip-asterisk      Don't ship pjsip.conf / extensions.conf / AGI.
--skip-verify        Don't run the post-deploy smoke tests.
--baseline           Mark every local migration as applied without running it.
                     Use ONCE on an existing server pre-dating _migrations_log.
--dry-run            Print every remote command instead of executing it.
```

## Switching to a new server

The architecture is server-agnostic — there's no per-server state baked
into the code. Switching means:

1. Spin up the new Debian 12 box. Get root SSH key access on it.
2. Decide what to do with data on the old box:
   - **Keep data:** `pg_dump` the `didstorage` database on the old server,
     run `bootstrap.sh` on the new one (which creates an empty DB),
     `pg_restore` over the new DB **after** bootstrap finishes. Optionally
     also rsync `/var/lib/asterisk/sounds/didstorage` (audio files) and
     `/var/lib/didstorage/kyc` (KYC docs).
   - **Clean slate:** skip the dump. Bootstrap gets you an empty
     installation.
3. `PUBLIC_IP=<new.ip> scripts/bootstrap.sh root@new.host`
4. If you restored data, log in with the OLD admin credentials. If not,
   use the printed bootstrap admin.
5. Update DNS to point at the new IP. Asterisk picks up the new public IP
   from the substituted `pjsip.conf`; SIP suppliers already whitelisted
   in `supplier_ip_groups` continue to work.
6. Decommission the old server: `systemctl stop didapi asterisk;
   systemctl disable didapi asterisk` — that's enough; the binary plus
   `/etc/didstorage/` is the whole state.

## Secret rotation

Each lives in `/etc/didstorage/didapi.env` on the server:

| Secret | Rotate by | Side-effects |
|---|---|---|
| `KAMAILIO_AUTH_TOKEN` | Edit the file, restart `didapi`, also edit `/etc/didstorage/auth_token` and reload Asterisk | The Asterisk AGI scripts use this on `/sipctl/*` requests. Out-of-sync = every call denied. |
| `DATABASE_URL` password | `ALTER USER didstorage WITH PASSWORD '...'`; edit the env file; restart didapi | Atomic from didapi's perspective if you reload the unit. |
| `SESSION_SECRET` | Not yet consumed by the binary; reserved. Edit + restart is harmless. | All existing admin sessions invalidate when this becomes consumed in the future. |

## Rollback

Two paths:

- **Binary only:** the previous binary is at
  `/opt/didstorage/bin/didapi.previous-<utc-ts>`. SSH in and
  `mv` it over `didapi`, then `systemctl restart didapi`. No schema
  changes are reverted by this.
- **Migration:** every migration has a sibling `.down.sql`. Apply it by
  hand (`sudo -u postgres psql -d didstorage -f <file>`), then DELETE
  the matching row from `_migrations_log`, then redeploy the older binary.
  Do this only with `pg_dump` in hand — `.down.sql` files have less
  testing than their `.up.sql` counterparts.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `GET /login → 502` or `→ 000` | didapi crashed; missing env var or DB unreachable | `journalctl -u didapi -n 100 --no-pager` |
| `didapi failed to start (state=inactive)` with "address already in use" on :80 | A cloud-image-default web server (nginx / apache2) is squatting on the port. Bootstrap wipes these on a re-run; for one-offs `apt purge nginx* apache2*` then `systemctl restart didapi`. | Re-run bootstrap, or manually purge + restart. |
| `login error … permission denied for table admins (SQLSTATE 42501)` | Migrations were applied by the `postgres` superuser, so tables are owned by `postgres` and the `didstorage` role can't read them. Bootstrap now grants on a fresh install, but older installs need a one-off backfill. | `sudo -u postgres psql -d didstorage -c "GRANT ALL ON ALL TABLES IN SCHEMA public TO didstorage; GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO didstorage; ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO didstorage;"` |
| `hangup: asterisk -rx: exit status 1 (Unable to connect to remote asterisk …)` from didapi or `/live` actions, despite asterisk being active | `/var/run/asterisk/asterisk.ctl` is mode `0755` (group can read but not write). The `[files]` section in `asterisk.conf` is commented out by default in the source-build samples, so `astctlpermissions = 0660` never takes effect. Bootstrap now patches this; older installs need a manual fix. | `sed -i 's\|^;\\[files\\]\|[files]\|; s\|^;astctlpermissions = 0660\|astctlpermissions = 0660\|; s\|^;astctlgroup = apache\|astctlgroup = asterisk\|' /etc/asterisk/asterisk.conf && systemctl restart asterisk` |
| `dids-authorize.py: PermissionError: '/etc/didstorage/auth_token'` in asterisk log; calls hung up with no SIP cause | `/etc/didstorage` is `0750`; the asterisk user can't traverse to the token file. Bootstrap now uses `0755` on the dir (per-file `0640` still protects the secrets). | `chmod 755 /etc/didstorage` |
| `Permission denied (publickey)` when ssh-ing from a NEW machine, or you need to add a teammate's key | Bootstrap leaves `/etc/ssh/sshd_config.d/00-didstorage-hardening.conf` in place: `PasswordAuthentication no`, `PermitRootLogin prohibit-password`. SSH brute-force noise hammers most cloud boxes (saw ~37k attempts/24h on the May 2026 168.119.33.30 box); password auth is permanently off. | From a working ssh session: `echo 'ssh-ed25519 AAAA... user@host' >> /root/.ssh/authorized_keys`. To re-enable password auth temporarily (don't), `rm /etc/ssh/sshd_config.d/00-didstorage-hardening.conf && systemctl reload ssh`. |
| Box was compromised via a co-hosted app (e.g. an exploited Next.js / WordPress / etc.) dropping miners to `/dev/shm/*` | Co-hosting unrelated apps on the same machine widens the attack surface — every dependency is a potential ingress. Service-account isolation helps (the SSH key-only lock above stops SSH brute force, but doesn't stop in-app RCEs). | Always confine 3rd-party apps to a dedicated unprivileged user with `systemd` hardening (`PrivateTmp=true`, `ProtectSystem=strict`, `ReadWritePaths=` enumerated, `NoNewPrivileges=true`). After a confirmed compromise, treat the box as untrusted — full bootstrap from a clean install onto persistent disks (see "Box is in NFS-rooted rescue mode" below). |
| Bootstrap stage 10 (ufw) prints `iptables-restore: Could not fetch rule set generation id` / `Extension conntrack not supported, missing kernel module` | Cloud kernel ships without the netfilter modules ufw needs. Hetzner's stock cloud image is the common case. Stage is now warn-not-fail, but the host has no in-OS firewall — configure one via your cloud provider's console instead (Hetzner Cloud Firewalls etc.). | Either accept the warn and add a cloud-side firewall, or `apt install linux-image-amd64 && reboot` to switch off the cloud kernel. |
| Calls denied with cause 21 | SIPCTL token mismatch between env file and Asterisk's `/etc/didstorage/auth_token` | Resync the file; reload Asterisk |
| Calls land but Asterisk says "no matching identify" | Supplier IP not in `supplier_ip_group_members` OR Asterisk has stale `pjsip_suppliers.conf` | `/suppliers/{id}#ips` add the IP; didapi reloads PJSIP automatically |
| `bootstrap.sh` fails at "Postgres role + db" | Old `didstorage` role still has owned objects | The wipe stage should drop them; if not, manually `REASSIGN OWNED BY didstorage TO postgres; DROP OWNED BY didstorage;` then re-run |
| Audio playback returns silence | Audio dir perms wrong (didapi can write, asterisk can't read) | `stat /var/lib/asterisk/sounds/didstorage` should be `didstorage:asterisk 2775`; fix with `chmod 2775 + chgrp asterisk` |
| Bootstrap admin can't log in | bcrypt cost mismatch, or htpasswd unavailable so seed was skipped | If `htpasswd` isn't on your dev box, bake a one-shot bcrypt helper: `mkdir /tmp/bch && cd /tmp/bch && cat > main.go <<EOF` … see the snippet below. Then `INSERT INTO admins (email, password_hash) VALUES (...) ON CONFLICT (email) DO UPDATE SET password_hash = EXCLUDED.password_hash;` |

### Seeding an admin without htpasswd

The bootstrap admin step uses `htpasswd` (from `apache2-utils`) if it's
on the dev box. If it isn't, use this Go one-liner (the project already
depends on `golang.org/x/crypto/bcrypt`):

```sh
mkdir /tmp/bch && cd /tmp/bch && cat > main.go <<'EOF'
package main
import ("fmt"; "os"; "golang.org/x/crypto/bcrypt")
func main() {
  h, err := bcrypt.GenerateFromPassword([]byte(os.Args[1]), 12)
  if err != nil { panic(err) }
  fmt.Print(string(h))
}
EOF
go mod init bch >/dev/null && go get golang.org/x/crypto/bcrypt >/dev/null
go build -o /tmp/bch-bin .

HASH=$(/tmp/bch-bin 'YourPasswordHere')
ssh root@HOST "sudo -u postgres psql -d didstorage -X <<SQL
  INSERT INTO admins (email, password_hash)
  VALUES ('admin@didstorage.local', '$HASH')
  ON CONFLICT (email) DO UPDATE SET password_hash = EXCLUDED.password_hash;
SQL"
```

The bcrypt cost (12) matches what `didapi` uses to verify — don't lower
it.

## File layout on the server

After a successful bootstrap:

```
/opt/didstorage/
  bin/didapi                     # the binary
  bin/didapi.previous-*          # rollback copies (one per deploy)
  migrations/*.sql               # canonical migration directory
  scripts/dids-{authorize,cdr}.py # AGI scripts
/etc/didstorage/
  didapi.env                     # 0640 root:didstorage
  auth_token                     # 0640 root:asterisk  (just the SIPCTL token)
/etc/systemd/system/
  didapi.service
  didbill.service
  sip-capture.service
/etc/asterisk/
  pjsip.conf                     # base — operator-edited if needed
  pjsip_users.conf               # auto-generated by didapi
  pjsip_suppliers.conf           # auto-generated by didapi
  extensions.conf                # base — operator-edited if needed
/var/lib/didstorage/
  sip-traces/sip-YYYYMMDD.pcap   # rotated daily, 7-day retention
  kyc/                           # uploaded KYC docs
/var/lib/asterisk/sounds/didstorage/
  af_<random>.slin               # uploaded audio files (Playback resolves)
```

The `/var/log/didstorage/` directory is reserved but unused — didapi logs
go to journald. `journalctl -u didapi` is the live tail.
