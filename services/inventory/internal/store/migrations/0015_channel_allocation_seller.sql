-- +goose Up
-- An allocation may bind to a SELLER (TKT-246, amending ADR-024).
--
-- Until now an allocation said WHEN a channel may sell (opens_at/closes_at, ADR-054)
-- and WHETHER a code is needed (requires_code, ADR-064). Neither says WHO may sell it,
-- and that gap is the channel seam: commerce resolved a channel's fees and inventory
-- never saw the channel, so a reseller-channel sale took reseller fees while consuming
-- public stock.
--
-- Closing it is an AUTHORIZATION change, not plumbing. TKT-240 forwarded the channel
-- and was reverted: POST /reservations is unauthenticated and takes channel_code from
-- the request body, so the forward alone let any caller drain a reseller's allocation
-- with no credential (executed, not theorised). The binding lives HERE, next to the
-- stock, because the guard has to be judged under the pool row lock -- a check in the
-- calling service binds only callers who go through it (ADR-043).
--
-- NULLABLE, and a uuid rather than a boolean. Nullable because every allocation that
-- exists today is public and must stay exactly as sellable as it was: NULL means
-- "anyone may sell this", which is the pre-migration behaviour by construction. A uuid
-- rather than a boolean because a bare "is reserved" flag would let reseller B consume
-- reseller A's allocation -- the same class of bug one layer in.
--
-- NO foreign key, deliberately, for the reason ADR-056 gives for the same column on
-- commerce's orders: the reseller registry lives in COMMERCE (reseller_credentials),
-- not inventory, and this is a cross-service identity. An FK is impossible across the
-- database boundary and would be wrong even within it -- revoking or rotating a
-- credential must not rewrite or block who a past allocation was bound to.
--
-- ADVERSARY (ADR-021): this is honest-writer authorization, NOT tamper-evidence.
-- Anyone who can write inventory's database, or reach /internal/ directly, can set or
-- clear sold_by at will. It constrains a caller coming through the hold path; it
-- constrains nobody with write access to this table.
ALTER TABLE channel_allocations
    ADD COLUMN sold_by uuid;

-- Partial: only bound rows are interesting to an operator asking "what does this
-- reseller hold?", and the claim path never uses it -- that lookup is on the primary
-- key (pool_id, channel_code) and reads sold_by from the row it already has.
CREATE INDEX channel_allocations_by_seller
  ON channel_allocations(sold_by)
  WHERE sold_by IS NOT NULL;

-- +goose Down
-- Keyed on the presence of REAL STATE, exactly as 0014 does: every allocation
-- predating this migration has sold_by NULL, so a bare row-count guard on
-- channel_allocations would refuse every rollback including the safe ones. IS NOT NULL
-- is the guard; absence is not state.
--
-- Dropping a bound allocation's binding would make a reseller's private allocation
-- publicly consumable, silently. That is a data-loss-shaped authorization regression,
-- so it is refused rather than performed.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM channel_allocations WHERE sold_by IS NOT NULL) THEN
    RAISE EXCEPTION 'seller-bound channel allocations exist; unbind them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX channel_allocations_by_seller;
ALTER TABLE channel_allocations DROP COLUMN sold_by;
