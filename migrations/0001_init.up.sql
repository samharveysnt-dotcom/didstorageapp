-- DIDStorage v1 schema.
-- All money in integer cents (USD only). All times TIMESTAMPTZ in UTC.

BEGIN;

CREATE TYPE did_type          AS ENUM ('mobile', 'national', 'local', 'tollfree');
CREATE TYPE route_kind        AS ENUM ('sip_account', 'sip_uri', 'ip');
CREATE TYPE assignment_status AS ENUM ('active', 'suspended', 'cancelled');
CREATE TYPE ledger_kind       AS ENUM (
  'topup', 'nrc', 'mrc', 'channel_fee', 'call_charge', 'manual_adj', 'refund'
);

CREATE TABLE countries (
  iso  CHAR(2) PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE TABLE suppliers (
  id          BIGSERIAL PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  status      TEXT NOT NULL DEFAULT 'active',
  notes       TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE supplier_ip_groups (
  id           BIGSERIAL PRIMARY KEY,
  supplier_id  BIGINT NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  UNIQUE (supplier_id, name)
);

CREATE TABLE supplier_ip_group_members (
  id        BIGSERIAL PRIMARY KEY,
  group_id  BIGINT NOT NULL REFERENCES supplier_ip_groups(id) ON DELETE CASCADE,
  cidr      CIDR NOT NULL,
  UNIQUE (group_id, cidr)
);
CREATE INDEX supplier_ip_group_members_cidr_idx ON supplier_ip_group_members USING gist (cidr inet_ops);

-- Pricing keyed by (supplier, country, did_type). All four price components live here.
CREATE TABLE rate_cards (
  id                     BIGSERIAL PRIMARY KEY,
  supplier_id            BIGINT  NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
  country_iso            CHAR(2) NOT NULL REFERENCES countries(iso),
  did_type               did_type NOT NULL,
  nrc_cents              INTEGER NOT NULL CHECK (nrc_cents >= 0),
  mrc_cents              INTEGER NOT NULL CHECK (mrc_cents >= 0),
  channel_monthly_cents  INTEGER NOT NULL CHECK (channel_monthly_cents >= 0),
  per_minute_cents       NUMERIC(10,4) NOT NULL CHECK (per_minute_cents >= 0),
  valid_from             TIMESTAMPTZ NOT NULL DEFAULT now(),
  valid_to               TIMESTAMPTZ,
  UNIQUE (supplier_id, country_iso, did_type, valid_from)
);
CREATE INDEX rate_cards_active_idx ON rate_cards (supplier_id, country_iso, did_type)
  WHERE valid_to IS NULL;

CREATE TABLE dids (
  id                     BIGSERIAL PRIMARY KEY,
  e164                   TEXT NOT NULL UNIQUE,
  supplier_id            BIGINT NOT NULL REFERENCES suppliers(id) ON DELETE RESTRICT,
  country_iso            CHAR(2) NOT NULL REFERENCES countries(iso),
  did_type               did_type NOT NULL,
  supplier_channel_cap   INTEGER,                   -- NULL = unlimited from supplier
  status                 TEXT NOT NULL DEFAULT 'available'
    CHECK (status IN ('available','assigned','retired')),
  created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX dids_status_idx ON dids (status);
CREATE INDEX dids_country_type_idx ON dids (country_iso, did_type) WHERE status = 'available';

CREATE TABLE resellers (
  id              BIGSERIAL PRIMARY KEY,
  name            TEXT NOT NULL UNIQUE,
  portal_hostname TEXT UNIQUE,
  status          TEXT NOT NULL DEFAULT 'active',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE pops (
  id                BIGSERIAL PRIMARY KEY,
  reseller_id       BIGINT REFERENCES resellers(id) ON DELETE SET NULL,
  name              TEXT NOT NULL UNIQUE,
  sip_listen_ip     INET NOT NULL,
  rtp_public_ip     INET NOT NULL,
  mtls_fingerprint  TEXT NOT NULL,                  -- POP client cert SHA256
  status            TEXT NOT NULL DEFAULT 'active',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX pops_reseller_idx ON pops (reseller_id);

CREATE TABLE users (
  id                  BIGSERIAL PRIMARY KEY,
  reseller_id         BIGINT REFERENCES resellers(id) ON DELETE SET NULL,
  email               TEXT NOT NULL UNIQUE,
  password_hash       TEXT NOT NULL,
  balance_cents       BIGINT NOT NULL DEFAULT 0,
  global_channel_cap  INTEGER,                      -- NULL = no global cap
  status              TEXT NOT NULL DEFAULT 'active',
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX users_reseller_idx ON users (reseller_id);

-- in-system SIP accounts users can route DIDs to
CREATE TABLE sip_accounts (
  id          BIGSERIAL PRIMARY KEY,
  user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  username    TEXT NOT NULL,
  realm       TEXT NOT NULL,
  ha1         TEXT NOT NULL,                        -- MD5(user:realm:pass)
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (username, realm)
);
CREATE INDEX sip_accounts_user_idx ON sip_accounts (user_id);

CREATE TABLE did_assignments (
  id                BIGSERIAL PRIMARY KEY,
  did_id            BIGINT NOT NULL REFERENCES dids(id) ON DELETE RESTRICT,
  user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  pop_id            BIGINT REFERENCES pops(id),     -- NULL means central acts as POP
  channel_count     INTEGER NOT NULL DEFAULT 2 CHECK (channel_count >= 2),
  route_kind        route_kind NOT NULL,
  route_target      TEXT NOT NULL,                  -- sip_account_id | sip uri | ip:port
  rate_card_id      BIGINT NOT NULL REFERENCES rate_cards(id),  -- snapshot
  status            assignment_status NOT NULL DEFAULT 'active',
  anniversary_day   SMALLINT NOT NULL CHECK (anniversary_day BETWEEN 1 AND 28),
  next_billing_at   TIMESTAMPTZ NOT NULL,
  assigned_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  ended_at          TIMESTAMPTZ
);
CREATE INDEX did_assignments_did_active_idx ON did_assignments (did_id) WHERE status = 'active';
CREATE INDEX did_assignments_user_idx ON did_assignments (user_id);
CREATE INDEX did_assignments_billing_due_idx ON did_assignments (next_billing_at)
  WHERE status = 'active';
-- enforce: at most one active assignment per DID
CREATE UNIQUE INDEX did_assignments_one_active_per_did
  ON did_assignments (did_id) WHERE status = 'active';

-- append-only money log; users.balance_cents is the cached projection
CREATE TABLE balance_ledger (
  id            BIGSERIAL PRIMARY KEY,
  user_id       BIGINT NOT NULL REFERENCES users(id),
  delta_cents   BIGINT NOT NULL,                    -- +credit / -debit
  kind          ledger_kind NOT NULL,
  ref_table     TEXT,
  ref_id        BIGINT,
  balance_after BIGINT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX balance_ledger_user_time_idx ON balance_ledger (user_id, created_at DESC);

-- subscription billing audit trail
CREATE TABLE billing_runs (
  id                     BIGSERIAL PRIMARY KEY,
  did_assignment_id      BIGINT NOT NULL REFERENCES did_assignments(id) ON DELETE CASCADE,
  ran_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
  charged_channels       INTEGER NOT NULL,
  mrc_charged_cents      INTEGER NOT NULL,
  channel_charged_cents  INTEGER NOT NULL,
  outcome                TEXT NOT NULL CHECK (outcome IN ('charged','downgraded','cancelled')),
  notes                  TEXT
);
CREATE INDEX billing_runs_assignment_idx ON billing_runs (did_assignment_id, ran_at DESC);

-- live call records; one row per completed call leg-pair
CREATE TABLE cdrs (
  id                 BIGSERIAL PRIMARY KEY,
  call_id            TEXT NOT NULL UNIQUE,
  did_assignment_id  BIGINT NOT NULL REFERENCES did_assignments(id),
  user_id            BIGINT NOT NULL REFERENCES users(id),
  supplier_id        BIGINT NOT NULL REFERENCES suppliers(id),
  pop_id             BIGINT REFERENCES pops(id),
  started_at         TIMESTAMPTZ NOT NULL,
  answered_at        TIMESTAMPTZ,
  ended_at           TIMESTAMPTZ NOT NULL,
  billsec            INTEGER NOT NULL,
  charged_minutes    INTEGER NOT NULL,
  rate_cents_per_min NUMERIC(10,4) NOT NULL,
  charge_cents       INTEGER NOT NULL,
  hangup_cause       TEXT,
  src_uri            TEXT,
  dst_uri            TEXT,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cdrs_user_started_idx ON cdrs (user_id, started_at DESC);
CREATE INDEX cdrs_did_started_idx  ON cdrs (did_assignment_id, started_at DESC);
CREATE INDEX cdrs_supplier_started_idx ON cdrs (supplier_id, started_at DESC);

-- platform admins (separate from users so resellers can't elevate)
CREATE TABLE admins (
  id            BIGSERIAL PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- session table for alexedwards/scs (admin + user portals share)
CREATE TABLE sessions (
  token  TEXT PRIMARY KEY,
  data   BYTEA NOT NULL,
  expiry TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- API keys for reseller external access
CREATE TABLE api_keys (
  id           BIGSERIAL PRIMARY KEY,
  reseller_id  BIGINT REFERENCES resellers(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  key_hash     TEXT NOT NULL UNIQUE,
  last_used_at TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at   TIMESTAMPTZ
);
CREATE INDEX api_keys_reseller_idx ON api_keys (reseller_id);

COMMIT;
