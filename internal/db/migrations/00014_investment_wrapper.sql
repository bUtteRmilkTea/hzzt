-- +goose Up
-- The exchange / broker / platform an investment is held on ("wrapper"), kept as a
-- first-class column so investments can be grouped by it.
ALTER TABLE investments ADD COLUMN wrapper text CHECK (wrapper IS NULL OR char_length(wrapper) BETWEEN 1 AND 60);

-- US_STOCK/GLOBAL_ETF prefer `platform`: `exchange` there is the listing venue (NASDAQ), not the broker.
UPDATE investments SET wrapper = NULLIF(left(btrim(COALESCE(details->>'exchangeOrWallet', details->>'broker', details->>'platform')), 60), '');

CREATE INDEX investments_space_wrapper_idx ON investments (space_id, asset_type, lower(wrapper)) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX investments_space_wrapper_idx;
ALTER TABLE investments DROP COLUMN wrapper;
