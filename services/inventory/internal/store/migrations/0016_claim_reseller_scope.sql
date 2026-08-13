-- +goose Up
-- Idempotency keys are scoped to the RESELLER, structurally (TKT-246 ai-review pass 2).
--
-- claims were UNIQUE (organizer_id, idempotency_key). Two reseller credentials may
-- legally share an organizer, and keys are caller-chosen and frequently sequential, so
-- reseller A and reseller B both sending "1" landed on one row. That was not merely a
-- collision, it was an AUTHORIZATION BYPASS: CreateHold looks a claim up by that key and
-- returns a fingerprint-matching row as a REPLAY before it reads channel_allocations
-- .sold_by, so B was handed A's authorized hold on A's bound allocation. The seller guard
-- was never reached.
--
-- The first fix namespaced the key in Go, as "r:<uuid>:<key>". That closed the handover
-- and opened a denial of service, which the second review pass found: public keys are
-- arbitrary raw strings in the SAME column, so a public caller can send that exact
-- derived string first, take the row, and permanently deny that reseller that key --
-- targeted, given a predictable key and a known reseller id. A prefix inside a shared
-- namespace is not a namespace; it is a naming convention that an attacker also gets to
-- use.
--
-- So the scope becomes a COLUMN, and the uniqueness covers it. Public and partner keys
-- can no longer name the same row whatever string either sends, because they differ in a
-- field the caller does not supply.
ALTER TABLE claims
    ADD COLUMN reseller_scope uuid;

-- Two partial indexes rather than one over COALESCE(reseller_scope, <sentinel>).
--
-- A sentinel uuid would make "no reseller" a value in the same domain as a reseller id,
-- and the whole defect above is what happens when two different things share one
-- namespace. NULL is not a value, which is exactly the property wanted here.
--
-- The public index is partial on reseller_scope IS NULL, and it is the pre-existing
-- constraint in every respect that matters: same columns, same order, same semantics for
-- every row that exists today (all of them have NULL). Nothing is re-keyed, and no
-- in-flight retry changes which row it finds.
DROP INDEX IF EXISTS claims_organizer_id_idempotency_key_key;
ALTER TABLE claims
    DROP CONSTRAINT IF EXISTS claims_organizer_id_idempotency_key_key;

CREATE UNIQUE INDEX claims_public_idempotency
    ON claims(organizer_id, idempotency_key)
    WHERE reseller_scope IS NULL;

CREATE UNIQUE INDEX claims_reseller_idempotency
    ON claims(organizer_id, reseller_scope, idempotency_key)
    WHERE reseller_scope IS NOT NULL;

-- +goose Down
-- Refuses rather than performs, for the reason 0014 and 0015 do: the rollback is only
-- safe while no partner-scoped claim exists. Collapsing the two namespaces back into one
-- would fail on any (organizer, key) pair a public caller and a reseller both used --
-- and if it did NOT fail, it would mean re-admitting the collision that was the bypass.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM claims WHERE reseller_scope IS NOT NULL) THEN
    RAISE EXCEPTION 'reseller-scoped claims exist; downgrading would merge two idempotency namespaces into one';
  END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX claims_reseller_idempotency;
DROP INDEX claims_public_idempotency;
ALTER TABLE claims
    ADD CONSTRAINT claims_organizer_id_idempotency_key_key UNIQUE (organizer_id, idempotency_key);
ALTER TABLE claims DROP COLUMN reseller_scope;
