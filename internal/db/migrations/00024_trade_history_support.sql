-- +goose Up
-- The trade list of one investment is read newest first (occurred_at, id). The older single-column index is a prefix
-- of the new one, so it is dropped.
CREATE INDEX transactions_investment_occurred_idx ON transactions (investment_id, occurred_at DESC, id DESC) WHERE investment_id IS NOT NULL;
DROP INDEX transactions_investment_idx;

-- The order trades were recorded in (the order of the position snapshots) is read from created_at. now() is the start
-- of the database transaction, which can be earlier than a competing trade that took the investment lock first;
-- clock_timestamp() is taken at the insert, which happens under that lock, so it follows the lock order. Existing rows
-- are not touched.
ALTER TABLE transactions ALTER COLUMN created_at SET DEFAULT clock_timestamp();

-- +goose Down
ALTER TABLE transactions ALTER COLUMN created_at SET DEFAULT now();
CREATE INDEX transactions_investment_idx ON transactions (investment_id) WHERE investment_id IS NOT NULL;
DROP INDEX transactions_investment_occurred_idx;
