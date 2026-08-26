-- Idempotency for catalog's three create operations (TKT-200).
--
-- POST /events, /performances and /ticket-types accepted no key, so a
-- double-click, a concurrent submit or a back-button re-POST created duplicate
-- rows. Commerce has solved this four times over; the asymmetry was catalog's,
-- not the platform's.
--
-- THE UNIQUE INDEX IS THE MECHANISM, and that is the load-bearing decision
-- here. A check-then-insert in Go closes the back-button case and NOTHING else:
-- two requests that are in flight together both read "no such key", both find
-- nothing, and both insert. The race this ticket exists to close is exactly the
-- one application logic cannot see. Postgres refusing the second insert is what
-- makes the guarantee hold, so the index is not an optimization of the lookup —
-- the lookup is a convenience on top of the index.
--
-- SCOPED BY organizer_id (ADR-002). Every entity here carries an organizer, and
-- a key is a string the caller chooses: without the scope, one organizer
-- picking 'create-event-1' would collide with another organizer's identical
-- choice, and the second tenant would be handed the first tenant's row as a
-- "replay". The organizer comes from the verified assertion (TKT-245), never
-- the request body, which is what makes the scope trustworthy rather than
-- self-asserted.
--
-- THE COLUMNS ARE NULLABLE AND THE INDEX IS PARTIAL, deliberately. Do not
-- "tighten" this to NOT NULL later:
--
--   * Rows created before this migration have no key and no honest value to
--     invent. A NOT NULL DEFAULT would need a DISTINCT legacy value per row or
--     the unique index could not be created at all.
--   * A DEFAULT on the insert path is a mechanism whose only guard is every
--     future caller remembering not to lean on it. The API refuses an empty key
--     before it ever reaches SQL, so the nullable column is unreachable from
--     the contract surface.
--   * NULL never collides with NULL under a unique index, so legacy rows and
--     any future non-API writer coexist without weakening the guarantee for
--     rows that DO carry a key.
--
-- This is the ordinary NULL-safety rule pointing the other way: a NULL here
-- means "no key was presented", and the guard that refuses that case lives in
-- the handler, not in this predicate. A guard predicate whose unknown case
-- means "pass" is a suggestion — this index's unknown case means "not subject
-- to the constraint", which is a different and correct statement.
--
-- request_fingerprint is what makes a reused key with a DIFFERENT body a 409
-- rather than a silent replay of somebody else's resource. Same decision, same
-- reasoning, as commerce's order_refunds and order_exchanges.
--
-- Name the adversary (ADR-021): this is HONEST-WRITER consistency. It stops a
-- double-submit, a retry and a buggy caller. Anyone with catalog DB access can
-- insert a duplicate row with a NULL key and this constraint will not notice —
-- it is not tamper-evidence and does not claim to be.

-- +goose Up
ALTER TABLE events
    ADD COLUMN idempotency_key     text CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    ADD COLUMN request_fingerprint text
    -- The two columns are present together or absent together. A keyed row with
    -- no fingerprint is the one state the replay path cannot answer correctly:
    -- replayLookup reads a NULL fingerprint as a MISMATCH, so an exact retry of
    -- the original request would be refused as a conflict forever (ai-review
    -- [medium]). Fail-closed is the right reading of an unknown fingerprint —
    -- but the row should not be constructible in the first place.
    ,ADD CONSTRAINT events_key_and_fingerprint_agree
        CHECK ((idempotency_key IS NULL) = (request_fingerprint IS NULL));
ALTER TABLE performances
    ADD COLUMN idempotency_key     text CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    ADD COLUMN request_fingerprint text
    -- The two columns are present together or absent together. A keyed row with
    -- no fingerprint is the one state the replay path cannot answer correctly:
    -- replayLookup reads a NULL fingerprint as a MISMATCH, so an exact retry of
    -- the original request would be refused as a conflict forever (ai-review
    -- [medium]). Fail-closed is the right reading of an unknown fingerprint —
    -- but the row should not be constructible in the first place.
    ,ADD CONSTRAINT performances_key_and_fingerprint_agree
        CHECK ((idempotency_key IS NULL) = (request_fingerprint IS NULL));
ALTER TABLE ticket_types
    ADD COLUMN idempotency_key     text CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    ADD COLUMN request_fingerprint text
    -- The two columns are present together or absent together. A keyed row with
    -- no fingerprint is the one state the replay path cannot answer correctly:
    -- replayLookup reads a NULL fingerprint as a MISMATCH, so an exact retry of
    -- the original request would be refused as a conflict forever (ai-review
    -- [medium]). Fail-closed is the right reading of an unknown fingerprint —
    -- but the row should not be constructible in the first place.
    ,ADD CONSTRAINT ticket_types_key_and_fingerprint_agree
        CHECK ((idempotency_key IS NULL) = (request_fingerprint IS NULL));

-- Plain CREATE INDEX, not CONCURRENTLY: ADR-020's preconditions are conjunctive
-- and (2) and (3) remain false, so catalog has not adopted CONCURRENTLY and this
-- migration does not become the exception.
CREATE UNIQUE INDEX events_organizer_idempotency_key
    ON events (organizer_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX performances_organizer_idempotency_key
    ON performances (organizer_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX ticket_types_organizer_idempotency_key
    ON ticket_types (organizer_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- +goose Down
DROP INDEX ticket_types_organizer_idempotency_key;
DROP INDEX performances_organizer_idempotency_key;
DROP INDEX events_organizer_idempotency_key;
ALTER TABLE ticket_types DROP COLUMN request_fingerprint, DROP COLUMN idempotency_key;
ALTER TABLE performances DROP COLUMN request_fingerprint, DROP COLUMN idempotency_key;
ALTER TABLE events       DROP COLUMN request_fingerprint, DROP COLUMN idempotency_key;
