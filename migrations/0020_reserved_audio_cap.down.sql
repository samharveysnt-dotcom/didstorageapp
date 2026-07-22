BEGIN;

DELETE FROM settings WHERE key = 'sip.reserved_audio_max_seconds';

COMMIT;
