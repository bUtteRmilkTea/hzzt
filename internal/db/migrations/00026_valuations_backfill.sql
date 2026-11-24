-- +goose Up
-- One valuation per existing mutual fund from today's manual price, dated by details.navDate (or the last update when it is
-- empty or not a date), so the screen is not empty. Funds that already have a valuation are skipped. The notice prints the count.
-- +goose StatementBegin
DO $$
DECLARE
    r record;
    d date;
    n integer := 0;
BEGIN
    FOR r IN
        SELECT i.id, i.quantity, i.avg_buy_price, i.fees, i.manual_price, i.created_by_id, i.updated_at, i.details->>'navDate' AS nav_date
        FROM investments i
        WHERE i.asset_type = 'MUTUAL_FUND' AND i.deleted_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM investment_valuations v WHERE v.investment_id = i.id)
    LOOP
        BEGIN
            d := r.nav_date::date;
        EXCEPTION WHEN others THEN
            d := NULL;
        END;
        IF d IS NULL THEN
            d := r.updated_at::date;
        END IF;
        INSERT INTO investment_valuations (investment_id, as_of, nav, quantity, cost_basis, value, note, created_by_id)
        VALUES (r.id, d, r.manual_price, r.quantity, round(r.quantity * r.avg_buy_price + r.fees, 8), round(r.quantity * r.manual_price, 8),
                'Backfilled from the current NAV', r.created_by_id);
        n := n + 1;
    END LOOP;
    RAISE NOTICE 'migration 00026 up: % mutual funds got a starting valuation', n;
END $$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM investment_valuations WHERE note = 'Backfilled from the current NAV';
