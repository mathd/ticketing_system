-- +goose Up
ALTER TABLE lifecycle_events DROP CONSTRAINT lifecycle_events_event_type_check;
ALTER TABLE lifecycle_events ADD CONSTRAINT lifecycle_events_event_type_check
  CHECK(event_type IN ('issued', 'delivered', 'redeemed'));

-- +goose Down
DO $$ BEGIN
  RAISE EXCEPTION 'cannot roll back redeemed lifecycle events without destroying immutable ticket history';
END $$;
