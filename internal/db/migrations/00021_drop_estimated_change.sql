-- +goose Up
-- The expected yearly change is removed from investments and manual assets. The notice prints how many rows had a value.
-- +goose StatementBegin
DO $$
DECLARE
    inv integer;
    ma  integer;
BEGIN
    SELECT count(*) INTO inv FROM investments WHERE estimated_annual_change_pct IS NOT NULL;
    SELECT count(*) INTO ma FROM manual_assets WHERE estimated_annual_change_pct IS NOT NULL;
    RAISE NOTICE 'migration 00021 up: dropping estimated_annual_change_pct (% investments and % manual assets had a value)', inv, ma;
END $$;
-- +goose StatementEnd
ALTER TABLE investments DROP COLUMN estimated_annual_change_pct;
ALTER TABLE manual_assets DROP COLUMN estimated_annual_change_pct;

-- +goose Down
-- The columns come back empty (nullable, the 00020 shape); the dropped values are not restored.
ALTER TABLE investments
    ADD COLUMN estimated_annual_change_pct numeric(8,4) CHECK (estimated_annual_change_pct IS NULL OR estimated_annual_change_pct BETWEEN -100 AND 1000);
ALTER TABLE manual_assets
    ADD COLUMN estimated_annual_change_pct numeric(8,4) CHECK (estimated_annual_change_pct IS NULL OR estimated_annual_change_pct BETWEEN -100 AND 1000);
