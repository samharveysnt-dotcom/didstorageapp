# Product

## Register

product

## Users

A single operator (the platform owner) runs the admin GUI. There is no shared admin pool, no reseller-facing GUI in this codebase, and no public marketing surface here. The admin GUI is for one technically expert person who lives in Postgres, Asterisk's CLI, and the Go source. It is the same person who would otherwise be running `psql` and `asterisk -rx` directly.

Daily work clusters around three loops, in roughly this priority:

1. **Provisioning customers and orders.** Create users, attach DIDs, top up balances, walk KYC bundles through approval, edit routes, swap a DID's audio reservation.
2. **Configuring suppliers, rates, and DIDs.** Add suppliers, manage IP and hostname whitelists, import DIDs in bulk, reserve DIDs (SIP target or audio clip), maintain rate cards, watch supplier identifies reload after a change.
3. **Reviewing compliance and billing.** Audit `user_block_log` and admin-action stamps on CDRs, export CDRs / ledger / blocks as CSV, reconcile balance ledger entries, approve KYC documents.

Live-call firefighting (the `/live` page, admin Hangup / Redirect, SIP traces) is secondary. Useful when needed but not the dominant loop. The chrome should optimise for the three primary workflows first; `/live`'s presence must not push the dominant rhythm around.

### Resellers and the API model

Resellers are **frontend-only distributors**, not platform operators. They have no server-side infrastructure of their own, no billing engine, no database, no admin backend. What they have is a branded UI (web app, mobile app, or similar) that calls the DIDStorage JSON API directly on behalf of their end-customers.

End users authenticate against the reseller's UI; the UI calls `/api/v1/*` to provision, list, top up, query CDRs, manage SIP peers, attach KYC bundles, and so on. Every state mutation and every cent of money lives in the DIDStorage backend; the reseller side never reconciles, never marks up, never stores a CDR locally.

Practical consequences for the design:

- **Poll-driven, not push-driven.** End users see live data because their UI polls our API when they open a page. The platform never POSTs out to a reseller server (no webhooks, no callbacks); there is no server to receive them.
- **All endpoints are read-with-action.** GETs return what the end-user is looking at. POSTs/PATCHes mutate platform state on their behalf. Nothing requires reseller-side persistence to make sense.
- **Sanitisation must be aggressive.** The reseller UI is consumer-facing; supplier IPs, hostnames, internal admin metadata, and other resellers' data MUST NEVER appear in any API response, even by mistake. The `/sipctl/*` control plane (Asterisk-only, shared-token authenticated) is a strict sibling, never co-mingled with reseller-visible routes.
- **No billing-style features needed.** Per-CDR reconciliation hooks, cost-mark-up tooling, multi-tier balance ledgers, push-notification fan-out: none of these apply. The platform is the system of record.

Anyone proposing a feature that assumes a reseller-side backend (webhooks, fire-and-forget callbacks, reseller-side cron jobs, reseller-managed signing keys with no UI to set them) is working from the wrong model and should re-read this section first.

## Product Purpose

DIDStorage is a multi-tenant inbound DID hosting and SIP routing platform running on a single Debian box. A single Go binary serves three roles on the same listener: admin GUI (this surface), reseller REST API, and the Asterisk control plane (`/sipctl/authorize` + `/sipctl/cdr`). The admin GUI is how the operator runs the business: every customer, every supplier, every rate card, every CDR, every compliance event passes through it.

Success looks like:

- A new customer goes from email to first billable call in five minutes without touching SQL.
- A supplier change (new IP, new hostname, new rate) takes effect within one round trip; PJSIP reloads itself; no Asterisk restart needed.
- A compliance question ("what happened on this call at this time?") gets answered in two clicks: search by call-id, open the trace.
- The interface never silently lies. If the platform thinks a DID is reserved, the GUI says reserved. If a balance falls below the minimum, the GUI shows the dollar gap. If an audio clip is in use, the delete button is disabled with the reason on hover.

Real money flows through this surface (balance ledgers, supplier charges, channel-monthly fees, anniversary billing) and real regulatory exposure (KYC bundles, denied-call audit, supplier compliance). The visual system has to read as a tool that can be trusted with both.

## Brand Personality

**Dense. Calm. Trustworthy.**

- *Dense.* Every screen carries more information per square inch than the SaaS default. White space serves rhythm, not breathing room. Tables, lists, and inline detail beat cards; cards beat panels; panels beat modals. A list row shows status, identity, state, and actions on a single line; expansion is opt-in. The operator should never scroll past empty pixels to find what they need.
- *Calm.* At-rest UI is silent. No flashing badges, no toast spam, no animated counters. Numbers update in place without bounce. The `/live` page can flip a row from ringing to answered without any motion outside the state pill. Severity uses position and weight before colour.
- *Trustworthy.* The design reads as a tool that knows where the money is. Tabular figures for every cents, every duration, every count, every ID. Hairline borders, not heavy chrome. Action buttons never disguise destructive intent: delete is delete, cancel is cancel, refund is refund. Confirmation language is plain ("Delete X? This cannot be undone."). Errors carry the specific reason and the next step.

Voice: factual, terse, no softeners ("simply", "easily", "powerful"). The operator is the only audience and already knows the domain. Tone shifts only at the edges: error messages get one extra sentence of context; empty states get one helpful pointer; success is silent because the row simply appeared.

## Anti-references

The dominant anti-reference is the **Discord / Slack-clone dark theme** that the current codebase ships with. The current `internal/web/templates/layout.html` cribs its surface tiers, its accent (Discord blurple `#5865f2`), its tooltip floater, and its active-link treatment wholesale from Discord. That look is the explicit anti-reference: it makes a serious telecom-ops tool read as a casual chat app and signals "this was themed quickly" rather than "this was designed for the work".

Specifically, any redesign refuses:

- Discord blurple (`#5865f2`) as the accent hue.
- The Discord surface scale (`#313338` bg, `#2b2d31` card, `#1e1f22` sidebar, `#404249` active) at those exact tints. A different tinted-neutral scale is fine; cribbing this one is not.
- The 3px left-border bar on the active sidebar item (chat-app navigation idiom).
- "Channels-and-DMs" sidebar density (tight icon-and-label rows with hover swap and pill-active state).
- Discord's accent-on-white text inside the active sidebar item; the system's accent is reserved for fills behind white text, not the other way around.

The replacement does not have to be light-themed. It has to feel designed for telecom ops, not borrowed from elsewhere.

## Design Principles

1. **The current look is the anti-reference.** Any redesign moves *away* from the Discord clone toward something the operator can read as built for telecom ops, not chat. Surface palette, accent hue, sidebar treatment, tooltip styling, and active-link treatment all change. Half-measures (keeping the surface scale but swapping the accent) miss the point.

2. **Density is the feature, not a flaw.** One operator lives in this tool for hours at a time. Optimise for information-per-square-inch and muscle-memory layouts. Tables beat cards; sortable headers beat filter pills; inline detail beats modals; expansion is opt-in, not the default. Same shape at 10 DIDs and 10,000.

3. **Calm at rest, choreographed in transit.** The at-rest UI is silent: no spinners, no toasts, no badges competing for attention. Motion appears only when state changes, with a single easing curve (ease-out-expo / quart) and a budget of 240ms. `/live` is the proof: a call flipping from ringing to answered is a quiet truth, not a celebration.

4. **Tabular figures everywhere measurable.** Cents, durations, call counts, channel caps, ledger balances, timestamps, call-ids, E.164 numbers, IP addresses, rate-card columns: all monospace, all tabular figures. Prose stays in the sans-serif. The mono / sans split is the system's voice; mixing them inside one cell is forbidden.

5. **Plain, factual copy.** Buttons say what they do ("Delete user", not "Confirm"). Error messages say the reason and the next step ("an audio file named X already exists; pick a different name"). No emoji, no exclamation marks, no marketing softeners. The operator does not need to be sold to.

## Accessibility & Inclusion

Baseline contrast only. Readable text and UI controls on every surface, no formal WCAG conformance target, no reduced-motion or colour-blind-safe commitments. The single operator is the only audience; design freedom and density take priority over conformance ceremony. Per-surface accommodations (severity tags using shape plus colour, focus rings on form controls) may be added where a specific screen genuinely needs them, but the system does not commit to them globally.
