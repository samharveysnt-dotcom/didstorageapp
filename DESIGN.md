<!-- ANTI-REFERENCE BASELINE.
     This file snapshots the visual system the codebase currently ships
     (Discord-clone dark theme). PRODUCT.md names this whole look as the
     anti-reference for any future design work. Every entry below is to be
     read as "what is" today, NOT as "what should be". When a redesign
     lands, re-run $impeccable document and write the target system over
     this file. Until then, treat the captured tokens as a delta-baseline
     for measuring change, not as a spec to extend. -->
---
name: DIDStorage Admin GUI
description: Anti-reference baseline. Current Discord-clone dark theme captured for delta-tracking against the future redesign.
colors:
  discord-bg:              "#313338"
  discord-card:            "#2b2d31"
  discord-sidebar:         "#1e1f22"
  discord-border:          "#1e1f22"
  discord-sidebar-active:  "#404249"
  discord-row-hover:       "#383a40"
  discord-tooltip-surface: "#111214"
  discord-text:            "#dbdee1"
  discord-text-muted:      "#949ba4"
  discord-accent-blurple:  "#5865f2"
  discord-accent-text:     "#9aa6ff"
  discord-ok:              "#23a55a"
  discord-warn:            "#f0b232"
  discord-err:             "#da373c"
  discord-error-ink:       "#1e1f22"
typography:
  body:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"
    fontSize:   "14px"
    fontWeight: 400
    lineHeight: 1.5
  headline:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"
    fontSize:   "1.5rem"
    fontWeight: 600
    lineHeight: 1.2
  title:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"
    fontSize:   "1.05rem"
    fontWeight: 500
    lineHeight: 1.3
    letterSpacing: "0.5px"
  body-cell:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"
    fontSize:   "13px"
    fontWeight: 400
    lineHeight: 1.4
  label:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"
    fontSize:   "11px"
    fontWeight: 500
    letterSpacing: "0.5px"
  mono:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace"
    fontSize:   "12px"
    fontWeight: 400
rounded:
  xs: "3px"
  sm: "5px"
  md: "6px"
  lg: "8px"
  xl: "10px"
spacing:
  xs: "0.25rem"
  sm: "0.5rem"
  md: "0.7rem"
  lg: "1rem"
  xl: "1.5rem"
components:
  button-primary:
    backgroundColor: "{colors.discord-accent-blurple}"
    textColor:       "#ffffff"
    rounded:         "{rounded.md}"
    padding:         "0.55rem 1rem"
    typography:      "{typography.body}"
  button-ghost:
    backgroundColor: "transparent"
    textColor:       "{colors.discord-text}"
    rounded:         "{rounded.md}"
    padding:         "0.55rem 1rem"
  button-danger:
    backgroundColor: "{colors.discord-err}"
    textColor:       "#ffffff"
    rounded:         "{rounded.md}"
    padding:         "0.55rem 1rem"
  button-warn:
    backgroundColor: "{colors.discord-warn}"
    textColor:       "{colors.discord-error-ink}"
    rounded:         "{rounded.md}"
    padding:         "0.55rem 1rem"
  button-secondary:
    backgroundColor: "{colors.discord-sidebar-active}"
    textColor:       "{colors.discord-text}"
    rounded:         "{rounded.md}"
    padding:         "0.55rem 1rem"
  pill-active:
    backgroundColor: "rgba(35,165,90,0.18)"
    textColor:       "{colors.discord-ok}"
    rounded:         "{rounded.xl}"
    padding:         "0.1rem 0.5rem"
  pill-warn:
    backgroundColor: "rgba(240,178,50,0.18)"
    textColor:       "{colors.discord-warn}"
    rounded:         "{rounded.xl}"
    padding:         "0.1rem 0.5rem"
  pill-err:
    backgroundColor: "rgba(218,55,60,0.18)"
    textColor:       "{colors.discord-err}"
    rounded:         "{rounded.xl}"
    padding:         "0.1rem 0.5rem"
  pill-muted:
    backgroundColor: "rgba(148,155,164,0.16)"
    textColor:       "{colors.discord-text-muted}"
    rounded:         "{rounded.xl}"
    padding:         "0.1rem 0.5rem"
  card:
    backgroundColor: "{colors.discord-card}"
    textColor:       "{colors.discord-text}"
    rounded:         "{rounded.lg}"
    padding:         "1rem 1.2rem"
  table-row:
    backgroundColor: "{colors.discord-card}"
    textColor:       "{colors.discord-text}"
    height:          "38px"
    padding:         "0.55rem 0.85rem"
  input-text:
    backgroundColor: "{colors.discord-sidebar}"
    textColor:       "{colors.discord-text}"
    rounded:         "{rounded.md}"
    padding:         "0.5rem 0.7rem"
  modal:
    backgroundColor: "{colors.discord-card}"
    textColor:       "{colors.discord-text}"
    rounded:         "{rounded.xl}"
    padding:         "0"
  sidebar:
    backgroundColor: "{colors.discord-sidebar}"
    textColor:       "{colors.discord-text-muted}"
    width:           "240px"
  sidebar-link-active:
    backgroundColor: "{colors.discord-sidebar-active}"
    textColor:       "{colors.discord-accent-text}"
    padding:         "0.55rem 1.5rem"
  tooltip:
    backgroundColor: "{colors.discord-tooltip-surface}"
    textColor:       "{colors.discord-text}"
    rounded:         "{rounded.md}"
    padding:         "0.55rem 0.75rem"
    width:           "280px"
---

# Design System: DIDStorage Admin GUI

> **Anti-reference baseline.** This document captures the visual system the codebase ships **today**. PRODUCT.md lists the whole look as the explicit anti-reference for future work. Read every section below as "what is in production right now, and what the next redesign refuses to keep". When the redesign lands, re-run `$impeccable document` to write the new target system over this file.

## 1. Overview

**Creative North Star: "The Discord Clone (Anti-Reference Baseline)"**

The current admin GUI is a wholesale port of Discord's chat-app chrome onto a telecom-ops tool. Surface scale, accent colour, sidebar treatment, and tooltip floater all come from the Discord visual vocabulary. PRODUCT.md names this as the dominant anti-reference: it makes a serious operator tool read as a casual chat app and signals "themed quickly" rather than "designed for the work". This document is the inventory of what to throw away, not the spec to extend.

The captured palette is dark, Discord-grey-tinted, with a single saturated blurple accent (`#5865f2`) carrying every action button, focus ring, sidebar-active border, and pagination-current chip. Typography is system-sans only (no monospace for measurable values; `<code>` is the only place mono shows up). Elevation is flat at rest with one shared drop-shadow on the sidebar and modal. Density is moderate, defaulting to single-line table rows with ellipsis truncation.

What this captured system silently does and what the redesign must change:

- **Cribs Discord's exact surface scale.** `#313338` bg, `#2b2d31` card, `#1e1f22` sidebar, `#404249` active are Discord's tints. Replace with a tinted-neutral scale that does not echo those values.
- **Uses Discord's blurple accent.** `#5865f2` and its lighter sibling `#9aa6ff` carry every interactive surface. The next system picks a different signal hue, ideally not a saturated tech-app blue.
- **Reproduces Discord's active-nav idiom.** 3px left-border bar in accent colour + tonal background swap on the active sidebar item. Replace with an idiom that does not read as "channel selected in a chat app".
- **Treats `<code>` as the only mono surface.** Every other measurable value (cents, durations, IDs, E.164, IPs) renders in the same sans-serif as prose. The next system commits to mono + tabular-figures for every measurable.
- **Defaults to body weight 400 with a single 1.25× scale step.** Hierarchy is weak. The next system uses heavier weight contrast and a tighter scale relationship.

**Key Characteristics:**

- Dark Discord-grey palette with one saturated blurple accent.
- System-sans-only typography; mono used incidentally in `<code>` blocks only.
- Flat at rest; one shared `0 4px 16px rgba(0,0,0,.5)` drop-shadow on sidebar and modal.
- Pill vocabulary uses 18% tinted backgrounds with the matching saturated colour as ink.
- Banned by upstream design laws but present in current code: `backdrop-filter: blur(2px)` on dialog `::backdrop`.

## 2. Colors

The palette is the Discord dark-theme scale, tinted slightly cool, plus one saturated blurple accent and three status hues (ok / warn / err) used inside tinted pill backgrounds.

### Primary

- **Discord Blurple** (`#5865f2`): the accent. Carries primary button fills, the active sidebar-item left-border bar, the current-page chip in pagination, focus rings on inputs (rgba shadow at 18%), and the `:hover` link colour swap target. **Used heavily across every interactive surface.** PRODUCT.md anti-reference: this exact hue is on the no-fly list.
- **Discord Accent Text** (`#9aa6ff`): the lighter blurple. Carries the brand wordmark in the sidebar, the active sidebar-item text, `.tip:hover`, and the `a.link` colour. Stated AA contrast: 6.4:1 on bg, 5.1:1 on sidebar-active.

### Status (used only inside 18% tinted pill backgrounds)

- **OK Green** (`#23a55a`): active / approved status pills.
- **Warn Yellow** (`#f0b232`): pending / kyc_pending / quarantined / warn pills.
- **Err Red** (`#da373c`): suspended / rejected / blocked pills; `.btn-danger` fill.

### Neutral

- **Discord BG** (`#313338`): main page background. Discord's exact main-chat tint.
- **Discord Card** (`#2b2d31`): cards, sections, table fill, modal surface.
- **Discord Sidebar** (`#1e1f22`): sidebar background, input background, `<code>` fill, border colour. Same value used as `--border`.
- **Discord Sidebar Active** (`#404249`): active-state row background; `.btn-secondary` fill.
- **Discord Row Hover** (`#383a40`): table-row `:hover` background tier.
- **Discord Tooltip Surface** (`#111214`): the floating tooltip's surface; the only surface in the system darker than the sidebar.
- **Discord Text** (`#dbdee1`): body text. Stated contrast: 11.4:1 on bg.
- **Discord Text Muted** (`#949ba4`): secondary text. Stated contrast: 5.45:1 on bg.

### Named Rules (current, all anti-reference)

**The One Discord Rule.** Today, every interactive accent path through the UI lands on `#5865f2` (primary fill) or `#9aa6ff` (link/active text). Concentration is exactly the issue PRODUCT.md objects to: the colour is Discord's wordmark blue, not a colour chosen for the work. Replace with a non-blurple accent in the next pass.

**The Black-Tinted-Toward-Black Rule.** Every surface tier is a Discord grey: bg `#313338` is barely warmer than its card `#2b2d31`, which is barely warmer than its sidebar `#1e1f22`. The scale is internally consistent but recognisable as Discord at a glance. Replace with a tinted-neutral scale that does not echo Discord's tint signature.

## 3. Typography

**Display / Body / Label Font:** `ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif`. System sans only. No webfont, no custom display, no custom mono.
**Mono Font:** `ui-monospace, SFMono-Regular, Menlo, monospace`. Used only inside `<code>` blocks; not used for measurable values in cells, ledgers, or call-id columns.

**Character:** The pairing is system-only and undifferentiated. The body text and pill labels render in the same sans at different sizes; the mono never carries figures. The result reads as "default browser font with a few size steps", not as an instrument.

### Hierarchy

- **Headline** (system-sans, weight 600, `1.5rem`, line-height ~1.2): page-level `h1`. Used on every list page heading.
- **Title** (system-sans, weight 500, `1.05rem`, letter-spacing 0.5px, **uppercase**, colour `--muted`): `h2` inside sections. Stated as "section opener".
- **Body** (system-sans, weight 400, `14px / 1.5`): every text run inside content areas, modal bodies, paragraphs, form descriptions.
- **Body Cell** (system-sans, weight 400, `13px / 1.4`): table cells. Smaller than body so single-line ellipsis truncation can fit more columns.
- **Label** (system-sans, weight 500-600, `11px`, letter-spacing 0.5px, uppercase): table column headers, `.label` form-field labels, sidebar group headers, pill text. Used heavily; carries most of the "small text" load.
- **Pill** (system-sans, weight 500, `11px`): inside `.pill-*` backgrounds. Same size as Label but lowercase.
- **Mono `<code>`** (system-mono, `12px`, `--sidebar` background, 3px rounded, 0 0.25rem padding): inline code blocks; the **only** monospace usage in the system. Cells holding E.164 numbers, call-ids, IDs, IPs, cents, durations are all sans, not mono.

### Named Rules (current, mostly anti-reference)

**The Sans-Only Rule.** Today, every measurable value (cents in `/cdrs`, call-ids in `/live`, E.164 numbers in `/dids`, IPs in supplier whitelisting) renders in the body sans. Only inline `<code>` spans use the mono. PRODUCT.md's design principle 4 explicitly inverts this: the next system commits to mono with tabular figures for every measurable.

**The Uppercase Label Rule.** Section headings (`h2`), table column headers, and form field labels all use `11px` weight 500-600 uppercase with 0.5px letter-spacing. Carried consistently. The new system can keep the uppercase-label idiom (it survives the anti-reference rewrite cleanly) or replace it with a small-caps / weight-only treatment.

## 4. Elevation

The system is **flat at rest with one shared drop-shadow on lifted surfaces**. Two surfaces use elevation: the sidebar (when collapsed/floating below 880px) and the modal (`<dialog>`). Cards, sections, tables, and inputs are all flat against their tonal background. Depth is mostly conveyed through the four-tier surface scale (bg → card → sidebar → sidebar-active).

The dialog uses a **`backdrop-filter: blur(2px)`** behind a semi-transparent backdrop. This is explicitly banned by upstream design laws (no glassmorphism as default). It is present in current code and must be removed by the next refactor.

### Shadow Vocabulary

- **Lifted Surface** (`box-shadow: 0 4px 16px rgba(0,0,0,.5)`): applied to the floating sidebar (mobile) and the modal dialog. One token used in two places. No variants (no "small / medium / large", no "hover / active").

### Named Rules (current)

**The Flat-Except-Modal-And-Mobile-Sidebar Rule.** No surface at rest uses `box-shadow`. The shared shadow is reserved for the two cases above. Carry forward in the next system if it suits.

**The Glassmorphism Violation.** `dialog::backdrop { backdrop-filter: blur(2px); }` is present today and breaks the upstream "no glassmorphism as default" law. The next system removes the blur (or uses a single deliberate purposeful surface, not the default backdrop).

## 5. Components

For each captured component below, the description reflects the current code. Behavioural notes mention which patterns are anti-reference and which can survive into the next system.

### Buttons

- **Shape:** rounded `--md` (6px). Same radius across primary / ghost / danger / warn / secondary.
- **Primary** (`button`, `a.btn`): `#5865f2` fill, white text, `0.55rem 1rem` padding, weight 500. Hover: `filter: brightness(1.08)`. **Carries the dominant anti-reference (the Discord blurple).**
- **Ghost** (`button.ghost`, `a.btn.ghost`): transparent fill, 1px border `--border`, `--text` colour. Hover: `--sidebar-active` background.
- **Danger** (`button.btn-danger`): `#da373c` fill, white text. Hover: `filter: brightness(1.1)`.
- **Warn** (`button.btn-warn`): `#f0b232` fill, **dark text `#1e1f22`** (not white — stated 7.4:1 contrast). Used on `release` action.
- **Secondary** (`button.btn-secondary`): `#404249` fill, `--text` colour. Used for `retire`, `rename`, low-priority actions.
- **Row-actions size**: `.row-actions button { padding: 0.25rem 0.6rem; font-size: 12px; }` — table-cell button compaction.

### Pills

- **Style**: 10px radius, `0.1rem 0.5rem` padding, `11px` weight 500. 18% tinted background, matching saturated colour as ink.
- **Variants**: `.pill-active` / `.pill-approved` (green), `.pill-suspended` / `.pill-rejected` (red), `.pill-cancelled` / `.pill-retired` / `.pill-inactive` (muted grey), `.pill-available` (blurple), `.pill-assigned` / `.pill-pending` / `.pill-kyc_pending` / `.pill-quarantined` / `.pill-warn` (warn yellow).
- **The 18% tint convention is consistent and survives anti-reference rewrite.** Replace only the underlying hues if the next system picks new status colours.

### Cards / Sections / Tables

- **Card** (`.card`): `#2b2d31` fill, `#1e1f22` 1px border, 8px radius, `1rem 1.2rem` padding. Used inside `.cards` grid (`auto-fit, minmax(180px, 1fr)`).
- **Section** (`.section`): same fill / border / radius as card; used as inline panel wrapper inside detail pages.
- **Tables**: `width: 100%`, `#2b2d31` fill, 8px radius, 1px `--border` border, `overflow: hidden`. Row height fixed at `38px`; cells `white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 340px`. Row hover: `#383a40`. Th: `font-size: 11px`, weight 500, uppercase, `#1e1f22` background.
- **Sortable headers** (`th a.sort`): underline removed, muted ink, hover swap to `--text`, active state swap, 9px arrow inline.
- **Section table opt-out**: `.section table td, .section table th { white-space: normal; overflow: visible; }` so detail pages get wrap-friendly cells.
- **The 38px fixed row-height + ellipsis truncation pattern survives anti-reference rewrite.** It is the densest defensible row height for system-sans `13px` body and pairs well with PRODUCT.md's density principle.

### Inputs / Fields

- **Style** (`input`, `select`, `textarea`): `#1e1f22` fill, 1px `--border` border, 6px radius, `0.5rem 0.7rem` padding, inherit text colour and font.
- **Focus**: `--accent` border + `0 0 0 3px rgba(88,101,242,.18)` shadow (Discord-blurple-tinted focus ring). **The hue is anti-reference; the focus-ring idiom itself is fine.**
- **Disabled / readonly**: `opacity: 0.6`. No second visual treatment.
- **Field label** (`.label`): `11px` weight uppercase muted, `0.25rem` bottom margin. Carried across every form.
- **Date range picker** (`.daterange`): inline composite — preset buttons + two date inputs joined inside a `#1e1f22` bordered wrapper. Active preset uses `rgba(88,101,242,.15)` background + `rgba(88,101,242,.35)` border + `--accent-text` ink. **Anti-reference hue, idiom survives.**

### Navigation (Sidebar)

- **Sidebar** (`.sidebar`): 240px wide, `#1e1f22` background, 1px `--border` right border, `1.2rem 0` vertical padding, sticky to viewport top, full height, vertical scroll on overflow.
- **Brand** (`.brand`): `--accent-text` colour, weight 700, letter-spacing 1px, `15px`. `padding: 0 1.5rem 1.2rem` with bottom border. Reads as "Discord wordmark slot".
- **Group headers** (`.group`): muted colour, `10px` uppercase, letter-spacing 1px, 0.7 opacity, `0.8rem 1.5rem 0.3rem` padding.
- **Link** (`nav a`): muted colour, `13.5px` weight 500, `0.55rem 1.5rem` padding, gap 0.6rem, `transition: all 0.15s`.
- **Link hover**: `--text` colour, `rgba(255,255,255,0.03)` background.
- **Link active** (`nav a.active`): `--accent-text` colour, `#404249` background, **3px `--accent` left border bar** (the Discord channel-selected idiom), padding-left compensated. **This is the most-cited anti-reference: replace the left-bar with a non-chat-app idiom.**
- **Footer** (`.footer`): `1rem 1.5rem` padding, 1px top border, muted text.
- **Mobile (<880px)**: sidebar position-fixed left:-260px, transition left 0.2s, `.open` class slides it in. Menu-toggle button (`☰`) appears in topbar.

### Modal (`<dialog>`)

- **Surface**: `--card` fill, `--text` ink, 1px `--border` border, 10px radius, no padding (sections own their own).
- **Backdrop**: `rgba(0,0,0,0.6)` + **`backdrop-filter: blur(2px)`** (banned by upstream laws).
- **Head** (`.modal-head`): `1rem 1.2rem` padding, 1px bottom border, flex `justify-content: space-between`. H3 weight 600 `1rem`. Close button: transparent, muted ink, `18px`, 24×24px.
- **Body** (`.modal-body`): `1rem 1.2rem` padding.
- **Foot** (`.modal-foot`): `0.8rem 1.2rem` padding, 1px top border, flex `justify-content: flex-end`, `0.5rem` gap.
- **Max-width**: 560px, width 92% (mobile-friendly).
- **The modal pattern is sound; the `backdrop-filter` is the only thing to fix.**

### Floating Tooltip (`#tooltip`)

- A single fixed-positioned element created at body scope by `DOMContentLoaded` handler, populated and moved on `mouseover` for any `.tip[data-tip]` trigger. Necessary because table cells `overflow: hidden` (uniform row height) would otherwise clip absolutely-positioned tooltips inside them.
- **Surface**: `#111214` fill (only surface darker than sidebar), `--text` ink, 1px `--border` border, 6px radius, `0.55rem 0.75rem` padding, `11.5px / 1.45`, 280px width, `z-index: 9999`, `pointer-events: none`.
- Shows by adding `.show`; hides on scroll and on `mouseout`.
- **Idiom survives anti-reference rewrite cleanly.** Only the surface hue needs to follow the new palette.

### Pagination

- **Container** (`.pagination`): flex `space-between`, `0.5rem 0` padding, muted ink, `13px`.
- **Page buttons** (`.pages a, .pages span`): `0.3rem 0.6rem` padding, 1px `--border` border, 5px radius, `--card` background, min-width 34px, `12px`. Hover: `--accent-text` border + `--text` ink. Current page: `#fff` ink on `--accent` fill with `--accent` border (Discord-blurple chip). Disabled: 0.4 opacity.
- **First / prev / next / last**: SVG chevron icons inline, same chip treatment as numeric pages.
- **The chip idiom survives; the blurple-fill on `.current` is anti-reference.**

### Daterange (composite)

- Inline composite control with 9 preset buttons + two `<input type="date">` joined inside a `#1e1f22` bordered wrapper, 6px radius, `0.25rem 0.35rem` padding.
- Preset buttons: `0.25rem 0.55rem` padding, `12px` weight 500, muted ink. Active state: blurple-tinted background + bordered + `--accent-text` ink.
- Used for date-range filtering on `/cdrs`, `/users/{id}/export/*`, `/orders/{id}/export/*`, etc.

### Flash banner (`.flash`)

- Top-of-page status banner injected by the layout from session flash:ok / flash:err.
- `.flash-ok`: green-tinted background, `--ok` ink, `0.32` alpha border.
- `.flash-err`: red-tinted background, `--err` ink, `0.32` alpha border.
- Padding `0.6rem 0.9rem`, 6px radius, `13px`. Single-line.

## 6. Do's and Don'ts

The framing of this section is inverted from a standard DESIGN.md. Because PRODUCT.md names the entire captured system as the dominant anti-reference, the "Don'ts" list every element above. The "Do's" list only the patterns that survive the anti-reference rewrite (the idioms whose hue or treatment changes but whose structure stays).

### Do (these patterns survive the anti-reference rewrite, with hue / treatment changes)

- **Do** keep the 38px fixed-row-height + ellipsis truncation idiom on list tables. Densest defensible row for a `13px` body. Matches PRODUCT.md's density principle.
- **Do** keep the floating singleton tooltip pattern. It is the right fix for the "table cell overflow:hidden clips absolutely-positioned tooltips" problem and survives any palette change.
- **Do** keep the section / detail-table opt-out (`section table td { white-space: normal; }`). It is the right escape valve for wrap-friendly cells inside detail pages.
- **Do** keep the 18% tinted pill backgrounds with saturated ink convention; replace the underlying hues if the next palette changes status colours.
- **Do** keep the dialog `<dialog>` pattern itself (HTML5 native, accessible by default); remove only the `backdrop-filter: blur(2px)`.
- **Do** keep the uppercase 11px label with 0.5px letter-spacing for section headings and form field labels — or replace cleanly with a small-caps / weight-only treatment.

### Don't (these are the anti-reference; the next redesign refuses them all)

- **Don't** carry the Discord blurple (`#5865f2`) accent. PRODUCT.md anti-reference: this exact hue is on the no-fly list. It currently fills every primary button, every focus ring, the sidebar-active left bar, the pagination-current chip, and is the colour of the brand wordmark via `#9aa6ff`.
- **Don't** carry the Discord surface scale (`#313338` bg, `#2b2d31` card, `#1e1f22` sidebar, `#404249` active). PRODUCT.md anti-reference: at these exact tints, the system reads as Discord at a glance.
- **Don't** carry the 3px `--accent` left-border bar on the active sidebar item. PRODUCT.md anti-reference: this is the Discord channel-selected idiom and signals "chat app".
- **Don't** carry the "channels-and-DMs" sidebar density (icon + label rows packed tight, hover swap, pill-active state). PRODUCT.md anti-reference.
- **Don't** carry Discord's accent-text-on-active-row treatment (the `--accent-text` `#9aa6ff` inside the active sidebar link). The system's accent should fill behind white text, not act as ink on a tonal background.
- **Don't** carry `backdrop-filter: blur(2px)` on `dialog::backdrop`. Banned by upstream design laws (no glassmorphism as default). Remove on first refactor.
- **Don't** carry the sans-only-typography choice. The next system commits to mono with tabular figures for every measurable (cents, durations, IDs, E.164, IPs, call-ids). PRODUCT.md design principle 4.
- **Don't** treat `<code>` as the only mono surface. It is incidental, not systematic. The next system uses mono structurally.
- **Don't** carry the muted-grey-on-very-dark-grey `<th>` background (`#1e1f22`). Reads as Discord chat-area header. Replace with a treatment that does not echo it.
- **Don't** rely on the single `--shadow: 0 4px 16px rgba(0,0,0,.5)` for both the floating sidebar and the modal. The next system either picks a different shadow vocabulary or commits more deliberately to flat-with-tonal-layering.
- **Don't** rely on `filter: brightness(1.08)` as the only hover treatment for primary buttons. It is a hack to avoid defining a real hover token; the next system commits to a real hover colour (or a real hover treatment) per variant.
- **Don't** carry the `:root` palette names (`--bg`, `--card`, `--accent`, `--accent-text`, `--sidebar-active`). They encode the Discord-clone vocabulary by name; the next system picks names that describe the work, not the source app.
- **Don't** introduce side-stripe borders > 1px as decorative accents anywhere (the active-sidebar 3px bar is the closest current code comes; it is already on the anti-reference list).
- **Don't** introduce gradient text. Not present today; not allowed in the next system either.
- **Don't** introduce the SaaS hero-metric template on the `/` dashboard (big number + small label + supporting stats + gradient accent). Not present today; remains banned.
