-- Audio routes on orders (not just reservations).
--
-- Until now, an active order's route_kind was constrained in the GUI to
-- sip_uri / ip / sip_account. The 'audio' and 'audio_group' enum values
-- only flowed through dids.reserved_route_kind. This migration opens the
-- door to letting a billed customer's DID always answer with a clip
-- (e.g. an after-hours announcement) or rotate through a group of clips
-- per call.
--
-- Two shapes:
--
--   route_kind = 'audio'        route_target = 'didstorage/af_<basename>'
--                               audio_group_id = NULL
--                               (mirrors reservations — basename baked
--                                into route_target at save time so the
--                                AGI path doesn't change)
--
--   route_kind = 'audio_group'  route_target = NULL (or stale; ignored)
--                               audio_group_id = <fk to audio_groups.id>
--                               (sipctl resolves to a concrete clip per
--                                INVITE via pickAudioGroupMember)
--
-- We only need ONE new column — audio_group_id — because audio_file_id
-- can be encoded in route_target as it is for reservations. Symmetrical
-- with dids.reserved_audio_group_id (migration 0018).

BEGIN;

ALTER TABLE orders
    ADD COLUMN audio_group_id BIGINT
        REFERENCES audio_groups(id) ON DELETE RESTRICT;

CREATE INDEX orders_audio_group_idx
    ON orders (audio_group_id)
    WHERE audio_group_id IS NOT NULL;

COMMIT;
