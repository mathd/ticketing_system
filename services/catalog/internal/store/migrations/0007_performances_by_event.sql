-- Scoped season/event public reads (TKT-60). The public read filters
-- performances by event id, but the only usable index was
-- performances_public_read (status, starts_at) — so a season read matched its
-- own events *after* Postgres had already scanned every published performance
-- in the catalog. Scoping the query alone does not fix that: it shrinks the
-- response, not the work (ADR-004 asks for the opposite).
--
-- event_id first (equality) then status (the public read's constant filter);
-- starts_at is deliberately left out — the ORDER BY is a COALESCE expression
-- over operating_date/opens_at for day kinds, so it is not an index-orderable
-- column here and a third key would only widen the index for no gain.
-- +goose Up
CREATE INDEX performances_by_event ON performances (event_id, status);

-- +goose Down
DROP INDEX performances_by_event;
