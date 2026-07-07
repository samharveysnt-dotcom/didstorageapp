-- Audio file library for "play audio then hang up" DID reservations.
--
-- Until now a reserved DID could only route to a SIP target (sip_uri, ip,
-- sip_account). Add a fourth kind, 'audio', where the inbound INVITE is
-- answered, an admin-uploaded clip plays once, and the call drops cleanly
-- on its own — no upstream leg, no billing.
--
-- Files live on disk under /var/lib/asterisk/sounds/didstorage/<filename>.
-- We canonicalise everything to 16-bit signed-linear 8 kHz mono (.slin) at
-- upload time so the dialplan can Playback() the basename without worrying
-- about codec mismatch with the call leg.
--
-- The relationship lives on dids via a new nullable column. We deliberately
-- do NOT ON DELETE CASCADE on the FK: deleting an audio file that's still
-- referenced by a reserved DID would silently break the routing for that
-- DID. The audioFileDelete handler explicitly refuses delete when any DID
-- still references the file.

BEGIN;

-- 1. Library table.
CREATE TABLE audio_files (
    id                 BIGSERIAL PRIMARY KEY,
    name               TEXT        NOT NULL UNIQUE,
    -- filename is the on-disk basename WITHOUT extension. Asterisk's
    -- Playback() wants the basename so it can pick the best matching codec
    -- file at runtime. E.g. filename='af_a1b2c3d4' on disk as
    -- /var/lib/asterisk/sounds/didstorage/af_a1b2c3d4.slin
    filename           TEXT        NOT NULL UNIQUE,
    original_filename  TEXT,
    size_bytes         BIGINT      NOT NULL DEFAULT 0,
    duration_ms        INTEGER     NOT NULL DEFAULT 0,
    -- format is the on-disk encoding we converted to. Always 'slin' today;
    -- kept as a column so a future "keep original codec" path stays trivial.
    format             TEXT        NOT NULL DEFAULT 'slin',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by         BIGINT      REFERENCES admins(id) ON DELETE SET NULL,
    CONSTRAINT audio_files_name_nonempty  CHECK (length(trim(name))     > 0),
    CONSTRAINT audio_files_filename_safe  CHECK (filename ~ '^[a-zA-Z0-9_-]+$')
);

CREATE INDEX audio_files_created_at_idx ON audio_files (created_at DESC);

-- 2. Add 'audio' to the route_kind enum so reserved_route_kind can hold it.
--    Postgres enum values can be appended online without rewriting referencing
--    columns. The new value flows through to orders.route_kind /
--    cdrs.routed_kind too, but app code only emits 'audio' for reservations.
ALTER TYPE route_kind ADD VALUE IF NOT EXISTS 'audio';

-- 3. dids.reserved_audio_file_id — set when reserved_route_kind = 'audio'.
ALTER TABLE dids
    ADD COLUMN reserved_audio_file_id BIGINT
        REFERENCES audio_files(id) ON DELETE RESTRICT;

CREATE INDEX dids_reserved_audio_file_idx
    ON dids (reserved_audio_file_id)
    WHERE reserved_audio_file_id IS NOT NULL;

-- 4. Grants. didapi connects as `didstorage`; the migration runs as the
--    `postgres` superuser, so explicit table-level grants are required.
GRANT SELECT, INSERT, UPDATE, DELETE ON audio_files TO didstorage;
GRANT USAGE, SELECT ON SEQUENCE audio_files_id_seq TO didstorage;

COMMIT;
