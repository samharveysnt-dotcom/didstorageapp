# DIDStorage — progress

Multi-tenant inbound DID hosting / SIP routing platform.
Live on Debian 12 at **45.8.93.244**.

**Current state:** post-Phase-2.5, plus live-action channel-name capture, multi-supplier interconnect engine, uniform busy-tone deny path, KYC promotion fix, DID bulk importer, `/live` channel-state visibility, **audio-clip-as-DID-reservation feature**, **configurable per-rate-card customer billing increment** (migration `0017_billing_increments`), and a **portable two-script deploy flow** — `scripts/bootstrap.sh` for fresh-server installs and `scripts/deploy.sh` for incremental updates. **Deploy procedure documented in `DEPLOY.md` at the project root** — read it before pointing the scripts at a new host. Phase-3 (WebRTC ChanSpy listen) + ACME auto-provisioning + outbound-Call-ID trace merging were built and then rolled back on 2026-05-12 — see §8. Restore points live under `checkpoints/NNNN-<slug>/`; latest is `checkpoints/0007-pre-shape-redesign/` (snapshot taken right before a Linear-class redesign of `layout.html` and `cdrs.html` lands).

---

## 1. Where we are

A single Go binary (`didapi`) serves three roles on the same listener:

| Role | Path prefix | Auth |
|---|---|---|
| Admin GUI (HTML over chi) | `/` | scs session cookie |
| Reseller REST API | `/api/v1/*` | bearer-token (`api_keys` row, bcrypt-hashed) |
| Asterisk control plane | `/sipctl/*` | shared `X-DIDS-Auth` token |

Plus an HTTPS twin listener on `:443` that comes up when `site.https_listen_addr` is set AND at least one row in `site_domains` carries a usable cert, using SNI to pick the right cert per request.

Postgres 15 + Redis 6 alongside; Asterisk 20 fronts the SIP signalling. Real calls land, get authorized via `/sipctl/authorize`, route per the order's `route_kind`/`route_target`, and get billed at call end via `/sipctl/cdr`. The `/live` page shows in-flight calls (SSE) and supports admin Hangup / Warn / Redirect actions; any of those stamps the resulting CDR's `admin_action` column and writes a compliance-log entry.

16 migrations applied plus a meta-table `_migrations_log` managed by `scripts/deploy.sh`. The `checkpoints/` directory at the project root holds numbered self-contained restore points (`0001-initial-migration-0012/`, `0002-pre-shadcn/`, `0003-pre-phase3/`, `0004-live-channel-fix/`, `0005-multi-supplier-and-busy-tone/`, `0006-audio-files-and-deploy-script/`, `0007-pre-shape-redesign/`). Convention: new checkpoints get the next sequential 4-digit prefix, mirroring how migrations are numbered. Each checkpoint contains `source/`, `db/`, `progress.md`, `instruct.md`.

---

## 2. What's running live

### Schema state (after migration 0016)

**20 application tables.** `users`, `orders`, `dids`, `cdrs`, `denied_calls`, `balance_ledger`, `suppliers`, `supplier_ip_groups`, `supplier_ip_group_members`, `rate_cards`, `kyc_bundles`, `kyc_documents`, `user_block_log`, `api_keys`, `admins`, `hangup_causes`, `settings`, `site_domains`, `did_reservation_history`, `audio_files`, plus reference data (`countries`, `pops`, `resellers`, `sessions`) and the deploy meta-table `_migrations_log`.

Key fields added since the original schema:

- `cdrs.routed_kind/target/did_id` — call-time route snapshot (0005)
- `cdrs.supplier_charge_cents/bill_min/bill_increment` — supplier-cost snapshot (0011)
- `rate_cards.bill_min_seconds/bill_increment_seconds` + `cdrs.bill_min_seconds/bill_increment_seconds` — customer-side billing increment, configurable per rate card (0017). Notation "min/inc" seconds: 60/60 = 60s connection minimum + 60s round-up (legacy default), 60/1 = 60s minimum + per-second, 6/6 = 6s minimum + 6s increments. CHECK (>0 AND ≤600). CDRs snapshot the values so card edits never disturb history. Mirrors the long-standing `supplier_bill_*` pattern.
- `cdrs.siptrace_json` + `siptrace_computed_at` — persisted trace blob (0007)
- `cdrs.admin_action / admin_action_by / admin_action_reason` — admin Hangup/Warn/Redirect stamp (0014)
- `did_reservation_history` — full reserve→release audit trail (0012)
- `dids.reserved_route_*` — admin-supplied test route for reserved DIDs (0005)
- `orders.pre_quarantine_*` — restore-on-unblock route memory (0005)
- `rate_cards` partial unique on `(supplier_id, country_iso, did_type) WHERE valid_to IS NULL` (0011)
- `hangup_causes.family ∈ {sip, platform}` with Title-Cased labels (0010)
- `settings`, `site_domains` for HTTPS SNI + runtime config (0009)
- `supplier_ip_group_members.hostname` (nullable) with CHECK exactly one of (cidr, hostname) (0015)
- `audio_files` library + `route_kind` enum now four-valued (sip_account, sip_uri, ip, **audio**) + `dids.reserved_audio_file_id` FK with ON DELETE RESTRICT (0016)
- `user_block_action` enum has an orphan `live_listen` value from the rolled-back Phase 3 — left in PG (can't drop enum values), unreferenced by code (see §8)

### Runtime services

| Process | Listens | Purpose |
|---|---|---|
| `didapi` (systemd) | `:80` (always) + `:443` (when site_domains has a cert AND `site.https_listen_addr` is set) | All HTTP/HTTPS routes |
| Asterisk 20 | UDP/5060 | SIP signalling; calls into `/sipctl` for decisions |
| Postgres 15 | `127.0.0.1:5432` | All persistent state |
| Redis 6 | `127.0.0.1:6379` | Channel reservations (`act:user:*`, `act:did:*`), live-call registry (`live:active`, `live:meta:*`), admin-action pending stash (`pending:admin_action:<call_id>`) |
| `sip-capture` (rolling tcpdump) | promiscuous on the SIP iface | Pcaps for trace replay; 7-day retention under `/var/lib/didstorage/sip-traces/` |
| `didbill` (cron) | one-shot | Anniversary billing run (separate binary) |

### Smoke tests we've kept green

- **Real call flow:** supplier IP → Asterisk INVITE → `/sipctl/authorize` → allow → `Dial(PJSIP/outbound/<uri>)` → BYE → `/sipctl/cdr` → CDR row + balance ledger debit + supplier-charge snapshot + siptrace precompute.
- **Denial flows:** `unknown_did`, `unauthorized_ip`, `insufficient_balance`, `insufficient_channels`, `user_blocked`, `quarantined`, `did_not_assigned`, `reservation_misconfigured` — each writes the expected combination of `cdrs` + `denied_calls` rows.
- **Compliance cascade:** block user → orders → `quarantined`; subsequent INVITEs return `decision: deny, reason: quarantined, sip_code: 480`. Unblock restores the pre-quarantine route from `orders.pre_quarantine_*`.
- **Live actions:** admin Hangup / Warn / Redirect from `/live` reach Asterisk via `asterisk -rx`; resulting CDR carries `admin_action`, `admin_action_by`, `admin_action_reason`; compliance log gets a `live_*` row.
- **KYC:** create bundle → upload doc → admin approve → dependent `kyc_pending` orders flip to `active`.
- **Trace pipeline:** at call end, sipctl spawns a goroutine that runs `siptrace.Lookup` + persists JSON to `cdrs.siptrace_json`; subsequent trace-page loads are sub-50 ms vs ~16 s cold. The sequence diagram overlays admin-action markers when the call was hungup/warned/redirected via `/live`.

---

## 3. What we've accomplished (phase by phase)

The work landed in deploy phases. Each phase = one or more migrations + matching code + GUI + verification.

### Phase 0 — Bootstrap
- Asterisk 20 + Postgres 15 + Redis 6 on Debian 12.
- chi/v5 router, pgx/v5, scs/v2 sessions in Postgres, html/template + embed.FS.
- First end-to-end real call billed correctly.

### Phase 1 — Domain restructure (0004)
- Renamed `orders` (customer-level) → `users`, `did_assignments` → `orders`.
- KYC bundles + documents, `denied_calls`, `user_block_log`, `kyc_pending` + `quarantined` order statuses, `resellers.brand_name`.
- Reseller API surface re-shaped to match: `/api/v1/users` + `/api/v1/orders`.

### Phase 2 — Operational features
- KYC workflow (admin GUI + reseller API + multipart uploads to `/var/lib/didstorage/kyc/{user}/{bundle}/`).
- Block / quarantine cascade + 480 reply.
- Compliance CSV exports (cdrs / ledger / blocks per user; cdrs per order; date-ranged).
- Sidebar GUI redesign, responsive, paginated tables, sort + search + modals.
- Cross-table global search.
- Reseller-side SIP-trace sanitization.

### Phase 3 — Schema polish (0005)
- `-1` sentinel for uncapped channel caps; `channel_count >= 0` floor.
- DID reservations: `dids.reserved_route_*` + `'reserved'` status; sipctl short-circuits to the admin-supplied route, no billing.
- `cdrs.did_id` + `routed_kind` + `routed_target` snapshotted at call time.
- FK actions softened on `cdrs` / `balance_ledger` / `user_block_log` so `userDelete` preserves CDR audit.

### Phase 4 — Suppliers redesign (0006)
- `rate_cards.supplier_*` cost columns + bill min/inc (CHECK ∈ {1, 6, 15, 30, 60}).
- `/suppliers/{id}` tabbed: Details / IP whitelisting / Rate cards / DIDs.
- Bulk-paste IPs; bulk CSV rate-card import.

### Phase 5 — SIP trace persistence + UX (0007)
- `cdrs.siptrace_json` JSONB + 5-second-delayed precompute goroutine.
- Trace page tabs: Overview (SVG sequence diagram), Message Timeline, Raw Dump.
- "End result" pill on every trace page.
- `domain.CleanCallerURI` for display.
- Compact + precise duration in `/cdrs`.

### Phase 6 — Cause-code editor (0008 + 0010)
- `hangup_causes` table seeded with 33 SIP cause codes + 7 platform reasons.
- `internal/causes` in-memory map with hot reload.
- `/cause-codes` admin page; built-ins protected.
- 0010 renamed family `q850 → sip`, Title-Cased every label, prefixed details `SIPN — `.

### Phase 7 — Settings + HTTPS (0009)
- `internal/settings` DB-backed key-value cache.
- `internal/sslmgr` `tls.Config.GetCertificate` with SNI + wildcard fallback + default.
- `:443` listener brought up only when `site.https_listen_addr` is set AND `ssl.IsConfigured()`. Admins paste cert+key PEM in `/settings#domains`.
- `/settings` page with four tabs (Company / Site / Admins / Domains & SSL).

### Phase 8 — Profit + rate-card UX (0011)
- `cdrs.supplier_charge_cents` + bill min/inc snapshotted on the `/cdr` path.
- `domain.SupplierChargeForCall` mirrored byte-for-byte in JS for live preview.
- `/suppliers/{id}#rates` redesigned: country-grouped expandable rows + profit-per-duration drop-down.
- `rate_cards` partial unique index — at most one active rate per (supplier, country, did_type).
- Rate-card modal two-column with live preview tables (per-call profit at 30…210s; first-month total at N DIDs × N channels).
- Profit columns added to `/cdrs`, `/orders`, `/resellers`.

### Phase 9 — `/orders` + `/resellers` re-layout
- `/orders` toolbar (search + reseller filter + status + per-page + Apply), sortable headers, profit / revenue columns, per-row edit modal.
- `/resellers` rebuilt with stats (users count, active orders, aggregate balance — **excludes blocked users** — 30-day revenue, API keys), sort + filter + pagination.

### Phase 10 — DID reservation history (0012)
- `did_reservation_history` archives every reserve→release cycle.
- `didRelease` snapshots into history inside the release tx, `SELECT … FOR UPDATE` against race with reserve.
- `safeReturnTo()` carries the admin back to their filtered view (e.g. `/dids?status=reserved`) after reserve/release/retire. Validated against open-redirect (must start with `/`, not `//`).
- Per-DID "Reservation history" section on `/dids/{id}/cdrs`.

### Phase 11 — Live calls + Hangup (live-page Phase 1)
- `internal/livecalls` package backed by Redis (`live:active` ZSET + `live:meta:*` hash). Atomic Register/Deregister via TxPipeline.
- `/live` page: server-rendered table + hidden `<tr id="live-empty-row">` toggled inside; client updates via SSE at `/live/stream` (1 s snapshots + 15 s heartbeats).
- Inbound-channel lookup matches by dialed DID (Asterisk's `pjsip show channels` doesn't expose Call-ID).
- `POST /live/{call_id}/hangup` → `asterisk -rx "channel request hangup <chan>"`; "channel not found" is a soft success (records intent in compliance log + Redis pending stash).
- Every action audit-logged structured + `writeComplianceEvent` to `user_block_log`.

### Phase 12 — Warn + Redirect (live-page Phase 2)
- `asterisk/extensions.conf` `[admin-actions]` context: `warn` answers + plays `${ADMIN_PROMPT}` + hangs up; `redirect` Dials `${ADMIN_REDIRECT}` (built by `buildRedirectDialString` to mirror `/sipctl/authorize` route conventions).
- `POST /live/{call_id}/warn` sets `ADMIN_PROMPT` chanvar + transfers to `warn@admin-actions`. Prompt validated `[a-zA-Z0-9_./-]{1,64}` no `..`.
- `POST /live/{call_id}/redirect` sets `ADMIN_REDIRECT` chanvar + transfers to `redirect@admin-actions`.
- Soft-success on channel-not-found; pending-action Redis stash means a CDR landing shortly after still gets the admin label.
- Live page rows: tight icon-led `Warn` / `Redirect` / `Hangup` buttons with parameter modals.

### Phase 13 — Compliance + admin-action visibility (0013 + 0014)
- **0013** `order_compliance` — order-scoped compliance log helpers; aggregate balance SQL excludes blocked users; KYC bundle attached to orders is fully editable.
- `/users/{id}` and `/orders/{id}` gained tabbed compliance log views surfacing `user_block_log` rows attributable to the user/order, with admin email + reason.
- Archive tab on `/users` shows blocked/inactive entries; users are not dropped from history.
- **0014** `cdr_admin_action` — `cdrs.admin_action` (enum `user_block_action`), `admin_action_by` (FK admins), `admin_action_reason` (text), partial index `(admin_action, started_at DESC) WHERE admin_action IS NOT NULL`.
- `/sipctl/cdr` pops `pending:admin_action:<call_id>` from Redis and stamps the CDR row in the same statement via `ON CONFLICT … DO UPDATE COALESCE`.
- `/cdrs` list shows the admin label as a pill + actor on rows where `admin_action IS NOT NULL`.
- SIP trace `buildAdminMarkers` maps `user_block_log` rows for the call_id to the closest SIP arrow by `UnixTime`, drawing horizontal dashed marker lines (`.adm-marker-{hangup|warn|redirect}` colour variants). `BadgeW`/`BadgeX` pre-computed server-side.

### Per-order detail page (`/orders/{id}`)
- Tabs: **Overview · Route · CDRs · KYC · Ledger · Compliance Log**.
- Route tab editable + visualised + carries `return_to` so save lands on the same tab.
- KYC tab focuses on attached bundle + swap selector.
- CDR / Ledger / Compliance log tabs mirror the user-detail equivalents but scoped to this order.

### Phase 15 — Multi-supplier engine, busy-tone deny, DID importer, live-state pills (migration 0015)

Captured in `checkpoints/0005-multi-supplier-and-busy-tone/`. Several converging improvements landed together over 2026-05-15/16:

**Multi-supplier engine.** The hardcoded `[globetelecom]` PJSIP endpoint became `[supplier-trunk]` — one shared endpoint for every supplier. Hardcoded identify + AOR blocks deleted from `pjsip.conf`. didapi now writes `/etc/asterisk/pjsip_suppliers.conf` from `supplier_ip_groups + supplier_ip_group_members` and reloads PJSIP on every IP add / edit / delete, and on startup. New `Handler.regenSupplierIdentifies()` + exported `RegenSupplierIdentifiesStartup()` (called from `cmd/didapi/main.go`). Editing a supplier IP is `DELETE old + INSERT new` inside one transaction (`ipMemberEdit` handler + per-IP "Edit" button in `supplier_detail.html`).

**Supplier hostnames (migration 0015).** `supplier_ip_group_members.cidr` made nullable; new `hostname text` column with a CHECK enforcing exactly one of `(cidr, hostname)`. The auto-generated identify file emits `match=<hostname>` lines verbatim; PJSIP resolves them at every reload, so a carrier rotating an A record propagates without code edits. `classifyMatch(s)` auto-detects whether bulk-add input is `cidr` or `hostname`. UI table shows a **Match / Kind** column.

**TCP transport.** `[transport-tcp]` added to `asterisk/pjsip.conf` bound `0.0.0.0:5060`. Some carriers (didcomms among them) send INVITEs over TCP; without this the SYN hits "connection refused". Identify blocks match by source IP regardless of transport.

**Uniform busy-tone deny.** `AuthorizeResponse` gained a `HangupCause` field. The AGI forwards it as `AUTH_HANGUP_CAUSE`; the dialplan reject branch reads it as `Hangup(${IF($[${AUTH_HANGUP_CAUSE} > 0]?${AUTH_HANGUP_CAUSE}:21)})`. `causeForReason()` deliberately returns `17` (User busy → SIP 486 Busy Here) for every reason — caller hears a clean busy tone, never the per-second "sorry we could not connect your call" TTS prompt. Internal reason string preserved on the response, in structured logs, and in `denialCDR` rows. New deny reason `kyc_pending` (with its own denialCDR) distinguishes "order exists but waiting on KYC approval" from the misleading `did_not_assigned`.

**KYC bug fix in `orderUpdate`.** Attaching an already-approved bundle to a `kyc_pending` order now flips the order to `active` in the same UPDATE via a CASE expression on `kyc_bundles.status`. Mirrors what `orderCreate` already does at INSERT time; the pre-fix bug stranded orders forever whenever an admin approved the bundle before attaching it.

**Warn action removed.** `[admin-actions] warn` extension deleted from `extensions.conf` (no more `Answer()` / `Playback()` anywhere in the dialplan). `liveWarn` handler + helpers + UI removed. Only Hangup and Redirect remain in `/live`.

**`/live` channel-state visibility.** New `liveRow.State` field, populated by `channelStates(ctx)` (one `asterisk -rx "core show channels concise"` per SSE tick, parsed into a `map[chanName]state`). `callStateLabel(raw)` maps to three buckets: `answered` (Asterisk `Up`), `ringing` (`Ring`/`Dialing`/`Pre-Ring`), `connecting` (`Down`/`Reserved`/`Off-Hook`). New **Status** column on `/live`, refreshed every snapshot — a ringing row flips to answered the moment the destination picks up.

**DID bulk importer (`internal/didsimport`).** New package with prefix-based country auto-detection (~190 ITU dial codes), three input modes (range / bulk paste / CSV upload with lenient header detection), job + SSE registry. Tabbed `/dids/import` page with per-row progress log, five counter cards, animated bar, rolling rate display. `Handler.Imports` field; `/dids/import/start` returns `{import_id, stream_url, total}`, `/dids/import/{id}/stream` is the SSE feed, `/dids/import/{id}/status` is a JSON snapshot fallback, `/dids/import/example.csv` serves the template.

**Small UI polish.** New-order modal in `user_detail.html` got the same peer-dropdown toggle as the edit modal (shared helper `toggleRouteTargetUI`). Orders tab on `/users/{id}` hyperlinks the DID code to `/orders/{id}`.

### Phase 16 — Audio-clip-as-DID-reservation + single-file Debian deploy (migration 0016)

Captured in `checkpoints/0006-audio-files-and-deploy-script/`. Two threads landed together on 2026-05-16:

**Audio clips for DID reservations.** A reserved DID can now route to an admin-uploaded audio clip instead of a SIP target. When the call lands, Asterisk answers, plays the clip once, and hangs up cleanly (cause 16 Normal Clearing). Useful for "out of service", parking, or test-tone DIDs that should never bill or forward.

- Migration `0016_audio_files` adds the `audio_files` table (`id, name, filename, original_filename, size_bytes, duration_ms, format, created_at, created_by`), extends `route_kind` enum with `'audio'`, adds `dids.reserved_audio_file_id BIGINT REFERENCES audio_files(id) ON DELETE RESTRICT`. Filename CHECK is `[a-zA-Z0-9_-]+`; name UNIQUE; filename UNIQUE.
- New package `internal/audio/audio.go` — always canonicalises to **8 kHz mono 16-bit signed-linear** (`.slin`). Prefers ffmpeg (`-ar 8000 -ac 1 -f s16le -acodec pcm_s16le`), falls back to sox. 60s convert timeout, 50 MB upload cap, broad input format support (mp3 / wav / m4a / aac / ogg / opus / flac / webm / slin / sln / ulaw / alaw / gsm). Storage at `/var/lib/asterisk/sounds/didstorage/<basename>.slin`. Helpers: `EnsureDir`, `Convert`, `Delete`, `Open`, `wavHeaderPCM16` (44-byte RIFF header so the admin browser can play raw slin via `<audio>`).
- New admin page `/audio-files` (template `audio_files.html`, handlers in `audio_handlers.go`): upload + rename + delete modals, inline HTML5 `<audio>` preview per row, in-use chip counts DIDs that reference each clip. Routes: `GET/POST /audio-files`, `POST /audio-files/{id}/{rename,delete}`, `GET /audio-files/{id}/play`, `GET /audio-files/options.json`. Nav link `Audio library` under `Inventory`.
- Reserve-DID modal in `templates/dids.html` gained a fourth route-kind option `audio (play clip + hang up)`; selecting it hides `route_target` and reveals a clip dropdown lazy-loaded from `/audio-files/options.json` (cached per page). `dids()` handler LEFT-JOINs `audio_files` and exposes `ReservedAudioName` so the row shows the friendly clip name instead of the on-disk basename. `siptarget` template helper pretty-prints `kind='audio'` by stripping the `didstorage/` prefix.
- `didReserve` handler branches on `route_kind`: for `audio` it looks up the `audio_files` row, stores `reserved_route_target = "didstorage/<basename>"` AND `reserved_audio_file_id = <id>` in one UPDATE. For other kinds the existing `NormalizeRouteTarget` flow runs with `reserved_audio_file_id` NULL.
- `asterisk/extensions.conf` [from-supplier] gained a new dispatch branch `GotoIf($["${AUTH_ROUTE_KIND}"="audio"]?dial-audio)` and a `(dial-audio)` label that does `Answer() → Wait(1) → Playback(${AUTH_ROUTE_TARGET}) → Hangup()`. The `Wait(1)` gives the carrier time to complete RTP setup before audio writes start. sipctl needs no code change — `reserved_route_kind` and `reserved_route_target` already flow from `dids` row → AGI → dialplan; the reserved-DID branch in `sipctl.decide()` got a documenting comment only.
- Operational requirement: `/var/lib/asterisk/sounds/didstorage/` must be `didstorage:asterisk` mode `2775` (setgid + group write) so didapi can write and Asterisk can read; ffmpeg must be on PATH. Both are handled by `scripts/deploy.sh`.

**Single-file Debian deploy script (`scripts/deploy.sh`).** Replaces the multi-command runbook recipes with one bash entry-point: `./scripts/deploy.sh root@HOST [flags]`. Nine numbered stages with stage-aware error trap, colour output, dry-run mode, distinct exit codes:

1. **Preconditions** — local tools (`go`, `ssh`, `scp`), SSH key present, target reachable, remote OS detected.
2. **Resolve PUBLIC_IP** — env override, then `/etc/didstorage/didapi.env`, then existing `pjsip.conf`, then SSH host if IPv4.
3. **Build** — `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/didapi`.
4. **Cold-boot prep (idempotent)** — `mkdir -p` + `chown` + `chmod` every conventional dir: bin / migrations / scripts / etc/didstorage / var/lib/didstorage/{sip-traces,kyc} / **sounds/didstorage (2775)** / pjsip_suppliers.conf / etc/asterisk group-write.
5. **Ship** — upload migrations + binary + asterisk configs + AGI scripts to `/tmp/didstorage-deploy/`.
6. **Migrations** — ensures `_migrations_log` meta-table exists; **auto-baselines** an existing server (if log empty AND `users` table present, marks every local migration as applied without running); applies each `migrations/*.up.sql` whose filename isn't in the log via `psql -1 -v ON_ERROR_STOP=1 -f`; INSERTs into the log on success.
7. **Asterisk** — `sed`-substitutes `@PUBLIC_IP@`, backs up live config with `*.backup-YYYYMMDD-HHMMSS`, atomic `mv`, copies AGI scripts, `asterisk -rx "core reload"`.
8. **Binary swap** — `cp -a` current binary to `didapi.previous-<ts>`, atomic `mv` of `didapi.new`, `systemctl restart didapi`, waits 2s, verifies `systemctl is-active`. On failure tails `journalctl -u didapi -n 30` to stderr.
9. **Verify** — HTTP 200 on `/login`, pjsip transport / identify / supplier-line counts, audio sounds dir mode, ffmpeg presence, `_migrations_log` count.

Flags: `--dry-run --skip-build --skip-asterisk --skip-migrations --skip-verify --baseline`. Env overrides: `SSH_KEY PUBLIC_IP BUILD_OUT REMOTE_STAGE`. Exit codes: `2 args / 3 preconditions / 4 build / 5 transfer / 6 remote setup / 7 verify`. On error the trap prints **stage number, stage label, line, the bash command that failed, exit code, elapsed time** in a red banner.

Validated end-to-end against the live server: 9 stages green in 98 seconds.

---

### Phase 14 — Live-action channel-name capture + redirect-stays-live (no migration)

A correctness fix for `/live` admin actions, captured in `checkpoints/0004-live-channel-fix/`. Two bugs collapsed into the same root cause.

- **Bug A — redirected calls disappeared from `/live`.** `liveRedirect` was calling `livecalls.Deregister(...)` on success; a redirected call is still alive on a new outbound leg, so the row should stay.
- **Bug B — hangup / warn after redirect targeted the wrong call (or nothing).** `findInboundLegChannel` matched by Asterisk's `Exten` field; when a channel is transferred into `[admin-actions]` its Exten changes from the dialled DID to `redirect`/`warn`. Same-DID + transfer → grep returns the other call (wrong target). Single-call + transfer → grep returns zero → soft-success path Deregisters the row without hanging up.

**Fix:** capture Asterisk's channel name (e.g. `PJSIP/globetelecom-00000063`) at `/sipctl/authorize` time and store it in `live:meta:<call_id>.asterisk_channel`. `liveHangup` / `liveWarn` / `liveRedirect` now use a `resolveChannel(ac)` helper that prefers the stored name (O(1), survives transfers, no Exten dependence). The Exten-grep is a fallback only for pre-deploy in-flight calls.

**Plus:** `liveRedirect` success path no longer Deregisters. It calls a new `livecalls.UpdateRoute(...)` that swaps `route_kind`/`route_target` in the meta and stamps `last_admin_action="redirect"`. `/sipctl/cdr` Deregisters when the call actually ends. The live page renders a `↪ redirect` pill in the DID cell when `last_admin_action` is set (server template + SSE `rowHTML` JS).

**Plumbing:** dialplan `[from-supplier]` AGI now passes `${CHANNEL}` as the 5th arg; `dids-authorize.py` accepts it and forwards as `asterisk_channel`; `sipctl.AuthorizeRequest.AsteriskChannel` carries it to both `Register(...)` call sites.

---

## 4. Architecture map

### Source layout

```
DIDStorage/
├── cmd/
│   ├── didapi/main.go    # the single binary; HTTP + HTTPS + sipctl + reseller API + admin GUI
│   └── didbill/main.go   # cron-driven anniversary billing run
├── deploy/central/       # Debian 12 install scripts + systemd units + fail2ban
├── scripts/              # ops tooling
│   ├── deploy.sh                # canonical single-file Debian deploy (build + ship + migrate + reload + verify)
│   ├── sshpw/                   # one-shot bcrypt + admin-row seeder Go binary
│   └── seed_trace_scenarios.sql # fixture DB used for trace-rendering sanity checks
├── migrations/           # 0001..0016 paired up.sql / down.sql files
├── checkpoints/          # numbered self-contained restore points
│   ├── 0001-initial-migration-0012/             # earliest snapshot (migration 0012)
│   ├── 0002-pre-shadcn/                         # pre-shadcn-experiment (migration 0012)
│   ├── 0003-pre-phase3/                         # post-Phase-2.5, pre-Phase-3 (migration 0014)
│   ├── 0004-live-channel-fix/                   # live channel-name capture (migration 0014)
│   ├── 0005-multi-supplier-and-busy-tone/       # multi-supplier engine + busy-tone deny + DID importer (migration 0015)
│   ├── 0006-audio-files-and-deploy-script/      # audio-clip reservations + scripts/deploy.sh (migration 0016)
│   └── 0007-pre-shape-redesign/                 # current — polished pre-redesign baseline (migration 0016)
└── internal/
    ├── auth/             # admin password + scs session helpers
    ├── billing/          # Run() — channel-ladder anniversary billing
    ├── causes/           # in-memory hangup-cause map; Reload after each Upsert
    ├── config/           # env loader (Postgres, Redis, public IP, listen addr)
    ├── db/               # pgxpool wrapper
    ├── domain/           # pure helpers — SanitizeCallID, CleanCallerURI, FlagEmoji,
    │                     #   ChargeForCall, SupplierChargeForCall, NextAnniversary,
    │                     #   NormalizeRouteTarget, MaxSecondsForBalance, CallState
    ├── kyc/              # bundle dir / sanitized filename / ApproveBundle / RejectBundle
    ├── livecalls/        # Redis-backed live-call registry (Register/Get/List/Deregister
    │                     #   + RecordPendingAction / PopPendingAction)
    ├── resellerapi/      # /api/v1/* — bearer-token authenticated REST surface
    ├── settings/         # DB-backed key-value cache
    ├── sipctl/           # /sipctl/authorize + /sipctl/cdr — Asterisk control plane
    ├── siptrace/         # pcap → tshark → Trace JSON (in-memory + JSONB persist)
    ├── sslmgr/           # tls.Config.GetCertificate from site_domains (manual cert paste)
    ├── didsimport/       # DID bulk-import engine (prefixes, parsers, job, worker)
    ├── audio/            # audio_files storage + ffmpeg/sox conversion to .slin
    └── web/              # admin GUI (chi router + html/template + embed.FS)
        ├── icons.go            # inline SVG library
        ├── pagination.go       # shared list-page helpers
        ├── trace_diagram.go    # BuildSequenceDiagram + EndResultLabel + buildAdminMarkers
        ├── live_handlers.go    # /live + Hangup/Redirect + channelStates + compliance writeback
        ├── settings_handlers.go # /settings + manual cert paste
        ├── dids_import.go      # DID import HTTP handlers (start/stream/status/example)
        ├── audio_handlers.go   # /audio-files CRUD + WAV-wrapped preview + dropdown JSON
        ├── crud.go             # CRUD + regenPJSIPUsers + regenSupplierIdentifies
        └── templates/          # embed.FS — layout + per-page HTML
```

### Key invariants

- **Sanitization vs. raw.** Admin sees raw call_ids, supplier IPs, raw SIP trace; reseller responses go through `CleanCallerURI` + `siptrace.Sanitize` rewrites (supplier IP → DID, our IP → brand_name).
- **Snapshot-at-time-of-call.** Every CDR carries the rate, route, supplier-charge, and billing-increment it actually used. Editing rate cards or routes never disturbs historical billing or `/cdrs/.../sip-trace`.
- **In-memory caches.** `causes`, `settings`, `sslmgr` each keep a `sync.RWMutex`-protected map and reload after every `Upsert`/`Set`/`Reload`. siptrace keeps an in-memory LRU AND persists JSONB on `cdrs`.
- **One-cert-per-SNI.** `sslmgr` matches `hello.ServerName` → exact host → wildcard host → default. No external proxy needed.
- **Idempotent CDR insert.** `/sipctl/cdr` uses `ON CONFLICT (call_id) DO UPDATE` so re-fires from Asterisk's `h`-extension don't duplicate; admin_action fields are merged with `COALESCE`.
- **Atomic live-action registration.** `livecalls.Register` and `Deregister` go through a single `TxPipeline` so an INVITE never half-lives in Redis.
- **Atomic admin-action stamping.** `/live` actions write to Redis `pending:admin_action:<call_id>` BEFORE the action lands on Asterisk; `/sipctl/cdr` pops it inside the CDR insert tx.

---

## 5. Page / endpoint inventory

### Admin GUI

| Route | Purpose |
|---|---|
| `/` (dashboard) | Stats cards, recent CDRs |
| `/login` `/logout` | scs session |
| `/search?q=…` | Cross-table search (users / DIDs / orders / call_ids) |
| `/suppliers` `/suppliers/{id}` | Tabbed: IP whitelisting · Rate cards · DIDs |
| `/dids` `/dids/import` `/dids/{id}/cdrs` | Inventory + reserve/release + per-DID CDRs + reservation history |
| `/users` `/users/{id}` | Tabbed: Overview / Orders / Peers / CDRs / KYC / Ledger / Compliance Log + Archive view |
| `/orders` `/orders/{id}` | Cross-user list + per-order tabbed: Overview / Route / CDRs / KYC / Ledger / Compliance Log |
| `/resellers` `/resellers/{id}` | List + tabbed detail with API key management; aggregate balance excludes blocked users |
| `/cdrs` `/cdrs/{call_id}/sip-trace` | Filterable list + tabbed trace (Overview / Timeline / Raw) with admin-action markers |
| `/cdrs/export.csv` `/users/{id}/export/*.csv` `/orders/{id}/export/cdrs.csv` | Compliance CSV |
| `/live` `/live/stream` | Active calls (SSE-driven); `POST /live/{call_id}/{hangup,warn,redirect}` |
| `/denied-calls` | Security view (unauthorized_ip + unknown_did + customer denials) |
| `/kyc-bundles/{bid}` | Bundle detail + doc download + approve/reject |
| `/cause-codes` | Hangup-cause editor |
| `/settings` | Company / Site / Admins / Domains & SSL (manual cert + key paste) |

### Reseller REST API (`/api/v1`)

| Method · path | Notes |
|---|---|
| `GET /users` `POST /users` `GET /users/{id}` `GET /users/by-external/{ext}` `POST /users/{id}/topup` | customer CRUD + ledger top-up |
| `GET /orders` `POST /orders` `GET /orders/{id}` `POST /orders/{id}/cancel` | per-DID rental CRUD |
| `GET /dids` | inventory (own assigned + globally available) |
| `GET /cdrs` `GET /cdrs/{call_id}/sip-trace` | sanitized SIP traces |
| `POST /users/{id}/kyc-bundles` `GET /users/{id}/kyc-bundles` `GET /kyc-bundles/{bid}` `POST /kyc-bundles/{bid}/documents` `GET /kyc-bundles/{bid}/documents/{did}/download` | KYC submission flow |
| `GET /users/{id}/export/cdrs.csv` `GET /users/{id}/export/ledger.csv` `GET /orders/{id}/export/cdrs.csv` | compliance CSV |

### Asterisk control plane (`/sipctl`)

| `POST /authorize` | called on every INVITE; returns allow/deny + route + max-seconds + reservation_id |
| `POST /cdr` | called on dialog teardown; persists CDR + ledger debit + siptrace precompute kick + pops pending admin-action |

---

## 6. Rules / conventions baked into this version

1. **G.711 + g722 are the only codecs offered.** No opus, SRTP, DTLS, AVPF, WebRTC. `[globetelecom]` and `[outbound]` allow ulaw / alaw / g722; `regenPJSIPUsers` writes `allow=ulaw,alaw,opus,g722` for user-account endpoints.
2. **No ACME, no auto-provision.** HTTPS certs are pasted PEM on `/settings#domains`. The `:443` listener only comes up when `site.https_listen_addr` is set AND at least one row in `site_domains` has a usable cert.
3. **No WebRTC ChanSpy listen.** The `[transport-wss]`, `[admin-spy*]`, `[admin-listen]` Asterisk constructs do not exist. No JsSIP in `/live`. Listen-only audio monitoring is not implemented in this checkpoint.
4. **`@PUBLIC_IP@` placeholder in `asterisk/pjsip.conf`.** Substituted to the real public IP at deploy time by `deploy/central/03-deploy-asterisk.sh`. The local repo file always has the placeholder; the live `/etc/asterisk/pjsip.conf` always has the substituted IP. Forgetting the substitution makes every outbound INVITE go out with `c=IN IP4 (null)` and downstream parsers reject with 400.
5. **Inbound-only.** `[from-account]` rejects any INVITE from a registered SIP account (cause 31). DIDStorage terminates inbound, never originates.
6. **SIP trace correlator uses ONE Call-ID per CDR.** `siptrace.Lookup` filters pcaps by the inbound supplier-side Call-ID. The outbound-leg's separate Call-ID is NOT merged into the trace.
7. **Snapshot-at-call-time pricing.** `cdrs.rate_cents_per_min`, `supplier_charge_cents`, `supplier_bill_min_seconds`, `supplier_bill_increment_seconds`, `routed_kind`, `routed_target` are written at `/sipctl/cdr` time and never updated. Rate-card or route edits do not retroactively re-bill.
8. **Atomic admin-action stamping.** `/live` actions call `livecalls.RecordPendingAction` into Redis BEFORE returning; `/sipctl/cdr` pops it inside the CDR insert tx.
9. **Em-dashes allowed.** No copy-style restriction in this codebase.
10. **Dark Discord theme.** Admin GUI uses `--bg #313338`, `--card #2b2d31`, `--accent #5865f2` (blurple). No light variant in the active template tree.
11. **CDR schema is forward-additive.** Existing columns are never repurposed. Adding a column is the standard pattern.
12. **No tests.** Zero `_test.go` files. Smoke tests are manual (`curl` checks + a real test call).
13. **Single admin row.** `admins` table holds one row; password is bcrypted in `password_hash`; email is hardcoded in `auth.go`. No RBAC.
14. **Live-action channel resolution uses the stored channel name.** The Asterisk channel name (e.g. `PJSIP/supplier-trunk-00000063`) is captured at `/sipctl/authorize` time from the dialplan's `${CHANNEL}` and persisted in `live:meta:<call_id>.asterisk_channel`. `liveHangup` / `liveRedirect` use `resolveChannel(ac)` which reads it directly — they do NOT grep `pjsip show channels` by Exten. The Exten-grep is a fallback only for pre-deploy in-flight calls.
15. **Redirect keeps the call in the live index.** `liveRedirect` success calls `livecalls.UpdateRoute(...)` instead of `Deregister`. Only `/sipctl/cdr` Deregisters mid-life. The "Routed to" column reflects the new destination; the DID cell shows a `↪ redirect` pill while `last_admin_action` is set.
16. **Checkpoint naming convention.** Restore points live under `checkpoints/NNNN-<slug>/`, 4-digit zero-padded prefix + descriptive slug, sequential regardless of whether a migration was added. Mirrors the migration numbering scheme. New checkpoint = next number.
17. **One shared `[supplier-trunk]` PJSIP endpoint** with per-supplier identify blocks. Per-supplier identifies are auto-generated into `/etc/asterisk/pjsip_suppliers.conf` from `supplier_ip_groups + supplier_ip_group_members` on every IP mutation, and on startup. Section name keyed `supplier-<id>-id` so renaming a supplier doesn't break Asterisk references.
18. **Supplier IP whitelist may carry hostnames alongside IPs.** PJSIP resolves the hostnames at every reload; `/sipctl/authorize` matches by source IP against the cidr rows. CHECK constraint enforces exactly one of `(cidr, hostname)` per row.
19. **Editing a supplier IP is delete-then-add in one transaction.** No in-place UPDATE; rows leave and enter the table.
20. **All authorize denials hang up with Q.850 cause 17 → SIP 486 Busy Here.** Caller hears a busy tone, never a recorded prompt. Internal reason string preserved on the response and in `denialCDR`.
21. **Order auto-promotes to active when an approved bundle is attached via `orderUpdate`.** The UPDATE statement carries a CASE that flips status atomically — mirrors `orderCreate`.
22. **`/live` shows live channel state per call.** One `core show channels concise` per SSE tick; per-row state derived from `ac.AsteriskChannel`. State refreshes every 1 s on existing rows.
23. **DID imports go through `internal/didsimport`.** Range / bulk-paste / CSV upload all feed the same Job + SSE feed; counters and per-row events are streamed; jobs reaped 10 min after termination.
24. **No `warn` action in `/live`.** Only Hangup and Redirect.
25. **TCP + UDP on port 5060.** Asterisk listens on both. Identify blocks match by source IP regardless of transport.
26. **Audio clips canonicalise to `.slin` (16-bit signed-linear, 8 kHz, mono).** Never store other formats. Conversion goes through `internal/audio.Convert` which prefers ffmpeg and falls back to sox. Inputs accepted: mp3 / wav / m4a / aac / ogg / opus / flac / webm / slin / sln / ulaw / alaw / gsm. Max 50 MB upload, 60 s convert timeout.
27. **Audio sounds dir is `didstorage:asterisk 2775`** (setgid + group write). Files inside inherit group asterisk via setgid; `audio.Convert` chmod 0644s every file so Asterisk's `Playback()` can read it.
28. **`audio_files` row delete is blocked twice over** — by the `dids.reserved_audio_file_id` FK (ON DELETE RESTRICT) and by an explicit handler check that returns "release the reservation first" on the flash. Renames change only the display name; the on-disk basename is immutable so reserved DIDs never break.
29. **`scripts/deploy.sh` is the canonical deploy path.** Migrations are tracked in `_migrations_log (filename PRIMARY KEY, applied_at, applied_by)`; the script auto-baselines existing servers on first run. The previous binary is preserved as `/opt/didstorage/bin/didapi.previous-<ts>` on every deploy. Use `--dry-run` to preview, `--baseline` to force-mark all migrations as applied, `--skip-{build,asterisk,migrations,verify}` for incremental runs.

---

## 7. How to deploy

**The canonical reference is `DEPLOY.md` at the project root.** Read it
end-to-end the first time you touch a server. The summary:

There are two scripts. Both run from your dev machine over SSH; neither
requires anything pre-installed on the server beyond root SSH access on
fresh Debian 12.

| Script | When | Effect |
|---|---|---|
| `scripts/bootstrap.sh` | **Once per server.** New box, or after a `WIPE`. | Destructive. Wipes any prior install, then installs Postgres+Redis+Asterisk+ffmpeg+ufw, creates the `didstorage` user, generates `/etc/didstorage/didapi.env` with fresh secrets (DB password, SIPCTL token, session secret), installs systemd units, lays down base Asterisk config with `@PUBLIC_IP@` substituted, opens the firewall, then hands off to `deploy.sh` for first build + migration. Requires `PUBLIC_IP=<v4>` env var. Prompts for "WIPE" confirmation. |
| `scripts/deploy.sh` | **Every change.** Daily driver. | Idempotent. Cross-compiles `cmd/didapi` for `linux/amd64`, ships it + new migration files + asterisk configs, applies any migration not in `_migrations_log` inside a single transaction each, atomic `mv` swap of the binary, restarts `didapi.service`, verifies `GET /login → 200` + Asterisk health. Previous binary preserved as `/opt/didstorage/bin/didapi.previous-<utc-ts>` for manual rollback. |

Fresh install:

```bash
PUBLIC_IP=203.0.113.10 scripts/bootstrap.sh root@new.example.com
```

Ongoing deploys (after schema migration, snapshot the DB first):

```bash
ssh -i ~/.ssh/didstorage_ed25519 root@HOST '
  TS=$(date -u +%Y%m%d-%H%M%S)
  sudo -u postgres pg_dump didstorage --format=custom \
    --file=/root/backups/pre-deploy-$TS.dump'

scripts/deploy.sh root@HOST
```

`deploy.sh` flags:

| Flag | Effect |
|---|---|
| `--dry-run` | print every remote / scp command instead of executing |
| `--skip-build` | reuse `/tmp/didapi-linux-amd64` (set `BUILD_OUT` to override) |
| `--skip-asterisk` | skip pjsip / extensions / AGI deploy + reload |
| `--skip-migrations` | skip the migration pass |
| `--skip-verify` | skip the post-deploy smoke tests |
| `--baseline` | mark every local migration as applied without running it (use once on a server pre-dating `_migrations_log`) |

Env overrides: `SSH_KEY` (default `~/.ssh/didstorage_ed25519`),
`PUBLIC_IP` (auto-discovered from `/etc/didstorage/didapi.env` after
bootstrap; required from env for bootstrap), `BUILD_OUT` (default
`/tmp/didapi-linux-amd64`), `REMOTE_STAGE` (default
`/tmp/didstorage-deploy`). bootstrap also accepts `DB_PASSWORD`,
`SIPCTL_TOKEN`, `SESSION_SECRET`, `ADMIN_EMAIL`, `ADMIN_PASSWORD` — leave
unset to auto-generate (printed once at the end).

Distinct exit codes (`2 args / 3 preconditions / 4 build / 5 transfer /
6 remote setup / 7 verify / 8 user declined wipe`) plus a `trap on_error
ERR` that prints the failing stage label, line, command, exit code, and
elapsed time in a red banner.

A successful bootstrap or deploy ends with a green `BOOTSTRAP OK` or
`DEPLOY OK — Ns · root@HOST` banner.

**Switching servers.** See `DEPLOY.md` §"Switching to a new server". The
architecture has no per-server state baked into the code; `pg_dump` from
old + `bootstrap.sh` on new + optional `pg_restore` is the whole flow.

---

## 8. Restore points / escape hatches

All under `checkpoints/NNNN-<slug>/`. Each is self-contained with `source/`, `progress.md`, `instruct.md`, and (from 0003 onward) `db/`. Naming mirrors the migration convention: 4-digit zero-padded prefix + descriptive slug; next checkpoint gets the next number, regardless of whether the DB schema changed.

| Directory | Migration | Date | Notes |
|---|---|---|---|
| `checkpoints/0001-initial-migration-0012/` | 0012 | 2026-05-11 | Earliest captured restore point. Pre-live work (no `/live`, no compliance log, no admin_action). |
| `checkpoints/0002-pre-shadcn/` | 0012 | 2026-05-11 | Snapshot right before a shadcn / TweakCN theming experiment that was later reverted. Same code shape as 0001. |
| `checkpoints/0003-pre-phase3/` | 0014 | 2026-05-12 | Post-Phase-2.5 (compliance log + admin-action stamping + order detail tabs + SIP trace markers). Pre-Phase-3 (no WebRTC, no ACME, no outbound-Call-ID trace merging). First snapshot with a bundled DB dump. |
| `checkpoints/0004-live-channel-fix/` | 0014 | 2026-05-12 | Post-Phase-2.5 + live-action channel-name capture fix. DB schema identical to 0003; delta is Go + AGI + dialplan + template only. |
| `checkpoints/0005-multi-supplier-and-busy-tone/` | 0015 | 2026-05-16 | Multi-supplier engine (auto-generated `pjsip_suppliers.conf`, shared `[supplier-trunk]` endpoint, hostname matching, edit-as-delete-then-add IPs), TCP transport, uniform busy-tone deny (Q.850 cause 17 → SIP 486 across all reasons), KYC promotion fix in `orderUpdate`, DID bulk importer with SSE progress, `/live` Status column. |
| `checkpoints/0006-audio-files-and-deploy-script/` | 0016 | 2026-05-16 | Audio-clip-as-DID-reservation feature (`audio_files` library, `route_kind='audio'`, `dids.reserved_audio_file_id` FK, `internal/audio` ffmpeg/sox conversion to `.slin`, `/audio-files` admin page with inline preview player, dial-audio dialplan branch). Single-file Debian deploy at `scripts/deploy.sh` with 9 numbered stages, `_migrations_log` tracking, auto-baseline, dry-run, distinct exit codes, error-trap banners. |
| `checkpoints/0007-pre-shape-redesign/` | 0016 | 2026-05-16 | **Current.** Polished pre-redesign baseline. Same DB schema as 0006; source tree carries cumulative impeccable polish rounds (typeset / animate / bolder / harden / clarify / polish / delight / adapt / optimize). `.num` mono+tabular utility rolled across every list page. Motion tokens + `prefers-reduced-motion` + focus-visible parity. Keyboard shortcuts (`g d / g u / g l / g s / g c / g r / g a / g o / g h`, `/`, `?`, `Esc`). Copy-on-click for `<code>`. State-pill pulse on `/live`. Modal enter/exit via `@starting-style`. Snapshot exists to provide a fast rollback target before a Linear-class redesign of `layout.html` + `cdrs.html` lands. |

**Rolled-back work** (built, deployed, then removed on 2026-05-12; available on disk if a redo is ever wanted):

| Removed | Replacement / where it lives |
|---|---|
| `migrations/0015_listen_action` | enum value `live_listen` left in PG (can't drop) |
| `migrations/0016_domain_auto_provision` | dropped from DB |
| `migrations/0017_acme_settings` | settings rows DELETEd |
| `migrations/0018_outbound_call_id` | dropped from DB |
| `internal/acme/` Go package | removed |
| WebRTC ChanSpy listen (`[transport-wss]` + `[admin-spy*]` + `[admin-listen]`, `liveListen`, JsSIP modal) | removed |
| `[outbound-pre-dial]` + `b(...)` Dial options | removed |
| `siptrace.LookupMulti` + multi-Call-ID trace merging | removed |
| Light-theme `/users` redesign | reverted to dark Discord theme |
| Pre-revert source tree | `C:\Users\Apex\Documents\DIDStorage-snapshots\pre-revert-20260511-230503\` |
| Pre-revert server bundle (binary + asterisk configs + scripts + DB schema) | `/opt/didstorage/pre-revert-20260511-230442/` on `45.8.93.244` |
| Per-file timestamped server backups from today's deploys | `*.backup-20260511-*` alongside binary + asterisk configs |

---

## 9. Configuration sources (server)

- `/etc/didstorage/didapi.env` — secrets: `DATABASE_URL`, `REDIS_URL`, `ADMIN_PASSWORD`, `SIPCTL_AUTH_TOKEN`. Outside any backup.
- `/etc/didstorage/auth_token` — root-only fallback the AGI scripts read for the sipctl shared secret.
- `settings` table — runtime-editable: `company.*`, `site.*`. `site.https_listen_addr` defaults to `""` (skip HTTPS unless explicitly set).
- `/etc/asterisk/pjsip.conf` — public IP substituted at deploy; auto-included `pjsip_users.conf` is hot-regenerated by `didapi.regenPJSIPUsers`.
- `/etc/asterisk/extensions.conf` — dialplan; reload with `asterisk -rx "dialplan reload"` or `core reload`.

End of progress.
