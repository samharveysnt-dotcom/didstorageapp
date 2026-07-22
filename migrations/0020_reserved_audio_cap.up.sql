-- Admin-editable runaway cap on reserved-DID audio playback calls.
--
-- Before this migration the ceiling was hardcoded to 300s in
-- internal/sipctl. The value comes back from /sipctl/authorize as
-- AUTH_MAX_SECONDS and the dial-audio branch enforces it via
-- Set(TIMEOUT(absolute)=…). The default keeps that behaviour; admins
-- can raise it (long announcement clips) or lower it (aggressive
-- runaway guard) from /settings without a redeploy.
--
-- 300s (5 min) covers any realistic announcement, IVR intro, or
-- hold-music loop with headroom. Set to 60 for aggressive integrators;
-- set high (e.g. 3600) if you host long recordings on reserved DIDs.

BEGIN;

INSERT INTO settings (key, value, category, description) VALUES
 ('sip.reserved_audio_max_seconds', '300', 'sip',
  'Absolute duration cap (seconds) for reserved-DID audio playback calls. Enforced via TIMEOUT(absolute) in the dial-audio branch and returned to the AGI as AUTH_MAX_SECONDS. Prevents a missing / looping / oversized clip from holding the channel open indefinitely.')
ON CONFLICT (key) DO NOTHING;

COMMIT;
