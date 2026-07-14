-- +goose Up
ALTER TABLE performances DROP CONSTRAINT performances_status_check;
ALTER TABLE performances ADD CONSTRAINT performances_status_check
    CHECK (status IN ('draft', 'published', 'archived'));
ALTER TABLE performances ADD COLUMN archived_at timestamptz;
ALTER TABLE performances ADD COLUMN archive_emitted_at timestamptz;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM performances WHERE status = 'archived') THEN
        RAISE EXCEPTION 'cannot roll back archived lifecycle migration: archived performances exist';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE performances DROP COLUMN archive_emitted_at;
ALTER TABLE performances DROP COLUMN archived_at;
ALTER TABLE performances DROP CONSTRAINT performances_status_check;
ALTER TABLE performances ADD CONSTRAINT performances_status_check
    CHECK (status IN ('draft', 'published'));
