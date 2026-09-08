-- +goose Up
-- Reseller payee kind (TKT-277): partner commission fees settle to a payee whose
-- kind is 'reseller'.
--
-- Kinds are rows in payee_kinds (ADR-047 §2, migration 0017), so this is data
-- rather than a schema change.
INSERT INTO payee_kinds (code, label) VALUES ('reseller', 'Reseller partner');

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payees WHERE kind = 'reseller') THEN
        RAISE EXCEPTION 'refusing to drop reseller payee kind: payees of kind reseller exist';
    END IF;
END
$$;
-- +goose StatementEnd
DELETE FROM payee_kinds WHERE code = 'reseller';
