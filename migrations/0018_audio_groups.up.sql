-- Audio groups: a named set of audio_files clips that, when used as a
-- reserved-DID route, plays a DIFFERENT clip on each incoming call.
--
-- Adds a fifth route_kind, 'audio_group'. didapi resolves the group → one
-- audio_files row on every INVITE and hands the dialplan a plain 'audio'
-- kind + that file's basename — so the dialplan needs no changes (it
-- already knows how to Playback() an audio target).
--
-- Selection strategy is random-no-repeat: pick uniformly from the group's
-- members EXCLUDING whichever file played most recently. State for that
-- exclusion lives in Redis (`audio_group_last:<id>` → audio_file_id, short
-- TTL) so the DB doesn't take a write per call.
--
-- Membership is many-to-many (a file can live in 0..N groups) — supports
-- the "Allow both" upload model where a single file might be reused
-- across campaigns. Files have a 'position' for stable display ordering
-- inside a group; selection ignores position.
--
-- File-naming convention is enforced at upload time, not in SQL: bulk
-- uploads to a group take a base prefix and number the saved files
-- "<prefix>-1", "<prefix>-2", ... The display name is what hits
-- audio_files.name; the on-disk filename remains af_<rand> for safety.

BEGIN;

-- 1. Named library.
CREATE TABLE audio_groups (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT        NOT NULL UNIQUE,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  BIGINT      REFERENCES admins(id) ON DELETE SET NULL,
    CONSTRAINT audio_groups_name_nonempty CHECK (length(trim(name)) > 0)
);

-- 2. Membership. ON DELETE CASCADE on group_id lets us drop a group cleanly
--    (the audio_files rows live on; only their group association vanishes).
--    ON DELETE RESTRICT on audio_file_id keeps the existing
--    "can't delete a file that's still referenced" guarantee.
CREATE TABLE audio_group_members (
    group_id      BIGINT      NOT NULL REFERENCES audio_groups(id) ON DELETE CASCADE,
    audio_file_id BIGINT      NOT NULL REFERENCES audio_files(id)  ON DELETE RESTRICT,
    position      INTEGER     NOT NULL DEFAULT 0,
    added_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, audio_file_id)
);

CREATE INDEX audio_group_members_group_idx
    ON audio_group_members (group_id, position);

CREATE INDEX audio_group_members_file_idx
    ON audio_group_members (audio_file_id);

-- 3. New enum value. As with 0016's 'audio', this is append-only and safe
--    to add online — Postgres lets enum values be added without rewriting
--    referencing columns.
ALTER TYPE route_kind ADD VALUE IF NOT EXISTS 'audio_group';

-- 4. dids.reserved_audio_group_id — set when reserved_route_kind='audio_group'.
--    Mirrors reserved_audio_file_id from 0016. RESTRICT (not CASCADE) on the
--    FK so deleting a group that's still referenced by a reservation fails
--    loudly instead of silently breaking routing.
ALTER TABLE dids
    ADD COLUMN reserved_audio_group_id BIGINT
        REFERENCES audio_groups(id) ON DELETE RESTRICT;

CREATE INDEX dids_reserved_audio_group_idx
    ON dids (reserved_audio_group_id)
    WHERE reserved_audio_group_id IS NOT NULL;

-- 5. Grants. didapi connects as the 'didstorage' role; the migration runs
--    as 'postgres' so explicit grants are required (mirrors 0016).
GRANT SELECT, INSERT, UPDATE, DELETE ON audio_groups,        audio_group_members TO didstorage;
GRANT USAGE, SELECT                   ON SEQUENCE audio_groups_id_seq            TO didstorage;

COMMIT;
