-- Audit trail for DID reservations.
--
-- Before this migration, `dids.reserved_*` columns held the *current* (still
-- reserved) snapshot only — releasing a DID NULLed everything, so there was
-- no record of who reserved it for what reason. Compliance and ops both
-- want that history.
--
-- The release flow is updated in the same deploy: didRelease snapshots into
-- this table inside the same transaction that flips the dids row back to
-- 'available', so we never produce orphan history rows or silent gaps.

CREATE TABLE did_reservation_history (
    id                     BIGSERIAL PRIMARY KEY,
    did_id                 BIGINT      NOT NULL REFERENCES dids(id) ON DELETE CASCADE,
    reserved_route_kind    route_kind,
    reserved_route_target  TEXT,
    reserved_note          TEXT,
    reserved_at            TIMESTAMPTZ NOT NULL,
    reserved_by            BIGINT      REFERENCES admins(id) ON DELETE SET NULL,
    released_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_by            BIGINT      REFERENCES admins(id) ON DELETE SET NULL
);

CREATE INDEX did_reservation_history_did_idx
    ON did_reservation_history (did_id, released_at DESC);

-- Match the grant pattern every other DIDStorage table follows: the
-- migration runs as superuser (postgres), but the app connects as
-- `didstorage`, which needs explicit table-level privileges.
GRANT SELECT, INSERT, UPDATE, DELETE ON did_reservation_history TO didstorage;
GRANT USAGE, SELECT ON SEQUENCE did_reservation_history_id_seq TO didstorage;
