-- +goose Up
-- Phase G (L1, Q5 answered yes): crypto holdings that are linked to a CoinMarketCap instrument are valued
-- with the market price. Only price_mode changes: avg_buy_price, fees, manual_price and
-- manual_current_value are never written, so the cost basis (quantity x avg_buy_price + fees, used for
-- profit and loss only) and the typed fallback price stay exactly as they were.
-- +goose StatementBegin
DO $$
DECLARE
    flipped  integer;
    unlinked record;
BEGIN
    UPDATE investments SET price_mode = 'AUTO'
     WHERE asset_type = 'CRYPTO' AND price_mode = 'MANUAL' AND instrument_id IS NOT NULL
       AND manual_current_value IS NULL AND deleted_at IS NULL;
    GET DIAGNOSTICS flipped = ROW_COUNT;
    RAISE NOTICE 'migration 00019: % crypto holdings switched to AUTO price mode', flipped;

    -- Holdings that cannot follow the market yet (no instrument) are only listed.
    FOR unlinked IN
        SELECT id, name, symbol FROM investments
         WHERE asset_type = 'CRYPTO' AND price_mode = 'MANUAL' AND instrument_id IS NULL AND deleted_at IS NULL
    LOOP
        RAISE NOTICE 'migration 00019: crypto % (%, %) has no market instrument and stays MANUAL', unlinked.id, unlinked.name, unlinked.symbol;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Nothing to undo: after the Up migration a row switched here cannot be told apart from a holding the
-- user set to AUTO on purpose, and a price mode is a setting, not a schema change.
SELECT 1;
