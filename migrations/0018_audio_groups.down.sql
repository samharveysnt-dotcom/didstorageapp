BEGIN;
ALTER TABLE dids DROP COLUMN IF EXISTS reserved_audio_group_id;
DROP TABLE IF EXISTS audio_group_members;
DROP TABLE IF EXISTS audio_groups;
-- route_kind enum value 'audio_group' is left in place: Postgres does not
-- support removing enum values without rewriting the enum. Unreferenced
-- enum values are harmless.
COMMIT;
