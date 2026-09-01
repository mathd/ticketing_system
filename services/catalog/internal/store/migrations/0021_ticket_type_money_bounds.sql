-- Bound ticket_types to the Money range its own contract declares (TKT-154).
--
-- Two pre-existing holes in one table, same shape. `price_amount` was
-- `bigint CHECK (price_amount >= 0)` with no upper bound, and `currency` was
-- `char(3)` with no case constraint, while the OpenAPI `Money` schema every
-- contract-declared response uses caps the amount at 9007199254740991 and
-- requires `^[A-Z]{3}$` (services/catalog/api/openapi.yaml:2568,2571). So a
-- price in (2^53-1, MaxInt64] and a lowercase code were both legal here and
-- unrepresentable there.
--
-- The bounds are COPIED FROM THE CONTRACT, not chosen. 9007199254740991 is
-- JavaScript's safe-integer limit: the storefront must represent the value
-- exactly, which is why the cap exists at all (ADR-001, money is integer minor
-- units and every consumer agrees on the integer). The database is the side
-- that disagreed, so the database is the side that changes (ADR-009).
--
-- THIS IS LIVE, NOT LATENT, and the ticket's own "theoretical" framing is out
-- of date. TKT-151 shipped GET /internal/ticket-types/{id}/price-resolution, a
-- contract-declared operation whose PriceResolution.base_price is populated
-- straight from this column (internal/api/pricing.go:61). Response validation
-- defaults on (compose.yaml:49), so ADR-028 turns a violating row into a
-- fail-closed 500 on a money read. A row nothing can create today would break
-- a read that exists today.
--
-- The sibling table has carried these exact CHECKs since TKT-151
-- (0012_price_rules.sql:34,40), and its comments name ticket_types as having
-- the same holes and defer them here. This migration is that deferral coming
-- due; the fix was written down before it was applied.
--
-- NO API-TIER VALIDATION ACCOMPANIES THIS, deliberately. `createTicketType` is
-- contract-declared (openapi.yaml:908) behind a spec-wide request validator
-- (internal/api/server.go:219), so an HTTP caller sending an out-of-range
-- amount or a lowercase code is already refused with a 400 before the handler
-- runs. A hand-written check there would duplicate a live mechanism and still
-- miss every writer that does not come through that handler. The column is the
-- only enforcement point that covers all of them, including the API's own
-- INSERT (internal/store/postgres_slots.go:167).
--
-- THE CONSTRAINTS ARE NAMED for a reason that is not style. An unnamed CHECK
-- takes a generated name, and a test asserting only "the INSERT errored" cannot
-- then say WHICH constraint fired -- so one broken constraint would still look
-- proven by the other's test. The store tests assert on these names.
--
-- No preflight or NOT VALID. `postgres_slots.go:167` is the only
-- `INSERT INTO ticket_types` outside tests and it writes contract-validated
-- input, so no violating row can exist and this applies cleanly. If it ever
-- fails to apply, that is a real finding about a writer nobody enumerated, and
-- the answer is to find that writer -- not to weaken the constraint. NOT VALID
-- would let the violating rows keep 500ing the reads this exists to protect.
--
-- Name the adversary (ADR-021): HONEST-WRITER consistency. This stops a buggy
-- caller, a future non-API writer and a careless backfill. Anyone with catalog
-- DB access can drop the constraint and insert whatever they like; it is not
-- tamper-evidence and does not claim to be.
--
-- Placement is out-of-band per ADR-022 -- the binary's `migrate` subcommand as
-- a Compose one-shot the service waits on. The server path never migrates.

-- +goose Up
ALTER TABLE ticket_types
    ADD CONSTRAINT ticket_types_price_amount_max
        CHECK (price_amount <= 9007199254740991),
    ADD CONSTRAINT ticket_types_currency_format
        CHECK (currency ~ '^[A-Z]{3}$');

-- +goose Down
-- No IF EXISTS. PostgreSQL DDL is transactional, so a missing constraint aborts
-- the whole statement and leaves the schema as it was, rather than reporting a
-- success that removed only half of what it named. The Down transforms no rows,
-- so there is nothing to lose: it widens what the column accepts, and the rows
-- already stored all satisfy the narrower rule.
ALTER TABLE ticket_types
    DROP CONSTRAINT ticket_types_currency_format,
    DROP CONSTRAINT ticket_types_price_amount_max;
