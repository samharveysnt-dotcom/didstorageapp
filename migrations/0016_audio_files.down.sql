-- Roll back the audio library. We can drop the column + table cleanly; the
-- 'audio' enum value lingers on `route_kind` because Postgres cannot remove
-- enum values online (a future migration could rebuild the enum if needed).
-- Any reserved DID with kind='audio' must be released BEFORE running this.

BEGIN;

ALTER TABLE dids DROP COLUMN IF EXISTS reserved_audio_file_id;

DROP TABLE IF EXISTS audio_files;

-- Note: cannot DROP a value from route_kind without rebuilding the enum.
-- Leave 'audio' in place; it's harmless when no code path emits it.

COMMIT;
