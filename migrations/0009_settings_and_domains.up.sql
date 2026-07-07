-- Phase 7: admin-editable site / company settings + a small
-- domains-and-certs table that drives an SNI-based HTTPS listener.

BEGIN;

-- Generic key/value store for tweakable strings (company name, support email,
-- session timeout, retention days, etc). Everything users see/configure
-- lands here; code reads through internal/settings which keeps an in-memory
-- map and reloads on every write.
CREATE TABLE IF NOT EXISTS settings (
  key         TEXT PRIMARY KEY,
  value       TEXT NOT NULL DEFAULT '',
  category    TEXT NOT NULL DEFAULT 'general',
  description TEXT NOT NULL DEFAULT '',
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO settings (key, value, category, description) VALUES
 ('company.name',          'DIDStorage',                                                          'company', 'Legal / brand name shown in the admin GUI header and on customer-facing surfaces.'),
 ('company.brand',         'DIDStorage',                                                          'company', 'White-label brand string used in SIP trace sanitization when no reseller-specific brand_name is set.'),
 ('company.support_email', '',                                                                    'company', 'Support contact shown to customers and resellers.'),
 ('company.legal_address', '',                                                                    'company', 'Registered legal address — used in invoices and compliance exports.'),
 ('company.tax_id',        '',                                                                    'company', 'VAT / tax identifier, included in invoices.'),

 ('site.public_ip',                '',          'site', 'Outward-facing IPv4 the SIP supplier reaches us on. Used for SIP trace sanitization (our public IP becomes the brand name in reseller-facing traces).'),
 ('site.public_url',               '',          'site', 'Base URL admins use to reach this GUI (e.g. https://admin.example.com). Used for absolute links in emails / exports.'),
 ('site.https_listen_addr',        ':443',      'site', 'Address the HTTPS listener binds to. Set blank to disable. Requires CAP_NET_BIND_SERVICE for low ports.'),
 ('site.session_timeout_minutes',  '480',       'site', 'How long an admin login lasts (minutes) before re-auth is required.'),
 ('site.default_per_page',         '25',        'site', 'Default page size for table listings.'),
 ('site.min_auth_seconds',         '6',         'site', 'Minimum call duration the user balance must cover before we authorize an INVITE.'),
 ('site.pcap_retention_days',      '7',         'site', 'Days of rolling SIP-capture pcaps we keep on disk before sip-capture rotates them out.')
ON CONFLICT (key) DO NOTHING;

-- One row per domain that should be served over HTTPS. cert_pem / key_pem are
-- the literal PEM text (NULL while pending). cert_expires_at is parsed out of
-- the cert at upload time so the admin GUI can warn about renewal.
CREATE TABLE IF NOT EXISTS site_domains (
  id              BIGSERIAL PRIMARY KEY,
  hostname        TEXT NOT NULL UNIQUE,
  cert_pem        TEXT,
  key_pem         TEXT,
  cert_subject    TEXT NOT NULL DEFAULT '',
  cert_issuer     TEXT NOT NULL DEFAULT '',
  cert_expires_at TIMESTAMPTZ,
  is_default      BOOLEAN NOT NULL DEFAULT false,
  notes           TEXT NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only one row can be the default (used as the SNI fallback).
CREATE UNIQUE INDEX IF NOT EXISTS site_domains_default_idx
  ON site_domains ((1)) WHERE is_default = true;

COMMIT;
