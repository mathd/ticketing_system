-- Series and seasons (US-010 / TKT-52). Lifecycle remains on performances;
-- series membership is a stable ordered grouping over those authoritative rows.
-- +goose Up
CREATE TABLE series (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id uuid NOT NULL REFERENCES organizers (id),
    event_id     uuid NOT NULL REFERENCES events (id),
    name         jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX series_by_event ON series (event_id);

CREATE TABLE series_performances (
    series_id      uuid NOT NULL REFERENCES series (id),
    performance_id uuid NOT NULL UNIQUE REFERENCES performances (id),
    position        integer NOT NULL CHECK (position > 0),
    PRIMARY KEY (series_id, performance_id),
    UNIQUE (series_id, position)
);

CREATE TABLE seasons (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id uuid NOT NULL REFERENCES organizers (id),
    name         jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE season_series (
    season_id uuid NOT NULL REFERENCES seasons (id),
    series_id uuid NOT NULL REFERENCES series (id),
    PRIMARY KEY (season_id, series_id)
);

CREATE TABLE season_events (
    season_id uuid NOT NULL REFERENCES seasons (id),
    event_id  uuid NOT NULL REFERENCES events (id),
    PRIMARY KEY (season_id, event_id)
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM series)
       OR EXISTS (SELECT 1 FROM seasons) THEN
        RAISE EXCEPTION 'cannot roll back 0005: series or season data exists';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE season_events;
DROP TABLE season_series;
DROP TABLE seasons;
DROP TABLE series_performances;
DROP TABLE series;
