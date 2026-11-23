-- +goose Up
-- Phase H (T8, D-B6): an investment no longer gets an assumed 5.5% annual change the user never typed.
-- The column becomes nullable with no default. Existing rows keep their stored value: turning the untouched
-- 5.5% rows into NULL is a data change that waits for the owner's answer (plan question Q6).
ALTER TABLE investments
    ALTER COLUMN estimated_annual_change_pct DROP NOT NULL,
    ALTER COLUMN estimated_annual_change_pct DROP DEFAULT;

-- +goose Down
-- NULLs go back to the old default so the NOT NULL constraint can be restored.
-- +goose StatementBegin
DO $$
DECLARE
    filled integer;
BEGIN
    UPDATE investments SET estimated_annual_change_pct = 5.5 WHERE estimated_annual_change_pct IS NULL;
    GET DIAGNOSTICS filled = ROW_COUNT;
    RAISE NOTICE 'migration 00020 down: % NULL estimated changes set back to 5.5', filled;
END $$;
-- +goose StatementEnd
ALTER TABLE investments
    ALTER COLUMN estimated_annual_change_pct SET DEFAULT 5.5,
    ALTER COLUMN estimated_annual_change_pct SET NOT NULL;
