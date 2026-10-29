-- +goose Up
-- Per-version publication timestamps (version -> RFC3339) served as Composer's
-- published-time. NULL until backfilled; see `wppackages backfill-published-times`.
ALTER TABLE packages ADD COLUMN published_times_json TEXT;

-- +goose Down
ALTER TABLE packages DROP COLUMN published_times_json;
