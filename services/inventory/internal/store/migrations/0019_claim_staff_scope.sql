-- +goose Up
-- Staff-created claims get a namespace of their own, for the reason migration 0016
-- gave when it did the same for resellers:
--
--   "A prefix inside a shared namespace is not a namespace; it is a naming
--    convention that an attacker also gets to use."
--
-- 0016 applied that rule to the reseller path and left four staff call sites still
-- decorating the key in Go -- "op-place:", "convert:<id>:", "grp-place:",
-- "grp-draw:<id>:" -- with reseller_scope NULL, so they land in
-- claims_public_idempotency beside arbitrary public keys. Public keys are caller
-- supplied, up to 200 chars (internal/api/server.go). Claims rows are never deleted.
--
-- So a public caller who sends Idempotency-Key: "op-place:X" permanently occupies
-- that row, and a later staff PlaceOperationalHold with key X passes the
-- claim_history registry (which is empty for it), hits the unique index at INSERT,
-- and answers an UNMAPPED 500 -- forever, for that (organizer, key). TKT-296 D2.
--
-- SHAPE: a nullable boolean, not a sentinel and not a text enum.
--   * NULL  = public or reseller claim (every row that exists today)
--   * TRUE  = staff-created claim
-- NULL is not a value, which is the property 0016 wanted and is wanted again here:
-- a public key and a staff key can no longer name the same row whatever string
-- either caller sends, because they differ in a field the caller cannot supply.
-- A text enum was rejected: it would re-key the reseller namespace 0016 just fixed.
ALTER TABLE claims
    ADD COLUMN staff_scope boolean;

-- Staff scope and reseller scope are mutually exclusive: a staff operation is not
-- placed on behalf of a partner. Enforced rather than assumed, so a future writer
-- cannot create a row that both partial indexes ignore.
ALTER TABLE claims
    ADD CONSTRAINT claims_staff_scope_shape
    CHECK (staff_scope IS NULL OR (staff_scope = true AND reseller_scope IS NULL));

-- BACKFILL: classify from claim_history, NOT from claim_kind.
--
-- claim_kind looks like the natural discriminator and IS THE TRAP. Two of the four
-- staff writers insert claim_kind='buyer' by design, because their rows genuinely
-- ARE buyer holds created by a staff action:
--   * ConvertOperational        (operational.go)  -> child is 'buyer'
--   * DrawDownGroupReservation  (reservations.go) -> child is 'buyer'
-- A backfill keyed on claim_kind would silently leave those two in the public
-- namespace and leave half the defect live under a green test suite.
--
-- claim_history records the ACTION that created each row, and the action vocabulary
-- distinguishes them exactly (0007 widened the CHECK to add 'reserve'/'draw_down'):
--   * place     -> claim_history.claim_id          (PlaceOperationalHold)
--   * reserve   -> claim_history.claim_id          (PlaceGroupReservation)
--   * convert   -> claim_history.related_claim_id  (the buyer child)
--   * draw_down -> claim_history.related_claim_id  (the buyer child)
UPDATE claims c SET staff_scope = true
 WHERE c.reseller_scope IS NULL
   AND EXISTS (
         SELECT 1 FROM claim_history h
          WHERE h.action IN ('place','reserve')
            AND h.claim_id = c.id
       );

UPDATE claims c SET staff_scope = true
 WHERE c.reseller_scope IS NULL
   AND EXISTS (
         SELECT 1 FROM claim_history h
          WHERE h.action IN ('convert','draw_down')
            AND h.related_claim_id = c.id
       );

-- Historical idempotency_key values are deliberately NOT rewritten. The backfill
-- sets scope only. Rewriting a key would change which row an in-flight retry
-- finds, which is the one behaviour this migration must not perturb; the existing
-- prefixed keys stay exactly as written and simply move to their own namespace.

-- Rebuild the public uniqueness so it excludes staff rows, and give staff its own.
-- DROP INDEX is correct HERE (unlike 0016, which needed DROP CONSTRAINT): 0016
-- created claims_public_idempotency as a plain index, not a constraint, so no
-- constraint owns it.
DROP INDEX claims_public_idempotency;

CREATE UNIQUE INDEX claims_public_idempotency
    ON claims(organizer_id, idempotency_key)
    WHERE reseller_scope IS NULL AND staff_scope IS NULL;

CREATE UNIQUE INDEX claims_staff_idempotency
    ON claims(organizer_id, idempotency_key)
    WHERE reseller_scope IS NULL AND staff_scope IS TRUE;

-- DEPLOY NOTE — quiesced cutover, same posture as 0016 and for the same reasons.
-- goose runs this in one transaction; ADD COLUMN takes ACCESS EXCLUSIVE and holds it
-- through the backfill and both index builds, so readers of claims block for the
-- whole thing. The binaries are not compatible in either direction across it:
--   * new binary on schema 0018 -> staff inserts name a column that does not exist
--   * previous binary on 0019   -> writes the PREFIXED key with staff_scope NULL, so
--                                  it lands in the public namespace: the defect, plus
--                                  a staff retry that misses its own row
-- Stop inventory, migrate, start the new binary.

-- +goose Down
-- Refuses rather than performs, exactly as 0014, 0015 and 0016 do. Collapsing the
-- staff namespace back into the public one would fail on any (organizer, key) pair
-- that a public caller and a staff operation both used -- and if it did NOT fail, it
-- would mean re-admitting the collision this migration exists to close.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM claims WHERE staff_scope IS TRUE) THEN
    RAISE EXCEPTION 'staff-scoped claims exist; downgrading would merge two idempotency namespaces into one';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX claims_staff_idempotency;

DROP INDEX claims_public_idempotency;

CREATE UNIQUE INDEX claims_public_idempotency
    ON claims(organizer_id, idempotency_key)
    WHERE reseller_scope IS NULL;

ALTER TABLE claims DROP CONSTRAINT claims_staff_scope_shape;

ALTER TABLE claims DROP COLUMN staff_scope;
