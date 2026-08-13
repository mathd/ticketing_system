-- +goose Up
-- DEPLOY NOTE — this migration is NOT online, and NOT expand/contract (ai-review pass 3).
--
-- goose runs it in one transaction. ADD COLUMN takes ACCESS EXCLUSIVE and holds it to
-- commit, and the two index builds happen inside that lock, so READERS of claims block
-- for the whole thing — not just writers. On a large claims table that can exceed
-- ADR-008's 30s migration bound and fail the deploy.
--
-- And the binaries are not compatible in either direction across it:
--   * new binary on schema 0015  -> every hold 500s (it selects reseller_scope)
--   * previous binary on 0016    -> writes NULL scope and decorates the key itself, so a
--                                   partner retry misses its row and places a SECOND hold
--
-- So this requires a QUIESCED CUTOVER of inventory, not a rolling one: stop inventory,
-- migrate, start the new binary. That is acceptable today because the partner write path
-- is new in this same ticket -- there are no partner claims in any database yet, so the
-- second-hold window has nothing to act on, and the table is small enough on every
-- current deployment for the lock to be brief.
--
-- It is NOT acceptable once partner sales are live at volume. Making it online means
-- splitting the column-add from the index builds, building them CONCURRENTLY outside a
-- transaction, and giving the binary a schema-tolerant read for one release. ADR-020's
-- preconditions for CONCURRENTLY are still not met, so that is a ticket, not a tweak.
-- Recorded here rather than discovered during an incident.
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
-- DROP CONSTRAINT, not DROP INDEX. The unnamed `UNIQUE (organizer_id,
-- idempotency_key)` in 0001 is a CONSTRAINT whose backing index Postgres owns, and it
-- refuses to drop the index out from under it:
--
--   ERROR: cannot drop index claims_organizer_id_idempotency_key_key because constraint
--          claims_organizer_id_idempotency_key_key on table claims requires it
--
-- Both spellings were here at first, the DROP INDEX ran first, and the migration failed
-- exactly there. Only the constraint form works; dropping it takes the index with it.
ALTER TABLE claims
    DROP CONSTRAINT claims_organizer_id_idempotency_key_key;

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
