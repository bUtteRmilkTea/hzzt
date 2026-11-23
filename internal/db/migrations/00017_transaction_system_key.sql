-- +goose Up
-- Phase F (T1, T12): a stable key for the transactions the backend writes itself, so the
-- client can localize their title, and top ups stop being income.
ALTER TABLE transactions ADD COLUMN system_key text
    CHECK (system_key IS NULL OR system_key IN ('OPENING_BALANCE', 'BALANCE_TOP_UP', 'BALANCE_ADJUSTMENT'));

-- +goose StatementBegin
DO $$
DECLARE
    moved integer;
BEGIN
    -- A top up was recorded as an uncategorised INCOME titled "Balance top up". Make it an
    -- ADJUSTMENT IN: same sign and amount, so no wallet balance changes. type and direction
    -- are set in one statement to satisfy the (type, direction) CHECK.
    UPDATE transactions
       SET type = 'ADJUSTMENT', direction = 'IN', system_key = 'BALANCE_TOP_UP'
     WHERE type = 'INCOME' AND category_id IS NULL AND title = 'Balance top up'
       AND investment_id IS NULL AND manual_asset_id IS NULL;
    GET DIAGNOSTICS moved = ROW_COUNT;
    RAISE NOTICE 'migration 00017: % top-up INCOME rows reclassified to ADJUSTMENT IN', moved;

    UPDATE transactions SET system_key = 'OPENING_BALANCE'
     WHERE type = 'OPENING_BALANCE' AND title = 'Opening balance';
    UPDATE transactions SET system_key = 'BALANCE_ADJUSTMENT'
     WHERE type = 'ADJUSTMENT' AND title = 'Balance adjustment' AND system_key IS NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Top ups go back to being INCOME (the behaviour before this migration), including those
-- created after it. The wallet balance effect of ADJUSTMENT IN and INCOME is identical.
-- +goose StatementBegin
DO $$
DECLARE
    moved integer;
BEGIN
    UPDATE transactions SET type = 'INCOME', direction = NULL
     WHERE type = 'ADJUSTMENT' AND system_key = 'BALANCE_TOP_UP';
    GET DIAGNOSTICS moved = ROW_COUNT;
    RAISE NOTICE 'migration 00017 down: % top-up rows are INCOME again', moved;
END $$;
-- +goose StatementEnd
ALTER TABLE transactions DROP COLUMN system_key;
